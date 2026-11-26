// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build !windows

package toolrun

import (
	"os"
	"testing"
)

// assertPrivate fails unless only the owner can access path.
func assertPrivate(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("%s has mode %v; others can access it", path, fi.Mode().Perm())
	}
}
