// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

func rfc(t time.Time) string { return t.UTC().Format("15:04") }

func names(segs []repo.WALSegment) string {
	var out []string
	for _, s := range segs {
		n := s.Name[16:]
		if s.Partial {
			n += "p"
		}
		out = append(out, n)
	}
	return strings.Join(out, ",")
}

// pitrRepo: snapshots at segments 3 and 6, WAL 3..9 stored an hour apart,
// segment 9 partial.
func pitrRepo() ([]repo.Snapshot, []repo.WALSegment) {
	return []repo.Snapshot{pgSnap(3), pgSnap(6)},
		[]repo.WALSegment{seg(3, 0), seg(4, 0), seg(5, 0), seg(6, 0), seg(7, 0), seg(8, 0), seg(9, 9<<24+0x800)}
}

func unreachableMsg(t *testing.T, err error) msg.Message {
	t.Helper()
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitUnrecoverable || me.Message.ID != IDUnreachable {
		t.Fatalf("err = %v", err)
	}
	return me.Message
}

func TestPlanTime(t *testing.T) {
	snaps, segs := pitrRepo()
	// 07:30 is after snapshot 6 ended (06:00): replay from it; the first
	// segment stored after 07:30 is 8, and one more is 9.
	plan, err := PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{Time: t0.Add(7*time.Hour + 30*time.Minute)}, rfc)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Snapshot.End != t0.Add(6*time.Hour) || names(plan.Segments) != "00000006,00000007,00000008,00000009p" || plan.SegmentSize != 16<<20 {
		t.Errorf("plan = %s from %s", names(plan.Segments), plan.Snapshot.End)
	}
	// 04:30: snapshot 3, segments 3..6.
	plan, err = PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{Time: t0.Add(4*time.Hour + 30*time.Minute)}, rfc)
	if err != nil || plan.Snapshot.End != t0.Add(3*time.Hour) || names(plan.Segments) != "00000003,00000004,00000005,00000006" {
		t.Errorf("04:30 plan = %+v, %v", plan, err)
	}
	// Exactly the newest stored change is reachable; a second later is not.
	if _, err := PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{Time: t0.Add(9 * time.Hour)}, rfc); err != nil {
		t.Errorf("the newest change: %v", err)
	}
	m := unreachableMsg(t, func() error {
		_, err := PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{Time: t0.Add(9*time.Hour + time.Second)}, rfc)
		return err
	}())
	if m.Fact != "cannot restore to 09:00: it is after the newest stored change, from 09:00" || m.Next != "restore to a point that can be reached: from 03:00 to 09:00" {
		t.Errorf("after the end: %+v", m)
	}
	m = unreachableMsg(t, func() error {
		_, err := PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{Time: t0.Add(2 * time.Hour)}, rfc)
		return err
	}())
	if !strings.Contains(m.Fact, "before the earliest point that can be reached, 03:00") {
		t.Errorf("before the start: %+v", m)
	}
}

func TestPlanGap(t *testing.T) {
	snaps, segs := pitrRepo()
	// Segment 5 is missing: 03:00-04:00 and 06:00-09:00 can be reached.
	var cut []repo.WALSegment
	for _, s := range segs {
		if !strings.HasSuffix(s.Name, "05") {
			cut = append(cut, s)
		}
	}
	m := unreachableMsg(t, func() error {
		_, err := PlanPostgres("prod-pg", snaps, cut, nil, engine.Point{Time: t0.Add(5 * time.Hour)}, rfc)
		return err
	}())
	if m.Fact != "cannot restore to 05:00: it falls in a gap in the captured WAL, between 04:00 and 06:00: WAL segment 0000000000000005 is missing" ||
		m.Next != "restore to a point that can be reached: from 03:00 to 04:00; from 06:00 to 09:00" || m.Impact != "nothing was restored" {
		t.Errorf("in the gap: %+v", m)
	}
	// Both sides of the gap still restore.
	if plan, err := PlanPostgres("prod-pg", snaps, cut, nil, engine.Point{Time: t0.Add(3*time.Hour + 30*time.Minute)}, rfc); err != nil || names(plan.Segments) != "00000003,00000004" {
		t.Errorf("before the gap: %+v, %v", plan, err)
	}
	if _, err := PlanPostgres("prod-pg", snaps, cut, nil, engine.Point{Time: t0.Add(8 * time.Hour)}, rfc); err != nil {
		t.Errorf("after the gap: %v", err)
	}
	// A recorded gap cuts the WAL even where every segment is stored.
	gap := repo.Gap{From: "1/0/5000100", To: "1/0/6000000", Reason: "the replication slot was invalidated", Time: t0.Add(6 * time.Hour)}
	m = unreachableMsg(t, func() error {
		_, err := PlanPostgres("prod-pg", snaps, segs, []repo.Gap{gap}, engine.Point{Time: t0.Add(5*time.Hour + 30*time.Minute)}, rfc)
		return err
	}())
	if !strings.Contains(m.Fact, "it falls in a gap in the captured WAL") || !strings.Contains(m.Fact, "the replication slot was invalidated") {
		t.Errorf("a recorded gap: %+v", m)
	}
}

func TestPlanLSNAndXID(t *testing.T) {
	snaps, segs := pitrRepo()
	plan, err := PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{Before: "0/7001000"}, rfc)
	if err != nil || plan.Snapshot.End != t0.Add(6*time.Hour) || names(plan.Segments) != "00000006,00000007" {
		t.Errorf("LSN plan = %+v, %v", plan, err)
	}
	// A capture position works too.
	if plan, err := PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{Before: "1/0/5000000"}, rfc); err != nil || plan.Snapshot.End != t0.Add(3*time.Hour) {
		t.Errorf("capture-position plan = %+v, %v", plan, err)
	}
	m := unreachableMsg(t, func() error {
		_, err := PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{Before: "0/9000900"}, rfc)
		return err
	}())
	if m.Fact != "cannot restore to WAL position 0/9000900: it is after the newest stored WAL, 0/9000800" {
		t.Errorf("beyond the WAL: %+v", m)
	}
	var me *msg.Error
	if _, err := PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{Before: "zz"}, rfc); !errors.As(err, &me) || me.Code != msg.ExitUsage {
		t.Errorf("a bad LSN: %v", err)
	}
	// A transaction ID: the earliest snapshot of the newest stretch, all
	// of its WAL.
	plan, err = PlanPostgres("prod-pg", snaps, segs, nil, engine.Point{XID: "8812"}, rfc)
	if err != nil || plan.Snapshot.End != t0.Add(3*time.Hour) || names(plan.Segments) != "00000003,00000004,00000005,00000006,00000007,00000008,00000009p" {
		t.Errorf("XID plan = %+v, %v", plan, err)
	}
}

func TestPlanNothing(t *testing.T) {
	var me *msg.Error
	if _, err := PlanPostgres("prod-pg", nil, nil, nil, engine.Point{XID: "1"}, rfc); !errors.As(err, &me) || me.Code != msg.ExitNotFound {
		t.Errorf("no snapshots: %v", err)
	}
	m := unreachableMsg(t, func() error {
		_, err := PlanPostgres("prod-pg", []repo.Snapshot{pgSnap(3)}, nil, nil, engine.Point{XID: "1"}, rfc)
		return err
	}())
	if !strings.Contains(m.Fact, "no WAL has been captured") {
		t.Errorf("no WAL: %+v", m)
	}
	m = unreachableMsg(t, func() error {
		_, err := PlanPostgres("prod-pg", []repo.Snapshot{pgSnap(1)}, []repo.WALSegment{seg(3, 0)}, nil, engine.Point{XID: "1"}, rfc)
		return err
	}())
	if !strings.Contains(m.Fact, "no snapshot lies within the captured WAL") {
		t.Errorf("no snapshot in the WAL: %+v", m)
	}
}

// TestStageable: per timeline, a complete segment wins over partials, and
// the longest partial over shorter ones.
func TestStageable(t *testing.T) {
	a := repo.WALSegment{Timeline: 1, Name: "000000010000000000000009", Partial: true, EndLSN: 0x9000100}
	b := repo.WALSegment{Timeline: 1, Name: "000000010000000000000009", Partial: true, EndLSN: 0x9000800}
	c := repo.WALSegment{Timeline: 2, Name: "000000020000000000000009"}
	d := repo.WALSegment{Timeline: 2, Name: "000000020000000000000009", Partial: true, EndLSN: 0x9000400}
	got := stageable(&walPiece{segs: []repo.WALSegment{a, d, b, c}})
	if len(got) != 2 || got[0] != b || got[1] != c {
		t.Errorf("stageable = %+v", got)
	}
}
