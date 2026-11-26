// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// The test binary doubles as fake tools: with fakeEnv set it acts as the
// tool named by its executable's base name.
const fakeEnv = "PENTYM_INSTANCE_FAKE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) != "" {
		fakeMain()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeMain is a fake postgres, mongod or docker. A server prints the
// version in version.txt next to it; docker appends its arguments to the
// file in PENTYM_FAKE_DOCKER_LOG and answers from canned outputs.
func fakeMain() {
	self, _ := os.Executable()
	name := strings.TrimSuffix(filepath.Base(self), ".exe")
	args := os.Args[1:]
	if name == "sleeper" {
		time.Sleep(time.Hour)
		return
	}
	if name != "docker" {
		v, _ := os.ReadFile(filepath.Join(filepath.Dir(self), "version.txt")) // #nosec G304 -- test fake
		fmt.Printf("%s (fake) %s\n", name, strings.TrimSpace(string(v)))
		return
	}
	f, _ := os.OpenFile(os.Getenv("PENTYM_FAKE_DOCKER_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 G703 -- test fake
	_, _ = fmt.Fprintln(f, strings.Join(args, "\x1f"))
	if len(args) > 2 && args[0] == "run" {
		for i, a := range args {
			if a == "--env-file" {
				b, _ := os.ReadFile(args[i+1]) // #nosec G304 G703 -- test fake
				_, _ = fmt.Fprintln(f, "ENVFILE\x1f"+strings.ReplaceAll(string(b), "\n", "\x1e"))
			}
		}
	}
	_ = f.Close()
	switch args[0] {
	case "version":
		if os.Getenv("PENTYM_FAKE_DOCKER_DOWN") != "" {
			fmt.Fprintln(os.Stderr, "Cannot connect to the Docker daemon")
			os.Exit(1)
		}
		fmt.Println("27.3.1")
	case "run":
		fmt.Println("0123456789ab")
	case "port":
		fmt.Println("127.0.0.1:55432")
	case "ps":
		fmt.Print(os.Getenv("PENTYM_FAKE_DOCKER_PS"))
	case "rm":
		if os.Getenv("PENTYM_FAKE_DOCKER_GONE") != "" {
			fmt.Fprintln(os.Stderr, "Error response from daemon: No such container: x")
			os.Exit(1)
		}
	}
}

// fakeTool places the test binary at dir/name, as a fake tool.
func fakeTool(t *testing.T, dir, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, exe(name))
	// A copy, not a link: on Windows a link to the running test binary
	// could not be deleted afterwards.
	b, err := os.ReadFile(self) // #nosec G304 -- the test binary
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o700); err != nil { // #nosec G306 G703 -- an executable
		t.Fatal(err)
	}
	return dst
}

// fakeServerDir makes a directory with a fake server binary of a version.
func fakeServerDir(t *testing.T, engine, version string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), engine+"-"+version)
	fakeTool(t, dir, serverBinary[engine])
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte(version), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newTestEnv(t *testing.T, tools config.Tools) *Env {
	t.Helper()
	work := t.TempDir()
	r := &toolrun.Runner{Environ: func() []string { return append(os.Environ(), fakeEnv+"=1") }}
	env := NewEnv(work, tools, r)
	env.LookPath = func(string) (string, error) { return "", errors.New("not on PATH") }
	return env
}

func TestFindAll(t *testing.T) {
	pg16, pg17 := fakeServerDir(t, Postgres, "16.6"), fakeServerDir(t, Postgres, "17.2")
	env := newTestEnv(t, config.Tools{Postgres: []string{pg16, pg17, pg16, filepath.Join(t.TempDir(), "empty")}})
	found, err := env.FindAll(context.Background(), Postgres)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].Version.Major != 16 || found[1].Version.Major != 17 {
		t.Fatalf("found = %+v", found)
	}
	f, err := env.find(context.Background(), Postgres, "17")
	if err != nil || f.Dir != pg17 {
		t.Errorf("find(17) = %+v, %v", f, err)
	}
	if _, err := env.find(context.Background(), Postgres, "18"); err == nil || err.Error() != "no postgres 18 binary in tools.postgres or on PATH (found 16, 17)" {
		t.Errorf("find(18) = %v", err)
	}

	// PATH is searched after the configured directories.
	mongo := fakeServerDir(t, MongoDB, "8.0.4")
	env = newTestEnv(t, config.Tools{})
	env.LookPath = func(name string) (string, error) { return filepath.Join(mongo, exe(name)), nil }
	if f, err := env.find(context.Background(), MongoDB, "8.0"); err != nil || f.Dir != mongo {
		t.Errorf("find(mongodb 8.0) via PATH = %+v, %v", f, err)
	}
	if _, err := env.find(context.Background(), MongoDB, "7.0"); err == nil || !strings.Contains(err.Error(), "(found 8.0)") {
		t.Errorf("find(mongodb 7.0) = %v", err)
	}
	env.LookPath = func(string) (string, error) { return "", errors.New("no") }
	if _, err := env.find(context.Background(), MongoDB, "8.0"); err == nil || err.Error() != "no mongod binary in tools.mongodb or on PATH" {
		t.Errorf("find with nothing = %v", err)
	}
}

func TestVersionMatches(t *testing.T) {
	for _, tt := range []struct {
		engine, want string
		got          toolrun.Version
		ok           bool
	}{
		{Postgres, "17", toolrun.Version{Major: 17, Minor: 2}, true},
		{Postgres, "17", toolrun.Version{Major: 16, Minor: 17}, false},
		{MongoDB, "8.0", toolrun.Version{Major: 8, Minor: 0, Patch: 4}, true},
		{MongoDB, "8.0", toolrun.Version{Major: 8, Minor: 2}, false},
	} {
		if versionMatches(tt.engine, tt.want, tt.got) != tt.ok {
			t.Errorf("versionMatches(%s, %s, %v) != %v", tt.engine, tt.want, tt.got, tt.ok)
		}
	}
}

// fakeProvider fails or succeeds as told.
type fakeProvider struct {
	name string
	err  error
}

func (f fakeProvider) Name() string { return f.name }
func (f fakeProvider) Start(context.Context, Spec) (Instance, error) {
	if f.err != nil {
		return nil, f.err
	}
	return nil, nil
}

func TestSetOrderAndError(t *testing.T) {
	env := newTestEnv(t, config.Tools{})
	s := NewSet(env, []string{"local", "docker"})
	if names := []string{s.Providers()[0].Name(), s.Providers()[1].Name()}; !slices.Equal(names, []string{"local", "docker"}) {
		t.Errorf("order = %v", names)
	}
	s.providers = []Provider{
		fakeProvider{"docker", errors.New("the Docker daemon is not reachable")},
		fakeProvider{"local", errors.New("no postgres 17 binary in tools.postgres or on PATH (found 16)")},
	}
	_, err := s.Start(context.Background(), Spec{Engine: Postgres, Version: "17", RunID: "r1"})
	var me *msg.Error
	if !errors.As(err, &me) || me.Message.ID != IDNoProvider || me.Message.Fact != "no instance provider could start postgres 17" ||
		me.Message.Cause != "docker: the Docker daemon is not reachable; local: no postgres 17 binary in tools.postgres or on PATH (found 16)" {
		t.Fatalf("Start = %v (%+v)", err, me)
	}
	// The first provider that works wins.
	s.providers = []Provider{fakeProvider{"docker", errors.New("down")}, fakeProvider{"local", nil}}
	if _, err := s.Start(context.Background(), Spec{Engine: Postgres, Version: "17"}); err != nil {
		t.Errorf("Start with a working provider = %v", err)
	}
}

// dockerLog reads the fake docker's recorded calls.
func dockerLog(t *testing.T, path string) [][]string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		out = append(out, strings.Split(l, "\x1f"))
	}
	return out
}

func newFakeDocker(t *testing.T, extraEnv ...string) (*Env, string) {
	t.Helper()
	env := newTestEnv(t, config.Tools{})
	bin := fakeTool(t, t.TempDir(), "docker")
	env.LookPath = func(string) (string, error) { return bin, nil }
	log := filepath.Join(t.TempDir(), "docker.log")
	base := append([]string{fakeEnv + "=1", "PENTYM_FAKE_DOCKER_LOG=" + log}, extraEnv...)
	env.Runner.Environ = func() []string { return append(os.Environ(), base...) }
	return env, log
}

// TestSecretLeakDockerRun checks the container's command line: published on
// 127.0.0.1 only, labelled, the data directory mounted, the password in
// an owner-only env file that is removed afterwards, and never on the
// command line.
func TestSecretLeakDockerRun(t *testing.T) {
	env, log := newFakeDocker(t)
	d := NewDocker(env)
	data := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Nothing listens on the fake port, so the readiness check fails, and
	// Start stops and removes the container.
	_, err := d.Start(ctx, Spec{Engine: Postgres, Version: "17", DataDir: data, RunID: "run1"})
	if err == nil {
		t.Fatal("Start succeeded without a server")
	}
	calls := dockerLog(t, log)
	var run []string
	var envFile string
	for _, c := range calls {
		switch c[0] {
		case "run":
			run = c
		case "ENVFILE":
			envFile = strings.ReplaceAll(c[1], "\x1e", "\n")
		}
	}
	if run == nil {
		t.Fatalf("no docker run in %v", calls)
	}
	joined := strings.Join(run, " ")
	for _, want := range []string{
		"--name pentym-run1-postgres", "--label pentym.run-id=run1", "--label pentym.owner-pid=" + strconv.Itoa(os.Getpid()),
		"--label pentym.engine=postgres", "--publish 127.0.0.1::5432", "--volume " + data + ":/var/lib/postgresql/data",
		"--env PGDATA=/var/lib/postgresql/data", "postgres:17",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run lacks %q:\n%s", want, joined)
		}
	}
	if (runtime.GOOS == "linux") != strings.Contains(joined, fmt.Sprintf("--user %d:%d", os.Getuid(), os.Getgid())) {
		t.Errorf("--user on %s: %s", runtime.GOOS, joined)
	}
	if !strings.HasPrefix(envFile, "POSTGRES_USER=pentym\nPOSTGRES_PASSWORD=") || len(envFile) < 50 {
		t.Fatalf("env file = %q", envFile)
	}
	pw := strings.TrimSpace(strings.TrimPrefix(envFile, "POSTGRES_USER=pentym\nPOSTGRES_PASSWORD="))
	for _, c := range calls {
		if c[0] != "ENVFILE" && strings.Contains(strings.Join(c, " "), pw) {
			t.Errorf("the password is on a docker command line: %v", c)
		}
	}
	if entries, _ := os.ReadDir(env.Runner.SecretDir); len(entries) != 0 {
		t.Errorf("secret files left: %v", entries)
	}
	last := calls[len(calls)-1]
	if last[0] != "rm" || last[len(last)-1] != "pentym-run1-postgres" || calls[len(calls)-2][0] != "stop" {
		t.Errorf("not stopped and removed after the failure: %v", calls[len(calls)-2:])
	}
}

func TestDockerDown(t *testing.T) {
	env, _ := newFakeDocker(t, "PENTYM_FAKE_DOCKER_DOWN=1")
	if _, err := NewDocker(env).ServerVersion(context.Background()); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("ServerVersion = %v", err)
	}
	if _, err := NewDocker(env).Start(context.Background(), Spec{Engine: MongoDB, Version: "8.0", DataDir: t.TempDir()}); err == nil {
		t.Error("Start succeeded with the daemon down")
	}
}

func TestDockerStopIdempotent(t *testing.T) {
	env, log := newFakeDocker(t, "PENTYM_FAKE_DOCKER_GONE=1")
	i := &dockerInstance{d: NewDocker(env), spec: Spec{RunID: "r"}, name: "pentym-r-mongodb"}
	if err := i.Stop(context.Background()); err != nil {
		t.Errorf("Stop of a removed container = %v", err)
	}
	if err := i.Stop(context.Background()); err != nil {
		t.Errorf("second Stop = %v", err)
	}
	if n := len(dockerLog(t, log)); n != 2 {
		t.Errorf("%d docker calls, want stop and rm once", n)
	}
}

// deadPID returns the PID of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	self, _ := os.Executable()
	r := &toolrun.Runner{Environ: func() []string { return append(os.Environ(), fakeEnv+"=1") }}
	p, err := r.Start(&toolrun.Cmd{Tool: "fake", Path: self, Stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Wait()
	for range 100 {
		if !toolrun.PIDAlive(p.Pid()) {
			return p.Pid()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the process did not exit")
	return 0
}

func TestCleanLeftovers(t *testing.T) {
	dead := deadPID(t)
	env, log := newFakeDocker(t, "PENTYM_FAKE_DOCKER_PS="+fmt.Sprintf("aaa %d\nbbb %d\nccc notapid\n", dead, os.Getpid()))
	// Local instance directories: a dead owner's, a live one's (this
	// process), and another host's.
	mk := func(name string, o owner) string {
		dir := filepath.Join(env.instancesDir(), name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeOwner(dir, o); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	gone := mk("r1-postgres", owner{Provider: "local", Engine: Postgres, Host: env.Host, PID: dead})
	mine := mk("r2-postgres", owner{Provider: "local", Engine: Postgres, Host: env.Host, PID: os.Getpid()})
	other := mk("r3-mongodb", owner{Provider: "local", Engine: MongoDB, Host: "elsewhere", PID: dead})
	stale := fmt.Sprintf("secret-%d-x", dead)
	if err := os.MkdirAll(env.Runner.SecretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.Runner.SecretDir, stale), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := NewSet(env, []string{"docker", "local"}).CleanLeftovers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// One local directory, one secret file, and the containers of the dead
	// owner and of the owner whose label is unreadable.
	if n != 4 {
		t.Errorf("removed %d items, want 4", n)
	}
	for dir, kept := range map[string]bool{gone: false, mine: true, other: true} {
		if _, err := os.Stat(dir); (err == nil) != kept {
			t.Errorf("%s kept = %v, want %v", dir, err == nil, kept)
		}
	}
	var removed []string
	for _, c := range dockerLog(t, log) {
		if c[0] == "rm" {
			removed = append(removed, c[len(c)-1])
		}
		if c[0] == "ps" && !slices.Contains(c, "label=pentym.owner-host="+env.Host) {
			t.Errorf("ps does not filter by this host: %v", c)
		}
	}
	if !slices.Equal(removed, []string{"aaa", "ccc"}) {
		t.Errorf("removed containers %v, want aaa and ccc", removed)
	}
}

// TestStopLeftoverLocalChecksLock: a recorded server PID is killed only if
// the server's lock file still names it.
func TestStopLeftoverLocalChecksLock(t *testing.T) {
	sleeper := fakeTool(t, t.TempDir(), "sleeper")
	r := &toolrun.Runner{Environ: func() []string { return append(os.Environ(), fakeEnv+"=1") }}
	p, err := r.Start(&toolrun.Cmd{Tool: "sleeper", Path: sleeper})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop(time.Second) }()
	data := t.TempDir()
	// The lock names another PID: the process must be left alone.
	if err := os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte("1\n/data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopLeftoverLocal(owner{Engine: Postgres, ServerPID: p.Pid(), DataDir: data})
	select {
	case <-p.Done():
		t.Fatal("a process not named by the lock file was killed")
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte(strconv.Itoa(p.Pid())+"\n/data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopLeftoverLocal(owner{Engine: Postgres, ServerPID: p.Pid(), DataDir: data})
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the leftover server was not stopped")
	}
}
