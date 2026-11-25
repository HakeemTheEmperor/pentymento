// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package backendtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// Injected failures, matched with errors.Is.
var (
	// ErrInjected is a failure injected before the call reached the backend,
	// so it had no effect.
	ErrInjected = errors.New("injected failure")
	// ErrAmbiguous is a failure injected after the call reached the backend
	// and succeeded, as when a response is lost: the effect may have
	// happened. It wraps ErrInjected.
	ErrAmbiguous = fmt.Errorf("%w after the call took effect", ErrInjected)
	// ErrCrashed means the simulated process has crashed; every call from
	// the crash on fails.
	ErrCrashed = errors.New("simulated crash")
)

// Faults configures a Faulty backend. Rates are probabilities from 0 to 1;
// choices come from a generator seeded with Seed, so a run is
// reproducible.
type Faults struct {
	Seed uint64
	// Latency is added before every call.
	Latency time.Duration
	// ErrorRate is the chance a call fails with ErrInjected without
	// reaching the backend.
	ErrorRate float64
	// AmbiguousRate is the chance a call reaches the backend and then
	// fails with ErrAmbiguous anyway.
	AmbiguousRate float64
	// CutRate is the chance a Put's upload is cut part-way: the backend
	// sees its reader fail.
	CutRate float64
	// CrashAtCall makes call number N (from 1) crash: a Put's upload is cut
	// part-way, and that call and every later one fail with ErrCrashed.
	// 0 means never.
	CrashAtCall int
}

// Faulty wraps a backend and injects the configured faults.
type Faulty struct {
	inner backend.Backend
	f     Faults

	mu      sync.Mutex
	rng     *rand.Rand
	calls   int
	crashed bool
}

var _ backend.Backend = (*Faulty)(nil)

// Wrap returns inner with faults injected.
func Wrap(inner backend.Backend, f Faults) *Faulty {
	return &Faulty{inner: inner, f: f, rng: rand.New(rand.NewPCG(f.Seed, f.Seed^0x9e3779b97f4a7c15))} // #nosec G404 -- reproducible test randomness
}

// Calls returns how many calls have been made.
func (w *Faulty) Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// Crashed reports whether the simulated crash has happened.
func (w *Faulty) Crashed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.crashed
}

// plan is what happens to one call.
type plan struct {
	fail      error // return this without calling the backend
	cutAt     int64 // for Put: fail the upload after this many bytes; -1 means no cut
	ambiguous bool  // call the backend, then return ErrAmbiguous
	crash     bool  // this call is the crash
}

func (w *Faulty) next(ctx context.Context) plan {
	if w.f.Latency > 0 {
		t := time.NewTimer(w.f.Latency)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	p := plan{cutAt: -1}
	switch {
	case w.crashed:
		p.fail = ErrCrashed
	case w.f.CrashAtCall > 0 && w.calls == w.f.CrashAtCall:
		w.crashed = true
		p.crash = true
		p.cutAt = w.rng.Int64N(64 << 10)
	case w.rng.Float64() < w.f.ErrorRate:
		p.fail = ErrInjected
	case w.rng.Float64() < w.f.CutRate:
		p.cutAt = w.rng.Int64N(64 << 10)
	case w.rng.Float64() < w.f.AmbiguousRate:
		p.ambiguous = true
	}
	return p
}

// Put stores r, possibly cutting the upload or failing.
func (w *Faulty) Put(ctx context.Context, key string, r io.Reader, opts backend.PutOptions) error {
	p := w.next(ctx)
	if p.fail != nil {
		return p.fail
	}
	if p.cutAt >= 0 {
		r = &cutReader{r: r, left: p.cutAt}
	}
	err := w.inner.Put(ctx, key, r, opts)
	switch {
	case p.crash:
		return ErrCrashed
	case p.cutAt >= 0 && err == nil:
		// The content was shorter than the cut point, so the upload
		// completed: report it as an ambiguous failure.
		return ErrAmbiguous
	case p.ambiguous && err == nil:
		return ErrAmbiguous
	}
	return err
}

// Get opens the object, or fails.
func (w *Faulty) Get(ctx context.Context, key string, rng *backend.Range) (io.ReadCloser, error) {
	if p := w.next(ctx); p.fail != nil || p.crash {
		return nil, failure(p)
	}
	return w.inner.Get(ctx, key, rng)
}

// Stat returns the object's metadata, or fails.
func (w *Faulty) Stat(ctx context.Context, key string) (backend.ObjectInfo, error) {
	if p := w.next(ctx); p.fail != nil || p.crash {
		return backend.ObjectInfo{}, failure(p)
	}
	return w.inner.Stat(ctx, key)
}

// List lists objects, or fails.
func (w *Faulty) List(ctx context.Context, prefix string, fn func(backend.ObjectInfo) error) error {
	if p := w.next(ctx); p.fail != nil || p.crash {
		return failure(p)
	}
	return w.inner.List(ctx, prefix, fn)
}

// Delete removes the object, or fails, possibly after deleting it.
func (w *Faulty) Delete(ctx context.Context, key string) error {
	p := w.next(ctx)
	if p.fail != nil || p.crash {
		return failure(p)
	}
	err := w.inner.Delete(ctx, key)
	if p.ambiguous && err == nil {
		return ErrAmbiguous
	}
	return err
}

// Capabilities reports the wrapped backend's capabilities.
func (w *Faulty) Capabilities() backend.Capabilities { return w.inner.Capabilities() }

func failure(p plan) error {
	if p.crash {
		return ErrCrashed
	}
	return p.fail
}

// errCut is what a cut upload's reader returns.
var errCut = errors.New("upload cut")

// cutReader fails after left bytes.
type cutReader struct {
	r    io.Reader
	left int64
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errCut
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
