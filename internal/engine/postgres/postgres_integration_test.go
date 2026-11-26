// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package postgres

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// server is a Postgres container with pentym's roles.
type server struct {
	pg   *dbtest.Postgres
	host string
	port int
	env  map[string]string
}

func startServer(t *testing.T, version string) *server {
	t.Helper()
	pg := dbtest.StartPostgres(t, version)
	cfg, err := pgconn.ParseConfig(pg.ConnString)
	if err != nil {
		t.Fatal(err)
	}
	return &server{pg: pg, host: cfg.Host, port: int(cfg.Port), env: map[string]string{
		"SU_PW": cfg.Password, "BK_PW": "backup-pw-" + randomHex(8), "NR_PW": "norepl-pw-" + randomHex(8),
	}}
}

func (s *server) target(user, pwEnv, db string) engine.Target {
	ref, _ := secrets.ParseRef("env:" + pwEnv)
	return engine.Target{Name: "prod-pg", ID: "0123456789abcdef", Config: config.Target{
		Engine: "postgres", Host: s.host, Port: s.port, Database: db, SSLMode: "disable",
		Credentials: config.Credentials{Backup: config.Credential{User: user, Password: ref}},
	}}
}

func (s *server) superuser() engine.Target { return s.target("pentym_test", "SU_PW", "pentym_test") }

func (s *server) engine(t *testing.T) *Engine {
	t.Helper()
	var dirs []string
	if d := os.Getenv("PENTYM_TEST_POSTGRES_BIN"); d != "" {
		dirs = filepath.SplitList(d)
	}
	return &Engine{
		Resolver: secrets.Resolver{Getenv: func(k string) string { return s.env[k] }},
		Runner:   &toolrun.Runner{SecretDir: filepath.Join(t.TempDir(), "secrets")},
		ToolDirs: dirs, RunID: "it",
	}
}

// exec runs SQL as the superuser.
func (s *server) exec(t *testing.T, e *Engine, sql string) [][]string {
	t.Helper()
	conn, err := e.connect(context.Background(), s.superuser(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "SELECT") {
		rows, err := query(context.Background(), conn, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return rows
	}
	if _, err := conn.Exec(context.Background(), sql).ReadAll(); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return nil
}

func findingIDs(r *engine.CheckResult) []msg.ID {
	var ids []msg.ID
	for _, f := range r.Findings {
		ids = append(ids, f.ID)
	}
	return ids
}

// tarNames lists an archive's member names.
func tarNames(t *testing.T, b []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("archive does not parse as tar after %d members: %v", len(names), err)
		}
		names = append(names, h.Name)
	}
}

// waitNoSlots waits until no temporary backup slot is left.
func (s *server) waitNoSlots(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		n := s.exec(t, e, "SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'pentym_bb_%'")[0][0]
		if n == "0" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s temporary backup slots left", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// checkBaseTar checks base.tar holds a cluster and the WAL it needs.
func checkBaseTar(t *testing.T, b []byte) {
	t.Helper()
	names := tarNames(t, b)
	wal := 0
	for _, n := range names {
		if strings.HasPrefix(n, "pg_wal/") && len(filepath.Base(n)) == 24 {
			wal++
		}
	}
	if !slices.Contains(names, "PG_VERSION") || !slices.Contains(names, "backup_label") || wal == 0 {
		t.Errorf("base.tar: PG_VERSION %v, backup_label %v, %d WAL segments", slices.Contains(names, "PG_VERSION"), slices.Contains(names, "backup_label"), wal)
	}
}

func TestPostgres(t *testing.T) {
	for _, version := range dbtest.PostgresVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(t, version)
			e := s.engine(t)

			// As the container's superuser, before replication is allowed.
			r, err := e.Check(ctx, s.superuser())
			if err != nil {
				t.Fatal(err)
			}
			ids := findingIDs(r)
			if !slices.Contains(ids, IDTooMuchPrivilege) || !slices.Contains(ids, IDReplicationHBA) || !strings.HasPrefix(r.ServerVersion, version+".") {
				t.Errorf("superuser check = %+v", r)
			}

			// Allow replication connections, and add pentym's roles.
			hba := s.exec(t, e, "SELECT current_setting('hba_file')")[0][0]
			s.pg.Exec(t, "sh", "-c", "echo 'host replication all all scram-sha-256' >> "+hba)
			s.exec(t, e, "SELECT pg_reload_conf()")
			s.exec(t, e, "CREATE ROLE pentym_backup LOGIN REPLICATION PASSWORD '"+s.env["BK_PW"]+"'")
			s.exec(t, e, "CREATE ROLE norepl LOGIN PASSWORD '"+s.env["NR_PW"]+"'")
			s.exec(t, e, "CREATE DATABASE app")
			backup := s.target("pentym_backup", "BK_PW", "postgres")

			if r, err := e.Check(ctx, backup); err != nil || len(r.Findings) != 0 {
				t.Errorf("minimal role check = %+v, %v", r, err)
			}
			if r, err := e.Check(ctx, s.target("norepl", "NR_PW", "postgres")); err != nil || !slices.Contains(findingIDs(r), IDNoReplication) {
				t.Errorf("role without REPLICATION = %+v, %v", r, err)
			}
			if _, err := e.Check(ctx, s.target("pentym_backup", "SU_PW", "postgres")); err == nil {
				t.Error("a wrong password passed the check")
			} else {
				var me *msg.Error
				if !errors.As(err, &me) || me.Code != msg.ExitAccess || strings.Contains(err.Error(), s.env["SU_PW"]) {
					t.Errorf("wrong password = %v", err)
				}
			}

			t.Run("inventory", func(t *testing.T) {
				appConn := s.target("pentym_test", "SU_PW", "app")
				conn, err := e.connect(ctx, appConn, false)
				if err != nil {
					t.Fatal(err)
				}
				_, err = conn.Exec(ctx, "CREATE TABLE orders AS SELECT generate_series(1, 1000) AS id; ANALYZE orders").ReadAll()
				_ = conn.Close(ctx)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := e.Inventory(ctx, backup)
				if err != nil {
					t.Fatal(err)
				}
				var inv Inventory
				if err := json.Unmarshal(raw, &inv); err != nil {
					t.Fatal(err)
				}
				var app *DatabaseSummary
				for i := range inv.Databases {
					if inv.Databases[i].Name == "app" {
						app = &inv.Databases[i]
					}
				}
				if app == nil || app.Tables != 1 || app.EstimatedRows != 1000 || app.SizeBytes == 0 || inv.SystemID == "" {
					t.Errorf("inventory = %s", raw)
				}
			})

			t.Run("native", func(t *testing.T) {
				sink := &memSink{}
				bb, err := e.BaseBackup(ctx, backup, sink)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(sink.names, []string{BaseArchive, ManifestArchive}) {
					t.Errorf("archives = %v", sink.names)
				}
				checkBaseTar(t, sink.data[BaseArchive].Bytes())
				var manifest struct {
					Version int `json:"PostgreSQL-Backup-Manifest-Version"`
				}
				if err := json.Unmarshal(sink.data[ManifestArchive].Bytes(), &manifest); err != nil || manifest.Version == 0 {
					t.Errorf("manifest: %v", err)
				}
				start, err1 := pglogrepl.ParseLSN(string(bb.Start))
				end, err2 := pglogrepl.ParseLSN(string(bb.End))
				if err1 != nil || err2 != nil || end <= start || bb.Timeline != "1" || bb.Method != "native" ||
					bb.SystemID == "" || !strings.HasPrefix(bb.EngineVersion, version+".") {
					t.Errorf("base backup = %+v", bb)
				}
				s.waitNoSlots(t, e)
			})

			t.Run("cancelled", func(t *testing.T) {
				cctx, cancel := context.WithCancel(ctx)
				defer cancel()
				_, err := e.BaseBackup(cctx, backup, &cancelSink{cancel: cancel})
				if err == nil {
					t.Fatal("a cancelled backup succeeded")
				}
				s.waitNoSlots(t, e)
			})

			pgBin := ""
			if p, err := e.findTool("pg_basebackup"); err == nil {
				if v, err := e.Runner.ToolVersion(ctx, "pg_basebackup", p); err == nil && strconv.Itoa(v.Major) == version {
					pgBin = p
				}
			}
			t.Run("pg_basebackup", func(t *testing.T) {
				if pgBin == "" {
					if strings.Contains(os.Getenv("PENTYM_TEST_PROVIDERS"), "local") {
						t.Fatalf("pg_basebackup %s is required but not found", version)
					}
					t.Skipf("no pg_basebackup %s", version)
				}
				sink := &memSink{}
				bb, err := e.pgBasebackup(ctx, backup, sink)
				if err != nil {
					t.Fatal(err)
				}
				checkBaseTar(t, sink.data[BaseArchive].Bytes())
				if bb.Start == "" || bb.End == "" || bb.Timeline != "1" || bb.SystemID == "" || !strings.HasPrefix(bb.EngineVersion, version+".") {
					t.Errorf("base backup = %+v", bb)
				}
			})

			t.Run("tablespace", func(t *testing.T) {
				s.pg.Exec(t, "sh", "-c", "mkdir -p /var/lib/postgresql/ts && chown postgres /var/lib/postgresql/ts")
				s.exec(t, e, "CREATE TABLESPACE ts LOCATION '/var/lib/postgresql/ts'")
				oid := s.exec(t, e, "SELECT oid FROM pg_tablespace WHERE spcname = 'ts'")[0][0]
				s.exec(t, e, "CREATE TABLE spaced TABLESPACE ts AS SELECT generate_series(1, 100) AS i")
				sink := &memSink{}
				if _, err := e.BaseBackup(ctx, backup, sink); err != nil {
					t.Fatal(err)
				}
				if len(sink.names) != 3 || !slices.Contains(sink.names, oid+".tar") || sink.names[len(sink.names)-1] != ManifestArchive {
					t.Errorf("archives = %v, want base.tar, %s.tar and the manifest", sink.names, oid)
				}
				names := tarNames(t, sink.data[BaseArchive].Bytes())
				if !slices.Contains(names, "tablespace_map") {
					t.Error("base.tar has no tablespace_map")
				}
				if len(tarNames(t, sink.data[oid+".tar"].Bytes())) == 0 {
					t.Error("the tablespace archive is empty")
				}
				if pgBin != "" {
					if _, err := e.pgBasebackup(ctx, backup, &memSink{}); err == nil {
						t.Error("pg_basebackup to stdout succeeded with a tablespace")
					}
				}
			})
		})
	}
}

// cancelSink cancels the backup as soon as data arrives.
type cancelSink struct{ cancel func() }

func (c *cancelSink) Archive(context.Context, string) (io.Writer, error) { return c, nil }

func (c *cancelSink) Write(p []byte) (int, error) {
	c.cancel()
	return len(p), nil
}
