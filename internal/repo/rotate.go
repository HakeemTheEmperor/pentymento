// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/compress"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
)

// RotateStats describes a key rotation.
type RotateStats struct {
	// PreviousKey is the object that now holds the replaced private key.
	PreviousKey string `json:"previous_key"`
	// IndexFiles is how many index files were rewritten, and Packs how many
	// packs' data keys they re-wrapped.
	IndexFiles int `json:"index_files"`
	Packs      int `json:"packs"`
	// Segments is how many log segments were re-wrapped.
	Segments int `json:"segments"`
}

// RotateKeys generates a new key pair and re-wraps every data key in the
// index and every log segment's key to it (key rotate, SDD 8.4). Packs are
// never rewritten. The caller holds the exclusive lock; u is the full
// slot's keys the repository was unlocked with.
//
// The order makes an interruption safe, and running it again finishes the
// work:
//
//  1. the current private key is kept as keys/private-<n>;
//  2. the new private key replaces keys/private, then the new public key
//     replaces keys/public, so nothing is ever wrapped to a key that is
//     not stored;
//  3. index files are rewritten with re-wrapped keys, each before the old
//     file is deleted;
//  4. log segments are re-wrapped in place.
//
// Data keys still wrapped to a previous key stay readable through the
// keyring until then, and pack trailers keep them for an index rebuild.
// Rotation protects data written after it: whoever holds a previous
// private key can still read the data it wrapped.
func (r *Repository) RotateKeys(ctx context.Context, u crypto.Unlocked) (*RotateStats, error) {
	if r.cfg.Insecure() {
		return nil, errors.New("an unencrypted repository has no keys to rotate")
	}
	if !r.CanRead() || u.Scope != crypto.ScopeFull {
		return nil, ErrNoPrivateKey
	}
	current, err := readAll(ctx, r.be, privateKeyObject, 4096)
	if err != nil {
		return nil, fmt.Errorf("read the private key: %w", err)
	}
	n, err := r.nextPreviousKey(ctx)
	if err != nil {
		return nil, err
	}
	stats := &RotateStats{PreviousKey: previousKeyKey(n)}
	if err := r.be.Put(ctx, stats.PreviousKey, bytes.NewReader(current), backend.PutOptions{IfNotExists: true}); err != nil {
		return nil, fmt.Errorf("keep the current private key: %w", err)
	}

	rot, err := crypto.Rotate(&crypto.Material{Master: u.Master, Metadata: u.Metadata, ChunkID: u.ChunkID, Private: r.keys.Current()})
	if err != nil {
		return nil, err
	}
	if err := r.be.Put(ctx, privateKeyObject, bytes.NewReader(rot.PrivateObject), backend.PutOptions{}); err != nil {
		return nil, fmt.Errorf("store the new private key: %w", err)
	}
	if err := r.be.Put(ctx, publicKeyObject, bytes.NewReader(rot.PublicObject), backend.PutOptions{}); err != nil {
		return nil, fmt.Errorf("store the new public key: %w", err)
	}
	r.keys = append(crypto.Keyring{rot.Private}, r.keys...)
	r.public = rot.Private.Public()

	if stats.IndexFiles, stats.Packs, err = r.rewrapIndex(ctx); err != nil {
		return stats, err
	}
	stats.Segments, err = r.rewrapSegments(ctx)
	return stats, err
}

// nextPreviousKey returns the number for the next replaced private key.
func (r *Repository) nextPreviousKey(ctx context.Context) (int, error) {
	keys, err := r.listKeys(ctx, previousKeysPrefix)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range keys {
		if v, err := strconv.Atoi(strings.TrimPrefix(k, previousKeysPrefix)); err == nil && v > n {
			n = v
		}
	}
	return n + 1, nil
}

// rewrapIndex rewrites every index file holding a data key that is not
// wrapped to the current public key.
func (r *Repository) rewrapIndex(ctx context.Context) (files, packs int, err error) {
	c, err := compress.New(compress.Default, indexMaxBytes)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = c.Close() }()
	err = r.eachIndexFile(ctx, func(key string, records []packRecord, err error) error {
		if err != nil {
			return err
		}
		changed := 0
		for i, p := range records {
			if len(p.wrappedKey) == 0 {
				continue
			}
			w, did, err := r.keys.Rewrap(r.public, p.wrappedKey)
			if err != nil {
				return fmt.Errorf("%s: pack %s: data key: %w: %w", key, p.id, ErrCorrupt, err)
			}
			if did {
				records[i].wrappedKey = w
				changed++
			}
		}
		if changed == 0 {
			return nil
		}
		newKey, err := r.writeIndex(ctx, c, records)
		if err != nil {
			return err
		}
		if newKey != key {
			if err := r.be.Delete(ctx, key); err != nil && !errors.Is(err, backend.ErrNotFound) {
				return err
			}
			if r.cache != nil {
				r.cache.remove(key)
			}
		}
		files++
		packs += changed
		return nil
	})
	return files, packs, err
}

// rewrapSegments re-wraps the data key of every log segment, timeline
// history file and oplog batch not wrapped to the current public key.
func (r *Repository) rewrapSegments(ctx context.Context) (int, error) {
	n := 0
	for _, prefix := range []string{walPrefixRoot, oplogPrefixRoot} {
		keys, err := r.listKeys(ctx, prefix)
		if err != nil {
			return n, err
		}
		for _, k := range keys {
			b, err := readAll(ctx, r.be, k, MaxSegmentSize+(1<<20))
			if errors.Is(err, backend.ErrNotFound) {
				continue // expired meanwhile
			}
			if err != nil {
				return n, err
			}
			out, changed, err := r.keys.RewrapObject(r.public, b)
			if err != nil {
				return n, fmt.Errorf("%s: %w: %w", k, ErrCorrupt, err)
			}
			if !changed {
				continue
			}
			if err := r.be.Put(ctx, k, bytes.NewReader(out), backend.PutOptions{}); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
