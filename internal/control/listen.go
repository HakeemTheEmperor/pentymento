// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// ErrUnreachable means no agent answers on the control socket.
var ErrUnreachable = errors.New("agent not reachable")

// dialTimeout bounds connecting to the socket; a live agent accepts at
// once.
const dialTimeout = 2 * time.Second

// Listen opens the control socket at addr: a Unix socket readable only by
// this user, or a Windows named pipe for this user, Administrators and
// SYSTEM. A socket left by a crashed agent is replaced; one a live agent
// answers on is refused.
func Listen(addr string) (net.Listener, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	if c, err := dial(ctx, addr); err == nil {
		_ = c.Close()
		return nil, &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
			Level: msg.LevelFailed, Fact: "another agent is already running on the control socket " + addr,
			Next: "stop that agent first, or give this one its own agent.socket",
		}}
	}
	l, err := listen(addr)
	if err != nil {
		return nil, fmt.Errorf("open the control socket %s: %w", addr, err)
	}
	return l, nil
}

// Dial connects to the control socket at addr; ErrUnreachable means no
// agent answers there.
func Dial(ctx context.Context, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, err := dial(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("%w at %s: %w", ErrUnreachable, addr, err)
	}
	return c, nil
}
