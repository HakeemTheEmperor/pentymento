// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Command commitcheck checks that every non-merge commit in a git revision
// range follows the project's commit conventions (CONTRIBUTING.md): a
// "<package path>: <lowercase summary>" subject whose prefix is a real
// directory, all or docs; at most 72 characters; no trailing full stop; a
// blank second line; and a DCO Signed-off-by line for the author in the
// final paragraph.
//
// Usage:
//
//	go run ./internal/devtools/commitcheck <base>..<head>
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || !strings.Contains(args[0], "..") {
		_, _ = fmt.Fprintln(stderr, "usage: commitcheck <base>..<head>")
		return 2
	}
	shas, err := git("rev-list", "--no-merges", "--reverse", args[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "commitcheck: list commits in %s: %v\n", args[0], err)
		return 2
	}
	failed := 0
	checked := 0
	for _, sha := range strings.Fields(shas) {
		c, err := readCommit(sha)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "commitcheck: read %s: %v\n", sha, err)
			return 2
		}
		checked++
		problems := CheckMessage(c, func(p string) bool { return isDirAt(sha, p) })
		if len(problems) == 0 {
			continue
		}
		failed++
		subject, _, _ := strings.Cut(c.Message, "\n")
		_, _ = fmt.Fprintf(stdout, "✗ %.12s %s\n", sha, subject)
		for _, p := range problems {
			_, _ = fmt.Fprintf(stdout, "  %s\n", p)
		}
	}
	if failed > 0 {
		_, _ = fmt.Fprintf(stdout, "%d of %d commits break the commit conventions\n", failed, checked)
		_, _ = fmt.Fprintln(stdout, "→ see CONTRIBUTING.md, then fix with git commit --amend -s or git rebase")
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "✓ %d commits follow the commit conventions\n", checked)
	return 0
}

func readCommit(sha string) (Commit, error) {
	out, err := git("log", "-1", "--format=%an%x00%ae%x00%B", sha)
	if err != nil {
		return Commit{}, err
	}
	parts := strings.SplitN(out, "\x00", 3)
	if len(parts) != 3 {
		return Commit{}, fmt.Errorf("unexpected git log output")
	}
	return Commit{AuthorName: parts[0], AuthorEmail: parts[1], Message: parts[2]}, nil
}

// isDirAt reports whether path is a directory in the tree of the commit or
// of its first parent, so a commit that removes a directory may still name
// it.
func isDirAt(sha, path string) bool {
	for _, rev := range []string{sha, sha + "^"} {
		out, err := git("cat-file", "-t", rev+":"+path)
		if err == nil && strings.TrimSpace(out) == "tree" {
			return true
		}
	}
	return false
}

func git(args ...string) (string, error) {
	// #nosec G204 G702 -- runs git only, with arguments built in this file and a
	// revision range from the developer or CI.
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
