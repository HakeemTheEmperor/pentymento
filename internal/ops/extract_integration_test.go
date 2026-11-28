// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/engine/postgres"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// newestClientTools is the newest PostgreSQL client tool directory on a
// Linux runner, so pg_dump can read every server version tested.
func newestClientTools() []string {
	if d := splitEnv("PENTYM_TEST_POSTGRES_BIN"); len(d) > 0 {
		return d
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	dirs, _ := filepath.Glob("/usr/lib/postgresql/*/bin")
	sort.Slice(dirs, func(i, j int) bool {
		a, _ := strconv.Atoi(filepath.Base(filepath.Dir(dirs[i])))
		b, _ := strconv.Atoi(filepath.Base(filepath.Dir(dirs[j])))
		return a > b
	})
	if len(dirs) > 0 {
		return dirs[:1]
	}
	return nil
}

// TestPostgresExtract (Build Order C21): a table dropped in a known
// transaction is restored into production to just before it, beside
// tables that kept changing, which are left untouched.
func TestPostgresExtract(t *testing.T) {
	for _, version := range dbtest.PostgresVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			target, eng, dsn, _ := pgServer(t, version)
			cfg, err := pgconn.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			target.Config.Database = cfg.Database
			rsRef, _ := secrets.ParseRef("env:RS_PW")
			target.Config.Credentials.Restore = config.Credential{User: "pentym_restore", Password: rsRef}
			eng.Resolver = secrets.Resolver{Getenv: func(k string) string {
				return map[string]string{"BK_PW": "backup-pw", "RS_PW": "restore-pw"}[k]
			}}
			eng.ToolDirs = newestClientTools()

			pgSQL(t, dsn, "DROP TABLE orders") // the shared server's; this test makes its own
			pgSQL(t, dsn, "CREATE ROLE pentym_restore LOGIN PASSWORD 'restore-pw'")
			pgSQL(t, dsn, "GRANT USAGE, CREATE ON SCHEMA public TO pentym_restore")
			pgSQL(t, dsn, "CREATE TABLE customers (id serial PRIMARY KEY, name text NOT NULL)")
			pgSQL(t, dsn, "INSERT INTO customers (name) SELECT 'c' || i FROM generate_series(1, 10) AS i")
			pgSQL(t, dsn, `CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id int REFERENCES customers,
				note text NOT NULL CHECK (note <> ''), created timestamptz DEFAULT now())`)
			pgSQL(t, dsn, "CREATE INDEX orders_note_idx ON orders (note)")
			pgSQL(t, dsn, "INSERT INTO orders (customer_id, note) SELECT 1 + i % 10, 'n' || i FROM generate_series(1, 100) AS i")
			pgSQL(t, dsn, "CREATE TABLE audit (id serial PRIMARY KEY, what text)")

			f := newFixture(t)
			tid, err := f.repo.PutTargetInfo(ctx, repo.TargetInfo{Name: target.Name, Engine: "postgres"})
			if err != nil {
				t.Fatal(err)
			}
			target.ID = tid
			sink := &walSink{r: f.repo, tid: tid}
			cctx, stopCapture := context.WithCancel(ctx)
			captured := make(chan error, 1)
			go func() { captured <- eng.Capture(cctx, target, "", sink) }()
			pgSQL(t, dsn, "SELECT pg_switch_wal()")
			waitUntil(t, "capture to start", time.Minute, func() bool { return sink.stored() > 0 })
			b := f.backup(eng)
			b.Target = target
			if _, err := b.Run(ctx); err != nil {
				t.Fatalf("backup: %v", err)
			}
			pgSQL(t, dsn, "INSERT INTO orders (customer_id, note) SELECT 1, 'late' || i FROM generate_series(1, 5) AS i")
			xid := pgSQLAll(t, dsn, "DROP TABLE orders; SELECT txid_current()::text")
			// Writes after the drop, which the restore must not touch.
			pgSQL(t, dsn, "INSERT INTO audit (what) VALUES ('after the drop')")
			pgSQL(t, dsn, "INSERT INTO customers (name) VALUES ('new')")
			end := lsnOfText(t, pgSQLAll(t, dsn, "SELECT pg_current_wal_flush_lsn()::text"))
			pgSQL(t, dsn, "SELECT pg_switch_wal()")
			waitUntil(t, "capture to store the writes", 2*time.Minute, func() bool { return sink.stored() >= end })
			stopCapture()
			if err := <-captured; err != nil {
				t.Fatalf("capture: %v", err)
			}
			t.Logf("orders dropped in transaction %s", xid)

			set, work := sideInstances(t, testProviders()[0])
			restore := func(t *testing.T, req *engine.ExtractRequest, rewindDir string) (*RestoreResult, error) {
				t.Helper()
				o := f.restore(t, eng, set, work)
				o.Target, o.Point, o.Extract, o.RewindDir = target, engine.Point{XID: xid}, req, rewindDir
				o.Suffix = func(at time.Time) string { return "_recovered_" + at.UTC().Format("20060102") }
				rctx, cancel := context.WithTimeout(ctx, restoreDeadline)
				defer cancel()
				return o.Run(rctx)
			}

			t.Run("safe name", func(t *testing.T) {
				res, err := restore(t, &engine.ExtractRequest{Objects: []string{"orders"}}, "")
				if err != nil {
					t.Fatal(err)
				}
				ex := res.Extracted
				if len(ex.Objects) != 1 || !strings.HasPrefix(ex.Objects[0].Name, "public.orders_recovered_") || ex.Objects[0].Rows != 105 {
					t.Fatalf("extracted %+v", ex.Objects)
				}
				name := strings.TrimPrefix(ex.Objects[0].Name, "public.")
				sfx := strings.TrimPrefix(name, "orders")
				if got := pgSQLAll(t, dsn, "SELECT count(*)::text FROM "+name); got != "105" {
					t.Errorf("%s holds %s rows", name, got)
				}
				idx := pgSQLAll(t, dsn, "SELECT string_agg(indexname, ',' ORDER BY indexname) FROM pg_indexes WHERE tablename = '"+name+"'")
				if idx != "orders_note_idx"+sfx+",orders_pkey"+sfx {
					t.Errorf("indexes = %s", idx)
				}
				if got := pgSQLAll(t, dsn, "SELECT count(*)::text FROM pg_constraint WHERE conrelid = '"+name+"'::regclass AND contype = 'c'"); got != "1" {
					t.Errorf("%s check constraints on the copy", got)
				}
				if got := pgSQLAll(t, dsn, "SELECT nextval('orders_id_seq"+sfx+"')::text"); got != "106" {
					t.Errorf("the sequence continues at %s, want 106", got)
				}
				if len(ex.SkippedForeignKeys) != 1 || ex.SkippedForeignKeys[0].References != "customers" ||
					!strings.Contains(ex.SkippedForeignKeys[0].Statement, "FOREIGN KEY (customer_id) REFERENCES customers(id)") {
					t.Errorf("skipped foreign keys = %+v", ex.SkippedForeignKeys)
				}
				if got := pgSQLAll(t, dsn, "SELECT count(*)::text FROM pg_constraint WHERE conrelid = '"+name+"'::regclass AND contype = 'f'"); got != "0" {
					t.Errorf("%s foreign keys on the copy", got)
				}
				if got := pgSQLAll(t, dsn, "SELECT string_agg(what, ',') FROM audit"); got != "after the drop" {
					t.Errorf("audit = %q: the restore touched a table it did not restore", got)
				}
				if got := pgSQLAll(t, dsn, "SELECT count(*)::text FROM customers"); got != "11" {
					t.Errorf("customers = %s", got)
				}
				if res.Instance != nil {
					t.Error("the side instance is still up")
				}
				// A second restore picks the next free name.
				res2, err := restore(t, &engine.ExtractRequest{Objects: []string{"orders"}}, "")
				if err != nil {
					t.Fatal(err)
				}
				if got := res2.Extracted.Objects[0].Name; got != "public."+name+"_2" {
					t.Errorf("second restore named %s", got)
				}
			})

			t.Run("in place", func(t *testing.T) {
				res, err := restore(t, &engine.ExtractRequest{Objects: []string{"orders"}, InPlace: true}, "")
				if err != nil {
					t.Fatal(err)
				}
				if res.Extracted.Objects[0].Name != "public.orders" || pgSQLAll(t, dsn, "SELECT count(*)::text FROM orders") != "105" {
					t.Errorf("in place: %+v", res.Extracted.Objects)
				}
				if got := pgSQLAll(t, dsn, "SELECT nextval('orders_id_seq')::text"); got != "106" {
					t.Errorf("the sequence continues at %s", got)
				}
				_, err = restore(t, &engine.ExtractRequest{Objects: []string{"orders"}, InPlace: true}, "")
				var me *msg.Error
				if !errors.As(err, &me) || me.Message.ID != postgres.IDObjectExists {
					t.Errorf("in place over an existing table = %v", err)
				}
			})

			t.Run("missing table", func(t *testing.T) {
				_, err := restore(t, &engine.ExtractRequest{Objects: []string{"no_such_table"}}, "")
				var me *msg.Error
				if !errors.As(err, &me) || me.Message.ID != postgres.IDObjectMissing {
					t.Errorf("a missing table = %v", err)
				}
			})

			t.Run("rewind", func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "rewound")
				res, err := restore(t, nil, dir)
				if err != nil {
					t.Fatal(err)
				}
				conf, _ := os.ReadFile(filepath.Join(dir, "postgresql.conf")) // #nosec G304 -- the test's directory
				if strings.Contains(string(conf), "Written by pentym") || len(conf) == 0 {
					t.Error("the rewound cluster still has pentym's configuration")
				}
				if _, err := os.Stat(filepath.Join(dir, "recovery.signal")); !os.IsNotExist(err) {
					t.Error("the rewound cluster would start in recovery")
				}
				// It starts as it is, with production's roles, and holds
				// orders as before the drop.
				inst, err := set.Start(ctx, instance.Spec{Engine: "postgres", Version: version, DataDir: dir, RunID: res.RunID + "-check"})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = inst.Stop(context.Background()) }()
				c := inst.Conn()
				check := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", c.Host, c.Port, cfg.User, cfg.Password, cfg.Database)
				waitUntil(t, "the rewound cluster", time.Minute, func() bool {
					conn, err := pgconn.Connect(ctx, check)
					if err == nil {
						_ = conn.Close(ctx)
					}
					return err == nil
				})
				if got := pgSQLAll(t, check, "SELECT count(*)::text FROM orders"); got != "105" {
					t.Errorf("the rewound orders hold %s rows", got)
				}
				if got := pgSQLAll(t, check, "SELECT count(*)::text FROM pg_roles WHERE rolname = '"+postgres.SideRole+"'"); got != "0" {
					t.Error("pentym's side role is left in the rewound cluster")
				}
			})
		})
	}
}
