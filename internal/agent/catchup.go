// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// Message IDs for catch-up (see docs/errors).
const (
	// IDGap is a stretch of log that can no longer be captured.
	IDGap msg.ID = "PTM-5001"
	// IDLeaseHeld is a capture lease another process keeps renewing.
	IDLeaseHeld msg.ID = "PTM-8001"
)

// catchUpJob is what the journal calls catch-up.
const catchUpJob = "catch-up"

// CatchUpResult is what a catch-up did.
type CatchUpResult struct {
	Target string
	// Generation is the lease generation catch-up held.
	Generation uint64
	// From is the checkpoint catch-up started from (empty if there was
	// none); To is the checkpoint it left; Head is where the server stood
	// when it started, which it drained to.
	From, To, Head engine.Position
	// Segments and Bytes are what it stored.
	Segments, Bytes int64
	// Gap is the gap it found and recorded, if any.
	Gap *engine.Gap
	// LeaseWait is how long it waited for an expired lease.
	LeaseWait time.Duration
	Duration  time.Duration
}

// CatchUpProgress receives catch-up's progress: a phase ("waiting" for
// a lease that has not expired yet, "draining") and, while draining, the
// bytes stored so far.
type CatchUpProgress func(phase string, stored int64)

// CatchUp drains the log a target's server still retains while no agent
// captures it (SDD 6.2, 7.3): it takes the target's capture lease, only
// once it has expired or was released, stores the log from the checkpoint
// up to where the server stood when it started, checkpoints, records any
// gap, releases the lease and returns. It never takes a base backup: after
// a gap it recommends one. o.Engine must be set; o.Backup is not used.
func CatchUp(ctx context.Context, o Options, name string, progress CatchUpProgress) (*CatchUpResult, error) {
	started := time.Now()
	if progress == nil {
		progress = func(string, int64) {}
	}
	if o.Engine == nil {
		return nil, errors.New("catch-up needs an engine")
	}
	o.Backup = nil
	a, err := newAgent(o)
	if err != nil {
		return nil, err
	}
	c, ok := a.o.Config.Targets[name]
	if !ok {
		return nil, fmt.Errorf("no target %s in the config", name)
	}
	tid, err := a.o.Repo.TargetID(name)
	if err != nil {
		return nil, err
	}
	t := Target{Name: name, ID: tid, Config: c}
	log := a.log.With("target", name, "job", catchUpJob)
	op := a.o.Repo.BeginOperation(repo.CurrentIdentity(), catchUpJob, tid, "", c.Credentials.CaptureCredential().Password)
	res := &CatchUpResult{Target: name}
	finish := func(err error) (*CatchUpResult, error) {
		res.Duration = time.Since(started)
		result := repo.ResultOK
		switch {
		case err != nil:
			result = repo.ResultFailed
		case res.Gap != nil:
			err = errors.New("capture gap: " + res.Gap.Reason)
		}
		_ = op.Finish(context.WithoutCancel(ctx), result, err)
		if res.Gap != nil && result == repo.ResultOK {
			err = nil
		}
		return res, err
	}

	// Take the lease, once it has expired or was released.
	lease := lock.NewLease(a.o.Repo.Backend(), tid, a.holder, a.o.Clock, a.o.Timings)
	waitStart := time.Now()
	if err := a.acquire(ctx, lease, t, progress); err != nil {
		return finish(err)
	}
	res.LeaseWait = time.Since(waitStart)
	res.Generation = lease.Generation()
	t.Generation = res.Generation
	log.Info("holding the capture lease for catch-up", "generation", res.Generation)
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := lease.Release(rctx); err != nil {
			log.Warn("could not release the capture lease; it expires by itself", "error", err)
		}
	}()

	// Keep the lease while draining; lose it and the drain stops.
	dctx, stopDrain := context.WithCancelCause(ctx)
	defer stopDrain(nil)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		tick := time.NewTicker(a.o.Timings.Check)
		defer tick.Stop()
		for {
			select {
			case <-dctx.Done():
				return
			case <-tick.C:
			}
			if err := lease.Tick(dctx); err != nil && dctx.Err() == nil {
				log.Warn("the capture lease could not be renewed", "error", err)
			}
			if !lease.MayCapture() {
				stopDrain(errors.New("lost the capture lease while catching up"))
				return
			}
		}
	}()
	err = a.drain(dctx, t, res, progress, log)
	stopDrain(nil)
	<-renewDone
	if cause := context.Cause(dctx); err == nil && cause != nil && !errors.Is(cause, context.Canceled) {
		err = cause
	}
	return finish(err)
}

// acquire waits for the lease: at once when no one holds it or it was
// released, after Length when its record stays unchanged. A holder that
// keeps renewing is alive, so catch-up refuses.
func (a *Agent) acquire(ctx context.Context, lease *lock.Lease, t Target, progress CatchUpProgress) error {
	tm := a.o.Timings
	deadline := time.Now().Add(tm.Length + tm.ClaimWait + 2*tm.Check)
	for tick := 0; ; tick++ {
		if tick == 1 {
			progress("waiting", 0)
		}
		if err := lease.Tick(ctx); err != nil {
			return fmt.Errorf("check the capture lease: %w", err)
		}
		if lease.MayCapture() {
			return nil
		}
		if time.Now().After(deadline) {
			return &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
				Level: msg.LevelFailed, ID: IDLeaseHeld, Target: t.Name,
				Fact:   "an agent is capturing this target: its capture lease was renewed while catch-up waited " + (tm.Length + tm.ClaimWait + 2*tm.Check).String(),
				Impact: "catch-up runs only when no agent captures, so two never write at once; nothing was stored",
				Next:   "pentym status",
			}}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(tm.Check, time.Until(deadline)+time.Millisecond)):
		}
	}
}

// drain stores the log from the checkpoint up to where the server stands
// now, recording a gap if the log since the checkpoint is gone.
func (a *Agent) drain(ctx context.Context, t Target, res *CatchUpResult, progress CatchUpProgress, log interface {
	Info(string, ...any)
	Warn(string, ...any)
	Error(string, ...any)
}) error {
	eng, err := a.o.Engine(t)
	if err != nil {
		return err
	}
	et := engine.Target{Name: t.Name, ID: t.ID, Config: t.Config}
	cp, err := a.o.Repo.LatestCheckpoint(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("read the checkpoint: %w", err)
	}
	if cp != nil {
		res.From = engine.Position(cp.Position)
	}
	res.To = res.From
	h, err := eng.LogHealth(ctx, et)
	if err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			return unsupportedCapture(t, err)
		}
		return fmt.Errorf("read where the server stands: %w", err)
	}
	res.Head = h.Head
	from := res.From
	for attempt := 0; ; attempt++ {
		gap, err := eng.DetectGap(ctx, et, from)
		if errors.Is(err, errors.ErrUnsupported) {
			return unsupportedCapture(t, err)
		}
		if err != nil {
			return fmt.Errorf("check for a gap: %w", err)
		}
		if gap != nil {
			a.recordGap(ctx, t, gap, a.log.With("target", t.Name))
			res.Gap, from = gap, ""
		}
		if from != "" {
			if done, err := reached(from, res.Head); err != nil {
				return err
			} else if done {
				return nil // nothing new since the checkpoint
			}
		}
		progress("draining", 0)
		cctx, stop := context.WithCancel(ctx)
		s := &segmentSink{a: a, t: t, log: a.log.With("target", t.Name)}
		s.checkpointed = func(p engine.Position) {
			progress("draining", s.stored.Load())
			if done, err := reached(p, res.Head); err == nil && done {
				stop()
			}
		}
		err = eng.Capture(cctx, et, from, s)
		reachedHead := cctx.Err() != nil && ctx.Err() == nil
		stop()
		if reachedHead && errors.Is(err, context.Canceled) {
			err = nil // stopped at the head: a store cut short there is not needed
		}
		res.Segments += s.segments.Load()
		res.Bytes += s.stored.Load()
		if p, _ := s.last(); p != "" {
			res.To = p
		}
		switch {
		case errors.Is(err, engine.ErrGap) && attempt == 0 && res.Gap == nil:
			continue // the log moved on while capture was starting
		case err != nil:
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case !reachedHead:
			return errors.New("capture ended before reaching " + string(res.Head))
		}
		return nil
	}
}

// unsupportedCapture explains a target whose log cannot be captured.
func unsupportedCapture(t Target, err error) error {
	return &msg.Error{Code: msg.ExitNotImplemented, Err: err, Message: msg.Message{
		Level: msg.LevelFailed, Target: t.Name, Fact: "there is no log to catch up: " + err.Error(),
		Next: "pentym backup " + t.Name,
	}}
}

// reached reports whether a captured position is at or past a target
// position. Positions are a Postgres <timeline>/<LSN> or a MongoDB
// <seconds>:<increment>.
func reached(pos, target engine.Position) (bool, error) {
	if strings.Contains(string(pos), ":") || strings.Contains(string(target), ":") {
		p, err1 := parseOplogTS(string(pos))
		t, err2 := parseOplogTS(string(target))
		if err1 != nil || err2 != nil {
			return false, fmt.Errorf("compare positions %q and %q: %w", pos, target, errors.Join(err1, err2))
		}
		return !p.Less(t), nil
	}
	ptl, plsn, err1 := parseWALPosition(string(pos))
	ttl, tlsn, err2 := parseWALPosition(string(target))
	if err1 != nil || err2 != nil {
		return false, fmt.Errorf("compare positions %q and %q: %w", pos, target, errors.Join(err1, err2))
	}
	return ptl > ttl || (ptl == ttl && plsn >= tlsn), nil
}

// parseWALPosition reads a Postgres capture position, <timeline>/<LSN>.
func parseWALPosition(s string) (uint64, repo.LSN, error) {
	tl, lsn, ok := strings.Cut(s, "/")
	n, err := strconv.ParseUint(tl, 10, 32)
	if !ok || err != nil {
		return 0, 0, fmt.Errorf("WAL position %q is not understood", s)
	}
	l, err := parseLSN(lsn)
	return n, l, err
}
