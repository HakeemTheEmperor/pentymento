// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// pinsPrefix holds pin records: pins/<snapshot-id>/<time>-<rand>. A
// snapshot never changes, so pinning and unpinning add records, and the
// newest record for a snapshot is its state.
const pinsPrefix = "pins/"

func pinPrefix(snapshotID string) string { return pinsPrefix + snapshotID + "/" }

func pinKey(snapshotID string, t time.Time) string {
	return fmt.Sprintf("%s%s-%s", pinPrefix(snapshotID), sortableTime(t.UTC()), randomHex(4))
}

// Pin is a snapshot's pin state (PRD 7.10): a pinned snapshot is kept
// whatever the retention rules say, until it is unpinned.
type Pin struct {
	Snapshot string    `json:"snapshot"`
	Pinned   bool      `json:"pinned"`
	Reason   string    `json:"reason,omitempty"`
	Who      string    `json:"who"`
	Host     string    `json:"host"`
	Time     time.Time `json:"time"`
}

// SetPin records that a snapshot is pinned (with a reason) or unpinned.
// The snapshot must exist.
func (r *Repository) SetPin(ctx context.Context, id Identity, snapshotID string, pinned bool, reason string) error {
	if b, err := hex.DecodeString(snapshotID); err != nil || len(b) != 16 {
		return fmt.Errorf("snapshot ID %q is malformed", snapshotID)
	}
	if _, err := r.be.Stat(ctx, snapshotKey(snapshotID)); err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return &msg.Error{Code: msg.ExitNotFound, Err: err, Message: msg.Message{
				Level: msg.LevelFailed, Fact: "no snapshot " + snapshotID, Next: "pentym list",
			}}
		}
		return err
	}
	now := r.now().UTC()
	p := Pin{Snapshot: snapshotID, Pinned: pinned, Reason: reason, Who: id.User, Host: id.Host, Time: now}
	return r.putMeta(ctx, pinKey(snapshotID, now), p, backend.PutOptions{IfNotExists: true})
}

// Pins returns every snapshot's current pin state: its newest pin record.
// Snapshots that were never pinned are absent.
func (r *Repository) Pins(ctx context.Context) (map[string]Pin, error) {
	keys, err := r.listKeys(ctx, pinsPrefix)
	if err != nil {
		return nil, err
	}
	newest := map[string]string{}
	for _, k := range keys {
		snap, _, ok := strings.Cut(strings.TrimPrefix(k, pinsPrefix), "/")
		if !ok {
			continue
		}
		if k > newest[snap] {
			newest[snap] = k
		}
	}
	out := make(map[string]Pin, len(newest))
	for snap, k := range newest {
		var p Pin
		if err := r.getMeta(ctx, k, &p); err != nil {
			return nil, err
		}
		if p.Snapshot != snap {
			return nil, fmt.Errorf("%s: %w: it records snapshot %s", k, ErrCorrupt, p.Snapshot)
		}
		out[snap] = p
	}
	return out, nil
}
