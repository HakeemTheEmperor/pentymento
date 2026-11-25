// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/zeebo/blake3"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/chunker"
	"github.com/HakeemTheEmperor/pentymento/internal/compress"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
)

const (
	indexPrefix  = "index/"
	indexPurpose = "index"
	// indexMaxBytes bounds one decompressed index file (4096 packs of up to
	// 80 entries each, with room to spare).
	indexMaxBytes = 256 << 20
	// IndexFlushPacks is how many new packs a backup records before
	// writing an index file, so new entries never pile up in memory.
	IndexFlushPacks = 4096
)

// Index file layout, zstd-compressed (one frame, with the chunk header
// byte) and encrypted with the metadata key:
//
//	"PTMi" | version | pack count (uint32) | for each pack:
//	  pack ID (32) | wrapped-key length (uint16) | wrapped key | chunk count (uint32) | entries
var indexMagic = []byte("PTMi")

const indexVersion = 1

// packRecord is one pack's index record.
type packRecord struct {
	id         PackID
	wrappedKey []byte
	entries    []Entry
}

// Location is where a chunk is stored.
type Location struct {
	Pack   PackID
	Offset uint32
	Length uint32
	RawLen uint32
}

// Index maps chunk IDs to their packs, and packs to their wrapped data
// keys. It is safe for concurrent use.
type Index struct {
	mu     sync.RWMutex
	chunks map[chunker.ID]Location
	keys   map[PackID][]byte
}

func newIndex() *Index {
	return &Index{chunks: map[chunker.ID]Location{}, keys: map[PackID][]byte{}}
}

// Has reports whether a chunk is stored.
func (x *Index) Has(id chunker.ID) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	_, ok := x.chunks[id]
	return ok
}

// Lookup returns a chunk's location and its pack's wrapped data key.
func (x *Index) Lookup(id chunker.ID) (Location, []byte, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	loc, ok := x.chunks[id]
	if !ok {
		return Location{}, nil, false
	}
	return loc, x.keys[loc.Pack], true
}

// Len returns the number of chunks indexed.
func (x *Index) Len() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.chunks)
}

// Packs returns the number of packs indexed.
func (x *Index) Packs() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.keys)
}

func (x *Index) add(p packRecord) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.keys[p.id] = p.wrappedKey
	for _, e := range p.entries {
		if _, ok := x.chunks[e.ID]; !ok {
			x.chunks[e.ID] = Location{Pack: p.id, Offset: e.Offset, Length: e.Length, RawLen: e.RawLen}
		}
	}
}

func marshalIndex(packs []packRecord) []byte {
	b := append([]byte(nil), indexMagic...)
	b = append(b, indexVersion)
	b = binary.BigEndian.AppendUint32(b, uint32(len(packs))) // #nosec G115 -- at most IndexFlushPacks
	for _, p := range packs {
		b = append(b, p.id[:]...)
		b = binary.BigEndian.AppendUint16(b, uint16(len(p.wrappedKey))) // #nosec G115 -- a few hundred bytes
		b = append(b, p.wrappedKey...)
		b = binary.BigEndian.AppendUint32(b, uint32(len(p.entries))) // #nosec G115 -- a few thousand at most
		for _, e := range p.entries {
			b = appendEntry(b, e)
		}
	}
	return b
}

func parseIndex(b []byte) ([]packRecord, error) {
	if len(b) < len(indexMagic)+5 || !bytes.Equal(b[:len(indexMagic)], indexMagic) {
		return nil, fmt.Errorf("not an index file: %w", errMalformed)
	}
	if v := b[len(indexMagic)]; v != indexVersion {
		return nil, fmt.Errorf("index version %d is not supported", v)
	}
	b = b[len(indexMagic)+1:]
	count := int(binary.BigEndian.Uint32(b))
	b = b[4:]
	var packs []packRecord
	for range count {
		if len(b) < 32+2 {
			return nil, errMalformed
		}
		var p packRecord
		copy(p.id[:], b)
		n := int(binary.BigEndian.Uint16(b[32:]))
		b = b[34:]
		if len(b) < n+4 {
			return nil, errMalformed
		}
		p.wrappedKey = append([]byte(nil), b[:n]...)
		entries := int(binary.BigEndian.Uint32(b[n:]))
		b = b[n+4:]
		if entries < 0 || len(b) < entries*entrySize {
			return nil, errMalformed
		}
		p.entries = make([]Entry, entries)
		for i := range p.entries {
			p.entries[i] = readEntry(b[i*entrySize:])
		}
		b = b[entries*entrySize:]
		packs = append(packs, p)
	}
	if len(b) != 0 {
		return nil, fmt.Errorf("index file has trailing data: %w", errMalformed)
	}
	return packs, nil
}

// writeIndex stores an index file for packs, named by the hash of its
// stored bytes.
func (r *Repository) writeIndex(ctx context.Context, c *compress.Compressor, packs []packRecord) error {
	if len(packs) == 0 {
		return nil
	}
	b := c.Compress(nil, marshalIndex(packs))
	if !r.cfg.Insecure() {
		b = crypto.EncryptMetadata(r.metadata, indexPurpose, b)
	}
	sum := blake3.Sum256(b)
	key := indexPrefix + PackID(sum).String()
	err := r.be.Put(ctx, key, bytes.NewReader(b), backend.PutOptions{IfNotExists: true})
	if errors.Is(err, backend.ErrExists) {
		return nil
	}
	return err
}

// LoadIndex reads every index file into memory.
func (r *Repository) LoadIndex(ctx context.Context) (*Index, error) {
	if !r.unlocked {
		return nil, ErrLocked
	}
	keys, err := r.listKeys(ctx, indexPrefix)
	if err != nil {
		return nil, err
	}
	c, err := compress.New(compress.Default, indexMaxBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if r.cache != nil {
		r.cache.retain(indexPrefix, keys)
	}
	x := newIndex()
	for _, k := range keys {
		matches := func(b []byte) bool { return indexPrefix+PackID(blake3.Sum256(b)).String() == k }
		b, err := r.cachedRead(ctx, k, indexMaxBytes, matches)
		if err != nil {
			return nil, err
		}
		if !matches(b) {
			return nil, fmt.Errorf("%s: content does not match its name: %w", k, ErrCorrupt)
		}
		if !r.cfg.Insecure() {
			if b, err = crypto.DecryptMetadata(r.metadata, indexPurpose, b); err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
		}
		if b, err = c.Decompress(nil, b); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		packs, err := parseIndex(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		for _, p := range packs {
			x.add(p)
		}
	}
	return x, nil
}
