// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// countingGets counts Get calls and the most in flight at once.
type countingGets struct {
	backend.Backend
	gets, inFlight, peak atomic.Int32
	delay                time.Duration
}

func (c *countingGets) Get(ctx context.Context, key string, rng *backend.Range) (io.ReadCloser, error) {
	c.gets.Add(1)
	n := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for p := c.peak.Load(); n > p; p = c.peak.Load() {
		if c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	return c.Backend.Get(ctx, key, rng)
}

// openOn opens the fixture's repository through be and unlocks it with the
// full slot.
func (f *fixture) openOn(t *testing.T, be backend.Backend, slot *crypto.Slot) *Repository {
	t.Helper()
	r, err := Open(context.Background(), be)
	if err != nil {
		t.Fatal(err)
	}
	u, err := slot.UnlockPassphrase(secrets.New([]byte("admin")))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Unlock(context.Background(), *u); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestParallelRead: a stream is read with one ranged request per run of
// adjacent chunks, several at once, and comes back intact.
func TestParallelRead(t *testing.T) {
	f := newFixture(t)
	const size = 48 << 20
	want := hashOf(t, newStream(21, size, nil))
	s, st := writeSnap(t, f.repo, loadIndex(t, f.repo), newStream(21, size, nil))
	cb := &countingGets{Backend: f.be, delay: 2 * time.Millisecond}
	r := f.openOn(t, cb, f.full)
	idx := loadIndex(t, r)
	cb.gets.Store(0)
	hw := newHashWriter()
	if err := r.ReadSnapshot(context.Background(), idx, s, hw); err != nil {
		t.Fatal(err)
	}
	if hw.n != size || hw.sum() != want {
		t.Fatalf("read %d bytes, hash mismatch %v", hw.n, hw.sum() != want)
	}
	// About 48 chunks in about 3 packs: far fewer requests than chunks.
	if g := cb.gets.Load(); int64(g) >= st.Chunks/2 {
		t.Errorf("%d requests for %d chunks", g, st.Chunks)
	}
	if p := cb.peak.Load(); p < 2 || p > readWorkers+1 {
		t.Errorf("peak requests in flight = %d, want 2..%d", p, readWorkers+1)
	}
	t.Logf("%d chunks, %d requests, peak %d in flight", st.Chunks, cb.gets.Load(), cb.peak.Load())
}

// failingWriter fails after n bytes.
type failingWriter struct {
	n   int
	err error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n -= len(p); w.n < 0 {
		return 0, w.err
	}
	return len(p), nil
}

// TestReadStops: a writer that fails, a cancelled context, or a corrupt
// chunk stop the read promptly, leaving no goroutine behind.
func TestReadStops(t *testing.T) {
	f := newFixture(t)
	idx := loadIndex(t, f.repo)
	s, _ := writeSnap(t, f.repo, idx, newStream(22, 30<<20, nil))
	before := runtime.NumGoroutine()

	cut := errors.New("disk full")
	if err := f.repo.ReadSnapshot(context.Background(), idx, s, &failingWriter{n: 5 << 20, err: cut}); !errors.Is(err, cut) {
		t.Errorf("failing writer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := f.repo.ReadSnapshot(ctx, idx, s, &cancelAfter{n: 3 << 20, cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	// Chunks missing from the index: whichever of them the read meets
	// first, it reports corruption.
	bad := newIndex()
	bad.keys = idx.keys
	n := 0
	for id, loc := range idx.chunks {
		if n++; n%7 != 0 {
			bad.chunks[id] = loc
		}
	}
	if err := f.repo.ReadSnapshot(context.Background(), bad, s, io.Discard); !errors.Is(err, ErrCorrupt) {
		t.Errorf("missing chunk: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("%d goroutines before the reads, %d after", before, after)
	}
}

// cancelAfter cancels a context after n bytes.
type cancelAfter struct {
	n      int
	cancel context.CancelFunc
}

func (c *cancelAfter) Write(p []byte) (int, error) {
	if c.n -= len(p); c.n < 0 {
		c.cancel()
	}
	return len(p), nil
}
