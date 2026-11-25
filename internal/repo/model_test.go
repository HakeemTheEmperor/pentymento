// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/backendtest"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/local"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// recipe describes a snapshot's content without holding it: a stream from
// newStream, optionally with bytes inserted, so later snapshots can share
// most chunks with earlier ones.
type recipe struct {
	seed  uint64
	size  int64
	insAt int64
	ins   []byte
}

func (rc recipe) reader() io.Reader {
	var inserts map[int64][]byte
	if len(rc.ins) > 0 {
		inserts = map[int64][]byte{rc.insAt: rc.ins}
	}
	return newStream(rc.seed, rc.size, inserts)
}

type pinState struct {
	pinned bool
	reason string
}

// modelRun is one random sequence of operations against a real repository
// on local storage, and the model of what it must hold.
type modelRun struct {
	t  *testing.T
	be backend.Backend
	// cleanTemp removes temporary files a crash left, where the backend has
	// any.
	cleanTemp func() error
	seen      *outcomes
	keys      crypto.Unlocked
	clock     atomic.Int64
	r         *Repository
	idx       *Index

	// The model.
	snaps map[string]recipe
	pins  map[string]pinState
}

func newModelRun(t *testing.T, rt *rapid.T, be backend.Backend, cleanTemp func() error, seen *outcomes) *modelRun {
	mat, err := crypto.Generate()
	if err != nil {
		rt.Fatal(err)
	}
	full, err := crypto.NewPassphraseSlot("admin", mat.Unlocked(), secrets.New([]byte("admin")), fastKDF)
	if err != nil {
		rt.Fatal(err)
	}
	other, err := crypto.NewPassphraseSlot("other", mat.Unlocked(), secrets.New([]byte("other")), fastKDF)
	if err != nil {
		rt.Fatal(err)
	}
	if _, err := Create(context.Background(), be, NewConfig(false, time.Now()), mat, []*crypto.Slot{full, other}); err != nil {
		rt.Fatal(err)
	}
	m := &modelRun{t: t, be: be, cleanTemp: cleanTemp, seen: seen, keys: mat.Unlocked(), snaps: map[string]recipe{}, pins: map[string]pinState{}}
	m.clock.Store(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	m.restart(rt)
	return m
}

// now is a clock that always moves forward, so the newest pin record is
// always the last one written.
func (m *modelRun) now() time.Time { return time.Unix(0, m.clock.Add(int64(time.Millisecond))).UTC() }

// open opens and unlocks the repository on be, as a new process would.
func (m *modelRun) open(be backend.Backend) (*Repository, *Index, error) {
	ctx := context.Background()
	r, err := Open(ctx, be)
	if err != nil {
		return nil, nil, err
	}
	if err := r.Unlock(ctx, m.keys); err != nil {
		return nil, nil, err
	}
	r.Now = m.now
	idx, err := r.LoadIndex(ctx)
	if err != nil {
		return nil, nil, err
	}
	return r, idx, nil
}

// restart drops the process state and opens the repository afresh,
// sometimes cleaning up temporary files a crash left.
func (m *modelRun) restart(rt *rapid.T) {
	if m.cleanTemp != nil && rapid.Bool().Draw(rt, "clean temp files") {
		if err := m.cleanTemp(); err != nil {
			rt.Fatal(err)
		}
	}
	r, idx, err := m.open(m.be)
	if err != nil {
		rt.Fatalf("restart: %v", err)
	}
	m.r, m.idx = r, idx
}

func (m *modelRun) drawRecipe(rt *rapid.T) recipe {
	if len(m.snaps) > 0 && rapid.Bool().Draw(rt, "derive from a snapshot") {
		base := m.snaps[rapid.SampledFrom(m.sortedSnaps()).Draw(rt, "base")]
		base.insAt = rapid.Int64Range(0, base.size).Draw(rt, "insert at")
		base.ins = rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(rt, "insert")
		return base
	}
	return recipe{
		seed: rapid.Uint64Range(0, 3).Draw(rt, "seed"), // few seeds, so content repeats
		size: rapid.Int64Range(0, 384<<10).Draw(rt, "size"),
	}
}

func (m *modelRun) sortedSnaps() []string {
	ids := slices.Collect(maps.Keys(m.snaps))
	sort.Strings(ids)
	return ids
}

func (m *modelRun) write(rt *rapid.T) {
	rc := m.drawRecipe(rt)
	s, _, err := m.r.WriteSnapshot(context.Background(), m.idx, rc.reader(), Snapshot{Target: "t", Engine: "postgres"}, WriteOptions{Workers: 2})
	if err != nil {
		rt.Fatalf("write: %v", err)
	}
	m.snaps[s.ID] = rc
}

// writeCrash writes a snapshot from a process that crashes at a random
// call, then restarts. The snapshot either exists in full or not at all.
func (m *modelRun) writeCrash(rt *rapid.T) {
	rc := m.drawRecipe(rt)
	faulty := backendtest.Wrap(m.be, backendtest.Faults{
		Seed:        rapid.Uint64().Draw(rt, "fault seed"),
		CrashAtCall: rapid.IntRange(1, 16).Draw(rt, "crash at call"),
	})
	done := false
	if r, idx, err := m.open(faulty); err == nil {
		s, _, err := r.WriteSnapshot(context.Background(), idx, rc.reader(), Snapshot{Target: "t", Engine: "postgres"}, WriteOptions{Workers: 2})
		if err == nil {
			m.snaps[s.ID] = rc // the crash came after the commit, or not at all
			done = true
		}
	}
	m.restart(rt)
	switch {
	case done:
		m.seen.add("write: done before the crash")
		if m.adoptAmbiguous(rt, rc) {
			rt.Fatal("a snapshot the model knows nothing about appeared")
		}
	case m.adoptAmbiguous(rt, rc):
		m.seen.add("write: committed, crash before the reply")
	default:
		m.seen.add("write: absent after the crash")
	}
}

// adoptAmbiguous handles a crash after the commit's write took effect
// but before it was acknowledged: a new snapshot may exist that the model
// does not know. There can be at most one, and it must hold rc's content.
func (m *modelRun) adoptAmbiguous(rt *rapid.T, rc recipe) bool {
	got, err := m.r.Snapshots(context.Background())
	if err != nil {
		rt.Fatal(err)
	}
	var extra []Snapshot
	for _, s := range got {
		if _, ok := m.snaps[s.ID]; !ok {
			extra = append(extra, s)
		}
	}
	switch len(extra) {
	case 0:
		return false
	case 1:
		m.verifyContent(rt, &extra[0], rc)
		m.snaps[extra[0].ID] = rc
		return true
	}
	rt.Fatalf("%d unexpected snapshots after one crashed write", len(extra))
	return false
}

func (m *modelRun) verifyContent(rt *rapid.T, s *Snapshot, rc recipe) {
	hw := newHashWriter()
	if err := m.r.ReadSnapshot(context.Background(), m.idx, s, hw); err != nil {
		rt.Fatalf("snapshot %s: %v", s.ID, err)
	}
	if hw.sum() != hashOf(m.t, rc.reader()) {
		rt.Fatalf("snapshot %s reads back different content", s.ID)
	}
}

func (m *modelRun) drawPin(rt *rapid.T) (string, pinState) {
	id := rapid.SampledFrom(m.sortedSnaps()).Draw(rt, "snapshot")
	st := pinState{pinned: rapid.Bool().Draw(rt, "pinned")}
	if st.pinned {
		st.reason = rapid.SampledFrom([]string{"", "pre-migration", "audit"}).Draw(rt, "reason")
	}
	return id, st
}

func (m *modelRun) pin(rt *rapid.T) {
	if len(m.snaps) == 0 {
		rt.Skip("no snapshot to pin")
	}
	id, st := m.drawPin(rt)
	if err := m.r.SetPin(context.Background(), Identity{User: "ada", Host: "h"}, id, st.pinned, st.reason); err != nil {
		rt.Fatalf("pin: %v", err)
	}
	m.pins[id] = st
}

// pinCrash pins from a process that crashes at a random call: afterwards
// the snapshot's pin state is either the old one or the new one.
func (m *modelRun) pinCrash(rt *rapid.T) {
	if len(m.snaps) == 0 {
		rt.Skip("no snapshot to pin")
	}
	id, st := m.drawPin(rt)
	faulty := backendtest.Wrap(m.be, backendtest.Faults{
		Seed:        rapid.Uint64().Draw(rt, "fault seed"),
		CrashAtCall: rapid.IntRange(1, 8).Draw(rt, "crash at call"),
	})
	if r, _, err := m.open(faulty); err == nil {
		if err := r.SetPin(context.Background(), Identity{User: "ada", Host: "h"}, id, st.pinned, st.reason); err == nil {
			m.pins[id] = st
		}
	}
	m.restart(rt)
	got, err := m.r.Pins(context.Background())
	if err != nil {
		rt.Fatal(err)
	}
	if p, ok := got[id]; ok && (pinState{p.Pinned, p.Reason}) == st {
		m.pins[id] = st // the record took effect before the crash
		m.seen.add("pin: new state after the crash")
	} else {
		m.seen.add("pin: old state after the crash")
	}
}

func (m *modelRun) pinMissing(rt *rapid.T) {
	id := fmt.Sprintf("%032x", rapid.Uint64().Draw(rt, "missing id"))
	if _, ok := m.snaps[id]; ok {
		rt.Skip("drew an existing ID")
	}
	err := m.r.SetPin(context.Background(), Identity{User: "ada", Host: "h"}, id, true, "x")
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitNotFound {
		rt.Fatalf("pinning a missing snapshot = %v", err)
	}
}

// invariants compare the repository with the model after every step.
func (m *modelRun) invariants(rt *rapid.T) {
	ctx := context.Background()
	got, err := m.r.Snapshots(ctx)
	if err != nil {
		rt.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if want := m.sortedSnaps(); !slices.Equal(ids, want) {
		rt.Fatalf("snapshots = %v, model %v", ids, want)
	}
	pins, err := m.r.Pins(ctx)
	if err != nil {
		rt.Fatal(err)
	}
	if len(pins) != len(m.pins) {
		rt.Fatalf("%d pin states, model %d", len(pins), len(m.pins))
	}
	for id, st := range m.pins {
		if p := pins[id]; p.Pinned != st.pinned || p.Reason != st.reason {
			rt.Fatalf("pin of %s = %+v, model %+v", id, p, st)
		}
	}
}

// final reads every snapshot back, checks the whole repository, and
// rebuilds the index from the packs and reads everything again.
func (m *modelRun) final(rt *rapid.T) {
	ctx := context.Background()
	check := func(when string) {
		for _, id := range m.sortedSnaps() {
			s, err := m.r.LoadSnapshot(ctx, id)
			if err != nil {
				rt.Fatal(err)
			}
			m.verifyContent(rt, s, m.snaps[id])
		}
		rep, err := m.r.Check(ctx, CheckOptions{Full: true})
		if err != nil {
			rt.Fatal(err)
		}
		if rep.Errors() > 0 {
			rt.Fatalf("check %s:\n%s", when, problemsText(rep))
		}
	}
	check("at the end")
	if _, err := m.r.RebuildIndex(ctx); err != nil {
		rt.Fatal(err)
	}
	m.restart(rt)
	check("after an index rebuild")
}

// outcomes counts what the crashes did, so a run that stops exercising an
// outcome fails instead of passing quietly.
type outcomes struct {
	mu sync.Mutex
	n  map[string]int
}

func (o *outcomes) add(k string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n[k]++
}

// require fails t unless every outcome occurred.
func (o *outcomes) require(t *testing.T, names ...string) {
	t.Helper()
	t.Logf("crash outcomes: %v", o.n)
	for _, k := range names {
		if o.n[k] == 0 {
			t.Errorf("outcome %q never occurred", k)
		}
	}
}

var allOutcomes = []string{
	"write: done before the crash", "write: committed, crash before the reply", "write: absent after the crash",
	"pin: new state after the crash", "pin: old state after the crash",
}

// runModel runs random sequences of snapshot writes, pins, crashes and
// restarts against an in-memory model of what the repository must hold
// (Build Order C11). Prune joins the model in C28.
func runModel(t *testing.T, newBackend func() (backend.Backend, func() error, func())) *outcomes {
	seen := &outcomes{n: map[string]int{}}
	rapid.Check(t, func(rt *rapid.T) {
		be, cleanTemp, done := newBackend()
		defer done()
		m := newModelRun(t, rt, be, cleanTemp, seen)
		rt.Repeat(map[string]func(*rapid.T){
			"write":       m.write,
			"write crash": m.writeCrash,
			"pin":         m.pin,
			"pin crash":   m.pinCrash,
			"pin missing": m.pinMissing,
			"restart":     m.restart,
			"":            m.invariants,
		})
		m.final(rt)
	})
	return seen
}

// TestRepositoryModel runs the model on in-memory storage.
func TestRepositoryModel(t *testing.T) {
	if raceEnabled {
		defer limitChecks(t, 10)() // several times slower; the nightly job runs many more
	}
	seen := runModel(t, func() (backend.Backend, func() error, func()) {
		return backendtest.NewMemory(), nil, func() {}
	})
	if !testing.Short() && !raceEnabled && !t.Failed() {
		seen.require(t, allOutcomes...)
	}
}

// TestRepositoryModelLocal runs fewer sequences on the local backend, so
// crashes leave real temporary files and restarts clean them up.
func TestRepositoryModelLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: real files and fsync")
	}
	checks := 15
	if raceEnabled {
		checks = 5
	}
	defer limitChecks(t, checks)()
	base := t.TempDir()
	runModel(t, func() (backend.Backend, func() error, func()) {
		dir, err := os.MkdirTemp(base, "run")
		if err != nil {
			t.Fatal(err)
		}
		be, err := local.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		clean := func() error { _, err := be.CleanTemp(context.Background(), 0); return err }
		return be, clean, func() { _ = os.RemoveAll(dir) }
	})
}

// limitChecks lowers rapid's number of checks to at most n for one test,
// unless -rapid.checks was given on the command line, and returns a
// function that restores it.
func limitChecks(t *testing.T, n int) func() {
	t.Helper()
	f := flag.Lookup("rapid.checks")
	if f == nil {
		t.Fatal("no -rapid.checks flag")
	}
	set := false
	flag.Visit(func(v *flag.Flag) { set = set || v.Name == "rapid.checks" })
	if set {
		return func() {}
	}
	old := f.Value.String()
	if cur, err := strconv.Atoi(old); err == nil && cur <= n {
		return func() {}
	}
	if err := f.Value.Set(strconv.Itoa(n)); err != nil {
		t.Fatal(err)
	}
	return func() { _ = f.Value.Set(old) }
}
