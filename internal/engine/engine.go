// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// This is the engine interface (SDD 4.2), finalized against the Postgres
// (C13) and MongoDB (C14) engines: callers use only this interface and
// build engines through the registry, so no caller names an engine. The
// capture, restore and verification methods are fixed here and filled in
// by later chunks.

// Capabilities are what an engine supports (SDD 4.2, first row).
type Capabilities struct {
	Snapshots bool
	PITR      bool
	// PITRNote qualifies PITR, such as "replica sets only".
	PITRNote string
	// Capture is how logs are captured: "physical replication slot" or
	// "oplog tail".
	Capture string
	// ExtractionUnit is what can be extracted on its own: "table" or
	// "collection".
	ExtractionUnit string
	// Versions are the supported major versions, such as "16", "17", "18"
	// or "7.0", "8.0".
	Versions []string
}

// Target is a configured database as an engine sees it.
type Target struct {
	// Name is the configured name; ID is its opaque ID in the repository.
	Name, ID string
	Config   config.Target
}

// Position is a place in an engine's log: a Postgres LSN ("0/1A2B3C4D")
// or a MongoDB oplog timestamp. Only the engine interprets it; to the rest
// of pentym it is opaque and compared only by the engine.
type Position string

// Finding is one result of a connection or privilege check, worded as a
// message (Brand 11).
type Finding struct {
	Level msg.Level
	ID    msg.ID
	Fact  string
	Next  string
}

// CheckResult is what Check found: the server, the role, and anything
// wrong, including too much privilege as well as too little (SDD 8.5).
type CheckResult struct {
	ServerVersion string
	Role          string
	Findings      []Finding
}

// BaseBackup describes a finished base-backup stream: what the snapshot
// records besides its chunks.
type BaseBackup struct {
	EngineVersion string
	// Method is how the backup was taken: "native" or "pg_basebackup"
	// (Postgres), "mongodump" (MongoDB).
	Method string
	// SystemID identifies the database cluster (Postgres) or replica set
	// (MongoDB, its set name); empty for a MongoDB standalone.
	SystemID string
	// Start and End are the log positions the snapshot covers; a MongoDB
	// standalone has no oplog, so neither.
	Start, End Position
	// Timeline is the Postgres timeline; empty for MongoDB.
	Timeline string
	// WALSegmentSize is the Postgres cluster's WAL segment size in bytes,
	// which captured WAL coverage is measured with; 0 for MongoDB.
	WALSegmentSize int64
	Inventory      json.RawMessage
}

// ArchiveSink receives a base backup's archives in order. A Postgres
// backup has base.tar, one <oid>.tar per tablespace, and backup_manifest;
// a MongoDB backup has one archive. Each is stored as its own stream, so
// no framing between them disturbs deduplication.
type ArchiveSink interface {
	// Archive ends the previous archive, if any, and returns a writer for
	// the next one.
	Archive(ctx context.Context, name string) (io.Writer, error)
}

// Segment is one unit of captured log data, stored as one object: a WAL
// segment (complete or partial), a timeline history file, or an oplog
// batch.
type Segment struct {
	// Kind is "wal", "wal-partial", "history" or "oplog".
	Kind string
	// Name is the engine's name for it: the WAL file name, or empty for an
	// oplog batch.
	Name        string
	Timeline    string
	First, Last Position
	// Checkpoint is where capture resumes once this segment is stored, in
	// the engine's own form; the agent records it in the checkpoint as it
	// is.
	Checkpoint Position
	Data       []byte
}

// ErrGap means capture cannot continue from the position asked for: the
// log since it is gone. DetectGap says what was lost.
var ErrGap = errors.New("the log since the checkpoint can no longer be captured")

// Event is a destructive event detected in the log (PRD 7.4): a drop,
// truncate, database drop, rename, or a burst of deletes.
type Event struct {
	Type   string
	Object string
	// XID is the Postgres transaction ID; empty for MongoDB.
	XID      string
	Position Position
	Time     time.Time
}

// LogSink receives what capture produces, in log order. A segment is safe
// only once Segment returns nil; capture then acknowledges it to the
// server (Postgres) or moves on (MongoDB).
type LogSink interface {
	Segment(ctx context.Context, s Segment) error
	Event(ctx context.Context, e Event) error
}

// Gap is a stretch of log that cannot be captured any more: a dropped or
// invalidated slot, removed WAL, or an oplog that rolled past the
// checkpoint. A new base backup is needed.
type Gap struct {
	From, To Position
	Reason   string
}

// LogHealth is the state of the log capture depends on, for monitoring
// against the alert thresholds (PRD 9.1).
type LogHealth struct {
	// Head is the server's newest log position.
	Head Position
	// RetainedBytes is the WAL the replication slot holds back on the
	// server (Postgres); RetainedLimit is max_slot_wal_keep_size in bytes,
	// or 0 when the server sets no limit.
	RetainedBytes, RetainedLimit int64
	// Lost reports a slot the server has invalidated.
	Lost bool
	// Oplog reports a MongoDB oplog: Window applies, and the retained
	// fields do not.
	Oplog bool
	// Window is how far back the oplog reaches (MongoDB).
	Window time.Duration
}

// Point is a restore target: exactly one field is set.
type Point struct {
	Time time.Time
	// XID restores to just before this Postgres transaction.
	XID string
	// Before restores to just before this position (an event's).
	Before Position
}

// LogSource gives restore the stored log segments.
type LogSource interface {
	// Names lists, in log order, the stored segments needed to replay
	// from one position to another.
	Names(ctx context.Context, from, to Position) ([]string, error)
	// Segment returns one stored segment's content by its engine name.
	Segment(ctx context.Context, name string) ([]byte, error)
}

// StartFunc starts a side instance of the engine's version (a Postgres
// major version such as "17", a MongoDB release such as "8.0") over a
// data directory, through the configured instance providers. An empty
// directory gets a fresh server; one the engine filled is used as it is.
type StartFunc func(ctx context.Context, version, dataDir string) (instance.Instance, error)

// ArchiveSource reads a snapshot's archives back, decrypted, in the
// order the engine wrote them.
type ArchiveSource interface {
	// Names lists the archives in order.
	Names() []string
	// Archive writes one archive to w.
	Archive(ctx context.Context, name string, w io.Writer) error
}

// RestoreRequest asks for a side instance at a point.
type RestoreRequest struct {
	// Target is the configured target the snapshot was taken from.
	Target Target
	// Archives are the snapshot's archives; Backup describes them.
	Archives ArchiveSource
	Backup   BaseBackup
	// Logs and Point are for replay to a point, which later chunks add;
	// a zero Point restores the snapshot as it is.
	Logs  LogSource
	Point Point
	// DataDir is an empty directory for the instance's data.
	DataDir string
}

// Reached is where a side instance stopped replaying: the point actually
// reached, for verify L3 and for reporting.
type Reached struct {
	Position Position
	Time     time.Time
}

// ExtractRequest asks to copy objects from a side instance into
// production, under a safe name unless InPlace (PRD 7.5).
type ExtractRequest struct {
	// Objects are tables ("orders", "billing.invoices") or collections.
	Objects []string
	// InPlace loads them under their own names; they must not exist.
	InPlace bool
	// Suffix makes the safe name, such as "_recovered_20261122"; when that
	// name is taken, _2, _3… follow it. Objects belonging to a table
	// (indexes, sequences) get the same suffix.
	Suffix string
}

// Extracted is one object loaded into production.
type Extracted struct {
	// Source is the object as asked for; Name what it is called in
	// production, both schema-qualified.
	Source, Name string
	Rows         int64
}

// SkippedConstraint is a foreign key left out of an extraction because
// it refers to a table that was not extracted.
type SkippedConstraint struct {
	// Table is the extracted table, as named in production.
	Table, Name string
	// References is the table it refers to; Statement adds it back.
	References, Statement string
}

// ExtractResult is what an extraction loaded.
type ExtractResult struct {
	Objects            []Extracted
	SkippedForeignKeys []SkippedConstraint
}

// Engine is one database engine (SDD 4.2). Every method that talks to a
// server uses the target's configured connection and secret references;
// child tools go through the tool runner.
type Engine interface {
	Name() string
	Capabilities() Capabilities
	// Check connects and checks the server version, the role and its
	// privileges, for doctor and before a backup.
	Check(ctx context.Context, t Target) (*CheckResult, error)
	// BaseBackup streams a base backup into sink, one archive after
	// another, and describes it. It returns only after the stream and any
	// child process finished cleanly; an error means nothing it wrote may
	// become a snapshot.
	BaseBackup(ctx context.Context, t Target, sink ArchiveSink) (*BaseBackup, error)
	// Capture streams the log from a checkpoint into sink until ctx ends.
	Capture(ctx context.Context, t Target, from Position, sink LogSink) error
	// DetectGap reports whether the log since a checkpoint can still be
	// captured; nil means it can.
	DetectGap(ctx context.Context, t Target, checkpoint Position) (*Gap, error)
	// LogHealth reports how much log the server retains for capture, for
	// monitoring.
	LogHealth(ctx context.Context, t Target) (*LogHealth, error)
	// Restore brings up a side instance with the snapshot restored (and,
	// later, replayed to req.Point), calling start when the data directory
	// is ready (Postgres) or first (MongoDB). The instance's Conn logs in
	// as a superuser of the side instance; the caller stops it.
	Restore(ctx context.Context, req RestoreRequest, start StartFunc) (instance.Instance, *Reached, error)
	// Extract copies objects from a side instance into the target.
	Extract(ctx context.Context, from instance.Instance, to Target, req ExtractRequest) (*ExtractResult, error)
	// Rewind restores the whole cluster to req.Point into req.DataDir and
	// stops it cleanly, ready to replace production's (opt-in,
	// destructive once swapped in); it never touches production itself.
	Rewind(ctx context.Context, t Target, req RestoreRequest, start StartFunc) (*Reached, error)
	// Inventory summarizes a server's contents for the snapshot record.
	Inventory(ctx context.Context, t Target) (json.RawMessage, error)
	// VerifyContents checks a side instance's integrity (pg_amcheck,
	// validate) and compares it with an inventory, for verify L4.
	VerifyContents(ctx context.Context, inst instance.Instance, inventory json.RawMessage) ([]Finding, error)
}

// Env is what an engine needs from the run that uses it.
type Env struct {
	// Resolver resolves the target's secret references.
	Resolver secrets.Resolver
	// Runner runs the engine's client tools.
	Runner *toolrun.Runner
	// Tools are the configured tool directories; each engine searches its
	// own (tools.postgres, tools.mongodb) before PATH.
	Tools config.Tools
	// Logger receives the engine's warnings; nil discards them.
	Logger *slog.Logger
}

// Factory builds an engine for a run.
type Factory func(Env) Engine

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes an engine available by its configured name, such as
// "postgres". Engine packages register in init; registering a name twice
// panics.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := factories[name]; ok {
		panic("engine registered twice: " + name)
	}
	factories[name] = f
}

// ErrUnavailable means no engine is registered under a name.
var ErrUnavailable = errors.New("engine not available in this build")

// New builds the engine for a configured engine name, so callers never
// name an engine themselves.
func New(name string, env Env) (Engine, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnavailable, name)
	}
	return f(env), nil
}

// Names returns the registered engines' names, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
