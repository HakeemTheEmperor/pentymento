// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package lock

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// Repository-lock defaults (PRD 9.1).
const (
	LockRefresh    = 5 * time.Minute
	LockStaleAfter = 30 * time.Minute
)

// IDLocked is the message for an operation blocked by another's lock.
const IDLocked msg.ID = "PTM-3004"

// ErrBlocked means a conflicting lock is held. Errors returned for it are
// *msg.Error values that wrap it.
var ErrBlocked = errors.New("repository lock held by another operation")

// Locks takes the repository's shared and exclusive locks (SDD 7.1).
// Snapshot backups and check hold a shared lock; garbage collection, index
// rebuilds, key rotation and format upgrades hold the exclusive lock. Log
// segments and prune's forget phase take no lock.
//
// Both kinds use write-then-check: a newcomer writes its lock, then lists
// the others, and releases its own and backs off if a conflicting live lock
// exists. A shared lock conflicts with any exclusive lock; an exclusive
// lock conflicts with every other lock. Because each newcomer checks only
// after its own record is visible, two conflicting newcomers can never
// both miss each other: with a strongly consistent backend, conflicting
// holders never proceed together, and at worst both back off and retry.
type Locks struct {
	be     backend.Backend
	holder Holder
	// StaleAfter is how long, in storage time, an unrefreshed lock stays
	// live; zero means LockStaleAfter.
	StaleAfter time.Duration
}

// NewLocks returns a lock manager for a holder.
func NewLocks(be backend.Backend, holder Holder) *Locks { return &Locks{be: be, holder: holder} }

func (m *Locks) staleAfter() time.Duration {
	if m.StaleAfter > 0 {
		return m.StaleAfter
	}
	return LockStaleAfter
}

func lockKey(kind, id string) string { return locksPrefix + kind + "-" + id }

// Lock is a held repository lock.
type Lock struct {
	m   *Locks
	key string
	rec Record
	mu  sync.Mutex
}

// Kind returns KindShared or KindExclusive.
func (l *Lock) Kind() string { return l.rec.Kind }

// AcquireShared takes a shared lock, for a snapshot backup. It fails with
// ErrBlocked while garbage collection holds the exclusive lock.
func (m *Locks) AcquireShared(ctx context.Context) (*Lock, error) {
	return m.acquire(ctx, KindShared)
}

// AcquireExclusive takes the exclusive lock, for garbage collection or an
// index rebuild. It fails with ErrBlocked while any backup holds a shared
// lock or another holder has the exclusive lock.
func (m *Locks) AcquireExclusive(ctx context.Context) (*Lock, error) {
	return m.acquire(ctx, KindExclusive)
}

func (m *Locks) acquire(ctx context.Context, kind string) (*Lock, error) {
	id := randomID()
	l := &Lock{m: m, key: lockKey(kind, id), rec: Record{
		Version: recordVersion, Kind: kind, ID: id, State: StateHeld, Holder: m.holder, Time: time.Now().UTC(),
	}}
	if err := putRecord(ctx, m.be, l.key, l.rec, true); err != nil {
		return nil, err
	}
	conflicts, err := m.conflicts(ctx, l)
	if err != nil {
		_ = l.Release(context.WithoutCancel(ctx))
		return nil, err
	}
	if len(conflicts) > 0 {
		_ = l.Release(context.WithoutCancel(ctx))
		return nil, blocked(kind, conflicts[0])
	}
	return l, nil
}

// conflicts returns the live locks that stop l: for a shared lock, any
// exclusive lock; for an exclusive lock, any other lock at all. (An
// earlier version let the lower ID win between exclusive locks, which let
// a newcomer proceed beside an exclusive lock already held.)
func (m *Locks) conflicts(ctx context.Context, l *Lock) ([]Record, error) {
	live, err := m.Live(ctx)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, r := range live {
		if r.ID == l.rec.ID {
			continue
		}
		if l.rec.Kind == KindExclusive || r.Kind == KindExclusive {
			out = append(out, r)
		}
	}
	return out, nil
}

// Live returns the shared and exclusive locks that are held and not stale.
func (m *Locks) Live(ctx context.Context) ([]Record, error) {
	recs, err := m.records(ctx)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	now, err := storageNow(ctx, m.be)
	if err != nil {
		return nil, err
	}
	var live []Record
	for _, r := range recs {
		if r.rec.State == StateHeld && now.Sub(r.mod) <= m.staleAfter() {
			live = append(live, r.rec)
		}
	}
	return live, nil
}

type storedRecord struct {
	key string
	rec Record
	mod time.Time
}

// records reads every shared and exclusive lock record.
func (m *Locks) records(ctx context.Context) ([]storedRecord, error) {
	var infos []backend.ObjectInfo
	for _, kind := range []string{KindShared, KindExclusive} {
		err := m.be.List(ctx, locksPrefix+kind+"-", func(oi backend.ObjectInfo) error {
			infos = append(infos, oi)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var out []storedRecord
	for _, oi := range infos {
		rec, err := getRecord(ctx, m.be, oi.Key)
		if errors.Is(err, backend.ErrNotFound) {
			continue // released and deleted meanwhile
		}
		if err != nil {
			// An unreadable record still counts, until it is stale.
			rec = Record{Kind: kindOf(oi.Key), ID: strings.TrimPrefix(oi.Key, locksPrefix+kindOf(oi.Key)+"-"), State: StateHeld}
		}
		out = append(out, storedRecord{key: oi.Key, rec: rec, mod: oi.ModTime})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

func kindOf(key string) string {
	if strings.HasPrefix(key, locksPrefix+KindExclusive+"-") {
		return KindExclusive
	}
	return KindShared
}

// Refresh rewrites the lock so it stays live. Holders refresh every
// LockRefresh.
func (l *Lock) Refresh(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rec.Seq++
	l.rec.Time = time.Now().UTC()
	return putRecord(ctx, l.m.be, l.key, l.rec, false)
}

// KeepFresh refreshes the lock every interval until the returned stop
// function is called.
func (l *Lock) KeepFresh(ctx context.Context, interval time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = l.Refresh(ctx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// Release gives up the lock: it is deleted, or overwritten as released
// where the credentials cannot delete.
func (l *Lock) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return deleteOrRelease(ctx, l.m.be, l.key, l.rec)
}

// RemoveStale deletes released lock records and locks not refreshed for
// StaleAfter, so a crashed process cannot block prune forever. It needs
// credentials that can delete (prune's).
func (m *Locks) RemoveStale(ctx context.Context) (int, error) {
	recs, err := m.records(ctx)
	if err != nil || len(recs) == 0 {
		return 0, err
	}
	now, err := storageNow(ctx, m.be)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range recs {
		if r.rec.State == StateReleased || now.Sub(r.mod) > m.staleAfter() {
			if err := m.be.Delete(ctx, r.key); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func blocked(kind string, by Record) error {
	what := "a backup holds a shared lock"
	if by.Kind == KindExclusive {
		what = "garbage collection holds the exclusive lock"
	}
	next := "retry when it finishes; scheduled runs retry at their next slot"
	if kind == KindShared {
		next = "retry the backup when prune finishes; scheduled backups retry at their next slot"
	}
	return &msg.Error{Code: msg.ExitFailure, Err: ErrBlocked, Message: msg.Message{
		Level: msg.LevelFailed, ID: IDLocked,
		Fact: fmt.Sprintf("the repository is locked: %s (holder %s@%s, process %d)", what, by.Holder.User, by.Holder.Host, by.Holder.PID),
		Next: next,
	}}
}
