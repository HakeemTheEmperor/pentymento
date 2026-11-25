// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/zeebo/blake3"

	"github.com/HakeemTheEmperor/pentymento/internal/chunker"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
)

// PackSize is the size at which a pack is closed and uploaded (PRD 9.1).
// A pack ends after the chunk that reaches it, so packs are 16 to 20 MiB.
const PackSize = 16 << 20

// Pack layout:
//
//	chunk 0 | chunk 1 | … | sealed trailer | trailer length (uint32, big-endian) | "PTMp" | version
//
// Each chunk is EncryptChunk(data key, chunk ID, compressed chunk). The
// trailer lists the pack's wrapped data key and every chunk's ID, offset
// and lengths, encrypted with the metadata key, so the index can be rebuilt
// from packs alone.
var packMagic = []byte("PTMp")

const (
	packVersion     = 1
	packFooterSize  = 4 + 4 + 1 // trailer length, magic, version
	entrySize       = 32 + 4 + 4 + 4
	trailerPurpose  = "pack-trailer"
	trailerMaxBytes = 1 << 20
)

// PackID names a pack: the BLAKE3-256 hash of its stored bytes.
type PackID [32]byte

// String returns 64 lowercase hex characters.
func (p PackID) String() string { return hex.EncodeToString(p[:]) }

// packKey returns the pack's object key, fanned out by its first byte.
func packKey(id PackID) string {
	s := id.String()
	return "data/" + s[:2] + "/" + s
}

// Entry locates one chunk inside a pack.
type Entry struct {
	ID     chunker.ID
	Offset uint32 // of the encrypted chunk in the pack
	Length uint32 // of the encrypted chunk
	RawLen uint32 // of the chunk's content, before compression
}

// packTrailer is a pack's self-description.
type packTrailer struct {
	wrappedKey []byte
	entries    []Entry
}

func (t packTrailer) marshal() []byte {
	b := make([]byte, 0, 2+len(t.wrappedKey)+4+len(t.entries)*entrySize)
	b = binary.BigEndian.AppendUint16(b, uint16(len(t.wrappedKey))) // #nosec G115 -- an age header is a few hundred bytes
	b = append(b, t.wrappedKey...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(t.entries))) // #nosec G115 -- a pack holds at most a few thousand chunks
	for _, e := range t.entries {
		b = appendEntry(b, e)
	}
	return b
}

func appendEntry(b []byte, e Entry) []byte {
	b = append(b, e.ID[:]...)
	b = binary.BigEndian.AppendUint32(b, e.Offset)
	b = binary.BigEndian.AppendUint32(b, e.Length)
	return binary.BigEndian.AppendUint32(b, e.RawLen)
}

func readEntry(b []byte) Entry {
	var e Entry
	copy(e.ID[:], b[:32])
	e.Offset = binary.BigEndian.Uint32(b[32:])
	e.Length = binary.BigEndian.Uint32(b[36:])
	e.RawLen = binary.BigEndian.Uint32(b[40:])
	return e
}

var errMalformed = errors.New("malformed")

func parseTrailer(b []byte) (packTrailer, error) {
	var t packTrailer
	if len(b) < 2 {
		return t, errMalformed
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if len(b) < n+4 {
		return t, errMalformed
	}
	t.wrappedKey = append([]byte(nil), b[:n]...)
	b = b[n:]
	count := int(binary.BigEndian.Uint32(b))
	b = b[4:]
	if count < 0 || len(b) != count*entrySize {
		return t, errMalformed
	}
	t.entries = make([]Entry, count)
	for i := range t.entries {
		t.entries[i] = readEntry(b[i*entrySize:])
	}
	return t, nil
}

// packBuilder accumulates encrypted chunks for one pack.
type packBuilder struct {
	r       *Repository
	dataKey crypto.Key
	buf     bytes.Buffer
	entries []Entry
}

// packCapacity fits a full pack: PackSize plus the chunk that crosses it,
// encryption overhead and the trailer, so the buffer never regrows.
const packCapacity = PackSize + chunker.MaxSize + 1<<20

func (r *Repository) newPack() *packBuilder {
	p := &packBuilder{r: r}
	p.buf.Grow(packCapacity)
	if !r.cfg.Insecure() {
		p.dataKey = crypto.NewKey()
	}
	return p
}

// add appends a compressed chunk, encrypting it with the pack's data key,
// and returns its stored size.
func (p *packBuilder) add(id chunker.ID, compressed []byte, rawLen int) int {
	sealed := compressed
	if !p.r.cfg.Insecure() {
		sealed = crypto.EncryptChunk(p.dataKey, id, compressed)
	}
	p.entries = append(p.entries, Entry{
		ID: id, Offset: uint32(p.buf.Len()), Length: uint32(len(sealed)), RawLen: uint32(rawLen), // #nosec G115 -- packs are at most 20 MiB
	})
	p.buf.Write(sealed)
	return len(sealed)
}

func (p *packBuilder) size() int { return p.buf.Len() }

// finish wraps the data key, appends the trailer and footer, and returns the
// pack's bytes, its ID and its index record.
func (p *packBuilder) finish() ([]byte, PackID, packRecord, error) {
	var wrapped []byte
	if !p.r.cfg.Insecure() {
		var err error
		if wrapped, err = crypto.WrapKey(p.r.public, p.dataKey); err != nil {
			return nil, PackID{}, packRecord{}, err
		}
		p.dataKey.Clear()
	}
	trailer := packTrailer{wrappedKey: wrapped, entries: p.entries}.marshal()
	if !p.r.cfg.Insecure() {
		trailer = crypto.EncryptMetadata(p.r.metadata, trailerPurpose, trailer)
	}
	p.buf.Write(trailer)
	var footer []byte
	footer = binary.BigEndian.AppendUint32(footer, uint32(len(trailer))) // #nosec G115 -- bounded by trailerMaxBytes
	footer = append(footer, packMagic...)
	footer = append(footer, packVersion)
	p.buf.Write(footer)
	data := p.buf.Bytes()
	id := PackID(blake3.Sum256(data))
	return data, id, packRecord{id: id, wrappedKey: wrapped, entries: p.entries}, nil
}

// readTrailer decodes a pack's trailer from the end of its bytes, as an
// index rebuild does with a range read.
func (r *Repository) readTrailer(tail []byte) (packTrailer, error) {
	if len(tail) < packFooterSize || !bytes.Equal(tail[len(tail)-5:len(tail)-1], packMagic) {
		return packTrailer{}, fmt.Errorf("not a pack: %w", errMalformed)
	}
	if v := tail[len(tail)-1]; v != packVersion {
		return packTrailer{}, fmt.Errorf("pack version %d is not supported", v)
	}
	n := int(binary.BigEndian.Uint32(tail[len(tail)-packFooterSize:]))
	if n > trailerMaxBytes || n > len(tail)-packFooterSize {
		return packTrailer{}, fmt.Errorf("pack trailer: %w", errMalformed)
	}
	sealed := tail[len(tail)-packFooterSize-n : len(tail)-packFooterSize]
	plain := sealed
	if !r.cfg.Insecure() {
		var err error
		if plain, err = crypto.DecryptMetadata(r.metadata, trailerPurpose, sealed); err != nil {
			return packTrailer{}, err
		}
	}
	return parseTrailer(plain)
}
