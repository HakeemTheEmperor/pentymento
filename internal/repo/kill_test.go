// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/backendtest"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// killFixture is a repository with two full slots and one committed
// snapshot, s0, on local storage.
type killFixture struct {
	*fixture
	s0 *Snapshot
}

const (
	killS0Seed, killS1Seed = 21, 22
	killS0Size             = 2 << 20
)

// killS1Size is the crashed write's size: smaller under the race detector,
// where the sweep is several times slower. Every call is crashed either way.
var killS1Size int64 = 20 << 20

func init() {
	if raceEnabled {
		killS1Size = 6 << 20
	}
}

func newKillFixture(t *testing.T) *killFixture {
	t.Helper()
	f := newFixture(t)
	other, err := crypto.NewPassphraseSlot("other", f.material.Unlocked(), secrets.New([]byte("other")), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.AddSlot(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	s0, _ := writeSnap(t, f.repo, loadIndex(t, f.repo), newStream(killS0Seed, killS0Size, nil))
	return &killFixture{fixture: f, s0: s0}
}

// openOn opens the repository on be with the admin slot, as a new process.
func (k *killFixture) openOn(t *testing.T, be backend.Backend) (*Repository, *Index, error) {
	t.Helper()
	ctx := context.Background()
	r, err := Open(ctx, be)
	if err != nil {
		return nil, nil, err
	}
	u, err := k.full.UnlockPassphrase(secrets.New([]byte("admin")))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Unlock(ctx, *u); err != nil {
		return nil, nil, err
	}
	idx, err := r.LoadIndex(ctx)
	return r, idx, err
}

// writeS1 writes the second snapshot through be.
func (k *killFixture) writeS1(t *testing.T, be backend.Backend) (*Snapshot, error) {
	t.Helper()
	r, idx, err := k.openOn(t, be)
	if err != nil {
		return nil, err
	}
	s, _, err := r.WriteSnapshot(context.Background(), idx, newStream(killS1Seed, killS1Size, nil), Snapshot{Target: "t1", Engine: "postgres"}, WriteOptions{Workers: 2})
	return s, err
}

// afterCrash restarts on the plain backend and checks the repository: s0
// intact, s1 absent or intact, no errors from check, a retried write
// succeeds, and an index rebuild leaves no problem at all.
func (k *killFixture) afterCrash(t *testing.T) (s1Committed bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := k.be.CleanTemp(ctx, 0); err != nil {
		t.Fatal(err)
	}
	r, idx, err := k.openOn(t, k.be)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint64]int64{killS0Seed: killS0Size}
	for _, s := range snaps {
		seed, size := uint64(killS0Seed), int64(killS0Size)
		if s.ID != k.s0.ID {
			seed, size, s1Committed = killS1Seed, killS1Size, true
			want[killS1Seed] = killS1Size
		}
		hw := newHashWriter()
		if err := r.ReadSnapshot(ctx, idx, &s, hw); err != nil {
			t.Fatalf("snapshot %s after the crash: %v", s.ID, err)
		}
		if hw.sum() != hashOf(t, newStream(seed, size, nil)) {
			t.Fatalf("snapshot %s reads back different data after the crash", s.ID)
		}
	}
	if len(snaps) != len(want) {
		t.Fatalf("%d snapshots after the crash", len(snaps))
	}
	if rep := check(t, r, CheckOptions{Full: true}); rep.Errors() > 0 {
		t.Fatalf("check after the crash:\n%s", problemsText(rep))
	}
	if !s1Committed {
		s, err := k.writeS1(t, k.be)
		if err != nil {
			t.Fatalf("retrying the write: %v", err)
		}
		r, idx, _ = k.openOn(t, k.be)
		if err := r.ReadSnapshot(ctx, idx, s, io.Discard); err != nil {
			t.Fatalf("the retried snapshot: %v", err)
		}
	}
	if _, err := r.RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	r, _, _ = k.openOn(t, k.be)
	if rep := check(t, r, CheckOptions{Full: true}); len(rep.Problems) != 0 {
		t.Fatalf("check after rebuilding the index:\n%s", problemsText(rep))
	}
	return s1Committed
}

// TestKillAtEveryCall crashes a snapshot write at each storage call in
// turn, from opening the repository to committing the snapshot.
func TestKillAtEveryCall(t *testing.T) {
	probe := newKillFixture(t)
	counter := backendtest.Wrap(probe.be, backendtest.Faults{})
	if _, err := probe.writeS1(t, counter); err != nil {
		t.Fatal(err)
	}
	calls := counter.Calls()
	if calls < 6 {
		t.Fatalf("a clean write made only %d calls", calls)
	}
	committed := 0
	for n := 1; n <= calls; n++ {
		k := newKillFixture(t)
		faulty := backendtest.Wrap(k.be, backendtest.Faults{Seed: uint64(n), CrashAtCall: n}) // #nosec G115 -- small
		if _, err := k.writeS1(t, faulty); err == nil {
			t.Fatalf("call %d: the write succeeded despite the crash", n)
		}
		if k.afterCrash(t) {
			committed++
		}
	}
	// Only a crash at the very last call (the snapshot commit, acknowledged
	// too late) can leave s1 committed.
	if committed > 1 {
		t.Errorf("s1 committed after %d of %d crashes", committed, calls)
	}
}

// cutPut cuts the first Put to a key with prefix after half its bytes, as a
// process killed mid-upload, and fails every call after it.
type cutPut struct {
	backend.Backend
	prefix  string
	mu      sync.Mutex
	crashed bool
}

var errKilled = errors.New("killed")

func (c *cutPut) dead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.crashed
}

func (c *cutPut) Put(ctx context.Context, key string, r io.Reader, o backend.PutOptions) error {
	if c.dead() {
		return errKilled
	}
	if !strings.HasPrefix(key, c.prefix) {
		return c.Backend.Put(ctx, key, r, o)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.crashed = true
	c.mu.Unlock()
	_ = c.Backend.Put(ctx, key, io.MultiReader(strings.NewReader(string(b[:len(b)/2])), failingReader{}), o)
	return errKilled
}

func (c *cutPut) Get(ctx context.Context, key string, rng *backend.Range) (io.ReadCloser, error) {
	if c.dead() {
		return nil, errKilled
	}
	return c.Backend.Get(ctx, key, rng)
}

func (c *cutPut) List(ctx context.Context, p string, fn func(backend.ObjectInfo) error) error {
	if c.dead() {
		return errKilled
	}
	return c.Backend.List(ctx, p, fn)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errKilled }

// TestKillDuringIndexCommit kills a backup half-way through uploading its
// index file: the packs are stored but unindexed, no snapshot is
// committed, nothing partial is visible, and the next run recovers.
func TestKillDuringIndexCommit(t *testing.T) {
	k := newKillFixture(t)
	before := len(listKeysOf(t, k.be, indexPrefix))
	if _, err := k.writeS1(t, &cutPut{Backend: k.be, prefix: indexPrefix}); !errors.Is(err, errKilled) {
		t.Fatalf("write = %v, want the kill", err)
	}
	if n := len(listKeysOf(t, k.be, indexPrefix)); n != before {
		t.Fatalf("%d index files after the kill, %d before: a partial one is visible", n, before)
	}
	r, _, err := k.openOn(t, k.be)
	if err != nil {
		t.Fatal(err)
	}
	rep := check(t, r, CheckOptions{Full: true})
	unindexed := 0
	for _, p := range rep.Problems {
		if p.Kind == ProblemUnindexed {
			unindexed++
		}
	}
	if rep.Errors() > 0 || unindexed == 0 {
		t.Fatalf("after the kill: %d errors, %d unindexed packs (want 0 errors and the new packs unindexed)\n%s", rep.Errors(), unindexed, problemsText(rep))
	}
	if k.afterCrash(t) {
		t.Error("the snapshot was committed although its index file was not")
	}
	if temps := tempFiles(t, k.dir); len(temps) != 0 {
		t.Errorf("temporary files left after CleanTemp: %v", temps)
	}
}

func listKeysOf(t *testing.T, be backend.Backend, prefix string) []string {
	t.Helper()
	var out []string
	if err := be.List(context.Background(), prefix, func(oi backend.ObjectInfo) error {
		out = append(out, oi.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".tmp-") {
			out = append(out, p)
		}
		return nil
	})
	return out
}
