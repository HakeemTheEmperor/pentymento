// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package compress compresses chunks with zstd (PRD 7.7, SDD 5.3).
//
// Each compressed chunk starts with a one-byte header: 0x00 when the
// content is stored raw, 0x01 when it is zstd-compressed. A chunk is stored
// raw when compression would save less than 3%, so incompressible data
// (already-compressed files, encrypted columns) costs no CPU to read back.
// Chunks are compressed independently, so one can be read with a range
// request.
package compress

import (
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Header bytes.
const (
	HeaderNone byte = 0x00
	HeaderZstd byte = 0x01
)

// minSavingPercent is the smallest saving worth keeping compressed.
const minSavingPercent = 3

// Level is a compression level (PRD 7.7).
type Level int

// Levels, mapped to klauspost/compress zstd speeds.
const (
	Fastest Level = iota
	Default       // about zstd level 3
	Better
	Best
)

var levelNames = map[Level]string{Fastest: "fastest", Default: "default", Better: "better", Best: "best"}

// String returns the level's config name.
func (l Level) String() string {
	if s, ok := levelNames[l]; ok {
		return s
	}
	return fmt.Sprintf("Level(%d)", int(l))
}

// ParseLevel parses fastest, default, better or best.
func ParseLevel(s string) (Level, error) {
	for l, name := range levelNames {
		if name == s {
			return l, nil
		}
	}
	return 0, fmt.Errorf("compression must be fastest, default, better or best, not %q", s)
}

func (l Level) zstd() (zstd.EncoderLevel, error) {
	switch l {
	case Fastest:
		return zstd.SpeedFastest, nil
	case Default:
		return zstd.SpeedDefault, nil
	case Better:
		return zstd.SpeedBetterCompression, nil
	case Best:
		return zstd.SpeedBestCompression, nil
	}
	return 0, fmt.Errorf("unknown compression level %d", int(l))
}

// ErrCorrupt means compressed data is malformed or decodes to more than
// the allowed size.
var ErrCorrupt = errors.New("corrupt compressed data")

// Compressor compresses and decompresses chunks. It is safe for concurrent
// use; Close releases its resources.
type Compressor struct {
	enc     *zstd.Encoder
	dec     *zstd.Decoder
	maxSize int
}

// New returns a compressor at level whose Decompress refuses output larger
// than maxSize bytes (the maximum chunk size).
func New(level Level, maxSize int) (*Compressor, error) {
	zl, err := level.zstd()
	if err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zl))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderMaxMemory(uint64(maxSize)), // #nosec G115 -- a chunk size, positive
		zstd.WithDecoderMaxWindow(uint64(maxSize)), // #nosec G115 -- a chunk size, positive
		zstd.WithDecoderConcurrency(0),
	)
	if err != nil {
		_ = enc.Close()
		return nil, err
	}
	return &Compressor{enc: enc, dec: dec, maxSize: maxSize}, nil
}

// Close releases the encoder and decoder.
func (c *Compressor) Close() error {
	c.dec.Close()
	return c.enc.Close()
}

// Compress appends the header and the content of src to dst: compressed,
// or raw when compression would save less than 3%.
func (c *Compressor) Compress(dst, src []byte) []byte {
	start := len(dst)
	dst = append(dst, HeaderZstd)
	dst = c.enc.EncodeAll(src, dst)
	if payload := len(dst) - start - 1; payload*100 > len(src)*(100-minSavingPercent) {
		dst = append(dst[:start], HeaderNone)
		dst = append(dst, src...)
	}
	return dst
}

// Decompress appends the content of a compressed chunk to dst.
func (c *Compressor) Decompress(dst, src []byte) ([]byte, error) {
	if len(src) == 0 {
		return dst, fmt.Errorf("%w: empty", ErrCorrupt)
	}
	switch src[0] {
	case HeaderNone:
		if len(src)-1 > c.maxSize {
			return dst, fmt.Errorf("%w: raw chunk of %d bytes exceeds %d", ErrCorrupt, len(src)-1, c.maxSize)
		}
		return append(dst, src[1:]...), nil
	case HeaderZstd:
		if len(src) == 1 {
			// Compress stores empty content raw, so this is never valid.
			return dst, fmt.Errorf("%w: zstd header without data", ErrCorrupt)
		}
		start := len(dst)
		out, err := c.dec.DecodeAll(src[1:], dst)
		if err != nil {
			return dst[:start], fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if len(out)-start > c.maxSize {
			return dst[:start], fmt.Errorf("%w: decodes to more than %d bytes", ErrCorrupt, c.maxSize)
		}
		return out, nil
	}
	return dst, fmt.Errorf("%w: unknown header byte 0x%02x", ErrCorrupt, src[0])
}
