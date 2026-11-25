// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package crypto

import (
	"fmt"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// FullSlots counts the slots that unlock everything (paper slots included).
func FullSlots(slots []*Slot) int {
	n := 0
	for _, s := range slots {
		if s.Scope == ScopeFull {
			n++
		}
	}
	return n
}

// EnoughFullSlots reports whether losing one way of unlocking would still
// leave the repository readable: init requires two full slots, or one and
// a paper key (PRD 8.2).
func EnoughFullSlots(slots []*Slot) bool { return FullSlots(slots) >= 2 }

// CheckRemove refuses to remove the last full slot, which would make every
// backup unreadable.
func CheckRemove(slots []*Slot, id string) error {
	var target *Slot
	for _, s := range slots {
		if s.ID == id {
			target = s
		}
	}
	if target == nil {
		return &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
			Level: msg.LevelFailed, Fact: fmt.Sprintf("no key slot %s", id), Next: "pentym key list",
		}}
	}
	if target.Scope == ScopeFull && FullSlots(slots) == 1 {
		return &msg.Error{Code: msg.ExitAccess, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDLastFullSlot,
			Fact:   fmt.Sprintf("key slot %s (%s) is the last full slot", id, target.Label),
			Impact: "removing it would make every backup unreadable",
			Next:   "pentym key add (add another full slot or a paper key first)",
		}}
	}
	return nil
}

// CheckInsecureTestMode allows an unencrypted repository only on local
// storage (PRD 8.1).
func CheckInsecureTestMode(localBackend bool) error {
	if localBackend {
		return nil
	}
	return &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
		Level: msg.LevelFailed, ID: IDInsecureNotLocal,
		Fact:   "--insecure-test-mode works only with a local repository",
		Impact: "an unencrypted repository on remote storage would expose every backup",
		Next:   "use a local path for development, or drop --insecure-test-mode",
	}}
}
