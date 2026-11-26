// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package mongodb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// server is a MongoDB container with pentym's users and some data.
type server struct {
	mg     *dbtest.MongoDB
	admin  *mongo.Client
	env    map[string]string
	writes atomic.Int64
}

// users are created on every test server: the minimal backup user, one
// with too much privilege, and one that can read only one database.
var users = map[string]bson.A{
	"pentym_backup": {bson.D{{Key: "role", Value: "backup"}, {Key: "db", Value: "admin"}}, bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "local"}}},
	"pentym_root":   {bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}},
	"pentym_app":    {bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "app"}}},
}

func setup(t *testing.T, mg *dbtest.MongoDB) *server {
	t.Helper()
	ctx := context.Background()
	admin, err := mongo.Connect(options.Client().ApplyURI(mg.URI))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Disconnect(context.Background()) })
	s := &server{mg: mg, admin: admin, env: map[string]string{}}
	for user, roles := range users {
		pw := user + "-" + randomHex(8)
		s.env[strings.ToUpper(user)] = pw
		if err := admin.Database("admin").RunCommand(ctx, bson.D{{Key: "createUser", Value: user}, {Key: "pwd", Value: pw}, {Key: "roles", Value: roles}}).Err(); err != nil {
			t.Fatalf("create %s: %v", user, err)
		}
	}
	orders := admin.Database("app").Collection("orders")
	docs := make([]any, 0, 1000)
	for i := range 20000 {
		docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "note", Value: randomHex(32)}})
		if len(docs) == cap(docs) {
			if _, err := orders.InsertMany(ctx, docs); err != nil {
				t.Fatal(err)
			}
			docs = docs[:0]
		}
	}
	if err := admin.Database("app").CreateView(ctx, "recent", "orders", bson.A{bson.D{{Key: "$limit", Value: 10}}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *server) target(user string) engine.Target {
	ref, _ := secrets.ParseRef("env:" + strings.ToUpper(user))
	return engine.Target{Name: "prod-mg", ID: "0123456789abcdef", Config: config.Target{
		Engine: "mongodb", URI: s.mg.TargetURI,
		Credentials: config.Credentials{Backup: config.Credential{User: user, Password: ref}},
	}}
}

func (s *server) engine(t *testing.T) *Engine {
	t.Helper()
	var dirs []string
	if d := os.Getenv("PENTYM_TEST_MONGODB_BIN"); d != "" {
		dirs = []string{d}
	}
	return &Engine{
		Resolver: secrets.Resolver{Getenv: func(k string) string { return s.env[k] }},
		Runner:   &toolrun.Runner{SecretDir: filepath.Join(t.TempDir(), "secrets")},
		ToolDirs: dirs, RunID: "it",
	}
}

// write keeps writing until ctx ends, so the oplog moves during a dump.
func (s *server) write(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	c := s.admin.Database("app").Collection("events")
	for ctx.Err() == nil {
		if _, err := c.InsertOne(ctx, bson.D{{Key: "at", Value: time.Now()}}); err == nil {
			s.writes.Add(1)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// cancelSink cancels the backup once more than limit bytes arrived.
type cancelSink struct {
	memSink
	limit  int
	cancel context.CancelFunc
}

type countingWriter struct {
	w io.Writer
	s *cancelSink
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.n += len(p); c.n > c.s.limit {
		c.s.cancel()
	}
	return c.w.Write(p)
}

func (s *cancelSink) Archive(ctx context.Context, name string) (io.Writer, error) {
	w, err := s.memSink.Archive(ctx, name)
	return &countingWriter{w: w, s: s}, err
}

func findings(res *engine.CheckResult, level msg.Level) []msg.ID {
	var ids []msg.ID
	for _, f := range res.Findings {
		if f.Level == level {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

func TestReplicaSet(t *testing.T) {
	for _, version := range dbtest.MongoDBVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			s := setup(t, dbtest.StartMongoDB(t, version))
			e := s.engine(t)

			res, err := e.Check(ctx, s.target("pentym_backup"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(res.ServerVersion, version+".") || res.Role != "pentym_backup" || len(res.Findings) != 0 {
				t.Errorf("check as pentym_backup = %+v", res)
			}
			res, err = e.Check(ctx, s.target("pentym_root"))
			if err != nil || len(findings(res, msg.LevelFailed)) != 0 || len(findings(res, msg.LevelWarning)) != 1 || res.Findings[0].ID != IDTooMuchPrivilege {
				t.Errorf("check as pentym_root = %+v, %v", res, err)
			}
			res, err = e.Check(ctx, s.target("pentym_app"))
			if err != nil || len(findings(res, msg.LevelFailed)) != 2 {
				t.Errorf("check as pentym_app (no backup role, no oplog) = %+v, %v", res, err)
			}
			bad := s.target("pentym_backup")
			s.env["WRONG"] = "wrong-password"
			bad.Config.Credentials.Backup.Password, _ = secrets.ParseRef("env:WRONG")
			var me *msg.Error
			if _, err := e.Check(ctx, bad); !errors.As(err, &me) || me.Message.ID != IDConnect || me.Code != msg.ExitAccess {
				t.Errorf("a wrong password = %v", err)
			}

			raw, err := e.Inventory(ctx, s.target("pentym_backup"))
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
				if internalDBs[inv.Databases[i].Name] {
					t.Errorf("the inventory lists %s", inv.Databases[i].Name)
				}
			}
			if inv.ReplicaSet != s.mg.ReplicaSet || app == nil || app.Collections != 1 || app.Views != 1 || app.EstimatedDocuments != 20000 || app.Error != "" {
				t.Errorf("inventory = %s", raw)
			}

			// A snapshot while the database is being written to.
			wctx, stop := context.WithCancel(ctx)
			var wg sync.WaitGroup
			wg.Add(1)
			go s.write(wctx, &wg)
			sink := &memSink{}
			bb, err := e.BaseBackup(ctx, s.target("pentym_backup"), sink)
			stop()
			wg.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if bb.Method != Method || !strings.HasPrefix(bb.EngineVersion, version+".") || bb.SystemID != s.mg.ReplicaSet ||
				bb.Start == "" || bb.End == "" || bb.Timeline != "" {
				t.Errorf("base backup = %+v", bb)
			}
			// L1: the stored stream parses as a whole archive.
			sum, err := ReadArchive(bytes.NewReader(sink.data[ArchiveName].Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if position(sum.OplogFirst) != bb.Start || position(sum.OplogLast) != bb.End || sum.Namespaces < 2 {
				t.Errorf("archive = %+v, backup %s..%s", sum, bb.Start, bb.End)
			}
			t.Logf("%d bytes, %d namespaces, %d oplog entries, %d writes during the test", sink.data[ArchiveName].Len(), sum.Namespaces, sum.OplogEntries, s.writes.Load())

			// Cancelled part-way: an error, and no secret file left.
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if _, err := e.BaseBackup(cctx, s.target("pentym_backup"), &cancelSink{limit: 64 << 10, cancel: cancel}); err == nil {
				t.Error("the cancelled backup succeeded")
			}
			if entries, _ := os.ReadDir(e.Runner.SecretDir); len(entries) != 0 {
				t.Errorf("secret files left: %v", entries)
			}
		})
	}
}

func TestStandalone(t *testing.T) {
	for _, version := range dbtest.MongoDBVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			s := setup(t, dbtest.StartMongoDBStandalone(t, version))
			e := s.engine(t)
			res, err := e.Check(ctx, s.target("pentym_backup"))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Findings) != 1 || res.Findings[0].ID != IDStandalone || res.Findings[0].Level != msg.LevelWarning {
				t.Errorf("check = %+v", res)
			}
			sink := &memSink{}
			bb, err := e.BaseBackup(ctx, s.target("pentym_backup"), sink)
			if err != nil {
				t.Fatal(err)
			}
			if bb.Method != Method || bb.SystemID != "" || bb.Start != "" || bb.End != "" {
				t.Errorf("base backup = %+v", bb)
			}
			sum, err := ReadArchive(bytes.NewReader(sink.data[ArchiveName].Bytes()))
			if err != nil || sum.OplogEntries != 0 || sum.Namespaces < 1 {
				t.Errorf("archive = %+v, %v", sum, err)
			}
		})
	}
}
