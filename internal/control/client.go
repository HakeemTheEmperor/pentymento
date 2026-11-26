// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/HakeemTheEmperor/pentymento/internal/agent"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// Client talks to a running agent over its control socket.
type Client struct {
	addr string
	hc   *http.Client
}

// NewClient returns a client for the socket at addr. Requests have no
// overall timeout: a backup through the agent runs as long as it takes,
// and cancelling ctx abandons it.
func NewClient(addr string) *Client {
	return &Client{addr: addr, hc: &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return Dial(ctx, addr) },
		DisableKeepAlives: true,
	}}}
}

// Addr is the socket the client talks to.
func (c *Client) Addr() string { return c.addr }

// do sends a request and decodes the reply into out, or the agent's error
// into a *msg.Error with the agent's exit code.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, out any) error {
	u := url.URL{Scheme: "http", Host: "pentym-agent", Path: path, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err // wraps ErrUnreachable when nothing answers
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error ErrorBody `json:"error"`
		}
		if json.Unmarshal(body, &e) != nil || e.Error.Message == "" {
			return fmt.Errorf("the agent answered %s", resp.Status)
		}
		return &msg.Error{Code: msg.ExitCode(e.Error.Code), Message: msg.Message{
			Level: msg.LevelFailed, ID: msg.ID(e.Error.ID), Fact: e.Error.Message,
			Cause: e.Error.Cause, Impact: e.Error.Impact, Next: e.Error.Next,
		}}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("the agent's reply is not understood: %w", err)
	}
	return nil
}

// Status returns the agent's live status.
func (c *Client) Status(ctx context.Context) (*agent.Status, error) {
	var s agent.Status
	if err := c.do(ctx, http.MethodGet, PathStatus, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Reload makes the agent reload its configuration.
func (c *Client) Reload(ctx context.Context) (*agent.ReloadResult, error) {
	var r agent.ReloadResult
	if err := c.do(ctx, http.MethodPost, PathReload, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Backup makes the agent back up a target now and decodes its report
// into out.
func (c *Client) Backup(ctx context.Context, target string, out any) error {
	return c.do(ctx, http.MethodPost, PathBackup, url.Values{"target": {target}}, out)
}
