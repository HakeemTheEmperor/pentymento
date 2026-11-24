// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package msg

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"
)

func TestErrorFormatting(t *testing.T) {
	cause := errors.New("dial tcp: refused")
	e := &Error{
		Code: ExitConnect,
		Message: Message{
			Level: LevelFailed, Target: "prod-pg", Fact: "cannot connect to 10.0.0.5:5432",
			Cause: "connection refused",
		},
		Err: cause,
	}
	if got, want := e.Error(), "prod-pg: cannot connect to 10.0.0.5:5432: connection refused"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(e, cause) {
		t.Error("errors.Is does not reach the underlying error")
	}
	var me *Error
	if !errors.As(fmt.Errorf("wrapped: %w", e), &me) || me.Code != ExitConnect {
		t.Error("errors.As does not find the *Error through wrapping")
	}
}

func TestHeadline(t *testing.T) {
	if got := (Message{Target: "prod-mg", Fact: "agent stale"}).Headline(); got != "prod-mg: agent stale" {
		t.Errorf("Headline with target = %q", got)
	}
	if got := (Message{Fact: "agent stale"}).Headline(); got != "agent stale" {
		t.Errorf("Headline without target = %q", got)
	}
}

func TestLevelString(t *testing.T) {
	for l, want := range map[Level]string{LevelOK: "ok", LevelWarning: "warning", LevelFailed: "failed", Level(9): "unknown"} {
		if got := l.String(); got != want {
			t.Errorf("Level(%d).String() = %q, want %q", l, got, want)
		}
	}
}

// TestExitCodesStable pins every exit code to its number. Changing a number
// breaks scripts that rely on it; add new codes instead.
func TestExitCodesStable(t *testing.T) {
	want := map[ExitCode]int{
		ExitOK: 0, ExitFailure: 1, ExitUsage: 2, ExitConfig: 3, ExitNotFound: 4,
		ExitConnect: 5, ExitAccess: 6, ExitIntegrity: 7, ExitUnrecoverable: 8,
		ExitDeclined: 9, ExitNotImplemented: 10, ExitInterrupted: 130,
	}
	if len(ExitCodes) != len(want) {
		t.Fatalf("ExitCodes has %d entries, want %d", len(ExitCodes), len(want))
	}
	seen := map[ExitCode]bool{}
	for _, c := range ExitCodes {
		if n, ok := want[c.Code]; !ok || int(c.Code) != n {
			t.Errorf("exit code %d is not in the stable table", c.Code)
		}
		if seen[c.Code] {
			t.Errorf("exit code %d listed twice", c.Code)
		}
		seen[c.Code] = true
	}
}

// TestExitCodesDocumented checks that docs/reference/exit-codes.md lists
// exactly the codes defined here.
func TestExitCodesDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/reference/exit-codes.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[int]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\| (\d+) \|`).FindAllStringSubmatch(string(doc), -1) {
		n, _ := strconv.Atoi(m[1])
		documented[n] = true
	}
	for _, c := range ExitCodes {
		if !documented[int(c.Code)] {
			t.Errorf("exit code %d is missing from docs/reference/exit-codes.md", c.Code)
		}
		delete(documented, int(c.Code))
	}
	for n := range documented {
		t.Errorf("docs/reference/exit-codes.md lists exit code %d, which is not defined", n)
	}
}

func TestRegistryLookup(t *testing.T) {
	saved := entries
	t.Cleanup(func() { entries = saved })
	entries = []Entry{{ID: "PTM-2001", Name: "connection-refused"}}

	e, ok := Lookup("PTM-2001")
	if !ok || e.Name != "connection-refused" {
		t.Fatalf("Lookup(PTM-2001) = %+v, %v", e, ok)
	}
	if got, want := e.DocsURL(), "https://pentymento.dev/errors/PTM-2001"; got != want {
		t.Errorf("DocsURL = %q, want %q", got, want)
	}
	if _, ok := Lookup("PTM-9999"); ok {
		t.Error("Lookup found an unregistered ID")
	}
	all := Entries()
	all[0].Name = "changed"
	if entries[0].Name != "connection-refused" {
		t.Error("Entries returned the registry itself, not a copy")
	}
}
