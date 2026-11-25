// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/chunker"
	"github.com/HakeemTheEmperor/pentymento/internal/compress"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
)

// ErrCorrupt means stored data failed an integrity check: a chunk's
// content does not match its ID, or a pack does not decrypt.
var ErrCorrupt = errors.New("repository data is corrupt")

// keyCacheSize bounds the unwrapped data keys kept while reading.
const keyCacheSize = 64

// ReadSnapshot writes a snapshot's stream to w, chunk by chunk, checking
// every chunk against its ID. Memory stays bounded whatever the snapshot's
// size. It needs a full key slot.
func (r *Repository) ReadSnapshot(ctx context.Context, idx *Index, s *Snapshot, w io.Writer) error {
	if !r.CanRead() {
		return ErrNoPrivateKey
	}
	cr, err := r.newChunkReader(idx)
	if err != nil {
		return err
	}
	defer cr.close()
	for _, lid := range s.List {
		id, err := chunker.ParseID(lid)
		if err != nil {
			return fmt.Errorf("snapshot %s: %w", s.ID, err)
		}
		block, err := cr.read(ctx, id)
		if err != nil {
			return fmt.Errorf("snapshot %s chunk list: %w", s.ID, err)
		}
		if len(block)%32 != 0 {
			return fmt.Errorf("snapshot %s chunk list block %s: %w", s.ID, id, ErrCorrupt)
		}
		for off := 0; off < len(block); off += 32 {
			var cid chunker.ID
			copy(cid[:], block[off:])
			data, err := cr.read(ctx, cid)
			if err != nil {
				return fmt.Errorf("snapshot %s: %w", s.ID, err)
			}
			if _, err := w.Write(data); err != nil {
				return err
			}
		}
	}
	return nil
}

// chunkReader fetches, decrypts, decompresses and checks chunks.
type chunkReader struct {
	r      *Repository
	idx    *Index
	comp   *compress.Compressor
	hasher *chunker.Hasher
	keys   map[PackID]crypto.Key
	order  []PackID // keys in insertion order, for eviction
}

func (r *Repository) newChunkReader(idx *Index) (*chunkReader, error) {
	comp, err := compress.New(compress.Default, chunker.MaxSize)
	if err != nil {
		return nil, err
	}
	return &chunkReader{
		r: r, idx: idx, comp: comp,
		hasher: chunker.NewHasher(r.chunkID.Array()),
		keys:   map[PackID]crypto.Key{},
	}, nil
}

func (c *chunkReader) close() {
	_ = c.comp.Close()
	for _, k := range c.keys {
		k.Clear()
	}
}

// read returns one chunk's content.
func (c *chunkReader) read(ctx context.Context, id chunker.ID) ([]byte, error) {
	loc, wrapped, ok := c.idx.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("chunk %s is not in the index: %w", id, ErrCorrupt)
	}
	rc, err := c.r.be.Get(ctx, packKey(loc.Pack), &backend.Range{Offset: int64(loc.Offset), Length: int64(loc.Length)})
	if errors.Is(err, backend.ErrInvalidRange) {
		return nil, fmt.Errorf("chunk %s in pack %s: the pack is shorter than the index says: %w: %w", id, loc.Pack, ErrCorrupt, err)
	}
	if err != nil {
		return nil, fmt.Errorf("chunk %s in pack %s: %w", id, loc.Pack, err)
	}
	sealed, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	return c.decode(id, loc, wrapped, sealed)
}

// decode decrypts, decompresses and checks one stored chunk.
func (c *chunkReader) decode(id chunker.ID, loc Location, wrapped, sealed []byte) ([]byte, error) {
	if len(sealed) != int(loc.Length) {
		return nil, fmt.Errorf("chunk %s in pack %s is truncated: %w", id, loc.Pack, ErrCorrupt)
	}
	compressed := sealed
	if !c.r.cfg.Insecure() {
		dk, err := c.dataKey(loc.Pack, wrapped)
		if err != nil {
			return nil, err
		}
		if compressed, err = crypto.DecryptChunk(dk, id, sealed); err != nil {
			return nil, fmt.Errorf("chunk %s in pack %s: %w: %w", id, loc.Pack, ErrCorrupt, err)
		}
	}
	data, err := c.comp.Decompress(nil, compressed)
	if err != nil {
		return nil, fmt.Errorf("chunk %s in pack %s: %w: %w", id, loc.Pack, ErrCorrupt, err)
	}
	if len(data) != int(loc.RawLen) || c.hasher.Sum(data) != id {
		return nil, fmt.Errorf("chunk %s in pack %s does not match its ID: %w", id, loc.Pack, ErrCorrupt)
	}
	return data, nil
}

// dataKey unwraps a pack's data key, keeping recent ones.
func (c *chunkReader) dataKey(pack PackID, wrapped []byte) (crypto.Key, error) {
	if k, ok := c.keys[pack]; ok {
		return k, nil
	}
	k, _, err := c.r.keys.Unwrap(wrapped)
	if err != nil {
		return crypto.Key{}, fmt.Errorf("pack %s data key: %w: %w", pack, ErrCorrupt, err)
	}
	if len(c.order) >= keyCacheSize {
		old := c.order[0]
		c.order = c.order[1:]
		c.keys[old].Clear()
		delete(c.keys, old)
	}
	c.keys[pack] = k
	c.order = append(c.order, pack)
	return k, nil
}
