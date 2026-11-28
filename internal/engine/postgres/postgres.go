// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Versions are the supported Postgres major versions (PRD 6.1).
var Versions = []string{"16", "17", "18"}

// errNotYet is returned by the parts of the engine later chunks build.
var errNotYet = fmt.Errorf("not implemented in this build: %w", errors.ErrUnsupported)

// Engine is the Postgres engine (SDD 4.2).
type Engine struct {
	// Resolver resolves the target's secret references.
	Resolver secrets.Resolver
	// Runner runs pg_basebackup, the fallback.
	Runner *toolrun.Runner
	// ToolDirs are tools.postgres: directories searched for client tools
	// before PATH.
	ToolDirs []string
	// LookPath finds a tool on PATH; nil means exec.LookPath.
	LookPath func(string) (string, error)
	// Logger receives warnings, such as a fallback to pg_basebackup.
	Logger *slog.Logger
	// RunID tags child processes' logs.
	RunID string
	// Observer, if set, receives captured WAL as it arrives.
	Observer WALObserver
	// sideRoleExisted records whether the last restored cluster already
	// had pentym's side role, so a rewind leaves it rather than dropping it.
	sideRoleExisted bool
}

var _ engine.Engine = (*Engine)(nil)

func init() {
	engine.Register("postgres", func(env engine.Env) engine.Engine {
		return &Engine{Resolver: env.Resolver, Runner: env.Runner, ToolDirs: env.Tools.Postgres, Logger: env.Logger}
	})
}

// Name is "postgres".
func (*Engine) Name() string { return "postgres" }

// Capabilities are snapshots and PITR through a physical replication slot;
// tables are extracted on their own.
func (*Engine) Capabilities() engine.Capabilities {
	return engine.Capabilities{
		Snapshots: true, PITR: true, Capture: "physical replication slot",
		ExtractionUnit: "table", Versions: append([]string(nil), Versions...),
	}
}

func (e *Engine) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// VerifyContents comes with verification.
func (*Engine) VerifyContents(context.Context, instance.Instance, json.RawMessage) ([]engine.Finding, error) {
	return nil, errNotYet
}
