// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/backendtest"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/s3/s3test"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"))
}

// testOptions point a backend at the fake, with small blocks and quick
// retries so the tests exercise read-ahead, paging and backoff.
func testOptions(url string) Options {
	return Options{
		Bucket: "pentym", Prefix: "repo", Region: "us-east-1", Endpoint: url, PathStyle: true,
		AccessKeyID: "test", SecretAccessKey: secrets.New([]byte("test-secret")),
		PartSize: minPartSize, Concurrency: 4, ReadBlock: 64 << 10, ReadAhead: 3,
		MaxAttempts: 4, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
	}
}

func newFake(t *testing.T, mode string, edit func(*Options)) (*Backend, *s3test.Server) {
	t.Helper()
	f, url := s3test.New(t)
	f.CCMode = mode
	f.PageSize = 100
	o := testOptions(url)
	if edit != nil {
		edit(&o)
	}
	b, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return b, f
}

func TestConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend { b, _ := newFake(t, "", nil); return b })
}

// TestConformanceNoConditionalCreate runs the suite on storage that ignores
// If-None-Match, with the fallback configured: check, then put.
func TestConformanceNoConditionalCreate(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		b, _ := newFake(t, "ignore", func(o *Options) { o.NoConditionalCreate = true })
		if b.Capabilities().ConditionalCreate {
			t.Fatal("NoConditionalCreate reports conditional create")
		}
		return b
	})
}

func put(t *testing.T, b *Backend, key string, data []byte, ifNotExists bool) error {
	t.Helper()
	return b.Put(context.Background(), key, bytes.NewReader(data), backend.PutOptions{IfNotExists: ifNotExists})
}

func get(t *testing.T, b *Backend, key string, rng *backend.Range) []byte {
	t.Helper()
	rc, err := b.Get(context.Background(), key, rng)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRefusedConditionalCreate: storage that answers If-None-Match with 501
// switches the backend to the fallback, for single and multipart puts.
func TestRefusedConditionalCreate(t *testing.T) {
	b, f := newFake(t, "501", nil)
	if !b.Capabilities().ConditionalCreate {
		t.Fatal("conditional create is off before any refusal")
	}
	if err := put(t, b, "locks/a", []byte("1"), true); err != nil {
		t.Fatal(err)
	}
	if b.Capabilities().ConditionalCreate {
		t.Error("conditional create still reported after a 501")
	}
	if err := put(t, b, "locks/a", []byte("2"), true); !errors.Is(err, backend.ErrExists) {
		t.Errorf("second create = %v", err)
	}
	if got := get(t, b, "locks/a", nil); string(got) != "1" {
		t.Errorf("the object is %q", got)
	}

	// A multipart upload refused at completion is completed after the
	// check, without losing the uploaded parts.
	b2, _ := newFake(t, "501", nil)
	big := bytes.Repeat([]byte("p"), minPartSize+10)
	if err := put(t, b2, "data/big", big, true); err != nil {
		t.Fatal(err)
	}
	if got := get(t, b2, "data/big", nil); !bytes.Equal(got, big) {
		t.Errorf("multipart object is %d bytes", len(got))
	}
	if f.Count("complete-multipart") != 0 {
		t.Errorf("the first fake saw multipart uploads")
	}
}

func TestMultipart(t *testing.T) {
	b, f := newFake(t, "", nil)
	data := make([]byte, 2*minPartSize+12345)
	rng := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- test data
	for i := range data {
		data[i] = byte(rng.UintN(256)) // #nosec G115 -- below 256
	}
	if err := put(t, b, "data/ab/pack", data, true); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("upload-part"); n != 3 {
		t.Errorf("%d parts uploaded, want 3", n)
	}
	if got := get(t, b, "data/ab/pack", nil); !bytes.Equal(got, data) {
		t.Fatal("the multipart object reads back different")
	}
	if got := get(t, b, "data/ab/pack", &backend.Range{Offset: minPartSize - 7, Length: 300000}); !bytes.Equal(got, data[minPartSize-7:minPartSize-7+300000]) {
		t.Error("a range across blocks reads back different")
	}
	// Exactly the part size is one PUT.
	if err := put(t, b, "data/ab/exact", data[:minPartSize], false); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("upload-part"); n != 3 {
		t.Errorf("an object of exactly the part size was uploaded in parts (%d parts in total)", n)
	}
	// A second conditional create of a multipart object fails and leaves
	// no upload behind.
	if err := put(t, b, "data/ab/pack", data, true); !errors.Is(err, backend.ErrExists) {
		t.Errorf("second create = %v", err)
	}
	if f.Uploads() != 0 {
		t.Errorf("%d multipart uploads left", f.Uploads())
	}
	// A reader that fails in the second part aborts the upload.
	cut := errors.New("connection reset")
	r := io.MultiReader(bytes.NewReader(data[:minPartSize+100]), &errReader{cut})
	if err := b.Put(context.Background(), "data/ab/cut", r, backend.PutOptions{}); !errors.Is(err, cut) {
		t.Errorf("interrupted multipart = %v", err)
	}
	if _, err := b.Stat(context.Background(), "data/ab/cut"); !errors.Is(err, backend.ErrNotFound) || f.Uploads() != 0 {
		t.Errorf("an interrupted multipart left an object (%v) or %d uploads", err, f.Uploads())
	}
}

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

// TestRetries: throttling and 5xx are retried with backoff, up to the
// attempt limit.
func TestRetries(t *testing.T) {
	b, f := newFake(t, "", nil)
	var failures atomic.Int32
	failures.Store(2)
	f.Fault = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && failures.Add(-1) >= 0 {
			s3test.Error(w, 503, "SlowDown")
			return true
		}
		return false
	}
	if err := put(t, b, "data/x", []byte("x"), true); err != nil {
		t.Fatalf("after two throttled attempts: %v", err)
	}
	if n := f.Count(http.MethodPut); n != 3 {
		t.Errorf("%d PUTs, want 3", n)
	}
	failures.Store(100)
	if err := put(t, b, "data/y", []byte("y"), false); err == nil || !strings.Contains(err.Error(), "SlowDown") {
		t.Errorf("throttled every time = %v", err)
	}
	if n := f.Count(http.MethodPut); n != 3+4 {
		t.Errorf("%d PUTs in total, want 7 (the attempt limit is 4)", n)
	}
}

// TestLostResponse: a conditional PUT that succeeded but whose response
// was lost is retried, meets its own object, and still succeeds; a
// different object at the key is still ErrExists.
func TestLostResponse(t *testing.T) {
	b, f := newFake(t, "", nil)
	var lost atomic.Bool
	f.Fault = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && lost.CompareAndSwap(false, true) {
			f.ServeHTTP(httptest.NewRecorder(), r) // the write happens
			s3test.Error(w, http.StatusInternalServerError, "InternalError")
			return true
		}
		return false
	}
	if err := put(t, b, "locks/l", []byte("holder-1"), true); err != nil {
		t.Fatalf("a create whose response was lost = %v", err)
	}
	if err := put(t, b, "locks/l", []byte("holder-2"), true); !errors.Is(err, backend.ErrExists) {
		t.Errorf("another holder's create = %v", err)
	}
}

// TestBrokenBody: a GET whose body breaks off is fetched again.
func TestBrokenBody(t *testing.T) {
	b, f := newFake(t, "", nil)
	data := bytes.Repeat([]byte("0123456789"), 30000)
	if err := put(t, b, "data/b", data, false); err != nil {
		t.Fatal(err)
	}
	var broken atomic.Bool
	f.Fault = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Range"), "bytes=65536-") && broken.CompareAndSwap(false, true) {
			w.Header().Set("Content-Length", "65536")
			w.Header().Set("Content-Range", "bytes 65536-131071/300000")
			w.WriteHeader(206)
			_, _ = w.Write(make([]byte, 1000))
			panic(http.ErrAbortHandler)
		}
		return false
	}
	if got := get(t, b, "data/b", nil); !bytes.Equal(got, data) {
		t.Error("the object reads back different after a broken body")
	}
	if !broken.Load() {
		t.Error("the fault never fired")
	}
}

// TestObjectChangedDuringRead: blocks after the first must come from the
// same version of the object.
func TestObjectChangedDuringRead(t *testing.T) {
	b, _ := newFake(t, "", func(o *Options) { o.ReadAhead = 1 })
	if err := put(t, b, "data/c", bytes.Repeat([]byte("a"), 400000), false); err != nil {
		t.Fatal(err)
	}
	rc, err := b.Get(context.Background(), "data/c", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if err := put(t, b, "data/c", bytes.Repeat([]byte("b"), 400000), false); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Errorf("reading a changed object = %v", err)
	}
}

// TestConcurrencyBound: no more requests are in flight than allowed, even
// with many readers reading ahead.
func TestConcurrencyBound(t *testing.T) {
	b, f := newFake(t, "", nil)
	data := bytes.Repeat([]byte("z"), 1<<20)
	if err := put(t, b, "data/d", data, false); err != nil {
		t.Fatal(err)
	}
	var inFlight, peak atomic.Int32
	f.Fault = func(http.ResponseWriter, *http.Request) bool {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		return false
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if got := get(t, b, "data/d", nil); !bytes.Equal(got, data) {
				t.Error("a concurrent read differs")
			}
		})
	}
	wg.Wait()
	if p := peak.Load(); p > 4 || p < 2 {
		t.Errorf("peak requests in flight = %d, want 2..4", p)
	}
}

// TestCloseEarly: closing a reader part-way stops its reads ahead.
func TestCloseEarly(t *testing.T) {
	b, _ := newFake(t, "", nil)
	if err := put(t, b, "data/e", bytes.Repeat([]byte("e"), 2<<20), false); err != nil {
		t.Fatal(err)
	}
	rc, err := b.Get(context.Background(), "data/e", nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := rc.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBackoff(t *testing.T) {
	j := jitterBackoff{min: time.Second, max: 5 * time.Minute}
	for attempt := 1; attempt <= 30; attempt++ {
		for range 50 {
			d, _ := j.BackoffDelay(attempt, nil)
			upper := min(time.Second<<attempt, 5*time.Minute)
			if attempt >= 9 {
				upper = 5 * time.Minute
			}
			if d < time.Second || d > upper || d < upper/2 {
				t.Fatalf("attempt %d: delay %v outside [%v, %v]", attempt, d, upper/2, upper)
			}
		}
	}
}

func TestNewInvalid(t *testing.T) {
	for _, o := range []Options{
		{Region: "us-east-1"},
		{Bucket: "b", Region: "us-east-1", PartSize: 1 << 20},
		{Bucket: "b", Region: "us-east-1", Prefix: "../up"},
	} {
		if _, err := New(context.Background(), o); err == nil {
			t.Errorf("New(%+v) succeeded", o)
		}
	}
	b, err := New(context.Background(), Options{Bucket: "b", Region: "us-east-1", Prefix: "/team/prod/", AccessKeyID: "k"})
	if err != nil || b.root != "team/prod/" {
		t.Errorf("prefix: %q, %v", b.root, err)
	}
}
