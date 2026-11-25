// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// Migration moves a repository from format From to From+1.
type Migration struct {
	From int
	// Describe says what the migration changes, for the confirmation
	// prompt and the journal.
	Describe string
	Run      func(ctx context.Context, r *Repository) error
}

// Migrations are the format migrations, in order (SDD 5.6). Format 1 is
// the first format, so there are none yet.
var Migrations []Migration

// UpgradePlan returns the migrations that bring the repository to
// FormatVersion, in order; empty when it is current.
func (r *Repository) UpgradePlan() ([]Migration, error) {
	var plan []Migration
	for v := r.cfg.Version; v < FormatVersion; v++ {
		found := false
		for _, m := range Migrations {
			if m.From == v {
				plan = append(plan, m)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("no migration from repository format %d", v)
		}
	}
	return plan, nil
}

// Upgrade runs the plan, recording the new format version in the config
// after each step, so an interrupted upgrade resumes where it stopped.
// The caller holds the exclusive lock, and every binary that uses the
// repository must already understand the new format: older ones refuse it.
func (r *Repository) Upgrade(ctx context.Context, plan []Migration) error {
	for _, m := range plan {
		if m.From != r.cfg.Version {
			return fmt.Errorf("migration from format %d does not apply to format %d", m.From, r.cfg.Version)
		}
		if err := m.Run(ctx, r); err != nil {
			return fmt.Errorf("migrate from format %d: %w", m.From, err)
		}
		cfg := r.cfg
		cfg.Version = m.From + 1
		b, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if err := r.be.Put(ctx, configKey, bytes.NewReader(b), backend.PutOptions{}); err != nil {
			return err
		}
		r.cfg = cfg
	}
	return nil
}
