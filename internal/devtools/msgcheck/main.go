// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Command msgcheck checks the PTM- message registry (Brand 11.3): every ID
// is well formed and unique, has a troubleshooting entry at
// docs/errors/<ID>.md, and no entry exists without an ID. Given a base
// revision, it also checks that no ID was removed, renamed or un-retired
// since then, so IDs are never reused.
//
// Usage, from the repository root:
//
//	go run ./internal/devtools/msgcheck [<base revision>]
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

const (
	registryPath = "internal/msg/registry.go"
	docsDir      = "docs/errors"
)

func main() {
	os.Exit(run(os.Args[1:], ".", os.Stdout, os.Stderr))
}

func run(args []string, root string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		_, _ = fmt.Fprintln(stderr, "usage: msgcheck [<base revision>]")
		return 2
	}
	src, err := os.ReadFile(root + "/" + registryPath) // #nosec G304 -- fixed path under the repository root
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "msgcheck: %v\n", err)
		return 2
	}
	head, err := parseRegistry(src)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "msgcheck: %s: %v\n", registryPath, err)
		return 2
	}
	docFiles, err := listDocs(root + "/" + docsDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "msgcheck: %v\n", err)
		return 2
	}

	problems := checkRegistry(head, docFiles)
	problems = append(problems, checkCompiled(head)...)
	if len(args) == 1 {
		baseSrc, found, err := gitShow(root, args[0], registryPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "msgcheck: %v\n", err)
			return 2
		}
		if found {
			base, err := parseRegistry(baseSrc)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "msgcheck: %s at %s: %v\n", registryPath, args[0], err)
				return 2
			}
			problems = append(problems, compareRegistries(base, head)...)
		}
	}

	if len(problems) > 0 {
		for _, p := range problems {
			_, _ = fmt.Fprintf(stdout, "✗ %s\n", p)
		}
		_, _ = fmt.Fprintln(stdout, "→ see the message ID rules in CONTRIBUTING.md")
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "✓ %d message IDs, each with a docs entry\n", len(head))
	return 0
}

// checkCompiled confirms the parsed source matches the registry compiled
// into the binary, so the source parse cannot drift from the real table.
// It is meaningful only when run on the working tree this binary was built
// from, which is how CI and make run it.
func checkCompiled(parsed []entry) []string {
	compiled := msg.Entries()
	if len(compiled) != len(parsed) {
		return []string{fmt.Sprintf("parsed %d registry entries but the build has %d", len(parsed), len(compiled))}
	}
	for i, c := range compiled {
		p := parsed[i]
		if string(c.ID) != p.ID || c.Name != p.Name || c.Retired != p.Retired {
			return []string{fmt.Sprintf("registry entry %d does not match the build (%s)", i+1, c.ID)}
		}
	}
	return nil
}

func listDocs(dir string) ([]string, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range des {
		if !d.IsDir() {
			names = append(names, d.Name())
		}
	}
	return names, nil
}

// gitShow returns a file's content at a revision; found is false when the
// file does not exist there.
func gitShow(root, rev, path string) (content []byte, found bool, err error) {
	// #nosec G204 G702 -- runs git only, with a revision from the developer or CI.
	cmd := exec.Command("git", "-C", root, "show", rev+":"+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		errText := stderr.String()
		if strings.Contains(errText, "does not exist") || strings.Contains(errText, "exists on disk, but not in") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("git show %s:%s: %w: %s", rev, path, err, strings.TrimSpace(errText))
	}
	return out, true, nil
}
