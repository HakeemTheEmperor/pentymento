// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package ops

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// walSink stores captured WAL into the repository, as the agent does.
type walSink struct {
	r   *repo.Repository
	tid string
	mu  sync.Mutex
	end pglogrepl.LSN
}

func (s *walSink) Segment(ctx context.Context, seg engine.Segment) error {
	tl, _ := strconv.ParseUint(seg.Timeline, 10, 32)
	timeline := repo.Timeline(tl) // #nosec G115 -- parsed as 32 bits
	var err error
	switch seg.Kind {
	case "wal":
		err = s.r.PutWALSegment(ctx, s.tid, timeline, seg.Name, seg.Data)
	case "wal-partial":
		var end pglogrepl.LSN
		if end, err = pglogrepl.ParseLSN(string(seg.Last)); err == nil {
			err = s.r.PutWALPartial(ctx, s.tid, timeline, seg.Name, repo.LSN(end), seg.Data)
		}
	case "history":
		err = s.r.PutTimelineHistory(ctx, s.tid, timeline, seg.Data)
	}
	if err != nil || seg.Last == "" {
		return err
	}
	if l, err := pglogrepl.ParseLSN(string(seg.Last)); err == nil {
		s.mu.Lock()
		s.end = max(s.end, l)
		s.mu.Unlock()
	}
	return nil
}

func (*walSink) Event(context.Context, engine.Event) error { return nil }

func (s *walSink) stored() pglogrepl.LSN {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end
}

func lsnOfText(t *testing.T, s string) pglogrepl.LSN {
	t.Helper()
	l, err := pglogrepl.ParseLSN(s)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestPostgresPointInTime (Build Order C21): WAL is captured around a
// base backup while rows a, b, c and d are written; restores to just
// before c's transaction, to a time between b and c, and to the WAL
// position between them hold a and b only. A point after the newest
// stored change is refused.
func TestPostgresPointInTime(t *testing.T) {
	for _, version := range dbtest.PostgresVersions() {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			target, eng, dsn, _ := pgServer(t, version)
			cfg, err := pgconn.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			target.Config.Database = cfg.Database
			pgSQL(t, dsn, "CREATE TABLE events (id serial PRIMARY KEY, note text NOT NULL)")
			pgSQL(t, dsn, "INSERT INTO events (note) VALUES ('a')")

			f := newFixture(t)
			tid, err := f.repo.PutTargetInfo(ctx, repo.TargetInfo{Name: target.Name, Engine: "postgres"})
			if err != nil {
				t.Fatal(err)
			}
			target.ID = tid
			sink := &walSink{r: f.repo, tid: tid}
			cctx, stopCapture := context.WithCancel(ctx)
			captured := make(chan error, 1)
			go func() { captured <- eng.Capture(cctx, target, "", sink) }()
			// The slot exists once capture has stored its first WAL.
			pgSQL(t, dsn, "SELECT pg_switch_wal()")
			waitUntil(t, "capture to start", time.Minute, func() bool { return sink.stored() > 0 })

			b := f.backup(eng)
			b.Target = target
			if _, err := b.Run(ctx); err != nil {
				t.Fatalf("backup: %v", err)
			}
			pgSQL(t, dsn, "INSERT INTO events (note) VALUES ('b')")
			beforeC := string(pgSQL(t, dsn, "SELECT pg_current_wal_insert_lsn()::text")[0])
			time.Sleep(1200 * time.Millisecond)
			between := string(pgSQL(t, dsn, "SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")[0])
			time.Sleep(1200 * time.Millisecond)
			xid := string(pgSQL(t, dsn, "WITH i AS (INSERT INTO events (note) VALUES ('c') RETURNING 1) SELECT txid_current()::text FROM i")[0])
			pgSQL(t, dsn, "INSERT INTO events (note) VALUES ('d')")
			end := lsnOfText(t, string(pgSQL(t, dsn, "SELECT pg_current_wal_flush_lsn()::text")[0]))
			pgSQL(t, dsn, "SELECT pg_switch_wal()")
			waitUntil(t, "capture to store the writes", 2*time.Minute, func() bool { return sink.stored() >= end })
			stopCapture()
			if err := <-captured; err != nil {
				t.Fatalf("capture: %v", err)
			}
			at, err := time.Parse(time.RFC3339Nano, between)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("b before %s, time between %s, c in transaction %s, WAL to %s", beforeC, at, xid, end)

			set, work := sideInstances(t, testProviders()[0])
			for _, tt := range []struct {
				name  string
				point engine.Point
			}{
				{"xid", engine.Point{XID: xid}},
				{"time", engine.Point{Time: at}},
				{"lsn", engine.Point{Before: engine.Position(beforeC)}},
			} {
				t.Run(tt.name, func(t *testing.T) {
					o := f.restore(t, eng, set, work)
					o.Target, o.Point = target, tt.point
					rctx, cancel := context.WithTimeout(ctx, restoreDeadline)
					defer cancel()
					res, err := o.Run(rctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := res.Close(context.Background()); err != nil {
							t.Errorf("close: %v", err)
						}
					}()
					c := res.Instance.Conn()
					side := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
						c.Host, c.Port, c.User, c.Password.Expose(), cfg.Database)
					rows := pgSQLAll(t, side, "SELECT string_agg(note, ',' ORDER BY id) FROM events")
					if rows != "a,b" {
						t.Errorf("restored rows = %q, want a,b", rows)
					}
					// The restored instance is promoted: it takes writes.
					pgSQL(t, side, "INSERT INTO events (note) VALUES ('side')")
					if res.Reached == nil || res.Reached.Position == "" {
						t.Errorf("reached = %+v", res.Reached)
					}
					t.Logf("reached %+v from snapshot %s", res.Reached, res.Snapshot.ID)
				})
			}

			t.Run("unreachable", func(t *testing.T) {
				o := f.restore(t, eng, set, work)
				o.Target, o.Point = target, engine.Point{Time: time.Now().Add(time.Hour)}
				_, err := o.Run(ctx)
				var me *msg.Error
				if !errors.As(err, &me) || me.Code != msg.ExitUnrecoverable || me.Message.ID != IDUnreachable ||
					!strings.Contains(me.Message.Fact, "after the newest stored change") {
					t.Errorf("restore after the newest change = %v", err)
				}
			})
		})
	}
}

// pgSQLAll returns the first column of the first row as text.
func pgSQLAll(t *testing.T, dsn, sql string) string {
	t.Helper()
	row := pgSQL(t, dsn, sql)
	if len(row) == 0 {
		return ""
	}
	return string(row[0])
}

func waitUntil(t *testing.T, what string, limit time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
