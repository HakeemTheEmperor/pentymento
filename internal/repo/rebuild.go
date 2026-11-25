// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/compress"
)

// RebuildStats describes an index rebuild.
type RebuildStats struct {
	Packs   int `json:"packs"`
	Chunks  int `json:"chunks"`
	Written int `json:"index_files_written"`
	Removed int `json:"index_files_removed"`
	// Unreadable lists packs whose trailer could not be read; their chunks
	// are not in the rebuilt index.
	Unreadable []string `json:"unreadable,omitempty"`
}

// packObject is a pack found in storage.
type packObject struct {
	id   PackID
	key  string
	size int64
}

// listPacks returns every object under data/ that is named like a pack;
// other keys are returned as strays.
func (r *Repository) listPacks(ctx context.Context) ([]packObject, []string, error) {
	var packs []packObject
	var strays []string
	err := r.be.List(ctx, dataPrefix, func(oi backend.ObjectInfo) error {
		name := oi.Key[strings.LastIndexByte(oi.Key, '/')+1:]
		b, err := hex.DecodeString(name)
		if err != nil || len(b) != len(PackID{}) || packKey(PackID(b)) != oi.Key {
			strays = append(strays, oi.Key)
			return nil
		}
		packs = append(packs, packObject{id: PackID(b), key: oi.Key, size: oi.Size})
		return nil
	})
	return packs, strays, err
}

// readPackTrailer reads a pack's trailer with one range read of its end.
func (r *Repository) readPackTrailer(ctx context.Context, p packObject) (packTrailer, error) {
	n := min(p.size, int64(trailerMaxBytes+packFooterSize))
	rc, err := r.be.Get(ctx, p.key, &backend.Range{Offset: p.size - n, Length: n})
	if err != nil {
		return packTrailer{}, err
	}
	tail, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return packTrailer{}, err
	}
	t, err := r.readTrailer(tail)
	if err != nil {
		return packTrailer{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return t, nil
}

// RebuildIndex replaces the index with one read from the pack trailers
// (SDD 5.5). New index files are written before the old ones are deleted,
// so an interruption leaves at worst duplicate entries, and running it
// again finishes. It needs the metadata key only, so an agent slot is
// enough; data keys are kept as the trailers hold them, which the keyring
// reads even after key rotations. The caller holds the exclusive lock.
func (r *Repository) RebuildIndex(ctx context.Context) (*RebuildStats, error) {
	if !r.unlocked {
		return nil, ErrLocked
	}
	old, err := r.listKeys(ctx, indexPrefix)
	if err != nil {
		return nil, err
	}
	packs, _, err := r.listPacks(ctx)
	if err != nil {
		return nil, err
	}
	c, err := compress.New(compress.Default, indexMaxBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()

	stats := &RebuildStats{}
	written := map[string]bool{}
	var batch []packRecord
	flush := func() error {
		key, err := r.writeIndex(ctx, c, batch)
		if err != nil {
			return err
		}
		if key != "" {
			written[key] = true
			stats.Written++
		}
		batch = nil
		return nil
	}
	for _, p := range packs {
		t, err := r.readPackTrailer(ctx, p)
		if errors.Is(err, backend.ErrNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				return nil, fmt.Errorf("%s: %w", p.key, err)
			}
			stats.Unreadable = append(stats.Unreadable, p.id.String())
			continue
		}
		batch = append(batch, packRecord{id: p.id, wrappedKey: t.wrappedKey, entries: t.entries})
		stats.Packs++
		stats.Chunks += len(t.entries)
		if len(batch) == IndexFlushPacks {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	for _, k := range old {
		if written[k] {
			continue
		}
		if err := r.be.Delete(ctx, k); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return nil, err
		}
		if r.cache != nil {
			r.cache.remove(k)
		}
		stats.Removed++
	}
	return stats, nil
}
