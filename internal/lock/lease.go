// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package lock

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// Lease defaults (PRD 9.1).
const (
	LeaseLength   = 2 * time.Minute
	LeaseRenew    = 30 * time.Second
	LeaseCheck    = 15 * time.Second
	LeaseStepDown = 90 * time.Second
	// ClaimWait is how long the fallback protocol waits between writing a
	// claim and reading the claims.
	ClaimWait = 10 * time.Second
)

// Clock is a monotonic clock. time.Now carries a monotonic reading, so
// Sub between two of its results is immune to wall-clock changes. Lease
// expiry is judged only on the candidate's own clock, never by comparing
// times across hosts.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock is the real clock.
var SystemClock Clock = systemClock{}

// LeaseTimings are the lease parameters.
type LeaseTimings struct {
	Length, Renew, Check, StepDown, ClaimWait time.Duration
}

// DefaultLeaseTimings are PRD 9.1's values. The 30-second gap between
// stepping down (90 s) and takeover (120 s) keeps two holders apart.
var DefaultLeaseTimings = LeaseTimings{
	Length: LeaseLength, Renew: LeaseRenew, Check: LeaseCheck, StepDown: LeaseStepDown, ClaimWait: ClaimWait,
}

// Lease is one agent's view of a target's capture lease (SDD 7.2). Drive
// it by calling Tick at least every Timings.Check; capture only while
// MayCapture is true, and write checkpoints with Generation.
//
// The lease for generation g is the object locks/lease-<target>/<g>. A
// candidate creates generation g+1 with conditional create, so exactly one
// candidate wins it; the holder renews by overwriting its record with a
// higher sequence. A candidate takes over only after the newest record has
// stayed unchanged for Length on its own clock, or at once when the holder
// released it. A holder that cannot renew for StepDown stops capturing
// before any candidate can take over.
type Lease struct {
	be      backend.Backend
	target  string
	holder  Holder
	clock   Clock
	timings LeaseTimings
	// bestEffort is set when the backend has no conditional create and the
	// lease uses the write-wait-list fallback.
	bestEffort bool

	held        bool
	gen         uint64
	seq         uint64
	lastRenewOK time.Time
	lastRenew   time.Time

	// Observation of another holder's record.
	obsKey   string
	obsSeq   uint64
	obsState string
	obsSince time.Time

	// Fallback claim in progress.
	claimKey   string
	claimGen   uint64
	claimSince time.Time
}

// NewLease returns a lease participant for a target.
func NewLease(be backend.Backend, target string, holder Holder, clock Clock, timings LeaseTimings) *Lease {
	if clock == nil {
		clock = SystemClock
	}
	return &Lease{
		be: be, target: target, holder: holder, clock: clock, timings: timings,
		bestEffort: !be.Capabilities().ConditionalCreate,
	}
}

func leasePrefix(target string) string { return locksPrefix + "lease-" + target + "/" }

func leaseKey(target string, gen uint64) string {
	return fmt.Sprintf("%s%020d", leasePrefix(target), gen)
}

// BestEffort reports whether the lease relies on the weaker fallback
// protocol, because the backend has no conditional create.
func (l *Lease) BestEffort() bool { return l.bestEffort }

// Generation is the generation this agent holds, or 0.
func (l *Lease) Generation() uint64 {
	if !l.held {
		return 0
	}
	return l.gen
}

// MayCapture reports whether this agent holds the lease and has renewed it
// within StepDown, so it may capture and write checkpoints now.
func (l *Lease) MayCapture() bool {
	return l.held && l.clock.Now().Sub(l.lastRenewOK) < l.timings.StepDown
}

// Tick advances the lease: a holder renews when due, or steps down after
// StepDown without a successful renewal; a candidate watches the newest
// record and takes over when it expires or is released.
func (l *Lease) Tick(ctx context.Context) error {
	now := l.clock.Now()
	if l.held {
		if now.Sub(l.lastRenewOK) >= l.timings.StepDown {
			l.stepDown()
			return nil
		}
		if now.Sub(l.lastRenew) < l.timings.Renew {
			return nil
		}
		return l.renew(ctx, now)
	}
	if l.claimKey != "" {
		return l.resolveClaim(ctx, now)
	}
	return l.watch(ctx, now)
}

func (l *Lease) stepDown() {
	l.held = false
	l.obsKey, l.obsSince = "", time.Time{}
}

// renew overwrites the holder's record with the next sequence, after
// checking no newer generation exists.
func (l *Lease) renew(ctx context.Context, now time.Time) error {
	l.lastRenew = now
	newest, _, err := l.newest(ctx)
	if err != nil {
		return err
	}
	if newest > l.gen {
		l.stepDown() // another agent took over
		return nil
	}
	rec := l.record(StateHeld)
	rec.Seq = l.seq + 1
	if err := putRecord(ctx, l.be, leaseKey(l.target, l.gen), rec, false); err != nil {
		return err
	}
	l.seq = rec.Seq
	l.lastRenewOK = now
	return nil
}

func (l *Lease) record(state string) Record {
	return Record{
		Version: recordVersion, Kind: KindLease, ID: l.target, Generation: l.gen, Seq: l.seq,
		State: state, Holder: l.holder, Time: time.Now().UTC(),
	}
}

// watch observes the newest record and takes over when it is released or
// has stayed unchanged for Length.
func (l *Lease) watch(ctx context.Context, now time.Time) error {
	gen, key, err := l.newest(ctx)
	if err != nil {
		return err
	}
	if key == "" {
		return l.acquire(ctx, now, 1)
	}
	rec, err := getRecord(ctx, l.be, key)
	if errors.Is(err, backend.ErrNotFound) {
		return l.acquire(ctx, now, gen+1) // released and deleted
	}
	if err != nil {
		return err
	}
	if rec.State == StateReleased {
		return l.acquire(ctx, now, gen+1)
	}
	if key != l.obsKey || rec.Seq != l.obsSeq || rec.State != l.obsState {
		l.obsKey, l.obsSeq, l.obsState, l.obsSince = key, rec.Seq, rec.State, now
		return nil
	}
	if now.Sub(l.obsSince) >= l.timings.Length {
		return l.acquire(ctx, now, gen+1)
	}
	return nil
}

// acquire tries to create generation gen.
func (l *Lease) acquire(ctx context.Context, now time.Time, gen uint64) error {
	if l.bestEffort {
		return l.claim(ctx, now, gen)
	}
	l.gen, l.seq = gen, 0
	err := putRecord(ctx, l.be, leaseKey(l.target, gen), l.record(StateHeld), true)
	if errors.Is(err, backend.ErrExists) {
		l.obsKey, l.obsSince = "", time.Time{} // another candidate won
		return nil
	}
	if err != nil {
		return err
	}
	l.won(now)
	return nil
}

func (l *Lease) won(now time.Time) {
	l.held = true
	l.lastRenewOK, l.lastRenew = now, now
	l.obsKey, l.obsSince = "", time.Time{}
}

// claim starts the write-wait-list fallback: write a uniquely named claim
// for gen, wait ClaimWait, then the lowest claim wins.
func (l *Lease) claim(ctx context.Context, now time.Time, gen uint64) error {
	id := randomID()
	key := fmt.Sprintf("%s%020d-claim-%s", leasePrefix(l.target), gen, id)
	rec := l.record(StateHeld)
	rec.Kind, rec.Generation = KindClaim, gen
	if err := putRecord(ctx, l.be, key, rec, false); err != nil {
		return err
	}
	l.claimKey, l.claimGen, l.claimSince = key, gen, now
	return nil
}

func (l *Lease) resolveClaim(ctx context.Context, now time.Time) error {
	if now.Sub(l.claimSince) < l.timings.ClaimWait {
		return nil
	}
	prefix := fmt.Sprintf("%s%020d-claim-", leasePrefix(l.target), l.claimGen)
	var claims []string
	if err := l.be.List(ctx, prefix, func(oi backend.ObjectInfo) error {
		claims = append(claims, oi.Key)
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(claims)
	mine, gen := l.claimKey, l.claimGen
	l.claimKey = ""
	if len(claims) == 0 || claims[0] != mine {
		l.obsKey, l.obsSince = "", time.Time{}
		return nil // another claim is lower
	}
	// Make sure no generation record appeared meanwhile.
	if newest, _, err := l.newest(ctx); err != nil || newest >= gen {
		return err
	}
	l.gen, l.seq = gen, 0
	if err := putRecord(ctx, l.be, leaseKey(l.target, gen), l.record(StateHeld), false); err != nil {
		return err
	}
	l.won(now)
	return nil
}

// newest returns the highest generation's record key, or "" if there is
// none.
func (l *Lease) newest(ctx context.Context) (uint64, string, error) {
	return newestGeneration(ctx, l.be, l.target)
}

func newestGeneration(ctx context.Context, be backend.Backend, target string) (uint64, string, error) {
	prefix := leasePrefix(target)
	var gen uint64
	var key string
	err := be.List(ctx, prefix, func(oi backend.ObjectInfo) error {
		name := strings.TrimPrefix(oi.Key, prefix)
		if len(name) != 20 { // claims and anything else
			return nil
		}
		g, err := strconv.ParseUint(name, 10, 64)
		if err != nil {
			return nil
		}
		if g >= gen {
			gen, key = g, oi.Key
		}
		return nil
	})
	return gen, key, err
}

// CurrentGeneration returns the newest lease generation of a target, or 0
// when none exists. Checkpoint writers compare their generation with it.
func CurrentGeneration(ctx context.Context, be backend.Backend, target string) (uint64, error) {
	g, _, err := newestGeneration(ctx, be, target)
	return g, err
}

// Release gives up the lease (graceful shutdown or catch-up exit): the
// record is overwritten as released, so a standby takes over at its next
// check. The record is never deleted: the newest generation must survive,
// or the next holder would start again at generation 1 and fencing would
// fail. Prune may remove older generations' records, never the newest.
func (l *Lease) Release(ctx context.Context) error {
	if !l.held {
		return nil
	}
	rec := l.record(StateReleased)
	rec.Seq = l.seq + 1
	l.held = false
	return putRecord(ctx, l.be, leaseKey(l.target, l.gen), rec, false)
}
