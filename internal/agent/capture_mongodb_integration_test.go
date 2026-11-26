// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/engine/mongodb"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Environment for a MongoDB capture child: the server's connection string
// without a user, and the backup user's password.
const (
	envMGURI = "PENTYM_IT_MG_URI"
	envMGPW  = "PENTYM_IT_MG_PW" // #nosec G101 -- the name of a variable, not a secret
)

func mgCaptureTarget(uri string) config.Target {
	ref, _ := secrets.ParseRef("env:" + envMGPW)
	return config.Target{Engine: "mongodb", URI: uri,
		Credentials: config.Credentials{Backup: config.Credential{User: "pentym_backup", Password: ref}}}
}

func mgEngine(dir string) *mongodb.Engine {
	return &mongodb.Engine{
		Resolver: secrets.Resolver{Getenv: os.Getenv},
		Runner:   &toolrun.Runner{SecretDir: filepath.Join(dir, "secrets")},
	}
}

// mgServer prepares a MongoDB replica set with pentym's backup user and
// returns a root client.
func mgServer(t *testing.T, mg *dbtest.MongoDB) *mongo.Client {
	t.Helper()
	admin, err := mongo.Connect(options.Client().ApplyURI(mg.URI))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Disconnect(context.Background()) })
	pw := make([]byte, 8)
	_, _ = rand.Read(pw)
	t.Setenv(envMGPW, "backup-"+hex.EncodeToString(pw))
	roles := bson.A{bson.D{{Key: "role", Value: "backup"}, {Key: "db", Value: "admin"}}, bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "local"}}}
	if err := admin.Database("admin").RunCommand(context.Background(), bson.D{
		{Key: "createUser", Value: "pentym_backup"}, {Key: "pwd", Value: os.Getenv(envMGPW)}, {Key: "roles", Value: roles},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	return admin
}

// majorityPoint is the replica set's majority commit point.
func majorityPoint(t *testing.T, admin *mongo.Client) repo.OplogTS {
	t.Helper()
	var h struct {
		LastWrite struct {
			MajorityOpTime struct {
				TS bson.Timestamp `bson:"ts"`
			} `bson:"majorityOpTime"`
		} `bson:"lastWrite"`
	}
	if err := admin.Database("admin").RunCommand(context.Background(), bson.D{{Key: "hello", Value: 1}}).Decode(&h); err != nil {
		t.Fatal(err)
	}
	ts := h.LastWrite.MajorityOpTime.TS
	return repo.OplogTS{T: ts.T, I: ts.I}
}

// oplogPos reads a checkpoint's oplog position.
func oplogPos(t *testing.T, cp *repo.Checkpoint) repo.OplogTS {
	t.Helper()
	ts, err := parseOplogTS(cp.Position)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// mgWriter inserts numbered documents into app.events until stopped, and
// returns the last number written.
func mgWriter(ctx context.Context, admin *mongo.Client, done chan<- int64) {
	c := admin.Database("app").Collection("events")
	next := int64(0)
	for ctx.Err() == nil {
		docs := make([]any, 200)
		for i := range docs {
			docs[i] = bson.D{{Key: "_id", Value: next + int64(i)}, {Key: "note", Value: "event"}}
		}
		if _, err := c.InsertMany(context.Background(), docs); err != nil {
			done <- -1
			return
		}
		next += int64(len(docs))
		time.Sleep(20 * time.Millisecond)
	}
	done <- next - 1
}

// storedInserts reads every stored oplog batch and returns the numbers
// inserted into app.events.
func storedInserts(t *testing.T, r *repo.Repository, tid string) map[int64]bool {
	t.Helper()
	batches, err := r.OplogBatches(context.Background(), tid)
	if err != nil || len(batches) == 0 {
		t.Fatalf("oplog batches = %d, %v", len(batches), err)
	}
	ids := map[int64]bool{}
	for _, b := range batches {
		data, err := r.GetSegment(context.Background(), b.Key)
		if err != nil {
			t.Fatal(err)
		}
		for len(data) > 0 {
			n := int(int32(data[0]) | int32(data[1])<<8 | int32(data[2])<<16 | int32(data[3])<<24)
			if n < 5 || n > len(data) {
				t.Fatalf("batch %s holds a malformed entry", b.Key)
			}
			inserts(bson.Raw(data[:n]), ids)
			data = data[n:]
		}
	}
	t.Logf("%d stored batches", len(batches))
	return ids
}

// inserts collects inserts into app.events, also inside applyOps.
func inserts(op bson.Raw, into map[int64]bool) {
	ns, _ := op.Lookup("ns").StringValueOK()
	kind, _ := op.Lookup("op").StringValueOK()
	if kind == "i" && ns == "app.events" {
		if id, ok := op.Lookup("o", "_id").Int64OK(); ok {
			into[id] = true
		}
	}
	if ops, ok := op.Lookup("o", "applyOps").ArrayOK(); ok && kind == "c" {
		vals, _ := ops.Values()
		for _, v := range vals {
			if d, ok := v.DocumentOK(); ok {
				inserts(d, into)
			}
		}
	}
}

// TestOplogCaptureSurvivesKills (Build Order C19): an agent capturing a
// busy replica set is killed at random points, then stopped gracefully;
// every insert since capture began is stored and no gap was needed.
func TestOplogCaptureSurvivesKills(t *testing.T) {
	for _, version := range dbtest.MongoDBVersions() {
		t.Run(version, func(t *testing.T) {
			mg := dbtest.StartMongoDB(t, version)
			admin := mgServer(t, mg)
			dir := newRepoDir(t)
			t.Setenv(envMGURI, mg.TargetURI)
			seed := uint64(time.Now().UnixNano())
			t.Logf("kill schedule seed %d", seed)
			rng := mrand.New(mrand.NewPCG(seed, seed+1)) // #nosec G404 -- a test schedule

			wctx, stopWriting := context.WithCancel(context.Background())
			lastID := make(chan int64, 1)
			go mgWriter(wctx, admin, lastID)
			t.Setenv("PENTYM_AGENT_CAPTURE_CHILD", dir)
			for round := range 5 {
				c := startChild(t, dir, fmt.Sprintf("k%d", round))
				waitFor(t, "the lease", 15*time.Second, func() bool { return c.saw("held") })
				// Long enough, now and then, for a batch to be stored.
				time.Sleep(time.Duration(1000+rng.IntN(14000)) * time.Millisecond)
				_ = c.cmd.Process.Kill()
				<-c.exit //nolint:errcheck // killed
			}
			stopWriting()
			last := <-lastID
			if last < 0 {
				t.Fatal("the writer failed")
			}
			end := majorityPoint(t, admin)

			c := startChild(t, dir, "final")
			waitFor(t, "the lease", 15*time.Second, func() bool { return c.saw("held") })
			caughtUp := time.Now()
			waitFor(t, "capture to catch up with the last insert", 5*time.Minute, func() bool {
				cp := checkpoint(t, dir)
				return cp != nil && !oplogPos(t, cp).Less(end)
			})
			t.Logf("the final agent caught up in %s", time.Since(caughtUp).Round(time.Second))
			_ = c.stdin.Close()
			if err := <-c.exit; err != nil {
				t.Fatalf("the final agent: %v: %s", err, c.output())
			}

			r := openAs(t, dir, nil, "admin")
			tid, _ := r.TargetID("prod-pg")
			gaps, err := r.Gaps(context.Background(), tid)
			if err != nil || len(gaps) != 0 {
				t.Errorf("gaps after kills = %+v, %v", gaps, err)
			}
			ids := storedInserts(t, r, tid)
			first := last + 1
			for id := range ids {
				first = min(first, id)
			}
			if first > last {
				t.Fatal("no insert was stored")
			}
			for id := first; id <= last; id++ {
				if !ids[id] {
					t.Fatalf("insert %d is missing; inserts %d to %d were stored around it", id, first, last)
				}
			}
			t.Logf("inserts %d to %d stored, of 0 to %d written", first, last, last)
		})
	}
}

// rollPast writes until the oplog's oldest entry is newer than ts.
func rollPast(t *testing.T, admin *mongo.Client, ts repo.OplogTS) {
	t.Helper()
	c := admin.Database("app").Collection("churn")
	pad := strings.Repeat("y", 8<<10)
	waitFor(t, "the oplog to roll over", 5*time.Minute, func() bool {
		docs := make([]any, 100)
		for i := range docs {
			docs[i] = bson.D{{Key: "pad", Value: pad}}
		}
		if _, err := c.InsertMany(context.Background(), docs); err != nil {
			t.Fatal(err)
		}
		var e struct {
			TS bson.Timestamp `bson:"ts"`
		}
		if err := admin.Database("local").Collection("oplog.rs").FindOne(context.Background(), bson.D{},
			options.FindOne().SetSort(bson.D{{Key: "$natural", Value: 1}})).Decode(&e); err != nil {
			t.Fatal(err)
		}
		return ts.Less(repo.OplogTS{T: e.TS.T, I: e.TS.I})
	})
}

// TestOplogRolloverThroughAgent: an oplog that rolled over while the agent
// was stopped is recorded as a gap; the agent forces a base backup and
// captures again.
func TestOplogRolloverThroughAgent(t *testing.T) {
	version := dbtest.MongoDBVersions()[0]
	mg := dbtest.StartMongoDBSmallOplog(t, version, 1)
	admin := mgServer(t, mg)
	dir := newRepoDir(t)
	cfg := &config.Config{Targets: map[string]config.Target{"prod-pg": mgCaptureTarget(mg.TargetURI)}}
	eng := mgEngine(t.TempDir())
	opts := func(b *backups) Options {
		return Options{Config: cfg, Repo: openAs(t, dir, nil, "agent"), Host: "a", Timings: fastTimings, Heartbeat: 100 * time.Millisecond,
			Engine: func(Target) (engine.Engine, error) { return eng, nil }, Backup: b.hook}
	}
	insert := func(note string) {
		if _, err := admin.Database("app").Collection("events").InsertOne(context.Background(), bson.D{{Key: "note", Value: note}}); err != nil {
			t.Fatal(err)
		}
	}
	first := start(t, opts(&backups{}))
	waitFor(t, "a checkpoint", mongodb.FlushInterval+30*time.Second, func() bool {
		insert("before")
		time.Sleep(time.Second)
		return checkpoint(t, dir) != nil
	})
	first.stop(t)
	before := checkpoint(t, dir)
	rollPast(t, admin, oplogPos(t, before))

	b := &backups{}
	start(t, opts(b))
	waitFor(t, "the forced backup", 30*time.Second, func() bool { return b.n.Load() == 1 })
	r := openAs(t, dir, nil, "admin")
	tid, _ := r.TargetID("prod-pg")
	gaps, err := r.Gaps(context.Background(), tid)
	if err != nil || len(gaps) != 1 || gaps[0].From != before.Position || !strings.Contains(gaps[0].Reason, "rolled over") {
		t.Fatalf("gaps = %+v, %v", gaps, err)
	}
	waitFor(t, "capture after the gap", mongodb.FlushInterval+30*time.Second, func() bool {
		insert("after")
		time.Sleep(time.Second)
		cp := checkpoint(t, dir)
		return cp != nil && cp.Generation == 2
	})
}
