// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const registrySrc = `package msg

var entries = []Entry{
	{ID: "PTM-2001", Name: "connection-refused"},
	{ID: "PTM-3001", Name: "bucket-missing", Retired: true},
}
`

func TestParseRegistry(t *testing.T) {
	got, err := parseRegistry([]byte(registrySrc))
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{{"PTM-2001", "connection-refused", false}, {"PTM-3001", "bucket-missing", true}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseRegistry = %+v, want %+v", got, want)
	}
}

func TestParseRegistryEmpty(t *testing.T) {
	got, err := parseRegistry([]byte("package msg\n\nvar entries = []Entry{}\n"))
	if err != nil || len(got) != 0 {
		t.Fatalf("parseRegistry(empty) = %v, %v", got, err)
	}
}

func TestParseRegistryRejects(t *testing.T) {
	tests := map[string]string{
		"no entries":       "package msg\n",
		"constant ID":      "package msg\nconst c = \"PTM-1001\"\nvar entries = []Entry{{ID: c, Name: \"x\"}}\n",
		"positional":       "package msg\nvar entries = []Entry{{\"PTM-1001\", \"x\", false}}\n",
		"unknown field":    "package msg\nvar entries = []Entry{{ID: \"PTM-1001\", Name: \"x\", Other: 1}}\n",
		"retired variable": "package msg\nvar r = true\nvar entries = []Entry{{ID: \"PTM-1001\", Name: \"x\", Retired: r}}\n",
	}
	for name, src := range tests {
		if _, err := parseRegistry([]byte(src)); err == nil {
			t.Errorf("%s: parseRegistry accepted %q", name, src)
		}
	}
}

func TestCheckRegistry(t *testing.T) {
	tests := []struct {
		name    string
		entries []entry
		docs    []string
		want    string // substring of a problem; empty means none
	}{
		{name: "valid", entries: []entry{{ID: "PTM-2001", Name: "connection-refused"}}, docs: []string{"PTM-2001.md", "README.md"}},
		{name: "empty", entries: nil, docs: []string{"README.md"}},
		{name: "bad format", entries: []entry{{ID: "PTM-201", Name: "x"}}, docs: []string{"PTM-201.md"}, want: "not a valid message ID"},
		{name: "block zero", entries: []entry{{ID: "PTM-0201", Name: "x"}}, docs: []string{"PTM-0201.md"}, want: "not a valid message ID"},
		{name: "duplicate ID", entries: []entry{{ID: "PTM-2001", Name: "a"}, {ID: "PTM-2001", Name: "b"}}, docs: []string{"PTM-2001.md"}, want: "more than once"},
		{name: "duplicate name", entries: []entry{{ID: "PTM-2001", Name: "a"}, {ID: "PTM-2002", Name: "a"}}, docs: []string{"PTM-2001.md", "PTM-2002.md"}, want: "already used"},
		{name: "bad name", entries: []entry{{ID: "PTM-2001", Name: "Connection Refused"}}, docs: []string{"PTM-2001.md"}, want: "kebab-case"},
		{name: "missing docs", entries: []entry{{ID: "PTM-2001", Name: "a"}}, want: "no troubleshooting entry"},
		{name: "orphan docs", docs: []string{"PTM-2001.md"}, want: "no entry in the registry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertProblem(t, checkRegistry(tt.entries, tt.docs), tt.want)
		})
	}
}

func TestCompareRegistries(t *testing.T) {
	base := []entry{{ID: "PTM-2001", Name: "a"}, {ID: "PTM-2002", Name: "b", Retired: true}}
	tests := []struct {
		name string
		head []entry
		want string
	}{
		{name: "unchanged", head: base},
		{name: "appended", head: append(append([]entry(nil), base...), entry{ID: "PTM-2003", Name: "c"})},
		{name: "retired", head: []entry{{ID: "PTM-2001", Name: "a", Retired: true}, base[1]}},
		{name: "removed", head: base[1:], want: "was removed"},
		{name: "renamed", head: []entry{{ID: "PTM-2001", Name: "z"}, base[1]}, want: "was renamed"},
		{name: "un-retired", head: []entry{base[0], {ID: "PTM-2002", Name: "b"}}, want: "cannot be reused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertProblem(t, compareRegistries(base, tt.head), tt.want)
		})
	}
}

func assertProblem(t *testing.T, problems []string, want string) {
	t.Helper()
	if want == "" {
		if len(problems) != 0 {
			t.Fatalf("want no problems, got %q", problems)
		}
		return
	}
	for _, p := range problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Fatalf("want a problem containing %q, got %q", want, problems)
}

// TestRepositoryRegistry runs the check against this repository, so
// `go test ./...` also enforces the registry rules.
func TestRepositoryRegistry(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, "../../..", &stdout, &stderr); code != 0 {
		t.Fatalf("msgcheck on the repository = %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

// TestRunAgainstBase checks the base comparison through git, with a
// temporary repository whose history removes an ID.
func TestRunAgainstBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- test helper; runs git with fixed arguments
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, content string) {
		t.Helper()
		p := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Ada Lovelace")
	git("config", "user.email", "ada@example.com")
	git("config", "commit.gpgsign", "false")
	write(registryPath, "package msg\n\nvar entries = []Entry{{ID: \"PTM-2001\", Name: \"a\"}}\n")
	write(docsDir+"/PTM-2001.md", "# PTM-2001\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")

	// The working tree drops the ID; the compiled registry is empty, which
	// matches the new working tree.
	write(registryPath, "package msg\n\nvar entries = []Entry{}\n")
	if err := os.Remove(filepath.Join(dir, docsDir, "PTM-2001.md")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"HEAD"}, dir, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "PTM-2001 was removed") {
		t.Fatalf("run = %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	// A directory without a registry file cannot be checked.
	stdout.Reset()
	if code := run([]string{"HEAD"}, t.TempDir(), &stdout, &stderr); code != 2 {
		t.Fatalf("run without a registry file = %d, want 2", code)
	}
}
