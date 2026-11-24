// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package msg

// ExitCode is a process exit status. The values are a stable public
// interface: new codes may be added, but existing ones are never renumbered
// or given a new meaning. The table is published in
// docs/reference/exit-codes.md.
type ExitCode int

// Exit codes.
const (
	// ExitOK means the command succeeded.
	ExitOK ExitCode = 0
	// ExitFailure means the operation failed.
	ExitFailure ExitCode = 1
	// ExitUsage means a command, flag or argument was wrong.
	ExitUsage ExitCode = 2
	// ExitConfig means the configuration is invalid.
	ExitConfig ExitCode = 3
	// ExitNotFound means a target, snapshot or event does not exist.
	ExitNotFound ExitCode = 4
	// ExitConnect means a database or storage connection failed.
	ExitConnect ExitCode = 5
	// ExitAccess means a key or permission problem: no key slot unlocks, or
	// a privilege is missing.
	ExitAccess ExitCode = 6
	// ExitIntegrity means check or verify found a problem.
	ExitIntegrity ExitCode = 7
	// ExitUnrecoverable means recovery is not possible as asked, because of
	// a gap or a point outside the recovery window.
	ExitUnrecoverable ExitCode = 8
	// ExitDeclined means the user declined a confirmation prompt.
	ExitDeclined ExitCode = 9
	// ExitNotImplemented means the command is not built yet.
	ExitNotImplemented ExitCode = 10
	// ExitInterrupted means the command was interrupted (Ctrl-C).
	ExitInterrupted ExitCode = 130
)

// ExitCodes lists every exit code with a short description, in order.
var ExitCodes = []struct {
	Code    ExitCode
	Meaning string
}{
	{ExitOK, "success"},
	{ExitFailure, "the operation failed"},
	{ExitUsage, "usage error: bad command, flag or argument"},
	{ExitConfig, "configuration error"},
	{ExitNotFound, "not found: target, snapshot or event"},
	{ExitConnect, "cannot connect to the database or storage"},
	{ExitAccess, "key or permission problem"},
	{ExitIntegrity, "integrity or verification failure"},
	{ExitUnrecoverable, "recovery not possible as asked: a gap, or a point outside the recovery window"},
	{ExitDeclined, "declined at a confirmation prompt"},
	{ExitNotImplemented, "not implemented"},
	{ExitInterrupted, "interrupted"},
}
