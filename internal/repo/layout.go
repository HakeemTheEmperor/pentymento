// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Object keys in the repository (SDD 5.1, docs/repository-format.md). Every
// name that varies is built here.
const (
	configKey        = "config"
	keysPrefix       = "keys/"
	slotPrefix       = keysPrefix + "slot-"
	privateKeyObject = keysPrefix + "private"
	publicKeyObject  = keysPrefix + "public"
	targetsPrefix    = "targets/"
	eventsPrefix     = "events/"
	heartbeatPrefix  = "heartbeat/"
	journalPrefix    = "journal/"
)

func slotKey(id string) string { return slotPrefix + id }

func targetInfoKey(tid string) string { return targetsPrefix + tid + "/info" }

func checkpointPrefix(tid string) string { return targetsPrefix + tid + "/checkpoints/" }

// checkpointKey sorts by lease generation, then by sequence.
func checkpointKey(tid string, generation, seq uint64) string {
	return fmt.Sprintf("%s%020d-%020d", checkpointPrefix(tid), generation, seq)
}

func eventPrefix(tid string) string { return eventsPrefix + tid + "/" }

// eventKey sorts by the event's time.
func eventKey(tid string, t time.Time) string {
	return fmt.Sprintf("%s%s-%s", eventPrefix(tid), sortableTime(t), randomHex(4))
}

func heartbeatHostPrefix(host string) string {
	return heartbeatPrefix + "agent-" + keySafe(host) + "/"
}

func heartbeatKey(host string, t time.Time) string {
	return heartbeatHostPrefix(host) + sortableTime(t)
}

// journalKey groups entries by month and sorts them by start time.
func journalKey(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%s%04d/%02d/%s-%s", journalPrefix, t.Year(), t.Month(), sortableTime(t), randomHex(4))
}

// sortableTime is Unix nanoseconds as 19 zero-padded digits, so keys sort
// by time.
func sortableTime(t time.Time) string { return fmt.Sprintf("%019d", t.UnixNano()) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// keySafe maps a host name to key characters: anything outside
// A–Z a–z 0–9 . _ - becomes "-", and a leading "." becomes "-".
func keySafe(s string) string {
	var b strings.Builder
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || (c == '.' && i > 0)
		if !ok {
			c = '-'
		}
		b.WriteRune(c)
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
