// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package workpool

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
	"pgregory.net/rapid"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// counter returns a source producing 0..n-1.
func counter(n int) func() (int, bool, error) {
	i := 0
	return func() (int, bool, error) {
		if i == n {
			return 0, false, nil
		}
		i++
		return i - 1, true, nil
	}
}

// TestPropertyOrder: results arrive in input order whatever the workers'
// timing, and concurrency never exceeds the bound.
func TestPropertyOrder(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 200).Draw(rt, "items")
		workers := rapid.IntRange(1, 8).Draw(rt, "workers")
		delays := rapid.SliceOfN(rapid.IntRange(0, 200), n, n).Draw(rt, "delays µs")
		var running, peak atomic.Int32
		var got []int
		err := Run(context.Background(), workers, counter(n),
			func(_ context.Context, i int) (int, error) {
				r := running.Add(1)
				for {
					p := peak.Load()
					if r <= p || peak.CompareAndSwap(p, r) {
						break
					}
				}
				time.Sleep(time.Duration(delays[i]) * time.Microsecond)
				running.Add(-1)
				return i * i, nil
			},
			func(r int) error {
				got = append(got, r)
				return nil
			})
		if err != nil {
			rt.Fatal(err)
		}
		if len(got) != n {
			rt.Fatalf("got %d results, want %d", len(got), n)
		}
		for i, r := range got {
			if r != i*i {
				rt.Fatalf("result %d = %d, want %d: out of order", i, r, i*i)
			}
		}
		if int(peak.Load()) > workers {
			rt.Fatalf("%d workers ran at once, limit %d", peak.Load(), workers)
		}
	})
}

func TestErrors(t *testing.T) {
	boom := errors.New("boom")
	identity := func(_ context.Context, i int) (int, error) { return i, nil }
	discard := func(int) error { return nil }

	t.Run("source", func(t *testing.T) {
		n := 0
		src := func() (int, bool, error) {
			n++
			if n == 5 {
				return 0, false, boom
			}
			return n, true, nil
		}
		if err := Run(context.Background(), 3, src, identity, discard); !errors.Is(err, boom) {
			t.Errorf("Run = %v", err)
		}
	})
	t.Run("work", func(t *testing.T) {
		var sunk []int
		err := Run(context.Background(), 4, counter(100),
			func(_ context.Context, i int) (int, error) {
				if i == 10 {
					return 0, boom
				}
				return i, nil
			},
			func(r int) error { sunk = append(sunk, r); return nil })
		if !errors.Is(err, boom) {
			t.Errorf("Run = %v", err)
		}
		for i, r := range sunk {
			if r != i || r >= 10 {
				t.Fatalf("sink received %v: results after the failure or out of order", sunk)
			}
		}
	})
	t.Run("sink", func(t *testing.T) {
		calls := 0
		err := Run(context.Background(), 4, counter(100), identity, func(int) error {
			calls++
			if calls == 3 {
				return boom
			}
			return nil
		})
		if !errors.Is(err, boom) || calls != 3 {
			t.Errorf("Run = %v after %d sink calls", err, calls)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		err := Run(ctx, 2, counter(1000),
			func(ctx context.Context, i int) (int, error) {
				if i == 20 {
					cancel()
				}
				return i, ctx.Err()
			}, discard)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v", err)
		}
	})
	t.Run("no items, no workers", func(t *testing.T) {
		if err := Run(context.Background(), 0, counter(0), identity, discard); err != nil {
			t.Errorf("Run = %v", err)
		}
		var got []int
		if err := Run(context.Background(), -1, counter(3), identity, func(r int) error { got = append(got, r); return nil }); err != nil || len(got) != 3 {
			t.Errorf("workers < 1: %v, %v", got, err)
		}
	})
}
