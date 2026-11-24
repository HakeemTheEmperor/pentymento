// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"strings"
	"testing"
)

const (
	author = "Ada Lovelace"
	email  = "ada@example.com"
	signed = "Signed-off-by: Ada Lovelace <ada@example.com>"
)

func dirs(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(p string) bool { return set[p] }
}

func TestCheckMessage(t *testing.T) {
	isDir := dirs("internal/repo", "cmd/pentym", "docs/design")
	tests := []struct {
		name    string
		msg     string
		email   string
		wantErr string // substring of one problem; empty means the message passes
	}{
		{name: "full message", msg: "internal/repo: commit snapshot only after a clean finish\n\nBody explaining why.\n\nFixes #42\n\n" + signed + "\n"},
		{name: "subject and sign-off only", msg: "cmd/pentym: add version flag\n\n" + signed},
		{name: "all prefix", msg: "all: rename the chunker package\n\n" + signed},
		{name: "docs prefix", msg: "docs: fix a typo in the quick start\n\n" + signed},
		{name: "nested real directory", msg: "docs/design: add v1 planning documents\n\nBody.\n\n" + signed},
		{name: "sign-off among other trailers", msg: "cmd/pentym: add flag\n\nBody.\n\nReviewed-by: Someone <s@example.com>\n" + signed},
		{name: "sign-off email case-insensitive", email: "ADA@example.com", msg: "cmd/pentym: add flag\n\n" + signed},
		{name: "CRLF line endings", msg: "cmd/pentym: add flag\r\n\r\nBody.\r\n\r\n" + signed + "\r\n"},
		{name: "summary starting with a digit", msg: "cmd/pentym: 3 new flags\n\n" + signed},
		{name: "exactly 72 characters", msg: "cmd/pentym: " + strings.Repeat("a", 60) + "\n\n" + signed},

		{name: "sign-off as subject", msg: signed + "\n\nall: add repository scaffold\n\nBody.", wantErr: `prefix "Signed-off-by" is not a directory`},
		{name: "sign-off as subject has no trailer", msg: signed + "\n\nall: add repository scaffold\n\nBody.", wantErr: "Signed-off-by"},
		{name: "missing sign-off", msg: "cmd/pentym: add flag\n\nBody.", wantErr: "Signed-off-by"},
		{name: "sign-off for another email", msg: "cmd/pentym: add flag\n\nSigned-off-by: Bob <bob@example.com>", wantErr: "Signed-off-by"},
		{name: "sign-off not in last paragraph", msg: "cmd/pentym: add flag\n\n" + signed + "\n\nMore body.", wantErr: "Signed-off-by"},
		{name: "subject only", msg: "cmd/pentym: add flag", wantErr: "Signed-off-by"},
		{name: "no prefix", msg: "add a flag\n\n" + signed, wantErr: "subject must be"},
		{name: "prefix is not a directory", msg: "internal/nope: add flag\n\n" + signed, wantErr: "not a directory"},
		{name: "prefix with trailing slash", msg: "cmd/pentym/: add flag\n\n" + signed, wantErr: "not a directory"},
		{name: "uppercase summary", msg: "cmd/pentym: Add flag\n\n" + signed, wantErr: "lowercase"},
		{name: "trailing full stop", msg: "cmd/pentym: add flag.\n\n" + signed, wantErr: "full stop"},
		{name: "73 characters", msg: "cmd/pentym: " + strings.Repeat("a", 61) + "\n\n" + signed, wantErr: "limit is 72"},
		{name: "second line not blank", msg: "cmd/pentym: add flag\nBody.\n\n" + signed, wantErr: "second line"},
		{name: "empty summary", msg: "cmd/pentym: \n\n" + signed, wantErr: "non-empty"},
		{name: "no space after colon", msg: "cmd/pentym:add flag\n\n" + signed, wantErr: "subject must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := email
			if tt.email != "" {
				e = tt.email
			}
			problems := CheckMessage(Commit{Message: tt.msg, AuthorName: author, AuthorEmail: e}, isDir)
			if tt.wantErr == "" {
				if len(problems) != 0 {
					t.Fatalf("want no problems, got %q", problems)
				}
				return
			}
			for _, p := range problems {
				if strings.Contains(p, tt.wantErr) {
					return
				}
			}
			t.Fatalf("want a problem containing %q, got %q", tt.wantErr, problems)
		})
	}
}
