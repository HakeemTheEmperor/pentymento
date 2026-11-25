// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// ageIdentityPrefix starts an age X25519 identity; any other secret is a
// passphrase or a paper recovery code.
const ageIdentityPrefix = "AGE-SECRET-KEY-"

// UnlockOptions choose which key slots UnlockWith tries.
type UnlockOptions struct {
	// SlotID tries only this slot; empty tries every slot that fits.
	SlotID string
	// NeedFull skips agent slots, for operations that read data or manage
	// keys.
	NeedFull bool
}

// IsAgeIdentity reports whether a secret is an age identity rather than a
// passphrase or recovery code.
func IsAgeIdentity(s secrets.Secret) bool {
	return bytes.HasPrefix(bytes.TrimSpace(s.Expose()), []byte(ageIdentityPrefix))
}

// UnlockWith tries secret against the repository's key slots: an age
// identity against age slots, anything else against passphrase and paper
// slots. The first slot that opens unlocks the repository; its keys and the
// slot are returned. Every attempt on a passphrase slot costs one Argon2id
// derivation, so SlotID saves time when there are many slots.
func (r *Repository) UnlockWith(ctx context.Context, secret secrets.Secret, opts UnlockOptions) (*crypto.Unlocked, *crypto.Slot, error) {
	slots, err := r.Slots(ctx)
	if err != nil {
		return nil, nil, err
	}
	age := IsAgeIdentity(secret)
	what := "passphrase or recovery code"
	if age {
		what = "age identity"
	}
	var candidates []*crypto.Slot
	for _, s := range slots {
		if opts.SlotID != "" && s.ID != opts.SlotID {
			continue
		}
		if opts.NeedFull && s.Scope != crypto.ScopeFull {
			continue
		}
		if (s.Kind == crypto.KindAge) != age {
			continue
		}
		candidates = append(candidates, s)
	}
	if len(candidates) == 0 {
		return nil, nil, noSlotFor(slots, opts, what)
	}
	var last error
	for _, s := range candidates {
		var u *crypto.Unlocked
		if age {
			u, err = s.UnlockAge(secret)
		} else {
			u, err = s.UnlockPassphrase(secret)
		}
		if err != nil {
			last = err
			continue
		}
		if err := r.Unlock(ctx, *u); err != nil {
			return nil, nil, err
		}
		return u, s, nil
	}
	if len(candidates) == 1 {
		return nil, nil, last
	}
	scope := "key slot"
	if opts.NeedFull {
		scope = "full key slot"
	}
	return nil, nil, &msg.Error{Code: msg.ExitAccess, Err: crypto.ErrDecrypt, Message: msg.Message{
		Level: msg.LevelFailed, ID: crypto.IDUnlockFailed,
		Fact: fmt.Sprintf("no %s unlocked with this %s (%d tried)", scope, what, len(candidates)),
		Next: "check the " + what + ", or see the slots with pentym key list",
	}}
}

// noSlotFor explains why no slot could be tried.
func noSlotFor(slots []*crypto.Slot, opts UnlockOptions, what string) error {
	if opts.SlotID != "" {
		for _, s := range slots {
			if s.ID != opts.SlotID {
				continue
			}
			fact := fmt.Sprintf("key slot %s (%s) is a %s %s slot, which %s cannot unlock", s.ID, s.Label, s.Scope, s.Kind, article(what))
			if opts.NeedFull && s.Scope != crypto.ScopeFull {
				fact = fmt.Sprintf("key slot %s (%s) is an agent slot; this needs a full slot", s.ID, s.Label)
			}
			return &msg.Error{Code: msg.ExitAccess, Err: errNoSlot, Message: msg.Message{
				Level: msg.LevelFailed, ID: crypto.IDUnlockFailed, Fact: fact,
				Next: "choose another slot from pentym key list",
			}}
		}
		return &msg.Error{Code: msg.ExitNotFound, Err: errNoSlot, Message: msg.Message{
			Level: msg.LevelFailed, Fact: fmt.Sprintf("no key slot %s", opts.SlotID), Next: "pentym key list",
		}}
	}
	scope := "key slot"
	if opts.NeedFull {
		scope = "full key slot"
	}
	return &msg.Error{Code: msg.ExitAccess, Err: errNoSlot, Message: msg.Message{
		Level: msg.LevelFailed, ID: crypto.IDUnlockFailed,
		Fact: fmt.Sprintf("this repository has no %s that %s can unlock", scope, article(what)),
		Next: "unlock with another kind of secret, or see the slots with pentym key list",
	}}
}

var errNoSlot = errors.New("no matching key slot")

// article prefixes "a" or "an".
func article(what string) string {
	if strings.HasPrefix(what, "a") {
		return "an " + what
	}
	return "a " + what
}
