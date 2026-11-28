// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func TestParseAt(t *testing.T) {
	wat := time.FixedZone("WAT", 3600)
	want := time.Date(2026, 11, 22, 14, 3, 0, 0, wat)
	for _, in := range []string{"2026-11-22T14:03:00+01:00", "2026-11-22 14:03", "2026-11-22 14:03:00", "2026-11-22T14:03", "2026-11-22T13:03:00Z"} {
		got, err := parseAt(in, wat)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseAt(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "yesterday", "2026-11-22", "14:03", "2026-13-01 00:00"} {
		if _, err := parseAt(in, wat); err == nil {
			t.Errorf("parseAt(%q) succeeded", in)
		}
	}
}

func TestRestoreUsage(t *testing.T) {
	c := backupEnv(t, "env:K1")
	for _, tt := range []struct {
		args []string
		code msg.ExitCode
		want string
	}{
		{[]string{"--table", "orders"}, msg.ExitUsage, "needs one restore point"},
		{[]string{"--xid", "1", "--at", "2026-11-22 14:03", "--table", "orders"}, msg.ExitUsage, "needs one restore point"},
		{[]string{"--xid", "1"}, msg.ExitUsage, "name the tables to load into production with --table"},
		{[]string{"--xid", "abc", "--table", "orders"}, msg.ExitUsage, `--xid "abc" is not a transaction ID`},
		{[]string{"--at", "soon", "--table", "orders"}, msg.ExitUsage, "is not a time"},
		{[]string{"--xid", "1", "--mode", "replace"}, msg.ExitUsage, "the only one is rewind"},
		{[]string{"--xid", "1", "--mode", "rewind"}, msg.ExitUsage, "--mode=rewind needs --data-dir"},
		{[]string{"--xid", "1", "--mode", "rewind", "--data-dir", "/x", "--table", "orders"}, msg.ExitUsage, "takes no --table or --in-place"},
		{[]string{"--xid", "1", "--table", "orders", "--data-dir", "/x"}, msg.ExitUsage, "--data-dir is for --mode=rewind"},
		{[]string{"--before-event", "3", "--table", "orders"}, msg.ExitNotImplemented, "restoring to just before an event is not implemented"},
	} {
		r := c.runFake(append([]string{"restore", "prod-pg"}, tt.args...)...)
		if r.code != tt.code || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("restore %v: exit %d\nstderr: %s", tt.args, r.code, r.stderr)
		}
	}
	r := c.runFake("restore", "prod-mg", "--xid", "1", "--table", "orders")
	if r.code != msg.ExitNotImplemented || !strings.Contains(r.stderr, "point-in-time restores of mongodb are not implemented") {
		t.Errorf("mongodb: exit %d\nstderr: %s", r.code, r.stderr)
	}
	r = c.runFake("restore", "nope", "--xid", "1", "--table", "orders")
	if r.code != msg.ExitNotFound {
		t.Errorf("unknown target: exit %d\nstderr: %s", r.code, r.stderr)
	}
}

// TestRestoreNeedsFullSlot: the agent slot cannot read backups.
func TestRestoreNeedsFullSlot(t *testing.T) {
	c := backupEnv(t, "env:KA")
	r := c.runFake("restore", "prod-pg", "--xid", "1", "--table", "orders")
	if r.code != msg.ExitAccess {
		t.Errorf("exit %d\nstderr: %s", r.code, r.stderr)
	}
}
