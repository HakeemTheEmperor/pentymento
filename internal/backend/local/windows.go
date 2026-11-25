// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package local

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// Rename retries on Windows, where antivirus and indexers briefly lock
// files: up to 10 attempts, waiting 10 ms and doubling up to 1 s.
const (
	retryAttempts = 10
	retryFirst    = 10 * time.Millisecond
	retryMax      = time.Second
)

// retrySleep waits between attempts; tests replace it.
var retrySleep = time.Sleep

// withRetry runs op, retrying while Windows reports a sharing violation or
// access denied.
func withRetry(op func() error) error {
	wait := retryFirst
	var err error
	for range retryAttempts {
		if err = op(); err == nil || !retryable(err) {
			return err
		}
		retrySleep(wait)
		wait = min(wait*2, retryMax)
	}
	return err
}

func retryable(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// renameNoReplace moves oldpath to newpath without replacing an existing
// file (MoveFileEx without MOVEFILE_REPLACE_EXISTING), retrying on
// transient locks.
func renameNoReplace(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	err = withRetry(func() error { return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH) })
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return fmt.Errorf("%w: %s", backend.ErrExists, newpath)
	}
	return err
}

// syncDir is a no-op: Windows cannot flush a directory handle; NTFS
// journals the rename itself.
func syncDir(string) error { return nil }
