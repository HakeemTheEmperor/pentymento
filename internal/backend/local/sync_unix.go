// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build !windows

package local

import "os"

// syncDir flushes a directory's entries, so a rename or new file in it
// survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- a directory inside the repository
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// withRetry runs op once: only Windows needs rename retries.
func withRetry(op func() error) error { return op() }
