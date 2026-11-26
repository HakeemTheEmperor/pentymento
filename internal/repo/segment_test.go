// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

func walData(seed byte) []byte {
	b := make([]byte, 16<<20)
	for i := range b {
		if i%4096 < 64 {
			b[i] = seed + byte(i/4096) // #nosec G115 -- test data
		}
	}
	return b
}

func TestWALSegments(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tid, _ := f.repo.TargetID("prod-pg")
	seg1, seg2 := walData(1), walData(2)
	if err := f.repo.PutWALSegment(ctx, tid, 1, "000000010000000000000003", seg1); err != nil {
		t.Fatal(err)
	}
	// Storing the same segment again (after a crash and resend) succeeds.
	if err := f.repo.PutWALSegment(ctx, tid, 1, "000000010000000000000003", seg1); err != nil {
		t.Errorf("duplicate segment: %v", err)
	}
	if err := f.repo.PutWALPartial(ctx, tid, 1, "000000010000000000000004", 0x4_0000_1000, seg2[:4096]); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.PutWALPartial(ctx, tid, 1, "000000010000000000000004", 0x4_0020_0000, seg2[:2<<20]); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.PutWALSegment(ctx, tid, 2, "000000020000000000000004", seg2); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.PutTimelineHistory(ctx, tid, 2, []byte("1\t0/4000000\tno recovery target specified\n")); err != nil {
		t.Fatal(err)
	}

	segs, err := f.repo.WALSegments(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 4 {
		t.Fatalf("WALSegments = %d: %+v", len(segs), segs)
	}
	want := []struct {
		tl      Timeline
		name    string
		partial bool
		end     LSN
	}{
		{1, "000000010000000000000003", false, 0},
		{1, "000000010000000000000004", true, 0x4_0000_1000},
		{1, "000000010000000000000004", true, 0x4_0020_0000},
		{2, "000000020000000000000004", false, 0},
	}
	for i, w := range want {
		s := segs[i]
		if s.Timeline != w.tl || s.Name != w.name || s.Partial != w.partial || s.EndLSN != w.end || s.Size == 0 {
			t.Errorf("segment %d = %+v, want %+v", i, s, w)
		}
	}
	if !strings.HasSuffix(segs[0].Key, "wal/"+tid+"/00000001/000000010000000000000003.zst.enc") ||
		!strings.Contains(segs[2].Key, ".partial-0000000400200000.zst.enc") {
		t.Errorf("keys: %s, %s", segs[0].Key, segs[2].Key)
	}
	// WAL compresses well, and each segment is its own object.
	if segs[0].Size > int64(len(seg1))/10 {
		t.Errorf("a 16 MiB segment was stored in %d bytes", segs[0].Size)
	}

	for i, data := range [][]byte{seg1, seg2[:4096], seg2[:2<<20], seg2} {
		got, err := f.repo.GetSegment(ctx, segs[i].Key)
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("segment %d round trip: %v", i, err)
		}
	}
	hist, err := f.repo.GetSegment(ctx, f.repo.TimelineHistoryKey(tid, 2))
	if err != nil || !strings.HasPrefix(string(hist), "1\t0/4000000") {
		t.Errorf("timeline history: %q, %v", hist, err)
	}

	for _, bad := range []string{"3", "000000010000000000000003.partial", "00000001000000000000000g"} {
		if err := f.repo.PutWALSegment(ctx, tid, 1, bad, seg1); err == nil {
			t.Errorf("segment name %q accepted", bad)
		}
	}
	if err := f.repo.PutWALSegment(ctx, tid, 1, "000000010000000000000009", make([]byte, MaxSegmentSize+1)); err == nil {
		t.Error("oversized segment accepted")
	}
}

func TestOplogBatches(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tid, _ := f.repo.TargetID("prod-mg")
	batches := []struct {
		first, last OplogTS
		data        string
	}{
		{OplogTS{1732540000, 1}, OplogTS{1732540009, 4}, "batch one"},
		{OplogTS{1732540009, 5}, OplogTS{1732540019, 1}, "batch two"},
		{OplogTS{1732540100, 1}, OplogTS{1732540100, 1}, "single entry"},
	}
	for _, b := range batches {
		if err := f.repo.PutOplogBatch(ctx, tid, b.first, b.last, []byte(b.data)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.repo.OplogBatches(ctx, tid)
	if err != nil || len(got) != 3 {
		t.Fatalf("OplogBatches = %+v, %v", got, err)
	}
	for i, b := range batches {
		if got[i].First != b.first || got[i].Last != b.last {
			t.Errorf("batch %d = %+v", i, got[i])
		}
		data, err := f.repo.GetSegment(ctx, got[i].Key)
		if err != nil || string(data) != b.data {
			t.Errorf("batch %d content = %q, %v", i, data, err)
		}
	}
	if err := f.repo.PutOplogBatch(ctx, tid, OplogTS{5, 0}, OplogTS{4, 9}, []byte("x")); err == nil {
		t.Error("a batch ending before it starts was accepted")
	}
	if !(OplogTS{1, 9}).Less(OplogTS{2, 0}) || (OplogTS{2, 1}).Less(OplogTS{2, 1}) {
		t.Error("OplogTS.Less is wrong")
	}
}

// TestSegmentsWriteOnly: an agent stores segments but cannot read them;
// a segment moved to another key does not open.
func TestSegmentsWriteOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	agent := f.reopen(t, f.agent, "agent")
	tid, _ := agent.TargetID("prod-pg")
	if err := agent.PutWALSegment(ctx, tid, 1, "000000010000000000000001", []byte("wal bytes")); err != nil {
		t.Fatal(err)
	}
	key := walSegmentKey(tid, 1, "000000010000000000000001")
	if _, err := agent.GetSegment(ctx, key); !errors.Is(err, ErrNoPrivateKey) {
		t.Errorf("agent GetSegment = %v", err)
	}
	full := f.reopen(t, f.full, "admin")
	if got, err := full.GetSegment(ctx, key); err != nil || string(got) != "wal bytes" {
		t.Errorf("full GetSegment = %q, %v", got, err)
	}
	b, _ := readAll(ctx, f.be, key, 1<<20)
	moved := walSegmentKey(tid, 1, "000000010000000000000002")
	_ = f.be.Put(ctx, moved, bytes.NewReader(b), backend.PutOptions{})
	if _, err := full.GetSegment(ctx, moved); !errors.Is(err, ErrCorrupt) {
		t.Errorf("moved segment opened: %v", err)
	}
	locked, _ := Open(ctx, f.be)
	if err := locked.PutWALSegment(ctx, tid, 1, "000000010000000000000003", []byte("x")); !errors.Is(err, ErrLocked) {
		t.Errorf("locked PutWALSegment = %v", err)
	}
}

func TestInsecureSegments(t *testing.T) {
	be, _ := newLocal(t)
	ctx := context.Background()
	r, err := Create(ctx, be, NewConfig(true, true, time.Now()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tid, _ := r.TargetID("dev")
	if err := r.PutOplogBatch(ctx, tid, OplogTS{1, 1}, OplogTS{2, 2}, []byte("oplog")); err != nil {
		t.Fatal(err)
	}
	bs, _ := r.OplogBatches(ctx, tid)
	if got, err := r.GetSegment(ctx, bs[0].Key); err != nil || string(got) != "oplog" {
		t.Errorf("insecure round trip: %q, %v", got, err)
	}
}

// countingBackend counts Get calls by key prefix.
type countingBackend struct {
	backend.Backend
	mu   sync.Mutex
	gets map[string]int
}

func (c *countingBackend) Get(ctx context.Context, key string, rng *backend.Range) (io.ReadCloser, error) {
	c.mu.Lock()
	if c.gets == nil {
		c.gets = map[string]int{}
	}
	c.gets[strings.SplitN(key, "/", 2)[0]]++
	c.mu.Unlock()
	return c.Backend.Get(ctx, key, rng)
}

func (c *countingBackend) count(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.gets[prefix]
	c.gets = nil
	return n
}

func TestMetadataCache(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := range 3 {
		writeSnap(t, f.repo, loadIndex(t, f.repo), newStream(uint64(20+i), 2<<20, nil))
	}
	cb := &countingBackend{Backend: f.be}
	r, err := Open(ctx, cb)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := f.full.UnlockPassphrase(secrets.New([]byte("admin")))
	if err := r.Unlock(ctx, *u); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	r.SetCache(NewCache(cacheDir))
	cb.count("")

	// First reads fill the cache; later ones make no index or snapshot GETs.
	idx1 := loadIndex(t, r)
	snaps1, err := r.Snapshots(ctx)
	if err != nil || len(snaps1) != 3 {
		t.Fatalf("Snapshots = %d, %v", len(snaps1), err)
	}
	if cb.count("index") == 0 {
		t.Fatal("first LoadIndex made no GETs")
	}
	idx2 := loadIndex(t, r)
	snaps2, _ := r.Snapshots(ctx)
	if n := cb.count("index"); n != 0 {
		t.Errorf("cached LoadIndex made %d index GETs", n)
	}
	if n := cb.count("snapshots"); n != 0 {
		t.Errorf("cached Snapshots made %d snapshot GETs", n)
	}
	if idx1.Len() != idx2.Len() || len(snaps2) != 3 {
		t.Error("cached reads differ")
	}

	// Cached files are the repository's bytes, still encrypted.
	var cached []string
	_ = filepath.WalkDir(cacheDir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			cached = append(cached, p)
		}
		return nil
	})
	if len(cached) == 0 {
		t.Fatal("nothing cached")
	}
	for _, p := range cached {
		rel, _ := filepath.Rel(cacheDir, p)
		local, _ := os.ReadFile(p) // #nosec G304 G122 -- test cache directory
		remote, _ := readAll(ctx, f.be, filepath.ToSlash(rel), 1<<30)
		if !bytes.Equal(local, remote) {
			t.Errorf("cache entry %s differs from the repository", rel)
		}
	}

	// A damaged entry is detected and fetched again.
	for _, p := range cached {
		_ = os.WriteFile(p, []byte("damaged"), 0o600) // #nosec G703 -- test cache directory
	}
	idx3 := loadIndex(t, r)
	if _, err := r.Snapshots(ctx); err != nil {
		t.Fatalf("Snapshots with a damaged cache: %v", err)
	}
	if idx3.Len() != idx1.Len() || cb.count("index") == 0 {
		t.Error("damaged cache entries were not fetched again")
	}
	if n := countFiles(cacheDir); n != len(cached) {
		t.Errorf("cache was not repaired: %d files, want %d", n, len(cached))
	}

	// Entries for objects that no longer exist are dropped.
	snapKeys, _ := r.listKeys(ctx, snapshotsPrefix)
	_ = f.be.Delete(ctx, snapKeys[0])
	if _, err := r.Snapshots(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.cache.get(snapKeys[0]); ok {
		t.Error("a deleted snapshot stayed in the cache")
	}

	if d, err := DefaultCacheDir(r.Config().ID); err != nil || !strings.Contains(d, r.Config().ID) {
		t.Errorf("DefaultCacheDir = %q, %v", d, err)
	}
}

func countFiles(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
