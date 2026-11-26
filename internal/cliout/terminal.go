// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Color modes for --color.
const (
	ColorAuto   = "auto"
	ColorAlways = "always"
	ColorNever  = "never"
)

// Settings are the global output flags and environment that decide how a
// Printer behaves.
type Settings struct {
	JSON      bool
	Color     string // ColorAuto, ColorAlways or ColorNever
	ASCII     bool
	Verbosity Verbosity
	// Getenv reads the environment; nil means os.Getenv.
	Getenv func(string) string
}

// NewPrinter returns a Printer for the given streams, detecting terminals,
// colour, Unicode support and the terminal width.
func NewPrinter(stdin io.Reader, stdout, stderr io.Writer, s Settings) (*Printer, error) {
	getenv := s.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	switch s.Color {
	case "", ColorAuto, ColorAlways, ColorNever:
	default:
		return nil, fmt.Errorf("--color must be auto, always or never, not %q", s.Color)
	}
	outTTY, errTTY, inTTY := isTerminal(stdout), isTerminal(stderr), isTerminal(stdin)

	p := &Printer{
		Stdout:      stdout,
		Stderr:      stderr,
		JSON:        s.JSON,
		ColorOut:    !s.JSON && ColorEnabled(s.Color, outTTY, getenv) && enableANSI(stdout),
		ColorErr:    ColorEnabled(s.Color, errTTY, getenv) && enableANSI(stderr),
		Unicode:     !s.ASCII && unicodeSupported(stdout, getenv),
		Verbosity:   s.Verbosity,
		Interactive: inTTY && errTTY,
		ErrTerminal: errTTY,
		OutTerminal: outTTY,
	}
	if outTTY {
		if f, ok := stdout.(*os.File); ok {
			if w, _, err := term.GetSize(int(f.Fd())); err == nil {
				p.Width = w
			}
		}
	}
	p.SetStdin(stdin)
	return p, nil
}

// ColorEnabled decides whether a stream gets colour (Brand 10.2): always
// with --color=always, never with --color=never; otherwise only on a
// terminal, and not when NO_COLOR is set, in CI, or with TERM=dumb.
func ColorEnabled(mode string, tty bool, getenv func(string) string) bool {
	switch mode {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	}
	return tty && getenv("NO_COLOR") == "" && getenv("CI") == "" && getenv("TERM") != "dumb"
}

// isTerminal reports whether v is a terminal.
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// localeIsUTF8 reports whether the POSIX locale environment selects UTF-8.
func localeIsUTF8(getenv func(string) string) bool {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := getenv(name); v != "" {
			v = strings.ToLower(v)
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}
