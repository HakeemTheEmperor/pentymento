// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func TestPins(t *testing.T) {
	d := newDataRepo(t)
	ctx := context.Background()
	r := d.open(t)
	clock := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	id := Identity{User: "ada", Host: "db-1"}

	if pins, err := r.Pins(ctx); err != nil || len(pins) != 0 {
		t.Fatalf("Pins of a new repository = %v, %v", pins, err)
	}
	if err := r.SetPin(ctx, id, d.snapA.ID, true, "pre-migration"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPin(ctx, id, d.snapB.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPin(ctx, id, d.snapB.ID, false, ""); err != nil { // the newest record wins
		t.Fatal(err)
	}
	pins, err := d.open(t).Pins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, b := pins[d.snapA.ID], pins[d.snapB.ID]
	if len(pins) != 2 || !a.Pinned || a.Reason != "pre-migration" || a.Who != "ada" || a.Host != "db-1" || a.Snapshot != d.snapA.ID || b.Pinned {
		t.Errorf("pins = %+v", pins)
	}

	// An agent slot can read and write pins too: they use the metadata key.
	agent := d.reopen(t, d.agent, "agent")
	if p, err := agent.Pins(ctx); err != nil || len(p) != 2 {
		t.Errorf("agent Pins = %v, %v", p, err)
	}

	var me *msg.Error
	if err := r.SetPin(ctx, id, "00112233445566778899aabbccddeeff", true, ""); !errors.As(err, &me) || me.Code != msg.ExitNotFound {
		t.Errorf("pinning a missing snapshot = %v", err)
	}
	if err := r.SetPin(ctx, id, "not-an-id", true, ""); err == nil {
		t.Error("pinning a malformed ID succeeded")
	}
	// A record moved under another snapshot is caught.
	keys := listKeysOf(t, d.be, pinPrefix(d.snapA.ID))
	b2, err := readAll(ctx, d.be, keys[0], 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	moved := pinPrefix(d.snapB.ID) + "9999999999999999999-00000000"
	if err := d.be.Put(ctx, moved, bytes.NewReader(b2), backend.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pins(ctx); err == nil {
		t.Error("a pin record moved under another snapshot was accepted")
	}
}
