// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

func TestWithRetry(t *testing.T) {
	var waits []time.Duration
	saved := retrySleep
	retrySleep = func(d time.Duration) { waits = append(waits, d) }
	t.Cleanup(func() { retrySleep = saved })

	calls := 0
	err := withRetry(func() error {
		calls++
		if calls < 4 {
			return &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
		}
		return nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("withRetry = %v after %d calls", err, calls)
	}
	if len(waits) != 3 || waits[0] != 10*time.Millisecond || waits[2] != 40*time.Millisecond {
		t.Errorf("waits = %v", waits)
	}

	waits, calls = nil, 0
	err = withRetry(func() error {
		calls++
		return windows.ERROR_ACCESS_DENIED
	})
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || calls != retryAttempts {
		t.Errorf("persistent failure: %v after %d calls", err, calls)
	}
	if waits[len(waits)-1] != time.Second {
		t.Errorf("waits do not cap at 1s: %v", waits)
	}

	calls = 0
	other := errors.New("disk full")
	if err := withRetry(func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
		t.Errorf("non-retryable error retried %d times", calls)
	}
}

// TestRenameRetriesWhileLocked holds the target open, as antivirus does,
// and checks the overwrite succeeds once the handle is released.
func TestRenameRetriesWhileLocked(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Put(ctx, "data/ab/pack", strings.NewReader("old"), backend.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.Root(), "data", "ab", "pack")
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Open without FILE_SHARE_DELETE, which blocks replacing the file.
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = windows.CloseHandle(h)
		close(released)
	}()
	if err := b.Put(ctx, "data/ab/pack", strings.NewReader("new"), backend.PutOptions{}); err != nil {
		t.Fatalf("Put while locked: %v", err)
	}
	<-released
	if got, _ := os.ReadFile(path); string(got) != "new" { // #nosec G304 -- test file
		t.Errorf("content = %q", got)
	}
}

// TestReplaceWhileReading: an object being read through Get can be
// replaced at once, as a lease holder renews the record a standby reads;
// the reader keeps what it opened.
func TestReplaceWhileReading(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Put(ctx, "locks/lease", strings.NewReader("old"), backend.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	rc, err := b.Get(ctx, "locks/lease", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	saved := retrySleep
	var waits int
	retrySleep = func(time.Duration) { waits++ }
	t.Cleanup(func() { retrySleep = saved })
	if err := b.Put(ctx, "locks/lease", strings.NewReader("new"), backend.PutOptions{}); err != nil {
		t.Fatalf("Put while a reader has the object open: %v", err)
	}
	if waits != 0 {
		t.Errorf("the replace waited %d times for the reader", waits)
	}
	buf := make([]byte, 8)
	n, _ := rc.Read(buf)
	if string(buf[:n]) != "old" {
		t.Errorf("the reader sees %q", buf[:n])
	}
	if got, _ := os.ReadFile(filepath.Join(b.Root(), "locks", "lease")); string(got) != "new" { // #nosec G304 -- test file
		t.Errorf("content = %q", got)
	}
}
