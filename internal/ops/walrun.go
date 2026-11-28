// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"fmt"
	"sort"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// walPiece is what is stored of one WAL segment number, on any timeline.
type walPiece struct {
	complete   bool
	partialEnd repo.LSN
	stored     time.Time
	segs       []repo.WALSegment
}

// walRun is a stretch of WAL stored without a break: every segment from
// first to last, all complete but possibly the last.
type walRun struct {
	// start and end are the LSNs it covers.
	start, end  uint64
	first, last uint64
	// endTime is when its newest WAL was stored.
	endTime time.Time
	// breakBefore says what interrupts the WAL before the run, and
	// breakTime when; empty for the oldest run.
	breakBefore string
	breakTime   time.Time
}

// snapshots returns the snapshots whose WAL lies inside the run: replay
// from them stays within it.
func (r walRun) snapshots(snaps []repo.Snapshot) []repo.Snapshot {
	var out []repo.Snapshot
	for _, s := range snaps {
		sl, ok1 := lsnOf(s.StartPosition)
		el, ok2 := lsnOf(s.EndPosition)
		if ok1 && ok2 && uint64(sl) >= r.start && uint64(el) <= r.end {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].End.Before(out[j].End) })
	return out
}

// earliest is the run's earliest usable snapshot, or nil.
func (r walRun) earliest(snaps []repo.Snapshot) *repo.Snapshot {
	if in := r.snapshots(snaps); len(in) > 0 {
		return &in[0]
	}
	return nil
}

// walLayout is a target's stored WAL, by segment number, cut into runs.
type walLayout struct {
	size   uint64
	pieces map[uint64]*walPiece
	runs   []walRun // oldest first
}

// walSegmentSize is the cluster's WAL segment size, from the newest
// snapshot that records it.
func walSegmentSize(snaps []repo.Snapshot) uint64 {
	newest := time.Time{}
	size := uint64(defaultWALSegmentSize)
	for _, s := range snaps {
		if s.WALSegmentSize > 0 && !s.End.Before(newest) {
			newest, size = s.End, uint64(s.WALSegmentSize) // #nosec G115 -- a positive size
		}
	}
	return size
}

// layoutWAL cuts the stored WAL into runs. A run breaks at a missing
// segment, at a segment stored only as a partial with WAL after it, and
// at a recorded gap.
func layoutWAL(snaps []repo.Snapshot, segs []repo.WALSegment, gaps []repo.Gap) walLayout {
	l := walLayout{size: walSegmentSize(snaps), pieces: map[uint64]*walPiece{}}
	for _, s := range segs {
		n, ok := segmentNumber(s.Name, l.size)
		if !ok {
			continue
		}
		p := l.pieces[n]
		if p == nil {
			p = &walPiece{}
			l.pieces[n] = p
		}
		p.segs = append(p.segs, s)
		if s.Partial {
			p.partialEnd = max(p.partialEnd, s.EndLSN)
		} else {
			p.complete = true
		}
		if s.Stored.After(p.stored) {
			p.stored = s.Stored
		}
	}
	nums := make([]uint64, 0, len(l.pieces))
	for n := range l.pieces {
		nums = append(nums, n)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	var runs []walRun
	for i, n := range nums {
		p := l.pieces[n]
		if i == 0 || !l.pieces[nums[i-1]].complete || nums[i-1] != n-1 {
			r := walRun{start: n * l.size, first: n}
			if i > 0 {
				prev := nums[i-1]
				pp := l.pieces[prev]
				if prev == n-1 {
					r.breakBefore = fmt.Sprintf("WAL segment %s is stored only up to %s", segmentLabel(prev, l.size), lsnText(pp.partialEnd))
				} else {
					r.breakBefore = fmt.Sprintf("WAL segment %s is missing", segmentLabel(prev+1, l.size))
				}
				r.breakTime = pp.stored
			}
			runs = append(runs, r)
		}
		r := &runs[len(runs)-1]
		r.last, r.endTime = n, p.stored
		r.end = (n + 1) * l.size
		if !p.complete {
			r.end = uint64(p.partialEnd)
		}
	}
	// A recorded gap inside a run cuts it: the WAL after the checkpoint
	// was lost even if what was stored looks continuous.
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Time.Before(gaps[j].Time) })
	for _, g := range gaps {
		from, okFrom := walPositionLSN(g.From)
		to, okTo := walPositionLSN(g.To)
		var after uint64
		switch {
		case okTo:
			after = uint64(to)
		case okFrom:
			after = uint64(from) + 1
		default:
			continue
		}
		for i, r := range runs {
			if after <= r.start || after > r.end {
				continue
			}
			cut := after
			if okFrom && uint64(from) > r.start && uint64(from) < after {
				cut = uint64(from)
			}
			before := r
			before.end, before.last = cut, cut/l.size
			if p := l.pieces[before.last]; p != nil {
				before.endTime = p.stored
			}
			next := r
			next.start = after
			next.first = after / l.size
			next.breakBefore, next.breakTime = g.Reason, g.Time
			runs = append(runs[:i], append([]walRun{before, next}, runs[i+1:]...)...)
			break
		}
	}
	l.runs = runs
	return l
}

// lsnText writes an LSN as Postgres does, X/Y in hex.
func lsnText(l repo.LSN) string { return fmt.Sprintf("%X/%X", uint64(l)>>32, uint64(l)&0xFFFFFFFF) }
