// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as an agent process when childEnv names a
// repository directory: it runs an agent for one target with the fast
// timings, prints its lease changes on stdout, and stops gracefully when
// its stdin closes.
const childEnv = "PENTYM_AGENT_TEST_CHILD"

func TestMain(m *testing.M) {
	if dir := os.Getenv(childEnv); dir != "" {
		os.Exit(childMain(dir, os.Getenv("PENTYM_AGENT_TEST_HOST")))
	}
	os.Exit(m.Run())
}

func childMain(dir, host string) int {
	r := openAs(&childTB{}, dir, nil, "agent")
	out := &syncWriter{w: os.Stdout}
	a, err := New(Options{
		Config: testConfig("prod-pg"), Repo: r, Host: host, Timings: fastTimings, Heartbeat: 100 * time.Millisecond,
		OnLease: func(_ string, held bool, gen uint64) {
			if held {
				out.line(fmt.Sprintf("held %d", gen))
			} else {
				out.line("lost")
			}
		},
	})
	if err != nil {
		out.line("error " + err.Error())
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	out.line("started")
	if err := a.Run(ctx); err != nil {
		out.line("error " + err.Error())
		return 1
	}
	out.line("stopped")
	return 0
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) line(l string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintln(s.w, l)
}

// childTB is the little of testing.TB the helpers need in a child process.
type childTB struct{ testing.TB }

func (*childTB) Helper()                      {}
func (*childTB) TempDir() string              { return os.TempDir() }
func (*childTB) Fatal(args ...any)            { fmt.Fprintln(os.Stderr, args...); os.Exit(2) }
func (*childTB) Fatalf(f string, args ...any) { fmt.Fprintf(os.Stderr, f+"\n", args...); os.Exit(2) }

// child is a running agent process.
type child struct {
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	mu    sync.Mutex
	lines []string
	exit  chan error
}

func startChild(t *testing.T, dir, name string) *child {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self) // #nosec G204 -- the test binary itself
	cmd.Env = append(os.Environ(), childEnv+"="+dir, "PENTYM_AGENT_TEST_HOST="+name)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{name: name, cmd: cmd, stdin: stdin, exit: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			c.mu.Lock()
			c.lines = append(c.lines, sc.Text())
			c.mu.Unlock()
		}
		c.exit <- cmd.Wait()
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitFor(t, name+" to start", 10*time.Second, func() bool { return c.saw("started") })
	return c
}

func (c *child) saw(prefix string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// holding reports whether the child's latest lease line says it holds.
func (c *child) holding() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.lines) - 1; i >= 0; i-- {
		switch l := c.lines[i]; {
		case strings.HasPrefix(l, "held"):
			return true
		case l == "lost":
			return false
		}
	}
	return false
}

func (c *child) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "; ")
}

// TestLeaseHandoverBetweenProcesses (Build Order C17): two agent processes
// for one target run side by side with exactly one holder; when the holder
// is killed, the other takes over once the lease expires; when the holder
// stops gracefully, it releases the lease and the other takes over at once.
func TestLeaseHandoverBetweenProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts agent processes")
	}
	dir := newRepoDir(t)
	a := startChild(t, dir, "a")
	waitFor(t, "a to hold the lease", 10*time.Second, a.holding)
	b := startChild(t, dir, "b")
	// Side by side: b only watches while a renews.
	deadline := time.Now().Add(3 * fastTimings.Length)
	for time.Now().Before(deadline) {
		if b.holding() {
			t.Fatalf("b took the lease while a held it: a: %s / b: %s", a.output(), b.output())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Killed: no release, so b waits out the lease length.
	killed := time.Now()
	if err := a.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-a.exit //nolint:errcheck // killed: its exit error is expected
	waitFor(t, "b to take over from the killed a", 10*time.Second, b.holding)
	// The lease runs from a's last renewal, at most one renew interval
	// before the kill, so b may take over that much early, never sooner.
	if took, earliest := time.Since(killed), fastTimings.Length-fastTimings.Renew; took < earliest {
		t.Errorf("b took over %s after a was killed, before the lease could expire (%s)", took, earliest)
	}
	if !b.saw("held 2") {
		t.Errorf("b holds a generation other than 2: %s", b.output())
	}

	// Graceful: b releases, and c takes over without waiting.
	c := startChild(t, dir, "c")
	time.Sleep(2 * fastTimings.Check)
	if c.holding() {
		t.Fatalf("c took the lease while b held it: %s", c.output())
	}
	stopping := time.Now()
	_ = b.stdin.Close()
	select {
	case err := <-b.exit:
		if err != nil || !b.saw("stopped") {
			t.Fatalf("b's graceful stop: %v: %s", err, b.output())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("b did not stop")
	}
	// b has exited, so its release is written: c must take over on its
	// next check, not after the lease length. b's shutdown itself (durable
	// writes of the release and the final heartbeat) is not part of this.
	exited := time.Now()
	waitFor(t, "c to take over from the stopped b", 10*time.Second, c.holding)
	took := time.Since(exited)
	t.Logf("b shut down in %s; c took over %s after b exited", exited.Sub(stopping), took)
	if took >= fastTimings.Length {
		t.Errorf("c took over %s after b exited; a released lease is taken over at once", took)
	}
	if !c.saw("held 3") {
		t.Errorf("c holds a generation other than 3: %s", c.output())
	}
	_ = c.stdin.Close()
	if err := <-c.exit; err != nil {
		t.Errorf("c's graceful stop: %v: %s", err, c.output())
	}
}
