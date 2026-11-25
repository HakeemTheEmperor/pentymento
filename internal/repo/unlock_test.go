// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// unlockFixture is a repository with a full passphrase slot ("admin"), an
// agent slot ("agent"), a second full passphrase slot ("other"), a full
// age slot and a paper slot.
type unlockFixture struct {
	*fixture
	other, ageSlot, paper *crypto.Slot
	identity, paperCode   string
}

func newUnlockFixture(t *testing.T) *unlockFixture {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	u := &unlockFixture{fixture: f}
	keys := f.material.Unlocked()
	var err error
	if u.other, err = crypto.NewPassphraseSlot("other", keys, secrets.New([]byte("other")), fastKDF); err != nil {
		t.Fatal(err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	u.identity = id.String()
	if u.ageSlot, err = crypto.NewAgeSlot("ci", keys, id.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	var code secrets.Secret
	if u.paper, code, err = crypto.NewPaperSlot("paper", keys, fastKDF); err != nil {
		t.Fatal(err)
	}
	u.paperCode = string(code.Expose())
	for _, s := range []*crypto.Slot{u.other, u.ageSlot, u.paper} {
		if err := f.repo.AddSlot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

func (u *unlockFixture) open(t *testing.T) *Repository {
	t.Helper()
	r, err := Open(context.Background(), u.be)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUnlockWithFindsTheSlot(t *testing.T) {
	u := newUnlockFixture(t)
	ctx := context.Background()
	tests := []struct {
		name, secret string
		opts         UnlockOptions
		slot         *crypto.Slot
		canRead      bool
	}{
		{"first full passphrase", "admin", UnlockOptions{}, u.full, true},
		{"second full passphrase", "other", UnlockOptions{}, u.other, true},
		{"agent passphrase", "agent", UnlockOptions{}, u.agent, false},
		{"age identity", u.identity, UnlockOptions{}, u.ageSlot, true},
		{"age identity with a newline", u.identity + "\n", UnlockOptions{NeedFull: true}, u.ageSlot, true},
		{"paper code", u.paperCode, UnlockOptions{NeedFull: true}, u.paper, true},
		{"paper code typed in lower case without dashes", strings.ToLower(strings.ReplaceAll(u.paperCode, "-", "")), UnlockOptions{}, u.paper, true},
		{"one named slot", "other", UnlockOptions{SlotID: u.other.ID, NeedFull: true}, u.other, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := u.open(t)
			keys, slot, err := r.UnlockWith(ctx, secrets.New([]byte(tt.secret)), tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if slot.ID != tt.slot.ID || keys.Scope != tt.slot.Scope {
				t.Errorf("unlocked slot %s (%s), want %s (%s)", slot.ID, keys.Scope, tt.slot.ID, tt.slot.Scope)
			}
			if r.CanRead() != tt.canRead {
				t.Errorf("CanRead = %v, want %v", r.CanRead(), tt.canRead)
			}
			if _, err := r.TargetID("prod-pg"); err != nil {
				t.Errorf("unlocked repository cannot use the metadata key: %v", err)
			}
		})
	}
}

func TestUnlockWithFailures(t *testing.T) {
	u := newUnlockFixture(t)
	ctx := context.Background()
	tests := []struct {
		name, secret string
		opts         UnlockOptions
		code         msg.ExitCode
		fact         string
	}{
		{"wrong passphrase", "nope", UnlockOptions{}, msg.ExitAccess,
			"no key slot unlocked with this passphrase or recovery code (4 tried)"},
		{"agent passphrase where a full slot is needed", "agent", UnlockOptions{NeedFull: true}, msg.ExitAccess,
			"no full key slot unlocked with this passphrase or recovery code (3 tried)"},
		{"wrong passphrase for a named slot", "admin", UnlockOptions{SlotID: u.other.ID}, msg.ExitAccess,
			"key slot " + u.other.ID + " (other) did not unlock: wrong passphrase"},
		{"named agent slot where a full slot is needed", "admin", UnlockOptions{SlotID: u.agent.ID, NeedFull: true}, msg.ExitAccess,
			"key slot " + u.agent.ID + " (agent) is an agent slot; this needs a full slot"},
		{"passphrase for a named age slot", "admin", UnlockOptions{SlotID: u.ageSlot.ID}, msg.ExitAccess,
			"key slot " + u.ageSlot.ID + " (ci) is a full age slot, which a passphrase or recovery code cannot unlock"},
		{"unknown slot", "admin", UnlockOptions{SlotID: "0011223344556677"}, msg.ExitNotFound,
			"no key slot 0011223344556677"},
		{"wrong age identity", mustIdentity(t), UnlockOptions{}, msg.ExitAccess,
			"key slot " + u.ageSlot.ID + " (ci) did not unlock: wrong age identity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := u.open(t)
			_, _, err := r.UnlockWith(ctx, secrets.New([]byte(tt.secret)), tt.opts)
			var me *msg.Error
			if !errors.As(err, &me) || me.Code != tt.code || me.Message.Fact != tt.fact || me.Message.Next == "" {
				t.Fatalf("UnlockWith = %v (%+v), want exit %d and %q", err, me, tt.code, tt.fact)
			}
			if strings.Contains(me.Error(), tt.secret) {
				t.Errorf("error shows the secret: %v", me)
			}
			if r.CanRead() {
				t.Error("repository unlocked after a failure")
			}
		})
	}
}

// TestUnlockWithNoAgeSlot: an age identity against a repository without
// age slots says so instead of trying passphrase slots with it.
func TestUnlockWithNoAgeSlot(t *testing.T) {
	f := newFixture(t)
	r, err := Open(context.Background(), f.be)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = r.UnlockWith(context.Background(), secrets.New([]byte(mustIdentity(t))), UnlockOptions{})
	var me *msg.Error
	if !errors.As(err, &me) || me.Message.Fact != "this repository has no key slot that an age identity can unlock" {
		t.Fatalf("UnlockWith(age, no age slot) = %v", err)
	}
}

func mustIdentity(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}
