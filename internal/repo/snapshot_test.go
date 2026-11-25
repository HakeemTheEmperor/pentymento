// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/local"
	"github.com/HakeemTheEmperor/pentymento/internal/chunker"
)

// stream generates size bytes deterministically from seed without holding
// them: 64 KiB blocks alternating between random and text-like content.
// inserts adds extra bytes at given offsets of the original stream, to make
// a slightly changed copy.
type stream struct {
	seed    uint64
	size    int64
	inserts map[int64][]byte

	pos   int64 // in the original stream
	block []byte
	bi    int64 // index of the block in block
	pend  []byte
}

const blockSize = 64 << 10

func newStream(seed uint64, size int64, inserts map[int64][]byte) *stream {
	return &stream{seed: seed, size: size, inserts: inserts, bi: -1}
}

func (s *stream) blockAt(i int64) []byte {
	if s.bi == i {
		return s.block
	}
	rng := rand.New(rand.NewPCG(s.seed, uint64(i))) // #nosec G404 G115 -- test data
	b := make([]byte, blockSize)
	if i%2 == 0 {
		for j := 0; j+8 <= len(b); j += 8 {
			binary.LittleEndian.PutUint64(b[j:], rng.Uint64())
		}
	} else {
		words := []string{"INSERT ", "INTO ", "orders ", "VALUES ", "(42, ", "'pending', ", "19.99);\n"}
		for j := 0; j < len(b); {
			j += copy(b[j:], words[rng.IntN(len(words))])
		}
	}
	s.block, s.bi = b, i
	return b
}

func (s *stream) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	if s.pos >= s.size {
		return 0, io.EOF
	}
	if ins, ok := s.inserts[s.pos]; ok {
		delete(s.inserts, s.pos)
		s.pend = append([]byte(nil), ins...)
		return s.Read(p)
	}
	end := s.size
	for off := range s.inserts {
		if off > s.pos && off < end {
			end = off
		}
	}
	b := s.blockAt(s.pos / blockSize)
	in := s.pos % blockSize
	n := min(int64(len(p)), int64(len(b))-in, end-s.pos)
	copy(p, b[in:in+n])
	s.pos += n
	return int(n), nil
}

func hashOf(t testing.TB, r io.Reader) [32]byte {
	t.Helper()
	h := blake3.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

// hashWriter hashes what is written to it.
type hashWriter struct {
	h *blake3.Hasher
	n int64
}

func newHashWriter() *hashWriter { return &hashWriter{h: blake3.New()} }

func (w *hashWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return w.h.Write(p)
}

func (w *hashWriter) sum() [32]byte {
	var s [32]byte
	w.h.Sum(s[:0])
	return s
}

func loadIndex(t testing.TB, r *Repository) *Index {
	t.Helper()
	idx, err := r.LoadIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func writeSnap(t testing.TB, r *Repository, idx *Index, src io.Reader) (*Snapshot, WriteStats) {
	t.Helper()
	s, st, err := r.WriteSnapshot(context.Background(), idx, src, Snapshot{Target: "t1", Engine: "postgres"}, WriteOptions{Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func TestSnapshotRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	idx := loadIndex(t, f.repo)
	for i, size := range []int64{0, 1, 100 << 10, 3 << 20, 40 << 20} {
		want := hashOf(t, newStream(uint64(i), size, nil))
		s, st := writeSnap(t, f.repo, idx, newStream(uint64(i), size, nil))
		if s.Size != size || st.Bytes != size {
			t.Errorf("size %d: snapshot size %d, stats %d", size, s.Size, st.Bytes)
		}
		// Read back with a fresh repository and index, as a restore would.
		r := f.reopen(t, f.full, "admin")
		got, err := r.LoadSnapshot(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		hw := newHashWriter()
		if err := r.ReadSnapshot(ctx, loadIndex(t, r), got, hw); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if hw.n != size || hw.sum() != want {
			t.Errorf("size %d: restored %d bytes, hash mismatch %v", size, hw.n, hw.sum() != want)
		}
	}
	snaps, err := f.repo.Snapshots(ctx)
	if err != nil || len(snaps) != 5 {
		t.Errorf("Snapshots = %d, %v", len(snaps), err)
	}
	if _, err := f.repo.LoadSnapshot(ctx, "../config"); err == nil {
		t.Error("LoadSnapshot accepted a malformed ID")
	}
}

func TestDeduplication(t *testing.T) {
	f := newFixture(t)
	idx := loadIndex(t, f.repo)
	const size = 64 << 20
	_, first := writeSnap(t, f.repo, idx, newStream(7, size, nil))
	_, again := writeSnap(t, f.repo, idx, newStream(7, size, nil))
	if again.NewChunks != 0 || again.Packs != 0 {
		t.Errorf("identical stream stored %d new chunks in %d packs", again.NewChunks, again.Packs)
	}
	edits := map[int64][]byte{5 << 20: []byte("UPDATE"), 30<<20 + 777: []byte("a new row"), 50 << 20: bytes.Repeat([]byte("x"), 5000)}
	snap, changed := writeSnap(t, f.repo, idx, newStream(7, size, edits))
	t.Logf("first: %d chunks, %d MiB stored; changed copy: %d new chunks, %d KiB stored",
		first.Chunks, first.NewBytes>>20, changed.NewChunks, changed.NewBytes>>10)
	if changed.NewChunks > 12 || changed.NewBytes > first.NewBytes/10 {
		t.Errorf("a slightly changed stream stored %d new chunks, %d bytes", changed.NewChunks, changed.NewBytes)
	}
	// The changed copy restores exactly.
	want := hashOf(t, newStream(7, size, map[int64][]byte{5 << 20: []byte("UPDATE"), 30<<20 + 777: []byte("a new row"), 50 << 20: bytes.Repeat([]byte("x"), 5000)}))
	hw := newHashWriter()
	if err := f.repo.ReadSnapshot(context.Background(), idx, snap, hw); err != nil || hw.sum() != want {
		t.Errorf("changed snapshot does not restore: %v", err)
	}
}

// TestPartialStreamsNeverCommit: a failing source, a failed child process
// or a cancellation leaves no snapshot and no index file.
func TestPartialStreamsNeverCommit(t *testing.T) {
	boom := errors.New("pg_basebackup exited with status 1")
	cases := map[string]func(*Repository, *Index) error{
		"source error": func(r *Repository, idx *Index) error {
			src := io.MultiReader(io.LimitReader(newStream(1, 40<<20, nil), 30<<20), &failReader{boom})
			_, _, err := r.WriteSnapshot(context.Background(), idx, src, Snapshot{Target: "t"}, WriteOptions{})
			return err
		},
		"child failed": func(r *Repository, idx *Index) error {
			_, _, err := r.WriteSnapshot(context.Background(), idx, newStream(1, 30<<20, nil), Snapshot{Target: "t"},
				WriteOptions{BeforeCommit: func() error { return boom }})
			return err
		},
		"cancelled": func(r *Repository, idx *Index) error {
			ctx, cancel := context.WithCancel(context.Background())
			var n atomic.Int64
			_, _, err := r.WriteSnapshot(ctx, idx, newStream(1, 40<<20, nil), Snapshot{Target: "t"},
				WriteOptions{OnChunk: func(size int, _ bool) {
					if n.Add(int64(size)) > 20<<20 {
						cancel()
					}
				}})
			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if err := run(f.repo, loadIndex(t, f.repo)); err == nil {
				t.Fatal("the write succeeded")
			}
			for _, prefix := range []string{snapshotsPrefix, indexPrefix} {
				if keys, _ := f.repo.listKeys(context.Background(), prefix); len(keys) != 0 {
					t.Errorf("a failed write left %v", keys)
				}
			}
		})
	}
}

type failReader struct{ err error }

func (f *failReader) Read([]byte) (int, error) { return 0, f.err }

// TestAgentWritesButCannotRead: an agent slot can take a snapshot but not
// restore it; a full slot can.
func TestAgentWritesButCannotRead(t *testing.T) {
	f := newFixture(t)
	agent := f.reopen(t, f.agent, "agent")
	idx := loadIndex(t, agent)
	s, _ := writeSnap(t, agent, idx, newStream(3, 5<<20, nil))
	if err := agent.ReadSnapshot(context.Background(), idx, s, io.Discard); !errors.Is(err, ErrNoPrivateKey) {
		t.Errorf("agent ReadSnapshot = %v", err)
	}
	full := f.reopen(t, f.full, "admin")
	hw := newHashWriter()
	if err := full.ReadSnapshot(context.Background(), loadIndex(t, full), s, hw); err != nil || hw.sum() != hashOf(t, newStream(3, 5<<20, nil)) {
		t.Errorf("full slot cannot read the agent's snapshot: %v", err)
	}
}

func TestCorruptionDetected(t *testing.T) {
	f := newFixture(t)
	idx := loadIndex(t, f.repo)
	s, _ := writeSnap(t, f.repo, idx, newStream(4, 20<<20, nil))
	var packs []string
	_ = f.be.List(context.Background(), "data/", func(oi backend.ObjectInfo) error {
		packs = append(packs, oi.Key)
		return nil
	})
	if len(packs) == 0 {
		t.Fatal("no packs written")
	}
	path := filepath.Join(f.dir, filepath.FromSlash(packs[0]))
	b, err := os.ReadFile(path) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	b[100] ^= 0xff
	if err := os.WriteFile(path, b, 0o600); err != nil { // #nosec G703 -- corrupting a pack in the test's own repository
		t.Fatal(err)
	}
	if err := f.repo.ReadSnapshot(context.Background(), idx, s, io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Errorf("flipped byte: ReadSnapshot = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.ReadSnapshot(context.Background(), idx, s, io.Discard); err == nil {
		t.Error("missing pack: ReadSnapshot succeeded")
	}
}

// TestPackTrailers: every pack describes itself, matching the index, so the
// index can be rebuilt from packs alone; pack names are their hashes.
func TestPackTrailers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	idx := loadIndex(t, f.repo)
	writeSnap(t, f.repo, idx, newStream(5, 50<<20, nil))
	var keys []string
	_ = f.be.List(ctx, "data/", func(oi backend.ObjectInfo) error {
		keys = append(keys, oi.Key)
		return nil
	})
	if len(keys) < 2 {
		t.Fatalf("want several packs, got %d", len(keys))
	}
	total := 0
	for _, k := range keys {
		b, err := readAll(ctx, f.be, k, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		if sum := blake3.Sum256(b); packKey(PackID(sum)) != k {
			t.Errorf("%s: name is not the hash of its content", k)
		}
		if len(b) > PackSize+chunker.MaxSize+(1<<20) {
			t.Errorf("%s is %d bytes", k, len(b))
		}
		tr, err := f.repo.readTrailer(b)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		for _, e := range tr.entries {
			loc, wrapped, ok := idx.Lookup(e.ID)
			if !ok || packKey(loc.Pack) != k || loc.Offset != e.Offset || !bytes.Equal(wrapped, tr.wrappedKey) {
				t.Errorf("%s: trailer entry does not match the index", k)
			}
		}
		total += len(tr.entries)
	}
	if total != idx.Len() {
		t.Errorf("trailers list %d chunks, index %d", total, idx.Len())
	}
	if _, err := f.repo.readTrailer([]byte("not a pack at all")); err == nil {
		t.Error("readTrailer accepted garbage")
	}
}

func TestInsecureSnapshots(t *testing.T) {
	be, _ := newLocal(t)
	ctx := context.Background()
	r, err := Create(ctx, be, NewConfig(true, time.Now()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	idx := loadIndex(t, r)
	s, _ := writeSnap(t, r, idx, newStream(6, 10<<20, nil))
	hw := newHashWriter()
	if err := r.ReadSnapshot(ctx, loadIndex(t, r), s, hw); err != nil || hw.sum() != hashOf(t, newStream(6, 10<<20, nil)) {
		t.Errorf("insecure round trip: %v", err)
	}
}

func newLocal(t *testing.T) (backend.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	be, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return be, dir
}

func TestIndexFormat(t *testing.T) {
	packs := []packRecord{
		{id: PackID{1}, wrappedKey: []byte("wrapped-1"), entries: []Entry{{ID: chunker.ID{9}, Offset: 0, Length: 100, RawLen: 300}}},
		{id: PackID{2}, wrappedKey: nil, entries: nil},
	}
	back, err := parseIndex(marshalIndex(packs))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(back) != fmt.Sprint(packs) {
		t.Errorf("index round trip:\n%v\n%v", back, packs)
	}
	good := marshalIndex(packs)
	for name, b := range map[string][]byte{
		"empty": nil, "magic": append([]byte("XXXX"), good[4:]...), "version": append(append([]byte("PTMi"), 9), good[5:]...),
		"truncated": good[:len(good)-3], "trailing": append(append([]byte(nil), good...), 0),
	} {
		if _, err := parseIndex(b); err == nil {
			t.Errorf("%s: parseIndex accepted it", name)
		}
	}
}

func FuzzParseIndex(f *testing.F) {
	f.Add(marshalIndex([]packRecord{{id: PackID{1}, wrappedKey: []byte("k"), entries: []Entry{{ID: chunker.ID{2}}}}}))
	f.Fuzz(func(_ *testing.T, b []byte) { _, _ = parseIndex(b) })
}

func FuzzParseTrailer(f *testing.F) {
	f.Add(packTrailer{wrappedKey: []byte("k"), entries: []Entry{{ID: chunker.ID{3}}}}.marshal())
	f.Fuzz(func(_ *testing.T, b []byte) { _, _ = parseTrailer(b) })
}

// heapCeiling bounds the live heap while writing, reading and re-writing a
// stream of any size. The write path's buffers are bounded by design: at
// most 2×workers prepared chunks (raw and compressed, up to 8 MiB each:
// 64 MiB with 4 workers) and four pack buffers (one being built, one
// waiting, two uploading: 4 × 21 MiB), plus zstd and runtime working
// memory, about 220 MiB at worst. How full those queues get depends on
// machine load, not on the stream's size: measured 92–144 MiB from 64 MiB
// to 4 GiB on a quiet machine, and 207 MiB under heavy load at both 256 MiB
// and 4 GiB. A leak that grows with the stream would pass this ceiling
// long before 4 GiB, which the nightly pipeline runs.
const heapCeiling = 320 << 20

// smallStreamMiB is a baseline logged next to the large stream.
const smallStreamMiB = 64

// TestLargeSnapshot is the C09 Done-when: a large synthetic stream
// round-trips with flat memory, and a slightly changed copy deduplicates.
// The size is PENTYM_TEST_LARGE_MIB (default 256; the nightly pipeline and
// a local check use 4096).
func TestLargeSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("large stream")
	}
	mib := int64(256)
	if v := os.Getenv("PENTYM_TEST_LARGE_MIB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		mib = n
	}
	small := roundTrip(t, smallStreamMiB)
	large := roundTrip(t, mib)
	t.Logf("peak live heap: %d MiB for %d MiB, %d MiB for %d MiB (ceiling %d MiB)",
		small>>20, smallStreamMiB, large>>20, mib, heapCeiling>>20)
	if large > heapCeiling {
		t.Errorf("peak live heap %d MiB for %d MiB exceeds the %d MiB ceiling: memory is not bounded",
			large>>20, mib, heapCeiling>>20)
	}
}

// roundTrip writes a stream of mib MiB, reads it back and checks it, writes
// a slightly changed copy and checks it deduplicates, and returns the peak
// live heap.
func roundTrip(t *testing.T, mib int64) uint64 {
	t.Helper()
	size := mib << 20
	f := newFixture(t)
	ctx := context.Background()
	idx := loadIndex(t, f.repo)
	runtime.GC()

	peak := sampleHeap(t)
	start := time.Now()
	s, st := writeSnap(t, f.repo, idx, newStream(11, size, nil))
	writeTime := time.Since(start)
	hw := newHashWriter()
	if err := f.repo.ReadSnapshot(ctx, idx, s, hw); err != nil {
		t.Fatal(err)
	}
	if hw.n != size || hw.sum() != hashOf(t, newStream(11, size, nil)) {
		t.Fatalf("%d MiB: restored stream differs", mib)
	}
	edits := map[int64][]byte{size / 3: []byte("changed row"), size / 2: bytes.Repeat([]byte("z"), 4096)}
	_, ch := writeSnap(t, f.repo, idx, newStream(11, size, edits))
	heap := peak()

	t.Logf("%d MiB: %d chunks in %d packs, %.0f MiB/s written; peak live heap %d MiB; changed copy stored %d new chunks (%d KiB)",
		mib, st.Chunks, st.Packs, float64(mib)/writeTime.Seconds(), heap>>20, ch.NewChunks, ch.NewBytes>>10)
	if ch.NewChunks > 8 || ch.NewBytes > st.NewBytes/20 {
		t.Errorf("%d MiB: changed copy stored %d new chunks, %d bytes", mib, ch.NewChunks, ch.NewBytes)
	}
	return heap
}

// sampleHeap samples the live heap (after a forced collection, so garbage
// awaiting collection does not count) until the returned function is
// called, and returns the peak.
func sampleHeap(t *testing.T) func() uint64 {
	t.Helper()
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		var ms runtime.MemStats
		for {
			runtime.GC()
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	return func() uint64 {
		close(stop)
		wg.Wait()
		return peak.Load()
	}
}
