// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build !windows

package control

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
)

// maxSocketPath is the longest Unix socket path every platform accepts
// (macOS allows 104 bytes including the terminator).
const maxSocketPath = 103

func listen(addr string) (net.Listener, error) {
	if len(addr) > maxSocketPath {
		return nil, fmt.Errorf("the path is %d bytes, longer than Unix sockets allow (%d); set a shorter agent.socket", len(addr), maxSocketPath)
	}
	dir := filepath.Dir(addr)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	// Nothing answered, so a socket there was left by a crashed agent.
	if fi, err := os.Lstat(addr); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", addr)
		}
		if err := os.Remove(addr); err != nil {
			return nil, err
		}
	}
	l, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(addr, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", addr)
}
