// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package chunker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/zeebo/blake3"
)

// Chunk sizes (PRD 9.1).
const (
	MinSize = 256 << 10
	AvgSize = 1 << 20
	MaxSize = 4 << 20
)

// gearContext separates the gear-table derivation from every other use of
// the chunk-ID key. Changing it changes every chunk boundary.
const gearContext = "pentymento chunker gear table v1"

// window is the gear hash's effective window: a byte affects the hash for
// the next 64 positions only.
const window = 64

// Table is a FastCDC gear table: 256 pseudo-random 64-bit values.
type Table [256]uint64

// NewTable derives the gear table from the repository's chunk-ID key with
// BLAKE3 key derivation.
func NewTable(chunkIDKey [32]byte) *Table {
	var raw [256 * 8]byte
	blake3.DeriveKey(gearContext, chunkIDKey[:], raw[:])
	var t Table
	for i := range t {
		t[i] = binary.LittleEndian.Uint64(raw[i*8:])
	}
	return &t
}

// Params are chunk-size limits. Avg must be a power of two, with
// window <= Min < Avg < Max.
type Params struct {
	Min, Avg, Max int
}

// DefaultParams are the sizes from PRD 9.1.
var DefaultParams = Params{Min: MinSize, Avg: AvgSize, Max: MaxSize}

func (p Params) validate() error {
	if p.Min < window || p.Min >= p.Avg || p.Avg >= p.Max || bits.OnesCount(uint(p.Avg)) != 1 {
		return fmt.Errorf("invalid chunk sizes %+v", p)
	}
	return nil
}

// masks returns FastCDC's normalized-chunking masks (level 2): a stricter
// mask with two more bits before the average size, a looser one with two
// fewer after it. They select the high bits of the gear hash, which depend
// on the whole 64-byte window.
func (p Params) masks() (small, large uint64) {
	b := bits.TrailingZeros(uint(p.Avg))
	return ^uint64(0) << (64 - (b + 2)), ^uint64(0) << (64 - (b - 2))
}

// Cut returns the length of the next chunk at the start of data. data holds
// at least p.Max bytes unless the stream ends sooner.
//
// The hash starts window bytes before Min, so the decision at each position
// depends only on the 64 bytes before it, not on where the chunk began.
func (p Params) cut(t *Table, data []byte) int {
	n := len(data)
	if n <= p.Min {
		return n
	}
	if n > p.Max {
		n = p.Max
	}
	normal := min(p.Avg, n)
	small, large := p.masks()
	var fp uint64
	for i := p.Min - window; i < p.Min; i++ {
		fp = fp<<1 + t[data[i]]
	}
	i := p.Min
	for ; i < normal; i++ {
		fp = fp<<1 + t[data[i]]
		if fp&small == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = fp<<1 + t[data[i]]
		if fp&large == 0 {
			return i + 1
		}
	}
	return n
}

// Chunker splits a stream into chunks, holding at most two maximum-size
// chunks in memory whatever the stream's length.
type Chunker struct {
	r     io.Reader
	table *Table
	p     Params
	buf   []byte
	start int // first unreturned byte
	end   int // end of buffered data
	eof   bool
}

// New returns a chunker over r with the default sizes.
func New(r io.Reader, table *Table) *Chunker {
	c, _ := NewWithParams(r, table, DefaultParams)
	return c
}

// NewWithParams returns a chunker with other chunk sizes, for tests.
func NewWithParams(r io.Reader, table *Table, p Params) (*Chunker, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &Chunker{r: r, table: table, p: p, buf: make([]byte, 2*p.Max)}, nil
}

// Next returns the next chunk, or io.EOF after the last. The slice is only
// valid until the next call.
func (c *Chunker) Next() ([]byte, error) {
	if err := c.fill(); err != nil {
		return nil, err
	}
	if c.start == c.end {
		return nil, io.EOF
	}
	n := c.p.cut(c.table, c.buf[c.start:c.end])
	chunk := c.buf[c.start : c.start+n]
	c.start += n
	return chunk, nil
}

// fill reads until a full maximum-size chunk is buffered or the stream
// ends, moving unreturned data to the front of the buffer first.
func (c *Chunker) fill() error {
	if c.end-c.start >= c.p.Max || c.eof {
		return nil
	}
	if c.start > 0 {
		c.end = copy(c.buf, c.buf[c.start:c.end])
		c.start = 0
	}
	for c.end < c.p.Max && !c.eof {
		n, err := c.r.Read(c.buf[c.end:])
		c.end += n
		if errors.Is(err, io.EOF) {
			c.eof = true
		} else if err != nil {
			return err
		}
	}
	return nil
}
