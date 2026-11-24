// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Command headercheck checks that every source file in the repository
// starts with the SPDX licence header and copyright line (CONTRIBUTING.md).
// It checks the files git tracks plus untracked files that are not ignored.
//
// Usage, from the repository root:
//
//	go run ./internal/devtools/headercheck
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

func main() {
	os.Exit(run(os.Stdout, os.Stderr))
}

func run(stdout, stderr io.Writer) int {
	out, err := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "headercheck: list files: %v\n", err)
		return 2
	}
	var missing []string
	checked := 0
	for _, p := range strings.Split(string(out), "\x00") {
		prefix, ok := commentPrefix(p)
		if p == "" || !ok {
			continue
		}
		content, err := os.ReadFile(p) // #nosec G304 -- paths come from git ls-files in this repository
		if errors.Is(err, fs.ErrNotExist) {
			continue // tracked but deleted in the working tree
		}
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "headercheck: read %s: %v\n", p, err)
			return 2
		}
		checked++
		if !hasHeader(content, prefix) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		for _, p := range missing {
			_, _ = fmt.Fprintf(stdout, "✗ %s: missing licence header\n", p)
		}
		_, _ = fmt.Fprintf(stdout, "%d of %d files lack the SPDX licence header and copyright line\n", len(missing), checked)
		_, _ = fmt.Fprintln(stdout, "→ add the two header lines shown in CONTRIBUTING.md to the top of each file")
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "✓ %d files have the licence header\n", checked)
	return 0
}
