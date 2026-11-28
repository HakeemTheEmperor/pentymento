// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// result is the outcome of one pentym invocation.
type result struct {
	code           msg.ExitCode
	stdout, stderr string
}

// pentym runs the CLI in-process with no terminal and an environment that
// has only a UTF-8 locale.
func pentym(t *testing.T, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := func(k string) string {
		if k == "LANG" {
			return "en_US.UTF-8"
		}
		return ""
	}
	code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, env)
	return result{code, stdout.String(), stderr.String()}
}

// TestRunReturnsZero: with no arguments pentym succeeds, prints its help on
// stdout and nothing on stderr.
func TestRunReturnsZero(t *testing.T) {
	r := pentym(t)
	if r.code != msg.ExitOK {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "Usage:") || r.stderr != "" {
		t.Fatalf("stdout = %q\nstderr = %q", r.stdout, r.stderr)
	}
}

// prdCommands is every command in PRD 7.15, with its subcommands.
var prdCommands = map[string][]string{
	"init": nil, "backup": nil, "catch-up": nil, "restore": nil, "events": nil,
	"list": nil, "status": nil, "verify": nil, "check": nil, "prune": nil,
	"pin": nil, "dump": nil, "history": nil, "doctor": nil,
	"agent":  {"install", "uninstall", "start", "stop", "reload"},
	"key":    {"add", "list", "remove", "rotate", "export"},
	"notify": {"test"},
	"debug":  {"bundle"},
}

func TestHelpListsEveryCommand(t *testing.T) {
	root := pentym(t, "--help")
	if root.code != msg.ExitOK {
		t.Fatalf("--help exit code = %d", root.code)
	}
	for name, subs := range prdCommands {
		if !strings.Contains(root.stdout, "\n  "+name+" ") {
			t.Errorf("pentym --help does not list %q", name)
		}
		if len(subs) == 0 {
			continue
		}
		help := pentym(t, name, "--help")
		for _, sub := range subs {
			if !strings.Contains(help.stdout, "\n  "+sub+" ") {
				t.Errorf("pentym %s --help does not list %q", name, sub)
			}
		}
	}
}

// walk visits every command below the root.
func walk(c *cobra.Command, fn func(*cobra.Command)) {
	for _, sub := range c.Commands() {
		if sub.Name() == "help" {
			continue
		}
		fn(sub)
		walk(sub, fn)
	}
}

func TestEveryCommandHasHelpAndExamples(t *testing.T) {
	root := newApp(nil, &bytes.Buffer{}, &bytes.Buffer{}, nil).rootCommand()
	if root.Example == "" {
		t.Error("pentym has no examples")
	}
	walk(root, func(c *cobra.Command) {
		if c.Short == "" {
			t.Errorf("%s has no description", c.CommandPath())
		}
		if !strings.Contains(c.Example, "  pentym ") {
			t.Errorf("%s has no examples", c.CommandPath())
		}
		if strings.Contains(c.Example, "$ ") {
			t.Errorf("%s examples contain a prompt character", c.CommandPath())
		}
	})
}

// stubArgs returns placeholder arguments for a command's required
// positional arguments, read from its Use line.
func stubArgs(c *cobra.Command) []string {
	var a []string
	for _, f := range strings.Fields(c.Use)[1:] {
		if strings.HasPrefix(f, "<") {
			a = append(a, "x")
		}
	}
	return a
}

// implemented lists the commands that are built; every other command is a
// stub.
var implemented = map[string]bool{
	"pentym backup":             true,
	"pentym list":               true,
	"pentym check":              true,
	"pentym doctor":             true,
	"pentym key rotate":         true,
	"pentym repo upgrade":       true,
	"pentym repo rebuild-index": true,
	"pentym init":               true,
	"pentym key add":            true,
	"pentym key list":           true,
	"pentym key remove":         true,
	"pentym key export":         true,
	"pentym dump":               true,
	"pentym agent":              true,
	"pentym agent reload":       true,
	"pentym catch-up":           true,
	"pentym status":             true,
}

func TestStubsAreNotImplemented(t *testing.T) {
	root := newApp(nil, &bytes.Buffer{}, &bytes.Buffer{}, nil).rootCommand()
	walk(root, func(c *cobra.Command) {
		if c.HasSubCommands() && c.Name() != "agent" {
			return // groups need a subcommand
		}
		path := strings.Fields(c.CommandPath())[1:]
		r := pentym(t, append(path, stubArgs(c)...)...)
		if implemented[c.CommandPath()] {
			if r.code == msg.ExitNotImplemented || strings.Contains(r.stderr, "not implemented") {
				t.Errorf("%s is built but reports not implemented: %s", c.CommandPath(), r.stderr)
			}
			return
		}
		if r.code != msg.ExitNotImplemented {
			t.Errorf("%s: exit code = %d, want %d\nstderr: %s", c.CommandPath(), r.code, msg.ExitNotImplemented, r.stderr)
		}
		want := "✗ " + c.CommandPath() + " is not implemented in this build\n→ pentym --help\n"
		if r.stderr != want || r.stdout != "" {
			t.Errorf("%s:\nstdout = %q\nstderr = %q\nwant stderr %q", c.CommandPath(), r.stdout, r.stderr, want)
		}
	})
}

func TestVersion(t *testing.T) {
	r := pentym(t, "--version")
	if r.code != msg.ExitOK || r.stderr != "" {
		t.Fatalf("--version: code %d, stderr %q", r.code, r.stderr)
	}
	v := currentVersion()
	if r.stdout != v.String()+"\n" || !strings.HasPrefix(r.stdout, "pentym dev (commit ") {
		t.Errorf("--version = %q", r.stdout)
	}

	r = pentym(t, "--version", "--json")
	var got versionInfo
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("--version --json is not JSON: %v\n%s", err, r.stdout)
	}
	if got != v {
		t.Errorf("--version --json = %+v, want %+v", got, v)
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"unknown command", []string{"frobnicate"},
			"✗ unknown command \"frobnicate\" for \"pentym\"\n→ pentym --help\n"},
		{"unknown flag", []string{"backup", "prod-pg", "--frobnicate"},
			"✗ unknown flag: --frobnicate\n→ pentym backup --help\n"},
		{"missing target", []string{"backup"},
			"✗ pentym backup needs <target>\n→ pentym backup --help\n"},
		{"too many arguments", []string{"backup", "a", "b"},
			"✗ pentym backup takes one argument, <target>, got 2 arguments\n→ pentym backup --help\n"},
		{"argument to a command without arguments", []string{"list", "a"},
			"✗ pentym list takes no arguments, got 1 arguments\n→ pentym list --help\n"},
		{"two optional arguments", []string{"check", "a", "b"},
			"✗ pentym check takes at most one argument, [target]; got 2\n→ pentym check --help\n"},
		{"bad colour", []string{"--color=sometimes", "list"},
			"✗ --color must be auto, always or never, not \"sometimes\"\n→ pentym list --help\n"},
		{"bad log format", []string{"--log-format=xml", "list"},
			"✗ --log-format must be text or json, not \"xml\"\n→ pentym list --help\n"},
		{"quiet and verbose", []string{"-q", "-v", "list"},
			"✗ -q and -v cannot be used together\n→ pentym list --help\n"},
		{"group without subcommand", []string{"key"},
			"✗ pentym key needs a subcommand: add, export, list, remove, rotate\n→ pentym key --help\n"},
		{"group with unknown subcommand", []string{"key", "frobnicate"},
			"✗ unknown command \"frobnicate\" for \"pentym key\"\n→ pentym key --help\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := pentym(t, tt.args...)
			if r.code != msg.ExitUsage {
				t.Errorf("exit code = %d, want %d", r.code, msg.ExitUsage)
			}
			if r.stderr != tt.stderr || r.stdout != "" {
				t.Errorf("stdout = %q\nstderr = %q\nwant   %q", r.stdout, r.stderr, tt.stderr)
			}
		})
	}
}

func TestJSONError(t *testing.T) {
	r := pentym(t, "--json", "restore", "prod-pg")
	if r.code != msg.ExitNotImplemented {
		t.Fatalf("exit code = %d", r.code)
	}
	if r.stderr != "" {
		t.Errorf("--json wrote to stderr: %q", r.stderr)
	}
	var doc struct {
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, r.stdout)
	}
	want := map[string]string{"message": "pentym restore is not implemented in this build", "next": "pentym --help"}
	if len(doc.Error) != len(want) || doc.Error["message"] != want["message"] || doc.Error["next"] != want["next"] {
		t.Errorf("error document = %v, want %v", doc.Error, want)
	}

	// Usage errors are JSON too, even when the flag that failed was parsed
	// before --json.
	r = pentym(t, "--json", "backup")
	if r.code != msg.ExitUsage || !strings.Contains(r.stdout, `"message": "pentym backup needs <target>"`) {
		t.Errorf("usage error with --json: code %d\nstdout: %s", r.code, r.stdout)
	}
}

func TestOutputModes(t *testing.T) {
	// ASCII fallback.
	r := pentym(t, "--ascii", "prune")
	if r.stderr != "FAIL pentym prune is not implemented in this build\n-> pentym --help\n" {
		t.Errorf("--ascii stderr = %q", r.stderr)
	}
	// Forced colour on a non-terminal.
	r = pentym(t, "--color=always", "prune")
	if r.stderr != "\x1b[31m✗\x1b[0m pentym prune is not implemented in this build\n→ \x1b[1mpentym --help\x1b[0m\n" {
		t.Errorf("--color=always stderr = %q", r.stderr)
	}
	// Quiet still shows errors.
	r = pentym(t, "-q", "prune")
	if !strings.HasPrefix(r.stderr, "✗ pentym prune") {
		t.Errorf("-q hid the error: %q", r.stderr)
	}
}

func TestInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, m := describe(ctx, context.Canceled)
	if code != msg.ExitInterrupted || m.Fact != "interrupted" {
		t.Errorf("describe(canceled) = %d, %+v", code, m)
	}
}
