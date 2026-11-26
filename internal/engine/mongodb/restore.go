// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// errRestoreExited stops the snapshot reader once mongorestore exits.
var errRestoreExited = errors.New("mongorestore exited")

// Restore starts an empty side instance of the snapshot's release and
// loads the snapshot into it with mongorestore, streaming the archive to
// its stdin (SDD 6.4 step 4). A replica-set snapshot is loaded with
// --oplogReplay, so the instance holds the data as of the snapshot's
// consistent point.
func (e *Engine) Restore(ctx context.Context, req engine.RestoreRequest, start engine.StartFunc) (instance.Instance, *engine.Reached, error) {
	if req.Point != (engine.Point{}) {
		return nil, nil, fmt.Errorf("restoring to a point: %w", errNotYet)
	}
	parts := strings.SplitN(req.Backup.EngineVersion, ".", 3)
	if len(parts) < 2 {
		return nil, nil, fmt.Errorf("the snapshot's MongoDB version %q is not understood", req.Backup.EngineVersion)
	}
	inst, err := start(ctx, parts[0]+"."+parts[1], req.DataDir)
	if err != nil {
		return nil, nil, err
	}
	if err := e.mongorestore(ctx, inst.Conn(), req); err != nil {
		_ = inst.Stop(context.WithoutCancel(ctx))
		return nil, nil, err
	}
	return inst, &engine.Reached{Position: req.Backup.End}, nil
}

// mongorestore loads the snapshot's archive into the side instance. The
// password goes in a --config file, never in the arguments.
func (e *Engine) mongorestore(ctx context.Context, c instance.Conn, req engine.RestoreRequest) error {
	bin, err := e.findTool("mongorestore")
	if err != nil {
		return err
	}
	conf := toolrun.MongoConfig(c.Password)
	f, err := toolrun.WriteSecretFile(e.Runner.SecretDir, ".yaml", conf)
	conf.Clear()
	if err != nil {
		return err
	}
	uri := "mongodb://" + net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) + "/?directConnection=true&authSource=admin"
	cmd := &toolrun.Cmd{
		Tool: "mongorestore", Path: bin, RunID: e.RunID,
		Args:    []string{"--uri=" + uri, "--username=" + c.User, "--config=" + f.Path, "--archive", "--stopOnError"},
		Files:   []*toolrun.SecretFile{f},
		Secrets: []secrets.Secret{c.Password},
	}
	if req.Backup.Start != "" {
		cmd.Args = append(cmd.Args, "--oplogReplay")
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := req.Archives.Archive(ctx, ArchiveName, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	cmd.Stdin = pr
	runErr := e.Runner.Run(ctx, cmd)
	// Unblock the reader if mongorestore stopped before the end.
	_ = pr.CloseWithError(errRestoreExited)
	readErr := <-done
	if runErr != nil {
		return runErr
	}
	// mongorestore succeeded, so it read the whole archive; bytes after the
	// archive's end are of no account.
	if readErr != nil && !errors.Is(readErr, errRestoreExited) {
		return fmt.Errorf("read the snapshot: %w", readErr)
	}
	return nil
}
