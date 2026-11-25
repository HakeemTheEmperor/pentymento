// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"errors"
	"testing"
)

func TestUpgradePlanCurrent(t *testing.T) {
	f := newFixture(t)
	plan, err := f.repo.UpgradePlan()
	if err != nil || len(plan) != 0 {
		t.Errorf("UpgradePlan of a new repository = %v, %v", plan, err)
	}
	if err := f.repo.Upgrade(context.Background(), nil); err != nil {
		t.Errorf("Upgrade with no plan = %v", err)
	}
}

// TestUpgradeRunsMigrations runs a migration from format 1, as a future
// format would: the config records format 2, which this pentym then refuses
// as newer, as every older binary must.
func TestUpgradeRunsMigrations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ran := 0
	plan := []Migration{{From: FormatVersion, Describe: "test step", Run: func(context.Context, *Repository) error {
		ran++
		return nil
	}}}
	if err := f.repo.Upgrade(ctx, plan); err != nil || ran != 1 || f.repo.Config().Version != FormatVersion+1 {
		t.Fatalf("Upgrade = %v, ran %d, version %d", err, ran, f.repo.Config().Version)
	}
	_, err := Open(ctx, f.be)
	wantMsgID(t, err, IDFormatNewer)

	// A plan that does not start at the repository's format is refused.
	if err := f.repo.Upgrade(ctx, plan); err == nil {
		t.Error("a migration from the wrong format ran")
	}
}

func TestUpgradeFailureKeepsFormat(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	boom := errors.New("boom")
	plan := []Migration{{From: FormatVersion, Run: func(context.Context, *Repository) error { return boom }}}
	if err := f.repo.Upgrade(ctx, plan); !errors.Is(err, boom) {
		t.Fatalf("Upgrade = %v", err)
	}
	r, err := Open(ctx, f.be)
	if err != nil || r.Config().Version != FormatVersion {
		t.Errorf("after a failed migration: %v, version %d", err, r.Config().Version)
	}
}

func TestUpgradePlanMissingMigration(t *testing.T) {
	f := newFixture(t)
	f.repo.cfg.Version = 0 // as if an older format had no migration
	if _, err := f.repo.UpgradePlan(); err == nil {
		t.Error("UpgradePlan found a path from format 0")
	}
}
