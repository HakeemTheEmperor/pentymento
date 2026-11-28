// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Rewind restores the whole cluster to req.Point into req.DataDir (PRD
// 7.5, --mode=rewind): the same restore and replay as a side instance,
// then pentym's side role is dropped, the server stopped cleanly, and the
// cluster's own configuration files put back, so the directory can
// replace production's data directory. It never stops or writes to
// production. A cluster with tablespaces is refused: they are restored
// inside the data directory, not where production keeps them.
func (e *Engine) Rewind(ctx context.Context, t engine.Target, req engine.RestoreRequest, start engine.StartFunc) (*engine.Reached, error) {
	for _, name := range req.Archives.Names() {
		if tablespaceArchiveRE.MatchString(name) {
			return nil, &msg.Error{Code: msg.ExitNotImplemented, Message: msg.Message{
				Level: msg.LevelFailed, Target: t.Name,
				Fact:   "this cluster has tablespaces, which a rewind cannot put back where production keeps them",
				Impact: "nothing was written",
				Next:   "restore the lost tables without --mode=rewind, with --table",
			}}
		}
	}
	inst, reached, err := e.Restore(ctx, req, start)
	if err != nil {
		return nil, err
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = inst.Stop(context.WithoutCancel(ctx))
		}
	}()
	if !e.sideRoleExisted {
		if err := e.dropSideRole(ctx, inst.Conn(), req.DataDir); err != nil {
			return nil, err
		}
	} else {
		e.logger().Warn("the restored cluster already had a role named "+SideRole+"; it was kept, with the password pentym set", "target", t.Name)
	}
	if err := inst.Stop(ctx); err != nil {
		return nil, fmt.Errorf("stop the rewound cluster: %w", err)
	}
	stopped = true
	if err := restoreConfig(req.DataDir); err != nil {
		return nil, err
	}
	return reached, nil
}

// dropSideRole drops pentym's side role from a rewound cluster: as the
// cluster's bootstrap superuser, through a brief trust window on the side
// instance, which listens only locally.
func (e *Engine) dropSideRole(ctx context.Context, c instance.Conn, dataDir string) error {
	side, err := sideConnect(ctx, c, c.User, c.Password)
	if err != nil {
		return err
	}
	rows, err := query(ctx, side, "SELECT rolname FROM pg_roles WHERE oid = 10")
	if err == nil && len(rows) == 1 {
		if werr := os.WriteFile(filepath.Join(dataDir, "pg_hba.conf"), []byte(trustHBA), 0o600); werr != nil {
			err = werr
		} else {
			_, err = side.Exec(ctx, "SELECT pg_reload_conf()").ReadAll()
		}
	}
	_ = side.Close(context.WithoutCancel(ctx))
	if err != nil || len(rows) != 1 {
		return fmt.Errorf("find the bootstrap superuser: %v", err)
	}
	boot := rows[0][0]
	for attempt := 0; ; attempt++ {
		conn, err := sideConnect(ctx, c, boot, secrets.Secret{})
		if err == nil {
			_, err = conn.Exec(ctx, "DROP ROLE "+SideRole+"; CHECKPOINT").ReadAll()
			_ = conn.Close(context.WithoutCancel(ctx))
			if err != nil {
				return fmt.Errorf("drop %s: %w", SideRole, err)
			}
			return nil
		}
		if attempt == 50 {
			return fmt.Errorf("log in as %s to drop %s: %w", boot, SideRole, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// restoreConfig removes pentym's configuration from a rewound data
// directory and puts the cluster's own back.
func restoreConfig(dataDir string) error {
	for _, name := range configFiles {
		p := filepath.Join(dataDir, name)
		orig := p + originalSuffix
		if _, err := os.Stat(orig); errors.Is(err, os.ErrNotExist) {
			// The cluster had none: remove pentym's (the side instance's
			// postgresql.auto.conf, say).
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := os.Rename(orig, p); err != nil {
			return err
		}
	}
	for _, name := range []string{"recovery.signal", "standby.signal"} {
		if err := os.Remove(filepath.Join(dataDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
