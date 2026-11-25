// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/chunker"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// dataRepo is a repository with two targets, a snapshot of each (each in
// its own pack), a WAL segment for a and an oplog batch for b, and two full
// slots ("admin", "other") plus the agent slot.
type dataRepo struct {
	*fixture
	tidA, tidB    string
	snapA, snapB  *Snapshot
	walKey, opKey string
	other         *crypto.Slot
	segA, segB    []byte
}

const dataSnapSize = 3 << 20

func newDataRepo(t *testing.T) *dataRepo {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	d := &dataRepo{fixture: f}
	var err error
	if d.other, err = crypto.NewPassphraseSlot("other", f.material.Unlocked(), secrets.New([]byte("other")), fastKDF); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.AddSlot(ctx, d.other); err != nil {
		t.Fatal(err)
	}
	if d.tidA, err = f.repo.PutTargetInfo(ctx, TargetInfo{Name: "a", Engine: "postgres"}); err != nil {
		t.Fatal(err)
	}
	if d.tidB, err = f.repo.PutTargetInfo(ctx, TargetInfo{Name: "b", Engine: "mongodb"}); err != nil {
		t.Fatal(err)
	}
	idx := loadIndex(t, f.repo)
	d.snapA = writeTargetSnap(t, f.repo, idx, d.tidA, 11)
	d.snapB = writeTargetSnap(t, f.repo, idx, d.tidB, 12)

	d.segA, d.segB = randomBytes(1, 256<<10), randomBytes(2, 128<<10)
	const walName = "000000010000000000000001"
	if err := f.repo.PutWALSegment(ctx, d.tidA, 1, walName, d.segA); err != nil {
		t.Fatal(err)
	}
	d.walKey = walSegmentKey(d.tidA, 1, walName)
	first, last := OplogTS{T: 1, I: 1}, OplogTS{T: 2, I: 1}
	if err := f.repo.PutOplogBatch(ctx, d.tidB, first, last, d.segB); err != nil {
		t.Fatal(err)
	}
	d.opKey = oplogBatchKey(d.tidB, first, last)
	return d
}

func writeTargetSnap(t *testing.T, r *Repository, idx *Index, tid string, seed uint64) *Snapshot {
	t.Helper()
	s, _, err := r.WriteSnapshot(context.Background(), idx, newStream(seed, dataSnapSize, nil),
		Snapshot{Target: tid, Engine: "postgres"}, WriteOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func randomBytes(seed uint64, n int) []byte {
	rng := rand.New(rand.NewPCG(seed, seed)) // #nosec G404 -- test data
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32()) // #nosec G115 -- truncation intended
	}
	return b
}

// open reopens the repository with the admin slot.
func (d *dataRepo) open(t *testing.T) *Repository {
	t.Helper()
	return d.reopen(t, d.full, "admin")
}

// packOf returns the key of the pack holding a snapshot's first chunk-list
// block; each data-repo snapshot fits in one pack.
func (d *dataRepo) packOf(t *testing.T, s *Snapshot) string {
	t.Helper()
	id, err := chunker.ParseID(s.List[0])
	if err != nil {
		t.Fatal(err)
	}
	loc, _, ok := loadIndex(t, d.open(t)).Lookup(id)
	if !ok {
		t.Fatal("snapshot's list block is not indexed")
	}
	return packKey(loc.Pack)
}

// path returns the file of an object.
func (d *dataRepo) path(key string) string { return filepath.Join(d.dir, filepath.FromSlash(key)) }

// mutate rewrites an object's bytes.
func (d *dataRepo) mutate(t *testing.T, key string, fn func([]byte) []byte) {
	t.Helper()
	b, err := os.ReadFile(d.path(key)) // #nosec G304 -- the test's own repository
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.path(key), fn(b), 0o600); err != nil { // #nosec G703 -- the test's own repository
		t.Fatal(err)
	}
}

// flip flips one byte of an object, at off from the start (or the end
// when negative).
func (d *dataRepo) flip(t *testing.T, key string, off int) {
	t.Helper()
	d.mutate(t, key, func(b []byte) []byte {
		if off < 0 {
			off += len(b)
		}
		b[off] ^= 0xff
		return b
	})
}

func (d *dataRepo) remove(t *testing.T, key string) {
	t.Helper()
	if err := os.Remove(d.path(key)); err != nil {
		t.Fatal(err)
	}
}

// keys lists object keys under prefix.
func (d *dataRepo) keys(t *testing.T, prefix string) []string {
	t.Helper()
	var out []string
	if err := d.be.List(context.Background(), prefix, func(oi backend.ObjectInfo) error {
		out = append(out, oi.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// readsBack checks every snapshot and segment reads back intact.
func (d *dataRepo) readsBack(t *testing.T, r *Repository) {
	t.Helper()
	ctx := context.Background()
	idx := loadIndex(t, r)
	for i, s := range []*Snapshot{d.snapA, d.snapB} {
		hw := newHashWriter()
		if err := r.ReadSnapshot(ctx, idx, s, hw); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
		if hw.sum() != hashOf(t, newStream(uint64(11+i), dataSnapSize, nil)) { // #nosec G115 -- 0 or 1
			t.Fatalf("snapshot %d reads back different data", i)
		}
	}
	for key, want := range map[string][]byte{d.walKey: d.segA, d.opKey: d.segB} {
		got, err := r.GetSegment(ctx, key)
		if err != nil || string(got) != string(want) {
			t.Fatalf("segment %s: %v", key, err)
		}
	}
}
