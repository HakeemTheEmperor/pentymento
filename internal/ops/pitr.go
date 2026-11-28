// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// IDUnreachable is a restore point the stored snapshots and log cannot
// reach (see docs/errors).
const IDUnreachable msg.ID = "PTM-6002"

// WALPlan is what a Postgres point-in-time restore replays (SDD 6.4 step
// 2): the anchor snapshot, and the stored WAL from its start to the point.
type WALPlan struct {
	Snapshot repo.Snapshot
	// Segments are the WAL to replay, in order: the complete segments,
	// and partial ones only where a timeline has no complete segment.
	Segments    []repo.WALSegment
	SegmentSize uint64
}

// reachable is a stretch of time a restore can reach: from the end of a
// snapshot to the newest WAL stored after it without a break.
type reachable struct {
	run        walRun
	snaps      []repo.Snapshot
	from, to   time.Time
	fromLSN    uint64
	runEndLSN  uint64
	breakAfter string
}

func reachableRuns(l walLayout, snaps []repo.Snapshot) []reachable {
	var out []reachable
	for i, r := range l.runs {
		in := r.snapshots(snaps)
		if len(in) == 0 {
			continue
		}
		el, _ := lsnOf(in[0].EndPosition)
		to := r.endTime
		if to.Before(in[0].End) {
			to = in[0].End
		}
		rr := reachable{run: r, snaps: in, from: in[0].End, to: to, fromLSN: uint64(el), runEndLSN: r.end}
		if i+1 < len(l.runs) {
			rr.breakAfter = l.runs[i+1].breakBefore
		}
		out = append(out, rr)
	}
	return out
}

// PlanPostgres chooses what a restore to p replays, or explains why p
// cannot be reached, naming the gap and the reachable points around it.
// when formats a time for messages.
//
//   - A time: the newest snapshot ending at or before it, in a stretch of
//     WAL stored without a break that reaches the time; WAL up to the
//     first segment stored after the time, and one more.
//   - An LSN: the newest snapshot ending at or before it, in the stretch
//     holding it; WAL up to the segment holding it.
//   - A transaction ID, whose time is not known before replay: the
//     earliest snapshot of the newest stretch, and all its WAL.
func PlanPostgres(target string, snaps []repo.Snapshot, segs []repo.WALSegment, gaps []repo.Gap, p engine.Point, when func(time.Time) string) (*WALPlan, error) {
	if len(snaps) == 0 {
		return nil, &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
			Level: msg.LevelFailed, Target: target, Fact: "the repository has no snapshots of " + target, Next: "pentym backup " + target,
		}}
	}
	l := layoutWAL(snaps, segs, gaps)
	runs := reachableRuns(l, snaps)
	if len(runs) == 0 {
		fact := "no WAL has been captured, so only snapshots can be restored"
		if len(l.runs) > 0 {
			fact = "no snapshot lies within the captured WAL, so there is nothing to replay from"
		}
		return nil, unreachable(target, describePoint(p, when), fact, nil, when)
	}
	var anchor *repo.Snapshot
	var through uint64 // the last segment number to replay
	var run walRun
	switch {
	case !p.Time.IsZero():
		t := p.Time
		for _, r := range runs {
			if t.Before(r.from) || t.After(r.to) {
				continue
			}
			for i := range r.snaps {
				if !r.snaps[i].End.After(t) {
					anchor = &r.snaps[i]
				}
			}
			run = r.run
			through = r.run.last
			for n := r.run.first; n <= r.run.last; n++ {
				if pc := l.pieces[n]; pc != nil && !pc.stored.Before(t) {
					through = min(n+1, r.run.last)
					break
				}
			}
			break
		}
		if anchor == nil {
			return nil, unreachable(target, describePoint(p, when), timeReason(runs, t, when), runs, when)
		}
	case p.XID != "":
		r := runs[len(runs)-1]
		anchor, run, through = &r.snaps[0], r.run, r.run.last
	case p.Before != "":
		lsn, ok := pointLSN(p.Before)
		if !ok {
			return nil, &msg.Error{Code: msg.ExitUsage, Message: msg.Message{Level: msg.LevelFailed, Target: target,
				Fact: fmt.Sprintf("%q is not a WAL position; write it as Postgres does, such as 0/3A00F128", p.Before), Next: "pentym restore --help"}}
		}
		for _, r := range runs {
			if lsn < r.fromLSN || lsn > r.runEndLSN {
				continue
			}
			for i := range r.snaps {
				if el, _ := lsnOf(r.snaps[i].EndPosition); uint64(el) <= lsn {
					anchor = &r.snaps[i]
				}
			}
			run, through = r.run, min(lsn/l.size, r.run.last)
			break
		}
		if anchor == nil {
			return nil, unreachable(target, describePoint(p, when), lsnReason(runs, lsn), runs, when)
		}
	default:
		return nil, errors.New("no restore point given")
	}
	sl, _ := lsnOf(anchor.StartPosition)
	plan := &WALPlan{Snapshot: *anchor, SegmentSize: l.size}
	for n := max(uint64(sl)/l.size, run.first); n <= through; n++ {
		plan.Segments = append(plan.Segments, stageable(l.pieces[n])...)
	}
	return plan, nil
}

// stageable picks what to replay of one segment number: each timeline's
// complete segment, or its longest partial when it has none.
func stageable(p *walPiece) []repo.WALSegment {
	if p == nil {
		return nil
	}
	byTL := map[repo.Timeline]repo.WALSegment{}
	for _, s := range p.segs {
		cur, ok := byTL[s.Timeline]
		switch {
		case !ok:
			byTL[s.Timeline] = s
		case cur.Partial && !s.Partial:
			byTL[s.Timeline] = s
		case cur.Partial && s.Partial && s.EndLSN > cur.EndLSN:
			byTL[s.Timeline] = s
		}
	}
	out := make([]repo.WALSegment, 0, len(byTL))
	for _, s := range byTL {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timeline < out[j].Timeline })
	return out
}

// pointLSN reads an LSN point: X/Y, or a capture position <timeline>/X/Y.
func pointLSN(p engine.Position) (uint64, bool) {
	if strings.Count(string(p), "/") == 2 {
		l, ok := walPositionLSN(string(p))
		return uint64(l), ok
	}
	l, ok := lsnOf(string(p))
	return uint64(l), ok
}

func describePoint(p engine.Point, when func(time.Time) string) string {
	switch {
	case !p.Time.IsZero():
		return when(p.Time)
	case p.XID != "":
		return "transaction " + p.XID
	}
	return "WAL position " + string(p.Before)
}

// timeReason says why a time falls outside what can be reached.
func timeReason(runs []reachable, t time.Time, when func(time.Time) string) string {
	first, last := runs[0], runs[len(runs)-1]
	switch {
	case t.After(last.to):
		return "it is after the newest stored change, from " + when(last.to)
	case t.Before(first.from):
		return "it is before the earliest point that can be reached, " + when(first.from)
	}
	for i := 0; i+1 < len(runs); i++ {
		if t.After(runs[i].to) && t.Before(runs[i+1].from) {
			return gapReason(runs[i], runs[i+1], when)
		}
	}
	return "no snapshot precedes it in the WAL that covers it"
}

// lsnReason says why an LSN falls outside what can be reached.
func lsnReason(runs []reachable, lsn uint64) string {
	first, last := runs[0], runs[len(runs)-1]
	switch {
	case lsn > last.runEndLSN:
		return "it is after the newest stored WAL, " + lsnText(repo.LSN(last.runEndLSN))
	case lsn < first.fromLSN:
		return "it is before the earliest point that can be reached, " + lsnText(repo.LSN(first.fromLSN))
	}
	for i := 0; i+1 < len(runs); i++ {
		if lsn > runs[i].runEndLSN && lsn < runs[i+1].fromLSN {
			return "it falls in a gap in the captured WAL: " + runs[i].breakAfter
		}
	}
	return "no snapshot precedes it in the WAL that covers it"
}

func gapReason(before, after reachable, when func(time.Time) string) string {
	reason := "it falls in a gap in the captured WAL, between " + when(before.to) + " and " + when(after.from)
	if before.breakAfter != "" {
		reason += ": " + before.breakAfter
	}
	return reason
}

// unreachable refuses a restore point, listing what can be reached.
func unreachable(target, point, reason string, runs []reachable, when func(time.Time) string) error {
	next := "pentym status"
	if len(runs) > 0 {
		var spans []string
		for _, r := range runs {
			spans = append(spans, "from "+when(r.from)+" to "+when(r.to))
		}
		next = "restore to a point that can be reached: " + strings.Join(spans, "; ")
	}
	return &msg.Error{Code: msg.ExitUnrecoverable, Message: msg.Message{
		Level: msg.LevelFailed, ID: IDUnreachable, Target: target,
		Fact:   "cannot restore to " + point + ": " + reason,
		Impact: "nothing was restored",
		Next:   next,
	}}
}

// walSource gives the engine a plan's WAL: the history files of the
// timelines it replays, then its segments, partial ones named
// <segment>.partial.
type walSource struct {
	repo  *repo.Repository
	names []string
	keys  map[string]string
}

func newWALSource(ctx context.Context, r *repo.Repository, tid string, plan *WALPlan) (*walSource, error) {
	w := &walSource{repo: r, keys: map[string]string{}}
	seen := map[repo.Timeline]bool{}
	for _, s := range plan.Segments {
		if s.Timeline <= 1 || seen[s.Timeline] {
			continue
		}
		seen[s.Timeline] = true
		key := r.TimelineHistoryKey(tid, s.Timeline)
		if _, err := r.Backend().Stat(ctx, key); errors.Is(err, backend.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		name := s.Timeline.String() + ".history"
		w.names, w.keys[name] = append(w.names, name), key
	}
	for _, s := range plan.Segments {
		name := s.Name
		if s.Partial {
			name += ".partial"
		}
		w.names, w.keys[name] = append(w.names, name), s.Key
	}
	return w, nil
}

// Names lists the plan's WAL, in replay order; the plan already holds
// what replay from the snapshot's start to the point needs.
func (w *walSource) Names(context.Context, engine.Position, engine.Position) ([]string, error) {
	return w.names, nil
}

func (w *walSource) Segment(ctx context.Context, name string) ([]byte, error) {
	key, ok := w.keys[name]
	if !ok {
		return nil, fmt.Errorf("%s is not part of this restore", name)
	}
	return w.repo.GetSegment(ctx, key)
}
