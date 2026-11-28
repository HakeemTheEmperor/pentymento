// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
)

// Starter starts side instances: the configured instance providers
// (*instance.Set).
type Starter interface {
	Start(ctx context.Context, spec instance.Spec) (instance.Instance, error)
}

// Restore restores a whole snapshot into a side instance (SDD 6.4 steps 2
// to 4, without log replay): the latest snapshot of a target, or a chosen
// one. It reads the repository only, so it never needs the agent.
type Restore struct {
	// Repo is open and unlocked with a full key slot.
	Repo   *repo.Repository
	Engine engine.Engine
	// Target is the configured target; its ID is filled in.
	Target engine.Target
	// SnapshotID chooses the snapshot; empty means the target's latest.
	SnapshotID string
	// Point, when set, restores to that point: the anchor snapshot is
	// chosen for it and the stored log replayed (SnapshotID is then not
	// used). When formats times in messages; nil means RFC 3339.
	Point     engine.Point
	When      func(time.Time) string
	Instances Starter
	// WorkDir holds the restore's data directory.
	WorkDir    string
	Identity   repo.Identity
	Credential secrets.Ref
	Telemetry  *telemetry.Telemetry
	// OnBytes, if set, is called as snapshot bytes are read, for progress.
	OnBytes func(n int64)
	// Extract, when set, loads objects from the side instance into
	// production once it is up (SDD 6.4 step 6); the side instance is then
	// stopped and removed (step 7), and the result has no instance.
	Extract *engine.ExtractRequest
	// Suffix, when Extract has none, makes it from the time restored to:
	// the point's time, or where replay stopped for a transaction ID or a
	// WAL position (the snapshot's end when nothing was replayed).
	Suffix func(time.Time) string
	// RewindDir, when set, rewinds the whole cluster into this directory
	// instead (--mode=rewind): it must be absent or empty, and is kept.
	RewindDir string
}

// RestoreResult is a running side instance with the snapshot restored.
// The caller must Close it.
type RestoreResult struct {
	Snapshot *repo.Snapshot
	Instance instance.Instance
	Reached  *engine.Reached
	// Extracted is what Extract loaded into production.
	Extracted *engine.ExtractResult
	// DataDir is the instance's data directory, removed by Close.
	DataDir  string
	Duration time.Duration
	RunID    string
	rewound  bool
}

// Close stops the side instance and removes its data directory; a
// rewound directory is kept.
func (r *RestoreResult) Close(ctx context.Context) error {
	var err error
	if r.Instance != nil {
		err = r.Instance.Stop(ctx)
		r.Instance = nil
	}
	if r.rewound || r.DataDir == "" {
		return err
	}
	return errors.Join(err, os.RemoveAll(r.DataDir))
}

// Run restores the snapshot:
//
//  1. take a shared repository lock, so prune removes nothing being read;
//  2. pick the snapshot: the chosen one, which must be the target's, or
//     the target's newest;
//  3. create an empty data directory under the work directory and let
//     the engine materialize the snapshot into a side instance of the
//     snapshot's version, reading its archives with parallel range reads;
//  4. journal the outcome. A failed restore stops its instance and
//     removes its directory.
func (o *Restore) Run(ctx context.Context) (res *RestoreResult, err error) {
	ctx, job := o.Telemetry.StartJob(ctx, "restore", o.Target.Name, "restore")
	res = &RestoreResult{RunID: job.RunID}
	start := time.Now()
	defer func() {
		res.Duration = time.Since(start)
		job.End(scrubbed(err))
	}()
	tid, err := o.Repo.TargetID(o.Target.Name)
	if err != nil {
		return res, err
	}
	o.Target.ID = tid
	op := o.Repo.BeginOperation(o.Identity, "restore", tid, job.RunID, o.Credential)
	defer func() {
		if jerr := op.Finish(context.WithoutCancel(ctx), repo.ResultOK, scrubbed(err)); err == nil {
			err = jerr
		}
	}()
	defer func() {
		if err != nil && res.DataDir != "" {
			_ = res.Close(context.WithoutCancel(ctx))
			res.Instance = nil
		}
	}()
	err = WithLock(ctx, o.Repo, false, func() error {
		var logs engine.LogSource
		var segSize uint64
		var s *repo.Snapshot
		if o.Point != (engine.Point{}) {
			plan, err := o.plan(ctx, tid)
			if err != nil {
				return err
			}
			s, segSize = &plan.Snapshot, plan.SegmentSize
			if logs, err = newWALSource(ctx, o.Repo, tid, plan); err != nil {
				return err
			}
		} else if s, err = o.pick(ctx, tid); err != nil {
			return err
		}
		res.Snapshot = s
		idx, err := o.Repo.LoadIndex(ctx)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(o.WorkDir, 0o700); err != nil {
			return err
		}
		if o.RewindDir != "" {
			if err := emptyDir(o.RewindDir); err != nil {
				return err
			}
			res.DataDir = o.RewindDir
		} else if res.DataDir, err = os.MkdirTemp(o.WorkDir, "restore-"+job.RunID+"-"); err != nil {
			return err
		}
		req := engine.RestoreRequest{
			Target:   o.Target,
			Archives: &snapshotArchives{repo: o.Repo, idx: idx, s: s, onBytes: o.OnBytes},
			Backup: engine.BaseBackup{
				EngineVersion: s.EngineVersion, Method: s.Method, SystemID: s.SystemID,
				Start: engine.Position(s.StartPosition), End: engine.Position(s.EndPosition),
				Timeline: s.Timeline, Inventory: s.Inventory, WALSegmentSize: int64(segSize), // #nosec G115 -- at most 1 GiB
			},
			Logs: logs, Point: o.Point,
			DataDir: res.DataDir,
		}
		startFn := func(ctx context.Context, version, dataDir string) (instance.Instance, error) {
			return o.Instances.Start(ctx, instance.Spec{Engine: o.Engine.Name(), Version: version, DataDir: dataDir, RunID: job.RunID})
		}
		if o.RewindDir != "" {
			res.Reached, err = o.Engine.Rewind(ctx, o.Target, req, startFn)
			if err == nil {
				res.rewound = true
			}
			return err
		}
		if res.Instance, res.Reached, err = o.Engine.Restore(ctx, req, startFn); err != nil || o.Extract == nil {
			return err
		}
		ex := *o.Extract
		if ex.Suffix == "" && o.Suffix != nil {
			at := o.Point.Time
			if at.IsZero() && res.Reached != nil {
				at = res.Reached.Time
			}
			if at.IsZero() {
				at = s.End
			}
			ex.Suffix = o.Suffix(at)
		}
		res.Extracted, err = o.Engine.Extract(ctx, res.Instance, o.Target, ex)
		if cerr := res.Close(context.WithoutCancel(ctx)); err == nil && cerr != nil {
			err = fmt.Errorf("clean up the side instance: %w", cerr)
		}
		return err
	})
	if err != nil {
		return res, err
	}
	if res.Reached != nil && res.Reached.Time.IsZero() {
		res.Reached.Time = res.Snapshot.End
	}
	return res, nil
}

// emptyDir makes sure dir is an empty directory, creating it.
func emptyDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return &msg.Error{Code: msg.ExitUsage, Message: msg.Message{Level: msg.LevelFailed,
			Fact: "the rewind directory must be an absolute path, not " + dir, Next: "pentym restore --help"}}
	}
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.MkdirAll(dir, 0o700)
	case err != nil:
		return err
	case len(entries) > 0:
		return &msg.Error{Code: msg.ExitUsage, Message: msg.Message{Level: msg.LevelFailed,
			Fact: dir + " is not empty; a rewind writes a whole cluster into an empty directory", Next: "choose an empty or new directory for --data-dir"}}
	}
	return os.Chmod(dir, 0o700) // #nosec G302 -- a data directory: Postgres needs 0700
}

// plan chooses the anchor snapshot and the log to replay to o.Point.
func (o *Restore) plan(ctx context.Context, tid string) (*WALPlan, error) {
	if o.Engine.Name() != "postgres" {
		return nil, fmt.Errorf("restoring %s to a point: %w", o.Engine.Name(), errors.ErrUnsupported)
	}
	all, err := o.Repo.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	var snaps []repo.Snapshot
	for _, s := range all {
		if s.Target == tid && s.Engine == o.Engine.Name() {
			snaps = append(snaps, s)
		}
	}
	segs, err := o.Repo.WALSegments(ctx, tid)
	if err != nil {
		return nil, err
	}
	gaps, err := o.Repo.Gaps(ctx, tid)
	if err != nil {
		return nil, err
	}
	when := o.When
	if when == nil {
		when = func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	}
	return PlanPostgres(o.Target.Name, snaps, segs, gaps, o.Point, when)
}

// pick returns the chosen snapshot, or the target's newest.
func (o *Restore) pick(ctx context.Context, tid string) (*repo.Snapshot, error) {
	if o.SnapshotID != "" {
		s, err := o.Repo.LoadSnapshot(ctx, o.SnapshotID)
		if errors.Is(err, backend.ErrNotFound) {
			return nil, &msg.Error{Code: msg.ExitNotFound, Err: err, Message: msg.Message{
				Level: msg.LevelFailed, Target: o.Target.Name, Fact: "no snapshot " + o.SnapshotID + " in the repository", Next: "pentym list",
			}}
		}
		if err != nil {
			return nil, err
		}
		if s.Target != tid {
			return nil, &msg.Error{Code: msg.ExitUsage, Message: msg.Message{
				Level: msg.LevelFailed, Target: o.Target.Name, Fact: "snapshot " + s.ID + " is not a snapshot of " + o.Target.Name, Next: "pentym list",
			}}
		}
		return o.checkEngine(s)
	}
	all, err := o.Repo.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	var latest *repo.Snapshot
	for i := range all {
		s := &all[i]
		if s.Target == tid && (latest == nil || s.End.After(latest.End) || (s.End.Equal(latest.End) && s.ID > latest.ID)) {
			latest = s
		}
	}
	if latest == nil {
		return nil, &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
			Level: msg.LevelFailed, Target: o.Target.Name, Fact: "the repository has no snapshots of " + o.Target.Name,
			Next: "pentym backup " + o.Target.Name,
		}}
	}
	return o.checkEngine(latest)
}

func (o *Restore) checkEngine(s *repo.Snapshot) (*repo.Snapshot, error) {
	if s.Engine != o.Engine.Name() {
		return nil, fmt.Errorf("snapshot %s is a %s snapshot, but %s is a %s target", s.ID, s.Engine, o.Target.Name, o.Engine.Name())
	}
	return s, nil
}

// snapshotArchives reads a snapshot's streams back as the engine's
// archives.
type snapshotArchives struct {
	repo    *repo.Repository
	idx     *repo.Index
	s       *repo.Snapshot
	onBytes func(int64)
}

func (a *snapshotArchives) Names() []string {
	var names []string
	for _, p := range a.s.Streams() {
		names = append(names, p.Name)
	}
	return names
}

func (a *snapshotArchives) Archive(ctx context.Context, name string, w io.Writer) error {
	if a.onBytes != nil {
		w = &countingWriter{w: w, onBytes: a.onBytes}
	}
	return a.repo.ReadPart(ctx, a.idx, a.s, name, w)
}
