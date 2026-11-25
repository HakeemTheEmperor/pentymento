// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build !windows

package config

import (
	"os"
	"testing"
)

// makeReadableByOthers gives the file group and world read permission.
func makeReadableByOthers(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil { // #nosec G302 -- deliberately readable, to test the check
		t.Fatal(err)
	}
}
