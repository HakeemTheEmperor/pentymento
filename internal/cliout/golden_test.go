// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

// golden compares got with testdata/<name>.golden, or rewrites the file
// with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// statusPrinter renders the Brand 10.5 status example: a table on stdout,
// then a warning with its next step on stderr.
func statusOutput(unicode, color bool, width int) (string, string) {
	var stdout, stderr bytes.Buffer
	p := &Printer{Stdout: &stdout, Stderr: &stderr, Unicode: unicode, ColorOut: color, ColorErr: color, Width: width}
	_ = p.Table(Table{
		Columns: []Column{
			{Header: "TARGET", Priority: 5},
			{Header: "ENGINE", Priority: 2},
			{Header: "AGENT", Priority: 4},
			{Header: "PITR WINDOW", Priority: 3},
			{Header: "LAST VERIFY", Priority: 1},
		},
		Rows: [][]Cell{
			{{Text: "prod-pg", Style: Bold}, {Text: "postgres 17"}, p.Status(msg.LevelOK, "capturing (4s)"),
				{Text: "7d 0h · up to 4s ago"}, p.Status(msg.LevelOK, "standard, 3d ago")},
			{{Text: "prod-mg", Style: Bold}, {Text: "mongodb 8.0"}, p.Status(msg.LevelWarning, "stale (2h 14m)"),
				{Text: "6d 22h · up to 14:02 WAT"}, p.Status(msg.LevelOK, "quick, 1d ago")},
		},
	})
	p.Message(msg.Message{
		Level:  msg.LevelWarning,
		Target: "prod-mg",
		Fact:   "agent last seen 2h 14m ago; changes since 14:02 WAT are not protected",
		Next:   "pentym catch-up prod-mg",
	})
	return stdout.String(), stderr.String()
}

func TestStatusGolden(t *testing.T) {
	tests := []struct {
		name    string
		unicode bool
		color   bool
		width   int
	}{
		{"status_unicode", true, false, 0},
		{"status_ascii", false, false, 0},
		{"status_color", true, true, 0},
		{"status_narrow", true, false, 70},
		{"status_very_narrow", true, false, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr := statusOutput(tt.unicode, tt.color, tt.width)
			golden(t, tt.name, "-- stdout --\n"+stdout+"-- stderr --\n"+stderr)
		})
	}
}

// brandMessages are the Brand 11.4 examples.
var brandMessages = []msg.Message{
	{
		Level: msg.LevelFailed, ID: "PTM-2001", Target: "prod-pg", Fact: "cannot connect to 10.0.0.5:5432",
		Cause: `password authentication failed for user "pentym_backup"`,
		Next:  "check the password reference for prod-pg.backup in config, then run: pentym doctor prod-pg",
	},
	{
		Level: msg.LevelWarning, Target: "prod-pg", Fact: "role pentym_repl is a superuser; it only needs REPLICATION",
		Next: "pentym init --print-sql   (prints the minimal role definition)",
	},
	{
		Level: msg.LevelFailed, Target: "prod-pg", Fact: "WAL missing from 14:10 to 14:52 WAT (slot invalidated by max_slot_wal_keep_size)",
		Impact: "point-in-time recovery currently covers only points after 14:52 WAT",
		Next:   "pentym backup prod-pg   (starts a fresh base)",
	},
	{
		Level: msg.LevelOK, Target: "prod-pg", Fact: "snapshot committed",
	},
	{
		Level: msg.LevelFailed, Fact: "unknown flag: --frobnicate", Next: "pentym backup --help",
	},
}

func messagesOutput(unicode, color bool, v Verbosity) string {
	var stderr bytes.Buffer
	p := &Printer{Stdout: &bytes.Buffer{}, Stderr: &stderr, Unicode: unicode, ColorErr: color, Verbosity: v}
	for _, m := range brandMessages {
		p.Message(m)
	}
	return stderr.String()
}

func TestMessagesGolden(t *testing.T) {
	tests := []struct {
		name    string
		unicode bool
		color   bool
		v       Verbosity
	}{
		{"messages_unicode", true, false, Normal},
		{"messages_ascii", false, false, Normal},
		{"messages_color", true, true, Normal},
		{"messages_verbose", true, false, Verbose},
		{"messages_quiet", true, false, Quiet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			golden(t, tt.name, messagesOutput(tt.unicode, tt.color, tt.v))
		})
	}
}

func TestFailJSONGolden(t *testing.T) {
	var stdout, stderr bytes.Buffer
	p := &Printer{Stdout: &stdout, Stderr: &stderr, JSON: true, Unicode: true, ColorErr: true}
	p.Fail(brandMessages[0])
	p.Fail(brandMessages[4])
	if stderr.Len() != 0 {
		t.Errorf("Fail with JSON wrote to stderr: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "\x1b[") {
		t.Error("JSON output contains colour codes")
	}
	golden(t, "fail_json", stdout.String())
}

func TestFailHuman(t *testing.T) {
	var stdout, stderr bytes.Buffer
	p := &Printer{Stdout: &stdout, Stderr: &stderr, Unicode: true, Verbosity: Quiet}
	p.Fail(brandMessages[2])
	if stdout.Len() != 0 {
		t.Errorf("Fail wrote to stdout: %q", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "✗ prod-pg: WAL missing") {
		t.Errorf("Fail in quiet mode did not print the failure: %q", stderr.String())
	}
}
