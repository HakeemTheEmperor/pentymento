// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	Instances  Starter
	// WorkDir holds the restore's data directory.
	WorkDir    string
	Identity   repo.Identity
	Credential secrets.Ref
	Telemetry  *telemetry.Telemetry
	// OnBytes, if set, is called as snapshot bytes are read, for progress.
	OnBytes func(n int64)
}

// RestoreResult is a running side instance with the snapshot restored.
// The caller must Close it.
type RestoreResult struct {
	Snapshot *repo.Snapshot
	Instance instance.Instance
	Reached  *engine.Reached
	// DataDir is the instance's data directory, removed by Close.
	DataDir  string
	Duration time.Duration
	RunID    string
}

// Close stops the side instance and removes its data directory.
func (r *RestoreResult) Close(ctx context.Context) error {
	var err error
	if r.Instance != nil {
		err = r.Instance.Stop(ctx)
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
		s, err := o.pick(ctx, tid)
		if err != nil {
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
		if res.DataDir, err = os.MkdirTemp(o.WorkDir, "restore-"+job.RunID+"-"); err != nil {
			return err
		}
		req := engine.RestoreRequest{
			Target:   o.Target,
			Archives: &snapshotArchives{repo: o.Repo, idx: idx, s: s, onBytes: o.OnBytes},
			Backup: engine.BaseBackup{
				EngineVersion: s.EngineVersion, Method: s.Method, SystemID: s.SystemID,
				Start: engine.Position(s.StartPosition), End: engine.Position(s.EndPosition),
				Timeline: s.Timeline, Inventory: s.Inventory,
			},
			DataDir: res.DataDir,
		}
		startFn := func(ctx context.Context, version, dataDir string) (instance.Instance, error) {
			return o.Instances.Start(ctx, instance.Spec{Engine: o.Engine.Name(), Version: version, DataDir: dataDir, RunID: job.RunID})
		}
		res.Instance, res.Reached, err = o.Engine.Restore(ctx, req, startFn)
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
