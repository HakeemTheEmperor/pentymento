// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Cache is a disposable local copy of immutable repository objects (index
// files and snapshots), to save GET requests (PRD 7.8, SDD 5.5). Objects
// are kept exactly as stored in the repository, still encrypted, so no
// plaintext reaches the disk; a damaged entry is detected (by its hash or
// by authentication) and fetched again. Deleting the cache is always safe.
type Cache struct {
	dir string
}

// DefaultCacheDir returns the cache directory for a repository: the user's
// cache directory, then pentym/<repository ID>.
func DefaultCacheDir(repoID string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "pentym", repoID), nil
}

// NewCache returns a cache in dir, created on first use.
func NewCache(dir string) *Cache { return &Cache{dir: dir} }

// SetCache makes the repository use c for index files and snapshots; nil
// turns caching off.
func (r *Repository) SetCache(c *Cache) { r.cache = c }

func (c *Cache) path(key string) string { return filepath.Join(c.dir, filepath.FromSlash(key)) }

// get returns a cached object.
func (c *Cache) get(key string) ([]byte, bool) {
	b, err := os.ReadFile(c.path(key)) // #nosec G304 -- keys are validated repository keys under the cache directory
	return b, err == nil
}

// put stores an object; failures are ignored, since the cache is only an
// optimization.
func (c *Cache) put(key string, data []byte) {
	p := c.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), p) != nil {
		_ = os.Remove(tmp.Name())
	}
}

func (c *Cache) remove(key string) { _ = os.Remove(c.path(key)) }

// retain deletes cached objects under prefix that are not in keep, such as
// index files replaced by a prune.
func (c *Cache) retain(prefix string, keep []string) {
	want := make(map[string]bool, len(keep))
	for _, k := range keep {
		want[k] = true
	}
	root := c.path(prefix)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(c.dir, p)
		if err != nil {
			return nil
		}
		if key := filepath.ToSlash(rel); strings.HasPrefix(key, prefix) && !want[key] {
			_ = os.Remove(p) // #nosec G122 -- the cache directory belongs to this user
		}
		return nil
	})
}

// cachedRead returns an object, from the cache when it holds a copy that
// valid accepts, otherwise from the repository (refreshing the cache).
func (r *Repository) cachedRead(ctx context.Context, key string, limit int64, valid func([]byte) bool) ([]byte, error) {
	if r.cache != nil {
		if b, ok := r.cache.get(key); ok {
			if valid(b) {
				return b, nil
			}
			r.cache.remove(key)
		}
	}
	b, err := readAll(ctx, r.be, key, limit)
	if err != nil {
		return nil, err
	}
	if r.cache != nil && valid(b) {
		r.cache.put(key, b)
	}
	return b, nil
}
