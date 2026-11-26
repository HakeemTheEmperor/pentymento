// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

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

// Versions are the supported MongoDB versions (PRD 6.1).
var Versions = []string{"7.0", "8.0", "9.0"}

// Method is how MongoDB snapshots are taken (PRD 7.2).
const Method = "mongodump"

// ArchiveName is the name of a MongoDB snapshot's one stream: the
// mongodump archive.
const ArchiveName = "mongodump.archive"

// errNotYet is returned by the parts of the engine later chunks build.
var errNotYet = fmt.Errorf("not implemented in this build: %w", errors.ErrUnsupported)

// Engine is the MongoDB engine (SDD 4.2).
type Engine struct {
	// Resolver resolves the target's secret references.
	Resolver secrets.Resolver
	// Runner runs mongodump.
	Runner *toolrun.Runner
	// ToolDirs are tools.mongodb: directories searched for the database
	// tools before PATH.
	ToolDirs []string
	// LookPath finds a tool on PATH; nil means exec.LookPath.
	LookPath func(string) (string, error)
	// Logger receives warnings.
	Logger *slog.Logger
	// RunID tags child processes' logs.
	RunID string
}

var _ engine.Engine = (*Engine)(nil)

func init() {
	engine.Register("mongodb", func(env engine.Env) engine.Engine {
		return &Engine{Resolver: env.Resolver, Runner: env.Runner, ToolDirs: env.Tools.MongoDB, Logger: env.Logger}
	})
}

// Name is "mongodb".
func (*Engine) Name() string { return "mongodb" }

// Capabilities are snapshots, and PITR through the oplog on replica sets
// only: a standalone server has no oplog. Collections are extracted on
// their own.
func (*Engine) Capabilities() engine.Capabilities {
	return engine.Capabilities{
		Snapshots: true, PITR: true, PITRNote: "replica sets only", Capture: "oplog tail",
		ExtractionUnit: "collection", Versions: append([]string(nil), Versions...),
	}
}

// findTool finds a database tool in tools.mongodb, then on PATH.
func (e *Engine) findTool(name string) (string, error) {
	return toolrun.Find(name, "mongodb", e.ToolDirs, e.LookPath)
}

// Capture comes with the agent.
func (*Engine) Capture(context.Context, engine.Target, engine.Position, engine.LogSink) error {
	return errNotYet
}

// DetectGap comes with the agent.
func (*Engine) DetectGap(context.Context, engine.Target, engine.Position) (*engine.Gap, error) {
	return nil, errNotYet
}

// LogHealth comes with oplog capture.
func (*Engine) LogHealth(context.Context, engine.Target) (*engine.LogHealth, error) {
	return nil, errNotYet
}

// Extract comes with restores.
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
