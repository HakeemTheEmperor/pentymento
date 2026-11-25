// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package lock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	"pgregory.net/rapid"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/local"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func newBackend(t testing.TB) (*local.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	be, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return be, dir
}

var ctx = context.Background()

// --- Repository locks ---

func holder(name string) Holder { return Holder{User: name, Host: "h-" + name, PID: 1} }

func wantBlocked(t *testing.T, err error) {
	t.Helper()
	var me *msg.Error
	if !errors.Is(err, ErrBlocked) || !errors.As(err, &me) || me.Message.ID != IDLocked {
		t.Fatalf("err = %v, want ErrBlocked with %s", err, IDLocked)
	}
}

func TestSharedLocksCoexist(t *testing.T) {
	be, _ := newBackend(t)
	a, err := NewLocks(be, holder("a")).AcquireShared(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewLocks(be, holder("b")).AcquireShared(ctx)
	if err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	live, _ := NewLocks(be, holder("x")).Live(ctx)
	if len(live) != 2 {
		t.Errorf("live = %d", len(live))
	}
	_ = a.Release(ctx)
	_ = b.Release(ctx)
}

func TestExclusiveExcludes(t *testing.T) {
	be, _ := newBackend(t)
	backup, err := NewLocks(be, holder("backup")).AcquireShared(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewLocks(be, holder("prune")).AcquireExclusive(ctx)
	wantBlocked(t, err)
	if err := backup.Release(ctx); err != nil {
		t.Fatal(err)
	}
	gc, err := NewLocks(be, holder("prune")).AcquireExclusive(ctx)
	if err != nil {
		t.Fatalf("exclusive after the backup finished: %v", err)
	}
	_, err = NewLocks(be, holder("backup2")).AcquireShared(ctx)
	wantBlocked(t, err)
	_, err = NewLocks(be, holder("prune2")).AcquireExclusive(ctx)
	wantBlocked(t, err)
	if err := gc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	// A blocked attempt leaves no live lock behind.
	if live, _ := NewLocks(be, holder("x")).Live(ctx); len(live) != 0 {
		t.Errorf("live after releases: %+v", live)
	}
}

func TestExclusiveTieBreak(t *testing.T) {
	be, _ := newBackend(t)
	// Two exclusive records written before either checks: the lower ID wins.
	low := Record{Version: 1, Kind: KindExclusive, ID: "0000000000000001", State: StateHeld}
	if err := putRecord(ctx, be, lockKey(KindExclusive, low.ID), low, true); err != nil {
		t.Fatal(err)
	}
	m := NewLocks(be, holder("high"))
	l := &Lock{m: m, key: lockKey(KindExclusive, "ffffffffffffffff"), rec: Record{Kind: KindExclusive, ID: "ffffffffffffffff", State: StateHeld}}
	if c, _ := m.conflicts(ctx, l); len(c) != 1 {
		t.Errorf("higher ID sees %d conflicts, want 1", len(c))
	}
	l.rec.ID, l.key = "0000000000000000", lockKey(KindExclusive, "0000000000000000")
	if c, _ := m.conflicts(ctx, l); len(c) != 0 {
		t.Errorf("lower ID sees %d conflicts, want 0", len(c))
	}
}

// TestLockExclusionConcurrent: many processes race for shared and exclusive
// locks; an exclusive winner never coexists with any other holder.
func TestLockExclusionConcurrent(t *testing.T) {
	for round := range 15 {
		be, _ := newBackend(t)
		var mu sync.Mutex
		var held []*Lock
		var wg sync.WaitGroup
		for i := range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m := NewLocks(be, holder(fmt.Sprint(i)))
				var l *Lock
				var err error
				if (i+round)%3 == 0 {
					l, err = m.AcquireExclusive(ctx)
				} else {
					l, err = m.AcquireShared(ctx)
				}
				if err == nil {
					mu.Lock()
					held = append(held, l)
					mu.Unlock()
				} else if !errors.Is(err, ErrBlocked) {
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		exclusive := 0
		for _, l := range held {
			if l.Kind() == KindExclusive {
				exclusive++
			}
		}
		if exclusive > 0 && len(held) > 1 {
			t.Fatalf("round %d: an exclusive lock held alongside %d others", round, len(held)-1)
		}
	}
}

func TestStaleLocks(t *testing.T) {
	be, dir := newBackend(t)
	crashed, err := NewLocks(be, holder("crashed")).AcquireShared(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The crashed backup stopped refreshing 31 minutes ago (storage time).
	old := time.Now().Add(-31 * time.Minute)
	if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(crashed.key)), old, old); err != nil {
		t.Fatal(err)
	}
	gc, err := NewLocks(be, holder("prune")).AcquireExclusive(ctx)
	if err != nil {
		t.Fatalf("a stale lock blocked prune: %v", err)
	}
	_ = gc.Release(ctx)

	// A refreshed lock is live again.
	if err := crashed.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = NewLocks(be, holder("prune")).AcquireExclusive(ctx)
	wantBlocked(t, err)

	// RemoveStale deletes stale and released records, not live ones.
	_ = os.Chtimes(filepath.Join(dir, filepath.FromSlash(crashed.key)), old, old)
	live, _ := NewLocks(be, holder("live")).AcquireShared(ctx)
	released := Record{Version: 1, Kind: KindShared, ID: "aaaaaaaaaaaaaaaa", State: StateReleased}
	_ = putRecord(ctx, be, lockKey(KindShared, released.ID), released, true)
	n, err := NewLocks(be, holder("prune")).RemoveStale(ctx)
	if err != nil || n != 2 {
		t.Fatalf("RemoveStale = %d, %v; want 2", n, err)
	}
	if recs, _ := NewLocks(be, holder("x")).records(ctx); len(recs) != 1 || recs[0].key != live.key {
		t.Errorf("records left: %+v", recs)
	}
	_ = live.Release(ctx)
}

func TestKeepFresh(t *testing.T) {
	be, _ := newBackend(t)
	l, err := NewLocks(be, holder("a")).AcquireShared(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stop := l.KeepFresh(ctx, 5*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, _ := getRecord(ctx, be, l.key)
		if rec.Seq >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if rec, _ := getRecord(ctx, be, l.key); rec.Seq < 3 {
		t.Errorf("KeepFresh did not refresh: seq %d", rec.Seq)
	}
	_ = l.Release(ctx)
}

// releaseOnly is a backend whose credentials cannot delete, like an agent's.
type releaseOnly struct{ backend.Backend }

func (releaseOnly) Delete(context.Context, string) error { return errors.New("AccessDenied") }

func TestReleaseWithoutDelete(t *testing.T) {
	be, _ := newBackend(t)
	ro := releaseOnly{be}
	l, err := NewLocks(ro, holder("agent")).AcquireShared(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("Release without delete permission: %v", err)
	}
	rec, err := getRecord(ctx, be, l.key)
	if err != nil || rec.State != StateReleased {
		t.Fatalf("record = %+v, %v; want released", rec, err)
	}
	if _, err := NewLocks(be, holder("prune")).AcquireExclusive(ctx); err != nil {
		t.Errorf("a released lock blocked prune: %v", err)
	}
}

// --- Capture leases, simulated ---

// fakeClock is a monotonic clock that moves only when told; offset models
// a host whose wall clock is wrong, which must not matter.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// switchable fails every call while failing is set, modelling an agent
// that has lost its connection to storage.
type switchable struct {
	backend.Backend
	mu      sync.Mutex
	failing bool
}

func (s *switchable) fail(on bool) { s.mu.Lock(); s.failing = on; s.mu.Unlock() }

func (s *switchable) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return errors.New("connection reset")
	}
	return nil
}

func (s *switchable) Put(ctx context.Context, k string, r io.Reader, o backend.PutOptions) error {
	if err := s.err(); err != nil {
		return err
	}
	return s.Backend.Put(ctx, k, r, o)
}

func (s *switchable) Get(ctx context.Context, k string, rng *backend.Range) (io.ReadCloser, error) {
	if err := s.err(); err != nil {
		return nil, err
	}
	return s.Backend.Get(ctx, k, rng)
}

func (s *switchable) List(ctx context.Context, p string, fn func(backend.ObjectInfo) error) error {
	if err := s.err(); err != nil {
		return err
	}
	return s.Backend.List(ctx, p, fn)
}

// agent is one simulated agent process.
type agent struct {
	name     string
	clock    *fakeClock
	be       *switchable
	lease    *Lease
	alive    bool
	paused   bool
	lastTick time.Time
}

// sim runs agents against one repository in simulated time.
type sim struct {
	t       *testing.T
	agents  []*agent
	elapsed time.Duration
	// holders records who could capture, second by second.
	holders []string
}

func newSim(t *testing.T, inner backend.Backend, skews ...time.Duration) *sim {
	s := &sim{t: t}
	base := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for i, skew := range skews {
		sw := &switchable{Backend: inner}
		c := &fakeClock{t: base.Add(skew)}
		a := &agent{name: fmt.Sprintf("agent-%d", i), clock: c, be: sw, alive: true}
		a.lease = NewLease(sw, "0123456789abcdef", Holder{User: "pentym", Host: a.name}, c, DefaultLeaseTimings)
		a.lastTick = c.Now().Add(-time.Hour)
		s.agents = append(s.agents, a)
	}
	return s
}

// run advances d, one second at a time; each live, unpaused agent ticks
// every Check (as the agent loop does), and the invariant is checked every
// second: at most one agent may capture.
func (s *sim) run(d time.Duration) {
	s.t.Helper()
	for range int(d / time.Second) {
		s.elapsed += time.Second
		for _, a := range s.agents {
			a.clock.advance(time.Second)
			if a.alive && !a.paused && a.clock.Now().Sub(a.lastTick) >= LeaseCheck {
				a.lastTick = a.clock.Now()
				_ = a.lease.Tick(ctx) // storage errors are part of the scenario
			}
		}
		holder := ""
		for _, a := range s.agents {
			if a.alive && a.lease.MayCapture() {
				if holder != "" {
					s.t.Fatalf("at %s, %s and %s may both capture", s.elapsed, holder, a.name)
				}
				holder = a.name
			}
		}
		s.holders = append(s.holders, holder)
	}
}

func (s *sim) holder() string { return s.holders[len(s.holders)-1] }

// changedAt returns the elapsed time at which the current holder was first
// recorded. holders[i] is recorded after second i+1.
func (s *sim) changedAt() time.Duration {
	last := s.holder()
	for i := len(s.holders) - 1; i >= 0; i-- {
		if s.holders[i] != last {
			return time.Duration(i+2) * time.Second
		}
	}
	return time.Second
}

func TestLeaseTwoCandidates(t *testing.T) {
	be, _ := newBackend(t)
	s := newSim(t, be, 0, 0)
	s.run(20 * time.Second)
	if s.holder() == "" {
		t.Fatal("nobody acquired the lease")
	}
	// The holder keeps it while renewing.
	first := s.holder()
	s.run(30 * time.Minute)
	if s.holder() != first {
		t.Errorf("lease moved from %s to %s while %s was renewing", first, s.holder(), first)
	}
	for _, a := range s.agents {
		if a.name == first && (a.lease.Generation() != 1 || a.lease.BestEffort()) {
			t.Errorf("holder generation %d, best effort %v", a.lease.Generation(), a.lease.BestEffort())
		}
	}
}

func byName(s *sim, name string) *agent {
	for _, a := range s.agents {
		if a.name == name {
			return a
		}
	}
	return nil
}

func TestLeaseTakeoverAfterKill(t *testing.T) {
	be, _ := newBackend(t)
	s := newSim(t, be, 0, 0)
	s.run(5 * time.Minute)
	dead := byName(s, s.holder())
	lastRenew := dead.lease.lastRenewOK
	killedAt := s.elapsed
	dead.alive = false // kill -9
	s.run(5 * time.Minute)
	if s.holder() == "" || s.holder() == dead.name {
		t.Fatalf("no takeover after the holder died (holder %q)", s.holder())
	}
	takeover := s.changedAt()
	// Takeover needs the last record unchanged for 2 minutes; observation
	// happens on the 15-second check, so it falls within one renew and one
	// check interval after that.
	sinceRenew := takeover - killedAt + dead.clock.Now().Sub(lastRenew) - (s.elapsed - killedAt)
	if sinceRenew < LeaseLength || sinceRenew > LeaseLength+LeaseRenew+LeaseCheck+time.Second {
		t.Errorf("takeover %s after the last renewal, want 2m to 2m45s", sinceRenew)
	}
	if g := byName(s, s.holder()).lease.Generation(); g != 2 {
		t.Errorf("new holder's generation = %d, want 2", g)
	}
}

func TestLeaseGracefulRelease(t *testing.T) {
	be, _ := newBackend(t)
	s := newSim(t, be, 0, 0)
	s.run(2 * time.Minute)
	h := byName(s, s.holder())
	if err := h.lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	h.alive = false
	releasedAt := s.elapsed
	s.run(time.Minute)
	if s.holder() == "" || s.holder() == h.name {
		t.Fatal("no takeover after a graceful release")
	}
	if d := s.changedAt() - releasedAt; d > LeaseCheck+time.Second {
		t.Errorf("takeover %s after release, want within one check (15s)", d)
	}
}

// TestLeaseStepDown: a holder that loses storage stops capturing at 90 s,
// before the standby can take over at 120 s, and rejoins as a candidate.
func TestLeaseStepDown(t *testing.T) {
	be, _ := newBackend(t)
	s := newSim(t, be, 0, 0)
	s.run(3 * time.Minute)
	h := byName(s, s.holder())
	h.be.fail(true)
	s.run(10 * time.Minute) // the invariant is checked every second
	if s.holder() == h.name || s.holder() == "" {
		t.Fatalf("holder %q after the original holder lost storage", s.holder())
	}
	h.be.fail(false)
	s.run(10 * time.Minute)
	if h.lease.MayCapture() {
		t.Error("the old holder grabbed the lease back from a healthy holder")
	}
}

// TestLeasePause: a holder frozen for 3 minutes (a VM pause, a long GC)
// stops capturing at 90 s by its own clock, and on waking finds the newer
// generation and stays down.
func TestLeasePause(t *testing.T) {
	be, _ := newBackend(t)
	s := newSim(t, be, 0, 0)
	s.run(2 * time.Minute)
	h := byName(s, s.holder())
	h.paused = true
	s.run(3 * time.Minute)
	h.paused = false
	s.run(2 * time.Minute)
	if s.holder() == h.name {
		t.Error("the paused holder took the lease back")
	}
	if h.lease.Generation() != 0 {
		t.Errorf("paused holder still claims generation %d", h.lease.Generation())
	}
}

// TestLeaseClockSkew: wall clocks hours apart change nothing; only each
// agent's own elapsed time matters.
func TestLeaseClockSkew(t *testing.T) {
	be, _ := newBackend(t)
	s := newSim(t, be, -3*time.Hour, 0, 7*time.Hour+13*time.Minute)
	s.run(5 * time.Minute)
	first := byName(s, s.holder())
	first.alive = false
	s.run(5 * time.Minute)
	if s.holder() == "" || s.holder() == first.name {
		t.Fatal("no takeover with skewed clocks")
	}
}

// TestLeaseProperty runs random kills, restarts, pauses and storage
// outages for three agents with skewed clocks; at no second may two agents
// capture, and while some agent stays healthy the lease is not left empty
// for long.
func TestLeaseProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		be := newMem()
		skews := []time.Duration{
			time.Duration(rapid.Int64Range(-int64(time.Hour), int64(time.Hour)).Draw(rt, "skew0")),
			time.Duration(rapid.Int64Range(-int64(time.Hour), int64(time.Hour)).Draw(rt, "skew1")),
			time.Duration(rapid.Int64Range(-int64(time.Hour), int64(time.Hour)).Draw(rt, "skew2")),
		}
		s := newSim(t, be, skews...)
		steps := rapid.IntRange(5, 25).Draw(rt, "steps")
		for range steps {
			a := s.agents[rapid.IntRange(0, 2).Draw(rt, "agent")]
			switch rapid.IntRange(0, 5).Draw(rt, "event") {
			case 0:
				a.alive = false
			case 1:
				if !a.alive {
					// A restarted process is a new participant.
					a.lease = NewLease(a.be, "0123456789abcdef", Holder{Host: a.name}, a.clock, DefaultLeaseTimings)
					a.alive = true
				}
			case 2:
				a.paused = !a.paused
			case 3:
				a.be.fail(rapid.Bool().Draw(rt, "fail"))
			case 4:
				if a.alive && a.lease.MayCapture() {
					_ = a.lease.Release(ctx)
				}
			}
			s.run(time.Duration(rapid.IntRange(1, 240).Draw(rt, "seconds")) * time.Second)
		}
		// Heal everyone: the lease must settle on one holder.
		for _, a := range s.agents {
			a.be.fail(false)
			a.paused = false
			if !a.alive {
				a.lease = NewLease(a.be, "0123456789abcdef", Holder{Host: a.name}, a.clock, DefaultLeaseTimings)
				a.alive = true
			}
		}
		s.run(5 * time.Minute)
		if s.holder() == "" {
			rt.Fatal("no holder five minutes after every agent recovered")
		}
	})
}

// noConditionalCreate hides conditional create, to exercise the fallback.
type noConditionalCreate struct{ backend.Backend }

func (n noConditionalCreate) Capabilities() backend.Capabilities {
	c := n.Backend.Capabilities()
	c.ConditionalCreate = false
	return c
}

func TestLeaseFallback(t *testing.T) {
	be, _ := newBackend(t)
	s := newSim(t, noConditionalCreate{be}, 0, 0, 0)
	s.run(time.Minute)
	if s.holder() == "" {
		t.Fatal("fallback: nobody acquired the lease")
	}
	h := byName(s, s.holder())
	if !h.lease.BestEffort() {
		t.Error("fallback lease not marked best effort")
	}
	h.alive = false
	s.run(5 * time.Minute)
	if s.holder() == "" || s.holder() == h.name {
		t.Fatal("fallback: no takeover")
	}
}

func TestCurrentGeneration(t *testing.T) {
	be, _ := newBackend(t)
	if g, err := CurrentGeneration(ctx, be, "0123456789abcdef"); err != nil || g != 0 {
		t.Fatalf("no lease: %d, %v", g, err)
	}
	s := newSim(t, be, 0, 0)
	s.run(time.Minute)
	byName(s, s.holder()).alive = false
	s.run(5 * time.Minute)
	if g, err := CurrentGeneration(ctx, be, "0123456789abcdef"); err != nil || g != 2 {
		t.Errorf("after one takeover: %d, %v", g, err)
	}
}

// memBackend is a small in-memory backend with the semantics leases rely
// on (conditional create, overwrite, listing in key order), so the
// property test runs thousands of lease steps quickly.
type memBackend struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newMem() *memBackend { return &memBackend{objs: map[string][]byte{}} }

func (m *memBackend) Put(_ context.Context, k string, r io.Reader, o backend.PutOptions) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[k]; ok && o.IfNotExists {
		return backend.ErrExists
	}
	m.objs[k] = b
	return nil
}

func (m *memBackend) Get(_ context.Context, k string, _ *backend.Range) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[k]
	if !ok {
		return nil, backend.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memBackend) Stat(_ context.Context, k string) (backend.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[k]
	if !ok {
		return backend.ObjectInfo{}, backend.ErrNotFound
	}
	return backend.ObjectInfo{Key: k, Size: int64(len(b)), ModTime: time.Now()}, nil
}

func (m *memBackend) List(_ context.Context, p string, fn func(backend.ObjectInfo) error) error {
	m.mu.Lock()
	var keys []string
	for k := range m.objs {
		if strings.HasPrefix(k, p) {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()
	sort.Strings(keys)
	for _, k := range keys {
		if err := fn(backend.ObjectInfo{Key: k, ModTime: time.Now()}); err != nil {
			return err
		}
	}
	return nil
}

func (m *memBackend) Delete(_ context.Context, k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, k)
	return nil
}

func (m *memBackend) Capabilities() backend.Capabilities {
	return backend.Capabilities{ConditionalCreate: true, StrongConsistency: true}
}

// TestGenerationsNeverReset: releasing never removes the newest lease
// record, so generations keep increasing even with delete permission.
func TestGenerationsNeverReset(t *testing.T) {
	be, _ := newBackend(t)
	for want := uint64(1); want <= 4; want++ {
		l := NewLease(be, "0123456789abcdef", Holder{Host: "h"}, nil, DefaultLeaseTimings)
		if err := l.Tick(ctx); err != nil || l.Generation() != want {
			t.Fatalf("holder %d got generation %d, %v", want, l.Generation(), err)
		}
		if err := l.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
