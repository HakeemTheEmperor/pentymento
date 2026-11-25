// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build linux

package local

import "golang.org/x/sys/unix"

var linuxFS = map[int64]struct {
	name    string
	network bool
}{
	unix.NFS_SUPER_MAGIC:       {"nfs", true},
	unix.SMB_SUPER_MAGIC:       {"smb", true},
	unix.SMB2_SUPER_MAGIC:      {"smb2", true},
	unix.CIFS_SUPER_MAGIC:      {"cifs", true},
	unix.V9FS_MAGIC:            {"9p", true},
	unix.EXT4_SUPER_MAGIC:      {"ext4", false},
	unix.XFS_SUPER_MAGIC:       {"xfs", false},
	unix.BTRFS_SUPER_MAGIC:     {"btrfs", false},
	unix.TMPFS_MAGIC:           {"tmpfs", false},
	unix.OVERLAYFS_SUPER_MAGIC: {"overlay", false},
}

func filesystem(path string) (FSInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	if fs, ok := linuxFS[int64(st.Type)]; ok { // #nosec G115 -- filesystem magic numbers fit in int64
		return FSInfo{Type: fs.name, Network: fs.network}, nil
	}
	if int64(st.Type) == unix.FUSE_SUPER_MAGIC {
		return FSInfo{Type: "fuse", Unknown: true}, nil
	}
	return FSInfo{Unknown: true}, nil
}
