// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package toolrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Defaults for stopping a child.
const (
	// DefaultGrace is how long a cancelled child gets to exit after being
	// asked to stop, before it is killed.
	DefaultGrace = 5 * time.Second
	// stderrTail is how many of the last stderr lines an error keeps.
	stderrTail = 20
	// maxStderrLine bounds one logged stderr line.
	maxStderrLine = 4096
)

// Errors, matched with errors.Is.
var (
	// ErrSecretInArgs means a secret's value appears in the command line.
	ErrSecretInArgs = errors.New("a secret would appear on the command line")
	// ErrSecretInEnv means a secret's value appears in the environment
	// without AllowEnvSecret.
	ErrSecretInEnv = errors.New("a secret would appear in the child's environment")
	// ErrSourceCompression means an argument asks the tool to compress its
	// output, which defeats deduplication (PRD 7.2).
	ErrSourceCompression = errors.New("source compression is not allowed")
)

// Runner runs external tools. The zero value is not usable: set
// SecretDir. A Runner is safe for concurrent use.
type Runner struct {
	// SecretDir holds temporary credential files (see WriteSecretFile).
	SecretDir string
	// Logger receives the children's stderr, one record per line, tagged
	// with the tool and the run ID. Nil discards it.
	Logger *slog.Logger
	// Grace is how long a cancelled child may take to exit before it is
	// killed; zero means DefaultGrace.
	Grace time.Duration
	// Environ is the base environment for children; nil means os.Environ.
	Environ func() []string
}

// Cmd is one tool invocation. pentym builds Args itself; Extra holds
// arguments from the configuration, which are checked for compression
// flags like Args.
type Cmd struct {
	// Tool names the tool in messages and logs, such as "pg_dump".
	Tool string
	// Path is the executable.
	Path  string
	Args  []string
	Extra []string
	// Env is added to the base environment.
	Env []string
	// Secrets are values that must appear in no argument and, unless
	// AllowEnvSecret is set, in no environment variable; they are also
	// scrubbed from logged stderr.
	Secrets []secrets.Secret
	// AllowEnvSecret permits a secret in Env, for the PGPASSWORD fallback
	// when a secret file cannot be used (PRD 7.2).
	AllowEnvSecret bool
	// Files are secret files to delete when the child exits, whatever the
	// outcome.
	Files []*SecretFile
	// Dir is the working directory; empty means pentym's.
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	// RunID tags the logged stderr lines (SDD 9.x run_id).
	RunID string
}

// ExitError is a tool that exited unsuccessfully.
type ExitError struct {
	Tool string
	Code int
	// Stderr holds the last lines the tool wrote to stderr, with secrets
	// scrubbed.
	Stderr []string
}

func (e *ExitError) Error() string {
	s := fmt.Sprintf("%s exited with code %d", e.Tool, e.Code)
	if len(e.Stderr) > 0 {
		s += ": " + e.Stderr[len(e.Stderr)-1]
	}
	return s
}

// compressionFlag is an option that can make a tool compress its output.
type compressionFlag struct {
	name string
	// value: the option takes a level or method, attached ("-Z9",
	// "--compress=gzip") or as the next argument; "0" and "none" turn
	// compression off and are allowed. Without a value the flag always
	// compresses.
	value bool
}

// compressionFlags are each tool's options that make its output
// compressed. pentym builds commands without them (or turns compression
// off explicitly); arguments from the configuration are refused if they
// turn it on.
var compressionFlags = map[string][]compressionFlag{
	"pg_basebackup": {{"-z", false}, {"--gzip", false}, {"-Z", true}, {"--compress", true}},
	"pg_dump":       {{"-Z", true}, {"--compress", true}},
	"pg_dumpall":    {{"-Z", true}, {"--compress", true}},
	"mongodump":     {{"--gzip", false}},
}

// CheckCompression refuses arguments that make tool compress its output.
func CheckCompression(tool string, args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		for _, f := range compressionFlags[tool] {
			long := strings.HasPrefix(f.name, "--")
			var value string
			var matched, hasValue bool
			switch {
			case a == f.name:
				matched = true
				if f.value && i+1 < len(args) {
					value, hasValue = args[i+1], true
				}
			case long && strings.HasPrefix(a, f.name+"="):
				matched, value, hasValue = true, a[len(f.name)+1:], true
			case !long && strings.HasPrefix(a, f.name):
				matched, value, hasValue = true, a[len(f.name):], true
			}
			if !matched {
				continue
			}
			if f.value && hasValue && (value == "0" || value == "none") {
				continue // compression explicitly off
			}
			return fmt.Errorf("%w: %s %s; pentym compresses after chunking, and compressed input defeats deduplication", ErrSourceCompression, tool, a)
		}
	}
	return nil
}

// check refuses a command that would leak a secret or compress its output.
func (c *Cmd) check() error {
	args := append(append([]string{}, c.Args...), c.Extra...)
	if err := CheckCompression(c.Tool, args); err != nil {
		return err
	}
	for _, s := range c.Secrets {
		v := s.Expose()
		if len(v) == 0 {
			continue
		}
		for _, a := range append([]string{c.Path}, args...) {
			if bytes.Contains([]byte(a), v) {
				return fmt.Errorf("%w (%s)", ErrSecretInArgs, c.Tool)
			}
		}
		if !c.AllowEnvSecret {
			for _, e := range c.Env {
				if bytes.Contains([]byte(e), v) {
					return fmt.Errorf("%w (%s)", ErrSecretInEnv, c.Tool)
				}
			}
		}
	}
	return nil
}

// Run starts the tool, streams its stdout to c.Stdout, logs its stderr,
// and waits for it. Cancelling ctx stops the tool and everything it
// started: they are asked to exit, then killed after the grace period.
// Secret files in c.Files are deleted whatever happens.
func (r *Runner) Run(ctx context.Context, c *Cmd) error {
	p, err := r.Start(c)
	if err != nil {
		return err
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		_ = p.Stop(r.grace())
		return fmt.Errorf("%s: %w", c.Tool, ctx.Err())
	}
	return p.Wait()
}

func (r *Runner) grace() time.Duration {
	if r.Grace > 0 {
		return r.Grace
	}
	return DefaultGrace
}

// Process is a tool running in the background, such as a side instance's
// server. Stop it with Stop; its secret files are deleted when it exits.
type Process struct {
	tool   string
	cmd    *exec.Cmd
	g      *group
	stderr *lineLogger
	files  []*SecretFile

	done    chan struct{}
	waitErr error

	mu      sync.Mutex
	stopped bool
	closed  bool // the group is closed; signalling it is no longer safe
}

// Start starts the tool and returns at once. The tool runs until it exits
// or is stopped with Stop; it is not tied to a context, so a server can
// outlive the call that started it.
func (r *Runner) Start(c *Cmd) (_ *Process, err error) {
	removeFiles := func() error {
		var errs error
		for _, f := range c.Files {
			errs = errors.Join(errs, f.Remove())
		}
		return errs
	}
	if err := c.check(); err != nil {
		return nil, errors.Join(err, removeFiles())
	}
	g, err := newGroup()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%s: %w", c.Tool, err), removeFiles())
	}
	cmd := exec.Command(c.Path, append(append([]string{}, c.Args...), c.Extra...)...) // #nosec G204 -- pentym builds the command; secrets and compression flags are refused above
	g.prepare(cmd)
	environ := os.Environ
	if r.Environ != nil {
		environ = r.Environ
	}
	cmd.Env = append(environ(), c.Env...)
	cmd.Dir = c.Dir
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}
	stderr := &lineLogger{logger: r.Logger, tool: c.Tool, runID: c.RunID, secrets: c.Secrets}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		g.close()
		return nil, errors.Join(fmt.Errorf("start %s: %w", c.Tool, err), removeFiles())
	}
	if err := g.started(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		g.close()
		return nil, errors.Join(fmt.Errorf("%s: contain the process: %w", c.Tool, err), removeFiles())
	}
	p := &Process{tool: c.Tool, cmd: cmd, g: g, stderr: stderr, files: c.Files, done: make(chan struct{})}
	go func() {
		werr := cmd.Wait()
		stderr.flush()
		p.mu.Lock()
		g.close() // stop anything the tool left running
		p.closed = true
		p.mu.Unlock()
		p.waitErr = errors.Join(werr, removeFiles())
		close(p.done)
	}()
	return p, nil
}

// Pid returns the tool's process ID.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Done is closed when the tool has exited and its leftovers are stopped.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait waits for the tool to exit. It returns nil for a clean exit, an
// *ExitError for a non-zero exit code, and nil after Stop.
func (p *Process) Wait() error {
	<-p.done
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	var ee *exec.ExitError
	switch {
	case stopped:
		var other error
		if !errors.As(p.waitErr, &ee) {
			other = p.waitErr
		}
		return other
	case errors.As(p.waitErr, &ee):
		return &ExitError{Tool: p.tool, Code: ee.ExitCode(), Stderr: p.stderr.tail()}
	case p.waitErr != nil:
		return fmt.Errorf("%s: %w", p.tool, p.waitErr)
	}
	return nil
}

// Stop asks the tool and everything it started to exit, kills them after
// grace, and waits for them.
func (p *Process) Stop(grace time.Duration) error {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	select {
	case <-p.done:
		return p.Wait()
	default:
	}
	p.signal((*group).terminate)
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
		p.signal((*group).kill)
		<-p.done
	}
	return p.Wait()
}

// signal sends a stop to the group unless it is already closed.
func (p *Process) signal(fn func(*group) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		_ = fn(p.g)
	}
}

// lineLogger logs a child's stderr line by line, scrubbed of secrets, and
// keeps the last lines for the error message.
type lineLogger struct {
	logger  *slog.Logger
	tool    string
	runID   string
	secrets []secrets.Secret

	mu      sync.Mutex
	partial []byte
	last    []string
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			room := maxStderrLine - len(l.partial)
			if room > 0 {
				l.partial = append(l.partial, p[:min(room, len(p))]...)
			}
			break
		}
		room := maxStderrLine - len(l.partial)
		if room > 0 {
			l.partial = append(l.partial, p[:min(room, i)]...)
		}
		l.emit()
		p = p[i+1:]
	}
	return n, nil
}

func (l *lineLogger) emit() {
	line := strings.TrimRight(string(l.partial), "\r")
	l.partial = l.partial[:0]
	if line == "" {
		return
	}
	line = secrets.Scrub(line, l.secrets...)
	if l.logger != nil {
		l.logger.Warn("tool stderr", slog.String("tool", l.tool), slog.String("run_id", l.runID), slog.String("line", line))
	}
	l.last = append(l.last, line)
	if len(l.last) > stderrTail {
		l.last = l.last[1:]
	}
}

func (l *lineLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		l.emit()
	}
}

func (l *lineLogger) tail() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.last...)
}
