// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package toolrun

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// Version is a tool or server version, such as 17.2 or 100.10.0.
type Version struct {
	Major, Minor, Patch int
}

func (v Version) String() string {
	if v.Patch != 0 {
		return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion finds the first version number in a tool's --version output:
//
//	pg_dump (PostgreSQL) 17.2 (Ubuntu 17.2-1.pgdg24.04+1)   → 17.2
//	db version v8.0.4                                       → 8.0.4
//	mongodump version: 100.10.0                             → 100.10.0
func ParseVersion(out []byte) (Version, error) {
	m := versionRE.FindSubmatch(out)
	if m == nil {
		first, _, _ := bytes.Cut(out, []byte("\n"))
		return Version{}, fmt.Errorf("no version number in %q", truncate(string(first), 80))
	}
	var v Version
	v.Major, _ = strconv.Atoi(string(m[1]))
	v.Minor, _ = strconv.Atoi(string(m[2]))
	if len(m[3]) > 0 {
		v.Patch, _ = strconv.Atoi(string(m[3]))
	}
	return v, nil
}

// ToolVersion runs `<path> --version` and parses its output.
func (r *Runner) ToolVersion(ctx context.Context, tool, path string) (Version, error) {
	var out bytes.Buffer
	if err := r.Run(ctx, &Cmd{Tool: tool, Path: path, Args: []string{"--version"}, Stdout: &out}); err != nil {
		return Version{}, err
	}
	v, err := ParseVersion(out.Bytes())
	if err != nil {
		return Version{}, fmt.Errorf("%s --version: %w", tool, err)
	}
	return v, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
