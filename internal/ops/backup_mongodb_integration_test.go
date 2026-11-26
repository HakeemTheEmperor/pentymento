// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package ops

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/engine/mongodb"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// mgServer prepares a MongoDB server for backups: a minimal backup user
// and an app database holding about 60 MB in one collection.
func mgServer(t *testing.T, mg *dbtest.MongoDB) (engine.Target, *mongodb.Engine, *mongo.Client) {
	t.Helper()
	ctx := context.Background()
	admin, err := mongo.Connect(options.Client().ApplyURI(mg.URI))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Disconnect(context.Background()) })
	pw := make([]byte, 12)
	_, _ = rand.Read(pw)
	password := "backup-" + hex.EncodeToString(pw)
	roles := bson.A{bson.D{{Key: "role", Value: "backup"}, {Key: "db", Value: "admin"}}}
	if mg.ReplicaSet != "" {
		roles = append(roles, bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "local"}})
	}
	if err := admin.Database("admin").RunCommand(ctx, bson.D{{Key: "createUser", Value: "pentym_backup"},
		{Key: "pwd", Value: password}, {Key: "roles", Value: roles}}).Err(); err != nil {
		t.Fatal(err)
	}
	orders := admin.Database("app").Collection("orders")
	note := make([]byte, 64)
	docs := make([]any, 0, 5000)
	for i := range 400000 {
		_, _ = rand.Read(note)
		docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "note", Value: hex.EncodeToString(note)}})
		if len(docs) == cap(docs) {
			if _, err := orders.InsertMany(ctx, docs); err != nil {
				t.Fatal(err)
			}
			docs = docs[:0]
		}
	}
	ref, _ := secrets.ParseRef("env:MG_PW")
	target := engine.Target{Name: "prod-mg", Config: config.Target{
		Engine: "mongodb", URI: mg.TargetURI,
		Credentials: config.Credentials{Backup: config.Credential{User: "pentym_backup", Password: ref}},
	}}
	var dirs []string
	if d := os.Getenv("PENTYM_TEST_MONGODB_BIN"); d != "" {
		dirs = []string{d}
	}
	eng := &mongodb.Engine{
		Resolver: secrets.Resolver{Getenv: func(k string) string {
			if k == "MG_PW" {
				return password
			}
			return ""
		}},
		Runner:   &toolrun.Runner{SecretDir: filepath.Join(t.TempDir(), "secrets")},
		ToolDirs: dirs,
	}
	return target, eng, admin
}

// reassembleArchive reads a MongoDB snapshot back and parses it as a
// whole mongodump archive (verify L1).
func reassembleArchive(t *testing.T, f *fixture, s *repo.Snapshot) *mongodb.ArchiveSummary {
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
	if loaded.Name != mongodb.ArchiveName || len(loaded.Parts) != 0 {
		t.Errorf("streams = %s and %d parts", loaded.Name, len(loaded.Parts))
	}
	var buf bytes.Buffer
	if err := admin.ReadPart(ctx, idx, loaded, mongodb.ArchiveName, &buf); err != nil {
		t.Fatal(err)
	}
	if int64(buf.Len()) != loaded.Size {
		t.Errorf("the archive is %d bytes, the snapshot records %d", buf.Len(), loaded.Size)
	}
	sum, err := mongodb.ReadArchive(&buf)
	if err != nil {
		t.Fatalf("the reassembled archive does not parse: %v", err)
	}
	return sum
}

// TestMongoDBBackups backs up each supported MongoDB version as a replica
// set (Build Order C14): the snapshot reassembles, a second backup after
// a small change deduplicates, and a cancelled backup commits nothing.
func TestMongoDBBackups(t *testing.T) {
	for _, version := range dbtest.MongoDBVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			target, eng, admin := mgServer(t, dbtest.StartMongoDB(t, version))
			f := newFixture(t)
			b := f.backup(eng)
			b.Target = target

			res, err := b.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s := res.Snapshot
			if s.Engine != "mongodb" || !strings.HasPrefix(s.EngineVersion, version+".") || s.Method != mongodb.Method ||
				s.StartPosition == "" || s.EndPosition == "" || s.SystemID == "" || len(s.Inventory) == 0 || len(res.Warnings) != 0 {
				t.Errorf("snapshot = %+v, warnings %+v", s, res.Warnings)
			}
			sum := reassembleArchive(t, f, s)
			if string(mongodb.Position(sum.OplogLast)) != s.EndPosition {
				t.Errorf("the archive's oplog ends at %v, the snapshot at %s", sum.OplogLast, s.EndPosition)
			}
			t.Logf("first backup: %d MiB, %d MiB new", res.Stats.Bytes>>20, res.Stats.NewBytes>>20)

			// A small change to a contiguous range of documents.
			if _, err := admin.Database("app").Collection("orders").UpdateMany(ctx,
				bson.D{{Key: "_id", Value: bson.D{{Key: "$lt", Value: 2000}}}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "note", Value: "changed"}}}}); err != nil {
				t.Fatal(err)
			}
			b2 := f.backup(eng)
			b2.Target = target
			res2, err := b2.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			reassembleArchive(t, f, res2.Snapshot)
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
			if entries, _ := os.ReadDir(eng.Runner.SecretDir); len(entries) != 0 {
				t.Errorf("secret files left: %v", entries)
			}
			f.noLocks(t)
		})
	}
}

// TestMongoDBStandaloneBackup backs up a standalone server: a snapshot
// with no log positions, and the snapshots-only warning.
func TestMongoDBStandaloneBackup(t *testing.T) {
	for _, version := range dbtest.MongoDBVersions() {
		t.Run(version, func(t *testing.T) {
			target, eng, _ := mgServer(t, dbtest.StartMongoDBStandalone(t, version))
			f := newFixture(t)
			b := f.backup(eng)
			b.Target = target
			res, err := b.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			s := res.Snapshot
			if s.StartPosition != "" || s.EndPosition != "" || s.SystemID != "" || s.Method != mongodb.Method {
				t.Errorf("snapshot = %+v", s)
			}
			if len(res.Warnings) != 1 || res.Warnings[0].ID != mongodb.IDStandalone || res.Warnings[0].Level != msg.LevelWarning {
				t.Errorf("warnings = %+v", res.Warnings)
			}
			if sum := reassembleArchive(t, f, s); sum.OplogEntries != 0 {
				t.Errorf("a standalone archive holds %d oplog entries", sum.OplogEntries)
			}
		})
	}
}
