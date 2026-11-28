// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
)

func catchUpOptions(t *testing.T, dir string, e engine.Engine) Options {
	t.Helper()
	return Options{
		Config: testConfig("prod-pg"), Repo: openAs(t, dir, nil, "agent"), Host: "cron",
		Engine: func(Target) (engine.Engine, error) { return e, nil }, Timings: fastTimings,
		Telemetry: telemetry.New(nil, telemetry.NewMetrics()),
	}
}

// captured runs an agent until it has checkpointed, then stops it
// gracefully, and returns the checkpoint.
func captured(t *testing.T, dir string) *repo.Checkpoint {
	t.Helper()
	r := start(t, captureOptions(t, dir, &walEngine{segEvery: 20 * time.Millisecond}, &backups{}))
	waitFor(t, "a checkpoint", 5*time.Second, func() bool { return checkpoint(t, dir) != nil })
	r.stop(t)
	return checkpoint(t, dir)
}

// segmentAfter is the WAL position n segments after a walEngine
// checkpoint.
func segmentAfter(t *testing.T, cp string, n uint64) engine.Position {
	t.Helper()
	_, lsn, err := parseWALPosition(cp)
	if err != nil {
		t.Fatal(err)
	}
	l := uint64(lsn) + n*0x1000000
	return engine.Position(fmt.Sprintf("1/%X/%X", l>>32, l&0xFFFFFFFF))
}

// lastCatchUp is the newest catch-up entry in the journal.
func lastCatchUp(t *testing.T, dir string) repo.JournalEntry {
	t.Helper()
	j, err := openAs(t, dir, nil, "admin").Journal(context.Background(), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := len(j) - 1; i >= 0; i-- {
		if j[i].Operation == catchUpJob {
			return j[i]
		}
	}
	t.Fatalf("no catch-up in the journal of %d entries", len(j))
	return repo.JournalEntry{}
}

// TestCatchUpDrains (Build Order C20): after an agent stopped, catch-up
// stores what the server retains up to where it stands, checkpoints with
// its own lease generation, and releases the lease.
func TestCatchUpDrains(t *testing.T) {
	dir := newRepoDir(t)
	cp := captured(t, dir)
	head := segmentAfter(t, cp.Position, 3)
	e := &walEngine{segEvery: 10 * time.Millisecond, health: engine.LogHealth{Head: head}}
	var mu sync.Mutex
	var phases []string
	res, err := CatchUp(context.Background(), catchUpOptions(t, dir, e), "prod-pg", func(p string, _ int64) {
		mu.Lock()
		phases = append(phases, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.From != engine.Position(cp.Position) || res.To != head || res.Head != head || res.Segments < 3 || res.Bytes == 0 || res.Gap != nil {
		t.Errorf("result = %+v", res)
	}
	if res.LeaseWait >= fastTimings.Length {
		t.Errorf("waited %s for a released lease", res.LeaseWait)
	}
	if got := e.fromsSeen(); len(got) != 1 || got[0] != engine.Position(cp.Position) {
		t.Errorf("captured from %v, the checkpoint is %s", got, cp.Position)
	}
	after := checkpoint(t, dir)
	if after.Position != string(head) || after.Generation != res.Generation || res.Generation != cp.Generation+1 {
		t.Errorf("checkpoint after catch-up = %+v (generation %d)", after, res.Generation)
	}
	mu.Lock()
	if len(phases) == 0 || phases[0] != "draining" {
		t.Errorf("progress phases = %v", phases)
	}
	mu.Unlock()
	if j := lastCatchUp(t, dir); j.Result != repo.ResultOK {
		t.Errorf("journal = %+v", j)
	}

	// The lease was released: the next catch-up takes it at once and has
	// nothing to store.
	e2 := &walEngine{segEvery: 10 * time.Millisecond, health: engine.LogHealth{Head: head}}
	res2, err := CatchUp(context.Background(), catchUpOptions(t, dir, e2), "prod-pg", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res2.LeaseWait >= fastTimings.Length || res2.Segments != 0 || e2.calls.Load() != 0 || res2.To != head {
		t.Errorf("second catch-up = %+v, %d capture calls", res2, e2.calls.Load())
	}
}

// TestCatchUpGap: a gap is recorded and reported, capture stores what the
// server has from where it stands, and no base backup is taken.
func TestCatchUpGap(t *testing.T) {
	dir := newRepoDir(t)
	cp := captured(t, dir)
	e := &walEngine{segEvery: 10 * time.Millisecond, gapFrom: engine.Position(cp.Position), health: engine.LogHealth{Head: "1/0/6000000"}}
	res, err := CatchUp(context.Background(), catchUpOptions(t, dir, e), "prod-pg", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Gap == nil || res.Gap.From != engine.Position(cp.Position) || res.To != "1/0/6000000" {
		t.Fatalf("result = %+v", res)
	}
	if got := e.fromsSeen(); len(got) != 1 || got[0] != "" {
		t.Errorf("after the gap capture started from %v", got)
	}
	r := openAs(t, dir, nil, "admin")
	tid, _ := r.TargetID("prod-pg")
	gaps, err := r.Gaps(context.Background(), tid)
	if err != nil || len(gaps) != 1 || gaps[0].From != cp.Position || gaps[0].Generation != res.Generation {
		t.Errorf("gaps = %+v, %v", gaps, err)
	}
	if j := lastCatchUp(t, dir); j.Result != repo.ResultFailed || !strings.Contains(j.Detail, "invalidated") {
		t.Errorf("journal = %+v", j)
	}
}

// TestCatchUpWaitsForExpiredLease: a holder that crashed without releasing
// its lease is waited out for the lease length, then catch-up takes over.
func TestCatchUpWaitsForExpiredLease(t *testing.T) {
	dir := newRepoDir(t)
	cp := captured(t, dir)
	r := openAs(t, dir, nil, "agent")
	tid, _ := r.TargetID("prod-pg")
	crashed := lock.NewLease(r.Backend(), tid, lock.CurrentHolder("u", "crashed", "dead"), nil, fastTimings)
	for !crashed.MayCapture() {
		if err := crashed.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(fastTimings.Check)
	}
	e := &walEngine{segEvery: 10 * time.Millisecond, health: engine.LogHealth{Head: segmentAfter(t, cp.Position, 1)}}
	var waited bool
	res, err := CatchUp(context.Background(), catchUpOptions(t, dir, e), "prod-pg", func(p string, _ int64) {
		if p == "waiting" {
			waited = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.LeaseWait < fastTimings.Length || !waited || res.Generation != crashed.Generation()+1 {
		t.Errorf("waited %s (told: %v), generation %d after the crashed holder's %d", res.LeaseWait, waited, res.Generation, crashed.Generation())
	}
}

// TestCatchUpRefusesLiveAgent: an agent that keeps renewing its lease is
// capturing, so catch-up stores nothing and says so.
func TestCatchUpRefusesLiveAgent(t *testing.T) {
	dir := newRepoDir(t)
	start(t, captureOptions(t, dir, &walEngine{segEvery: 20 * time.Millisecond}, &backups{}))
	waitFor(t, "a checkpoint", 5*time.Second, func() bool { return checkpoint(t, dir) != nil })
	e := &walEngine{segEvery: 10 * time.Millisecond, health: engine.LogHealth{Head: "1/0/9000000"}}
	_, err := CatchUp(context.Background(), catchUpOptions(t, dir, e), "prod-pg", nil)
	var me *msg.Error
	if !errors.As(err, &me) || me.Message.ID != IDLeaseHeld || me.Code != msg.ExitFailure {
		t.Fatalf("CatchUp beside a live agent = %v", err)
	}
	if e.calls.Load() != 0 {
		t.Error("catch-up captured beside a live agent")
	}
}

// noLogEngine has no log to capture, like a standalone MongoDB
// server.
type noLogEngine struct{ walEngine }

func (*noLogEngine) LogHealth(context.Context, engine.Target) (*engine.LogHealth, error) {
	return nil, errors.ErrUnsupported
}

func TestCatchUpUnsupported(t *testing.T) {
	dir := newRepoDir(t)
	_, err := CatchUp(context.Background(), catchUpOptions(t, dir, &noLogEngine{}), "prod-pg", nil)
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitNotImplemented || me.Message.Next != "pentym backup prod-pg" {
		t.Fatalf("CatchUp = %v", err)
	}
	// The lease is released on the way out.
	res, err := CatchUp(context.Background(), catchUpOptions(t, dir, &walEngine{segEvery: 10 * time.Millisecond, health: engine.LogHealth{Head: "1/0/1000000"}}), "prod-pg", nil)
	if err != nil || res.LeaseWait >= fastTimings.Length {
		t.Errorf("catch-up after a failed one = %+v, %v", res, err)
	}
}

func TestReached(t *testing.T) {
	for _, tt := range []struct {
		pos, target engine.Position
		want        bool
	}{
		{"1/0/3000000", "1/0/3000000", true},
		{"1/0/3000001", "1/0/3000000", true},
		{"1/0/2FFFFFF", "1/0/3000000", false},
		{"1/1/0", "1/0/FFFFFFFF", true},
		{"2/0/1000000", "1/0/3000000", true},
		{"1/0/9000000", "2/0/1000000", false},
		{"1700000000:5", "1700000000:5", true},
		{"1700000000:4", "1700000000:5", false},
		{"1700000001:1", "1700000000:5", true},
	} {
		got, err := reached(tt.pos, tt.target)
		if err != nil || got != tt.want {
			t.Errorf("reached(%s, %s) = %v, %v", tt.pos, tt.target, got, err)
		}
	}
	for _, bad := range [][2]engine.Position{{"1/0/3000000", "1700000000:5"}, {"x", "1/0/1"}, {"1/0/1", ""}} {
		if _, err := reached(bad[0], bad[1]); err == nil {
			t.Errorf("reached(%q, %q) succeeded", bad[0], bad[1])
		}
	}
}
