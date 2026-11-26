// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
)

// cliFakeEngine streams a small base.tar and a manifest.
type cliFakeEngine struct{ engine.Engine }

func (cliFakeEngine) Name() string { return "postgres" }

// TestNewEngine builds each configured target's engine through the
// registry: no command names an engine.
func TestNewEngine(t *testing.T) {
	c := backupEnv(t, "env:K1")
	a := newApp(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, func(k string) string { return c.env[k] })
	a.tel = telemetry.New(telemetry.Discard(), nil)
	cfg, err := config.Load(c.config, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"prod-pg": "postgres", "prod-mg": "mongodb"} {
		e, err := a.newEngine(cfg, cfg.Targets[name])
		if err != nil || e.Name() != want {
			t.Errorf("%s: %v, %v", name, e, err)
		}
	}
}

func (cliFakeEngine) Check(context.Context, engine.Target) (*engine.CheckResult, error) {
	return &engine.CheckResult{ServerVersion: "17.2", Findings: []engine.Finding{
		{Level: msg.LevelWarning, ID: "PTM-2004", Fact: "pentym_backup is a superuser, which is more privilege than backups need", Next: "use a role with only REPLICATION"},
	}}, nil
}

func (cliFakeEngine) Inventory(context.Context, engine.Target) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func (cliFakeEngine) BaseBackup(ctx context.Context, _ engine.Target, sink engine.ArchiveSink) (*engine.BaseBackup, error) {
	// In order, as a server sends them: base.tar first.
	for _, a := range []struct {
		name string
		data []byte
	}{{"base.tar", bytes.Repeat([]byte("pentymento "), 200000)}, {"backup_manifest", []byte("{}")}} {
		name, data := a.name, a.data
		w, err := sink.Archive(ctx, name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	return &engine.BaseBackup{EngineVersion: "17.2", Method: "native", Start: "0/2000028", End: "0/2000100", Timeline: "1"}, nil
}

// cliMongoFake is a MongoDB standalone server: one archive, no log
// positions, and the snapshots-only warning.
type cliMongoFake struct{ engine.Engine }

func (cliMongoFake) Name() string { return "mongodb" }

func (cliMongoFake) Check(context.Context, engine.Target) (*engine.CheckResult, error) {
	return &engine.CheckResult{ServerVersion: "8.0.4", Findings: []engine.Finding{{Level: msg.LevelWarning, ID: "PTM-2011",
		Fact: "this is a standalone server with no oplog, so pentym takes snapshots only: no point-in-time recovery",
		Next: "run it as a single-node replica set to get point-in-time recovery"}}}, nil
}

func (cliMongoFake) Inventory(context.Context, engine.Target) (json.RawMessage, error) {
	return json.RawMessage(`{"server_version":"8.0.4","databases":[]}`), nil
}

func (cliMongoFake) BaseBackup(ctx context.Context, _ engine.Target, sink engine.ArchiveSink) (*engine.BaseBackup, error) {
	w, err := sink.Archive(ctx, "mongodump.archive")
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(bytes.Repeat([]byte("mongodump "), 100000)); err != nil {
		return nil, err
	}
	return &engine.BaseBackup{EngineVersion: "8.0.4", Method: "mongodump"}, nil
}

// TestBackupCommandMongoDB backs up a MongoDB target through the same
// command and lists it beside a Postgres snapshot.
func TestBackupCommandMongoDB(t *testing.T) {
	c := backupEnv(t, "env:KA")
	r := c.runFake("--json", "backup", "prod-mg")
	var doc struct {
		Snapshot snapshotInfo `json:"snapshot"`
	}
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &doc) != nil {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	s := doc.Snapshot
	if s.Target != "prod-mg" || s.Engine != "mongodb" || s.Method != "mongodump" || strings.Join(s.Parts, ",") != "mongodump.archive" ||
		s.StartPosition != "" || s.Timeline != "" || s.Size != 1000000 {
		t.Errorf("--json = %+v", s)
	}
	r = c.runFake("backup", "prod-mg")
	if r.code != msg.ExitOK || !strings.Contains(r.stderr, "! prod-mg: this is a standalone server with no oplog") {
		t.Errorf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	c.runFake("backup", "prod-pg")
	lines := strings.Split(strings.TrimSuffix(c.runFake("list").stdout, "\n"), "\n")
	if len(lines) != 4 || !strings.Contains(lines[1], "  prod-mg  mongodb 8.0.4  ") || !strings.Contains(lines[3], "  prod-pg  postgres 17.2  ") {
		t.Errorf("list:\n%s", strings.Join(lines, "\n"))
	}
}

// backupEnv is a repository with a Postgres target and a MongoDB one,
// backed up with the agent slot.
func backupEnv(t *testing.T, key string) *cliEnv {
	t.Helper()
	c := newCLIEnv(t, key)
	c.mustInit()
	cfg, _ := os.ReadFile(c.config)
	cfg = append(cfg, []byte("targets:\n  prod-pg:\n    engine: postgres\n    credentials:\n      backup:\n        user: pentym_backup\n  prod-mg:\n    engine: mongodb\n")...)
	if err := os.WriteFile(c.config, cfg, 0o600); err != nil { // #nosec G703 -- the test's own config
		t.Fatal(err)
	}
	return c
}

// runFake runs pentym with the fake engine.
func (c *cliEnv) runFake(args ...string) result {
	c.t.Helper()
	var stdout, stderr bytes.Buffer
	a := newApp(strings.NewReader(""), &stdout, &stderr, func(k string) string { return c.env[k] })
	a.engineFor = func(_ *config.Config, t config.Target) (engine.Engine, error) {
		if t.Engine == "mongodb" {
			return cliMongoFake{}, nil
		}
		return cliFakeEngine{}, nil
	}
	code := a.execute(context.Background(), append([]string{"--config", c.config}, args...))
	return result{code, stdout.String(), stderr.String()}
}

func TestBackupCommand(t *testing.T) {
	c := backupEnv(t, "env:KA") // the agent slot is enough
	r := c.runFake("backup", "prod-pg")
	if r.code != msg.ExitOK || !regexp.MustCompile(`^[0-9a-f]{32}\n$`).MatchString(r.stdout) {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	id := strings.TrimSpace(r.stdout)
	if !strings.Contains(r.stderr, "! prod-pg: pentym_backup is a superuser") ||
		!regexp.MustCompile(`✓ prod-pg: snapshot `+id+`: 2\.1 MiB in .+, .+ new after deduplication\n`).MatchString(r.stderr) {
		t.Errorf("stderr:\n%s", r.stderr)
	}

	r = c.runFake("--json", "backup", "prod-pg")
	var doc struct {
		Snapshot snapshotInfo `json:"snapshot"`
		Stats    backupStats  `json:"stats"`
	}
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &doc) != nil {
		t.Fatalf("--json: exit %d\n%s", r.code, r.stdout)
	}
	if doc.Snapshot.Target != "prod-pg" || doc.Snapshot.EngineVersion != "17.2" || strings.Join(doc.Snapshot.Parts, ",") != "base.tar,backup_manifest" ||
		doc.Snapshot.StartPosition != "0/2000028" || doc.Stats.Bytes != 2200002 || doc.Stats.RunID == "" {
		t.Errorf("--json = %+v", doc)
	}

	j := c.journal()
	if last := j[len(j)-1]; last.Operation != "backup" || last.Result != repo.ResultOK || last.Target == "" {
		t.Errorf("journal = %+v", last)
	}
}

func TestBackupCommandRefusals(t *testing.T) {
	c := backupEnv(t, "env:K1")
	for _, tt := range []struct {
		args []string
		code msg.ExitCode
		fact string
	}{
		{[]string{"backup", "nosuch"}, msg.ExitNotFound, "no target nosuch in the config"},
		{[]string{"backup", "prod-pg", "--now"}, msg.ExitNotImplemented, "--now needs the agent, which is not implemented in this build"},
	} {
		r := c.runFake(append([]string{"--json"}, tt.args...)...)
		if doc := errorDoc(t, r); r.code != tt.code || doc.Message != tt.fact {
			t.Errorf("%v: exit %d, %+v", tt.args, r.code, doc)
		}
	}
}

func TestListCommand(t *testing.T) {
	c := backupEnv(t, "env:K1")
	if r := c.runFake("list"); r.code != msg.ExitOK || r.stdout != "" || !strings.Contains(r.stderr, "the repository has no snapshots yet") {
		t.Errorf("empty list: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	first := strings.TrimSpace(c.runFake("backup", "prod-pg").stdout)
	second := strings.TrimSpace(c.runFake("backup", "prod-pg").stdout)
	admin, _, err := c.unlock(pass1, repo.UnlockOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.SetPin(context.Background(), repo.Identity{User: "ada", Host: "h"}, first, true, "pre-migration"); err != nil {
		t.Fatal(err)
	}

	res := c.runFake("list")
	lines := strings.Split(strings.TrimSuffix(res.stdout, "\n"), "\n")
	if res.code != msg.ExitOK || len(lines) != 3 ||
		!regexp.MustCompile(`^SNAPSHOT +TARGET +ENGINE +TAKEN +DURATION +SIZE +PINNED$`).MatchString(lines[0]) ||
		!strings.HasPrefix(lines[1], first+"  prod-pg  postgres 17.2  ") || !strings.HasSuffix(lines[1], "2.1 MiB  pinned (pre-migration)") ||
		!strings.HasPrefix(lines[2], second+"  prod-pg") {
		t.Errorf("exit %d\nstdout:\n%s", res.code, res.stdout)
	}

	res = c.runFake("--json", "list")
	var doc struct {
		Snapshots []snapshotInfo `json:"snapshots"`
	}
	if json.Unmarshal([]byte(res.stdout), &doc) != nil || len(doc.Snapshots) != 2 || !doc.Snapshots[0].Pinned ||
		doc.Snapshots[0].PinReason != "pre-migration" || doc.Snapshots[1].Pinned || doc.Snapshots[0].Size != 2200002 {
		t.Errorf("--json = %s", res.stdout)
	}
}
