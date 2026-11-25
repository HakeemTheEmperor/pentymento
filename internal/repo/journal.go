// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Journal results.
const (
	ResultOK      = "ok"
	ResultFailed  = "failed"
	ResultSkipped = "skipped"
)

// JournalEntry is one operation in the repository's operations journal
// (PRD 5.4, SDD 5.2): who did what, from where, when, and how it ended.
type JournalEntry struct {
	Who       string `json:"who"`
	Host      string `json:"host"`
	Operation string `json:"operation"`
	Target    string `json:"target,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	// Credential is the reference used (such as "env:PGPASSWORD"), never
	// a secret value.
	Credential string    `json:"credential,omitempty"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Result     string    `json:"result"`
	// Detail explains a failure or skip, scrubbed of secrets.
	Detail string `json:"detail,omitempty"`
}

// Identity is who and where an operation runs, recorded in every journal
// entry, lock and lease.
type Identity struct {
	User string
	Host string
}

// CurrentIdentity returns the operating-system user and host.
func CurrentIdentity() Identity {
	id := Identity{User: "unknown", Host: "unknown"}
	if u, err := user.Current(); err == nil && u.Username != "" {
		id.User = u.Username
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		id.Host = h
	}
	return id
}

// Operation is a journal entry being recorded.
type Operation struct {
	r     *Repository
	entry JournalEntry
}

// BeginOperation starts a journal entry for an operation on a target (empty
// for repository-wide operations). credential is the reference the
// operation uses, if any.
func (r *Repository) BeginOperation(id Identity, operation, target, runID string, credential secrets.Ref) *Operation {
	return &Operation{r: r, entry: JournalEntry{
		Who: id.User, Host: id.Host, Operation: operation, Target: target, RunID: runID,
		Credential: credential.String(), Start: r.now().UTC(),
	}}
}

// Finish records the operation's end. A nil err is a success; detail is
// scrubbed of secrets before it is stored.
func (o *Operation) Finish(ctx context.Context, result string, err error) error {
	o.entry.End = o.r.now().UTC()
	o.entry.Result = result
	if err != nil {
		if result == ResultOK {
			o.entry.Result = ResultFailed
		}
		o.entry.Detail = secrets.Scrub(err.Error())
	}
	return o.r.putMeta(ctx, journalKey(o.entry.Start), o.entry, backend.PutOptions{IfNotExists: true})
}

// Journal returns the entries that started at or after since, oldest
// first; target filters by target ID when not empty.
func (r *Repository) Journal(ctx context.Context, since time.Time, target string) ([]JournalEntry, error) {
	since = since.UTC()
	var out []JournalEntry
	// Entries are grouped by month: list only the months from since on.
	for m := time.Date(since.Year(), since.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(r.now()); m = m.AddDate(0, 1, 0) {
		prefix := fmt.Sprintf("%s%04d/%02d/", journalPrefix, m.Year(), m.Month())
		keys, err := r.listKeys(ctx, prefix)
		if err != nil {
			return nil, err
		}
		from := prefix + sortableTime(since)
		for _, k := range keys {
			if k < from {
				continue
			}
			var e JournalEntry
			if err := r.getMeta(ctx, k, &e); err != nil {
				return nil, err
			}
			if target == "" || e.Target == target {
				out = append(out, e)
			}
		}
	}
	return out, nil
}
