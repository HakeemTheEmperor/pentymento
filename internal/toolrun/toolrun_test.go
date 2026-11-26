// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package toolrun

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// The test binary doubles as the child process: with helperEnv set, it
// runs helperMain instead of the tests.
const helperEnv = "PENTYM_TOOLRUN_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		helperMain(os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helperMain is a stand-in tool. Its first argument chooses what it does.
func helperMain(args []string) {
	switch args[0] {
	case "echo": // copy stdin to stdout
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "stderr": // write each argument as a stderr line, with {SECRET} replaced
		for _, a := range args[1:] {
			fmt.Fprintln(os.Stderr, strings.ReplaceAll(a, "{SECRET}", os.Getenv("TOOLRUN_SECRET")))
		}
	case "exit": // write a stderr line, then exit with the code
		fmt.Fprintln(os.Stderr, "fatal: "+strings.ReplaceAll(args[2], "{SECRET}", os.Getenv("TOOLRUN_SECRET")))
		code, _ := strconv.Atoi(args[1])
		os.Exit(code)
	case "record": // record argv and environment in a file
		b, _ := json.Marshal(map[string][]string{"args": os.Args, "env": os.Environ()})
		_ = os.WriteFile(args[1], b, 0o600) // #nosec G703 -- test helper writing where the test asks
	case "cat-env-file": // print the file named by an environment variable
		b, _ := os.ReadFile(os.Getenv(args[1])) // #nosec G304 G703 -- test helper
		_, _ = os.Stdout.Write(b)
	case "tree": // start a grandchild, write both PIDs, then wait forever or exit
		if len(args) > 3 && args[3] == "ignore-term" {
			signal.Ignore(syscall.SIGTERM)
		}
		child := exec.Command(os.Args[0], "sleep") // #nosec G204 G702 -- test helper
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			os.Exit(9)
		}
		_ = os.WriteFile(args[1], fmt.Appendf(nil, "%d %d", os.Getpid(), child.Process.Pid), 0o600) // #nosec G703 -- test helper writing where the test asks
		if args[2] == "exit" {
			return
		}
		time.Sleep(time.Hour)
	case "sleep":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
	case "--version":
		fmt.Println("helper (PostgreSQL) 17.2 (Debian 17.2-1)")
	case "version":
		fmt.Println(strings.Join(args[1:], " "))
	}
}

// helper returns a Cmd running the helper with args.
func helper(args ...string) *Cmd {
	exe, _ := os.Executable()
	return &Cmd{Tool: "helper", Path: exe, Args: args, Env: []string{helperEnv + "=1"}, RunID: "run123"}
}

func newRunner(t *testing.T) (*Runner, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	return &Runner{
		SecretDir: filepath.Join(t.TempDir(), "secrets"),
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
		Grace:     500 * time.Millisecond,
	}, &logs
}

func TestRunStreams(t *testing.T) {
	r, _ := newRunner(t)
	in := make([]byte, 5<<20)
	_, _ = rand.Read(in)
	var out bytes.Buffer
	c := helper("echo")
	c.Stdin, c.Stdout = bytes.NewReader(in), &out
	if err := r.Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), in) {
		t.Errorf("stdout: %d bytes, want the %d bytes of stdin", out.Len(), len(in))
	}
}

func TestRunLogsStderr(t *testing.T) {
	r, logs := newRunner(t)
	pw := secrets.New([]byte("hunter2-pw"))
	c := helper("stderr", "connecting", "password {SECRET} rejected", "done")
	c.Secrets = []secrets.Secret{pw}
	c.Env, c.AllowEnvSecret = append(c.Env, "TOOLRUN_SECRET=hunter2-pw"), true
	if err := r.Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	var lines []map[string]string
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]string
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 3 || lines[1]["line"] != "password [redacted] rejected" || lines[0]["tool"] != "helper" || lines[2]["run_id"] != "run123" {
		t.Errorf("logs:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "hunter2") {
		t.Error("a secret reached the log")
	}
}

func TestRunExitError(t *testing.T) {
	r, _ := newRunner(t)
	err := r.Run(context.Background(), helper("exit", "3", "role does not exist"))
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || ee.Tool != "helper" || len(ee.Stderr) != 1 || ee.Stderr[0] != "fatal: role does not exist" {
		t.Fatalf("Run = %v (%+v)", err, ee)
	}
	if err.Error() != "helper exited with code 3: fatal: role does not exist" {
		t.Errorf("message = %q", err.Error())
	}
	if err := r.Run(context.Background(), &Cmd{Tool: "nothing", Path: filepath.Join(t.TempDir(), "no-such-tool")}); err == nil || !strings.HasPrefix(err.Error(), "start nothing: ") {
		t.Errorf("missing executable: %v", err)
	}
}

// readPIDs waits for the helper's PID file.
func readPIDs(t *testing.T, file string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(file) // #nosec G304 -- test file
		if err == nil && len(b) > 0 {
			var a, b2 int
			if _, err := fmt.Sscanf(string(b), "%d %d", &a, &b2); err == nil {
				return a, b2
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the helper never wrote its PIDs")
	return 0, 0
}

// waitDead fails unless every PID is gone within a few seconds.
func waitDead(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, p := range pids {
			alive = alive || pidAlive(p)
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range pids {
		if pidAlive(p) {
			t.Errorf("process %d is still running", p)
		}
	}
}

// TestRunCancelStopsTree: cancelling stops the tool and the process it
// started, gently first and by force after the grace period.
func TestRunCancelStopsTree(t *testing.T) {
	for _, mode := range []string{"obeys", "ignore-term"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := newRunner(t)
			pidFile := filepath.Join(t.TempDir(), "pids")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx, helper("tree", pidFile, "wait", mode)) }()
			child, grandchild := readPIDs(t, pidFile)
			start := time.Now()
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Run = %v, want context.Canceled", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("Run did not return after cancellation")
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("stopping took %s", d)
			}
			waitDead(t, child, grandchild)
		})
	}
}

// TestRunKillsLeftovers: a process the tool left behind when it exited is
// stopped too.
func TestRunKillsLeftovers(t *testing.T) {
	r, _ := newRunner(t)
	pidFile := filepath.Join(t.TempDir(), "pids")
	if err := r.Run(context.Background(), helper("tree", pidFile, "exit")); err != nil {
		t.Fatal(err)
	}
	child, grandchild := readPIDs(t, pidFile)
	waitDead(t, child, grandchild)
}

func TestRunRefuses(t *testing.T) {
	r, _ := newRunner(t)
	pw := secrets.New([]byte("hunter2-pw"))
	record := filepath.Join(t.TempDir(), "ran")
	tests := []struct {
		name string
		mod  func(c *Cmd)
		want error
	}{
		{"secret in an argument", func(c *Cmd) { c.Args = append(c.Args, "--password=hunter2-pw") }, ErrSecretInArgs},
		{"secret in an extra argument", func(c *Cmd) { c.Extra = []string{"hunter2-pw"} }, ErrSecretInArgs},
		{"secret in the environment", func(c *Cmd) { c.Env = append(c.Env, "PGPASSWORD=hunter2-pw") }, ErrSecretInEnv},
		{"compression flag", func(c *Cmd) { c.Tool, c.Extra = "pg_basebackup", []string{"-Z9"} }, ErrSourceCompression},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := helper("record", record)
			c.Secrets = []secrets.Secret{pw}
			tt.mod(c)
			err := r.Run(context.Background(), c)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Run = %v, want %v", err, tt.want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the error shows the secret: %v", err)
			}
			if _, err := os.Stat(record); err == nil {
				t.Error("the tool ran")
			}
		})
	}
	// PGPASSWORD is allowed when asked for, as the fallback.
	c := helper("record", record)
	c.Secrets, c.Env, c.AllowEnvSecret = []secrets.Secret{pw}, append(c.Env, "PGPASSWORD=hunter2-pw"), true
	if err := r.Run(context.Background(), c); err != nil {
		t.Fatalf("with AllowEnvSecret: %v", err)
	}
}

func TestCheckCompression(t *testing.T) {
	for _, tt := range []struct {
		tool string
		args []string
		bad  bool
	}{
		{"pg_basebackup", []string{"-D", "-", "-Ft", "-X", "none"}, false},
		{"pg_basebackup", []string{"-z"}, true},
		{"pg_basebackup", []string{"-Z", "9"}, true},
		{"pg_basebackup", []string{"-Z9"}, true},
		{"pg_basebackup", []string{"--compress=server-gzip"}, true},
		{"pg_basebackup", []string{"--gzip"}, true},
		{"pg_basebackup", []string{"--compress=none"}, false}, // off is allowed
		{"pg_basebackup", []string{"-Z", "0"}, false},
		{"pg_basebackup", []string{"-Z"}, true}, // no value: the tool would fail anyway
		{"pg_dump", []string{"-Fc", "-Z0"}, false},
		{"pg_dump", []string{"-Fc", "--compress", "gzip:9"}, true},
		{"pg_dump", []string{"-Fc", "--compress=zstd"}, true},
		{"pg_dump", []string{"-t", "orders"}, false},
		{"mongodump", []string{"--archive", "--oplog"}, false},
		{"mongodump", []string{"--gzip"}, true},
		{"mongorestore", []string{"--gzip"}, false}, // reading, not producing
		{"pg_restore", []string{"-Z9"}, false},
	} {
		err := CheckCompression(tt.tool, tt.args)
		if (err != nil) != tt.bad || (err != nil && !errors.Is(err, ErrSourceCompression)) {
			t.Errorf("%s %v: %v", tt.tool, tt.args, err)
		}
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Version{
		"pg_dump (PostgreSQL) 17.2 (Ubuntu 17.2-1.pgdg24.04+1)\n": {17, 2, 0},
		"postgres (PostgreSQL) 16.6\n":                            {16, 6, 0},
		"db version v8.0.4\nBuild Info: {}\n":                     {8, 0, 4},
		"mongodump version: 100.10.0\ngit version: abc\n":         {100, 10, 0},
		"Docker version 27.3.1, build ce12230\n":                  {27, 3, 1},
	} {
		got, err := ParseVersion([]byte(in))
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseVersion([]byte("no numbers here\n")); err == nil {
		t.Error("ParseVersion accepted text without a version")
	}
	if !(Version{16, 6, 0}).Less(Version{17, 0, 0}) || (Version{17, 2, 0}).Less(Version{17, 1, 9}) || (Version{8, 0, 4}).String() != "8.0.4" {
		t.Error("Version comparison or formatting")
	}
}

func TestToolVersion(t *testing.T) {
	r, _ := newRunner(t)
	r.Environ = func() []string { return append(os.Environ(), helperEnv+"=1") }
	exe, _ := os.Executable()
	v, err := r.ToolVersion(context.Background(), "helper", exe)
	if err != nil || v != (Version{17, 2, 0}) {
		t.Errorf("ToolVersion = %v, %v", v, err)
	}
}

// TestStartStop: a background tool runs until stopped; Stop ends it and
// what it started, deletes its secret files, and is not an error.
func TestStartStop(t *testing.T) {
	r, _ := newRunner(t)
	pidFile := filepath.Join(t.TempDir(), "pids")
	f, err := WriteSecretFile(r.SecretDir, ".pw", secrets.New([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	c := helper("tree", pidFile, "wait", "ignore-term")
	c.Files = []*SecretFile{f}
	p, err := r.Start(c)
	if err != nil {
		t.Fatal(err)
	}
	child, grandchild := readPIDs(t, pidFile)
	if p.Pid() != child {
		t.Errorf("Pid = %d, want %d", p.Pid(), child)
	}
	select {
	case <-p.Done():
		t.Fatal("the tool exited by itself")
	case <-time.After(200 * time.Millisecond):
	}
	if err := p.Stop(500 * time.Millisecond); err != nil {
		t.Errorf("Stop = %v", err)
	}
	waitDead(t, child, grandchild)
	if _, err := os.Stat(f.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("secret file left: %v", err)
	}
	if err := p.Stop(time.Second); err != nil {
		t.Errorf("second Stop = %v", err)
	}
}

// TestStartExit: a background tool that fails reports it through Wait.
func TestStartExit(t *testing.T) {
	r, _ := newRunner(t)
	p, err := r.Start(helper("exit", "4", "bad option"))
	if err != nil {
		t.Fatal(err)
	}
	var ee *ExitError
	if err := p.Wait(); !errors.As(err, &ee) || ee.Code != 4 {
		t.Errorf("Wait = %v", err)
	}
	if _, err := r.Start(&Cmd{Tool: "pg_dump", Path: "x", Extra: []string{"-Z9"}}); !errors.Is(err, ErrSourceCompression) {
		t.Errorf("Start with a compression flag = %v", err)
	}
}
