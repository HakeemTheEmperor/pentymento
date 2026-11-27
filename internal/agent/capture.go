// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// Monitoring defaults (PRD 9.1). The configuration keys come with
// notifications.
const (
	// HealthInterval is how often capture checks what the server retains.
	HealthInterval = 30 * time.Second
	// Capture lag thresholds.
	LagWarning  = 5 * time.Minute
	LagCritical = 15 * time.Minute
	// Retained-WAL thresholds when max_slot_wal_keep_size is set, as
	// fractions of it, and when it is not.
	RetainedWarningShare  = 0.5
	RetainedCriticalShare = 0.8
	RetainedWarning       = 10 << 30
	RetainedCritical      = 50 << 30
	// Oplog window thresholds; the window is also critical when capture
	// lags more than half of it.
	WindowWarning  = 24 * time.Hour
	WindowCritical = 6 * time.Hour
)

// Alert levels.
const (
	levelOK = iota
	levelWarning
	levelCritical
)

// captureJob is what the metrics and journal call capture.
const captureJob = "capture"

// captureFor returns the engine-backed capture function (SDD 6.2): it
// resumes from the target's checkpoint, records and reports any gap and
// forces a base backup after it, stores what the engine captures, writes
// a checkpoint with the lease generation after each stored segment, and
// watches what the server retains.
func (a *Agent) captureFor(ctx context.Context, t Target) error {
	log := a.log.With("target", t.Name)
	eng, err := a.o.Engine(t)
	if err != nil {
		return err
	}
	et := engine.Target{Name: t.Name, ID: t.ID, Config: t.Config}
	cp, err := a.o.Repo.LatestCheckpoint(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("read the checkpoint: %w", err)
	}
	s := &segmentSink{a: a, t: t, log: log}
	var from engine.Position
	if cp != nil {
		from = engine.Position(cp.Position)
		if cp.Generation == t.Generation {
			s.seq = cp.Seq // this lease's capture resumes: keep counting
		}
	}
	for attempt := 0; ; attempt++ {
		gap, err := eng.DetectGap(ctx, et, from)
		if errors.Is(err, errors.ErrUnsupported) {
			return a.notCapturing(ctx, log, err)
		}
		if err != nil {
			return fmt.Errorf("check for a gap: %w", err)
		}
		if gap != nil {
			a.recordGap(ctx, t, gap, log)
			a.forceBackup(t, log, "a capture gap: "+gap.Reason)
			from = "" // capture starts again where the server stands
		} else if from == "" && a.firstStart(t.Name) {
			// Nothing captured can be replayed onto the snapshots taken
			// before now: a base backup starts recovery from here. Once per
			// target and process, so a capture that keeps failing before
			// its first checkpoint does not take backup after backup.
			a.forceBackup(t, log, "capture is starting without a checkpoint to continue from")
		}
		mctx, stop := context.WithCancel(ctx)
		monitorDone := make(chan struct{})
		go func() { defer close(monitorDone); a.monitor(mctx, eng, et, s, log) }()
		err = eng.Capture(ctx, et, from, s)
		stop()
		<-monitorDone
		switch {
		case errors.Is(err, errors.ErrUnsupported):
			return a.notCapturing(ctx, log, err)
		case errors.Is(err, engine.ErrGap) && attempt == 0:
			// The log moved on while capture was starting: detect the gap
			// again and record it.
			continue
		case err != nil && ctx.Err() == nil:
			a.o.Telemetry.Metrics.JobFailures.WithLabelValues(t.Name, captureJob).Inc()
		}
		return err
	}
}

// notCapturing holds the lease without capturing, for an engine that
// does not capture yet, and says so once.
func (a *Agent) notCapturing(ctx context.Context, log *slog.Logger, err error) error {
	log.Info("not capturing the log; holding the lease only", "reason", err.Error())
	<-ctx.Done()
	return nil
}

// recordGap records a gap in the repository and the journal (PRD 7.3);
// the caller forces a base backup, or recommends one.
func (a *Agent) recordGap(ctx context.Context, t Target, gap *engine.Gap, log *slog.Logger) {
	log.Error("capture gap: the log since the checkpoint cannot be captured; point-in-time recovery cannot reach inside it",
		"from", gap.From, "to", gap.To, "reason", gap.Reason)
	a.o.Telemetry.Metrics.JobFailures.WithLabelValues(t.Name, captureJob).Inc()
	g := repo.Gap{Target: t.ID, From: string(gap.From), To: string(gap.To), Reason: gap.Reason, Time: time.Now(), Generation: t.Generation}
	if err := a.o.Repo.PutGap(ctx, g); err != nil {
		log.Error("could not record the gap", "error", err)
	}
	op := a.o.Repo.BeginOperation(repo.CurrentIdentity(), captureJob, t.ID, "", t.Config.Credentials.CaptureCredential().Password)
	_ = op.Finish(context.WithoutCancel(ctx), repo.ResultOK, errors.New("capture gap: "+gap.Reason))
}

// firstStart reports whether capture of a target starts without a
// checkpoint for the first time in this process.
func (a *Agent) firstStart(target string) bool {
	a.forcedMu.Lock()
	defer a.forcedMu.Unlock()
	if a.started0[target] {
		return false
	}
	a.started0[target] = true
	return true
}

// forceBackup takes a base backup of the target in the background, unless
// one it forced is still running.
func (a *Agent) forceBackup(t Target, log *slog.Logger, why string) {
	if a.o.Backup == nil {
		log.Warn("a base backup is needed, but this agent cannot take one", "why", why)
		return
	}
	a.forcedMu.Lock()
	if a.forced[t.Name] {
		a.forcedMu.Unlock()
		return
	}
	a.forced[t.Name] = true
	a.forcedMu.Unlock()
	log.Warn("taking a base backup", "why", why)
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		defer func() {
			a.forcedMu.Lock()
			delete(a.forced, t.Name)
			a.forcedMu.Unlock()
		}()
		if _, err := a.o.Backup(a.bgCtx, t); err != nil && a.bgCtx.Err() == nil {
			log.Error("the forced base backup failed", "error", err)
		}
	}()
}

// segmentSink stores what capture produces and checkpoints it.
type segmentSink struct {
	a   *Agent
	t   Target
	log *slog.Logger

	// checkpointed, if set, is told each position checkpointed.
	checkpointed func(engine.Position)

	mu        sync.Mutex
	seq       uint64
	position  engine.Position
	lastStore time.Time
	stored    atomic.Int64
	segments  atomic.Int64
}

func (s *segmentSink) Segment(ctx context.Context, seg engine.Segment) error {
	r := s.a.o.Repo
	tl, err := strconv.ParseUint(seg.Timeline, 10, 32)
	if err != nil && seg.Kind != "oplog" {
		return fmt.Errorf("segment %s: timeline %q: %w", seg.Name, seg.Timeline, err)
	}
	timeline := repo.Timeline(tl) // #nosec G115 -- parsed as a 32-bit number
	put := func() error {
		switch seg.Kind {
		case "wal":
			return r.PutWALSegment(ctx, s.t.ID, timeline, seg.Name, seg.Data)
		case "wal-partial":
			end, err := parseLSN(string(seg.Last))
			if err != nil {
				return err
			}
			return r.PutWALPartial(ctx, s.t.ID, timeline, seg.Name, end, seg.Data)
		case "history":
			return r.PutTimelineHistory(ctx, s.t.ID, timeline, seg.Data)
		case "oplog":
			first, err := parseOplogTS(string(seg.First))
			if err != nil {
				return err
			}
			last, err := parseOplogTS(string(seg.Last))
			if err != nil {
				return err
			}
			return r.PutOplogBatch(ctx, s.t.ID, first, last, seg.Data)
		}
		return fmt.Errorf("segment kind %q is not stored by this build", seg.Kind)
	}
	if err := s.a.uploads.Do(ctx, put); err != nil {
		name := seg.Name
		if name == "" {
			name = "the " + seg.Kind + " batch " + string(seg.First) + " to " + string(seg.Last)
		}
		return fmt.Errorf("store %s: %w", name, err)
	}
	s.stored.Add(int64(len(seg.Data)))
	s.segments.Add(1)
	if seg.Checkpoint == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if err := r.PutCheckpoint(ctx, repo.Checkpoint{
		Target: s.t.ID, Generation: s.t.Generation, Seq: s.seq, Position: string(seg.Checkpoint), Time: time.Now(),
	}); err != nil {
		return fmt.Errorf("checkpoint %s: %w", seg.Checkpoint, err)
	}
	s.position, s.lastStore = seg.Checkpoint, time.Now()
	if s.checkpointed != nil {
		s.checkpointed(seg.Checkpoint)
	}
	return nil
}

func (s *segmentSink) Event(ctx context.Context, e engine.Event) error {
	return s.a.o.Repo.PutEvent(ctx, repo.Event{
		Target: s.t.ID, Type: e.Type, Object: e.Object, XID: e.XID, Position: string(e.Position), Time: e.Time,
	})
}

// stored reports the last checkpointed position and when it was stored.
func (s *segmentSink) last() (engine.Position, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.position, s.lastStore
}

// parseLSN reads a Postgres LSN written as X/Y in hex.
func parseLSN(s string) (repo.LSN, error) {
	hi, lo, ok := strings.Cut(s, "/")
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if !ok || err1 != nil || err2 != nil {
		return 0, fmt.Errorf("LSN %q is not understood", s)
	}
	return repo.LSN(h<<32 | l), nil
}

// parseOplogTS reads a MongoDB oplog position written as
// <seconds>:<increment>.
func parseOplogTS(s string) (repo.OplogTS, error) {
	t, i, ok := strings.Cut(s, ":")
	ts, err1 := strconv.ParseUint(t, 10, 32)
	inc, err2 := strconv.ParseUint(i, 10, 32)
	if !ok || err1 != nil || err2 != nil {
		return repo.OplogTS{}, fmt.Errorf("oplog position %q is not understood", s)
	}
	return repo.OplogTS{T: uint32(ts), I: uint32(inc)}, nil // #nosec G115 -- parsed as 32-bit numbers
}

// monitor checks what the server retains and how far capture lags, every
// HealthInterval, against the thresholds, logging when a condition starts,
// changes level or clears (alert delivery comes with notifications).
func (a *Agent) monitor(ctx context.Context, eng engine.Engine, et engine.Target, s *segmentSink, log *slog.Logger) {
	m := a.o.Telemetry.Metrics
	started := time.Now()
	var lagLevel, retainedLevel, windowLevel int
	tick := time.NewTicker(a.o.HealthInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		h, err := eng.LogHealth(ctx, et)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("could not check what the server retains", "error", err)
			}
			continue
		}
		pos, at := s.last()
		if at.IsZero() {
			at = started
		}
		// Caught up when the newest stored position is the server's head;
		// otherwise the lag is the time since capture last stored WAL.
		var lag time.Duration
		if pos != h.Head {
			lag = time.Since(at)
		}
		m.CaptureLag.WithLabelValues(et.Name).Set(lag.Seconds())
		lc := LiveCapture{Position: string(pos), Head: string(h.Head), LagSeconds: lag.Seconds(), Checked: time.Now().UTC()}
		if h.Oplog {
			lc.OplogWindowSeconds = h.Window.Seconds()
		} else {
			lc.RetainedBytes, lc.RetainedLimit = h.RetainedBytes, h.RetainedLimit
		}
		a.liveMu.Lock()
		a.live[et.Name] = lc
		a.liveMu.Unlock()
		lagLevel = report(log, lagLevel, lagAlert(lag), "capture lag", "lag", lag.Round(time.Second).String())
		if h.Oplog {
			m.MongoOplogWindow.WithLabelValues(et.Name).Set(h.Window.Seconds())
			lvl, advice := windowAlert(h.Window, lag)
			windowLevel = report(log, windowLevel, lvl, "the oplog window",
				"window", h.Window.Round(time.Second).String(), "lag", lag.Round(time.Second).String(), "advice", advice)
			continue
		}
		m.PGSlotRetainedWAL.WithLabelValues(et.Name).Set(float64(h.RetainedBytes))
		lvl, advice := retainedAlert(h)
		retainedLevel = report(log, retainedLevel, lvl, "WAL retained by the replication slot",
			"retained_bytes", h.RetainedBytes, "limit_bytes", h.RetainedLimit, "advice", advice)
	}
}

// report logs a condition when its level changes and returns the new
// level.
func report(log *slog.Logger, was, now int, what string, args ...any) int {
	if now == was {
		return now
	}
	switch now {
	case levelCritical:
		log.Error(what+" is critical", args...)
	case levelWarning:
		log.Warn(what+" is above the warning threshold", args...)
	default:
		log.Info(what+" is back to normal", args...)
	}
	return now
}

// lagAlert classifies capture lag (PRD 9.1).
func lagAlert(lag time.Duration) int {
	switch {
	case lag >= LagCritical:
		return levelCritical
	case lag >= LagWarning:
		return levelWarning
	}
	return levelOK
}

// retainedAlert classifies the WAL a slot retains (PRD 9.1): against
// max_slot_wal_keep_size when it is set, and against fixed sizes with the
// advice to set it when it is not. A lost slot is critical.
func retainedAlert(h *engine.LogHealth) (int, string) {
	switch {
	case h.Lost:
		return levelCritical, "the slot was invalidated; capture will record the gap and take a new base backup"
	case h.RetainedLimit > 0:
		share := float64(h.RetainedBytes) / float64(h.RetainedLimit)
		switch {
		case share >= RetainedCriticalShare:
			return levelCritical, "the server invalidates the slot at max_slot_wal_keep_size; check that capture keeps up"
		case share >= RetainedWarningShare:
			return levelWarning, "check that capture keeps up before the slot reaches max_slot_wal_keep_size"
		}
	case h.RetainedBytes >= RetainedCritical:
		return levelCritical, "set max_slot_wal_keep_size so a stopped agent cannot fill the server's disk"
	case h.RetainedBytes >= RetainedWarning:
		return levelWarning, "set max_slot_wal_keep_size so a stopped agent cannot fill the server's disk"
	}
	return levelOK, ""
}

// windowAlert classifies the oplog window (PRD 9.1): how far back the
// oplog reaches bounds how long capture can stop before entries are lost.
func windowAlert(window, lag time.Duration) (int, string) {
	switch {
	case window < WindowCritical:
		return levelCritical, "enlarge the oplog (replSetResizeOplog) so a stopped agent can catch up before entries are overwritten"
	case lag > window/2:
		return levelCritical, "capture lags more than half the oplog window; check that the agent keeps up before the oplog rolls over"
	case window < WindowWarning:
		return levelWarning, "enlarge the oplog (replSetResizeOplog) so it holds at least a day"
	}
	return levelOK, ""
}
