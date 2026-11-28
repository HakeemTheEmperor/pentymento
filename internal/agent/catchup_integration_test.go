// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/engine/mongodb"
	"github.com/HakeemTheEmperor/pentymento/internal/engine/postgres"
	"github.com/HakeemTheEmperor/pentymento/internal/ops"
)

// TestCatchUpAfterDowntimePostgres (Build Order C20): WAL written while no
// agent ran is drained by catch-up, with no hole and no gap.
func TestCatchUpAfterDowntimePostgres(t *testing.T) {
	version := dbtest.PostgresVersions()[0]
	dsn, host, port := pgServer(t, version)
	dir := newRepoDir(t)
	cfg := &config.Config{Targets: map[string]config.Target{"prod-pg": pgCaptureTarget(host, port)}}
	eng := pgEngine(t.TempDir())
	opts := Options{Config: cfg, Repo: openAs(t, dir, nil, "agent"), Host: "a", Timings: fastTimings, Heartbeat: 100 * time.Millisecond,
		Engine: func(Target) (engine.Engine, error) { return eng, nil }}
	exec := func(sql string) string {
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
			return string(res[len(res)-1].Rows[0][0])
		}
		return ""
	}
	a := start(t, opts)
	exec("INSERT INTO events (note) VALUES ('before')")
	waitFor(t, "a checkpoint", postgres.PartialInterval+15*time.Second, func() bool { return checkpoint(t, dir) != nil })
	a.stop(t)
	before := checkpoint(t, dir)

	// The agent is down: write, across several WAL files.
	for range 5 {
		exec("INSERT INTO events (note) SELECT md5(g::text) FROM generate_series(1, 20000) AS g")
		exec("SELECT pg_switch_wal()")
	}
	exec("INSERT INTO events (note) VALUES ('last')")
	end := exec("SELECT pg_current_wal_flush_lsn()::text")

	// status: the agent stopped, the window ends at its checkpoint.
	ts, err := ops.ReadTargetStatus(context.Background(), openAs(t, dir, nil, "admin"), cfg, "prod-pg", nil, nil, time.Now())
	if err != nil || ts.Agent.State != ops.AgentStopped || ts.Checkpoint == nil || ts.Checkpoint.Position != before.Position || !ts.Window.Logs {
		t.Fatalf("status before catch-up = %+v, %v", ts, err)
	}

	co := opts
	co.Repo, co.Host = openAs(t, dir, nil, "agent"), "cron"
	res, err := CatchUp(context.Background(), co, "prod-pg", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("catch-up stored %d segments, %d bytes, in %s", res.Segments, res.Bytes, res.Duration.Round(time.Millisecond))
	if res.Gap != nil || res.From != engine.Position(before.Position) || res.Segments < 5 {
		t.Errorf("result = %+v", res)
	}
	_, lsn, _ := strings.Cut(string(res.To), "/")
	if parseLSNText(t, lsn) < parseLSNText(t, end) {
		t.Errorf("catch-up reached %s, short of the last commit %s", res.To, end)
	}
	r := openAs(t, dir, nil, "admin")
	tid, _ := r.TargetID("prod-pg")
	if gaps, err := r.Gaps(context.Background(), tid); err != nil || len(gaps) != 0 {
		t.Errorf("gaps = %+v, %v", gaps, err)
	}
	coverage(t, r, tid, parseLSNText(t, end))
	ts, err = ops.ReadTargetStatus(context.Background(), r, cfg, "prod-pg", nil, nil, time.Now())
	if err != nil || ts.Checkpoint == nil || ts.Checkpoint.Position != string(res.To) || ts.Agent.State != ops.AgentStopped ||
		ts.Agent.Generation != res.Generation || ts.Window.Break != "" {
		t.Errorf("status after catch-up = %+v, %v", ts, err)
	}
}

// TestCatchUpAfterCrashMongoDB (Build Order C20): an agent is killed
// without releasing its lease; inserts made while it is down are drained
// by catch-up once the lease expires, with none missing.
func TestCatchUpAfterCrashMongoDB(t *testing.T) {
	version := dbtest.MongoDBVersions()[0]
	mg := dbtest.StartMongoDB(t, version)
	admin := mgServer(t, mg)
	dir := newRepoDir(t)
	t.Setenv(envMGURI, mg.TargetURI)
	t.Setenv("PENTYM_AGENT_CAPTURE_CHILD", dir)

	wctx, stopWriting := context.WithCancel(context.Background())
	lastID := make(chan int64, 1)
	go mgWriter(wctx, admin, lastID)
	c := startChild(t, dir, "crashing")
	waitFor(t, "the lease", 15*time.Second, func() bool { return c.saw("held") })
	waitFor(t, "a checkpoint", mongodb.FlushInterval+30*time.Second, func() bool { return checkpoint(t, dir) != nil })
	_ = c.cmd.Process.Kill()
	<-c.exit                    //nolint:errcheck // killed
	time.Sleep(3 * time.Second) // writes while no agent runs
	stopWriting()
	last := <-lastID
	if last < 0 {
		t.Fatal("the writer failed")
	}
	end := majorityPoint(t, admin)

	cfg := &config.Config{Targets: map[string]config.Target{"prod-pg": mgCaptureTarget(mg.TargetURI)}}
	eng := mgEngine(t.TempDir())
	res, err := CatchUp(context.Background(), Options{Config: cfg, Repo: openAs(t, dir, nil, "agent"), Host: "cron", Timings: fastTimings,
		Engine: func(Target) (engine.Engine, error) { return eng, nil }}, "prod-pg", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("catch-up waited %s for the lease, stored %d batches, %d bytes, in %s",
		res.LeaseWait.Round(time.Millisecond), res.Segments, res.Bytes, res.Duration.Round(time.Millisecond))
	if res.Gap != nil || res.LeaseWait < fastTimings.Length {
		t.Errorf("result = %+v", res)
	}
	to, err := parseOplogTS(string(res.To))
	if err != nil || to.Less(end) {
		t.Errorf("catch-up reached %s, short of %d:%d (%v)", res.To, end.T, end.I, err)
	}
	r := openAs(t, dir, nil, "admin")
	tid, _ := r.TargetID("prod-pg")
	ids := storedInserts(t, r, tid)
	first := last + 1
	for id := range ids {
		first = min(first, id)
	}
	for id := first; id <= last; id++ {
		if !ids[id] {
			t.Fatalf("insert %d is missing; %d to %d were stored around it", id, first, last)
		}
	}
	t.Logf("inserts %s to %s stored", strconv.FormatInt(first, 10), strconv.FormatInt(last, 10))
}
