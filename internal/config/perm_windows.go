// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package config

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// readAccess is any access mask that allows reading a file's data.
const readAccess = windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL

// broadSIDs are the groups that mean "other users": Everyone, Users and
// Authenticated Users.
var broadSIDs = []windows.WELL_KNOWN_SID_TYPE{
	windows.WinWorldSid, windows.WinBuiltinUsersSid, windows.WinAuthenticatedUserSid,
}

// readableByOthers reports whether the file's DACL lets Everyone, Users or
// Authenticated Users read it. A missing (null) DACL grants everyone
// access.
func readableByOthers(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, err
	}
	if dacl == nil {
		return true, nil
	}
	var broad []*windows.SID
	for _, t := range broadSIDs {
		s, err := windows.CreateWellKnownSid(t)
		if err != nil {
			return false, err
		}
		broad = append(broad, s)
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Mask&readAccess == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) // #nosec G103 -- the SID starts at SidStart in an ACCESS_ALLOWED_ACE
		for _, b := range broad {
			if sid.Equals(b) {
				return true, nil
			}
		}
	}
	return false, nil
}

func privateFix(file string) string {
	return "remove read access for Everyone, Users and Authenticated Users from " + file
}
