// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// Verbosity controls how much human output is shown (Brand 10.6). It never
// changes the format of stdout.
type Verbosity int

// Verbosity levels.
const (
	// Quiet prints only results and errors.
	Quiet Verbosity = iota - 1
	// Normal prints results, messages and the next step.
	Normal
	// Verbose adds detail such as message IDs.
	Verbose
)

// ErrNotInteractive is returned by the confirmation prompts when there is no
// terminal to ask on.
var ErrNotInteractive = errors.New("cannot ask for confirmation: not running in a terminal")

// Printer writes the CLI's output. Results go to Stdout; messages and
// prompts go to Stderr.
type Printer struct {
	Stdout io.Writer
	Stderr io.Writer
	// JSON selects --json output on stdout.
	JSON bool
	// ColorOut and ColorErr enable ANSI styles on each stream.
	ColorOut, ColorErr bool
	// Unicode selects Unicode symbols instead of the ASCII fallback.
	Unicode bool
	// Verbosity is the -q / -v setting.
	Verbosity Verbosity
	// Width is the terminal width for tables, or 0 for no limit.
	Width int
	// Interactive reports whether prompts can be shown: stdin and stderr
	// are both terminals.
	Interactive bool
	// Location and Now give the local time zone and clock for human times.
	Location *time.Location
	Now      func() time.Time

	stdin *bufio.Reader
}

// SetStdin sets where prompt answers are read from.
func (p *Printer) SetStdin(r io.Reader) { p.stdin = bufio.NewReader(r) }

// Table writes a table of results to stdout.
func (p *Printer) Table(t Table) error {
	return RenderTable(p.Stdout, t, p.Width, p.ColorOut)
}

// Status returns a table cell holding a status word with its symbol and
// colour, such as "✓ capturing (4s)".
func (p *Printer) Status(l msg.Level, text string) Cell {
	return Cell{Text: LevelSymbol(l).Text(p.Unicode) + " " + text, Style: LevelStyle(l)}
}

// WriteJSON writes v to stdout as one indented JSON document.
func (p *Printer) WriteJSON(v any) error {
	enc := json.NewEncoder(p.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Time formats t for human output in the printer's location.
func (p *Printer) Time(t time.Time) string {
	return Time(t, p.now(), p.location())
}

func (p *Printer) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Printer) location() *time.Location {
	if p.Location != nil {
		return p.Location
	}
	return time.Local
}

// Message writes m to stderr in the Brand 11.1 structure. In quiet mode
// only failures are written.
func (p *Printer) Message(m msg.Message) {
	if p.Verbosity == Quiet && m.Level != msg.LevelFailed {
		return
	}
	_, _ = io.WriteString(p.Stderr, FormatMessage(m, p.Unicode, p.ColorErr, p.Verbosity >= Verbose))
}

// FormatMessage renders m in the Brand 11.1 structure:
//
//	<symbol> <target>: <fact>   (PTM-NNNN, with showID)
//	  <impact>
//	  cause: <cause>
//	→ <next>
func FormatMessage(m msg.Message, unicode, color, showID bool) string {
	var b strings.Builder
	b.WriteString(Paint(LevelSymbol(m.Level).Text(unicode), LevelStyle(m.Level), color))
	b.WriteString(" ")
	if m.Target != "" {
		b.WriteString(Paint(m.Target, Bold, color))
		b.WriteString(": ")
	}
	b.WriteString(m.Fact)
	if showID && m.ID != "" {
		b.WriteString(" ")
		b.WriteString(Paint("("+string(m.ID)+")", Dim, color))
	}
	b.WriteString("\n")
	if m.Impact != "" {
		b.WriteString("  " + m.Impact + "\n")
	}
	if m.Cause != "" {
		b.WriteString("  cause: " + m.Cause + "\n")
	}
	if m.Next != "" {
		b.WriteString(SymNext.Text(unicode) + " " + Paint(m.Next, Bold, color) + "\n")
	}
	return b.String()
}

// ErrorDoc is the --json document written to stdout when a command fails.
type ErrorDoc struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the body of an ErrorDoc.
type ErrorBody struct {
	ID      msg.ID `json:"id,omitempty"`
	Target  string `json:"target,omitempty"`
	Message string `json:"message"`
	Impact  string `json:"impact,omitempty"`
	Cause   string `json:"cause,omitempty"`
	Next    string `json:"next,omitempty"`
}

// Fail reports a failed command: the error document on stdout with --json,
// otherwise the message on stderr.
func (p *Printer) Fail(m msg.Message) {
	if p.JSON {
		_ = p.WriteJSON(ErrorDoc{Error: ErrorBody{
			ID: m.ID, Target: m.Target, Message: m.Fact,
			Impact: m.Impact, Cause: m.Cause, Next: m.Next,
		}})
		return
	}
	p.Message(m)
}

// Confirm asks a yes/no question on stderr; the default is No. It returns
// ErrNotInteractive when there is no terminal.
//
//	continue? [y/N]
func (p *Printer) Confirm(question string) (bool, error) {
	if !p.Interactive {
		return false, ErrNotInteractive
	}
	_, _ = fmt.Fprintf(p.Stderr, "%s [y/N] ", question)
	answer, err := p.readLine()
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes", nil
}

// ConfirmTyped asks the user to type want exactly, for the most destructive
// operations (Brand 11.5). It returns ErrNotInteractive when there is no
// terminal.
func (p *Printer) ConfirmTyped(want string) (bool, error) {
	if !p.Interactive {
		return false, ErrNotInteractive
	}
	_, _ = fmt.Fprintf(p.Stderr, "type %s to continue: ", Paint(want, Bold, p.ColorErr))
	answer, err := p.readLine()
	if err != nil {
		return false, err
	}
	return answer == want, nil
}

// readLine reads one answer; end of input counts as an empty answer.
func (p *Printer) readLine() (string, error) {
	if p.stdin == nil {
		return "", nil
	}
	line, err := p.stdin.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
