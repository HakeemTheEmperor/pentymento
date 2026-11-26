// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package ops

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/engine/postgres"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// pgSQL runs SQL on a server as its superuser.
func pgSQL(t *testing.T, dsn, sql string) [][]byte {
	t.Helper()
	conn, err := pgconn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	res, err := conn.Exec(context.Background(), sql).ReadAll()
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if len(res) > 0 && len(res[len(res)-1].Rows) > 0 {
		return res[len(res)-1].Rows[0]
	}
	return nil
}

// pgServer starts a server that allows replication connections, with a
// minimal backup role and an app database holding about 100 MB.
func pgServer(t *testing.T, version string) (engine.Target, *postgres.Engine, string) {
	t.Helper()
	pg := dbtest.StartPostgres(t, version)
	hba := string(pgSQL(t, pg.ConnString, "SELECT current_setting('hba_file')")[0])
	pg.Exec(t, "sh", "-c", "echo 'host replication all all scram-sha-256' >> "+hba)
	pgSQL(t, pg.ConnString, "SELECT pg_reload_conf()")
	pgSQL(t, pg.ConnString, "CREATE ROLE pentym_backup LOGIN REPLICATION PASSWORD 'backup-pw'")
	pgSQL(t, pg.ConnString, "CREATE TABLE orders AS SELECT i AS id, md5(i::text) AS note FROM generate_series(1, 1500000) AS i")
	// Set hint bits and the visibility map now, so later scans do not
	// rewrite every page and defeat deduplication between backups.
	pgSQL(t, pg.ConnString, "VACUUM (FREEZE, ANALYZE) orders")
	pgSQL(t, pg.ConnString, "CHECKPOINT")
	cfg, err := pgconn.ParseConfig(pg.ConnString)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := secrets.ParseRef("env:BK_PW")
	target := engine.Target{Name: "prod-pg", Config: config.Target{
		Engine: "postgres", Host: cfg.Host, Port: int(cfg.Port), SSLMode: "disable",
		Credentials: config.Credentials{Backup: config.Credential{User: "pentym_backup", Password: ref}},
	}}
	eng := &postgres.Engine{
		Resolver: secrets.Resolver{Getenv: func(k string) string {
			if k == "BK_PW" {
				return "backup-pw"
			}
			return ""
		}},
		Runner: &toolrun.Runner{SecretDir: filepath.Join(t.TempDir(), "secrets")},
	}
	return target, eng, pg.ConnString
}

// reassemble reads every part of a snapshot back and checks it (verify
// L1): the archives are valid tar streams holding a cluster, and the
// manifest is JSON.
func reassemble(t *testing.T, f *fixture, s *repo.Snapshot) {
	t.Helper()
	ctx := context.Background()
	admin := f.admin(t)
	idx, err := admin.LoadIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := admin.LoadSnapshot(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{loaded.Name}
	for _, p := range loaded.Parts {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{postgres.BaseArchive, postgres.ManifestArchive}) {
		t.Errorf("parts = %v", names)
	}
	var base, manifest bytes.Buffer
	if err := admin.ReadPart(ctx, idx, loaded, postgres.BaseArchive, &base); err != nil {
		t.Fatal(err)
	}
	if err := admin.ReadPart(ctx, idx, loaded, postgres.ManifestArchive, &manifest); err != nil {
		t.Fatal(err)
	}
	if int64(base.Len()) != loaded.Size {
		t.Errorf("base.tar is %d bytes, the snapshot records %d", base.Len(), loaded.Size)
	}
	tr := tar.NewReader(&base)
	var members []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("base.tar does not parse after %d members: %v", len(members), err)
		}
		members = append(members, h.Name)
	}
	if !slices.Contains(members, "PG_VERSION") || !slices.Contains(members, "backup_label") {
		t.Errorf("base.tar lacks PG_VERSION or backup_label (%d members)", len(members))
	}
	if !json.Valid(manifest.Bytes()) {
		t.Error("the manifest is not JSON")
	}
}

// TestPostgresBackups backs up each supported Postgres version (Build
// Order C13): the snapshot reassembles, a second backup after a small
// change deduplicates, and a cancelled backup commits nothing.
func TestPostgresBackups(t *testing.T) {
	for _, version := range dbtest.PostgresVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			target, eng, dsn := pgServer(t, version)
			f := newFixture(t)
			b := f.backup(eng)
			b.Target = target

			res, err := b.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s := res.Snapshot
			if !strings.HasPrefix(s.EngineVersion, version+".") || s.StartPosition == "" || s.EndPosition == "" || s.Timeline != "1" || s.SystemID == "" {
				t.Errorf("snapshot = %+v", s)
			}
			reassemble(t, f, s)
			t.Logf("first backup: %d MiB, %d MiB new", res.Stats.Bytes>>20, res.Stats.NewBytes>>20)

			// A small change to a contiguous range of rows, then a second
			// backup.
			pgSQL(t, dsn, "UPDATE orders SET note = 'changed' WHERE id <= 2000")
			b2 := f.backup(eng)
			b2.Target = target
			res2, err := b2.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			reassemble(t, f, res2.Snapshot)
			t.Logf("second backup: %d MiB, %d MiB new", res2.Stats.Bytes>>20, res2.Stats.NewBytes>>20)
			if res2.Stats.NewBytes > res.Stats.NewBytes/2 {
				t.Errorf("the second backup stored %d MiB of %d MiB; the first stored %d MiB", res2.Stats.NewBytes>>20, res2.Stats.Bytes>>20, res.Stats.NewBytes>>20)
			}

			// Cancelled part-way: nothing is committed.
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			var seen int64
			b3 := f.backup(eng)
			b3.Target = target
			b3.OnBytes = func(n int64) {
				if seen += n; seen > 8<<20 {
					cancel()
				}
			}
			if _, err := b3.Run(cctx); err == nil {
				t.Fatal("the cancelled backup succeeded")
			}
			if snaps := f.snapshots(t); len(snaps) != 2 {
				t.Errorf("%d snapshots after the cancelled backup, want 2", len(snaps))
			}
			deadline := time.Now().Add(20 * time.Second)
			for {
				n := string(pgSQL(t, dsn, "SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'pentym_bb_%'")[0])
				if n == "0" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s temporary slots left after the cancelled backup", n)
				}
				time.Sleep(200 * time.Millisecond)
			}
		})
	}
}
