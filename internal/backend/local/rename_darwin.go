// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build darwin

package local

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// renameNoReplace renames oldpath to newpath atomically, failing with
// backend.ErrExists if newpath exists (renamex_np RENAME_EXCL). Where the
// filesystem does not support it, it falls back to a hard link.
func renameNoReplace(oldpath, newpath string) error {
	err := unix.RenamexNp(oldpath, newpath, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%w: %s", backend.ErrExists, newpath)
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
		return linkNoReplace(oldpath, newpath)
	}
	return err
}
