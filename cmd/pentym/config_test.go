// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func TestConfigPath(t *testing.T) {
	env := map[string]string{"PENTYM_CONFIG": "/from-env.yaml"}
	a := newApp(nil, &bytes.Buffer{}, &bytes.Buffer{}, func(k string) string { return env[k] })
	if got := a.configPath(); got != "/from-env.yaml" {
		t.Errorf("configPath with PENTYM_CONFIG = %q", got)
	}
	a.opts.config = "/from-flag.yaml"
	if got := a.configPath(); got != "/from-flag.yaml" {
		t.Errorf("configPath with --config = %q", got)
	}
}

func TestConfigFlagInHelp(t *testing.T) {
	r := pentym(t, "--help")
	if !strings.Contains(r.stdout, "--config string") || !strings.Contains(r.stdout, "$PENTYM_CONFIG") {
		t.Errorf("--help does not describe --config:\n%s", r.stdout)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	a := newApp(nil, &bytes.Buffer{}, &bytes.Buffer{}, func(string) string { return "" })
	a.opts.config = filepath.Join(dir, "missing.yaml")
	_, err := a.loadConfig(nil)
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitConfig || me.Message.ID != "PTM-1008" {
		t.Fatalf("loadConfig(missing) = %v", err)
	}
}

// TestSecretLeakCLI runs every command with a config and environment full
// of canary secrets, in every output mode, and scans all output. It also
// renders a config error caused by a plaintext canary.
func TestSecretLeakCLI(t *testing.T) {
	canaries := leaktest.New(t, 3)
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "pw")
	if err := os.WriteFile(secretFile, []byte(canaries[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(good, []byte("targets:\n  prod-pg:\n    engine: postgres\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("targets:\n  prod-pg:\n    engine: "+string(canaries[2])+"\n  x: "+string(canaries[2])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"LANG":       "en_US.UTF-8",
		"PGPASSWORD": string(canaries[0]),
		"DB_SECRET":  "file:" + secretFile,
	}
	getenv := func(k string) string { return env[k] }

	var outputs []string
	runCLI := func(args ...string) {
		var stdout, stderr bytes.Buffer
		run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, getenv)
		outputs = append(outputs, stdout.String(), stderr.String())
	}

	root := newApp(nil, &bytes.Buffer{}, &bytes.Buffer{}, nil).rootCommand()
	walk(root, func(c *cobra.Command) {
		path := strings.Fields(c.CommandPath())[1:]
		args := append(path, stubArgs(c)...)
		for _, mode := range [][]string{{"-v", "--debug"}, {"--json"}, {"--color=always", "--ascii"}} {
			runCLI(append(append([]string{"--config", good}, mode...), args...)...)
			runCLI(append(append([]string{"--config", bad}, mode...), args...)...)
		}
	})

	// A config error rendered in every mode.
	a := newApp(nil, &bytes.Buffer{}, &bytes.Buffer{}, getenv)
	a.opts.config = bad
	_, err := a.loadConfig([]string{"PENTYM_TARGETS__PROD_PG__ENGINE=" + string(canaries[0])})
	var me *msg.Error
	if !errors.As(err, &me) {
		t.Fatalf("loadConfig(bad) = %v", err)
	}
	for _, json := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		p := &cliout.Printer{Stdout: &stdout, Stderr: &stderr, JSON: json, Unicode: true, Verbosity: cliout.Verbose}
		p.Fail(me.Message)
		outputs = append(outputs, stdout.String(), stderr.String(), err.Error())
	}

	leaktest.Scan(t, "CLI output", canaries, outputs...)
}
