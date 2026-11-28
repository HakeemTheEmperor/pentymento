// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// statusRepo writes what an agent leaves behind for prod-pg: a snapshot
// with its WAL (segment 2 complete, segment 3 partial), a checkpoint, and
// the lease of agent a1 on db1 with a heartbeat age old.
type statusRepo struct {
	c     *cliEnv
	r     *repo.Repository
	tid   string
	lease *lock.Lease
}

func newStatusRepo(t *testing.T, c *cliEnv, age time.Duration) *statusRepo {
	t.Helper()
	if r := c.runFake("backup", "prod-pg"); r.code != msg.ExitOK { // WAL from 0/2000028 to 0/2000100
		t.Fatalf("backup: %d %s", r.code, r.stderr)
	}
	ctx := context.Background()
	r, _, err := c.unlock(c.env["K1"], repo.UnlockOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tid, err := r.TargetID("prod-pg")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PutWALSegment(ctx, tid, 1, "000000010000000000000002", []byte("wal 2")); err != nil {
		t.Fatal(err)
	}
	if err := r.PutWALPartial(ctx, tid, 1, "000000010000000000000003", 0x3000100, []byte("wal 3")); err != nil {
		t.Fatal(err)
	}
	lease := lock.NewLease(r.Backend(), tid, lock.Holder{User: "pentym", Host: "db1", AgentID: "a1"}, nil, lock.DefaultLeaseTimings)
	if err := lease.Tick(ctx); err != nil || !lease.MayCapture() {
		t.Fatalf("lease: %v", err)
	}
	if err := r.PutCheckpoint(ctx, repo.Checkpoint{Target: tid, Generation: lease.Generation(), Seq: 1, Position: "1/0/3000100", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutHeartbeat(ctx, repo.Heartbeat{Host: "db1", AgentID: "a1", State: repo.StateRunning, Time: time.Now().Add(-age)}); err != nil {
		t.Fatal(err)
	}
	return &statusRepo{c: c, r: r, tid: tid, lease: lease}
}

func statusJSON(t *testing.T, r result) statusReport {
	t.Helper()
	var rep statusReport
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &rep) != nil {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	return rep
}

func target(t *testing.T, rep statusReport, name string) targetStatusReport {
	t.Helper()
	for _, tr := range rep.Targets {
		if tr.Name == name {
			return tr
		}
	}
	t.Fatalf("no %s in %+v", name, rep.Targets)
	return targetStatusReport{}
}

// TestStatusNothingYet: a new repository shows no snapshot and no agent,
// with the commands to run.
func TestStatusNothingYet(t *testing.T) {
	c := backupEnv(t, "env:KA") // any slot reads status
	r := c.runFake("status")
	if r.code != msg.ExitOK || !regexp.MustCompile(`prod-pg\s+postgres\s+! none\s+✗ no snapshot\s+—`).MatchString(r.stdout) ||
		!strings.Contains(r.stderr, "✗ prod-pg: no snapshot yet: nothing can be restored\n→ pentym backup prod-pg") ||
		!strings.Contains(r.stderr, "! prod-pg: no agent captures this target, so point-in-time recovery is off\n→ pentym agent") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	rep := statusJSON(t, c.runFake("--json", "status"))
	pg := target(t, rep, "prod-pg")
	if rep.FullKeySlots != 2 || pg.Agent.State != "none" || pg.Window.PITR || pg.Window.ConfiguredSeconds != 7*24*3600 ||
		pg.Snapshots != 0 || pg.Gaps == nil || pg.LastVerify != nil || pg.DeadMan != nil {
		t.Errorf("--json = %+v", rep)
	}
	if !strings.Contains(c.runFake("--json", "status").stdout, `"last_verify": null`) {
		t.Error("last_verify is not in the JSON")
	}
}

// TestStatusStaleAgent (Build Order C20): an agent silent for 20 minutes
// is critical, with the catch-up command; one silent for 6 minutes is a
// warning; a fresh one is capturing.
func TestStatusStaleAgent(t *testing.T) {
	c := backupEnv(t, "env:K1")
	s := newStatusRepo(t, c, 20*time.Minute)
	r := c.runFake("status")
	if r.code != msg.ExitOK || !regexp.MustCompile(`prod-pg\s+postgres 17\s+✗ stale \(20m 0s\)\s+✓ \S+ · up to \S+ ago`).MatchString(r.stdout) ||
		!regexp.MustCompile(`✗ prod-pg: agent last seen 20m 0s ago on db1; changes since .+ are not protected\n→ pentym catch-up prod-pg`).MatchString(r.stderr) {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	pg := target(t, statusJSON(t, c.runFake("--json", "status")), "prod-pg")
	if pg.Agent.State != "stale" || pg.Agent.Level != "failed" || pg.Agent.Host != "db1" || !pg.Window.PITR || pg.Window.Shortened ||
		pg.Checkpoint == nil || pg.Checkpoint.Position != "1/0/3000100" || pg.LatestSnapshot == nil {
		t.Errorf("prod-pg = %+v", pg)
	}

	if err := s.r.PutHeartbeat(context.Background(), repo.Heartbeat{Host: "db1", AgentID: "a1", State: repo.StateRunning, Time: time.Now().Add(-6 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if r := c.runFake("status"); !strings.Contains(r.stdout, "! stale (6m 0s)") || !strings.Contains(r.stderr, "! prod-pg: agent last seen 6m 0s ago") {
		t.Errorf("6 minutes:\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	if err := s.r.PutHeartbeat(context.Background(), repo.Heartbeat{Host: "db1", AgentID: "a1", State: repo.StateRunning, Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if r := c.runFake("status"); !strings.Contains(r.stdout, "✓ capturing (") || strings.Contains(r.stderr, "prod-pg: agent") {
		t.Errorf("fresh:\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
}

// TestStatusGap (Build Order C20): a gap after the snapshot leaves no
// snapshot inside the captured log; a snapshot after it restarts the
// window, shortened by the gap.
func TestStatusGap(t *testing.T) {
	c := backupEnv(t, "env:K1")
	s := newStatusRepo(t, c, 0)
	ctx := context.Background()
	gap := repo.Gap{Target: s.tid, From: "1/0/2000100", To: "1/0/3000000", Reason: "the replication slot pentym_prod_pg was invalidated", Time: time.Now()}
	if err := s.r.PutGap(ctx, gap); err != nil {
		t.Fatal(err)
	}
	r := c.runFake("status")
	if !strings.Contains(r.stdout, "✗ no snapshot in the captured log") ||
		!strings.Contains(r.stderr, "✗ prod-pg: no snapshot lies within the captured log, so point-in-time recovery has no starting point") {
		t.Fatalf("stdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	// A base backup after the gap, as the agent takes one.
	idx, err := s.r.LoadIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.r.WriteSnapshot(ctx, idx, bytes.NewReader([]byte("after the gap")), repo.Snapshot{
		Target: s.tid, Engine: "postgres", EngineVersion: "17.2", StartPosition: "0/3000028", EndPosition: "0/3000080", Timeline: "1",
	}, repo.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	r = c.runFake("-v", "status")
	if !regexp.MustCompile(`! prod-pg: configured 7d 0h, actual \S+ \(gap at .+\): the replication slot pentym_prod_pg was invalidated \(PTM-5001\)`).MatchString(r.stderr) ||
		!strings.Contains(r.stdout, "! ") {
		t.Fatalf("stdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	pg := target(t, statusJSON(t, c.runFake("--json", "status")), "prod-pg")
	if !pg.Window.PITR || !pg.Window.Shortened || len(pg.Gaps) != 1 || pg.Gaps[0].From != gap.From || pg.Window.Break != gap.Reason || pg.Snapshots != 2 {
		t.Errorf("prod-pg = %+v", pg)
	}
}

// TestStatusAutoCatchUp: with catch_up.auto, status runs catch-up for a
// target whose agent stopped; without it, it prints the command.
func TestStatusAutoCatchUp(t *testing.T) {
	c := backupEnv(t, "env:K1")
	s := newStatusRepo(t, c, time.Minute)
	if err := s.lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := cliLogFake{head: "1/0/4000000"}
	r := c.runCatchUp(f, "status")
	if !regexp.MustCompile(`! prod-pg: the agent on db1 stopped .+ ago; changes since .+ are not protected\n→ pentym catch-up prod-pg, or start the agent`).MatchString(r.stderr) ||
		strings.Contains(r.stderr, "caught up") {
		t.Fatalf("without auto:\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	cfg, _ := os.ReadFile(c.config)
	if err := os.WriteFile(c.config, append(cfg, []byte("catch_up:\n  auto: true\n")...), 0o600); err != nil { // #nosec G703 -- the test's own config
		t.Fatal(err)
	}
	r = c.runCatchUp(f, "status")
	if r.code != msg.ExitOK || !strings.Contains(r.stderr, "! prod-pg: no agent captures this target; running catch-up (catch_up.auto)") ||
		!strings.Contains(r.stderr, "✓ prod-pg: caught up to 1/0/4000000") {
		t.Fatalf("auto:\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	// Caught up and released again: the next run catches up with nothing
	// new, and --json carries the report.
	rep := statusJSON(t, c.runCatchUp(f, "--json", "status"))
	pg := target(t, rep, "prod-pg")
	if pg.CatchUp == nil || pg.CatchUp.To != "1/0/4000000" || pg.Checkpoint == nil || pg.Checkpoint.Position != "1/0/4000000" {
		t.Errorf("--json = %+v", pg)
	}
}

// TestStatusLive: --live adds the running agent's view; without an agent
// it says so and shows the repository's.
func TestStatusLive(t *testing.T) {
	c := agentEnv(t, "env:KA")
	r := c.runFake("status", "--live")
	if r.code != msg.ExitOK || !strings.Contains(r.stderr, "! agent not reachable at ") || !strings.Contains(r.stdout, "prod-pg") {
		t.Fatalf("no agent: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if rep := statusJSON(t, c.runFake("--json", "status", "--live")); rep.AgentReached == nil || *rep.AgentReached || rep.AgentError == "" {
		t.Errorf("no agent, --json = %+v", rep)
	}
	runningAgent(t, c)
	deadline := time.Now().Add(15 * time.Second)
	for {
		r = c.runFake("status", "--live")
		if strings.Contains(r.stdout, "held (generation 1)") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !regexp.MustCompile(`agent [0-9a-f]+ on \S+ · upload queue 0`).MatchString(r.stdout) || strings.Contains(r.stderr, "not reachable") {
		t.Errorf("stdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	rep := statusJSON(t, c.runFake("--json", "status", "--live"))
	if rep.AgentReached == nil || !*rep.AgentReached || rep.UploadQueue == nil {
		t.Errorf("--json = %+v", rep)
	}
}

// TestStatusOneSlot (PRD 8.2): a repository with one full key slot is
// warned about; two are not.
func TestStatusOneSlot(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	res := c.mustInit()
	cfg, _ := os.ReadFile(c.config)
	if err := os.WriteFile(c.config, append(cfg, testTargets...), 0o600); err != nil { // #nosec G703 -- the test's own config
		t.Fatal(err)
	}
	if r := c.runFake("status"); strings.Contains(r.stderr, "full key slot") {
		t.Fatalf("two full slots warned: %s", r.stderr)
	}
	if r := c.pentym("key", "remove", res.Slots[1].ID); r.code != msg.ExitOK {
		t.Fatalf("remove: exit %d, %s", r.code, r.stderr)
	}
	r := c.runFake("-v", "status")
	if r.code != msg.ExitOK || !strings.Contains(r.stderr, "! the repository has only 1 full key slot (PTM-4006)\n  losing its passphrase or identity would make every backup unreadable\n→ pentym key add, or pentym key export --paper") {
		t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	if rep := statusJSON(t, c.runFake("--json", "status")); rep.FullKeySlots != 1 {
		t.Errorf("full_key_slots = %d", rep.FullKeySlots)
	}
}
