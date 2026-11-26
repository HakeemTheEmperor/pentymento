// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package mongodb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
)

// logSink records what capture stores. block, if set, holds the first
// Segment call until it is closed; held is closed when that call starts.
type logSink struct {
	mu    sync.Mutex
	segs  []engine.Segment
	block chan struct{}
	held  chan struct{}
	once  sync.Once
}

func (l *logSink) Segment(ctx context.Context, s engine.Segment) error {
	if l.block != nil {
		var wait bool
		l.once.Do(func() { wait = true })
		if wait {
			close(l.held)
			select {
			case <-l.block:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.segs = append(l.segs, s)
	return nil
}

func (*logSink) Event(context.Context, engine.Event) error { return nil }

func (l *logSink) segments() []engine.Segment {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]engine.Segment(nil), l.segs...)
}

// reached reports whether the sink holds entries up to pos.
func (l *logSink) reached(t *testing.T, pos engine.Position) bool {
	segs := l.segments()
	if len(segs) == 0 {
		return false
	}
	last, err1 := ParsePosition(segs[len(segs)-1].Checkpoint)
	want, err2 := ParsePosition(pos)
	if err1 != nil || err2 != nil {
		t.Fatalf("positions: %v, %v", err1, err2)
	}
	return !last.Before(want)
}

// capture runs Capture in the background; stop ends it and returns its
// error.
func capture(t *testing.T, e *Engine, tg engine.Target, from engine.Position, sink engine.LogSink) (stop func() error, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- e.Capture(ctx, tg, from, sink) }()
	t.Cleanup(cancel)
	return func() error {
		cancel()
		select {
		case err := <-errc:
			return err
		case <-time.After(time.Minute):
			t.Fatal("capture did not stop")
			return nil
		}
	}, errc
}

func waitFor(t *testing.T, what string, limit time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func head(t *testing.T, e *Engine, tg engine.Target) engine.Position {
	t.Helper()
	h, err := e.LogHealth(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	return h.Head
}

// captured is the entries in stored segments, checked for order and
// against each segment's bounds.
type captured struct {
	ts      []bson.Timestamp
	inserts map[string]int // _id → count, for ns app.capture
}

func readSegments(t *testing.T, segs []engine.Segment) captured {
	t.Helper()
	c := captured{inserts: map[string]int{}}
	for n, s := range segs {
		if s.Kind != "oplog" || s.Checkpoint != s.Last {
			t.Fatalf("segment %d: %+v", n, s)
		}
		rest := s.Data
		entries := 0
		for len(rest) > 0 {
			doc, r, ok := bsonDoc(rest)
			if !ok {
				t.Fatalf("segment %d: entry %d is not a BSON document", n, entries)
			}
			rest = r
			raw := bson.Raw(doc)
			if err := raw.Validate(); err != nil {
				t.Fatalf("segment %d: entry %d: %v", n, entries, err)
			}
			sec, inc, _ := raw.Lookup("ts").TimestampOK()
			ts := bson.Timestamp{T: sec, I: inc}
			if entries == 0 && Position(ts) != s.First {
				t.Fatalf("segment %d starts at %s, not %s", n, Position(ts), s.First)
			}
			if len(c.ts) > 0 && !ts.After(c.ts[len(c.ts)-1]) {
				t.Fatalf("segment %d: entry %s is not after %s", n, Position(ts), Position(c.ts[len(c.ts)-1]))
			}
			c.ts = append(c.ts, ts)
			collectInserts(raw, c.inserts)
			entries++
		}
		if Position(c.ts[len(c.ts)-1]) != s.Last {
			t.Fatalf("segment %d ends at %s, not %s", n, Position(c.ts[len(c.ts)-1]), s.Last)
		}
		if entries > BatchEntries || (entries > 1 && len(s.Data) > BatchBytes) {
			t.Fatalf("segment %d holds %d entries, %d bytes", n, entries, len(s.Data))
		}
	}
	return c
}

// collectInserts counts inserts into app.capture, also inside applyOps.
func collectInserts(op bson.Raw, into map[string]int) {
	ns, _ := op.Lookup("ns").StringValueOK()
	kind, _ := op.Lookup("op").StringValueOK()
	if kind == "i" && ns == "app.capture" {
		if id, ok := op.Lookup("o", "_id").StringValueOK(); ok {
			into[id]++
		}
	}
	if kind == "c" {
		if ops, ok := op.Lookup("o", "applyOps").ArrayOK(); ok {
			vals, _ := ops.Values()
			for _, v := range vals {
				if d, ok := v.DocumentOK(); ok {
					collectInserts(d, into)
				}
			}
		}
	}
}

// insert writes n documents to app.capture with ids prefix-0 onward, some
// of them large, and returns their ids.
func (s *server) insert(t *testing.T, prefix string, n, large int) []string {
	t.Helper()
	c := s.admin.Database("app").Collection("capture")
	var ids []string
	docs := make([]any, 0, 1000)
	flush := func() {
		if len(docs) > 0 {
			if _, err := c.InsertMany(context.Background(), docs); err != nil {
				t.Fatal(err)
			}
			docs = docs[:0]
		}
	}
	for i := range n {
		id := fmt.Sprintf("%s-%d", prefix, i)
		ids = append(ids, id)
		pad := ""
		if i < large {
			pad = strings.Repeat("x", 3<<20)
		}
		docs = append(docs, bson.D{{Key: "_id", Value: id}, {Key: "pad", Value: pad}})
		if len(docs) == cap(docs) || i < large {
			flush()
		}
	}
	flush()
	return ids
}

// TestOplogCapture captures the oplog from a position, checks every
// entry arrives once, in order, in batches within the limits, then
// resumes from the checkpoint.
func TestOplogCapture(t *testing.T) {
	for _, version := range dbtest.MongoDBVersions() {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			s := setup(t, dbtest.StartMongoDB(t, version))
			e, tg := s.engine(t), s.target("pentym_backup")
			h, err := e.LogHealth(context.Background(), tg)
			if err != nil || h.Head == "" || h.Window < 0 {
				t.Fatalf("LogHealth = %+v, %v", h, err)
			}
			from := h.Head
			if gap, err := e.DetectGap(context.Background(), tg, from); gap != nil || err != nil {
				t.Fatalf("DetectGap = %+v, %v", gap, err)
			}

			sink := &logSink{}
			stop, _ := capture(t, e, tg, from, sink)
			ids := s.insert(t, "a", 25000, 8) // over 2 batches by count, and over 16 MiB
			last := head(t, e, tg)
			waitFor(t, "capture to store the inserts", 2*time.Minute, func() bool { return sink.reached(t, last) })
			if err := stop(); err != nil {
				t.Fatalf("Capture: %v", err)
			}
			segs := sink.segments()
			got := readSegments(t, segs)
			start, _ := ParsePosition(from)
			if !got.ts[0].After(start) {
				t.Fatalf("capture stored %s, at or before its start %s", Position(got.ts[0]), from)
			}
			for _, id := range ids {
				if got.inserts[id] != 1 {
					t.Fatalf("insert %s stored %d times", id, got.inserts[id])
				}
			}
			if len(segs) < 3 {
				t.Errorf("%d segments; the limits should have split the inserts into more", len(segs))
			}
			t.Logf("%d entries in %d segments", len(got.ts), len(segs))

			// Resume from the checkpoint: nothing before it again, nothing
			// after it missed.
			cp := segs[len(segs)-1].Checkpoint
			more := s.insert(t, "b", 300, 0)
			sink2 := &logSink{}
			stop2, _ := capture(t, e, tg, cp, sink2)
			last = head(t, e, tg)
			waitFor(t, "the resumed capture", 2*time.Minute, func() bool { return sink2.reached(t, last) })
			if err := stop2(); err != nil {
				t.Fatalf("resumed Capture: %v", err)
			}
			got2 := readSegments(t, sink2.segments())
			cpTS, _ := ParsePosition(cp)
			if !got2.ts[0].After(cpTS) {
				t.Fatalf("the resumed capture stored %s, not after the checkpoint %s", Position(got2.ts[0]), cp)
			}
			for _, id := range more {
				if got2.inserts[id] != 1 {
					t.Fatalf("insert %s stored %d times after resuming", id, got2.inserts[id])
				}
			}
		})
	}
}

// TestOplogStartsWithoutCheckpoint checks a capture with no checkpoint
// stores what is written after it starts.
func TestOplogStartsWithoutCheckpoint(t *testing.T) {
	version := dbtest.MongoDBVersions()[0]
	s := setup(t, dbtest.StartMongoDB(t, version))
	e, tg := s.engine(t), s.target("pentym_backup")
	sink := &logSink{}
	stop, _ := capture(t, e, tg, "", sink)
	c := s.admin.Database("app").Collection("capture")
	n := 0
	waitFor(t, "a stored batch", 2*time.Minute, func() bool {
		if _, err := c.InsertOne(context.Background(), bson.D{{Key: "_id", Value: fmt.Sprintf("c-%d", n)}}); err != nil {
			t.Fatal(err)
		}
		n++
		return len(sink.segments()) > 0
	})
	if err := stop(); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got := readSegments(t, sink.segments()); len(got.inserts) == 0 {
		t.Error("no insert was stored")
	}
}

// oldest is the oldest oplog entry, read as the server's root.
func (s *server) oldest(t *testing.T) bson.Timestamp {
	t.Helper()
	var e struct {
		TS bson.Timestamp `bson:"ts"`
	}
	if err := s.admin.Database("local").Collection("oplog.rs").FindOne(context.Background(), bson.D{},
		options.FindOne().SetSort(bson.D{{Key: "$natural", Value: 1}})).Decode(&e); err != nil {
		t.Fatal(err)
	}
	return e.TS
}

// rollPast writes until the oplog no longer holds pos.
func (s *server) rollPast(t *testing.T, pos engine.Position) {
	t.Helper()
	ts, err := ParsePosition(pos)
	if err != nil {
		t.Fatal(err)
	}
	c := s.admin.Database("app").Collection("churn")
	pad := strings.Repeat("y", 8<<10)
	waitFor(t, "the oplog to roll over", 5*time.Minute, func() bool {
		docs := make([]any, 100)
		for i := range docs {
			docs[i] = bson.D{{Key: "pad", Value: pad}}
		}
		if _, err := c.InsertMany(context.Background(), docs); err != nil {
			t.Fatal(err)
		}
		return ts.Before(s.oldest(t))
	})
}

// TestOplogRollover checks a checkpoint the oplog rolled past is reported
// as a gap, both before capture starts and while it reads.
func TestOplogRollover(t *testing.T) {
	for _, version := range dbtest.MongoDBVersions() {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			s := setup(t, dbtest.StartMongoDBSmallOplog(t, version, 1))
			e, tg := s.engine(t), s.target("pentym_backup")

			t.Run("before capture", func(t *testing.T) {
				cp := head(t, e, tg)
				if gap, err := e.DetectGap(context.Background(), tg, cp); gap != nil || err != nil {
					t.Fatalf("DetectGap before the rollover = %+v, %v", gap, err)
				}
				s.rollPast(t, cp)
				gap, err := e.DetectGap(context.Background(), tg, cp)
				if err != nil || gap == nil {
					t.Fatalf("DetectGap after the rollover = %+v, %v", gap, err)
				}
				to, _ := ParsePosition(gap.To)
				cpTS, _ := ParsePosition(cp)
				if gap.From != cp || !to.After(cpTS) || !strings.Contains(gap.Reason, "rolled over") {
					t.Errorf("gap = %+v", gap)
				}
				err = e.Capture(context.Background(), tg, cp, &logSink{})
				if !errors.Is(err, engine.ErrGap) {
					t.Errorf("Capture from a rolled-over checkpoint = %v", err)
				}
			})

			t.Run("during capture", func(t *testing.T) {
				// Capture stalls storing its first batch while the oplog
				// rolls past where its cursor stands.
				sink := &logSink{block: make(chan struct{}), held: make(chan struct{})}
				from := head(t, e, tg)
				_, done := capture(t, e, tg, from, sink)
				s.insert(t, "d", 10, 0)
				select {
				case <-sink.held: // the batch is stored after the flush interval
				case <-time.After(time.Minute):
					t.Fatal("capture stored no batch")
				}
				s.rollPast(t, head(t, e, tg))
				close(sink.block)
				select {
				case err := <-done:
					if !errors.Is(err, engine.ErrGap) {
						t.Fatalf("Capture after the oplog rolled past it = %v", err)
					}
				case <-time.After(2 * time.Minute):
					t.Fatal("capture did not report the rollover")
				}
			})
		})
	}
}

func TestOplogStandalone(t *testing.T) {
	version := dbtest.MongoDBVersions()[0]
	s := setup(t, dbtest.StartMongoDBStandalone(t, version))
	e, tg := s.engine(t), s.target("pentym_backup")
	if err := e.Capture(context.Background(), tg, "", &logSink{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Capture on a standalone server = %v", err)
	}
	if _, err := e.LogHealth(context.Background(), tg); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("LogHealth on a standalone server = %v", err)
	}
}
