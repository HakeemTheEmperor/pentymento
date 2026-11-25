// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build !windows

package config

import "os"

// readableByOthers reports whether the file's group or other read bit is
// set.
func readableByOthers(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return fi.Mode().Perm()&0o044 != 0, nil
}

func privateFix(file string) string { return "chmod 600 " + file }
