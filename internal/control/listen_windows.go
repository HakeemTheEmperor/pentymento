// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package control

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// listen creates the named pipe with an access list that admits only the
// agent's user, Administrators and SYSTEM, with no inherited entries.
func listen(addr string) (net.Listener, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sddl := "D:P(A;;GA;;;" + user.User.Sid.String() + ")(A;;GA;;;BA)(A;;GA;;;SY)"
	return winio.ListenPipe(addr, &winio.PipeConfig{SecurityDescriptor: sddl})
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, addr)
}
