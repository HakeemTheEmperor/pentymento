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

// gitRepo creates a temporary repository and changes into it for the test.
func gitRepo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Chdir(t.TempDir())
	runGit(t, "init", "-q", "-b", "main")
	runGit(t, "config", "user.name", "Ada Lovelace")
	runGit(t, "config", "user.email", "ada@example.com")
	runGit(t, "config", "commit.gpgsign", "false")
}

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput() // #nosec G204 -- test helper; runs git with fixed arguments

	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, path, msg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, "add", path)
	runGit(t, "commit", "-q", "-m", msg)
}

func TestRunRange(t *testing.T) {
	gitRepo(t)
	commitFile(t, "README.md", "docs: start\n\nSigned-off-by: Ada Lovelace <ada@example.com>")
	base := runGit(t, "rev-parse", "HEAD")

	runGit(t, "checkout", "-q", "-b", "feature")
	commitFile(t, "cmd/pentym/a.txt", "cmd/pentym: add a\n\nSigned-off-by: Ada Lovelace <ada@example.com>")

	// A merge commit with a message that breaks every rule is skipped.
	runGit(t, "checkout", "-q", "main")
	commitFile(t, "other.txt", "docs: other\n\nSigned-off-by: Ada Lovelace <ada@example.com>")
	runGit(t, "checkout", "-q", "feature")
	runGit(t, "merge", "-q", "--no-ff", "-m", "Merge branch 'main' into feature", "main")

	var stdout, stderr bytes.Buffer
	if code := run([]string{base + "..HEAD"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "✓ 2 commits") {
		t.Fatalf("stdout = %q, want 2 commits checked", stdout.String())
	}

	// A bad commit fails the range.
	head := runGit(t, "rev-parse", "HEAD")
	commitFile(t, "cmd/pentym/b.txt", "Signed-off-by: Ada Lovelace <ada@example.com>\n\ncmd/pentym: add b")
	stdout.Reset()
	if code := run([]string{head + "..HEAD"}, &stdout, &stderr); code != 1 {
		t.Fatalf("run = %d, want 1\nstdout: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "1 of 1 commits") {
		t.Fatalf("stdout = %q, want 1 of 1 commits failing", stdout.String())
	}
}

func TestRunUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("run(nil) = %d, want 2", code)
	}
	if code := run([]string{"HEAD"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(HEAD) = %d, want 2", code)
	}
}
