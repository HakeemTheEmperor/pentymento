// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// tempPrefix starts the name of every file being written. Such files are
// never listed or read as objects; CleanTemp removes stale ones.
const tempPrefix = ".tmp-"

// Permissions for repository files and directories: owner only.
const (
	dirPerm = 0o700
)

// Backend stores objects as files under a root directory. Writes go to a
// temporary file that is synced and then renamed into place, and the
// directory is synced (except on Windows, where that is not possible), so a
// crash never leaves a partial object.
type Backend struct {
	root string
	// noReplace publishes a temp file only if the target does not exist;
	// tests replace it to exercise the link fallback.
	noReplace func(oldpath, newpath string) error
}

var _ backend.Backend = (*Backend)(nil)

// New returns a backend rooted at dir, creating it if needed.
func New(dir string) (*Backend, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", abs)
	}
	return &Backend{root: abs, noReplace: renameNoReplace}, nil
}

// Root returns the repository directory.
func (b *Backend) Root() string { return b.root }

// Capabilities reports conditional create and strong consistency, and no
// object lock.
func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{ConditionalCreate: true, StrongConsistency: true}
}

func (b *Backend) path(key string) string {
	return filepath.Join(b.root, filepath.FromSlash(key))
}

// Put writes r to a temporary file beside the target, syncs it, and renames
// it into place; with IfNotExists the rename fails if the target exists.
func (b *Backend) Put(ctx context.Context, key string, r io.Reader, opts backend.PutOptions) (err error) {
	if err := backend.ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	target := b.path(key)
	dir := filepath.Dir(target)
	if err := b.mkdirAll(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = io.Copy(tmp, ctxReader{ctx: ctx, r: r}); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if opts.IfNotExists {
		err = b.noReplace(tmpName, target)
	} else {
		err = withRetry(func() error { return os.Rename(tmpName, target) })
	}
	if err != nil {
		return err
	}
	return syncDir(dir)
}

// mkdirAll creates dir and any missing parents, syncing each new
// directory's parent so the new entries survive a crash.
func (b *Backend) mkdirAll(dir string) error {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir && len(parent) >= len(b.root) {
		if err := b.mkdirAll(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncDir(parent)
}

// Get opens the object, or the part of it in rng.
func (b *Backend) Get(ctx context.Context, key string, rng *backend.Range) (io.ReadCloser, error) {
	if err := backend.ValidateKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(b.path(key))
	if err != nil {
		return nil, mapNotFound(key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if fi.IsDir() {
		_ = f.Close()
		return nil, notFound(key)
	}
	off, n, err := backend.CheckRange(rng, fi.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return readCloser{Reader: io.LimitReader(f, n), Closer: f}, nil
}

// Stat returns the object's size and modification time.
func (b *Backend) Stat(ctx context.Context, key string) (backend.ObjectInfo, error) {
	if err := backend.ValidateKey(key); err != nil {
		return backend.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return backend.ObjectInfo{}, err
	}
	fi, err := os.Stat(b.path(key))
	if err != nil {
		return backend.ObjectInfo{}, mapNotFound(key, err)
	}
	if fi.IsDir() {
		return backend.ObjectInfo{}, notFound(key)
	}
	return backend.ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// List walks the directory holding the prefix and reports matching
// objects in key order. Temporary files and anything that is not a valid
// key are skipped.
func (b *Backend) List(ctx context.Context, prefix string, fn func(backend.ObjectInfo) error) error {
	if err := backend.ValidatePrefix(prefix); err != nil {
		return err
	}
	base := b.root
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		base = b.path(prefix[:i])
	}
	var found []backend.ObjectInfo
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == base {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(b.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) || backend.ValidateKey(key) != nil {
			return nil
		}
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil // deleted while listing
		}
		if err != nil {
			return err
		}
		found = append(found, backend.ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Key < found[j].Key })
	for _, oi := range found {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(oi); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the object; a missing key succeeds.
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := backend.ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p := b.path(key)
	if fi, err := os.Lstat(p); err != nil || fi.IsDir() {
		return nil
	}
	err := withRetry(func() error { return os.Remove(p) })
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(p))
}

// CleanTemp removes temporary files left by interrupted writes that are
// older than olderThan, and returns how many it removed.
func (b *Backend) CleanTemp(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	// Removals go through an os.Root, so a directory swapped for a symlink
	// during the walk cannot lead outside the repository.
	root, err := os.OpenRoot(b.root)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	err = filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), tempPrefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			return nil
		}
		rel, err := filepath.Rel(b.root, p)
		if err != nil {
			return nil
		}
		if err := root.Remove(rel); err == nil {
			removed++
		}
		return nil
	})
	return removed, err
}

func notFound(key string) error { return fmt.Errorf("%w: %s", backend.ErrNotFound, key) }

func mapNotFound(key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return notFound(key)
	}
	return err
}

// linkNoReplace publishes oldpath at newpath with a hard link, which fails
// if newpath exists, then removes oldpath. It is the fallback where an
// exclusive rename is not supported.
func linkNoReplace(oldpath, newpath string) error {
	if err := os.Link(oldpath, newpath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", backend.ErrExists, newpath)
		}
		return err
	}
	return os.Remove(oldpath)
}

// ctxReader stops a copy when the context is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type readCloser struct {
	io.Reader
	io.Closer
}
