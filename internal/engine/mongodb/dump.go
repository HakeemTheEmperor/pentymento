// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// BaseBackup streams a mongodump archive into sink (PRD 7.2). On a
// replica set it runs mongodump --oplog, so the archive restores to one
// consistent point, and the snapshot covers the oplog from the first to
// the last entry the archive holds. A standalone server has no oplog: its
// snapshot is taken without one and covers no log positions.
func (e *Engine) BaseBackup(ctx context.Context, t engine.Target, sink engine.ArchiveSink) (*engine.BaseBackup, error) {
	s, err := e.connect(ctx, t)
	if err != nil {
		return nil, err
	}
	version, err := s.version(ctx)
	h := s.hello
	s.close(ctx)
	if err != nil {
		return nil, err
	}
	if h.sharded() {
		return nil, &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDSharded, Target: t.Name,
			Fact: "the uri points at a mongos router; sharded clusters are not supported",
			Next: "point uri at a replica set or a standalone server",
		}}
	}
	sum, err := e.mongodump(ctx, t, sink, h.replicaSet())
	if err != nil {
		return nil, err
	}
	bb := &engine.BaseBackup{EngineVersion: version, Method: Method, SystemID: h.SetName}
	if h.replicaSet() {
		if sum.OplogEntries == 0 {
			return nil, errors.New("the mongodump archive holds no oplog entries, so its consistent point is unknown")
		}
		bb.Start, bb.End = Position(sum.OplogFirst), Position(sum.OplogLast)
	}
	return bb, nil
}

// mongodump runs mongodump --archive to stdout into sink, reading the
// archive as it passes to check it and find the oplog's span. The
// password goes in a --config file, never in the arguments; compression
// is never asked for (--gzip would defeat deduplication).
func (e *Engine) mongodump(ctx context.Context, t engine.Target, sink engine.ArchiveSink, oplog bool) (*ArchiveSummary, error) {
	cred, err := backupCred(t)
	if err != nil {
		return nil, err
	}
	bin, err := e.findTool("mongodump")
	if err != nil {
		return nil, err
	}
	cmd := &toolrun.Cmd{Tool: "mongodump", Path: bin, RunID: e.RunID, Args: []string{"--uri=" + targetURI(t.Config)}}
	if cred.User != "" {
		cmd.Args = append(cmd.Args, "--username="+cred.User)
	}
	pw, err := e.password(ctx, t.Name, cred)
	if err != nil {
		return nil, err
	}
	defer pw.Clear()
	if !pw.IsEmpty() {
		conf := toolrun.MongoConfig(pw)
		f, err := toolrun.WriteSecretFile(e.Runner.SecretDir, ".yaml", conf)
		conf.Clear()
		if err != nil {
			return nil, err
		}
		cmd.Args = append(cmd.Args, "--config="+f.Path)
		cmd.Files, cmd.Secrets = []*toolrun.SecretFile{f}, []secrets.Secret{pw}
	}
	cmd.Args = append(cmd.Args, "--archive")
	if oplog {
		cmd.Args = append(cmd.Args, "--oplog")
	}

	w, err := sink.Archive(ctx, ArchiveName)
	if err != nil {
		return nil, err
	}
	// The archive is read as it is written, through a pipe; a reader that
	// fails keeps draining so mongodump is never blocked.
	pr, pwr := io.Pipe()
	type read struct {
		sum *ArchiveSummary
		err error
	}
	done := make(chan read, 1)
	go func() {
		sum, err := ReadArchive(pr)
		if err != nil {
			_, _ = io.Copy(io.Discard, pr)
		}
		done <- read{sum, err}
	}()
	cmd.Stdout = io.MultiWriter(w, pwr)
	runErr := e.Runner.Run(ctx, cmd)
	_ = pwr.CloseWithError(runErr)
	got := <-done
	if runErr != nil {
		return nil, runErr
	}
	if got.err != nil {
		return nil, fmt.Errorf("the mongodump archive does not parse: %w", got.err)
	}
	return got.sum, nil
}
