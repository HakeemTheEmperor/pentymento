// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package local

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func filesystem(path string) (FSInfo, error) {
	vol := filepath.VolumeName(path)
	if strings.HasPrefix(vol, `\\`) {
		return FSInfo{Type: "unc", Network: true}, nil
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return FSInfo{}, err
	}
	info := FSInfo{Network: windows.GetDriveType(root) == windows.DRIVE_REMOTE}
	name := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err == nil { // #nosec G115 -- MAX_PATH+1
		info.Type = windows.UTF16ToString(name)
	}
	return info, nil
}
