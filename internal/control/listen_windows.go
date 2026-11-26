// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package control

import (
	"context"
	"errors"
	"net"
	"time"

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
	l, err := winio.ListenPipe(addr, &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		return nil, err
	}
	return &pipeListener{Listener: l}, nil
}

// Waiting for go-winio's listener to close.
const (
	closeNudge = 100 * time.Millisecond
	closeLimit = 5 * time.Second
)

// pipeListener works around a race in go-winio's pipe listener: a Close
// that arrives while a connection is being accepted can be taken by the
// accept, which then fails with an error the listener does not treat as
// closing, so it waits for another close signal and Close waits for it
// forever (holding http.Server's lock, which blocks Serve too). Close here
// sends another close signal while the first is unanswered; once the
// listener stops, every Close returns.
type pipeListener struct{ net.Listener }

func (p *pipeListener) Close() error {
	done := make(chan struct{})
	go func() { _ = p.Listener.Close(); close(done) }()
	nudge := time.NewTicker(closeNudge)
	defer nudge.Stop()
	limit := time.NewTimer(closeLimit)
	defer limit.Stop()
	for {
		select {
		case <-done:
			return nil
		case <-nudge.C:
			go func() { _ = p.Listener.Close() }()
		case <-limit.C:
			return errors.New("the control pipe did not close")
		}
	}
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, addr)
}
