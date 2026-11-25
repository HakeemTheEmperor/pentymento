// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// Objects that change over time are never rewritten: each change is a new
// object with a sortable name, and readers take the newest. This keeps
// every object write-once, so write-only credentials and S3 Object Lock
// work; prune removes superseded records.

// TargetInfo maps an opaque target ID back to the target's name.
type TargetInfo struct {
	Name   string `json:"name"`
	Engine string `json:"engine"`
}

// PutTargetInfo records a target's name and engine. It is idempotent for
// the same target.
func (r *Repository) PutTargetInfo(ctx context.Context, info TargetInfo) (string, error) {
	tid, err := r.TargetID(info.Name)
	if err != nil {
		return "", err
	}
	err = r.putMeta(ctx, targetInfoKey(tid), info, backend.PutOptions{IfNotExists: true})
	if errors.Is(err, backend.ErrExists) {
		err = nil
	}
	return tid, err
}

// Targets returns every recorded target, by ID.
func (r *Repository) Targets(ctx context.Context) (map[string]TargetInfo, error) {
	keys, err := r.listKeys(ctx, targetsPrefix)
	if err != nil {
		return nil, err
	}
	out := map[string]TargetInfo{}
	for _, k := range keys {
		tid, rest, ok := strings.Cut(strings.TrimPrefix(k, targetsPrefix), "/")
		if !ok || rest != "info" {
			continue
		}
		var info TargetInfo
		if err := r.getMeta(ctx, k, &info); err != nil {
			return nil, err
		}
		out[tid] = info
	}
	return out, nil
}

// Checkpoint is the last position of a target's log capture that is safely
// stored, written with the capture lease's generation (SDD 5.2, 7.2).
type Checkpoint struct {
	Target     string `json:"target"`
	Generation uint64 `json:"generation"`
	Seq        uint64 `json:"seq"`
	// Position is the engine's position: an LSN and timeline for Postgres,
	// an oplog timestamp for MongoDB.
	Position string    `json:"position"`
	Time     time.Time `json:"time"`
}

// PutCheckpoint writes a checkpoint. Its key sorts by generation and then
// sequence, so the newest checkpoint of the newest lease is last.
func (r *Repository) PutCheckpoint(ctx context.Context, c Checkpoint) error {
	return r.putMeta(ctx, checkpointKey(c.Target, c.Generation, c.Seq), c, backend.PutOptions{IfNotExists: true})
}

// LatestCheckpoint returns a target's newest checkpoint, or nil if there is
// none.
func (r *Repository) LatestCheckpoint(ctx context.Context, tid string) (*Checkpoint, error) {
	k, err := r.lastKey(ctx, checkpointPrefix(tid))
	if err != nil || k == "" {
		return nil, err
	}
	var c Checkpoint
	return &c, r.getMeta(ctx, k, &c)
}

// Heartbeat records that an agent is alive (SDD 5.2).
type Heartbeat struct {
	Host    string    `json:"host"`
	AgentID string    `json:"agent_id"`
	State   string    `json:"state"` // "running" or "stopped"
	Time    time.Time `json:"time"`
}

// Heartbeat states.
const (
	StateRunning = "running"
	StateStopped = "stopped"
)

// PutHeartbeat writes a heartbeat.
func (r *Repository) PutHeartbeat(ctx context.Context, h Heartbeat) error {
	return r.putMeta(ctx, heartbeatKey(h.Host, h.Time), h, backend.PutOptions{IfNotExists: true})
}

// LatestHeartbeats returns the newest heartbeat of every agent host.
func (r *Repository) LatestHeartbeats(ctx context.Context) ([]Heartbeat, error) {
	keys, err := r.listKeys(ctx, heartbeatPrefix)
	if err != nil {
		return nil, err
	}
	latest := map[string]string{} // host prefix → newest key
	var order []string
	for _, k := range keys {
		i := strings.LastIndex(k, "/")
		host := k[:i]
		if _, ok := latest[host]; !ok {
			order = append(order, host)
		}
		latest[host] = k
	}
	var out []Heartbeat
	for _, host := range order {
		var h Heartbeat
		if err := r.getMeta(ctx, latest[host], &h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// Event is a detected destructive operation (SDD 5.2, 6.3).
type Event struct {
	Target string `json:"target"`
	// Type is the operation, such as "DROP TABLE" or "dropDatabase".
	Type string `json:"type"`
	// Object is the table, database or collection affected.
	Object string `json:"object"`
	// XID and LSN (Postgres) or OplogTS (MongoDB) locate the event.
	XID      string    `json:"xid,omitempty"`
	LSN      string    `json:"lsn,omitempty"`
	OplogTS  string    `json:"oplog_ts,omitempty"`
	Position string    `json:"position"`
	Time     time.Time `json:"time"`
}

// PutEvent records an event.
func (r *Repository) PutEvent(ctx context.Context, e Event) error {
	return r.putMeta(ctx, eventKey(e.Target, e.Time), e, backend.PutOptions{IfNotExists: true})
}

// Events returns a target's events from since on, oldest first.
func (r *Repository) Events(ctx context.Context, tid string, since time.Time) ([]Event, error) {
	keys, err := r.listKeys(ctx, eventPrefix(tid))
	if err != nil {
		return nil, err
	}
	from := eventPrefix(tid) + sortableTime(since)
	var out []Event
	for _, k := range keys {
		if k < from {
			continue
		}
		var e Event
		if err := r.getMeta(ctx, k, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
