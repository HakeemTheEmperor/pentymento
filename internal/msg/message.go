// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package msg

import "strings"

// Level is the status a message reports.
type Level int

// Levels, each shown with a word or symbol as well as a colour (Brand 10).
const (
	// LevelOK is a success.
	LevelOK Level = iota
	// LevelWarning needs attention but nothing failed.
	LevelWarning
	// LevelFailed is a failure or a critical condition.
	LevelFailed
)

// String returns the level's name.
func (l Level) String() string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelWarning:
		return "warning"
	case LevelFailed:
		return "failed"
	}
	return "unknown"
}

// Message is a user-facing message in the Brand 11.1 structure: what
// happened, its impact, the underlying cause, and the next step.
//
//	<symbol> <target>: <fact>
//	  <impact>
//	  cause: <cause>
//	→ <next>
//
// Fields must never contain a secret or row data (Brand 11.2).
type Message struct {
	Level Level
	// ID is the message ID for errors and warnings people act on; empty
	// otherwise.
	ID ID
	// Target is the configured target the message is about, if any.
	Target string
	// Fact is what happened, with the specific fact; it starts lowercase
	// and has no trailing full stop.
	Fact string
	// Impact is the impact on data or recovery, when not obvious.
	Impact string
	// Cause is the underlying error, already scrubbed of secrets.
	Cause string
	// Next is the next command or action.
	Next string
}

// Headline returns the first line of the message without its symbol:
// "<target>: <fact>", or just the fact when there is no target.
func (m Message) Headline() string {
	if m.Target == "" {
		return m.Fact
	}
	return m.Target + ": " + m.Fact
}

// Error is an error that carries a user-facing message and the exit code
// the process ends with.
type Error struct {
	Code    ExitCode
	Message Message
	// Err is the underlying error, for errors.Is and errors.As. It is not
	// shown to users; Message.Cause carries its scrubbed text.
	Err error
}

// Error returns the message headline, plus the cause when there is one.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Message.Headline())
	if e.Message.Cause != "" {
		b.WriteString(": ")
		b.WriteString(e.Message.Cause)
	}
	return b.String()
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }
