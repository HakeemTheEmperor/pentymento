// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/agent"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// testAddr is a fresh socket address: a Unix socket in a short temporary
// directory, or a uniquely named pipe.
func testAddr(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	if runtime.GOOS == "windows" {
		return `\\.\pipe\pentym-test-` + hex.EncodeToString(b)
	}
	dir, err := os.MkdirTemp("", "pc") // short: socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "agent.sock")
}

type fakeAgent struct{ ready atomic.Bool }

func (f *fakeAgent) Status() agent.Status {
	return agent.Status{AgentID: "a1", Host: "db-1", Ready: f.ready.Load(),
		Targets: []agent.TargetStatus{{Name: "prod-pg", Holding: true, Generation: 4}}}
}
func (f *fakeAgent) Ready() bool { return f.ready.Load() }

// serve runs the server on a fresh socket and returns a client.
func serve(t *testing.T, s *Server) (*Client, string) {
	t.Helper()
	addr := testAddr(t)
	l, err := Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewClient(addr), addr
}

func testServer(fa *fakeAgent) *Server {
	return &Server{
		Agent: fa,
		Reload: func(context.Context) (*agent.ReloadResult, error) {
			return &agent.ReloadResult{Added: []string{"prod-mg"}, Unchanged: []string{"prod-pg"}}, nil
		},
		Backup: func(_ context.Context, target string) (any, error) {
			if target != "prod-pg" {
				return nil, &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
					Level: msg.LevelFailed, Fact: "the running agent has no target " + target, Next: "pentym agent reload"}}
			}
			return map[string]string{"snapshot": "abc"}, nil
		},
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pentym_up 1\n") }),
	}
}

func TestServerAndClient(t *testing.T) {
	fa := &fakeAgent{}
	fa.ready.Store(true)
	c, _ := serve(t, testServer(fa))
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil || st.AgentID != "a1" || len(st.Targets) != 1 || st.Targets[0].Generation != 4 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	rr, err := c.Reload(ctx)
	if err != nil || strings.Join(rr.Added, ",") != "prod-mg" {
		t.Errorf("reload = %+v, %v", rr, err)
	}
	var out map[string]string
	if err := c.Backup(ctx, "prod-pg", &out); err != nil || out["snapshot"] != "abc" {
		t.Errorf("backup = %v, %v", out, err)
	}
	// The agent's error comes back with its message and exit code.
	err = c.Backup(ctx, "nosuch", &out)
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitNotFound || me.Message.Fact != "the running agent has no target nosuch" || me.Message.Next != "pentym agent reload" {
		t.Errorf("backup of an unknown target = %v", err)
	}
	// A plain error is reported as a failure, scrubbed.
	s := testServer(fa)
	s.Reload = func(context.Context) (*agent.ReloadResult, error) {
		return nil, errors.New("config invalid: password=hunter2")
	}
	c2, _ := serve(t, s)
	_, err = c2.Reload(ctx)
	if !errors.As(err, &me) || me.Code != msg.ExitFailure || strings.Contains(me.Message.Fact, "hunter2") {
		t.Errorf("plain error = %v", err)
	}
}

// get fetches a path through the socket.
func get(t *testing.T, c *Client, path string) (int, string) {
	t.Helper()
	resp, err := c.hc.Get("http://pentym-agent" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHealthReadyMetricsPProf(t *testing.T) {
	fa := &fakeAgent{}
	c, _ := serve(t, testServer(fa))
	if code, _ := get(t, c, PathHealth); code != http.StatusOK {
		t.Errorf("health = %d", code)
	}
	if code, _ := get(t, c, PathReady); code != http.StatusServiceUnavailable {
		t.Errorf("ready while starting = %d", code)
	}
	fa.ready.Store(true)
	if code, _ := get(t, c, PathReady); code != http.StatusOK {
		t.Errorf("ready = %d", code)
	}
	if code, body := get(t, c, PathMetrics); code != http.StatusOK || body != "pentym_up 1\n" {
		t.Errorf("metrics = %d %q", code, body)
	}
	if code, _ := get(t, c, "/debug/pprof/"); code != http.StatusNotFound {
		t.Errorf("pprof without agent.debug.pprof = %d", code)
	}
	s := testServer(fa)
	s.PProf = true
	c2, _ := serve(t, s)
	if code, _ := get(t, c2, "/debug/pprof/"); code != http.StatusOK {
		t.Errorf("pprof enabled = %d", code)
	}
}

// TestHealthPortServesOnlyHealth: the TCP health port has no control
// action and no profiling.
func TestHealthPortServesOnlyHealth(t *testing.T) {
	fa := &fakeAgent{}
	fa.ready.Store(true)
	srv := httptest.NewServer(HealthHandler(fa, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "m") })))
	defer srv.Close()
	for path, want := range map[string]int{
		PathHealth: 200, PathReady: 200, PathMetrics: 200,
		PathStatus: 404, PathReload: 404, PathBackup + "?target=prod-pg": 404, "/debug/pprof/": 404,
	} {
		method := http.MethodGet
		if path == PathReload || strings.HasPrefix(path, PathBackup) {
			method = http.MethodPost
		}
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s %s = %d, want %d", method, path, resp.StatusCode, want)
		}
	}
}

func TestUnreachable(t *testing.T) {
	c := NewClient(testAddr(t))
	_, err := c.Status(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("no agent = %v", err)
	}
}

// TestListenRefusesLiveAgent: a second agent cannot take over the socket
// of a live one; a socket left by a crashed agent is replaced.
func TestListenRefusesLiveAgent(t *testing.T) {
	_, addr := serve(t, testServer(&fakeAgent{}))
	_, err := Listen(addr)
	var me *msg.Error
	if !errors.As(err, &me) || !strings.Contains(me.Message.Fact, "another agent is already running") {
		t.Errorf("second listener = %v", err)
	}
	if runtime.GOOS == "windows" {
		return // a pipe disappears with its server
	}
	stale := testAddr(t)
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close() // the socket file stays, as after a crash
	l2, err := Listen(stale)
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	_ = l2.Close()
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(stale); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("a regular file at the socket path = %v", err)
	}
}

func TestSocketPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes carry an access list instead")
	}
	_, addr := serve(t, testServer(&fakeAgent{}))
	fi, err := os.Stat(addr)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v", fi.Mode().Perm())
	}
	long := "/tmp/" + strings.Repeat("x", 120) + "/agent.sock"
	if _, err := Listen(long); err == nil || !strings.Contains(err.Error(), "longer than Unix sockets allow") {
		t.Errorf("a long path = %v", err)
	}
}
