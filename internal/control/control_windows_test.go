// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package control

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestPipeAccessList: the pipe admits only the agent's user,
// Administrators and SYSTEM, with no inherited entries.
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
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	if !strings.Contains(sddl, "D:P") || !strings.Contains(sddl, user.User.Sid.String()) ||
		!strings.Contains(sddl, ";;;BA)") || !strings.Contains(sddl, ";;;SY)") {
		t.Errorf("access list = %s", sddl)
	}
	for _, other := range []string{";;;WD)", ";;;AU)", ";;;BU)", ";;;AN)"} {
		if strings.Contains(sddl, other) {
			t.Errorf("the pipe admits %s: %s", other, sddl)
		}
	}
}
