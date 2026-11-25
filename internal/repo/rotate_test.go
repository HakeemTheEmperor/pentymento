// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/chunker"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// unlocked returns the admin slot's keys.
func (d *dataRepo) unlocked(t *testing.T) crypto.Unlocked {
	t.Helper()
	u, err := d.full.UnlockPassphrase(secrets.New([]byte("admin")))
	if err != nil {
		t.Fatal(err)
	}
	return *u
}

func rotate(t *testing.T, d *dataRepo) *RotateStats {
	t.Helper()
	st, err := d.open(t).RotateKeys(context.Background(), d.unlocked(t))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// allCurrent checks every data key in the index and every segment opens
// with the current private key alone.
func (d *dataRepo) allCurrent(t *testing.T, r *Repository) {
	t.Helper()
	current := crypto.Keyring{r.keys.Current()}
	idx := loadIndex(t, r)
	for id, w := range idx.keys {
		if _, _, err := current.Unwrap(w); err != nil {
			t.Errorf("pack %s: data key is not wrapped to the current key", id)
		}
	}
	for _, k := range []string{d.walKey, d.opKey} {
		b, err := readAll(context.Background(), d.be, k, MaxSegmentSize+(1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := current.OpenObject(k, b); err != nil {
			t.Errorf("%s: not wrapped to the current key", k)
		}
	}
}

func TestRotateKeys(t *testing.T) {
	d := newDataRepo(t)
	before := d.open(t)
	oldPub := before.PublicKey().String()
	oldPriv := before.keys.Current()

	st := rotate(t, d)
	if st.PreviousKey != "keys/private-000001" || st.IndexFiles == 0 || st.Packs < 2 || st.Segments != 2 {
		t.Errorf("stats = %+v", st)
	}
	r := d.open(t) // the same slot still unlocks: slots protect the master key
	if r.PublicKey().String() == oldPub || len(r.keys) != 2 {
		t.Fatalf("after rotation: public key unchanged or keyring of %d", len(r.keys))
	}
	d.readsBack(t, r)
	d.allCurrent(t, r)

	// New data is wrapped to the new key only: the old private key cannot
	// read it.
	idx := loadIndex(t, r)
	s := writeTargetSnap(t, r, idx, d.tidA, 77)
	id, _ := chunker.ParseID(s.List[0])
	_, wrapped, _ := loadIndex(t, r).Lookup(id)
	if _, err := crypto.UnwrapKey(oldPriv, wrapped); err == nil {
		t.Error("the replaced private key opens data written after the rotation")
	}

	// An agent picks up the new public key.
	agent := d.reopen(t, d.agent, "agent")
	if agent.PublicKey().String() != r.PublicKey().String() {
		t.Error("an agent slot sees the old public key")
	}

	// A second rotation keeps both previous keys.
	st = rotate(t, d)
	if st.PreviousKey != "keys/private-000002" {
		t.Errorf("second rotation kept %s", st.PreviousKey)
	}
	r = d.open(t)
	if len(r.keys) != 3 {
		t.Errorf("keyring of %d after two rotations", len(r.keys))
	}
	d.readsBack(t, r)
	d.allCurrent(t, r)
	if rep := check(t, r, CheckOptions{Full: true}); len(rep.Problems) != 0 {
		t.Errorf("check after rotations:\n%s", problemsText(rep))
	}
}

func TestRotateNeedsFullSlot(t *testing.T) {
	d := newDataRepo(t)
	agent := d.reopen(t, d.agent, "agent")
	if _, err := agent.RotateKeys(context.Background(), d.unlocked(t)); !errors.Is(err, ErrNoPrivateKey) {
		t.Errorf("RotateKeys with an agent unlock = %v", err)
	}
}

// failingPuts fails writes to keys with a given prefix, after the first
// skip of them, like a crash at that point.
type failingPuts struct {
	backend.Backend
	prefix string
	skip   int
}

var errCrash = errors.New("simulated crash")

func (f *failingPuts) Put(ctx context.Context, key string, r io.Reader, o backend.PutOptions) error {
	if strings.HasPrefix(key, f.prefix) {
		if f.skip == 0 {
			return errCrash
		}
		f.skip--
	}
	return f.Backend.Put(ctx, key, r, o)
}

// TestRotateInterrupted stops a rotation at each step; the repository
// stays readable, and running the rotation again finishes the job.
func TestRotateInterrupted(t *testing.T) {
	for _, tt := range []struct {
		name   string
		prefix string
		skip   int
	}{
		{"before the new private key", privateKeyObject, 0},
		{"between the private and public keys", publicKeyObject, 0},
		{"before the index", indexPrefix, 0},
		{"before the segments", walPrefixRoot, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newDataRepo(t)
			ctx := context.Background()
			r, err := Open(ctx, &failingPuts{Backend: d.be, prefix: tt.prefix, skip: tt.skip})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Unlock(ctx, d.unlocked(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := r.RotateKeys(ctx, d.unlocked(t)); !errors.Is(err, errCrash) {
				t.Fatalf("RotateKeys = %v, want the simulated crash", err)
			}
			after := d.open(t)
			d.readsBack(t, after)
			// New writes after the interruption are readable too.
			s := writeTargetSnap(t, after, loadIndex(t, after), d.tidA, 55)
			if err := d.open(t).ReadSnapshot(ctx, loadIndex(t, d.open(t)), s, io.Discard); err != nil {
				t.Fatalf("snapshot written after the interruption: %v", err)
			}
			rotate(t, d)
			final := d.open(t)
			d.readsBack(t, final)
			d.allCurrent(t, final)
		})
	}
}

// TestRebuildAfterRotation: pack trailers keep data keys wrapped to the
// key of the day; after rotation, an index rebuilt from them still reads,
// and the next rotation re-wraps them.
func TestRebuildAfterRotation(t *testing.T) {
	d := newDataRepo(t)
	rotate(t, d)
	st, err := d.open(t).RebuildIndex(context.Background())
	if err != nil || st.Packs < 2 || len(st.Unreadable) != 0 {
		t.Fatalf("RebuildIndex = %+v, %v", st, err)
	}
	r := d.open(t)
	d.readsBack(t, r)
	if rep := check(t, r, CheckOptions{Full: true}); len(rep.Problems) != 0 {
		t.Errorf("check after the rebuild:\n%s", problemsText(rep))
	}
	rotate(t, d)
	d.allCurrent(t, d.open(t))
}

// TestBadPreviousKey: a replaced private key that does not open is
// reported by check, and the rest stays usable.
func TestBadPreviousKey(t *testing.T) {
	d := newDataRepo(t)
	rotate(t, d)
	d.flip(t, previousKeyKey(1), 30)
	r := d.open(t)
	if got := r.BadPreviousKeys(); len(got) != 1 || got[0] != previousKeyKey(1) {
		t.Fatalf("BadPreviousKeys = %v", got)
	}
	d.readsBack(t, r) // everything was re-wrapped to the current key
	rep := check(t, r, CheckOptions{Full: true})
	if !hasProblem(rep, Problem{Step: CheckSlots, Severity: SeverityError, Kind: ProblemCorrupt, Object: previousKeyKey(1)}) {
		t.Errorf("no problem for the bad previous key:\n%s", problemsText(rep))
	}
}

func TestRebuildIndex(t *testing.T) {
	d := newDataRepo(t)
	ctx := context.Background()
	old := d.keys(t, indexPrefix)
	for _, k := range old {
		d.remove(t, k)
	}
	if loadIndex(t, d.open(t)).Len() != 0 {
		t.Fatal("index not empty")
	}
	// An agent slot is enough to rebuild.
	st, err := d.reopen(t, d.agent, "agent").RebuildIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := d.open(t)
	idx := loadIndex(t, r)
	if st.Packs != idx.Packs() || st.Chunks < idx.Len() || st.Written != 1 || st.Removed != 0 || len(st.Unreadable) != 0 {
		t.Errorf("stats = %+v; index has %d packs, %d chunks", st, idx.Packs(), idx.Len())
	}
	d.readsBack(t, r)

	// A second rebuild replaces the index file (encryption makes each copy
	// differ) and leaves exactly one.
	st, err = r.RebuildIndex(ctx)
	if err != nil || st.Written != 1 || st.Removed != 1 || len(d.keys(t, indexPrefix)) != 1 {
		t.Errorf("second rebuild: %+v, %v, %d files", st, err, len(d.keys(t, indexPrefix)))
	}
	d.readsBack(t, d.open(t))
}

func TestRebuildIndexUnreadablePack(t *testing.T) {
	d := newDataRepo(t)
	bad := d.packOf(t, d.snapB)
	d.flip(t, bad, -20) // inside the encrypted trailer
	st, err := d.open(t).RebuildIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Unreadable) != 1 || packKey(mustPackID(t, st.Unreadable[0])) != bad {
		t.Fatalf("unreadable = %v, want %s", st.Unreadable, bad)
	}
	// a's snapshot still reads; b's chunks are gone from the index.
	r := d.open(t)
	idx := loadIndex(t, r)
	if err := r.ReadSnapshot(context.Background(), idx, d.snapA, io.Discard); err != nil {
		t.Errorf("a: %v", err)
	}
	if err := r.ReadSnapshot(context.Background(), idx, d.snapB, io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Errorf("b: %v", err)
	}
}

// TestRebuildInterrupted: new index files are written before old ones go,
// so a rebuild that stops part-way leaves a readable index.
func TestRebuildInterrupted(t *testing.T) {
	d := newDataRepo(t)
	ctx := context.Background()
	r, err := Open(ctx, &failingDeletes{Backend: d.be})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Unlock(ctx, d.unlocked(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RebuildIndex(ctx); !errors.Is(err, errCrash) {
		t.Fatalf("RebuildIndex = %v", err)
	}
	d.readsBack(t, d.open(t))
	if _, err := d.open(t).RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(d.keys(t, indexPrefix)); n != 1 {
		t.Errorf("%d index files after finishing the rebuild", n)
	}
}

type failingDeletes struct{ backend.Backend }

func (f *failingDeletes) Delete(context.Context, string) error { return errCrash }

func mustPackID(t *testing.T, s string) PackID {
	t.Helper()
	var id PackID
	b := []byte(s)
	if len(b) != 64 {
		t.Fatalf("pack ID %q", s)
	}
	if _, err := hex.Decode(id[:], b); err != nil {
		t.Fatal(err)
	}
	return id
}
