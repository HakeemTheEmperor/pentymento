// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/local"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// addSnapshot writes a 1 MiB snapshot of random data for a target, as a
// backup will, and returns the key of the pack holding it.
func (c *cliEnv) addSnapshot(target string) string {
	c.t.Helper()
	ctx := context.Background()
	r, _, err := c.unlock(c.env["K1"], repo.UnlockOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	tid, err := r.PutTargetInfo(ctx, repo.TargetInfo{Name: target, Engine: "postgres"})
	if err != nil {
		c.t.Fatal(err)
	}
	idx, err := r.LoadIndex(ctx)
	if err != nil {
		c.t.Fatal(err)
	}
	data := make([]byte, 1<<20)
	_, _ = rand.Read(data)
	before := c.objects("data/")
	if _, _, err := r.WriteSnapshot(ctx, idx, bytes.NewReader(data), repo.Snapshot{Target: tid, Engine: "postgres"}, repo.WriteOptions{}); err != nil {
		c.t.Fatal(err)
	}
	for _, k := range c.objects("data/") {
		if !contains(before, k) {
			return k
		}
	}
	c.t.Fatal("no pack written")
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// objects lists object keys under prefix.
func (c *cliEnv) objects(prefix string) []string {
	c.t.Helper()
	var out []string
	be, err := backendAt(c.repoDir)
	if err != nil {
		c.t.Fatal(err)
	}
	_ = be.List(context.Background(), prefix, func(oi backend.ObjectInfo) error {
		out = append(out, oi.Key)
		return nil
	})
	return out
}

func backendAt(dir string) (backend.Backend, error) { return local.New(dir) }

func (c *cliEnv) objectPath(key string) string {
	return filepath.Join(c.repoDir, filepath.FromSlash(key))
}

func checkReport(t *testing.T, r result) repo.CheckReport {
	t.Helper()
	var rep repo.CheckReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("not a check report: %v\nstdout: %s\nstderr: %s", err, r.stdout, r.stderr)
	}
	return rep
}

func TestCheckCommand(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	c.mustInit()
	c.addSnapshot("prod-pg")
	c.addSnapshot("prod-mg")

	r := c.pentym("check")
	if r.code != msg.ExitOK || r.stderr != "" {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	want := []string{
		`^CHECK +RESULT$`,
		`^slots +✓ 3 key slots, 2 full$`,
		`^index +✓ 2 index files, 2 packs, \d+ chunks$`, // one per snapshot
		`^packs +✓ 2 packs checked$`,
		`^snapshots +✓ 2 snapshots, \d+ chunk references$`,
		`^data +✓ 1 packs read \(sampled\), \d+ chunks verified$`,
		`^segments +✓ 0 of 0 log segments read$`,
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("stdout:\n%s", r.stdout)
	}
	for i, w := range want {
		if !regexp.MustCompile(w).MatchString(lines[i]) {
			t.Errorf("line %d = %q, want %s", i, lines[i], w)
		}
	}

	rep := checkReport(t, c.pentym("--json", "check", "--full"))
	if len(rep.Problems) != 0 || rep.PacksRead != 2 || rep.Snapshots != 2 {
		t.Errorf("--full --json: %+v", rep)
	}
	rep = checkReport(t, c.pentym("--json", "check", "prod-pg"))
	if rep.Snapshots != 1 || rep.PacksRead != 1 {
		t.Errorf("check prod-pg: %+v", rep)
	}
	j := c.journal()
	if last := j[len(j)-1]; last.Operation != "check" || last.Result != repo.ResultOK || last.Target == "" {
		t.Errorf("journal: %+v", last)
	}
}

func TestCheckCommandRefusals(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	c.mustInit()
	for _, tt := range []struct {
		args []string
		code msg.ExitCode
		text string
	}{
		{[]string{"check", "nosuch"}, msg.ExitNotFound, "nosuch: the repository holds nothing for this target"},
		{[]string{"check", "--sample", "0"}, msg.ExitUsage, "--sample must be above 0 and at most 100, not 0"},
		{[]string{"check", "--sample", "101"}, msg.ExitUsage, "--sample must be above 0 and at most 100, not 101"},
		{[]string{"check", "--full", "--sample", "10"}, msg.ExitUsage, "--full and --sample cannot be used together"},
	} {
		r := c.pentym(tt.args...)
		if r.code != tt.code || !strings.Contains(r.stderr, tt.text) {
			t.Errorf("%v: exit %d, %s", tt.args, r.code, r.stderr)
		}
	}
	c.setKey("env:KA")
	if r := c.pentym("check"); r.code != msg.ExitAccess {
		t.Errorf("check with the agent passphrase: exit %d", r.code)
	}
}

func TestCheckCommandFindsProblems(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	c.mustInit()
	pack := c.addSnapshot("prod-pg")
	if err := os.Remove(c.objectPath(pack)); err != nil {
		t.Fatal(err)
	}

	r := c.pentym("-v", "check")
	if r.code != msg.ExitIntegrity {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if !regexp.MustCompile(`(?m)^packs +✗ 1 packs checked; 1 problem$`).MatchString(r.stdout) {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	for _, s := range []string{
		"✗ " + pack + ": pack is missing; ",
		"(PTM-7001)",
		"✗ check found 2 problems in the repository\n",
	} {
		if !strings.Contains(r.stderr, s) {
			t.Errorf("stderr has no %q:\n%s", s, r.stderr)
		}
	}

	// With --json: one document on stdout, exit 7, nothing on stderr.
	r = c.pentym("--json", "check")
	rep := checkReport(t, r)
	if r.code != msg.ExitIntegrity || r.stderr != "" || rep.Errors() != 2 {
		t.Errorf("--json: exit %d, %d errors\nstderr: %s", r.code, rep.Errors(), r.stderr)
	}
	j := c.journal()
	if last := j[len(j)-1]; last.Operation != "check" || last.Result != repo.ResultFailed {
		t.Errorf("journal: %+v", last)
	}
}

// TestCheckRespectsExclusiveLock: check backs off while prune holds the exclusive
// lock.
func TestCheckRespectsExclusiveLock(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	c.mustInit()
	be, err := backendAt(c.repoDir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := lock.NewLocks(be, lock.Holder{User: "prune", Host: "admin-1", PID: 1}).AcquireExclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := c.pentym("--json", "check")
	if doc := errorDoc(t, r); doc.ID != lock.IDLocked {
		t.Errorf("check under an exclusive lock: exit %d, %+v", r.code, doc)
	}
	_ = l.Release(context.Background())
	if r := c.pentym("check"); r.code != msg.ExitOK {
		t.Errorf("check after the lock was released: exit %d, %s", r.code, r.stderr)
	}
}
