// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
)

// Backup takes a base backup of a target into the repository (SDD 6.1). It
// is a write operation.
type Backup struct {
	// Repo is open and unlocked; an agent slot is enough.
	Repo   *repo.Repository
	Engine engine.Engine
	// Target is the configured target; its ID is filled in.
	Target engine.Target
	// Identity and Credential are recorded in the journal; Credential is
	// the reference used, never a secret.
	Identity   repo.Identity
	Credential secrets.Ref
	Telemetry  *telemetry.Telemetry
	// OnBytes, if set, is called as stream bytes are written, for
	// progress.
	OnBytes func(n int64)
	Write   repo.WriteOptions
}

// BackupResult describes a committed snapshot.
type BackupResult struct {
	Snapshot *repo.Snapshot
	Stats    repo.WriteStats
	Duration time.Duration
	RunID    string
	// Warnings are the engine check's non-fatal findings.
	Warnings []engine.Finding
}

// Run takes the backup:
//
//  1. check the connection and privileges (a failed check stops here);
//  2. take a shared repository lock, refreshed while the backup runs;
//  3. record the inventory, then stream every archive of the base backup
//     into its own snapshot part;
//  4. commit the snapshot only if the stream and the engine finished
//     cleanly; a failed or cancelled backup leaves only orphaned packs,
//     which prune removes;
//  5. journal the outcome and record metrics.
func (b *Backup) Run(ctx context.Context) (res *BackupResult, err error) {
	ctx, job := b.Telemetry.StartJob(ctx, "backup", b.Target.Name, "backup")
	res = &BackupResult{RunID: job.RunID}
	start := time.Now()
	defer func() {
		res.Duration = time.Since(start)
		job.End(scrubbed(err))
	}()

	tid, err := b.Repo.PutTargetInfo(ctx, repo.TargetInfo{Name: b.Target.Name, Engine: b.Engine.Name()})
	if err != nil {
		return res, err
	}
	b.Target.ID = tid
	op := b.Repo.BeginOperation(b.Identity, "backup", tid, job.RunID, b.Credential)
	defer func() {
		if jerr := op.Finish(context.WithoutCancel(ctx), repo.ResultOK, scrubbed(err)); err == nil {
			err = jerr
		}
	}()

	check, err := b.Engine.Check(ctx, b.Target)
	if err != nil {
		return res, err
	}
	for _, f := range check.Findings {
		if f.Level == msg.LevelFailed {
			return res, &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
				Level: msg.LevelFailed, ID: f.ID, Target: b.Target.Name, Fact: f.Fact,
				Impact: "no backup was taken", Next: f.Next,
			}}
		}
		res.Warnings = append(res.Warnings, f)
		job.Log.Warn(f.Fact, "id", string(f.ID))
	}

	err = WithLock(ctx, b.Repo, false, func() error {
		inventory, err := b.Engine.Inventory(ctx, b.Target)
		if err != nil {
			return err
		}
		idx, err := b.Repo.LoadIndex(ctx)
		if err != nil {
			return err
		}
		sw, err := b.Repo.NewSnapshotWriter(ctx, idx, repo.Snapshot{
			Target: tid, Engine: b.Engine.Name(), Inventory: inventory,
		}, b.Write)
		if err != nil {
			return err
		}
		defer sw.Close()
		sink := &partSink{sw: sw, onBytes: b.OnBytes}
		bb, berr := b.Engine.BaseBackup(ctx, b.Target, sink)
		if err := sink.finish(berr); err != nil {
			return err
		}
		sw.Describe(func(s *repo.Snapshot) {
			s.EngineVersion, s.Method, s.SystemID = bb.EngineVersion, bb.Method, bb.SystemID
			s.StartPosition, s.EndPosition, s.Timeline = string(bb.Start), string(bb.End), bb.Timeline
		})
		res.Snapshot, res.Stats, err = sw.Commit()
		return err
	})
	if err != nil {
		return res, err
	}
	if r := res.Stats.DedupRatio(); r > 0 {
		b.Telemetry.Metrics.DedupRatio.Set(r)
	}
	return res, nil
}

// scrubbed hides secrets that a driver may have put in an error.
func scrubbed(err error) error {
	if err == nil {
		return nil
	}
	return secrets.ScrubError(err)
}

// errPartDone ends a part's stream once it is written.
var errPartDone = errors.New("snapshot part already written")

// partSink turns the engine's archives into snapshot parts: each archive
// is piped into SnapshotWriter.WritePart while the engine writes it.
type partSink struct {
	sw      *repo.SnapshotWriter
	onBytes func(int64)

	pw   *io.PipeWriter
	done chan error
}

// Archive ends the previous part and starts the next.
func (p *partSink) Archive(_ context.Context, name string) (io.Writer, error) {
	if err := p.end(nil); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	p.pw, p.done = pw, make(chan error, 1)
	go func() {
		err := p.sw.WritePart(name, pr)
		// If the part failed, the engine's next write fails with its error.
		cause := err
		if cause == nil {
			cause = errPartDone
		}
		_ = pr.CloseWithError(cause)
		p.done <- err
	}()
	return &countingWriter{w: pw, onBytes: p.onBytes}, nil
}

// end closes the current part's stream, with cause if the engine failed,
// and waits for the part to be written.
func (p *partSink) end(cause error) error {
	if p.pw == nil {
		return nil
	}
	if cause != nil {
		_ = p.pw.CloseWithError(cause)
	} else {
		_ = p.pw.Close()
	}
	err := <-p.done
	p.pw = nil
	return err
}

// finish ends the last part. The engine's error comes first: a part error
// it caused is only a consequence.
func (p *partSink) finish(engineErr error) error {
	err := p.end(engineErr)
	if engineErr != nil {
		return engineErr
	}
	return err
}

// countingWriter reports bytes written, for progress.
type countingWriter struct {
	w       io.Writer
	onBytes func(int64)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	if c.onBytes != nil && n > 0 {
		c.onBytes(int64(n))
	}
	return n, err
}
