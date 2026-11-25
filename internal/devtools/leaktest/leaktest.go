// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package leaktest is the secret-leak test harness (PRD 8.3, 10.1). Tests
// create known fake secrets ("canaries"), feed them to pentym through every
// secret path, and scan everything pentym wrote (stdout, stderr, logs,
// error text, and later journal entries and notifications) for them.
//
// Tests using it are named TestSecretLeak…, so `make leaktest` runs them
// all.
package leaktest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// Canary is a fake secret with a distinctive, random value.
type Canary string

// New returns n distinct canaries.
func New(t testing.TB, n int) []Canary {
	t.Helper()
	out := make([]Canary, n)
	for i := range out {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		out[i] = Canary("pentym-leak-canary-" + hex.EncodeToString(b))
	}
	return out
}

// forms returns the encodings in which a leaked canary would show up:
// as is, hex, base64 and as Go prints a byte slice.
func (c Canary) forms() map[string]string {
	b := []byte(c)
	return map[string]string{
		"plain":      string(c),
		"hex":        hex.EncodeToString(b),
		"base64":     base64.StdEncoding.EncodeToString(b),
		"base64url":  base64.RawURLEncoding.EncodeToString(b),
		"byte slice": strings.Trim(fmt.Sprintf("%d", b), "[]"),
	}
}

// Scan fails the test if any canary appears, in any form, in any output.
// label names what was scanned, for the failure message.
func Scan(t testing.TB, label string, canaries []Canary, outputs ...string) {
	t.Helper()
	for _, out := range outputs {
		for _, c := range canaries {
			for form, v := range c.forms() {
				if strings.Contains(out, v) {
					t.Errorf("secret leak in %s: canary found (%s form)\n%s", label, form, out)
				}
			}
		}
	}
}
