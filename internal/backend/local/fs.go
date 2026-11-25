// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package local

// FSInfo describes the filesystem holding the repository, for doctor.
type FSInfo struct {
	// Type is the filesystem's name, such as "ext4", "nfs" or "NTFS", when
	// known.
	Type string
	// Network is true for network filesystems (NFS, SMB/CIFS, AFP, WebDAV,
	// 9P, network drives), where atomic creates and renames may not hold.
	Network bool
	// Unknown is true when the type could not be classified, such as FUSE.
	Unknown bool
}

// Filesystem reports the filesystem holding the repository.
func (b *Backend) Filesystem() (FSInfo, error) { return filesystem(b.root) }
