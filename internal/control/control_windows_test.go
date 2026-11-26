// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package control

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestPipeAccessList: the pipe admits exactly the agent's user,
// Administrators and SYSTEM, with no inherited entries. It compares SIDs,
// not SDDL text, which abbreviates well-known accounts (a runner's built-in
// Administrator prints as LA).
func TestPipeAccessList(t *testing.T) {
	_, addr := serve(t, testServer(&fakeAgent{}))
	// Reading the access list opens the pipe like a client; until the
	// server has its next instance waiting, Windows answers that every
	// instance is busy, so try again briefly, as clients do.
	var sd *windows.SECURITY_DESCRIPTOR
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		sd, err = windows.GetNamedSecurityInfo(addr, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sd.String(), "D:P") {
		t.Errorf("the access list inherits entries: %s", sd)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*windows.SID{"the agent's user": user.User.Sid, "Administrators": admins, "SYSTEM": system}

	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("entry %d is not an allow entry: type %d", i, ace.Header.AceType)
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) // #nosec G103 -- the SID follows the ACE header, as Windows lays it out
		matched := ""
		for name, w := range want {
			if sid.Equals(w) {
				matched = name
			}
		}
		if matched == "" {
			t.Errorf("the pipe admits %s, which it must not", sid)
			continue
		}
		found[matched] = true
	}
	for name := range want {
		if !found[name] {
			t.Errorf("the pipe does not admit %s: %s", name, sd)
		}
	}
}
