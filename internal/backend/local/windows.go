// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package local

import (
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

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

// openRead opens a file for reading and lets others rename, replace or
// delete it meanwhile (FILE_SHARE_DELETE, which os.Open does not grant):
// otherwise a reader, such as a standby agent reading a lease record,
// makes the holder's overwrite of that record fail until it closes the
// file. The reader keeps the content it opened.
func openRead(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0) // the flag opens directories, as os.Open does
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// fileRenameInfoEx is FILE_RENAME_INFO with its flags (FileRenameInfoEx).
type fileRenameInfoEx struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// replaceFile moves oldpath to newpath, replacing it, with POSIX
// semantics (Windows 10 1607 and later, on NTFS): the replace succeeds
// while others have newpath open, and they keep reading the old file.
// MoveFileEx, which os.Rename uses, fails while any handle to newpath is
// open, so a standby agent reading a lease record would make its holder's
// renewal fail. Where the volume does not support it, it falls back to
// os.Rename. Both retry on transient locks.
func replaceFile(oldpath, newpath string) error {
	err := withRetry(func() error { return posixRename(oldpath, newpath) })
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		return withRetry(func() error { return os.Rename(oldpath, newpath) })
	}
	return err
}

func posixRename(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(newpath)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(from, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var info fileRenameInfoEx
	size := int(unsafe.Offsetof(info.FileName)) + len(name)*2
	buf := make([]byte, size)
	ri := (*fileRenameInfoEx)(unsafe.Pointer(&buf[0])) // #nosec G103 -- the Win32 FILE_RENAME_INFO layout, within buf
	ri.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	ri.FileNameLength = uint32((len(name) - 1) * 2)                                                                // #nosec G115 -- a path's length; without the NUL
	copy(unsafe.Slice(&ri.FileName[0], len(name)), name)                                                           // #nosec G103 -- buf holds the name after the fixed fields
	if err := windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(size)); err != nil { // #nosec G115 -- a small buffer
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

// syncDir is a no-op: Windows cannot flush a directory handle; NTFS
// journals the rename itself.
func syncDir(string) error { return nil }
