// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package local

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/backendtest"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend { return newTestBackend(t) })
}

// TestConformanceLinkFallback runs the suite with conditional create done
// by the hard-link fallback, as on filesystems without an exclusive rename.
func TestConformanceLinkFallback(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		b := newTestBackend(t)
		b.noReplace = linkNoReplace
		return b
	})
}

func TestLayout(t *testing.T) {
	b := newTestBackend(t)
	if err := b.Put(context.Background(), "data/ab/pack-1", strings.NewReader("x"), backend.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(b.Root(), "data", "ab", "pack-1"))
	if err != nil {
		t.Fatalf("object not stored at data/ab/pack-1: %v", err)
	}
	if fi.Size() != 1 {
		t.Errorf("size = %d", fi.Size())
	}
}

func TestNewRejectsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f); err == nil {
		t.Error("New accepted a regular file as the root")
	}
}

// TestTempFiles checks that temporary files are never shown as objects and
// that CleanTemp removes only stale ones.
func TestTempFiles(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	if err := b.Put(ctx, "data/ab/pack", strings.NewReader("x"), backend.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(b.Root(), "data", "ab")
	stale := filepath.Join(dir, tempPrefix+"stale")
	fresh := filepath.Join(dir, tempPrefix+"fresh")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// A stray file with an invalid key name is not an object either.
	if err := os.WriteFile(filepath.Join(dir, "not a key"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var keys []string
	if err := b.List(ctx, "", func(oi backend.ObjectInfo) error {
		keys = append(keys, oi.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, []string{"data/ab/pack"}) {
		t.Errorf("List = %v", keys)
	}
	if _, err := b.Stat(ctx, "data/ab/.tmp-stale"); !errors.Is(err, backend.ErrInvalidKey) {
		t.Errorf("Stat(temp file) = %v, want ErrInvalidKey", err)
	}

	n, err := b.CleanTemp(ctx, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("CleanTemp = %d, %v; want 1", n, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale temp file not removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh temp file removed")
	}
}

// TestFailedPutLeavesNoTemp checks that a failed upload removes its
// temporary file.
func TestFailedPutLeavesNoTemp(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	err := b.Put(ctx, "data/ab/pack", &errReader{}, backend.PutOptions{})
	if err == nil {
		t.Fatal("Put with a failing reader succeeded")
	}
	if err := b.Put(ctx, "locks/l", strings.NewReader("a"), backend.PutOptions{IfNotExists: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "locks/l", strings.NewReader("b"), backend.PutOptions{IfNotExists: true}); !errors.Is(err, backend.ErrExists) {
		t.Fatalf("second create = %v", err)
	}
	var temps []string
	_ = filepath.WalkDir(b.Root(), func(p string, d os.DirEntry, _ error) error {
		if strings.HasPrefix(d.Name(), tempPrefix) {
			temps = append(temps, p)
		}
		return nil
	})
	if len(temps) != 0 {
		t.Errorf("temp files left behind: %v", temps)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestFilesystem(t *testing.T) {
	b := newTestBackend(t)
	info, err := b.Filesystem()
	if err != nil {
		t.Fatal(err)
	}
	if info.Network {
		t.Errorf("temporary directory reported as a network filesystem: %+v", info)
	}
	t.Logf("temporary directory filesystem: %+v", info)
}

func TestCapabilities(t *testing.T) {
	c := newTestBackend(t).Capabilities()
	if !c.ConditionalCreate || c.ObjectLock || !c.StrongConsistency {
		t.Errorf("Capabilities = %+v", c)
	}
}

func TestLinkNoReplace(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkNoReplace(a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); !errors.Is(err, os.ErrNotExist) {
		t.Error("source not removed after link")
	}
	if err := os.WriteFile(a, []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkNoReplace(a, b); !errors.Is(err, backend.ErrExists) {
		t.Errorf("link over an existing file = %v", err)
	}
	if got, _ := os.ReadFile(b); !bytes.Equal(got, []byte("1")) { // #nosec G304 -- test file
		t.Errorf("target changed to %q", got)
	}
}
