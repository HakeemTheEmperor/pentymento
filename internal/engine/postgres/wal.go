// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pglogrepl"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
)

// walPos is a place in a cluster's WAL: a timeline and an LSN. As a
// capture position it is written <timeline>/<LSN>, such as 1/0/3000060.
type walPos struct {
	tl  uint32
	lsn pglogrepl.LSN
}

func (p walPos) position() engine.Position {
	return engine.Position(fmt.Sprintf("%d/%s", p.tl, p.lsn))
}

// parseWALPos reads a capture position.
func parseWALPos(p engine.Position) (walPos, error) {
	tl, lsn, ok := strings.Cut(string(p), "/")
	if !ok {
		return walPos{}, fmt.Errorf("capture position %q is not <timeline>/<LSN>", p)
	}
	n, err := strconv.ParseUint(tl, 10, 32)
	if err != nil || n == 0 {
		return walPos{}, fmt.Errorf("capture position %q has no valid timeline", p)
	}
	l, err := pglogrepl.ParseLSN(lsn)
	if err != nil {
		return walPos{}, fmt.Errorf("capture position %q: %w", p, err)
	}
	return walPos{tl: uint32(n), lsn: l}, nil
}

// walFileName is the name Postgres gives the WAL segment holding lsn on a
// timeline: 24 hex digits of timeline, log and segment numbers.
func walFileName(tl uint32, lsn pglogrepl.LSN, segSize uint64) string {
	segno := uint64(lsn) / segSize
	perID := 0x100000000 / segSize
	return fmt.Sprintf("%08X%08X%08X", tl, segno/perID, segno%perID)
}

// parseSize reads a size setting as SHOW prints it, such as "16MB".
func parseSize(s string) (uint64, error) {
	units := []struct {
		suffix string
		mult   uint64
	}{{"kB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"TB", 1 << 40}, {"B", 1}}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseUint(strings.TrimSpace(n), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("size %q is not understood", s)
			}
			return v * u.mult, nil
		}
	}
	return 0, fmt.Errorf("size %q is not understood", s)
}

// walSegment is a stretch of WAL to store: a whole segment, or the part of
// the current one received so far.
type walSegment struct {
	name       string
	start, end pglogrepl.LSN
	data       []byte
	complete   bool
}

// errStreamGap means the server sent WAL that does not follow what came
// before.
var errStreamGap = errors.New("the WAL stream is not contiguous")

// walAssembler collects streamed WAL into segments. The stream starts at a
// segment boundary, so every segment it completes is whole; the bytes of
// the current segment received so far can be stored as a partial segment.
type walAssembler struct {
	tl       uint32
	segSize  uint64
	segStart pglogrepl.LSN
	buf      []byte
}

// newWALAssembler starts at the boundary of the segment holding lsn.
func newWALAssembler(tl uint32, segSize uint64, lsn pglogrepl.LSN) *walAssembler {
	start := pglogrepl.LSN(uint64(lsn) / segSize * segSize)
	return &walAssembler{tl: tl, segSize: segSize, segStart: start, buf: make([]byte, 0, segSize)}
}

// start is where streaming must begin.
func (a *walAssembler) start() pglogrepl.LSN { return a.segStart }

// received is the end of the WAL received so far.
func (a *walAssembler) received() pglogrepl.LSN {
	return a.segStart + pglogrepl.LSN(len(a.buf))
}

// add appends WAL starting at start and returns the segments it completes.
// A returned segment's data is valid until the next call to add.
func (a *walAssembler) add(start pglogrepl.LSN, data []byte) ([]walSegment, error) {
	if start != a.received() {
		return nil, fmt.Errorf("%w: expected %s, got %s", errStreamGap, a.received(), start)
	}
	var done []walSegment
	for len(data) > 0 {
		room := int(a.segSize) - len(a.buf) // #nosec G115 -- a segment size, at most 1 GB
		n := min(room, len(data))
		a.buf = append(a.buf, data[:n]...)
		data = data[n:]
		if len(a.buf) == int(a.segSize) { // #nosec G115 -- a segment size, at most 1 GB
			seg := walSegment{
				name: walFileName(a.tl, a.segStart, a.segSize), start: a.segStart,
				end: a.segStart + pglogrepl.LSN(a.segSize), data: a.buf, complete: true,
			}
			done = append(done, seg)
			a.segStart = seg.end
			// The completed segment's bytes stay valid until the next add:
			// the next segment gets a new buffer.
			a.buf = make([]byte, 0, a.segSize)
		}
	}
	return done, nil
}

// partial returns the current segment's bytes received so far, if any.
func (a *walAssembler) partial() (walSegment, bool) {
	if len(a.buf) == 0 {
		return walSegment{}, false
	}
	return walSegment{
		name: walFileName(a.tl, a.segStart, a.segSize), start: a.segStart,
		end: a.received(), data: a.buf,
	}, true
}
