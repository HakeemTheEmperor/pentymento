// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
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
	config    string
}

// app holds the process streams, the global flags and, once the flags are
// validated, the printer that every command writes through.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
	opts           options
	printer        *cliout.Printer
	// interactive, set by tests, makes prompts work without a terminal.
	interactive bool
	// upgradePlan, set by tests, replaces the repository's upgrade plan.
	upgradePlan func(*repo.Repository) ([]repo.Migration, error)
	// engineFor, set by tests, replaces the engines.
	engineFor engineFactory
	// tel is the logging and metrics facade for one-shot commands: logs go
	// to stderr, and only with --debug.
	tel *telemetry.Telemetry
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
	if a.interactive {
		p.Interactive = true
	}

	logger := telemetry.Discard()
	if a.opts.debug {
		logger, err = telemetry.NewLogger(a.stderr, a.opts.logFormat, p.ErrTerminal, slog.LevelDebug)
		if err != nil {
			return newUsageError(cmd, err.Error())
		}
	}
	a.tel = telemetry.New(logger, nil)
	return nil
}

// progress returns a progress reporter for label: a redrawn stderr line on
// a terminal, JSON events on stderr with --json, an info log line on stderr
// every 30 s otherwise, and nothing with -q.
func (a *app) progress(label string, total int64) (*telemetry.Progress, error) {
	o := telemetry.ProgressOptions{W: a.stderr, Label: label, Total: total, Unicode: a.printer.Unicode}
	switch {
	case a.opts.quiet:
		o.Mode = telemetry.ProgressOff
	case a.opts.json:
		o.Mode = telemetry.ProgressJSON
	case a.printer.ErrTerminal:
		o.Mode = telemetry.ProgressTerminal
	default:
		o.Mode = telemetry.ProgressLog
		logger, err := telemetry.NewLogger(a.stderr, a.opts.logFormat, false, slog.LevelInfo)
		if err != nil {
			return nil, err
		}
		o.Logger = logger
	}
	return telemetry.NewProgress(o), nil
}

// configPath is the config file to use: --config, else $PENTYM_CONFIG,
// else the system location.
func (a *app) configPath() string {
	return config.Path(a.opts.config, a.getenv)
}

// loadConfig loads the config file with environment overrides from environ
// (as from os.Environ).
func (a *app) loadConfig(environ []string) (*config.Config, error) {
	return config.Load(a.configPath(), environ)
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
