// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build linux

package local

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// renameNoReplace renames oldpath to newpath atomically, failing with
// backend.ErrExists if newpath exists (renameat2 RENAME_NOREPLACE). Where
// the filesystem does not support it, it falls back to a hard link.
func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%w: %s", backend.ErrExists, newpath)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.ENOTSUP):
		return linkNoReplace(oldpath, newpath)
	}
	return err
}
