// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build !windows

package toolrun

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// group puts a child and everything it starts in one process group, so
// they can be stopped together.
type group struct{ pgid int }

func newGroup() (*group, error) { return &group{}, nil }

// prepare runs the child as the leader of a new process group.
func (g *group) prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// started records the group: the child's PID is its group ID.
func (g *group) started(cmd *exec.Cmd) error {
	g.pgid = cmd.Process.Pid
	return nil
}

// terminate asks every process in the group to stop.
func (g *group) terminate() error { return g.signal(syscall.SIGTERM) }

// kill stops every process in the group at once.
func (g *group) kill() error { return g.signal(syscall.SIGKILL) }

func (g *group) signal(sig syscall.Signal) error {
	if g.pgid <= 0 {
		return nil
	}
	err := syscall.Kill(-g.pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil // already gone
	}
	return err
}

// close kills anything the child left running in its group.
func (g *group) close() { _ = g.kill() }

// pidAlive reports whether a process exists. EPERM means it exists but
// belongs to someone else.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// restrict makes a file or directory accessible to its owner only. The
// file was created 0600 or the directory 0700; this repairs a directory
// that already existed with wider permissions.
func restrict(path string, dir bool) error {
	mode := os.FileMode(0o600)
	if dir {
		mode = 0o700
	}
	return os.Chmod(path, mode)
}
