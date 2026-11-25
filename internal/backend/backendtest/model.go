// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package backendtest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// faultKeys are the keys the fault test works on.
var faultKeys = []string{"locks/lease", "data/ab/pack-1", "data/ab/pack-2", "snapshots/s1"}

// state is one possible content of a key; absent means no object.
type state struct {
	absent bool
	data   []byte
}

func (s state) equal(o state) bool {
	return s.absent == o.absent && (s.absent || bytes.Equal(s.data, o.data))
}

// model tracks, for each key, every state it may be in, given that some
// failed calls may still have taken effect.
type model map[string][]state

func (m model) set(key string, s state) { m[key] = []state{s} }

func (m model) add(key string, s state) {
	if !slices.ContainsFunc(m[key], s.equal) {
		m[key] = append(m[key], s)
	}
}

func (m model) mayBe(key string, s state) bool { return slices.ContainsFunc(m[key], s.equal) }

func (m model) definitelyPresent(key string) bool { return !m.mayBe(key, state{absent: true}) }

// testFaults runs random operations through the fault wrapper and checks,
// after every call and at the end, that the backend only ever holds states
// the model allows: whole objects as written, never partial ones, and no
// effect from calls that failed before reaching the backend.
func testFaults(t *testing.T, newBackend NewFunc) {
	runs, ops := 30, 80
	if testing.Short() {
		runs, ops = 8, 40
	}
	// seen counts the outcomes that occurred, so the test cannot pass
	// without exercising each kind of fault.
	seen := map[string]int{}
	t.Cleanup(func() {
		t.Logf("fault outcomes: %v", seen)
		if testing.Short() || t.Failed() {
			return
		}
		for _, k := range []string{"ok", "injected", "ambiguous", "cut", "crash", "exists"} {
			if seen[k] == 0 {
				t.Errorf("no %q outcome in %d runs; the fault test is not exercising it", k, runs)
			}
		}
	})
	for seed := range uint64(runs) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			inner := newBackend(t)
			rng := newRNG(seed + 100)
			f := Faults{
				Seed:          seed,
				ErrorRate:     0.15,
				AmbiguousRate: 0.1,
				CutRate:       0.15,
				CrashAtCall:   1 + rng.IntN(ops+ops/2), // sometimes never within the run
			}
			fb := Wrap(inner, f)
			m := model{}
			for _, k := range faultKeys {
				m.set(k, state{absent: true})
			}
			for range ops {
				key := faultKeys[rng.IntN(len(faultKeys))]
				crashedBefore := fb.Crashed()
				switch op := rng.IntN(10); {
				case op < 5:
					data := randomBytes(rng, rng.IntN(96<<10))
					ifNot := rng.IntN(3) == 0
					err := fb.Put(ctx(t), key, bytes.NewReader(data), backend.PutOptions{IfNotExists: ifNot})
					s := state{data: data}
					seen[outcome(err, crashedBefore)]++
					switch {
					case err == nil:
						if ifNot && m.definitelyPresent(key) {
							t.Fatalf("conditional create of %s succeeded though the object exists", key)
						}
						m.set(key, s)
					case errors.Is(err, backend.ErrExists):
						if !ifNot || m.definitelyAbsent(key) {
							t.Fatalf("Put(%s) = ErrExists, but the object cannot exist", key)
						}
						m[key] = slices.DeleteFunc(m[key], func(s state) bool { return s.absent })
					case errors.Is(err, ErrAmbiguous) || (errors.Is(err, ErrCrashed) && !crashedBefore):
						m.add(key, s)
					case errors.Is(err, ErrInjected), errors.Is(err, ErrCrashed), errors.Is(err, errCut):
						// No effect.
					default:
						t.Fatalf("Put(%s): unexpected error %v", key, err)
					}
				case op < 7:
					err := fb.Delete(ctx(t), key)
					switch {
					case err == nil:
						m.set(key, state{absent: true})
					case errors.Is(err, ErrAmbiguous):
						m.add(key, state{absent: true})
					case errors.Is(err, ErrInjected), errors.Is(err, ErrCrashed):
					default:
						t.Fatalf("Delete(%s): unexpected error %v", key, err)
					}
				default:
					rc, err := fb.Get(ctx(t), key, nil)
					switch {
					case err == nil:
						data, rerr := io.ReadAll(rc)
						_ = rc.Close()
						if rerr != nil {
							t.Fatalf("reading %s: %v", key, rerr)
						}
						s := state{data: data}
						if !m.mayBe(key, s) {
							t.Fatalf("Get(%s) returned %d bytes the model does not allow", key, len(data))
						}
						m.set(key, s)
					case errors.Is(err, backend.ErrNotFound):
						if !m.mayBe(key, state{absent: true}) {
							t.Fatalf("Get(%s) = not found, but it must exist", key)
						}
						m.set(key, state{absent: true})
					case errors.Is(err, ErrInjected), errors.Is(err, ErrCrashed):
					default:
						t.Fatalf("Get(%s): unexpected error %v", key, err)
					}
				}
			}

			// After the run, or the crash, the backend itself ("after a
			// restart") must hold an allowed state for every key and list
			// exactly the objects that exist.
			var present []string
			for _, key := range faultKeys {
				data, err := read(t, inner, key, nil)
				switch {
				case errors.Is(err, backend.ErrNotFound):
					if !m.mayBe(key, state{absent: true}) {
						t.Errorf("after the run, %s is missing but must exist", key)
					}
				case err != nil:
					t.Fatalf("after the run, Get(%s): %v", key, err)
				default:
					present = append(present, key)
					if !m.mayBe(key, state{data: data}) {
						t.Errorf("after the run, %s holds %d bytes that were never fully written", key, len(data))
					}
				}
			}
			slices.Sort(present)
			if got := listKeys(t, inner, ""); !slices.Equal(got, present) {
				t.Errorf("after the run, List = %v, want %v", got, present)
			}
		})
	}
}

func (m model) definitelyAbsent(key string) bool {
	for _, s := range m[key] {
		if !s.absent {
			return false
		}
	}
	return true
}

// outcome names a Put result for the coverage count.
func outcome(err error, crashedBefore bool) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, backend.ErrExists):
		return "exists"
	case errors.Is(err, ErrAmbiguous):
		return "ambiguous"
	case errors.Is(err, ErrCrashed) && !crashedBefore:
		return "crash"
	case errors.Is(err, ErrCrashed):
		return "after crash"
	case errors.Is(err, ErrInjected):
		return "injected"
	case errors.Is(err, errCut):
		return "cut"
	}
	return "other"
}
