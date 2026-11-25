// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build darwin

package local

import (
	"bytes"

	"golang.org/x/sys/unix"
)

var darwinNetworkFS = map[string]bool{"nfs": true, "smbfs": true, "afpfs": true, "webdav": true, "cifs": true}

func filesystem(path string) (FSInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	name := string(bytes.TrimRight(st.Fstypename[:], "\x00"))
	return FSInfo{Type: name, Network: darwinNetworkFS[name]}, nil
}
