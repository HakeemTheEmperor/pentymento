// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
)

// walEngine is a fake engine whose capture stores numbered segments, one
// per segEvery, from the position it is given.
type walEngine struct {
	engine.Engine
	mu       sync.Mutex
	froms    []engine.Position
	gapFrom  engine.Position // DetectGap reports a gap from here
	failures int             // Capture fails this many times first
	panics   bool            // the first Capture panics
	health   engine.LogHealth
	segEvery time.Duration
	calls    atomic.Int32
}

func (e *walEngine) Name() string { return "postgres" }

func (e *walEngine) DetectGap(_ context.Context, _ engine.Target, cp engine.Position) (*engine.Gap, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cp != "" && cp == e.gapFrom {
		return &engine.Gap{From: cp, Reason: "the replication slot was invalidated"}, nil
	}
	return nil, nil
}

func (e *walEngine) Capture(ctx context.Context, _ engine.Target, from engine.Position, sink engine.LogSink) error {
	n := e.calls.Add(1)
	e.mu.Lock()
	e.froms = append(e.froms, from)
	fail, panics := e.failures > 0, e.panics && n == 1
	if fail {
		e.failures--
	}
	e.mu.Unlock()
	if panics {
		panic("a bug in capture")
	}
	if fail {
		return errors.New("replication refused")
	}
	seg := 3
	if from != "" {
		_, lsn, err := parseWALPosition(string(from))
		if err != nil {
			return err
		}
		seg = int(lsn >> 24) // #nosec G115 -- a small test position
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(e.segEvery):
		}
		seg++
		name := fmt.Sprintf("0000000100000000%08X", seg-1)
		if err := sink.Segment(ctx, engine.Segment{
			Kind: "wal", Name: name, Timeline: "1",
			First: engine.Position(fmt.Sprintf("0/%X000000", seg-1)), Last: engine.Position(fmt.Sprintf("0/%X000000", seg)),
			Checkpoint: engine.Position(fmt.Sprintf("1/0/%X000000", seg)), Data: []byte("wal " + name),
		}); err != nil {
			return err
		}
	}
}

func (e *walEngine) LogHealth(context.Context, engine.Target) (*engine.LogHealth, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.health
	return &h, nil
}

func (e *walEngine) fromsSeen() []engine.Position {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]engine.Position(nil), e.froms...)
}

// backups counts the base backups an agent takes.
type backups struct{ n atomic.Int32 }

func (b *backups) hook(context.Context, Target) (any, error) { b.n.Add(1); return nil, nil }

func captureOptions(t *testing.T, dir string, e *walEngine, b *backups) Options {
	t.Helper()
	return Options{
		Config: testConfig("prod-pg"), Repo: openAs(t, dir, nil, "agent"), Host: "a",
		Engine: func(Target) (engine.Engine, error) { return e, nil }, Backup: b.hook,
		Telemetry: telemetry.New(nil, telemetry.NewMetrics()), HealthInterval: 50 * time.Millisecond,
	}
}

func checkpoint(t *testing.T, dir string) *repo.Checkpoint {
	t.Helper()
	r := openAs(t, dir, nil, "admin")
	tid, _ := r.TargetID("prod-pg")
	cp, err := r.LatestCheckpoint(context.Background(), tid)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// TestCaptureStoresAndResumes: segments are stored, each followed by a
// checkpoint with the lease generation, and a restarted agent resumes
// from the checkpoint.
func TestCaptureStoresAndResumes(t *testing.T) {
	dir := newRepoDir(t)
	e := &walEngine{segEvery: 30 * time.Millisecond}
	b := &backups{}
	r := start(t, captureOptions(t, dir, e, b))
	waitFor(t, "segments", 5*time.Second, func() bool { cp := checkpoint(t, dir); return cp != nil && cp.Seq >= 3 })
	r.stop(t)
	cp := checkpoint(t, dir)
	if cp.Generation != 1 || cp.Position == "" {
		t.Fatalf("checkpoint = %+v", cp)
	}
	full := openAs(t, dir, nil, "admin")
	tid, _ := full.TargetID("prod-pg")
	segs, err := full.WALSegments(context.Background(), tid)
	if err != nil || uint64(len(segs)) < cp.Seq || segs[0].Name != "000000010000000000000003" {
		t.Fatalf("stored segments = %v, %v", segs, err)
	}
	data, err := full.GetSegment(context.Background(), segs[0].Key)
	if err != nil || string(data) != "wal 000000010000000000000003" {
		t.Errorf("segment content = %q, %v", data, err)
	}
	if b.n.Load() != 1 {
		t.Errorf("%d base backups; capture without a checkpoint forces exactly one", b.n.Load())
	}

	// A new agent (a new lease generation) resumes from the checkpoint.
	e2 := &walEngine{segEvery: 30 * time.Millisecond}
	b2 := &backups{}
	start(t, captureOptions(t, dir, e2, b2))
	waitFor(t, "the resumed capture", 5*time.Second, func() bool { return len(e2.fromsSeen()) == 1 })
	if got := e2.fromsSeen()[0]; got != engine.Position(cp.Position) {
		t.Errorf("resumed from %q, the checkpoint is %q", got, cp.Position)
	}
	waitFor(t, "a generation-2 checkpoint", 5*time.Second, func() bool { c := checkpoint(t, dir); return c.Generation == 2 && c.Seq >= 1 })
	if b2.n.Load() != 0 {
		t.Error("a resume with a checkpoint forced a base backup")
	}
}

// TestCaptureGap: a gap is recorded with the lease generation, a base
// backup is forced, and capture starts afresh.
func TestCaptureGap(t *testing.T) {
	dir := newRepoDir(t)
	first := &walEngine{segEvery: 20 * time.Millisecond}
	r := start(t, captureOptions(t, dir, first, &backups{}))
	waitFor(t, "a checkpoint", 5*time.Second, func() bool { return checkpoint(t, dir) != nil })
	r.stop(t)
	cp := checkpoint(t, dir)

	e := &walEngine{segEvery: 20 * time.Millisecond, gapFrom: engine.Position(cp.Position)}
	b := &backups{}
	o := captureOptions(t, dir, e, b)
	start(t, o)
	waitFor(t, "capture after the gap", 5*time.Second, func() bool { return len(e.fromsSeen()) == 1 })
	if got := e.fromsSeen()[0]; got != "" {
		t.Errorf("after the gap capture started from %q, not where the server stands", got)
	}
	waitFor(t, "the forced backup", 5*time.Second, func() bool { return b.n.Load() == 1 })
	full := openAs(t, dir, nil, "admin")
	tid, _ := full.TargetID("prod-pg")
	gaps, err := full.Gaps(context.Background(), tid)
	if err != nil || len(gaps) != 1 || gaps[0].From != cp.Position || gaps[0].Generation != 2 || !strings.Contains(gaps[0].Reason, "invalidated") {
		t.Errorf("gaps = %+v, %v", gaps, err)
	}
	if testutil.ToFloat64(o.Telemetry.Metrics.JobFailures.WithLabelValues("prod-pg", captureJob)) != 1 {
		t.Error("the gap is not counted as a capture failure")
	}
	journal, _ := full.Journal(context.Background(), time.Time{}, "")
	if last := journal[len(journal)-1]; last.Operation != captureJob || last.Result == repo.ResultOK || !strings.Contains(last.Detail, "invalidated") {
		t.Errorf("journal = %+v", last)
	}
}

// TestCaptureFailsBeforeCheckpoint: a capture that keeps failing before its
// first checkpoint forces one base backup, not one per attempt.
func TestCaptureFailsBeforeCheckpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for capture retries")
	}
	dir := newRepoDir(t)
	e := &walEngine{segEvery: 20 * time.Millisecond, failures: 2}
	b := &backups{}
	start(t, captureOptions(t, dir, e, b))
	waitFor(t, "the third attempt", 2*captureRetry+5*time.Second, func() bool { return e.calls.Load() >= 3 })
	if b.n.Load() != 1 {
		t.Errorf("%d base backups for 3 attempts", b.n.Load())
	}
}

// TestCaptureUnsupported: an engine that does not capture holds the lease
// without retrying.
func TestCaptureUnsupported(t *testing.T) {
	dir := newRepoDir(t)
	var calls atomic.Int32
	o := captureOptions(t, dir, nil, &backups{})
	o.Engine = func(Target) (engine.Engine, error) { calls.Add(1); return unsupportedEngine{}, nil }
	h := newHolders(t)
	o.OnLease = h.hook("a")
	start(t, o)
	waitFor(t, "the lease", 5*time.Second, func() bool { return h.holder("prod-pg") == "a" })
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 1 {
		t.Errorf("the engine was built %d times; an unsupported capture is not retried", calls.Load())
	}
}

type unsupportedEngine struct{ engine.Engine }

func (unsupportedEngine) DetectGap(context.Context, engine.Target, engine.Position) (*engine.Gap, error) {
	return nil, fmt.Errorf("oplog capture: %w", errors.ErrUnsupported)
}

// TestCapturePanicRecovered: a panic in capture does not stop the agent;
// capture starts again.
func TestCapturePanicRecovered(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a capture retry")
	}
	dir := newRepoDir(t)
	e := &walEngine{segEvery: 20 * time.Millisecond, panics: true}
	start(t, captureOptions(t, dir, e, &backups{}))
	waitFor(t, "capture after the panic", captureRetry+5*time.Second, func() bool { return checkpoint(t, dir) != nil })
}

func TestAlertThresholds(t *testing.T) {
	for lag, want := range map[time.Duration]int{0: levelOK, 4 * time.Minute: levelOK, 5 * time.Minute: levelWarning, 15 * time.Minute: levelCritical} {
		if got := lagAlert(lag); got != want {
			t.Errorf("lagAlert(%s) = %d, want %d", lag, got, want)
		}
	}
	for _, tt := range []struct {
		h      engine.LogHealth
		want   int
		advice string
	}{
		{engine.LogHealth{RetainedBytes: 1 << 30}, levelOK, ""},
		{engine.LogHealth{RetainedBytes: 10 << 30}, levelWarning, "set max_slot_wal_keep_size"},
		{engine.LogHealth{RetainedBytes: 50 << 30}, levelCritical, "set max_slot_wal_keep_size"},
		{engine.LogHealth{RetainedBytes: 4 << 30, RetainedLimit: 10 << 30}, levelOK, ""},
		{engine.LogHealth{RetainedBytes: 5 << 30, RetainedLimit: 10 << 30}, levelWarning, "before the slot reaches"},
		{engine.LogHealth{RetainedBytes: 8 << 30, RetainedLimit: 10 << 30}, levelCritical, "invalidates the slot"},
		{engine.LogHealth{Lost: true}, levelCritical, "invalidated"},
	} {
		got, advice := retainedAlert(&tt.h)
		if got != tt.want || !strings.Contains(advice, tt.advice) {
			t.Errorf("retainedAlert(%+v) = %d %q", tt.h, got, advice)
		}
	}
}

// TestMonitor: capture publishes lag and retained WAL.
func TestMonitor(t *testing.T) {
	dir := newRepoDir(t)
	e := &walEngine{segEvery: time.Hour, health: engine.LogHealth{Head: "1/0/9000000", RetainedBytes: 12 << 30}}
	o := captureOptions(t, dir, e, &backups{})
	r := start(t, o)
	m := o.Telemetry.Metrics
	waitFor(t, "the metrics", 5*time.Second, func() bool {
		return testutil.ToFloat64(m.PGSlotRetainedWAL.WithLabelValues("prod-pg")) == 12<<30 &&
			testutil.ToFloat64(m.CaptureLag.WithLabelValues("prod-pg")) > 0
	})
	// status --live reads the same reading.
	st := r.a.Status()
	if len(st.Targets) != 1 || st.Targets[0].Capture == nil || st.Targets[0].Capture.RetainedBytes != 12<<30 ||
		st.Targets[0].Capture.Head != "1/0/9000000" || st.Targets[0].Capture.LagSeconds <= 0 {
		t.Errorf("live status = %+v", st.Targets)
	}
}

// oplogEngine is a fake MongoDB engine whose capture stores numbered
// oplog batches of two entries, one per walEngine.segEvery.
type oplogEngine struct{ walEngine }

func (e *oplogEngine) Name() string { return "mongodb" }

func (e *oplogEngine) Capture(ctx context.Context, _ engine.Target, from engine.Position, sink engine.LogSink) error {
	e.mu.Lock()
	e.froms = append(e.froms, from)
	e.mu.Unlock()
	n := 0
	if from != "" {
		_, _ = fmt.Sscanf(string(from), "1700000000:%d", &n)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(e.segEvery):
		}
		first, last := fmt.Sprintf("1700000000:%d", n+1), fmt.Sprintf("1700000000:%d", n+2)
		n += 2
		if err := sink.Segment(ctx, engine.Segment{
			Kind: "oplog", First: engine.Position(first), Last: engine.Position(last),
			Checkpoint: engine.Position(last), Data: []byte("oplog " + first + "-" + last),
		}); err != nil {
			return err
		}
	}
}

func oplogOptions(t *testing.T, dir string, e *oplogEngine) Options {
	t.Helper()
	o := captureOptions(t, dir, &e.walEngine, &backups{})
	o.Engine = func(Target) (engine.Engine, error) { return e, nil }
	return o
}

// TestCaptureOplog: oplog batches are stored under their first and last
// timestamps, each followed by a checkpoint at the last, and a restarted
// agent resumes from it.
func TestCaptureOplog(t *testing.T) {
	dir := newRepoDir(t)
	e := &oplogEngine{walEngine{segEvery: 30 * time.Millisecond}}
	r := start(t, oplogOptions(t, dir, e))
	waitFor(t, "batches", 5*time.Second, func() bool { cp := checkpoint(t, dir); return cp != nil && cp.Seq >= 3 })
	r.stop(t)
	cp := checkpoint(t, dir)
	full := openAs(t, dir, nil, "admin")
	tid, _ := full.TargetID("prod-pg")
	batches, err := full.OplogBatches(context.Background(), tid)
	if err != nil || uint64(len(batches)) < cp.Seq {
		t.Fatalf("stored batches = %v, %v", batches, err)
	}
	if b := batches[0]; b.First != (repo.OplogTS{T: 1700000000, I: 1}) || b.Last != (repo.OplogTS{T: 1700000000, I: 2}) {
		t.Errorf("first batch = %+v", b)
	}
	data, err := full.GetSegment(context.Background(), batches[0].Key)
	if err != nil || string(data) != "oplog 1700000000:1-1700000000:2" {
		t.Errorf("batch content = %q, %v", data, err)
	}
	// The checkpoint is at the end of a stored batch. A stop can cut the
	// last checkpoint short after its batch was stored: at most that one
	// batch lies after it, and is stored again on resume.
	at := -1
	for i, b := range batches {
		if cp.Position == fmt.Sprintf("%d:%d", b.Last.T, b.Last.I) {
			at = i
		}
	}
	if at < 0 || len(batches)-1-at > 1 {
		t.Errorf("checkpoint %s; stored batches end at %+v", cp.Position, batches)
	}

	e2 := &oplogEngine{walEngine{segEvery: 30 * time.Millisecond}}
	start(t, oplogOptions(t, dir, e2))
	waitFor(t, "the resumed capture", 5*time.Second, func() bool { return len(e2.fromsSeen()) == 1 })
	if got := e2.fromsSeen()[0]; got != engine.Position(cp.Position) {
		t.Errorf("resumed from %q, the checkpoint is %q", got, cp.Position)
	}
}

func TestParseOplogTS(t *testing.T) {
	if ts, err := parseOplogTS("1700000000:42"); err != nil || ts != (repo.OplogTS{T: 1700000000, I: 42}) {
		t.Errorf("parseOplogTS = %+v, %v", ts, err)
	}
	for _, s := range []string{"", "1700000000", "1/0/3000060", "4294967296:1", "1:x"} {
		if _, err := parseOplogTS(s); err == nil {
			t.Errorf("parseOplogTS(%q) succeeded", s)
		}
	}
}

func TestWindowAlert(t *testing.T) {
	for _, tt := range []struct {
		window, lag time.Duration
		want        int
		advice      string
	}{
		{48 * time.Hour, 0, levelOK, ""},
		{24 * time.Hour, time.Hour, levelOK, ""},
		{23 * time.Hour, 0, levelWarning, "at least a day"},
		{6 * time.Hour, 0, levelWarning, "at least a day"},
		{6*time.Hour - time.Second, 0, levelCritical, "enlarge the oplog"},
		{48 * time.Hour, 24*time.Hour + time.Second, levelCritical, "half the oplog window"},
		{48 * time.Hour, 24 * time.Hour, levelOK, ""},
	} {
		got, advice := windowAlert(tt.window, tt.lag)
		if got != tt.want || !strings.Contains(advice, tt.advice) {
			t.Errorf("windowAlert(%s, %s) = %d %q", tt.window, tt.lag, got, advice)
		}
	}
}

// TestMonitorOplog: capture of an oplog publishes the window, not the
// slot's retained WAL.
func TestMonitorOplog(t *testing.T) {
	dir := newRepoDir(t)
	e := &oplogEngine{walEngine{segEvery: time.Hour, health: engine.LogHealth{Oplog: true, Head: "1700000000:9", Window: 3 * time.Hour}}}
	o := oplogOptions(t, dir, e)
	r := start(t, o)
	m := o.Telemetry.Metrics
	waitFor(t, "the metrics", 5*time.Second, func() bool {
		return testutil.ToFloat64(m.MongoOplogWindow.WithLabelValues("prod-pg")) == (3 * time.Hour).Seconds()
	})
	if c := r.a.Status().Targets[0].Capture; c == nil || c.OplogWindowSeconds != (3*time.Hour).Seconds() || c.RetainedBytes != 0 {
		t.Errorf("live capture = %+v", c)
	}
	if n := testutil.CollectAndCount(m.PGSlotRetainedWAL); n != 0 {
		t.Errorf("an oplog capture published %d retained-WAL series", n)
	}
}
