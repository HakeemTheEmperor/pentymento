// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

var t0 = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

// seg is a stored WAL segment n (16 MiB) on timeline 1, stored at t0 plus
// n hours; partialEnd > 0 makes it a partial.
func seg(n uint64, partialEnd uint64) repo.WALSegment {
	s := repo.WALSegment{
		Timeline: 1, Name: fmt.Sprintf("00000001%08X%08X", n/256, n%256),
		Stored: t0.Add(time.Duration(n) * time.Hour), // #nosec G115 -- small
	}
	if partialEnd > 0 {
		s.Partial, s.EndLSN = true, repo.LSN(partialEnd)
	}
	return s
}

// pgSnap is a snapshot from WAL segment n's start to 0x100 bytes later,
// ending at t0 plus n hours.
func pgSnap(n uint64) repo.Snapshot {
	lsn := n << 24
	return repo.Snapshot{
		StartPosition: fmt.Sprintf("%X/%X", lsn>>32, lsn&0xFFFFFFFF+0x28),
		EndPosition:   fmt.Sprintf("%X/%X", lsn>>32, lsn&0xFFFFFFFF+0x100),
		End:           t0.Add(time.Duration(n) * time.Hour), // #nosec G115 -- small
	}
}

func TestPostgresWindow(t *testing.T) {
	t.Run("contiguous", func(t *testing.T) {
		segs := []repo.WALSegment{seg(3, 0), seg(4, 0), seg(5, 0), seg(6, 6<<24+0x500)}
		w := PostgresWindow([]repo.Snapshot{pgSnap(2), pgSnap(3), pgSnap(5)}, segs, nil)
		// Snapshot 2's WAL was never captured; 3 is the earliest usable.
		if !w.PITR || !w.Logs || !w.Start.Equal(t0.Add(3*time.Hour)) || !w.End.Equal(t0.Add(6*time.Hour)) || w.Break != "" {
			t.Errorf("window = %+v", w)
		}
		if w.Actual() != 3*time.Hour {
			t.Errorf("actual = %s", w.Actual())
		}
	})
	t.Run("a missing segment", func(t *testing.T) {
		segs := []repo.WALSegment{seg(3, 0), seg(4, 0), seg(6, 0), seg(7, 0)}
		w := PostgresWindow([]repo.Snapshot{pgSnap(3), pgSnap(6)}, segs, nil)
		if !w.PITR || !w.Start.Equal(t0.Add(6*time.Hour)) || w.Break != "WAL segment 0000000000000005 is missing" ||
			!w.BreakTime.Equal(t0.Add(4*time.Hour)) {
			t.Errorf("window = %+v", w)
		}
	})
	t.Run("a partial with WAL after it", func(t *testing.T) {
		segs := []repo.WALSegment{seg(3, 0), seg(4, 4<<24+0x10), seg(5, 0)}
		w := PostgresWindow([]repo.Snapshot{pgSnap(3), pgSnap(5)}, segs, nil)
		if !w.Start.Equal(t0.Add(5*time.Hour)) || !strings.Contains(w.Break, "stored only up to 0/4000010") {
			t.Errorf("window = %+v", w)
		}
		// The complete segment beside the partial closes the hole.
		w = PostgresWindow([]repo.Snapshot{pgSnap(3), pgSnap(5)}, append(segs, seg(4, 0)), nil)
		if !w.Start.Equal(t0.Add(3*time.Hour)) || w.Break != "" {
			t.Errorf("with the complete segment: window = %+v", w)
		}
	})
	t.Run("a recorded gap", func(t *testing.T) {
		segs := []repo.WALSegment{seg(3, 0), seg(4, 0), seg(5, 0), seg(6, 0)}
		gap := repo.Gap{From: "1/0/4000100", To: "1/0/5000000", Reason: "the replication slot was invalidated", Time: t0.Add(5 * time.Hour)}
		w := PostgresWindow([]repo.Snapshot{pgSnap(3), pgSnap(5)}, segs, []repo.Gap{gap})
		if !w.Start.Equal(t0.Add(5*time.Hour)) || w.Break != gap.Reason || !w.BreakTime.Equal(gap.Time) {
			t.Errorf("window = %+v", w)
		}
		w.Configured = 7 * 24 * time.Hour
		if !w.Shortened(t0.Add(7 * time.Hour)) {
			t.Error("a gap inside the configured window does not shorten it")
		}
	})
	t.Run("an old gap before the run", func(t *testing.T) {
		segs := []repo.WALSegment{seg(8, 0), seg(9, 0)}
		gap := repo.Gap{From: "1/0/3000100", To: "1/0/4000000", Reason: "old", Time: t0}
		w := PostgresWindow([]repo.Snapshot{pgSnap(8)}, segs, []repo.Gap{gap})
		if !w.Start.Equal(t0.Add(8*time.Hour)) || w.Break != "" {
			t.Errorf("window = %+v", w)
		}
	})
	t.Run("no snapshot inside the log", func(t *testing.T) {
		w := PostgresWindow([]repo.Snapshot{pgSnap(1)}, []repo.WALSegment{seg(3, 0)}, nil)
		if w.PITR || !w.Logs {
			t.Errorf("window = %+v", w)
		}
	})
	t.Run("no log", func(t *testing.T) {
		w := PostgresWindow([]repo.Snapshot{pgSnap(1)}, nil, nil)
		if w.PITR || w.Logs || w.Actual() != 0 {
			t.Errorf("window = %+v", w)
		}
	})
	t.Run("a recorded segment size", func(t *testing.T) {
		// 64 MiB segments: segment 00000001 0000000000000002 starts at 0/8000000.
		s := repo.Snapshot{StartPosition: "0/8000028", EndPosition: "0/8000100", End: t0, WALSegmentSize: 64 << 20}
		segs := []repo.WALSegment{{Timeline: 1, Name: "000000010000000000000002", Stored: t0.Add(time.Hour)}}
		if w := PostgresWindow([]repo.Snapshot{s}, segs, nil); !w.PITR {
			t.Errorf("window = %+v", w)
		}
		s.WALSegmentSize = 0 // read as 16 MiB: segment 2 starts at 0/2000000, before the snapshot's WAL ends
		if w := PostgresWindow([]repo.Snapshot{s}, segs, nil); w.PITR {
			t.Errorf("16 MiB: window = %+v", w)
		}
	})
	t.Run("across a 4 GiB boundary", func(t *testing.T) {
		segs := []repo.WALSegment{seg(254, 0), seg(255, 0), seg(256, 0), seg(257, 0)}
		w := PostgresWindow([]repo.Snapshot{pgSnap(254)}, segs, nil)
		if !w.Start.Equal(t0.Add(254*time.Hour)) || w.Break != "" {
			t.Errorf("window = %+v", w)
		}
	})
}

func batch(first, last uint32) repo.OplogBatch {
	return repo.OplogBatch{First: repo.OplogTS{T: first, I: 1}, Last: repo.OplogTS{T: last, I: 1}}
}

func mgSnap(end uint32) repo.Snapshot {
	return repo.Snapshot{StartPosition: fmt.Sprintf("%d:1", end-5), EndPosition: fmt.Sprintf("%d:1", end), End: time.Unix(int64(end), 0).UTC()}
}

func TestMongoWindow(t *testing.T) {
	const base = 1_800_000_000
	batches := []repo.OplogBatch{batch(base+100, base+110), batch(base+111, base+120), batch(base+121, base+200)}
	t.Run("contiguous", func(t *testing.T) {
		w := MongoWindow([]repo.Snapshot{mgSnap(base + 50), mgSnap(base + 105), mgSnap(base + 150)}, batches, nil)
		// The base+50 snapshot ends before capture began.
		if !w.PITR || !w.Start.Equal(time.Unix(base+105, 0)) || !w.End.Equal(time.Unix(base+200, 0)) || w.Break != "" {
			t.Errorf("window = %+v", w)
		}
	})
	t.Run("a snapshot ending just before capture began", func(t *testing.T) {
		s := repo.Snapshot{EndPosition: fmt.Sprintf("%d:0", base+100), End: time.Unix(base+100, 0)}
		if w := MongoWindow([]repo.Snapshot{s}, batches, nil); !w.PITR {
			t.Errorf("window = %+v", w)
		}
	})
	t.Run("a rollover", func(t *testing.T) {
		gap := repo.Gap{From: fmt.Sprintf("%d:1", base+110), Reason: "the oplog rolled over"}
		w := MongoWindow([]repo.Snapshot{mgSnap(base + 105), mgSnap(base + 150)}, batches, []repo.Gap{gap})
		if !w.Start.Equal(time.Unix(base+150, 0)) || w.Break != gap.Reason || !w.BreakTime.Equal(time.Unix(base+110, 0)) {
			t.Errorf("window = %+v", w)
		}
	})
	t.Run("a snapshot after the newest entry", func(t *testing.T) {
		if w := MongoWindow([]repo.Snapshot{mgSnap(base + 300)}, batches, nil); w.PITR {
			t.Errorf("window = %+v", w)
		}
	})
	t.Run("no log", func(t *testing.T) {
		if w := MongoWindow([]repo.Snapshot{mgSnap(base)}, nil, nil); w.PITR || w.Logs {
			t.Errorf("window = %+v", w)
		}
	})
}

func TestAgentView(t *testing.T) {
	now := t0.Add(time.Hour)
	held := &lock.Record{State: lock.StateHeld, Generation: 3, Holder: lock.Holder{Host: "db1", AgentID: "a1"}, Time: now.Add(-time.Hour)}
	hb := func(age time.Duration, id string) []repo.Heartbeat {
		return []repo.Heartbeat{{Host: "db1", AgentID: id, State: repo.StateRunning, Time: now.Add(-age)}}
	}
	for _, tt := range []struct {
		name  string
		lease *lock.Record
		hbs   []repo.Heartbeat
		state string
		level msg.Level
		age   time.Duration
	}{
		{"no lease", nil, nil, AgentNone, msg.LevelWarning, 0},
		{"fresh", held, hb(4*time.Second, "a1"), AgentCapturing, msg.LevelOK, 4 * time.Second},
		{"5 minutes", held, hb(5*time.Minute, "a1"), AgentStale, msg.LevelWarning, 5 * time.Minute},
		{"15 minutes", held, hb(15*time.Minute, "a1"), AgentStale, msg.LevelFailed, 15 * time.Minute},
		{"another agent's heartbeat", held, hb(time.Second, "other"), AgentStale, msg.LevelFailed, time.Hour},
		{"released", &lock.Record{State: lock.StateReleased, Holder: held.Holder, Time: now.Add(-time.Minute)}, nil, AgentStopped, msg.LevelWarning, time.Minute},
	} {
		v := agentView(tt.lease, tt.hbs, now)
		if v.State != tt.state || v.Level != tt.level || v.Age != tt.age {
			t.Errorf("%s: %+v", tt.name, v)
		}
	}
}
