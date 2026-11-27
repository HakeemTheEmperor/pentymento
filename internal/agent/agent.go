// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
)

// IDSlotCanRead means the agent's key slot can unlock the private key
// while agent.write_only is on (SDD 8.4).
const IDSlotCanRead msg.ID = "PTM-4007"

// Timings (SDD 9).
const (
	// HeartbeatInterval is how often the agent writes its heartbeat.
	HeartbeatInterval = 30 * time.Second
	// ShutdownDeadline bounds a graceful shutdown; service managers allow
	// 45 s before they kill the agent.
	ShutdownDeadline = 30 * time.Second
	// captureRetry is the pause before capture starts again after it
	// failed while the agent still holds the lease.
	captureRetry = 5 * time.Second
)

// Target is a target as a capture loop sees it.
type Target struct {
	Name, ID string
	Config   config.Target
	// Generation is the lease generation held: checkpoints carry it, so a
	// late write from an older holder is fenced off (SDD 7.2).
	Generation uint64
}

// CaptureFunc captures a target's log while this agent holds its lease,
// until ctx ends; then it flushes what it has (SDD 9.5 step 3) and
// returns. The WAL and oplog capture loops (C18, C19) provide it.
type CaptureFunc func(ctx context.Context, t Target) error

// Options configure an Agent.
type Options struct {
	Config *config.Config
	// Repo is open and unlocked with the agent's key slot.
	Repo      *repo.Repository
	Telemetry *telemetry.Telemetry
	// Host names this machine in heartbeats and leases; empty means the
	// host name.
	Host string
	// Capture runs while a lease is held. When nil and Engine is set, the
	// agent captures through the target's engine; with neither it holds
	// leases without capturing.
	Capture CaptureFunc
	// Engine builds a target's engine, for capture and gap detection.
	Engine func(t Target) (engine.Engine, error)
	// Cleanup removes what a crashed run left behind (side instances,
	// temporary credential files); nil does nothing.
	Cleanup func(ctx context.Context) (int, error)
	// Backup takes a backup of a target now, through the agent's
	// repository and lock (backup --now); nil refuses.
	Backup func(ctx context.Context, t Target) (any, error)
	// OnLease, if set, is told when this agent gains or loses a target's
	// lease.
	OnLease func(target string, held bool, generation uint64)
	// Tuning for tests; zero means the defaults.
	Timings          lock.LeaseTimings
	Heartbeat        time.Duration
	ShutdownDeadline time.Duration
	HealthInterval   time.Duration
	Clock            lock.Clock
}

// Agent is the long-running agent (SDD 9.1): per target a lease loop that
// starts and stops capture, a heartbeat, a shared upload pool, all under
// one root context.
type Agent struct {
	o       Options
	id      string
	holder  lock.Holder
	log     *slog.Logger
	uploads *Pool
	ready   atomic.Bool

	mu      sync.Mutex
	cfg     *config.Config
	targets map[string]*targetLoop
	runCtx  context.Context
	started time.Time
	lastHB  atomic.Pointer[time.Time]

	// Background work, such as backups forced by a capture gap: cancelled
	// and waited for at shutdown.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup
	forcedMu sync.Mutex
	forced   map[string]bool
	started0 map[string]bool

	// live holds each capturing target's newest monitoring reading.
	liveMu sync.Mutex
	live   map[string]LiveCapture
}

// LiveCapture is a target's newest monitoring reading, for status --live.
type LiveCapture struct {
	// Position is the newest stored position; Head the server's.
	Position string `json:"position,omitempty"`
	Head     string `json:"server_position,omitempty"`
	// LagSeconds is how far capture lags (PRD 9.1).
	LagSeconds float64 `json:"lag_seconds"`
	// RetainedBytes and RetainedLimit are the WAL the Postgres slot holds
	// back and max_slot_wal_keep_size (0: no limit).
	RetainedBytes int64 `json:"slot_retained_bytes,omitempty"`
	RetainedLimit int64 `json:"slot_retained_limit_bytes,omitempty"`
	// OplogWindowSeconds is how far back the MongoDB oplog reaches.
	OplogWindowSeconds float64   `json:"oplog_window_seconds,omitempty"`
	Checked            time.Time `json:"checked"`
}

// targetLoop is one target's lease loop.
type targetLoop struct {
	t      Target
	lease  *lock.Lease
	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.Mutex
	held      bool
	gen       uint64
	heldSince time.Time
	lastErr   string
}

// New checks the agent may run with this key slot (SDD 9.4 step 2) and
// prepares it.
func New(o Options) (*Agent, error) {
	if o.Config.Agent.IsWriteOnly() && o.Repo.CanRead() {
		return nil, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDSlotCanRead,
			Fact:   "the agent's key slot can read backups, and agent.write_only is on",
			Impact: "an agent that can read backups lets anyone who takes over the database host read the backup history",
			Next:   "set repository.key to an agent slot (pentym key add --agent), or set agent.write_only: false",
		}}
	}
	return newAgent(o)
}

// newAgent prepares an agent, or catch-up, without the write-only check.
func newAgent(o Options) (*Agent, error) {
	if o.Telemetry == nil {
		o.Telemetry = telemetry.New(nil, nil)
	}
	if o.Host == "" {
		o.Host, _ = os.Hostname()
	}
	if o.Timings == (lock.LeaseTimings{}) {
		o.Timings = lock.DefaultLeaseTimings
	}
	if o.Heartbeat == 0 {
		o.Heartbeat = HeartbeatInterval
	}
	if o.ShutdownDeadline == 0 {
		o.ShutdownDeadline = ShutdownDeadline
	}
	if o.Clock == nil {
		o.Clock = lock.SystemClock
	}
	if o.HealthInterval == 0 {
		o.HealthInterval = HealthInterval
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b)
	ident := repo.CurrentIdentity()
	a := &Agent{
		o: o, id: id, cfg: o.Config,
		holder:   lock.CurrentHolder(ident.User, o.Host, id),
		log:      o.Telemetry.Component("agent"),
		uploads:  NewPool(runtime.GOMAXPROCS(0)),
		targets:  map[string]*targetLoop{},
		forced:   map[string]bool{},
		started0: map[string]bool{},
		live:     map[string]LiveCapture{},
	}
	a.bgCtx, a.bgCancel = context.WithCancel(context.Background())
	if a.o.Capture == nil && a.o.Engine != nil {
		a.o.Capture = a.captureFor
	}
	return a, nil
}

// ID is this agent's random ID, recorded in its heartbeats and leases.
func (a *Agent) ID() string { return a.id }

// Ready reports whether startup has finished and shutdown not begun.
func (a *Agent) Ready() bool { return a.ready.Load() }

// Uploads is the agent's upload pool, shared by everything that
// compresses, encrypts and uploads.
func (a *Agent) Uploads() *Pool { return a.uploads }

// Run starts the agent and runs it until ctx ends, then shuts down
// gracefully within the shutdown deadline (SDD 9.4, 9.5).
func (a *Agent) Run(ctx context.Context) error {
	a.mu.Lock() // Status may already be read through the control socket
	a.started = time.Now()
	a.mu.Unlock()
	if a.o.Cleanup != nil {
		if n, err := a.o.Cleanup(ctx); err != nil {
			a.log.Warn("could not remove everything a previous run left behind", "error", err)
		} else if n > 0 {
			a.log.Info("removed what a previous run left behind", "items", n)
		}
	}
	if err := a.heartbeat(ctx, repo.StateRunning); err != nil {
		return fmt.Errorf("write the first heartbeat: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.runCtx = runCtx
	for _, name := range sortedNames(a.cfg.Targets) {
		if err := a.startTarget(name, a.cfg.Targets[name]); err != nil {
			a.mu.Unlock()
			return errors.Join(err, a.stopAll(context.WithoutCancel(ctx)))
		}
	}
	n := len(a.targets)
	a.mu.Unlock()

	hbDone := make(chan struct{})
	go a.heartbeatLoop(runCtx, hbDone)
	a.ready.Store(true)
	a.log.Info("agent started", "agent_id", a.id, "host", a.o.Host, "targets", n)

	<-ctx.Done()
	a.ready.Store(false)
	a.log.Info("agent stopping")
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), a.o.ShutdownDeadline)
	defer scancel()
	cancel()
	<-hbDone
	err := a.stopAll(sctx)
	a.bgCancel() // forced backups commit nothing when stopped
	bgDone := make(chan struct{})
	go func() { a.bg.Wait(); close(bgDone) }()
	select {
	case <-bgDone:
	case <-sctx.Done():
		err = errors.Join(err, errors.New("background work did not stop in time"))
	}
	if herr := a.heartbeat(sctx, repo.StateStopped); herr != nil {
		err = errors.Join(err, fmt.Errorf("write the final heartbeat: %w", herr))
	}
	a.log.Info("agent stopped")
	return err
}

func sortedNames(m map[string]config.Target) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// startTarget starts a target's lease loop; a.mu is held.
func (a *Agent) startTarget(name string, c config.Target) error {
	tid, err := a.o.Repo.TargetID(name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(a.runCtx)
	tl := &targetLoop{
		t:      Target{Name: name, ID: tid, Config: c},
		lease:  lock.NewLease(a.o.Repo.Backend(), tid, a.holder, a.o.Clock, a.o.Timings),
		cancel: cancel, done: make(chan struct{}),
	}
	if tl.lease.BestEffort() {
		a.log.Warn("the storage has no conditional create, so the capture lease is best-effort", "target", name)
	}
	a.targets[name] = tl
	go a.leaseLoop(ctx, tl)
	return nil
}

// stopTarget stops a target's lease loop and releases its lease.
func (a *Agent) stopTarget(ctx context.Context, tl *targetLoop) error {
	tl.cancel()
	select {
	case <-tl.done:
	case <-ctx.Done():
		return fmt.Errorf("target %s did not stop in time", tl.t.Name)
	}
	// The loop has exited, so the lease is this goroutine's alone.
	if err := tl.lease.Release(ctx); err != nil {
		return fmt.Errorf("release the lease of %s: %w", tl.t.Name, err)
	}
	return nil
}

// stopAll stops every target, in parallel, and releases their leases.
func (a *Agent) stopAll(ctx context.Context) error {
	a.mu.Lock()
	loops := make([]*targetLoop, 0, len(a.targets))
	for _, tl := range a.targets {
		loops = append(loops, tl)
	}
	a.targets = map[string]*targetLoop{}
	a.mu.Unlock()
	errs := make([]error, len(loops))
	var wg sync.WaitGroup
	for i, tl := range loops {
		wg.Go(func() { errs[i] = a.stopTarget(ctx, tl) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// leaseLoop drives a target's lease: it ticks every check interval, starts
// capture when this agent holds the lease and stops it when the lease is
// lost or the loop ends.
func (a *Agent) leaseLoop(ctx context.Context, tl *targetLoop) {
	defer close(tl.done)
	log := a.log.With("target", tl.t.Name)
	var stopCapture func()
	defer func() {
		if stopCapture != nil {
			stopCapture()
			a.setHeld(tl, false, 0)
		}
	}()
	ticker := time.NewTicker(a.o.Timings.Check)
	defer ticker.Stop()
	for {
		if err := tl.lease.Tick(ctx); err != nil && ctx.Err() == nil {
			log.Warn("the capture lease could not be renewed or checked", "error", err)
			tl.mu.Lock()
			tl.lastErr = err.Error()
			tl.mu.Unlock()
		}
		may := tl.lease.MayCapture()
		switch {
		case may && stopCapture == nil:
			gen := tl.lease.Generation()
			log.Info("holding the capture lease", "generation", gen)
			a.setHeld(tl, true, gen)
			t := tl.t
			t.Generation = gen
			stopCapture = a.startCapture(ctx, t, log)
		case !may && stopCapture != nil:
			log.Warn("lost the capture lease; capture stopped")
			stopCapture()
			stopCapture = nil
			a.setHeld(tl, false, 0)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) setHeld(tl *targetLoop, held bool, gen uint64) {
	tl.mu.Lock()
	tl.held, tl.gen = held, gen
	if held {
		tl.heldSince = time.Now()
	}
	tl.mu.Unlock()
	v := 0.0
	if held {
		v = 1
	}
	a.o.Telemetry.Metrics.LeaseHeld.WithLabelValues(tl.t.Name).Set(v)
	if a.o.OnLease != nil {
		a.o.OnLease(tl.t.Name, held, gen)
	}
}

// startCapture runs the capture function, restarting it after a failure,
// and returns a function that stops it and waits.
func (a *Agent) startCapture(ctx context.Context, t Target, log *slog.Logger) func() {
	if a.o.Capture == nil {
		return func() {}
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for cctx.Err() == nil {
			err := a.runCapture(cctx, t)
			if cctx.Err() != nil {
				return
			}
			log.Error("capture stopped; starting it again", "error", err, "in", captureRetry.String())
			select {
			case <-cctx.Done():
			case <-time.After(captureRetry):
			}
		}
	}()
	return func() { cancel(); <-done }
}

// runCapture runs the capture function once, turning a panic into an
// error: one failing target must not stop the agent.
func (a *Agent) runCapture(ctx context.Context, t Target) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("capture panicked: %v\n%s", p, debug.Stack())
		}
	}()
	return a.o.Capture(ctx, t)
}

// heartbeat writes one heartbeat.
func (a *Agent) heartbeat(ctx context.Context, state string) error {
	now := time.Now()
	if err := a.o.Repo.PutHeartbeat(ctx, repo.Heartbeat{Host: a.o.Host, AgentID: a.id, State: state, Time: now}); err != nil {
		return err
	}
	a.lastHB.Store(&now)
	return nil
}

func (a *Agent) heartbeatLoop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(a.o.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.heartbeat(ctx, repo.StateRunning); err != nil && ctx.Err() == nil {
				a.log.Warn("could not write the heartbeat", "error", err)
			}
		}
	}
}

// BackupNow backs up one of the agent's configured targets now, as the
// control socket's backup-now asks.
func (a *Agent) BackupNow(ctx context.Context, name string) (any, error) {
	if a.o.Backup == nil {
		return nil, errors.New("this agent cannot take backups")
	}
	a.mu.Lock()
	c, ok := a.cfg.Targets[name]
	a.mu.Unlock()
	if !ok {
		return nil, &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
			Level: msg.LevelFailed, Fact: "the running agent has no target " + name,
			Next: "add it under targets: in the agent's config, then run pentym agent reload",
		}}
	}
	tid, err := a.o.Repo.TargetID(name)
	if err != nil {
		return nil, err
	}
	return a.o.Backup(ctx, Target{Name: name, ID: tid, Config: c})
}

// Config returns the agent's current configuration.
func (a *Agent) Config() *config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// ReloadResult says what a reload changed.
type ReloadResult struct {
	Added, Removed, Restarted, Unchanged []string
}

// Reload applies a new configuration without disturbing targets whose
// settings did not change, so their replication connections stay up:
// new targets start, removed ones stop and release their leases, and
// changed ones restart. A different repository needs a restart.
func (a *Agent) Reload(ctx context.Context, cfg *config.Config) (*ReloadResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runCtx == nil || a.runCtx.Err() != nil {
		return nil, errors.New("the agent is not running")
	}
	if a.cfg.Repository.Location() != cfg.Repository.Location() {
		return nil, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, Fact: "the new configuration names a different repository; a reload cannot switch repositories",
			Next: "restart the agent",
		}}
	}
	res := &ReloadResult{}
	for _, name := range sortedNames(a.cfg.Targets) {
		if _, ok := cfg.Targets[name]; !ok {
			if err := a.stopTarget(ctx, a.targets[name]); err != nil {
				return res, err
			}
			delete(a.targets, name)
			res.Removed = append(res.Removed, name)
		}
	}
	for _, name := range sortedNames(cfg.Targets) {
		nc := cfg.Targets[name]
		tl, ok := a.targets[name]
		switch {
		case !ok:
			if err := a.startTarget(name, nc); err != nil {
				return res, err
			}
			res.Added = append(res.Added, name)
		case sameTarget(tl.t.Config, nc):
			res.Unchanged = append(res.Unchanged, name)
		default:
			if err := a.stopTarget(ctx, tl); err != nil {
				return res, err
			}
			if err := a.startTarget(name, nc); err != nil {
				return res, err
			}
			res.Restarted = append(res.Restarted, name)
		}
	}
	a.cfg = cfg
	a.log.Info("configuration reloaded", "added", res.Added, "removed", res.Removed, "restarted", res.Restarted)
	return res, nil
}

// sameTarget compares two target settings, ignoring where in the config
// file their secret references were written.
func sameTarget(x, y config.Target) bool {
	x.Credentials.Backup.Password.Line, y.Credentials.Backup.Password.Line = 0, 0
	x.Credentials.Replication.Password.Line, y.Credentials.Replication.Password.Line = 0, 0
	return reflect.DeepEqual(x, y)
}

// TargetStatus is one target's state in this agent.
type TargetStatus struct {
	Name       string    `json:"name"`
	ID         string    `json:"id"`
	Holding    bool      `json:"holding_lease"`
	Generation uint64    `json:"generation,omitempty"`
	Since      time.Time `json:"since,omitzero"`
	BestEffort bool      `json:"best_effort_lease,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	// Capture is the newest monitoring reading while this agent captures.
	Capture *LiveCapture `json:"capture,omitempty"`
}

// Status is the agent's live state, for the control socket.
type Status struct {
	AgentID       string         `json:"agent_id"`
	Host          string         `json:"host"`
	Started       time.Time      `json:"started"`
	Ready         bool           `json:"ready"`
	LastHeartbeat time.Time      `json:"last_heartbeat,omitzero"`
	UploadQueue   int            `json:"upload_queue"`
	Targets       []TargetStatus `json:"targets"`
}

// Status returns the agent's live state.
func (a *Agent) Status() Status {
	s := Status{AgentID: a.id, Host: a.o.Host, Ready: a.Ready(), UploadQueue: a.uploads.Waiting(), Targets: []TargetStatus{}}
	if hb := a.lastHB.Load(); hb != nil {
		s.LastHeartbeat = *hb
	}
	a.mu.Lock()
	s.Started = a.started
	for _, tl := range a.targets {
		tl.mu.Lock()
		ts := TargetStatus{
			Name: tl.t.Name, ID: tl.t.ID, Holding: tl.held, Generation: tl.gen, Since: tl.heldSince,
			BestEffort: tl.lease.BestEffort(), LastError: tl.lastErr,
		}
		tl.mu.Unlock()
		a.liveMu.Lock()
		if lc, ok := a.live[tl.t.Name]; ok && ts.Holding {
			ts.Capture = &lc
		}
		a.liveMu.Unlock()
		s.Targets = append(s.Targets, ts)
	}
	a.mu.Unlock()
	slices.SortFunc(s.Targets, func(x, y TargetStatus) int {
		if x.Name < y.Name {
			return -1
		}
		if x.Name > y.Name {
			return 1
		}
		return 0
	})
	return s
}
