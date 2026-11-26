// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os/exec"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Versions are the supported Postgres major versions (PRD 6.1).
var Versions = []string{"16", "17", "18"}

// errNotYet is returned by the parts of the engine later chunks build.
var errNotYet = errors.New("not implemented in this build")

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
}

var _ engine.Engine = (*Engine)(nil)

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

func (e *Engine) lookPath(name string) (string, error) {
	if e.LookPath != nil {
		return e.LookPath(name)
	}
	return exec.LookPath(name)
}

// Capture comes with the agent (C17).
func (*Engine) Capture(context.Context, engine.Target, engine.Position, engine.LogSink) error {
	return errNotYet
}

// DetectGap comes with the agent (C17).
func (*Engine) DetectGap(context.Context, engine.Target, engine.Position) (*engine.Gap, error) {
	return nil, errNotYet
}

// Restore comes with restores (C16).
func (*Engine) Restore(context.Context, engine.RestoreRequest, engine.StartFunc) (instance.Instance, *engine.Reached, error) {
	return nil, nil, errNotYet
}

// Extract comes with restores (C16).
func (*Engine) Extract(context.Context, instance.Instance, engine.Target, engine.ExtractRequest) error {
	return errNotYet
}

// Rewind comes with restores.
func (*Engine) Rewind(context.Context, engine.Target, engine.RestoreRequest, engine.StartFunc) error {
	return errNotYet
}

// VerifyContents comes with verification.
func (*Engine) VerifyContents(context.Context, instance.Instance, json.RawMessage) ([]engine.Finding, error) {
	return nil, errNotYet
}
