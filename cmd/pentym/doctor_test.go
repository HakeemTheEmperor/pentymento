// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func doctorReport(t *testing.T, r result) []doctorCheck {
	t.Helper()
	var doc struct {
		Checks []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("not a doctor report: %v\nstdout: %s\nstderr: %s", err, r.stdout, r.stderr)
	}
	return doc.Checks
}

// checkConsistent: the exit code follows the statuses, and every check
// that is not ok says what to do or why.
func checkConsistent(t *testing.T, r result, checks []doctorCheck) {
	t.Helper()
	failed := false
	for _, c := range checks {
		if statusLevel[c.Status] == 0 && c.Status != "ok" {
			t.Errorf("check %s has status %q", c.Check, c.Status)
		}
		failed = failed || c.Status == "failed"
		if c.Result == "" {
			t.Errorf("check %s has no result", c.Check)
		}
	}
	if (r.code == msg.ExitFailure) != failed || (r.code != msg.ExitOK && r.code != msg.ExitFailure) {
		t.Errorf("exit %d with failed checks: %v", r.code, failed)
	}
}

func TestDoctor(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	work := filepath.Join(t.TempDir(), "work")
	cfg, _ := os.ReadFile(c.config)
	cfg = append(cfg, []byte("work_dir: '"+work+"'\ntargets:\n  prod-pg:\n    engine: postgres\n")...)
	if err := os.WriteFile(c.config, cfg, 0o600); err != nil { // #nosec G703 -- the test's own config
		t.Fatal(err)
	}
	c.mustInit()

	r := c.pentym("--json", "doctor")
	checks := doctorReport(t, r)
	var names []string
	for _, ch := range checks {
		names = append(names, ch.Check)
	}
	if strings.Join(names, ",") != "postgres server,postgres tools,docker,instance providers,repository" {
		t.Errorf("checks = %v", names)
	}
	checkConsistent(t, r, checks)
	repo := checks[len(checks)-1]
	if repo.Status != "ok" || !strings.Contains(repo.Result, "conditional create: yes (tested)") {
		t.Errorf("repository check = %+v", repo)
	}
	if r.stderr != "" {
		t.Errorf("--json wrote to stderr: %s", r.stderr)
	}

	// Text: a table on stdout, and next steps for what is not ok.
	r = c.pentym("doctor", "prod-pg")
	if !strings.HasPrefix(r.stdout, "CHECK") || !strings.Contains(r.stdout, "repository") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	checkConsistent(t, r, checks)

	if r := c.pentym("--json", "doctor", "nosuch"); r.code != msg.ExitNotFound || errorDoc(t, r).Message != "no target nosuch in the config" {
		t.Errorf("doctor nosuch: exit %d, %s", r.code, r.stdout)
	}
}

// TestDoctorWithoutTargets: with no targets both engines are checked, and
// missing tools are only warnings.
func TestDoctorWithoutTargets(t *testing.T) {
	c := newCLIEnv(t, "")
	r := c.pentym("--json", "doctor")
	checks := doctorReport(t, r)
	if len(checks) != 7 || checks[0].Check != "postgres server" || checks[2].Check != "mongodb server" {
		t.Fatalf("checks = %+v", checks)
	}
	for _, ch := range checks[:4] {
		if ch.Status == "failed" {
			t.Errorf("%s failed without any target: %+v", ch.Check, ch)
		}
	}
	if repo := checks[6]; repo.Status != "warning" || repo.Next != "pentym init" {
		t.Errorf("repository without a repository = %+v", repo)
	}
	checkConsistent(t, r, checks)
}
