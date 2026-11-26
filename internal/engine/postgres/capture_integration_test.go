// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// captureSink keeps what capture stores, copying it (capture reuses the
// partial segment's buffer).
type captureSink struct {
	mu   sync.Mutex
	segs []engine.Segment
}

func (s *captureSink) Segment(_ context.Context, seg engine.Segment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg.Data = bytes.Clone(seg.Data)
	s.segs = append(s.segs, seg)
	return nil
}

func (*captureSink) Event(context.Context, engine.Event) error { return nil }

func (s *captureSink) snapshot() []engine.Segment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]engine.Segment(nil), s.segs...)
}

func (s *captureSink) checkpoint() engine.Position {
	segs := s.snapshot()
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i].Checkpoint != "" {
			return segs[i].Checkpoint
		}
	}
	return ""
}

func (s *captureSink) find(pred func(engine.Segment) bool) *engine.Segment {
	for _, seg := range s.snapshot() {
		if pred(seg) {
			return &seg
		}
	}
	return nil
}

// running is a capture in progress.
type running struct {
	sink   *captureSink
	cancel context.CancelFunc
	done   chan error
}

func startCapture(t *testing.T, e *Engine, tg engine.Target, from engine.Position) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{sink: &captureSink{}, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- e.Capture(ctx, tg, from, r.sink) }()
	t.Cleanup(cancel)
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("capture stopped with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("capture did not stop")
	}
}

func waitSeg(t *testing.T, r *running, what string, limit time.Duration, pred func(engine.Segment) bool) engine.Segment {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		if s := r.sink.find(pred); s != nil {
			return *s
		}
		select {
		case err := <-r.done:
			t.Fatalf("capture ended while waiting for %s: %v", what, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s within %s; got %d segments", what, limit, len(r.sink.snapshot()))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// checkSegmentHeader checks a stored segment starts with a WAL page header
// naming its own position and timeline.
func checkSegmentHeader(t *testing.T, s engine.Segment) {
	t.Helper()
	if len(s.Data) < 16 {
		t.Fatalf("%s holds %d bytes", s.Name, len(s.Data))
	}
	tli := binary.LittleEndian.Uint32(s.Data[4:8])
	pageAddr := binary.LittleEndian.Uint64(s.Data[8:16])
	first, _ := pglogrepl.ParseLSN(string(s.First))
	if pageAddr != uint64(first) || strconv.FormatUint(uint64(tli), 10) != s.Timeline {
		t.Errorf("%s: page header says %X on timeline %d, the segment is %s on %s", s.Name, pageAddr, tli, s.First, s.Timeline)
	}
}

func TestCapture(t *testing.T) {
	for _, version := range dbtest.PostgresVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(t, version)
			e := s.engine(t)
			hba := s.exec(t, e, "SELECT current_setting('hba_file')")[0][0]
			s.pg.Exec(t, "sh", "-c", "echo 'host replication all all scram-sha-256' >> "+hba)
			s.exec(t, e, "SELECT pg_reload_conf()")
			s.env["REPL_PW"] = "repl-pw-" + randomHex(8)
			s.exec(t, e, "CREATE ROLE pentym_repl LOGIN REPLICATION PASSWORD '"+s.env["REPL_PW"]+"'")
			s.exec(t, e, "CREATE TABLE events (id serial PRIMARY KEY, note text)")
			tg := s.target("nobody", "NONE", "pentym_test")
			tg.Config.Credentials.Backup.User = ""
			ref, _ := secrets.ParseRef("env:REPL_PW")
			tg.Config.Credentials.Replication.User, tg.Config.Credentials.Replication.Password = "pentym_repl", ref
			slot := tg.Config.SlotName(tg.Name)

			// No checkpoint: capture creates the slot and starts where it
			// stands.
			r := startCapture(t, e, tg, "")
			waitActive(t, s, e, slot)
			// A complete segment once the server switches WAL files.
			s.exec(t, e, "INSERT INTO events (note) SELECT md5(i::text) FROM generate_series(1, 1000) AS i")
			switched := s.exec(t, e, "SELECT pg_walfile_name(pg_current_wal_lsn())")[0][0]
			s.exec(t, e, "SELECT pg_switch_wal()")
			full := waitSeg(t, r, "the switched segment", 30*time.Second, func(sg engine.Segment) bool { return sg.Kind == "wal" && sg.Name == switched })
			checkSegmentHeader(t, full)
			if len(full.Data) != 16<<20 {
				t.Errorf("a complete segment holds %d bytes", len(full.Data))
			}
			// A partial segment within the partial interval, while new WAL
			// arrives.
			s.exec(t, e, "INSERT INTO events (note) VALUES ('after the switch')")
			wrote := time.Now()
			part := waitSeg(t, r, "a partial segment", PartialInterval+15*time.Second, func(sg engine.Segment) bool { return sg.Kind == "wal-partial" })
			checkSegmentHeader(t, part)
			if took := time.Since(wrote); took > PartialInterval+5*time.Second {
				t.Errorf("the partial segment came %s after the write", took)
			}

			// Stop: the last WAL is flushed as a partial; resume from the
			// checkpoint without a gap.
			s.exec(t, e, "INSERT INTO events (note) VALUES ('just before the stop')")
			time.Sleep(time.Second)
			r.stop(t)
			cp := r.sink.checkpoint()
			if cp == "" {
				t.Fatal("no checkpoint position after the stop")
			}
			last := r.sink.snapshot()[len(r.sink.snapshot())-1]
			if gap, err := e.DetectGap(ctx, tg, cp); err != nil || gap != nil {
				t.Fatalf("a gap right after a clean stop: %+v, %v", gap, err)
			}
			s.exec(t, e, "INSERT INTO events (note) SELECT 'while stopped' FROM generate_series(1, 2000)")
			h, err := e.LogHealth(ctx, tg)
			if err != nil || h.RetainedBytes <= 0 || h.Lost || h.Head == "" {
				t.Errorf("health while stopped = %+v, %v", h, err)
			}
			r2 := startCapture(t, e, tg, cp)
			if last.Kind == "wal-partial" {
				// The segment in progress is streamed again from its start:
				// the same bytes, then the WAL written while stopped.
				again := waitSeg(t, r2, "the resumed segment", PartialInterval+15*time.Second, func(sg engine.Segment) bool { return sg.Name == last.Name })
				if again.First != last.First || !bytes.HasPrefix(again.Data, last.Data) || len(again.Data) <= len(last.Data) {
					t.Errorf("resuming lost or changed WAL: before %s %s..%s (%d bytes), after %s..%s (%d bytes)",
						last.Name, last.First, last.Last, len(last.Data), again.First, again.Last, len(again.Data))
				}
			} else {
				waitSeg(t, r2, "WAL after the resume", PartialInterval+15*time.Second, func(engine.Segment) bool { return true })
			}
			r2.stop(t)
			cp = r2.sink.checkpoint()

			// A dropped slot is a gap.
			s.exec(t, e, "SELECT pg_drop_replication_slot('"+slot+"')")
			gap, err := e.DetectGap(ctx, tg, cp)
			if err != nil || gap == nil || !strings.Contains(gap.Reason, "no longer exists") || gap.From != cp {
				t.Errorf("dropped slot: %+v, %v", gap, err)
			}

			// An invalidated slot is a gap; capture then starts afresh.
			r3 := startCapture(t, e, tg, "")
			waitActive(t, s, e, slot)
			s.exec(t, e, "INSERT INTO events (note) VALUES ('tick')")
			waitSeg(t, r3, "WAL on the new slot", PartialInterval+15*time.Second, func(engine.Segment) bool { return true })
			r3.stop(t)
			cp = r3.sink.checkpoint()
			s.exec(t, e, "ALTER SYSTEM SET max_slot_wal_keep_size = '1MB'")
			s.exec(t, e, "SELECT pg_reload_conf()")
			deadline := time.Now().Add(60 * time.Second)
			for s.exec(t, e, "SELECT coalesce(wal_status, '') FROM pg_replication_slots WHERE slot_name = '"+slot+"'")[0][0] != "lost" {
				if time.Now().After(deadline) {
					t.Fatal("the slot was not invalidated")
				}
				s.exec(t, e, "INSERT INTO events (note) SELECT md5(i::text) FROM generate_series(1, 5000) AS i")
				s.exec(t, e, "SELECT pg_switch_wal()")
				s.exec(t, e, "CHECKPOINT")
			}
			gap, err = e.DetectGap(ctx, tg, cp)
			if err != nil || gap == nil || !strings.Contains(gap.Reason, "invalidated") {
				t.Errorf("invalidated slot: %+v, %v", gap, err)
			}
			if h, err := e.LogHealth(ctx, tg); err != nil || !h.Lost || h.RetainedLimit != 1<<20 {
				t.Errorf("health of the invalidated slot = %+v, %v", h, err)
			}
			s.exec(t, e, "ALTER SYSTEM RESET max_slot_wal_keep_size")
			s.exec(t, e, "SELECT pg_reload_conf()")
			r4 := startCapture(t, e, tg, "")
			waitActive(t, s, e, slot)
			s.exec(t, e, "INSERT INTO events (note) VALUES ('tock')")
			waitSeg(t, r4, "WAL on the recreated slot", PartialInterval+15*time.Second, func(engine.Segment) bool { return true })
			r4.stop(t)
			if st := s.exec(t, e, "SELECT wal_status FROM pg_replication_slots WHERE slot_name = '"+slot+"'")[0][0]; st == "lost" {
				t.Error("the invalidated slot was not replaced")
			}
		})
	}
}

// waitActive waits until capture is streaming from the slot.
func waitActive(t *testing.T, s *server, e *Engine, slot string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for s.exec(t, e, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = '"+slot+"' AND active")[0][0] != "1" {
		if time.Now().After(deadline) {
			t.Fatal("capture is not streaming from the slot")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
