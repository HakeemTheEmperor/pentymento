// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func TestWALFileName(t *testing.T) {
	const mb16 = 16 << 20
	for _, tt := range []struct {
		tl      uint32
		lsn     string
		segSize uint64
		want    string
	}{
		{1, "0/0", mb16, "000000010000000000000000"},
		{1, "0/3000060", mb16, "000000010000000000000003"},
		{1, "0/FFFFFFFF", mb16, "0000000100000000000000FF"},
		{2, "1/0", mb16, "000000020000000100000000"},
		{1, "5/A1000000", mb16, "0000000100000005000000A1"},
		{3, "2/40000000", 1 << 30, "000000030000000200000001"},
		{1, "0/1FFFFF", 1 << 20, "000000010000000000000001"},
	} {
		lsn, _ := pglogrepl.ParseLSN(tt.lsn)
		if got := walFileName(tt.tl, lsn, tt.segSize); got != tt.want {
			t.Errorf("walFileName(%d, %s, %d) = %s, want %s", tt.tl, tt.lsn, tt.segSize, got, tt.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"16MB": 16 << 20, "1GB": 1 << 30, "64kB": 64 << 10, "8192B": 8192} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "16", "MB", "16 PB", "x MB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) succeeded", bad)
		}
	}
}

func TestWALPos(t *testing.T) {
	p, err := parseWALPos("2/1/A3000060")
	if err != nil || p.tl != 2 || p.lsn != pglogrepl.LSN(0x1_A3000060) || p.position() != "2/1/A3000060" {
		t.Errorf("parse = %+v, %v", p, err)
	}
	for _, bad := range []engine.Position{"", "0/3000060", "x/0/1", "0/0/1", "1/zz"} {
		if _, err := parseWALPos(bad); err == nil {
			t.Errorf("parseWALPos(%q) succeeded", bad)
		}
	}
}

// pattern is n bytes of WAL that tell their own position.
func pattern(start, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((start + i) % 251) // #nosec G115 -- below 251
	}
	return b
}

func TestWALAssembler(t *testing.T) {
	const seg = 1 << 20
	// Resuming mid-segment starts at the segment's boundary.
	a := newWALAssembler(1, seg, pglogrepl.LSN(3*seg+500))
	if a.start() != 3*seg {
		t.Fatalf("start = %s", a.start())
	}
	if _, ok := a.partial(); ok {
		t.Error("a partial segment before any WAL")
	}
	pos := 3 * seg
	add := func(n int) []walSegment {
		t.Helper()
		done, err := a.add(pglogrepl.LSN(pos), pattern(pos, n)) // #nosec G115 -- a small test position
		if err != nil {
			t.Fatal(err)
		}
		pos += n
		return done
	}
	if done := add(seg / 2); len(done) != 0 {
		t.Fatalf("completed %d segments with half of one", len(done))
	}
	p, ok := a.partial()
	if !ok || p.complete || p.start != 3*seg || p.end != 3*seg+seg/2 || p.name != "000000010000000000000003" || len(p.data) != seg/2 {
		t.Errorf("partial = %+v", p)
	}
	// One message crossing a boundary completes the segment and starts the
	// next.
	done := add(seg)
	if len(done) != 1 || !done[0].complete || done[0].name != "000000010000000000000003" ||
		done[0].start != 3*seg || done[0].end != 4*seg || !bytes.Equal(done[0].data, pattern(3*seg, seg)) {
		t.Fatalf("completed = %+v", done)
	}
	first := done[0].data
	if p, _ := a.partial(); p.start != 4*seg || p.end != 4*seg+seg/2 || !bytes.Equal(p.data, pattern(4*seg, seg/2)) {
		t.Errorf("partial after the boundary = %s..%s", p.start, p.end)
	}
	// Several segments in one message.
	done = add(2*seg + 10)
	if len(done) != 2 || done[0].name != "000000010000000000000004" || done[1].name != "000000010000000000000005" ||
		!bytes.Equal(done[1].data, pattern(5*seg, seg)) {
		t.Fatalf("completed = %d segments", len(done))
	}
	// A completed segment's bytes are not changed by later WAL.
	if !bytes.Equal(first, pattern(3*seg, seg)) {
		t.Error("later WAL overwrote a completed segment")
	}
	if a.received() != pglogrepl.LSN(pos) {
		t.Errorf("received = %s, want %X", a.received(), pos)
	}
	// A hole in the stream is refused.
	if _, err := a.add(pglogrepl.LSN(pos+8), []byte("x")); !errors.Is(err, errStreamGap) {
		t.Errorf("a hole = %v", err)
	}
}

func TestCaptureCred(t *testing.T) {
	tg := engine.Target{Name: "prod-pg", Config: config.Target{Credentials: config.Credentials{Backup: config.Credential{User: "pentym_backup"}}}}
	if c, err := captureCred(tg); err != nil || c.User != "pentym_backup" {
		t.Errorf("fallback = %+v, %v", c, err)
	}
	tg.Config.Credentials.Replication.User = "pentym_repl"
	if c, err := captureCred(tg); err != nil || c.User != "pentym_repl" {
		t.Errorf("replication = %+v, %v", c, err)
	}
	var me *msg.Error
	if _, err := captureCred(engine.Target{Name: "x"}); !errors.As(err, &me) || me.Code != msg.ExitConfig || !strings.Contains(me.Message.Fact, "credentials.replication.user") {
		t.Errorf("no role = %v", err)
	}
}
