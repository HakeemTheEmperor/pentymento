// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// Log formats for --log-format.
const (
	logFormatText = "text"
	logFormatJSON = "json"
)

// options holds the global flags.
type options struct {
	json      bool
	color     string
	ascii     bool
	quiet     bool
	verbose   bool
	debug     bool
	logFormat string
	version   bool
}

// app holds the process streams, the global flags and, once the flags are
// validated, the printer that every command writes through.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
	opts           options
	printer        *cliout.Printer
}

func newApp(stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) *app {
	if getenv == nil {
		getenv = os.Getenv
	}
	return &app{stdin: stdin, stdout: stdout, stderr: stderr, getenv: getenv}
}

func (a *app) settings() cliout.Settings {
	v := cliout.Normal
	switch {
	case a.opts.quiet:
		v = cliout.Quiet
	case a.opts.verbose:
		v = cliout.Verbose
	}
	return cliout.Settings{
		JSON: a.opts.json, Color: a.opts.color, ASCII: a.opts.ascii,
		Verbosity: v, Getenv: a.getenv,
	}
}

// setup validates the global flags and builds the printer. It runs before
// every command.
func (a *app) setup(cmd *cobra.Command) error {
	if a.opts.quiet && a.opts.verbose {
		return newUsageError(cmd, "-q and -v cannot be used together")
	}
	switch a.opts.logFormat {
	case "", logFormatText, logFormatJSON:
	default:
		return newUsageError(cmd, fmt.Sprintf("--log-format must be text or json, not %q", a.opts.logFormat))
	}
	p, err := cliout.NewPrinter(a.stdin, a.stdout, a.stderr, a.settings())
	if err != nil {
		return newUsageError(cmd, err.Error())
	}
	a.printer = p
	return nil
}

// usageError is a wrong command, flag or argument (exit code 2).
type usageError struct {
	text string
	cmd  string // command path, for the --help hint
}

func newUsageError(cmd *cobra.Command, text string) *usageError {
	return &usageError{text: text, cmd: cmd.CommandPath()}
}

func (e *usageError) Error() string { return e.text }

func (e *usageError) message() msg.Message {
	return msg.Message{Level: msg.LevelFailed, Fact: e.text, Next: e.cmd + " --help"}
}

// args requires exactly the named positional arguments, such as "<target>".
func args(names ...string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if len(a) == len(names) {
			return nil
		}
		if len(a) < len(names) {
			return newUsageError(cmd, fmt.Sprintf("%s needs %s", cmd.CommandPath(), strings.Join(names[len(a):], " ")))
		}
		return newUsageError(cmd, fmt.Sprintf("%s takes %s, got %d arguments", cmd.CommandPath(), describeArgs(names), len(a)))
	}
}

// optionalArg allows zero or one positional argument.
func optionalArg(name string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if len(a) <= 1 {
			return nil
		}
		return newUsageError(cmd, fmt.Sprintf("%s takes at most one argument, %s; got %d", cmd.CommandPath(), name, len(a)))
	}
}

func describeArgs(names []string) string {
	switch len(names) {
	case 0:
		return "no arguments"
	case 1:
		return "one argument, " + names[0]
	}
	return fmt.Sprintf("%d arguments, %s", len(names), strings.Join(names, " "))
}

// notImplemented is the result of every command that is not built yet.
func notImplemented(cmd *cobra.Command, _ []string) error {
	return &msg.Error{
		Code: msg.ExitNotImplemented,
		Message: msg.Message{
			Level: msg.LevelFailed,
			Fact:  cmd.CommandPath() + " is not implemented in this build",
			Next:  "pentym --help",
		},
	}
}
