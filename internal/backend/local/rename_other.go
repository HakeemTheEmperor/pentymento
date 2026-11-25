// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build !linux && !darwin && !windows

package local

// renameNoReplace publishes oldpath at newpath with a hard link, which is
// atomic and fails if newpath exists.
func renameNoReplace(oldpath, newpath string) error { return linkNoReplace(oldpath, newpath) }
