// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// cliLogFake is a Postgres server whose WAL catch-up can drain: one
// segment up to head, or a gap after the checkpoint.
type cliLogFake struct {
	cliFakeEngine
	head engine.Position
	gap  bool
}

func (f cliLogFake) LogHealth(context.Context, engine.Target) (*engine.LogHealth, error) {
	return &engine.LogHealth{Head: f.head}, nil
}

func (f cliLogFake) DetectGap(_ context.Context, _ engine.Target, cp engine.Position) (*engine.Gap, error) {
	if f.gap && cp != "" {
		return &engine.Gap{From: cp, Reason: "the replication slot pentym_prod_pg was invalidated (max_slot_wal_keep_size)"}, nil
	}
	return nil, nil
}

func (f cliLogFake) Capture(ctx context.Context, _ engine.Target, _ engine.Position, sink engine.LogSink) error {
	if err := sink.Segment(ctx, engine.Segment{
		Kind: "wal", Name: "000000010000000000000003", Timeline: "1", First: "0/3000000", Last: "0/4000000",
		Checkpoint: f.head, Data: []byte("wal 000000010000000000000003"),
	}); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// runCatchUp runs pentym with a draining fake for prod-pg.
func (c *cliEnv) runCatchUp(f cliLogFake, args ...string) result {
	c.t.Helper()
	var stdout, stderr bytes.Buffer
	a := newApp(strings.NewReader(""), &stdout, &stderr, func(k string) string { return c.env[k] })
	a.engineFor = func(cfg *config.Config, t config.Target) (engine.Engine, error) {
		if t.Engine == "postgres" {
			return f, nil
		}
		return fakeEngines(cfg, t)
	}
	code := a.execute(context.Background(), append([]string{"--config", c.config}, args...))
	return result{code, stdout.String(), stderr.String()}
}

func TestCatchUpCommand(t *testing.T) {
	c := backupEnv(t, "env:KA") // the agent slot is enough
	f := cliLogFake{head: "1/0/4000000"}
	r := c.runCatchUp(f, "catch-up", "prod-pg")
	if r.code != msg.ExitOK || !regexp.MustCompile(`✓ prod-pg: caught up to 1/0/4000000: stored 1 segment, .+ in .+\n`).MatchString(r.stderr) {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	r = c.runCatchUp(f, "catch-up", "prod-pg")
	if r.code != msg.ExitOK || !strings.Contains(r.stderr, "✓ prod-pg: already caught up at 1/0/4000000; nothing to store") {
		t.Fatalf("second run: exit %d\nstderr: %s", r.code, r.stderr)
	}

	// A gap: reported, with the next step, and exit code 8.
	gap := cliLogFake{head: "1/0/6000000", gap: true}
	r = c.runCatchUp(gap, "catch-up", "prod-pg")
	if r.code != msg.ExitUnrecoverable || !strings.Contains(r.stderr, "✗ prod-pg: a gap in captured log after 1/0/4000000: the replication slot pentym_prod_pg was invalidated") ||
		!strings.Contains(r.stderr, "→ pentym backup prod-pg") {
		t.Fatalf("gap: exit %d\nstderr: %s", r.code, r.stderr)
	}
	r = c.runCatchUp(cliLogFake{head: "1/0/8000000", gap: true}, "--json", "catch-up", "prod-pg")
	var rep catchUpReport
	if r.code != msg.ExitUnrecoverable || json.Unmarshal([]byte(r.stdout), &rep) != nil || rep.Gap == nil ||
		rep.Gap.From != "1/0/6000000" || rep.To != "1/0/8000000" || rep.Segments != 1 || rep.Generation != 4 {
		t.Fatalf("--json gap: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
}

func TestCatchUpCommandNoLog(t *testing.T) {
	c := backupEnv(t, "env:KA")
	r := c.runCatchUp(cliLogFake{}, "catch-up", "prod-mg")
	if r.code != msg.ExitNotImplemented || !strings.Contains(r.stderr, "✗ prod-mg: there is no log to catch up") ||
		!strings.Contains(r.stderr, "→ pentym backup prod-mg") {
		t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	r = c.runCatchUp(cliLogFake{}, "catch-up", "nope")
	if r.code != msg.ExitNotFound {
		t.Errorf("an unknown target: exit %d\nstderr: %s", r.code, r.stderr)
	}
}
