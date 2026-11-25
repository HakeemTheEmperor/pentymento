// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package chunker

import (
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/zeebo/blake3"
)

// ID names a chunk: the keyed BLAKE3-256 hash of its raw content.
type ID [32]byte

// String returns the ID as 64 lowercase hex characters.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// ParseID parses 64 lowercase hex characters.
func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != 2*len(id) {
		return ID{}, fmt.Errorf("chunk ID %q: want %d hex characters", s, 2*len(id))
	}
	for _, c := range s {
		if !isLowerHex(c) {
			return ID{}, fmt.Errorf("chunk ID %q: not lowercase hex", s)
		}
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return ID{}, fmt.Errorf("chunk ID %q: %w", s, err)
	}
	return id, nil
}

func isLowerHex(c rune) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}

// Hasher computes chunk IDs with the repository's chunk-ID key. It is safe
// for concurrent use, and never prints its key.
type Hasher struct {
	pool sync.Pool // of *blake3.Hasher, each keyed
}

// NewHasher returns a hasher keyed with the chunk-ID key.
func NewHasher(chunkIDKey [32]byte) *Hasher {
	base, err := blake3.NewKeyed(chunkIDKey[:])
	if err != nil {
		panic(err) // unreachable: the key is always 32 bytes
	}
	h := &Hasher{}
	h.pool.New = func() any { return base.Clone() }
	return h
}

// Sum returns the chunk ID of data.
func (h *Hasher) Sum(data []byte) ID {
	b := h.pool.Get().(*blake3.Hasher)
	b.Reset()
	_, _ = b.Write(data)
	var id ID
	b.Sum(id[:0])
	h.pool.Put(b)
	return id
}

// String does not reveal the key.
func (*Hasher) String() string { return "[chunk-ID hasher]" }

// GoString does not reveal the key.
func (*Hasher) GoString() string { return "[chunk-ID hasher]" }

// Format does not reveal the key.
func (*Hasher) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[chunk-ID hasher]")) }
