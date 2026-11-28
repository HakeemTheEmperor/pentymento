// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/local"
	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

var fastKDF = crypto.KDFParams{Name: "argon2id", MemoryKiB: 8 << 10, Time: 1, Threads: 1}

// fastTimings keep the lease's order (renew, then step down, then
// takeover, well apart) at a fraction of the real durations. A holder
// tolerates StepDown-Renew (2.1 s) without a successful write: CI disks,
// Windows' especially, stall writes for over a second under load, and a
// holder that cannot write in time steps down, as it must.
var fastTimings = lock.LeaseTimings{
	Length: 3 * time.Second, Renew: 300 * time.Millisecond, Check: 50 * time.Millisecond,
	StepDown: 2400 * time.Millisecond, ClaimWait: 300 * time.Millisecond,
}

// newRepoDir creates an encrypted repository with a full slot ("admin")
// and an agent slot ("agent") in a new directory.
func newRepoDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	be, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	full, err := crypto.NewPassphraseSlot("admin", m.Unlocked(), secrets.New([]byte("admin")), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := crypto.NewPassphraseSlot("agent", m.Unlocked().Agent(), secrets.New([]byte("agent")), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(context.Background(), be, repo.NewConfig(false, true, time.Now()), m, []*crypto.Slot{full, ag}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// openAs opens the repository in dir through be (or a new local backend)
// and unlocks it with a passphrase.
func openAs(t testing.TB, dir string, be backend.Backend, pass string) *repo.Repository {
	t.Helper()
	if be == nil {
		var err error
		if be, err = local.New(dir); err != nil {
			t.Fatal(err)
		}
	}
	r, err := repo.Open(context.Background(), be)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.UnlockWith(context.Background(), secrets.New([]byte(pass)), repo.UnlockOptions{}); err != nil {
		t.Fatal(err)
	}
	return r
}

func testConfig(targets ...string) *config.Config {
	c := &config.Config{Targets: map[string]config.Target{}}
	for _, n := range targets {
		c.Targets[n] = config.Target{Engine: "postgres", Host: "db-" + n}
	}
	return c
}

// holders tracks, per target, which agents hold the lease, and fails the
// test if two ever do at once.
type holders struct {
	t   *testing.T
	mu  sync.Mutex
	now map[string]map[string]bool // target → agent → held
	log []string
}

func newHolders(t *testing.T) *holders { return &holders{t: t, now: map[string]map[string]bool{}} }

func (h *holders) hook(agent string) func(string, bool, uint64) {
	return func(target string, held bool, _ uint64) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.now[target] == nil {
			h.now[target] = map[string]bool{}
		}
		h.now[target][agent] = held
		n := 0
		for _, v := range h.now[target] {
			if v {
				n++
			}
		}
		h.log = append(h.log, agent+" "+target+" "+map[bool]string{true: "held", false: "lost"}[held])
		if n > 1 {
			h.t.Errorf("two agents hold %s at once: %v", target, h.log)
		}
	}
}

func (h *holders) holder(target string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for a, held := range h.now[target] {
		if held {
			return a
		}
	}
	return ""
}

func waitFor(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type running struct {
	a      *Agent
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, o Options) *running {
	t.Helper()
	if o.Timings == (lock.LeaseTimings{}) {
		o.Timings = fastTimings
	}
	if o.Heartbeat == 0 {
		o.Heartbeat = 100 * time.Millisecond
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{a: a, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- a.Run(ctx) }()
	t.Cleanup(func() { r.stop(t) })
	waitFor(t, "the agent to start", 5*time.Second, a.Ready)
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	if r.cancel == nil {
		return
	}
	r.cancel()
	r.cancel = nil
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("agent stopped with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the agent did not stop")
	}
}

func TestWriteOnlyGuard(t *testing.T) {
	dir := newRepoDir(t)
	full := openAs(t, dir, nil, "admin")
	_, err := New(Options{Config: testConfig(), Repo: full})
	var me *msg.Error
	if !errors.As(err, &me) || me.Message.ID != IDSlotCanRead || me.Code != msg.ExitConfig {
		t.Errorf("a full slot with write_only on = %v", err)
	}
	off := false
	c := testConfig()
	c.Agent.WriteOnly = &off
	if _, err := New(Options{Config: c, Repo: full}); err != nil {
		t.Errorf("a full slot with write_only off = %v", err)
	}
	if _, err := New(Options{Config: testConfig(), Repo: openAs(t, dir, nil, "agent")}); err != nil {
		t.Errorf("an agent slot = %v", err)
	}
}

// TestOneHolder: two agents for the same targets never hold a lease at
// once; a graceful stop releases the leases, and the other agent takes
// over at once rather than after the lease length.
func TestOneHolder(t *testing.T) {
	dir := newRepoDir(t)
	h := newHolders(t)
	a := start(t, Options{Config: testConfig("prod-pg", "prod-mg"), Repo: openAs(t, dir, nil, "agent"), Host: "a", OnLease: h.hook("a")})
	waitFor(t, "a to hold both leases", 5*time.Second, func() bool { return h.holder("prod-pg") == "a" && h.holder("prod-mg") == "a" })
	b := start(t, Options{Config: testConfig("prod-pg", "prod-mg"), Repo: openAs(t, dir, nil, "agent"), Host: "b", OnLease: h.hook("b")})
	time.Sleep(3 * fastTimings.Length) // b watches; a keeps renewing
	if h.holder("prod-pg") != "a" || h.holder("prod-mg") != "a" {
		t.Fatalf("the leases moved while a was renewing: %v", h.log)
	}

	a.stop(t)
	// a has stopped, so its releases are written: b takes over on its next
	// check, not after the lease length.
	stopped := time.Now()
	waitFor(t, "b to take over", 5*time.Second, func() bool { return h.holder("prod-pg") == "b" && h.holder("prod-mg") == "b" })
	if took := time.Since(stopped); took >= fastTimings.Length {
		t.Errorf("b took over %s after a stopped; a released lease is taken over at once", took)
	}
	// a's last heartbeat says it stopped; b's says it runs.
	hbs, err := openAs(t, dir, nil, "agent").LatestHeartbeats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, hb := range hbs {
		states[hb.Host] = hb.State
	}
	if states["a"] != repo.StateStopped || states["b"] != repo.StateRunning {
		t.Errorf("heartbeats = %v", states)
	}
	st := b.a.Status()
	if len(st.Targets) != 2 || !st.Targets[0].Holding || st.Targets[0].Name != "prod-mg" || st.Targets[0].Generation != 2 || st.LastHeartbeat.IsZero() {
		t.Errorf("status = %+v", st)
	}
}

// failingLocks fails writes under locks/ while fail is set, as an outage
// of the storage would.
type failingLocks struct {
	backend.Backend
	fail atomic.Bool
}

func (f *failingLocks) Put(ctx context.Context, key string, r io.Reader, opts backend.PutOptions) error {
	if f.fail.Load() && strings.HasPrefix(key, "locks/") {
		return errors.New("storage unreachable")
	}
	return f.Backend.Put(ctx, key, r, opts)
}

// TestStepDown: a holder that cannot renew stops capturing within the step
// down time, before anyone may take over.
func TestStepDown(t *testing.T) {
	dir := newRepoDir(t)
	lb, _ := local.New(dir)
	fb := &failingLocks{Backend: lb}
	var capturing atomic.Int32
	capture := func(ctx context.Context, tg Target) error {
		capturing.Add(1)
		defer capturing.Add(-1)
		if tg.Generation == 0 {
			t.Error("capture started without a lease generation")
		}
		<-ctx.Done()
		return nil
	}
	h := newHolders(t)
	start(t, Options{Config: testConfig("prod-pg"), Repo: openAs(t, dir, fb, "agent"), Host: "a", OnLease: h.hook("a"), Capture: capture})
	waitFor(t, "capture to start", 5*time.Second, func() bool { return capturing.Load() == 1 })
	fb.fail.Store(true)
	lost := time.Now()
	waitFor(t, "capture to stop", 5*time.Second, func() bool { return capturing.Load() == 0 })
	if took := time.Since(lost); took >= fastTimings.Length {
		t.Errorf("capture stopped %s after renewals began to fail; it must stop before the lease length", took)
	}
	if h.holder("prod-pg") != "" {
		t.Error("the agent still reports the lease")
	}
	fb.fail.Store(false)
	// Its own record stood unchanged while it failed: it takes the lease
	// again after the lease length, like any candidate.
	waitFor(t, "the lease again", fastTimings.Length+5*time.Second, func() bool { return h.holder("prod-pg") == "a" && capturing.Load() == 1 })
}

// TestCaptureRestarts: a capture that fails is started again while the
// lease is held.
func TestCaptureRestarts(t *testing.T) {
	dir := newRepoDir(t)
	var runs atomic.Int32
	capture := func(ctx context.Context, _ Target) error {
		if runs.Add(1) == 1 {
			return errors.New("connection lost")
		}
		<-ctx.Done()
		return nil
	}
	start(t, Options{Config: testConfig("prod-pg"), Repo: openAs(t, dir, nil, "agent"), Capture: capture})
	waitFor(t, "a second capture run", captureRetry+5*time.Second, func() bool { return runs.Load() == 2 })
}

func TestReload(t *testing.T) {
	dir := newRepoDir(t)
	h := newHolders(t)
	r := start(t, Options{Config: testConfig("prod-pg", "prod-mg"), Repo: openAs(t, dir, nil, "agent"), Host: "a", OnLease: h.hook("a")})
	waitFor(t, "both leases", 5*time.Second, func() bool { return h.holder("prod-pg") == "a" && h.holder("prod-mg") == "a" })

	next := testConfig("prod-pg", "prod-mg", "prod-pg2")
	delete(next.Targets, "prod-mg")
	changed := next.Targets["prod-pg"]
	next.Targets["prod-pg"] = changed // unchanged
	res, err := r.a.Reload(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Added, ",") != "prod-pg2" || strings.Join(res.Removed, ",") != "prod-mg" ||
		strings.Join(res.Unchanged, ",") != "prod-pg" || len(res.Restarted) != 0 {
		t.Errorf("reload = %+v", res)
	}
	waitFor(t, "the new target's lease", 5*time.Second, func() bool { return h.holder("prod-pg2") == "a" })
	if h.holder("prod-mg") != "" {
		t.Error("a removed target kept its lease")
	}
	// A changed target restarts; a new repository is refused.
	next2 := testConfig("prod-pg", "prod-pg2")
	pg := next2.Targets["prod-pg"]
	pg.Port = 5433
	next2.Targets["prod-pg"] = pg
	if res, err := r.a.Reload(context.Background(), next2); err != nil || strings.Join(res.Restarted, ",") != "prod-pg" {
		t.Errorf("changed target: %+v, %v", res, err)
	}
	other := testConfig("prod-pg")
	other.Repository.Path = "/elsewhere"
	if _, err := r.a.Reload(context.Background(), other); err == nil {
		t.Error("a reload switched repositories")
	}
}

func TestPool(t *testing.T) {
	p := NewPool(2)
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	release := make(chan struct{})
	for range 5 {
		wg.Go(func() {
			_ = p.Do(context.Background(), func() error {
				n := running.Add(1)
				for q := peak.Load(); n > q; q = peak.Load() {
					if peak.CompareAndSwap(q, n) {
						break
					}
				}
				<-release
				running.Add(-1)
				return nil
			})
		})
	}
	waitFor(t, "the queue", 2*time.Second, func() bool { return p.Waiting() == 3 })
	close(release)
	wg.Wait()
	if peak.Load() != 2 || p.Size() != 2 {
		t.Errorf("peak %d of %d slots", peak.Load(), p.Size())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	block := NewPool(1)
	go func() {
		_ = block.Do(context.Background(), func() error { time.Sleep(200 * time.Millisecond); return nil })
	}()
	time.Sleep(20 * time.Millisecond)
	if err := block.Do(ctx, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait = %v", err)
	}
}
