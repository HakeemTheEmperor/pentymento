// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package toolrun

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFind(t *testing.T) {
	empty, dir := t.TempDir(), t.TempDir()
	tool := filepath.Join(dir, ExeName("mongodump"))
	if err := os.WriteFile(tool, nil, 0o700); err != nil { // #nosec G306 -- an executable
		t.Fatal(err)
	}
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }
	if p, err := Find("mongodump", "mongodb", []string{empty, dir}, notOnPath); err != nil || p != tool {
		t.Errorf("in a tools directory: %q, %v", p, err)
	}
	onPath := func(n string) (string, error) { return "/usr/bin/" + n, nil }
	if p, err := Find("mongodump", "mongodb", []string{empty}, onPath); err != nil || p != "/usr/bin/mongodump" {
		t.Errorf("on PATH: %q, %v", p, err)
	}
	// A directory of the tool's name is not the tool.
	if err := os.Mkdir(filepath.Join(empty, ExeName("pg_dump")), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Find("pg_dump", "postgres", []string{empty}, notOnPath); err == nil || err.Error() != "pg_dump is not in tools.postgres or on PATH" {
		t.Errorf("missing: %v", err)
	}
}
