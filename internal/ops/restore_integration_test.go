// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/HakeemTheEmperor/pentymento/internal/backend/s3"
	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// restoreDeadline bounds one restore, so a stuck one fails with its own
// error rather than at the test binary's timeout.
const restoreDeadline = 10 * time.Minute

// testProviders are the instance providers to restore through, from
// PENTYM_TEST_PROVIDERS (CI sets docker,local); Docker by default.
func testProviders() []string {
	var out []string
	for _, p := range strings.Split(os.Getenv("PENTYM_TEST_PROVIDERS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = []string{config.ProviderDocker}
	}
	return out
}

func splitEnv(name string) []string {
	var out []string
	for _, d := range filepath.SplitList(os.Getenv(name)) {
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// sideInstances is one provider's instance set and the work directory it
// and the restore use.
func sideInstances(t *testing.T, provider string) (*instance.Set, string) {
	t.Helper()
	tools := config.Tools{Postgres: splitEnv("PENTYM_TEST_POSTGRES_BIN"), MongoDB: splitEnv("PENTYM_TEST_MONGODB_BIN")}
	if len(tools.Postgres) == 0 && runtime.GOOS == "linux" {
		tools.Postgres, _ = filepath.Glob("/usr/lib/postgresql/*/bin")
	}
	work := t.TempDir()
	env := instance.NewEnv(work, tools, &toolrun.Runner{})
	return instance.NewSet(env, []string{provider}), work
}

// s3Fixture is a repository under a fresh prefix of a MinIO bucket.
func s3Fixture(t *testing.T, m *dbtest.MinIO) *fixture {
	t.Helper()
	id := make([]byte, 6)
	_, _ = rand.Read(id)
	be, err := s3.New(context.Background(), s3.Options{
		Bucket: m.Bucket, Prefix: "ops-" + hex.EncodeToString(id), Region: "us-east-1", Endpoint: m.Endpoint, PathStyle: true,
		AccessKeyID: m.AccessKey, SecretAccessKey: secrets.New([]byte(m.SecretKey)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return newFixtureOn(t, be)
}

// pgChecksums is every user table's row count and a checksum of its rows,
// independent of their physical order.
func pgChecksums(t *testing.T, conn *pgconn.PgConn) map[string]string {
	t.Helper()
	ctx := context.Background()
	tables, err := conn.Exec(ctx, `SELECT format('%I.%I', schemaname, tablename) FROM pg_tables
		WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1`).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, row := range tables[0].Rows {
		name := string(row[0])
		res, err := conn.Exec(ctx, "SELECT count(*)::text || ':' || coalesce(md5(string_agg(md5(t::text), '' ORDER BY md5(t::text))), '') FROM "+name+" t").ReadAll()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = string(res[0].Rows[0][0])
	}
	return out
}

func pgConnect(t *testing.T, dsn string) *pgconn.PgConn {
	t.Helper()
	conn, err := pgconn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func compareChecksums(t *testing.T, want, got map[string]string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatal("the source has no tables to compare")
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if got[name] != want[name] {
			t.Errorf("%s: restored %q, source %q", name, got[name], want[name])
		}
	}
	if len(got) != len(want) {
		t.Errorf("restored %d tables, source %d", len(got), len(want))
	}
}

// TestPostgresRestore backs up each supported Postgres version, with a
// tablespace, to local and S3 storage, restores the latest snapshot into
// a side instance, and compares every table (Build Order C16).
func TestPostgresRestore(t *testing.T) {
	m := dbtest.StartMinIO(t)
	for _, version := range dbtest.PostgresVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			target, eng, dsn, pg := pgServer(t, version)
			pg.Exec(t, "sh", "-c", "mkdir -p /var/lib/postgresql/ts && chown postgres /var/lib/postgresql/ts")
			pgSQL(t, dsn, "CREATE TABLESPACE ts LOCATION '/var/lib/postgresql/ts'")
			pgSQL(t, dsn, "CREATE TABLE spaced TABLESPACE ts AS SELECT i, repeat(md5(i::text), 4) AS v FROM generate_series(1, 20000) AS i")
			pgSQL(t, dsn, "CREATE SCHEMA billing")
			pgSQL(t, dsn, "CREATE TABLE billing.invoices (id serial PRIMARY KEY, total numeric, issued timestamptz DEFAULT now())")
			pgSQL(t, dsn, "INSERT INTO billing.invoices (total) SELECT i * 1.5 FROM generate_series(1, 5000) AS i")
			want := pgChecksums(t, pgConnect(t, dsn))
			cfg, err := pgconn.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}

			for _, store := range []string{"local", "s3"} {
				f := newFixture(t)
				if store == "s3" {
					f = s3Fixture(t, m)
				}
				b := f.backup(eng)
				b.Target = target
				if _, err := b.Run(ctx); err != nil {
					t.Fatalf("%s backup: %v", store, err)
				}
				providers := testProviders()
				if store == "s3" {
					providers = providers[:1]
				}
				for _, p := range providers {
					t.Run(store+"/"+p, func(t *testing.T) {
						set, work := sideInstances(t, p)
						o := f.restore(t, eng, set, work)
						o.Target = target
						rctx, cancel := context.WithTimeout(ctx, restoreDeadline)
						defer cancel()
						res, err := o.Run(rctx)
						if err != nil {
							t.Fatal(err)
						}
						defer func() {
							if err := res.Close(context.Background()); err != nil {
								t.Errorf("close: %v", err)
							}
						}()
						c := res.Instance.Conn()
						side := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
							c.Host, c.Port, c.User, c.Password.Expose(), cfg.Database)
						compareChecksums(t, want, pgChecksums(t, pgConnect(t, side)))
						// Only pentym's side role logs in: the trust window is closed.
						if _, err := pgconn.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable",
							c.Host, c.Port, cfg.User, cfg.Database)); err == nil {
							t.Error("the side instance lets the cluster's superuser in without a password")
						}
					})
				}
			}
		})
	}
}

// mongoChecksums is the app database's per-collection hashes (dbHash),
// which depend only on the documents.
func mongoChecksums(t *testing.T, cl *mongo.Client) map[string]string {
	t.Helper()
	var res struct {
		Collections map[string]string `bson:"collections"`
	}
	if err := cl.Database("app").RunCommand(context.Background(), bson.D{{Key: "dbHash", Value: 1}}).Decode(&res); err != nil {
		t.Fatal(err)
	}
	return res.Collections
}

// TestMongoDBRestore backs up each supported MongoDB version as a replica
// set to local and S3 storage, restores it into a side instance and
// compares every collection (Build Order C16).
func TestMongoDBRestore(t *testing.T) {
	m := dbtest.StartMinIO(t)
	for _, version := range dbtest.MongoDBVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			target, eng, admin := mgServer(t, dbtest.StartMongoDB(t, version))
			if _, err := admin.Database("app").Collection("customers").InsertMany(ctx, []any{
				bson.D{{Key: "_id", Value: "ada"}, {Key: "since", Value: 2019}},
				bson.D{{Key: "_id", Value: "grace"}, {Key: "since", Value: 2021}, {Key: "tags", Value: bson.A{"vip"}}},
			}); err != nil {
				t.Fatal(err)
			}
			want := mongoChecksums(t, admin)
			for _, store := range []string{"local", "s3"} {
				f := newFixture(t)
				if store == "s3" {
					f = s3Fixture(t, m)
				}
				b := f.backup(eng)
				b.Target = target
				if _, err := b.Run(ctx); err != nil {
					t.Fatalf("%s backup: %v", store, err)
				}
				providers := testProviders()
				if store == "s3" {
					providers = providers[:1]
				}
				for _, p := range providers {
					t.Run(store+"/"+p, func(t *testing.T) {
						set, work := sideInstances(t, p)
						o := f.restore(t, eng, set, work)
						o.Target = target
						rctx, cancel := context.WithTimeout(ctx, restoreDeadline)
						defer cancel()
						res, err := o.Run(rctx)
						if err != nil {
							t.Fatal(err)
						}
						defer func() {
							if err := res.Close(context.Background()); err != nil {
								t.Errorf("close: %v", err)
							}
						}()
						c := res.Instance.Conn()
						cl, err := mongo.Connect(options.Client().
							SetHosts([]string{net.JoinHostPort(c.Host, strconv.Itoa(c.Port))}).SetDirect(true).
							SetAuth(options.Credential{Username: c.User, Password: string(c.Password.Expose()), AuthSource: "admin"}))
						if err != nil {
							t.Fatal(err)
						}
						defer func() { _ = cl.Disconnect(context.Background()) }()
						got := mongoChecksums(t, cl)
						if len(want) < 2 {
							t.Fatalf("the source has %d collections", len(want))
						}
						for _, name := range slices.Sorted(maps.Keys(want)) {
							if got[name] != want[name] {
								t.Errorf("%s: restored %q, source %q", name, got[name], want[name])
							}
						}
					})
				}
			}
		})
	}
}
