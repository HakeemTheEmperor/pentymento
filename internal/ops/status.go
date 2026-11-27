// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// Heartbeat staleness thresholds (PRD 9.1).
const (
	StaleWarning  = 5 * time.Minute
	StaleCritical = 15 * time.Minute
)

// defaultWALSegmentSize is Postgres' default, used for snapshots that do
// not record their cluster's segment size.
const defaultWALSegmentSize = 16 << 20

// Agent states, as status reports them.
const (
	// AgentCapturing: the lease is held and its holder's heartbeat is
	// fresh.
	AgentCapturing = "capturing"
	// AgentStale: the lease is held but its holder has not written a
	// heartbeat for StaleWarning or more.
	AgentStale = "stale"
	// AgentStopped: the lease was released; no agent captures.
	AgentStopped = "stopped"
	// AgentNone: no agent has ever held the target's lease.
	AgentNone = "none"
)

// AgentView is what the repository says about a target's capturing agent.
type AgentView struct {
	State string
	Level msg.Level
	// Host and AgentID name the lease holder, if any.
	Host, AgentID string
	Generation    uint64
	// LastSeen is the holder's newest heartbeat, or its lease record's time
	// when it has written none; Age is how long ago that was.
	LastSeen time.Time
	Age      time.Duration
}

// Window is a target's point-in-time recovery window (SDD 7.4): recovery
// can reach any time from Start to End.
type Window struct {
	Configured time.Duration
	// PITR is false when no captured log continues from a snapshot: then
	// only snapshots can be restored, and Start and End are zero.
	PITR       bool
	Start, End time.Time
	// Break is the newest place the log is interrupted before the window
	// (a recorded gap or a missing segment), and BreakTime when it
	// happened; zero when the log has no break.
	Break     string
	BreakTime time.Time
	// Logs reports whether any log has been captured.
	Logs bool
}

// Actual is the window's length.
func (w Window) Actual() time.Duration {
	if !w.PITR {
		return 0
	}
	return w.End.Sub(w.Start)
}

// Shortened reports whether a break in the log, rather than the
// repository's age, makes the window shorter than configured.
func (w Window) Shortened(now time.Time) bool {
	return !w.BreakTime.IsZero() && w.Actual() < w.Configured && w.BreakTime.After(now.Add(-w.Configured))
}

// TargetStatus is one target's protection, as the repository shows it.
type TargetStatus struct {
	Name   string
	ID     string
	Engine string
	// EngineVersion is the newest snapshot's.
	EngineVersion  string
	LatestSnapshot *repo.Snapshot
	Snapshots      int
	// SnapshotsOnly is a MongoDB standalone server: its snapshots record no
	// log positions, and it has no log to capture.
	SnapshotsOnly bool
	Window        Window
	Checkpoint    *repo.Checkpoint
	Agent         AgentView
	// Gaps are the gaps recorded since the configured window began.
	Gaps []repo.Gap
}

// Status is every configured target's protection.
type Status struct {
	Time      time.Time
	Targets   []TargetStatus
	FullSlots int
}

// ReadStatus reads every configured target's status from the repository
// (SDD 5.3: repository first). It needs a key slot that reads metadata.
func ReadStatus(ctx context.Context, r *repo.Repository, cfg *config.Config, now time.Time) (*Status, error) {
	st := &Status{Time: now}
	slots, err := r.Slots(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the key slots: %w", err)
	}
	for _, s := range slots {
		if s.Scope == crypto.ScopeFull {
			st.FullSlots++
		}
	}
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the snapshots: %w", err)
	}
	hbs, err := r.LatestHeartbeats(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the heartbeats: %w", err)
	}
	names := make([]string, 0, len(cfg.Targets))
	for n := range cfg.Targets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		ts, err := ReadTargetStatus(ctx, r, cfg, name, snaps, hbs, now)
		if err != nil {
			return nil, err
		}
		st.Targets = append(st.Targets, *ts)
	}
	return st, nil
}

// ReadTargetStatus reads one target's status. snaps and hbs are all the
// repository's snapshots and newest heartbeats; nil reads them.
func ReadTargetStatus(ctx context.Context, r *repo.Repository, cfg *config.Config, name string, snaps []repo.Snapshot, hbs []repo.Heartbeat, now time.Time) (*TargetStatus, error) {
	var err error
	if snaps == nil {
		if snaps, err = r.Snapshots(ctx); err != nil {
			return nil, err
		}
	}
	if hbs == nil {
		if hbs, err = r.LatestHeartbeats(ctx); err != nil {
			return nil, err
		}
	}
	tc := cfg.Targets[name]
	tid, err := r.TargetID(name)
	if err != nil {
		return nil, err
	}
	ts := &TargetStatus{Name: name, ID: tid, Engine: tc.Engine}
	var mine []repo.Snapshot
	for _, s := range snaps {
		if s.Target == tid {
			mine = append(mine, s)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].End.Before(mine[j].End) })
	ts.Snapshots = len(mine)
	if len(mine) > 0 {
		latest := mine[len(mine)-1]
		ts.LatestSnapshot, ts.EngineVersion = &latest, latest.EngineVersion
		ts.SnapshotsOnly = tc.Engine == "mongodb" && latest.StartPosition == "" && latest.EndPosition == ""
	}
	if ts.Checkpoint, err = r.LatestCheckpoint(ctx, tid); err != nil {
		return nil, fmt.Errorf("read the checkpoint of %s: %w", name, err)
	}
	gaps, err := r.Gaps(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("read the gaps of %s: %w", name, err)
	}
	configured := cfg.Retention.Window()
	for _, g := range gaps {
		if g.Time.After(now.Add(-configured)) {
			ts.Gaps = append(ts.Gaps, g)
		}
	}
	switch tc.Engine {
	case "postgres":
		segs, err := r.WALSegments(ctx, tid)
		if err != nil {
			return nil, fmt.Errorf("list the WAL of %s: %w", name, err)
		}
		ts.Window = PostgresWindow(mine, segs, gaps)
	case "mongodb":
		batches, err := r.OplogBatches(ctx, tid)
		if err != nil {
			return nil, fmt.Errorf("list the oplog of %s: %w", name, err)
		}
		ts.Window = MongoWindow(mine, batches, gaps)
	}
	ts.Window.Configured = configured
	lease, err := lock.ReadLease(ctx, r.Backend(), tid)
	if err != nil {
		return nil, fmt.Errorf("read the capture lease of %s: %w", name, err)
	}
	ts.Agent = agentView(lease, hbs, now)
	return ts, nil
}

// agentView judges the target's agent from its lease and the holder's
// heartbeat. Heartbeat age compares the holder's clock with this host's,
// which is fine for minutes of staleness; lease expiry is never judged
// that way.
func agentView(lease *lock.Record, hbs []repo.Heartbeat, now time.Time) AgentView {
	if lease == nil {
		return AgentView{State: AgentNone, Level: msg.LevelWarning}
	}
	v := AgentView{Host: lease.Holder.Host, AgentID: lease.Holder.AgentID, Generation: lease.Generation}
	// Staleness is the holder's heartbeat age (PRD 9.1); a holder without
	// heartbeats is judged by its lease record.
	for _, hb := range hbs {
		if hb.Host == lease.Holder.Host && hb.AgentID == lease.Holder.AgentID && hb.Time.After(v.LastSeen) {
			v.LastSeen = hb.Time
		}
	}
	if v.LastSeen.IsZero() {
		v.LastSeen = lease.Time
	}
	v.Age = max(now.Sub(v.LastSeen), 0)
	switch {
	case lease.State == lock.StateReleased:
		v.State, v.Level = AgentStopped, msg.LevelWarning
	case v.Age >= StaleCritical:
		v.State, v.Level = AgentStale, msg.LevelFailed
	case v.Age >= StaleWarning:
		v.State, v.Level = AgentStale, msg.LevelWarning
	default:
		v.State, v.Level = AgentCapturing, msg.LevelOK
	}
	return v
}

// walPiece is what is stored of one WAL segment.
type walPiece struct {
	complete   bool
	partialEnd repo.LSN
	stored     time.Time
}

// PostgresWindow computes a Postgres target's window: the run of stored
// WAL segments that reaches the newest one without a missing segment or a
// recorded gap, from the earliest snapshot whose WAL lies inside it.
// Times come from snapshots and from when segments were stored.
func PostgresWindow(snaps []repo.Snapshot, segs []repo.WALSegment, gaps []repo.Gap) Window {
	size := uint64(defaultWALSegmentSize)
	for i := len(snaps) - 1; i >= 0; i-- {
		if snaps[i].WALSegmentSize > 0 {
			size = uint64(snaps[i].WALSegmentSize) // #nosec G115 -- a positive size
			break
		}
	}
	pieces := map[uint64]*walPiece{}
	for _, s := range segs {
		n, ok := segmentNumber(s.Name, size)
		if !ok {
			continue
		}
		p := pieces[n]
		if p == nil {
			p = &walPiece{}
			pieces[n] = p
		}
		if s.Partial {
			p.partialEnd = max(p.partialEnd, s.EndLSN)
		} else {
			p.complete = true
		}
		if s.Stored.After(p.stored) {
			p.stored = s.Stored
		}
	}
	w := Window{Logs: len(pieces) > 0}
	if !w.Logs {
		return w
	}
	nums := make([]uint64, 0, len(pieces))
	for n := range pieces {
		nums = append(nums, n)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	last := nums[len(nums)-1]
	end := (last + 1) * size
	if !pieces[last].complete {
		end = uint64(pieces[last].partialEnd)
	}
	w.End = pieces[last].stored
	// Walk back while each earlier segment is stored complete; below the
	// oldest stored segment is where capture began, not a break.
	first := last
	for first > nums[0] {
		p := pieces[first-1]
		if p != nil && p.complete {
			first--
			continue
		}
		if p == nil {
			w.Break = fmt.Sprintf("WAL segment %s is missing", segmentLabel(first-1, size))
		} else {
			w.Break = fmt.Sprintf("WAL segment %s is stored only up to %X/%X", segmentLabel(first-1, size), uint64(p.partialEnd)>>32, uint64(p.partialEnd)&0xFFFFFFFF)
		}
		w.BreakTime = breakTime(pieces, first)
		break
	}
	start := first * size
	// A recorded gap inside the run moves its start past the gap.
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
		if after > start && after <= end {
			start = after
			w.Break, w.BreakTime = g.Reason, g.Time
		}
	}
	// The earliest snapshot whose WAL lies inside the run starts the
	// window.
	for _, s := range snaps {
		sl, ok1 := lsnOf(s.StartPosition)
		el, ok2 := lsnOf(s.EndPosition)
		if !ok1 || !ok2 || uint64(sl) < start || uint64(el) > end {
			continue
		}
		if !w.PITR || s.End.Before(w.Start) {
			w.PITR, w.Start = true, s.End
		}
	}
	if w.PITR && w.End.Before(w.Start) {
		w.End = w.Start // stored before the snapshot finished: the snapshot itself is the newest point
	}
	return w
}

// breakTime is when the WAL before a missing segment was stored: the
// newest piece below n.
func breakTime(pieces map[uint64]*walPiece, n uint64) time.Time {
	var t time.Time
	for m, p := range pieces {
		if m < n && p.stored.After(t) {
			t = p.stored
		}
	}
	return t
}

// segmentLabel names a segment by its number, as its file name without
// the timeline.
func segmentLabel(n, size uint64) string {
	per := (1 << 32) / size
	return fmt.Sprintf("%08X%08X", n/per, n%per)
}

// segmentNumber reads a WAL file name's segment number for a segment size.
func segmentNumber(name string, size uint64) (uint64, bool) {
	if len(name) != 24 || size == 0 || size > 1<<32 {
		return 0, false
	}
	logID, err1 := strconv.ParseUint(name[8:16], 16, 32)
	segID, err2 := strconv.ParseUint(name[16:24], 16, 32)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return logID*((1<<32)/size) + segID, true
}

// lsnOf reads a snapshot's LSN, X/Y in hex.
func lsnOf(s string) (repo.LSN, bool) {
	hi, lo, ok := strings.Cut(s, "/")
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if !ok || err1 != nil || err2 != nil {
		return 0, false
	}
	return repo.LSN(h<<32 | l), true
}

// walPositionLSN reads a capture position, <timeline>/<LSN>.
func walPositionLSN(s string) (repo.LSN, bool) {
	_, lsn, ok := strings.Cut(s, "/")
	if !ok {
		return 0, false
	}
	return lsnOf(lsn)
}

// MongoWindow computes a MongoDB target's window: the oplog batches
// stored since the newest recorded gap, from the earliest snapshot whose
// end they continue from. Oplog timestamps carry their own time.
func MongoWindow(snaps []repo.Snapshot, batches []repo.OplogBatch, gaps []repo.Gap) Window {
	w := Window{Logs: len(batches) > 0}
	if !w.Logs {
		return w
	}
	sort.Slice(batches, func(i, j int) bool { return batches[i].First.Less(batches[j].First) })
	runStart := batches[0].First
	newest := batches[len(batches)-1].Last
	for _, b := range batches {
		if newest.Less(b.Last) {
			newest = b.Last
		}
	}
	// The run starts at the first batch after the newest gap.
	for _, g := range gaps {
		from, ok := oplogTS(g.From)
		if !ok {
			continue
		}
		for _, b := range batches {
			if from.Less(b.First) {
				if runStart.Less(b.First) {
					runStart = b.First
					w.Break, w.BreakTime = g.Reason, time.Unix(int64(from.T), 0).UTC()
				}
				break
			}
		}
	}
	w.End = time.Unix(int64(newest.T), 0).UTC()
	// Entries after a snapshot's end must be stored: the run starts at or
	// before the entry after it.
	for _, s := range snaps {
		end, ok := oplogTS(s.EndPosition)
		if !ok || end.Less(prevTS(runStart)) || newest.Less(end) {
			continue
		}
		if !w.PITR || s.End.Before(w.Start) {
			w.PITR, w.Start = true, s.End
		}
	}
	if w.PITR && w.End.Before(w.Start) {
		w.End = w.Start
	}
	return w
}

// prevTS is the timestamp just before ts: a run starting at ts continues
// from a snapshot ending just before it.
func prevTS(ts repo.OplogTS) repo.OplogTS {
	if ts.I > 0 {
		return repo.OplogTS{T: ts.T, I: ts.I - 1}
	}
	if ts.T > 0 {
		return repo.OplogTS{T: ts.T - 1, I: 1<<32 - 1}
	}
	return ts
}

// oplogTS reads a MongoDB position, <seconds>:<increment>.
func oplogTS(s string) (repo.OplogTS, bool) {
	t, i, ok := strings.Cut(s, ":")
	ts, err1 := strconv.ParseUint(t, 10, 32)
	inc, err2 := strconv.ParseUint(i, 10, 32)
	if !ok || err1 != nil || err2 != nil {
		return repo.OplogTS{}, false
	}
	return repo.OplogTS{T: uint32(ts), I: uint32(inc)}, true // #nosec G115 -- parsed as 32-bit numbers
}
