// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// CmdTimeout bounds how long a cmd: helper may run.
const CmdTimeout = 30 * time.Second

// maxCmdStderr is how much of a failing helper's stderr is shown.
const maxCmdStderr = 200

// Message IDs for secret resolution (see docs/errors).
const (
	IDPlaintextRejected msg.ID = "PTM-1001"
	IDEnvUnset          msg.ID = "PTM-1004"
	IDFileUnreadable    msg.ID = "PTM-1005"
	IDKeychainMissing   msg.ID = "PTM-1006"
	IDCommandFailed     msg.ID = "PTM-1007"
)

// Resolver resolves secret references at the moment of use. The zero value
// uses the process environment, the filesystem, the OS keychain and real
// helper commands; tests replace them.
type Resolver struct {
	// Getenv reads an environment variable; nil means os.Getenv.
	Getenv func(string) string
	// ReadFile reads a file; nil means os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// KeychainGet reads an item from the OS keychain; nil means the
	// platform keychain (macOS Keychain, Windows Credential Manager, Linux
	// Secret Service).
	KeychainGet func(service, item string) (string, error)
	// RunCommand runs a helper and returns its stdout and stderr; nil runs
	// it as a child process with CmdTimeout.
	RunCommand func(ctx context.Context, argv []string) (stdout, stderr []byte, err error)
	// Timeout bounds a cmd: helper; zero means CmdTimeout.
	Timeout time.Duration
}

// Resolve returns the secret a reference points to. Failures are *msg.Error
// values with exit code ExitConfig whose text never contains the secret.
// target names the configured target, if any, for the message.
func (r Resolver) Resolve(ctx context.Context, target string, ref Ref) (Secret, error) {
	switch ref.scheme {
	case SchemePlaintext:
		return New(ref.plain.Expose()), nil
	case SchemeEnv:
		return r.resolveEnv(target, ref)
	case SchemeFile:
		return r.resolveFile(target, ref)
	case SchemeKeychain:
		return r.resolveKeychain(target, ref)
	case SchemeCmd:
		return r.resolveCmd(ctx, target, ref)
	}
	return Secret{}, fail(target, "", "no secret reference configured", "", "add a reference such as env:NAME to the config")
}

func (r Resolver) resolveEnv(target string, ref Ref) (Secret, error) {
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	v := getenv(ref.arg)
	if v == "" {
		return Secret{}, fail(target, IDEnvUnset,
			fmt.Sprintf("secret reference %s: environment variable %s is not set", ref, ref.arg), "",
			fmt.Sprintf("set %s in pentym's environment, or change the reference", ref.arg))
	}
	return New([]byte(v)), nil
}

func (r Resolver) resolveFile(target string, ref Ref) (Secret, error) {
	read := r.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	b, err := read(ref.arg)
	next := fmt.Sprintf("check that %s exists and is readable by pentym, or change the reference", ref.arg)
	if err != nil {
		cause := "cannot read the file"
		switch {
		case errors.Is(err, fs.ErrNotExist):
			cause = "the file does not exist"
		case errors.Is(err, fs.ErrPermission):
			cause = "permission denied"
		}
		return Secret{}, fail(target, IDFileUnreadable, fmt.Sprintf("secret reference %s cannot be read", ref), cause, next)
	}
	b = trimNewline(b)
	if len(b) == 0 {
		return Secret{}, fail(target, IDFileUnreadable, fmt.Sprintf("secret reference %s: the file is empty", ref), "", next)
	}
	s := New(b)
	clear(b)
	return s, nil
}

func (r Resolver) resolveKeychain(target string, ref Ref) (Secret, error) {
	get := r.KeychainGet
	if get == nil {
		get = keyring.Get
	}
	service, item, _ := strings.Cut(ref.arg, "/")
	v, err := get(service, item)
	if err != nil || v == "" {
		cause := "the keychain could not be read"
		if errors.Is(err, keyring.ErrNotFound) || (err == nil && v == "") {
			cause = "no such item"
		}
		return Secret{}, fail(target, IDKeychainMissing, fmt.Sprintf("secret reference %s cannot be read from the keychain", ref), cause,
			fmt.Sprintf("store the secret as service %q, item %q in the OS keychain of the user pentym runs as", service, item))
	}
	return New([]byte(v)), nil
}

func (r Resolver) resolveCmd(ctx context.Context, target string, ref Ref) (Secret, error) {
	argv, err := SplitCommand(ref.arg)
	next := "run the helper by hand as the user pentym runs as and check its output"
	if err != nil {
		return Secret{}, fail(target, IDCommandFailed, fmt.Sprintf("secret reference %s cannot be parsed", ref), err.Error(), next)
	}
	run := r.RunCommand
	if run == nil {
		run = runCommand
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = CmdTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, err := run(ctx, argv)
	out := trimNewline(stdout)
	if err != nil {
		secret := New(out)
		defer secret.Clear()
		cause := err.Error()
		if ctx.Err() != nil {
			cause = fmt.Sprintf("the helper did not finish within %s", timeout)
		} else if s := strings.TrimSpace(string(stderr)); s != "" {
			cause += ": " + truncate(s, maxCmdStderr)
		}
		clear(stdout)
		return Secret{}, fail(target, IDCommandFailed, fmt.Sprintf("secret reference %s failed", ref), Scrub(cause, secret), next)
	}
	if len(out) == 0 {
		return Secret{}, fail(target, IDCommandFailed, fmt.Sprintf("secret reference %s printed nothing", ref), "", next)
	}
	s := New(out)
	clear(stdout)
	return s, nil
}

// runCommand runs a helper as a child process. Its stdin is empty and it
// inherits pentym's environment.
func runCommand(ctx context.Context, argv []string) ([]byte, []byte, error) {
	// #nosec G204 G702 -- the helper command is configured by the operator
	// as a cmd: secret reference; it is run directly, without a shell.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// trimNewline removes one trailing "\n" or "\r\n".
func trimNewline(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fail(target string, id msg.ID, fact, cause, next string) error {
	return &msg.Error{
		Code: msg.ExitConfig,
		Message: msg.Message{
			Level: msg.LevelFailed, ID: id, Target: target,
			Fact: fact, Cause: cause, Next: next,
		},
	}
}
