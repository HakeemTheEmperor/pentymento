// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	cfg = append(cfg, []byte("work_dir: '"+filepath.Join(t.TempDir(), "work")+"'\ninstance_provider: [local]\n"+testTargets)...)
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
