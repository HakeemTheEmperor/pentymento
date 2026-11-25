// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Command pentym backs up and restores self-hosted Postgres and MongoDB, with
// point-in-time recovery. It runs one-shot commands and the long-running
// agent from one binary.
package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(int(code))
}

// run executes pentym with the given arguments and returns the exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) msg.ExitCode {
	return newApp(stdin, stdout, stderr, getenv).execute(ctx, args)
}

// execute runs the command line args and returns the exit code.
func (a *app) execute(ctx context.Context, args []string) msg.ExitCode {
	root := a.rootCommand()
	if args == nil {
		args = []string{} // never let cobra fall back to os.Args
	}
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return msg.ExitOK
	}

	code, m := describe(ctx, err)
	p := a.printer
	if p == nil {
		// Flags failed before the printer was built: use plain output that
		// honours whatever global flags were parsed.
		p = a.fallbackPrinter()
	}
	p.Fail(m)
	return code
}

// describe maps an error to its exit code and user-facing message.
func describe(ctx context.Context, err error) (msg.ExitCode, msg.Message) {
	var me *msg.Error
	var ue *usageError
	switch {
	case errors.As(err, &me):
		return me.Code, me.Message
	case errors.As(err, &ue):
		return msg.ExitUsage, ue.message()
	case strings.HasPrefix(err.Error(), "unknown command"):
		return msg.ExitUsage, msg.Message{Level: msg.LevelFailed, Fact: err.Error(), Next: "pentym --help"}
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return msg.ExitInterrupted, msg.Message{Level: msg.LevelFailed, Fact: "interrupted"}
	default:
		return msg.ExitFailure, msg.Message{Level: msg.LevelFailed, Fact: err.Error()}
	}
}

// fallbackPrinter builds a printer from the global flags parsed so far,
// ignoring flag values that are themselves invalid.
func (a *app) fallbackPrinter() *cliout.Printer {
	s := a.settings()
	s.Color = cliout.ColorAuto
	if a.opts.color == cliout.ColorAlways || a.opts.color == cliout.ColorNever {
		s.Color = a.opts.color
	}
	p, err := cliout.NewPrinter(a.stdin, a.stdout, a.stderr, s)
	if err != nil {
		return &cliout.Printer{Stdout: a.stdout, Stderr: a.stderr, JSON: a.opts.json}
	}
	return p
}
