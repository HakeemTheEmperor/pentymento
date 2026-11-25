// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package lock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// Lock and lease records live under locks/ as plaintext JSON: they hold
// no secrets (SDD 5.2). They are created with conditional create and then
// refreshed and released by overwriting, because holders with write-only
// storage credentials cannot delete; prune removes released and stale
// records.
const locksPrefix = "locks/"

// recordVersion is the lock record format.
const recordVersion = 1

// Record kinds.
const (
	KindShared    = "shared"
	KindExclusive = "exclusive"
	KindLease     = "lease"
	KindClaim     = "claim"
)

// Record states.
const (
	StateHeld     = "held"
	StateReleased = "released"
)

// Holder identifies who holds a lock or lease (PRD 5.4).
type Holder struct {
	User    string `json:"user"`
	Host    string `json:"host"`
	PID     int    `json:"pid"`
	AgentID string `json:"agent_id,omitempty"`
}

// CurrentHolder returns this process's identity, with an optional agent ID.
func CurrentHolder(user, host, agentID string) Holder {
	return Holder{User: user, Host: host, PID: os.Getpid(), AgentID: agentID}
}

// Record is one lock or lease object.
type Record struct {
	Version    int    `json:"version"`
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Generation uint64 `json:"generation,omitempty"`
	Seq        uint64 `json:"seq"`
	State      string `json:"state"`
	Holder     Holder `json:"holder"`
	// Time is the writer's wall clock, for people reading the record. It is
	// never compared across hosts.
	Time time.Time `json:"time"`
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func putRecord(ctx context.Context, be backend.Backend, key string, rec Record, create bool) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return be.Put(ctx, key, bytes.NewReader(b), backend.PutOptions{IfNotExists: create})
}

// getRecord reads a record and its storage modification time.
func getRecord(ctx context.Context, be backend.Backend, key string) (Record, error) {
	rc, err := be.Get(ctx, key, nil)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, fmt.Errorf("%s is malformed: %w", key, err)
	}
	return rec, nil
}

// storageNow returns the storage's current time: the modification time of
// a probe object written now. Lock staleness compares it with a lock's
// modification time, both set by the storage, so host clocks never meet.
func storageNow(ctx context.Context, be backend.Backend) (time.Time, error) {
	key := locksPrefix + "probe-" + randomID()
	if err := be.Put(ctx, key, bytes.NewReader([]byte("{}")), backend.PutOptions{}); err != nil {
		return time.Time{}, err
	}
	oi, err := be.Stat(ctx, key)
	_ = be.Delete(ctx, key) // best effort: write-only credentials cannot delete; prune can
	if err != nil {
		return time.Time{}, err
	}
	return oi.ModTime, nil
}

// deleteOrRelease deletes a record, or, where the credentials cannot
// delete, overwrites it as released.
func deleteOrRelease(ctx context.Context, be backend.Backend, key string, rec Record) error {
	rec.State = StateReleased
	rec.Seq++
	rec.Time = time.Now().UTC()
	if err := putRecord(ctx, be, key, rec, false); err != nil {
		return err
	}
	_ = be.Delete(ctx, key) // released is enough when deletion is not allowed
	return nil
}
