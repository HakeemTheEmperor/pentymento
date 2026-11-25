// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package backendtest is the conformance suite every storage backend must
// pass (PRD 10.1), and a fault-injecting wrapper for testing code built on
// backends.
//
// A backend's tests call Run with a function that returns a new, empty
// backend for each subtest.
package backendtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// NewFunc returns a new, empty backend.
type NewFunc func(t *testing.T) backend.Backend

// Run runs the conformance suite.
func Run(t *testing.T, newBackend NewFunc) {
	t.Run("PutGetStat", func(t *testing.T) { testPutGetStat(t, newBackend(t)) })
	t.Run("Overwrite", func(t *testing.T) { testOverwrite(t, newBackend(t)) })
	t.Run("NotFound", func(t *testing.T) { testNotFound(t, newBackend(t)) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, newBackend(t)) })
	t.Run("Ranges", func(t *testing.T) { testRanges(t, newBackend(t)) })
	t.Run("InvalidKeys", func(t *testing.T) { testInvalidKeys(t, newBackend(t)) })
	t.Run("ListPrefix", func(t *testing.T) { testListPrefix(t, newBackend(t)) })
	t.Run("ListLarge", func(t *testing.T) { testListLarge(t, newBackend(t)) })
	t.Run("ConditionalCreate", func(t *testing.T) { testConditionalCreate(t, newBackend(t)) })
	t.Run("ConditionalCreateRace", func(t *testing.T) { testConditionalCreateRace(t, newBackend(t)) })
	t.Run("ConcurrentWriters", func(t *testing.T) { testConcurrentWriters(t, newBackend(t)) })
	t.Run("InterruptedUpload", func(t *testing.T) { testInterruptedUpload(t, newBackend(t)) })
	t.Run("ContextCanceled", func(t *testing.T) { testContextCanceled(t, newBackend(t)) })
	t.Run("Faults", func(t *testing.T) { testFaults(t, newBackend) })
}

func ctx(t *testing.T) context.Context { return t.Context() }

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.UintN(256)) // #nosec G115 -- below 256
	}
	return b
}

func newRNG(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed+1)) } // #nosec G404 -- test data

func put(t *testing.T, b backend.Backend, key string, data []byte, opts backend.PutOptions) error {
	t.Helper()
	return b.Put(ctx(t), key, bytes.NewReader(data), opts)
}

func mustPut(t *testing.T, b backend.Backend, key string, data []byte) {
	t.Helper()
	if err := put(t, b, key, data, backend.PutOptions{}); err != nil {
		t.Fatalf("Put(%s): %v", key, err)
	}
}

// read returns the object's content, or the error from Get or reading.
func read(t *testing.T, b backend.Backend, key string, rng *backend.Range) ([]byte, error) {
	t.Helper()
	rc, err := b.Get(ctx(t), key, rng)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func mustRead(t *testing.T, b backend.Backend, key string) []byte {
	t.Helper()
	data, err := read(t, b, key, nil)
	if err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	return data
}

func listKeys(t *testing.T, b backend.Backend, prefix string) []string {
	t.Helper()
	var keys []string
	if err := b.List(ctx(t), prefix, func(oi backend.ObjectInfo) error {
		keys = append(keys, oi.Key)
		return nil
	}); err != nil {
		t.Fatalf("List(%q): %v", prefix, err)
	}
	return keys
}

func testPutGetStat(t *testing.T, b backend.Backend) {
	rng := newRNG(1)
	for _, size := range []int{0, 1, 4096, 1<<20 + 7} {
		key := fmt.Sprintf("data/ab/obj-%d", size)
		want := randomBytes(rng, size)
		before := time.Now().Add(-time.Minute)
		mustPut(t, b, key, want)
		if got := mustRead(t, b, key); !bytes.Equal(got, want) {
			t.Errorf("Get(%s) returned %d bytes, want %d identical bytes", key, len(got), size)
		}
		oi, err := b.Stat(ctx(t), key)
		if err != nil {
			t.Fatalf("Stat(%s): %v", key, err)
		}
		if oi.Key != key || oi.Size != int64(size) || oi.ModTime.Before(before) || oi.ModTime.After(time.Now().Add(time.Minute)) {
			t.Errorf("Stat(%s) = %+v", key, oi)
		}
	}
}

func testOverwrite(t *testing.T, b backend.Backend) {
	mustPut(t, b, "x/key", []byte("first, longer content"))
	mustPut(t, b, "x/key", []byte("second"))
	if got := mustRead(t, b, "x/key"); string(got) != "second" {
		t.Errorf("after overwrite, Get = %q", got)
	}
	if keys := listKeys(t, b, "x/"); len(keys) != 1 {
		t.Errorf("overwrite left %v", keys)
	}
}

func testNotFound(t *testing.T, b backend.Backend) {
	if _, err := b.Get(ctx(t), "no/such/key", nil); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
	if _, err := b.Stat(ctx(t), "no/such/key"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Stat(missing) = %v, want ErrNotFound", err)
	}
	// A "directory" holding objects is not itself an object.
	mustPut(t, b, "dir/child", []byte("x"))
	if _, err := b.Stat(ctx(t), "dir"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Stat(prefix of a key) = %v, want ErrNotFound", err)
	}
	if _, err := b.Get(ctx(t), "dir", nil); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Get(prefix of a key) = %v, want ErrNotFound", err)
	}
}

func testDelete(t *testing.T, b backend.Backend) {
	mustPut(t, b, "d/one", []byte("1"))
	mustPut(t, b, "d/two", []byte("2"))
	if err := b.Delete(ctx(t), "d/one"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx(t), "d/one"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Stat after Delete = %v", err)
	}
	if err := b.Delete(ctx(t), "d/one"); err != nil {
		t.Errorf("Delete(missing) = %v, want nil", err)
	}
	if err := b.Delete(ctx(t), "never/existed"); err != nil {
		t.Errorf("Delete(never existed) = %v, want nil", err)
	}
	if keys := listKeys(t, b, "d/"); !slices.Equal(keys, []string{"d/two"}) {
		t.Errorf("after Delete, List = %v", keys)
	}
}

func testRanges(t *testing.T, b backend.Backend) {
	data := randomBytes(newRNG(2), 100)
	mustPut(t, b, "r/obj", data)
	tests := []struct {
		off, length int64
		want        []byte
		err         error
	}{
		{0, -1, data, nil},
		{0, 100, data, nil},
		{10, 20, data[10:30], nil},
		{90, -1, data[90:], nil},
		{90, 50, data[90:], nil},
		{99, 1, data[99:], nil},
		{100, -1, []byte{}, nil},
		{100, 10, []byte{}, nil},
		{5, 0, []byte{}, nil},
		{101, 1, nil, backend.ErrInvalidRange},
		{-1, 5, nil, backend.ErrInvalidRange},
		{0, -2, nil, backend.ErrInvalidRange},
	}
	for _, tt := range tests {
		got, err := read(t, b, "r/obj", &backend.Range{Offset: tt.off, Length: tt.length})
		if tt.err != nil {
			if !errors.Is(err, tt.err) {
				t.Errorf("range %d+%d: err = %v, want %v", tt.off, tt.length, err, tt.err)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, tt.want) {
			t.Errorf("range %d+%d = %d bytes, %v; want %d bytes", tt.off, tt.length, len(got), err, len(tt.want))
		}
	}
}

func testInvalidKeys(t *testing.T, b backend.Backend) {
	bad := []string{"", "/a", "a/", "a//b", "../a", "a/../b", "a/./b", ".hidden", "a/.tmp-x", `a\b`, "a b", "a:b", "a*b", "é"}
	for _, key := range bad {
		if err := put(t, b, key, []byte("x"), backend.PutOptions{}); !errors.Is(err, backend.ErrInvalidKey) {
			t.Errorf("Put(%q) = %v, want ErrInvalidKey", key, err)
		}
		if _, err := b.Get(ctx(t), key, nil); !errors.Is(err, backend.ErrInvalidKey) {
			t.Errorf("Get(%q) = %v, want ErrInvalidKey", key, err)
		}
		if _, err := b.Stat(ctx(t), key); !errors.Is(err, backend.ErrInvalidKey) {
			t.Errorf("Stat(%q) = %v, want ErrInvalidKey", key, err)
		}
		if err := b.Delete(ctx(t), key); !errors.Is(err, backend.ErrInvalidKey) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
	for _, prefix := range []string{"/", "../", "a//", ".x/", `a\`} {
		if err := b.List(ctx(t), prefix, func(backend.ObjectInfo) error { return nil }); !errors.Is(err, backend.ErrInvalidKey) {
			t.Errorf("List(%q) = %v, want ErrInvalidKey", prefix, err)
		}
	}
}

func testListPrefix(t *testing.T, b backend.Backend) {
	for _, k := range []string{"a/b", "a/bc", "ab", "a-c", "b/x/y", "a/b-sibling"} {
		mustPut(t, b, k, []byte(k))
	}
	tests := []struct {
		prefix string
		want   []string
	}{
		{"", []string{"a-c", "a/b", "a/b-sibling", "a/bc", "ab", "b/x/y"}},
		{"a", []string{"a-c", "a/b", "a/b-sibling", "a/bc", "ab"}},
		{"a/", []string{"a/b", "a/b-sibling", "a/bc"}},
		{"a/b", []string{"a/b", "a/b-sibling", "a/bc"}},
		{"a/bc", []string{"a/bc"}},
		{"b/x/", []string{"b/x/y"}},
		{"zzz", nil},
		{"zzz/", nil},
	}
	for _, tt := range tests {
		if got := listKeys(t, b, tt.prefix); !slices.Equal(got, tt.want) {
			t.Errorf("List(%q) = %v, want %v", tt.prefix, got, tt.want)
		}
	}
	var sizes []int64
	_ = b.List(ctx(t), "a/b", func(oi backend.ObjectInfo) error {
		sizes = append(sizes, oi.Size)
		return nil
	})
	if !slices.Equal(sizes, []int64{3, 11, 4}) {
		t.Errorf("List sizes = %v", sizes)
	}
	stop := errors.New("stop")
	calls := 0
	err := b.List(ctx(t), "", func(backend.ObjectInfo) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("List with fn error = %v after %d calls, want stop after 1", err, calls)
	}
}

func testListLarge(t *testing.T, b backend.Backend) {
	n := 2000
	if testing.Short() {
		n = 300
	}
	want := make([]string, n)
	for i := range n {
		want[i] = fmt.Sprintf("data/%02x/pack-%05d", i%256, i)
	}
	// Write in parallel: the listing, not the writing, is under test.
	var wg sync.WaitGroup
	errs := make(chan error, n)
	sem := make(chan struct{}, 8)
	for _, k := range want {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := b.Put(context.Background(), k, bytes.NewReader([]byte(k)), backend.PutOptions{}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Put: %v", err)
	}
	mustPut(t, b, "index/0001", []byte("other"))
	sort.Strings(want)
	if got := listKeys(t, b, "data/"); !slices.Equal(got, want) {
		t.Fatalf("List(data/) returned %d keys, want %d in order", len(got), len(want))
	}
	var want0a []string
	for _, k := range want {
		if strings.HasPrefix(k, "data/0a/") {
			want0a = append(want0a, k)
		}
	}
	if got := listKeys(t, b, "data/0a/"); !slices.Equal(got, want0a) {
		t.Errorf("List(data/0a/) = %d keys, want %d", len(got), len(want0a))
	}
}

func testConditionalCreate(t *testing.T, b backend.Backend) {
	if !b.Capabilities().ConditionalCreate {
		t.Skip("backend has no conditional create")
	}
	// The same create twice, as init and doctor test it.
	if err := put(t, b, "locks/x", []byte("first"), backend.PutOptions{IfNotExists: true}); err != nil {
		t.Fatalf("first conditional create: %v", err)
	}
	if err := put(t, b, "locks/x", []byte("second"), backend.PutOptions{IfNotExists: true}); !errors.Is(err, backend.ErrExists) {
		t.Fatalf("second conditional create = %v, want ErrExists", err)
	}
	if got := mustRead(t, b, "locks/x"); string(got) != "first" {
		t.Errorf("content after a failed conditional create = %q", got)
	}
	if keys := listKeys(t, b, "locks/"); !slices.Equal(keys, []string{"locks/x"}) {
		t.Errorf("failed conditional create left %v", keys)
	}
	if err := b.Delete(ctx(t), "locks/x"); err != nil {
		t.Fatal(err)
	}
	if err := put(t, b, "locks/x", []byte("third"), backend.PutOptions{IfNotExists: true}); err != nil {
		t.Errorf("conditional create after delete: %v", err)
	}
}

func testConditionalCreateRace(t *testing.T, b backend.Backend) {
	if !b.Capabilities().ConditionalCreate {
		t.Skip("backend has no conditional create")
	}
	const n = 16
	for round := range 5 {
		key := fmt.Sprintf("locks/race-%d", round)
		var wg sync.WaitGroup
		results := make([]error, n)
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = b.Put(context.Background(), key, bytes.NewReader([]byte(fmt.Sprint(i))), backend.PutOptions{IfNotExists: true})
			}()
		}
		close(start)
		wg.Wait()
		winners := -1
		for i, err := range results {
			switch {
			case err == nil:
				if winners >= 0 {
					t.Fatalf("round %d: writers %d and %d both created %s", round, winners, i, key)
				}
				winners = i
			case !errors.Is(err, backend.ErrExists):
				t.Fatalf("round %d: writer %d: %v", round, i, err)
			}
		}
		if winners < 0 {
			t.Fatalf("round %d: no writer created %s", round, key)
		}
		if got := mustRead(t, b, key); string(got) != fmt.Sprint(winners) {
			t.Errorf("round %d: content %q, winner %d", round, got, winners)
		}
	}
}

func testConcurrentWriters(t *testing.T, b backend.Backend) {
	const n = 16
	values := make([][]byte, n)
	for i := range values {
		values[i] = bytes.Repeat([]byte{byte('a' + i)}, 64<<10)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2*n)
	for i := range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[2*i] = b.Put(context.Background(), "shared/obj", bytes.NewReader(values[i]), backend.PutOptions{})
		}()
		go func() {
			defer wg.Done()
			errs[2*i+1] = b.Put(context.Background(), fmt.Sprintf("own/%02d", i), bytes.NewReader(values[i]), backend.PutOptions{})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	got := mustRead(t, b, "shared/obj")
	if !slices.ContainsFunc(values, func(v []byte) bool { return bytes.Equal(v, got) }) {
		t.Errorf("shared object is a mix of writes (%d bytes, starts %q)", len(got), got[:1])
	}
	for i := range n {
		if got := mustRead(t, b, fmt.Sprintf("own/%02d", i)); !bytes.Equal(got, values[i]) {
			t.Errorf("own/%02d corrupted", i)
		}
	}
	if keys := listKeys(t, b, ""); len(keys) != n+1 {
		t.Errorf("concurrent writes left %d objects, want %d: %v", len(keys), n+1, keys)
	}
}

// failingReader returns data, then fails.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func testInterruptedUpload(t *testing.T, b backend.Backend) {
	cut := errors.New("connection reset")
	err := b.Put(ctx(t), "u/new", &failingReader{data: bytes.Repeat([]byte("x"), 100<<10), err: cut}, backend.PutOptions{})
	if !errors.Is(err, cut) {
		t.Errorf("interrupted Put = %v, want the reader's error", err)
	}
	if _, err := b.Stat(ctx(t), "u/new"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("interrupted Put left an object: Stat = %v", err)
	}

	mustPut(t, b, "u/old", []byte("original"))
	_ = b.Put(ctx(t), "u/old", &failingReader{data: []byte("partial"), err: cut}, backend.PutOptions{})
	if got := mustRead(t, b, "u/old"); string(got) != "original" {
		t.Errorf("interrupted overwrite changed the object to %q", got)
	}

	c, cancel := context.WithCancel(ctx(t))
	r := &cancelReader{data: bytes.Repeat([]byte("y"), 1<<20), cancel: cancel}
	if err := b.Put(c, "u/cancel", r, backend.PutOptions{IfNotExists: true}); err == nil {
		t.Error("Put with a context cancelled mid-upload succeeded")
	}
	if _, err := b.Stat(ctx(t), "u/cancel"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("cancelled Put left an object: Stat = %v", err)
	}
	if keys := listKeys(t, b, "u/"); !slices.Equal(keys, []string{"u/old"}) {
		t.Errorf("interrupted uploads left %v", keys)
	}
}

// cancelReader cancels its context after the first read.
type cancelReader struct {
	data   []byte
	cancel context.CancelFunc
	reads  int
}

func (c *cancelReader) Read(p []byte) (int, error) {
	c.reads++
	if c.reads == 2 {
		c.cancel()
	}
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 4096)], c.data)
	c.data = c.data[n:]
	return n, nil
}

func testContextCanceled(t *testing.T, b backend.Backend) {
	mustPut(t, b, "c/obj", []byte("x"))
	c, cancel := context.WithCancel(ctx(t))
	cancel()
	checks := map[string]error{
		"Put":    b.Put(c, "c/new", bytes.NewReader([]byte("x")), backend.PutOptions{}),
		"Delete": b.Delete(c, "c/obj"),
		"List":   b.List(c, "", func(backend.ObjectInfo) error { return nil }),
	}
	_, checks["Get"] = b.Get(c, "c/obj", nil)
	_, checks["Stat"] = b.Stat(c, "c/obj")
	for op, err := range checks {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a cancelled context = %v, want context.Canceled", op, err)
		}
	}
	if got := mustRead(t, b, "c/obj"); string(got) != "x" {
		t.Error("cancelled Delete removed the object")
	}
}
