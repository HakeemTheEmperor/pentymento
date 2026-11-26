// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// untar reads a tar's entries.
func untar(t *testing.T, b []byte) (names []string, data map[string][]byte) {
	t.Helper()
	data = map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, data
		}
		if err != nil {
			t.Fatalf("not a tar: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		data[h.Name] = content
	}
}

func TestDumpPostgres(t *testing.T) {
	c := backupEnv(t, "env:K1")
	id := strings.TrimSpace(c.runFake("backup", "prod-pg").stdout)
	out := filepath.Join(t.TempDir(), "prod-pg.tar")

	r := c.runFake("--json", "dump", id, "--output", out)
	var res dumpResult
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &res) != nil {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	b, err := os.ReadFile(out) // #nosec G304 -- test output
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != dumpTar || res.Bytes != int64(len(b)) || strings.Join(res.Streams, ",") != "base.tar,backup_manifest" {
		t.Errorf("result = %+v", res)
	}
	names, data := untar(t, b)
	if strings.Join(names, ",") != "base.tar,backup_manifest" ||
		!bytes.Equal(data["base.tar"], bytes.Repeat([]byte("pentymento "), 200000)) || string(data["backup_manifest"]) != "{}" {
		t.Errorf("tar holds %v", names)
	}
	if fi, err := os.Stat(out); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Errorf("output mode: %v, %v", fi, err)
	}

	// The same dump on stdout (not a terminal here).
	r = c.runFake("dump", id)
	if r.code != msg.ExitOK || !bytes.Equal([]byte(r.stdout), b) || !strings.Contains(r.stderr, "dumped snapshot "+id+" (a tar of its archives") {
		t.Errorf("stdout dump: exit %d, %d bytes\n%s", r.code, len(r.stdout), r.stderr)
	}
	if j := c.journal(); j[len(j)-1].Operation != "dump" {
		t.Errorf("journal = %+v", j[len(j)-1])
	}
}

func TestDumpMongoDB(t *testing.T) {
	c := backupEnv(t, "env:K1")
	id := strings.TrimSpace(c.runFake("backup", "prod-mg").stdout)
	r := c.runFake("dump", id)
	if r.code != msg.ExitOK || !bytes.Equal([]byte(r.stdout), bytes.Repeat([]byte("mongodump "), 100000)) {
		t.Errorf("exit %d, %d bytes\n%s", r.code, len(r.stdout), r.stderr)
	}
}

func TestDumpRefusals(t *testing.T) {
	c := backupEnv(t, "env:K1")
	id := strings.TrimSpace(c.runFake("backup", "prod-pg").stdout)
	existing := filepath.Join(t.TempDir(), "keep.tar")
	if err := os.WriteFile(existing, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		args []string
		code msg.ExitCode
		fact string
	}{
		{[]string{"dump", "nosuch"}, msg.ExitUsage, "a snapshot ID is 32 hexadecimal characters, as pentym list shows"},
		{[]string{"dump", strings.Repeat("ab", 16), "--output", filepath.Join(t.TempDir(), "x")}, msg.ExitNotFound, "no snapshot " + strings.Repeat("ab", 16) + " in the repository"},
		{[]string{"dump", id}, msg.ExitUsage, "--json needs --output: without it the dump itself goes to stdout"},
		{[]string{"dump", id, "--output", existing}, msg.ExitUsage, existing + " already exists"},
	} {
		r := c.runFake(append([]string{"--json"}, tt.args...)...)
		if doc := errorDoc(t, r); r.code != tt.code || doc.Message != tt.fact {
			t.Errorf("%v: exit %d, %+v", tt.args, r.code, doc)
		}
	}
	if b, _ := os.ReadFile(existing); string(b) != "mine" { // #nosec G304 -- test file
		t.Errorf("an existing file was changed to %q", b)
	}
	// The agent slot cannot read backups.
	c.setKey("env:KA")
	if r := c.runFake("dump", id); r.code == msg.ExitOK {
		t.Error("the agent slot dumped a snapshot")
	}
}

// TestDumpFailureLeavesNoFile: a dump that fails part-way removes its file.
func TestDumpFailureLeavesNoFile(t *testing.T) {
	c := backupEnv(t, "env:K1")
	id := strings.TrimSpace(c.runFake("backup", "prod-pg").stdout)
	for _, key := range c.objects("data/") {
		if err := os.Remove(c.objectPath(key)); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "broken.tar")
	if r := c.runFake("dump", id, "--output", out); r.code == msg.ExitOK {
		t.Fatal("a dump with its packs missing succeeded")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed dump left %s: %v", out, err)
	}
}
