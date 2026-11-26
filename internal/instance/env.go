// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package instance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Engines.
const (
	Postgres = "postgres"
	MongoDB  = "mongodb"
)

// superuser is the side instance's superuser name.
const superuser = "pentym"

// Timeouts.
const (
	// readyTimeout bounds how long an instance may take to accept a
	// login, after its binaries or image are in place.
	readyTimeout = 3 * time.Minute
	// stopTimeout bounds a clean shutdown before the server is killed.
	stopTimeout = 60 * time.Second
)

// Env is what the providers share: the tool runner, the work directory
// and the configured tool directories.
type Env struct {
	Runner *toolrun.Runner
	// WorkDir holds instances/ (one directory per running instance) and
	// the runner's secrets/.
	WorkDir string
	Tools   config.Tools
	// Host names this machine in owner records, so leftovers are only
	// cleaned up by the host that created them.
	Host string
	// LookPath finds a binary on PATH; nil means exec.LookPath.
	LookPath func(string) (string, error)
}

// NewEnv returns the providers' environment for a work directory, with a
// tool runner whose secret files live in <work_dir>/secrets.
func NewEnv(workDir string, tools config.Tools, runner *toolrun.Runner) *Env {
	host, _ := os.Hostname()
	if runner.SecretDir == "" {
		runner.SecretDir = SecretDir(workDir)
	}
	return &Env{Runner: runner, WorkDir: workDir, Tools: tools, Host: host}
}

// SecretDir is where the tool runner keeps temporary credential files.
func SecretDir(workDir string) string { return filepath.Join(workDir, "secrets") }

func (e *Env) instancesDir() string { return filepath.Join(e.WorkDir, "instances") }

// instanceDir creates a private directory for one instance's files: its
// owner record, sockets and logs.
func (e *Env) instanceDir(spec Spec) (string, error) {
	dir := filepath.Join(e.instancesDir(), spec.RunID+"-"+spec.Engine)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o700) // #nosec G302 -- a directory: owner-only needs the search bit
}

func (e *Env) lookPath(name string) (string, error) {
	if e.LookPath != nil {
		return e.LookPath(name)
	}
	return exec.LookPath(name)
}

// owner records who started an instance, so a later run can tell whether
// it is a leftover: its owner process is gone.
type owner struct {
	Provider string `json:"provider"`
	Engine   string `json:"engine"`
	Host     string `json:"host"`
	PID      int    `json:"pid"`
	// ServerPID and DataDir identify a local server; the server's own lock
	// file in DataDir must name ServerPID before it is stopped.
	ServerPID int    `json:"server_pid,omitempty"`
	DataDir   string `json:"data_dir,omitempty"`
	BinDir    string `json:"bin_dir,omitempty"`
}

const ownerFile = "owner.json"

func writeOwner(dir string, o owner) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ownerFile), b, 0o600)
}

func readOwner(dir string) (owner, error) {
	var o owner
	b, err := os.ReadFile(filepath.Join(dir, ownerFile)) // #nosec G304 -- pentym's own work directory
	if err != nil {
		return o, err
	}
	return o, json.Unmarshal(b, &o)
}

// newPassword returns a random password for a side instance's superuser.
func newPassword() secrets.Secret {
	b := make([]byte, 24)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	pw := base64.RawURLEncoding.EncodeToString(b)
	clear(b)
	return secrets.New([]byte(pw))
}

// freePort finds a free TCP port on 127.0.0.1. Another process could take
// it before the server binds; the server then fails to start and says so.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// exe adds .exe on Windows.
func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// emptyDir reports whether dir exists and has no entries.
func emptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("data directory %s does not exist", dir)
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// versionMatches reports whether a found version is the one asked for: the
// major version for Postgres ("17"), major.minor for MongoDB ("8.0").
func versionMatches(engine, want string, got toolrun.Version) bool {
	if engine == Postgres {
		return fmt.Sprint(got.Major) == want
	}
	return fmt.Sprintf("%d.%d", got.Major, got.Minor) == want
}

func versionText(engine string, v toolrun.Version) string {
	if engine == Postgres {
		return fmt.Sprint(v.Major)
	}
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

// waitFor polls fn until it succeeds, ctx ends or timeout passes, and
// returns fn's last error.
func waitFor(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		attempt, stop := context.WithTimeout(ctx, 10*time.Second)
		last = fn(attempt)
		stop()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready after %s: %w", timeout, last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}
