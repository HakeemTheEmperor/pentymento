// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package instance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// IDNoProvider means no configured provider could start a side instance.
const IDNoProvider msg.ID = "PTM-6001"

// Set is the configured providers, tried in order (PRD 7.6).
type Set struct {
	env       *Env
	providers []Provider
}

// NewSet returns the providers named in order ("docker", "local").
func NewSet(env *Env, order []string) *Set {
	s := &Set{env: env}
	for _, name := range order {
		switch name {
		case config.ProviderDocker:
			s.providers = append(s.providers, NewDocker(env))
		case config.ProviderLocal:
			s.providers = append(s.providers, NewLocal(env))
		}
	}
	return s
}

// Providers returns the providers in order.
func (s *Set) Providers() []Provider { return s.providers }

// Start removes leftovers of crashed runs, then starts an instance with the
// first provider that can. If none can, the error says why for each.
func (s *Set) Start(ctx context.Context, spec Spec) (Instance, error) {
	_, _ = s.CleanLeftovers(ctx)
	var reasons []string
	for _, p := range s.providers {
		inst, err := p.Start(ctx, spec)
		if err == nil {
			return inst, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reasons = append(reasons, p.Name()+": "+err.Error())
	}
	return nil, &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
		Level: msg.LevelFailed, ID: IDNoProvider,
		Fact:   fmt.Sprintf("no instance provider could start %s %s", spec.Engine, spec.Version),
		Impact: "restores and verification need a temporary server of the backup's version",
		Cause:  strings.Join(reasons, "; "),
		Next:   fmt.Sprintf("install %s %s binaries and list their directory under tools.%s, or start Docker; then run pentym doctor", spec.Engine, spec.Version, spec.Engine),
	}}
}

// CleanLeftovers removes what crashed runs on this host left behind: local
// servers and their directories, containers, and temporary credential
// files. It returns how many items it removed.
func (s *Set) CleanLeftovers(ctx context.Context) (int, error) {
	n, err := s.env.cleanLocal()
	m, serr := toolrun.CleanStale(s.env.Runner.SecretDir)
	n += m
	err = errors.Join(err, serr)
	for _, p := range s.providers {
		if d, ok := p.(*Docker); ok {
			if _, verr := d.ServerVersion(ctx); verr != nil {
				continue // nothing to clean without a daemon
			}
			m, derr := d.leftovers(ctx)
			n += m
			err = errors.Join(err, derr)
		}
	}
	return n, err
}

// cleanLocal stops local servers whose owner is gone and removes their
// instance directories.
func (e *Env) cleanLocal() (int, error) {
	entries, err := os.ReadDir(e.instancesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ent := range entries {
		dir := filepath.Join(e.instancesDir(), ent.Name())
		o, err := readOwner(dir)
		if err != nil {
			continue // not ours, or being written
		}
		if o.Host != e.Host || o.PID == os.Getpid() || toolrun.PIDAlive(o.PID) {
			continue
		}
		stopLeftoverLocal(o)
		if err := os.RemoveAll(dir); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
