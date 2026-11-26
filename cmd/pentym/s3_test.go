// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/backend/s3/s3test"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

const testTargets = "targets:\n  prod-pg:\n    engine: postgres\n    credentials:\n      backup:\n        user: pentym_backup\n"

// s3CLIEnv is a CLI environment whose repository is in a bucket at
// endpoint, with static keys from the environment.
func s3CLIEnv(t *testing.T, endpoint string) *cliEnv {
	t.Helper()
	c := newCLIEnv(t, "env:K1")
	c.env["S3_KEY_ID"], c.env["S3_SECRET"] = "pentym", "s3-secret-key"
	cfg := "repository:\n  s3:\n    bucket: pentym\n    prefix: prod\n    region: us-east-1\n" +
		"    endpoint: '" + endpoint + "'\n    path_style: true\n" +
		"    credentials:\n      access_key_id: env:S3_KEY_ID\n      secret_access_key: env:S3_SECRET\n" +
		"  key: env:K1\n" + testTargets
	if err := os.WriteFile(c.config, []byte(cfg), 0o600); err != nil { // #nosec G703 -- the test's own config
		t.Fatal(err)
	}
	return c
}

// localCLIEnv is the same environment on local storage.
func localCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	c := newCLIEnv(t, "env:K1")
	cfg, _ := os.ReadFile(c.config)
	if err := os.WriteFile(c.config, append(cfg, testTargets...), 0o600); err != nil { // #nosec G703 -- the test's own config
		t.Fatal(err)
	}
	return c
}

var (
	idRE       = regexp.MustCompile(`[0-9a-f]{16,}`)
	timeRE     = regexp.MustCompile(`"\d{4}-\d\d-\d\dT[^"]*"`)
	durationRE = regexp.MustCompile(`"duration_seconds": ?[0-9.e-]+`)
	runIDRE    = regexp.MustCompile(`"run_id": ?"[^"]*"`)
	locationRE = regexp.MustCompile(`"location": ?"[^"]*"`)
)

// normalize masks what differs between two runs: IDs, times, durations
// and the location.
func normalize(s string) string {
	for _, r := range []struct {
		re   *regexp.Regexp
		with string
	}{{idRE, "<id>"}, {timeRE, `"<time>"`}, {durationRE, `"duration_seconds":0`}, {runIDRE, `"run_id":""`}, {locationRE, `"location":""`}} {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return s
}

// session runs the same commands on a repository and returns their
// normalized results.
func session(t *testing.T, c *cliEnv) []string {
	t.Helper()
	c.mustInit()
	var out []string
	for _, args := range [][]string{
		{"--json", "backup", "prod-pg"},
		{"--json", "backup", "prod-pg"},
		{"--json", "list"},
		{"--json", "check"},
		{"--json", "check", "--full"},
		{"--json", "key", "list"},
	} {
		r := c.runFake(args...)
		if r.code != msg.ExitOK {
			t.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
		}
		out = append(out, strings.Join(args, " ")+"\n"+normalize(r.stdout))
	}
	return out
}

// TestS3Parity: the same commands on local storage and in a bucket give
// the same results (Build Order C15).
func TestS3Parity(t *testing.T) {
	_, url := s3test.New(t)
	local := session(t, localCLIEnv(t))
	remote := session(t, s3CLIEnv(t, url))
	for i := range local {
		if local[i] != remote[i] {
			t.Errorf("local and S3 differ:\n--- local\n%s\n--- s3\n%s", local[i], remote[i])
		}
	}
}

func doctorRepositoryCheck(t *testing.T, c *cliEnv) doctorCheck {
	t.Helper()
	r := c.pentym("--json", "doctor")
	var doc struct {
		Checks []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, r.stdout)
	}
	for _, ch := range doc.Checks {
		if ch.Check == "repository" {
			return ch
		}
	}
	t.Fatalf("no repository check in %s", r.stdout)
	return doctorCheck{}
}

func TestS3InitAndDoctor(t *testing.T) {
	srv, url := s3test.New(t)
	c := s3CLIEnv(t, url)
	if res := c.mustInit(); res.Location != "s3://pentym/prod" || !res.ConditionalCreate {
		t.Errorf("init = %+v", res)
	}
	if srv.Count("PUT") == 0 {
		t.Error("init wrote nothing to the bucket")
	}
	if ch := doctorRepositoryCheck(t, c); ch.Status != "ok" ||
		!strings.HasPrefix(ch.Result, "S3 bucket s3://pentym/prod at "+url+"; conditional create: yes (tested)") {
		t.Errorf("doctor = %+v", ch)
	}
	// --insecure-test-mode is refused on remote storage.
	c2 := s3CLIEnv(t, url)
	if r := c2.pentym("--json", "init", "--insecure-test-mode"); r.code != msg.ExitConfig || errorDoc(t, r).ID != "PTM-4003" {
		t.Errorf("insecure on S3: exit %d %s", r.code, r.stdout)
	}
}

// TestS3WithoutConditionalCreate: on storage that ignores or refuses
// If-None-Match, init records it and warns, later commands use the
// fallback, and doctor reports it.
func TestS3WithoutConditionalCreate(t *testing.T) {
	for _, mode := range []string{"ignore", "501"} {
		t.Run(mode, func(t *testing.T) {
			srv, url := s3test.New(t)
			srv.CCMode = mode
			c := s3CLIEnv(t, url)
			r := c.pentym("init", "--key", "env:K1", "--key", "env:K2", "--agent-key", "env:KA")
			if r.code != msg.ExitOK || !strings.Contains(r.stderr, "the storage does not honour conditional create") {
				t.Fatalf("init: exit %d\n%s", r.code, r.stderr)
			}
			if r := c.runFake("backup", "prod-pg"); r.code != msg.ExitOK {
				t.Errorf("backup: exit %d\n%s", r.code, r.stderr)
			}
			if r := c.runFake("--json", "check"); r.code != msg.ExitOK {
				t.Errorf("check: exit %d\n%s", r.code, r.stdout)
			}
			if ch := doctorRepositoryCheck(t, c); ch.Status != "warning" || !strings.HasSuffix(ch.Result, "conditional create not honoured") {
				t.Errorf("doctor = %+v", ch)
			}
		})
	}
}
