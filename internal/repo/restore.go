// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

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

// ReadSnapshot writes a snapshot's main stream to w, chunk by chunk,
// checking every chunk against its ID. Memory stays bounded whatever the
// snapshot's size. It needs a full key slot.
func (r *Repository) ReadSnapshot(ctx context.Context, idx *Index, s *Snapshot, w io.Writer) error {
	return r.readStream(ctx, idx, s, s.List, w)
}

// ReadPart writes one of a snapshot's streams to w: the main stream (by
// its name) or a further part.
func (r *Repository) ReadPart(ctx context.Context, idx *Index, s *Snapshot, name string, w io.Writer) error {
	for _, p := range s.streams() {
		if p.Name == name {
			return r.readStream(ctx, idx, s, p.List, w)
		}
	}
	return fmt.Errorf("snapshot %s has no part %q", s.ID, name)
}

// Read-path tuning (PRD 7.1: restores fetch chunks with parallel range
// reads and read-ahead).
const (
	// readWorkers fetch and decode chunk groups at once.
	readWorkers = 4
	// readWindow bounds the groups fetched ahead of the writer; with
	// maxGroupBytes it bounds the memory a read holds.
	readWindow = 8
	// A group is a run of chunks stored next to each other in one pack,
	// read with one ranged request: at most maxGroupBytes stored and
	// maxGroupBytes decoded, and at most maxGroupChunks chunks.
	maxGroupBytes  = 8 << 20
	maxGroupChunks = 64
)

// chunkRef is one chunk of a stream, located.
type chunkRef struct {
	id      chunker.ID
	loc     Location
	wrapped []byte
}

// fetchGroup is a run of adjacent chunks, fetched together, or an error
// that ends the stream.
type fetchGroup struct {
	refs []chunkRef
	done chan struct{}
	data [][]byte
	err  error
}

// readStream writes a stream to w. Chunks stored next to each other in a
// pack are fetched with one ranged read; up to readWorkers groups are
// fetched and decoded at once and up to readWindow wait ahead of the
// writer, delivered in order, so memory stays bounded whatever the
// stream's size. Every chunk is checked against its ID.
func (r *Repository) readStream(ctx context.Context, idx *Index, s *Snapshot, list []string, w io.Writer) error {
	if !r.CanRead() {
		return ErrNoPrivateKey
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readers := make([]*chunkReader, 0, readWorkers+1)
	defer func() {
		for _, cr := range readers {
			cr.close()
		}
	}()
	for range readWorkers + 1 {
		cr, err := r.newChunkReader(idx)
		if err != nil {
			return err
		}
		readers = append(readers, cr)
	}
	order := make(chan *fetchGroup, readWindow)
	jobs := make(chan *fetchGroup)
	var wg sync.WaitGroup
	for _, cr := range readers[1:] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := range jobs {
				cr.fetch(ctx, g)
				close(g.done)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(jobs)
		defer close(order)
		if err := r.planStream(ctx, readers[0], idx, s, list, order, jobs); err != nil {
			g := &fetchGroup{err: err, done: make(chan struct{})}
			close(g.done)
			select {
			case order <- g:
			case <-ctx.Done():
			}
		}
	}()
	err := writeGroups(ctx, order, w)
	cancel()
	for range order { //nolint:revive // drained only to let the planner exit
	}
	wg.Wait()
	return err
}

// writeGroups writes the groups' chunks to w in order.
func writeGroups(ctx context.Context, order <-chan *fetchGroup, w io.Writer) error {
	for g := range order {
		select {
		case <-g.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if g.err != nil {
			return g.err
		}
		for _, d := range g.data {
			if _, err := w.Write(d); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// planStream reads the stream's chunk lists and sends runs of adjacent
// chunks, first in order (bounding the read-ahead), then to a worker.
func (r *Repository) planStream(ctx context.Context, cr *chunkReader, idx *Index, s *Snapshot, list []string,
	order chan<- *fetchGroup, jobs chan<- *fetchGroup,
) error {
	var cur *fetchGroup
	var stored, raw int
	send := func() error {
		if cur == nil {
			return nil
		}
		g := cur
		cur = nil
		for _, ch := range []chan<- *fetchGroup{order, jobs} {
			select {
			case ch <- g:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	for _, lid := range list {
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
			loc, wrapped, ok := idx.Lookup(cid)
			if !ok {
				return fmt.Errorf("snapshot %s: chunk %s is not in the index: %w", s.ID, cid, ErrCorrupt)
			}
			if cur != nil {
				last := cur.refs[len(cur.refs)-1].loc
				adjacent := last.Pack == loc.Pack && last.Offset+last.Length == loc.Offset
				fits := stored+int(loc.Length) <= maxGroupBytes && raw+int(loc.RawLen) <= maxGroupBytes && len(cur.refs) < maxGroupChunks
				if !adjacent || !fits {
					if err := send(); err != nil {
						return err
					}
				}
			}
			if cur == nil {
				cur = &fetchGroup{done: make(chan struct{})}
				stored, raw = 0, 0
			}
			cur.refs = append(cur.refs, chunkRef{id: cid, loc: loc, wrapped: wrapped})
			stored += int(loc.Length)
			raw += int(loc.RawLen)
		}
	}
	return send()
}

// fetch reads a group of adjacent chunks with one ranged read and decodes
// each, setting g.data or g.err.
func (c *chunkReader) fetch(ctx context.Context, g *fetchGroup) {
	first, last := g.refs[0].loc, g.refs[len(g.refs)-1].loc
	length := int64(last.Offset+last.Length) - int64(first.Offset)
	rc, err := c.r.be.Get(ctx, packKey(first.Pack), &backend.Range{Offset: int64(first.Offset), Length: length})
	if errors.Is(err, backend.ErrInvalidRange) {
		g.err = fmt.Errorf("pack %s is shorter than the index says: %w: %w", first.Pack, ErrCorrupt, err)
		return
	}
	if err != nil {
		g.err = fmt.Errorf("chunk %s in pack %s: %w", g.refs[0].id, first.Pack, err)
		return
	}
	buf, err := io.ReadAll(io.LimitReader(rc, length+1))
	_ = rc.Close()
	if err != nil {
		g.err = err
		return
	}
	if int64(len(buf)) != length {
		g.err = fmt.Errorf("pack %s is shorter than the index says: %w", first.Pack, ErrCorrupt)
		return
	}
	g.data = make([][]byte, 0, len(g.refs))
	for _, ref := range g.refs {
		at := ref.loc.Offset - first.Offset
		data, err := c.decode(ref.id, ref.loc, ref.wrapped, buf[at:at+ref.loc.Length])
		if err != nil {
			g.err, g.data = err, nil
			return
		}
		g.data = append(g.data, data)
	}
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
