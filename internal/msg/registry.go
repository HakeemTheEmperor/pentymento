// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package msg

// ID is a permanent message ID of the form PTM-NNNN (Brand 11.3).
//
// IDs are grouped in blocks by area:
//
//	1xxx  configuration and secrets
//	2xxx  database connection and privileges
//	3xxx  storage and repository
//	4xxx  keys
//	5xxx  capture and gaps
//	6xxx  restore
//	7xxx  verify and check
//	8xxx  agent and lease
//	9xxx  notifications
type ID string

// DocsBaseURL is where each message's troubleshooting entry is published.
// The source of the entry for PTM-NNNN is docs/errors/PTM-NNNN.md.
const DocsBaseURL = "https://pentymento.dev/errors/"

// Entry is one message in the registry.
type Entry struct {
	// ID is the permanent message ID.
	ID ID
	// Name is a short kebab-case name, such as "connection-refused". It
	// fixes the meaning of the ID and never changes.
	Name string
	// Retired marks an ID that is no longer used. Retired IDs stay in the
	// registry so they are never reused.
	Retired bool
}

// DocsURL returns the address of the entry's troubleshooting page.
func (e Entry) DocsURL() string { return DocsBaseURL + string(e.ID) }

// entries is the message registry. It is append-only: entries are never
// removed, renumbered or renamed; an ID that is no longer used is marked
// Retired. Each entry is written as a literal with string-literal ID and
// Name fields, because internal/devtools/msgcheck reads this file's source
// to compare it with earlier versions. Every ID needs a troubleshooting
// entry at docs/errors/<ID>.md.
var entries = []Entry{
	{ID: "PTM-1001", Name: "plaintext-secret-rejected"},
	{ID: "PTM-1002", Name: "plaintext-secret-file-readable"},
	{ID: "PTM-1003", Name: "config-invalid"},
	{ID: "PTM-1004", Name: "secret-env-unset"},
	{ID: "PTM-1005", Name: "secret-file-unreadable"},
	{ID: "PTM-1006", Name: "secret-keychain-missing"},
	{ID: "PTM-1007", Name: "secret-command-failed"},
	{ID: "PTM-1008", Name: "config-not-found"},
	{ID: "PTM-1009", Name: "repository-not-configured"},
	{ID: "PTM-3001", Name: "repository-format-newer"},
	{ID: "PTM-3002", Name: "repository-not-found"},
	{ID: "PTM-3003", Name: "repository-exists"},
	{ID: "PTM-3004", Name: "repository-locked"},
	{ID: "PTM-3005", Name: "conditional-create-unsupported"},
	{ID: "PTM-4001", Name: "key-slot-unlock-failed"},
	{ID: "PTM-4002", Name: "last-full-slot"},
	{ID: "PTM-4003", Name: "insecure-test-mode-remote"},
	{ID: "PTM-4004", Name: "no-unlock-secret"},
	{ID: "PTM-4005", Name: "too-few-key-slots"},
}

// Lookup returns the registry entry for an ID.
func Lookup(id ID) (Entry, bool) {
	for _, e := range entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Entries returns a copy of the registry.
func Entries() []Entry {
	return append([]Entry(nil), entries...)
}
