// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/control"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// agentEnv is a repository with one target, for the agent; work_dir is
// the test's own, so leftover cleanup touches nothing else.
func agentEnv(t *testing.T, key string) *cliEnv {
	t.Helper()
	c := newCLIEnv(t, key)
	c.mustInit()
	cfg, _ := os.ReadFile(c.config)
	cfg = append(cfg, []byte("work_dir: '"+filepath.Join(t.TempDir(), "work")+"'\ninstance_provider: [local]\n"+
		"agent:\n  socket: '"+shortSocket(t)+"'\n"+testTargets)...)
	if err := os.WriteFile(c.config, cfg, 0o600); err != nil { // #nosec G703 -- the test's own config
		t.Fatal(err)
	}
	return c
}

func TestAgentCommand(t *testing.T) {
	c := agentEnv(t, "env:KA")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	a := newApp(strings.NewReader(""), &stdout, &stderr, func(k string) string { return c.env[k] })
	done := make(chan msg.ExitCode, 1)
	go func() { done <- a.execute(ctx, []string{"--config", c.config, "agent"}) }()

	r, _, err := c.unlock(c.env["K1"], repo.UnlockOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		hbs, _ := r.LatestHeartbeats(context.Background())
		if len(hbs) == 1 && hbs[0].State == repo.StateRunning {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no running heartbeat\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != msg.ExitOK {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
		}
	case <-time.After(40 * time.Second):
		t.Fatal("the agent did not stop")
	}
	hbs, err := r.LatestHeartbeats(context.Background())
	if err != nil || len(hbs) != 1 || hbs[0].State != repo.StateStopped {
		t.Errorf("heartbeats after stop = %+v, %v", hbs, err)
	}
	// Logs went to stdout as JSON lines (stdout is not a terminal here).
	if !strings.Contains(stdout.String(), `"msg":"agent started"`) || !strings.Contains(stdout.String(), `"msg":"agent stopped"`) {
		t.Errorf("stdout:\n%s", stdout.String())
	}
}

func TestAgentRefusesFullSlot(t *testing.T) {
	c := agentEnv(t, "env:K1")
	r := c.pentym("--json", "agent")
	if doc := errorDoc(t, r); r.code != msg.ExitConfig || doc.ID != "PTM-4007" {
		t.Errorf("exit %d, %+v", r.code, doc)
	}
}

// shortSocket is a fresh control socket address: a Unix socket in a short
// temporary directory (socket paths are limited), or a unique named pipe.
func shortSocket(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	if runtime.GOOS == "windows" {
		return `\\.\pipe\pentym-test-` + hex.EncodeToString(b)
	}
	dir, err := os.MkdirTemp("", "pa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "agent.sock")
}

// runningAgent runs pentym agent, with the fake engines, until the test
// ends, and waits until its socket answers.
func runningAgent(t *testing.T, c *cliEnv) *bytes.Buffer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	a := newApp(strings.NewReader(""), &stdout, &stderr, func(k string) string { return c.env[k] })
	a.engineFor = fakeEngines
	done := make(chan msg.ExitCode, 1)
	go func() { done <- a.execute(ctx, []string{"--config", c.config, "agent"}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(40 * time.Second):
			t.Error("the agent did not stop")
		}
	})
	cfg, err := config.Load(c.config, nil)
	if err != nil {
		t.Fatal(err)
	}
	sock, _ := cfg.AgentSocket()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if st, err := control.NewClient(sock).Status(context.Background()); err == nil && st.Ready {
			return &stdout
		}
		select {
		case code := <-done:
			t.Fatalf("the agent exited with %d\nstderr: %s", code, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent's socket did not answer\nstderr: %s", stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBackupNow: backup --now runs the backup in the running agent, and
// prints what a local backup prints.
func TestBackupNow(t *testing.T) {
	c := agentEnv(t, "env:KA")
	runningAgent(t, c)
	r := c.runFake("--json", "backup", "prod-pg", "--now")
	var rep backupReport
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &rep) != nil {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if rep.Snapshot.Target != "prod-pg" || rep.Snapshot.Size != 2200002 || rep.Stats.RunID == "" || len(rep.Warnings) != 1 {
		t.Errorf("report = %+v", rep)
	}
	r = c.runFake("backup", "prod-pg", "--now")
	if r.code != msg.ExitOK || !strings.Contains(r.stderr, "! prod-pg: pentym_backup is a superuser") || !strings.Contains(r.stderr, "✓ prod-pg: snapshot ") {
		t.Errorf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	if l := c.runFake("--json", "list"); !strings.Contains(l.stdout, rep.Snapshot.ID) {
		t.Errorf("the snapshot is not listed: %s", l.stdout)
	}
	// A target the agent does not know: its error, with its exit code.
	cfg, _ := os.ReadFile(c.config)
	if err := os.WriteFile(c.config, append(cfg, []byte("  prod-new:\n    engine: postgres\n")...), 0o600); err != nil { // #nosec G703 -- test config
		t.Fatal(err)
	}
	r = c.runFake("--json", "backup", "prod-new", "--now")
	if doc := errorDoc(t, r); r.code != msg.ExitNotFound || doc.Message != "the running agent has no target prod-new" {
		t.Errorf("unknown target: exit %d, %+v", r.code, doc)
	}
	// After a reload it knows it.
	r = c.pentym("--json", "agent", "reload")
	var res struct {
		Added []string `json:"Added"`
	}
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &res) != nil || strings.Join(res.Added, ",") != "prod-new" {
		t.Errorf("reload: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if r := c.pentym("agent", "reload"); r.code != msg.ExitOK || !strings.Contains(r.stderr, "agent reloaded: 0 added, 0 removed, 0 restarted, 2 unchanged") {
		t.Errorf("second reload: exit %d\n%s", r.code, r.stderr)
	}
}

// TestAgentNotReachable: without a running agent, --now and reload say so
// and show the last heartbeat from the repository.
func TestAgentNotReachable(t *testing.T) {
	c := agentEnv(t, "env:KA")
	r := c.runFake("--json", "backup", "prod-pg", "--now")
	if doc := errorDoc(t, r); r.code != msg.ExitConnect || !strings.HasPrefix(doc.Message, "agent not reachable at ") ||
		strings.Contains(doc.Message, "heartbeat") || !strings.Contains(doc.Next, "pentym backup prod-pg") {
		t.Errorf("no agent, no heartbeat: exit %d, %+v", r.code, doc)
	}
	full, _, err := c.unlock(c.env["K1"], repo.UnlockOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := full.PutHeartbeat(context.Background(), repo.Heartbeat{Host: "db-1", AgentID: "x", State: repo.StateStopped, Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r = c.runFake("--json", "agent", "reload")
	if doc := errorDoc(t, r); r.code != msg.ExitConnect || !strings.Contains(doc.Message, "; last heartbeat from db-1 at ") ||
		!strings.HasSuffix(doc.Message, "(stopped)") || doc.Next != "start the agent: pentym agent" {
		t.Errorf("no agent, a heartbeat: exit %d, %+v", r.code, doc)
	}
}

// TestSecondAgentRefused: a second agent on the same socket is refused
// while the first runs.
func TestSecondAgentRefused(t *testing.T) {
	c := agentEnv(t, "env:KA")
	runningAgent(t, c)
	r := c.pentym("--json", "agent")
	if doc := errorDoc(t, r); r.code != msg.ExitFailure || !strings.Contains(doc.Message, "another agent is already running") {
		t.Errorf("exit %d, %+v", r.code, doc)
	}
}
