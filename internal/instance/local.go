// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package instance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Local runs side instances from binaries on this machine (SDD 4.3):
// postgres or mongod of the matching version, run as children through the
// tool runner, so they stop with pentym.
type Local struct{ env *Env }

// NewLocal returns the local-binaries provider.
func NewLocal(env *Env) *Local { return &Local{env: env} }

// Name is "local".
func (*Local) Name() string { return "local" }

// localInstance is a running local server.
type localInstance struct {
	env     *Env
	spec    Spec
	dir     string // the instance's own directory
	binDir  string
	proc    *toolrun.Process
	conn    Conn
	once    sync.Once
	stopErr error
}

func (i *localInstance) Conn() Conn { return i.conn }

// LastOutput returns the server's last stderr lines once it has exited;
// a server still running has nothing to explain.
func (i *localInstance) LastOutput(context.Context) string {
	if i.proc == nil {
		return ""
	}
	select {
	case <-i.proc.Done():
	default:
		return ""
	}
	var ee *toolrun.ExitError
	switch err := i.proc.Wait(); {
	case errors.As(err, &ee):
		return fmt.Sprintf("exited with code %d:\n%s", ee.Code, strings.Join(ee.Stderr, "\n"))
	case err != nil:
		return err.Error()
	}
	return "the server exited"
}

// Start starts a server over spec.DataDir. An empty data directory is
// initialized first, with a random superuser password; a directory that
// already holds data is used as it is, and the engine must have arranged
// access (restores, C16).
func (l *Local) Start(ctx context.Context, spec Spec) (Instance, error) {
	found, err := l.env.find(ctx, spec.Engine, spec.Version)
	if err != nil {
		return nil, err
	}
	empty, err := emptyDir(spec.DataDir)
	if err != nil {
		return nil, err
	}
	dir, err := l.env.instanceDir(spec)
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	inst := &localInstance{env: l.env, spec: spec, dir: dir, binDir: found.Dir,
		conn: Conn{Host: "127.0.0.1", Port: port, User: superuser}}
	if empty {
		inst.conn.Password = newPassword()
	}
	o := owner{Provider: l.Name(), Engine: spec.Engine, Host: l.env.Host, PID: os.Getpid(), DataDir: spec.DataDir, BinDir: found.Dir}
	if err := writeOwner(dir, o); err != nil {
		return nil, err
	}
	fail := func(err error) (Instance, error) {
		_ = inst.Stop(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("local %s %s: %w", spec.Engine, spec.Version, err)
	}
	switch spec.Engine {
	case Postgres:
		err = inst.startPostgres(ctx, empty)
	case MongoDB:
		err = inst.startMongo(ctx, empty)
	default:
		err = fmt.Errorf("unknown engine %q", spec.Engine)
	}
	if err != nil {
		return fail(err)
	}
	o.ServerPID = inst.proc.Pid()
	if err := writeOwner(dir, o); err != nil {
		return fail(err)
	}
	if !empty {
		return inst, nil // access is the engine's to arrange
	}
	if spec.Engine == MongoDB {
		if err := inst.createMongoUser(ctx); err != nil {
			return fail(fmt.Errorf("%w; see %s", err, inst.logFile()))
		}
	}
	if err := awaitReady(ctx, spec, inst.conn); err != nil {
		return fail(fmt.Errorf("%w; see %s", err, inst.logFile()))
	}
	return inst, nil
}

func (i *localInstance) bin(name string) string { return filepath.Join(i.binDir, exe(name)) }

func (i *localInstance) logFile() string { return filepath.Join(i.dir, "server.log") }

// startPostgres initializes the cluster if needed, points it at 127.0.0.1
// and the instance's private socket directory, and starts postgres.
func (i *localInstance) startPostgres(ctx context.Context, fresh bool) error {
	r := i.env.Runner
	if fresh {
		pw, err := toolrun.WriteSecretFile(r.SecretDir, ".pw", i.conn.Password)
		if err != nil {
			return err
		}
		if err := r.Run(ctx, &toolrun.Cmd{
			Tool: "initdb", Path: i.bin("initdb"), RunID: i.spec.RunID, Files: []*toolrun.SecretFile{pw},
			Args:    []string{"-D", i.spec.DataDir, "-U", superuser, "--pwfile=" + pw.Path, "--auth=scram-sha-256", "--encoding=UTF8", "--no-instructions"},
			Secrets: []secrets.Secret{i.conn.Password},
		}); err != nil {
			return err
		}
	}
	sockets := "''"
	if runtime.GOOS != "windows" {
		sockets = pgQuote(i.dir)
	}
	settings := fmt.Sprintf("\n# Added by pentym for a side instance.\nlisten_addresses = '127.0.0.1'\nport = %d\nunix_socket_directories = %s\nlogging_collector = off\n", i.conn.Port, sockets)
	f, err := os.OpenFile(filepath.Join(i.spec.DataDir, "postgresql.auto.conf"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the instance's data directory
	if err != nil {
		return err
	}
	_, werr := f.WriteString(settings)
	if err := errors.Join(werr, f.Close()); err != nil {
		return err
	}
	logf, err := os.OpenFile(i.logFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the instance's own directory
	if err != nil {
		return err
	}
	i.proc, err = r.Start(&toolrun.Cmd{Tool: "postgres", Path: i.bin("postgres"), RunID: i.spec.RunID,
		Args: []string{"-D", i.spec.DataDir}, Stdout: logf})
	if err != nil {
		_ = logf.Close()
		return err
	}
	go func() { <-i.proc.Done(); _ = logf.Close() }()
	return nil
}

// pgQuote quotes a value for postgresql.conf.
func pgQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// startMongo starts mongod on 127.0.0.1 with authentication on.
func (i *localInstance) startMongo(_ context.Context, _ bool) error {
	args := []string{"--dbpath", i.spec.DataDir, "--bind_ip", "127.0.0.1", "--port", strconv.Itoa(i.conn.Port),
		"--auth", "--logpath", i.logFile(), "--logappend"}
	if runtime.GOOS != "windows" {
		args = append(args, "--unixSocketPrefix", i.dir)
	}
	var err error
	i.proc, err = i.env.Runner.Start(&toolrun.Cmd{Tool: "mongod", Path: i.bin("mongod"), RunID: i.spec.RunID, Args: args})
	return err
}

// createMongoUser creates the superuser through the localhost exception:
// with authentication on and no users yet, a client on 127.0.0.1 may
// create the first one.
func (i *localInstance) createMongoUser(ctx context.Context) error {
	noAuth := i.conn
	noAuth.User = ""
	return waitFor(ctx, readyTimeout, func(ctx context.Context) error {
		// Once the user exists, the localhost exception is closed: try
		// logging in first, so a retry after a lost reply succeeds.
		if _, err := mongoVersion(ctx, i.conn); err == nil {
			return nil
		}
		cl, err := mongoClientNoAuth(noAuth)
		if err != nil {
			return err
		}
		defer func() { _ = cl.Disconnect(context.Background()) }()
		err = cl.Database("admin").RunCommand(ctx, bson.D{
			{Key: "createUser", Value: superuser},
			{Key: "pwd", Value: string(i.conn.Password.Expose())},
			{Key: "roles", Value: bson.A{"root"}},
		}).Err()
		if err != nil {
			return secrets.ScrubError(err, i.conn.Password)
		}
		_, err = mongoVersion(ctx, i.conn)
		return err
	})
}

// Stop shuts the server down cleanly, kills it if that fails, and removes
// the instance's own directory. The data directory is left to the caller.
func (i *localInstance) Stop(ctx context.Context) error {
	i.once.Do(func() {
		if i.proc != nil {
			i.stopErr = i.shutdown(ctx)
		}
		i.stopErr = errors.Join(i.stopErr, os.RemoveAll(i.dir))
	})
	return i.stopErr
}

func (i *localInstance) shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	switch i.spec.Engine {
	case Postgres:
		// pg_ctl stops the server gently on every OS, Windows included.
		_ = i.env.Runner.Run(ctx, &toolrun.Cmd{Tool: "pg_ctl", Path: i.bin("pg_ctl"), RunID: i.spec.RunID,
			Args: []string{"stop", "-D", i.spec.DataDir, "-m", "fast", "-w", "-t", "50"}})
	case MongoDB:
		if cl, err := mongoClient(i.conn); err == nil {
			_ = cl.Database("admin").RunCommand(ctx, bson.D{{Key: "shutdown", Value: 1}}).Err() // the connection drops
			_ = cl.Disconnect(context.Background())
		}
	}
	select {
	case <-i.proc.Done():
	case <-time.After(10 * time.Second):
	}
	return i.proc.Stop(10 * time.Second)
}

// stopLeftoverLocal stops a local server whose owner is gone, if its lock
// file still names the recorded PID, so an unrelated process that reused
// the PID is never touched.
func stopLeftoverLocal(o owner) {
	if o.ServerPID <= 0 || o.DataDir == "" {
		return
	}
	lock := "postmaster.pid"
	if o.Engine == MongoDB {
		lock = "mongod.lock"
	}
	b, err := os.ReadFile(filepath.Join(o.DataDir, lock)) // #nosec G304 -- recorded by pentym
	if err != nil {
		return
	}
	first, _, _ := bytes.Cut(bytes.TrimSpace(b), []byte("\n"))
	if pid, err := strconv.Atoi(string(bytes.TrimSpace(first))); err != nil || pid != o.ServerPID {
		return
	}
	if p, err := os.FindProcess(o.ServerPID); err == nil {
		_ = p.Kill()
	}
}
