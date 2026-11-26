// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"context"
	"fmt"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// agentUnreachable explains that no agent answers on the control socket,
// with the last heartbeat the repository holds (PRD 5.3): liveness is
// judged from the repository, never from the socket.
func (a *app) agentUnreachable(ctx context.Context, cfg *config.Config, sock string, err error, next string) error {
	fact := "agent not reachable at " + sock
	if hb := a.lastHeartbeat(ctx, cfg); hb != nil {
		fact += fmt.Sprintf("; last heartbeat from %s at %s (%s)", hb.Host, a.printer.Time(hb.Time), hb.State)
	}
	return &msg.Error{Code: msg.ExitConnect, Err: err, Message: msg.Message{
		Level: msg.LevelFailed, Fact: fact, Next: next,
	}}
}

// lastHeartbeat returns the newest agent heartbeat, or nil when there is
// none or the repository cannot be read without asking for a secret.
func (a *app) lastHeartbeat(ctx context.Context, cfg *config.Config) *repo.Heartbeat {
	if cfg.Repository.Key.IsZero() {
		return nil // reading it would mean prompting for a passphrase
	}
	_, r, err := a.openRepo(ctx)
	if err != nil {
		return nil
	}
	if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{}); err != nil {
		return nil
	}
	hbs, err := r.LatestHeartbeats(ctx)
	if err != nil || len(hbs) == 0 {
		return nil
	}
	newest := hbs[0]
	for _, hb := range hbs[1:] {
		if hb.Time.After(newest.Time) {
			newest = hb
		}
	}
	return &newest
}
