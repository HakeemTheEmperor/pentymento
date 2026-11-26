// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// Batch limits and timings for oplog capture (PRD 9.1): a batch is stored
// after FlushInterval, BatchBytes or BatchEntries, whichever comes first.
const (
	FlushInterval = 10 * time.Second
	BatchBytes    = 16 << 20
	BatchEntries  = 10000
	// awaitWake bounds a wait for new entries, so timers and a stop are
	// noticed quickly.
	awaitWake = 500 * time.Millisecond
	// majorityWake is how often capture asks for the majority commit point
	// while it waits for one.
	majorityWake = 50 * time.Millisecond
	// flushTimeout bounds storing the last batch on a stop.
	flushTimeout = 20 * time.Second
)

// codeCappedPositionLost is the server's error when the oplog rolled past
// an open cursor's position.
const codeCappedPositionLost = 136

// OplogObserver receives oplog entries as they arrive, before they are
// stored: the event detector (SDD 6.3) plugs in here. The entry is only
// valid during the call.
type OplogObserver interface {
	Entry(entry bson.Raw)
}

// errStandalone means a server has no oplog to capture.
var errStandalone = fmt.Errorf("a standalone MongoDB server has no oplog to capture; point-in-time recovery needs a replica set: %w", errors.ErrUnsupported)

func (e *Engine) logger() *slog.Logger {
	if e.Logger == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return e.Logger
}

// captureCred is the user capture connects as: the replication
// credential, or the backup credential when none is set.
func captureCred(t engine.Target) (config.Credential, error) {
	c := t.Config.Credentials.CaptureCredential()
	if c.User == "" && !x509(targetURI(t.Config)) {
		return c, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, Target: t.Name,
			Fact: "no user configured for oplog capture (credentials.replication.user or credentials.backup.user)",
			Next: "set targets." + t.Name + ".credentials.backup.user in the config, or authenticate with a client certificate (authMechanism=MONGODB-X509 in uri)",
		}}
	}
	return c, nil
}

// ParsePosition reads a log position written by Position.
func ParsePosition(p engine.Position) (bson.Timestamp, error) {
	t, i, ok := strings.Cut(string(p), ":")
	ts, err1 := strconv.ParseUint(t, 10, 32)
	inc, err2 := strconv.ParseUint(i, 10, 32)
	if !ok || err1 != nil || err2 != nil {
		return bson.Timestamp{}, fmt.Errorf("oplog position %q is not understood", p)
	}
	return bson.Timestamp{T: uint32(ts), I: uint32(inc)}, nil // #nosec G115 -- parsed as 32-bit numbers
}

// captureSession connects as the capture user to a replica set member.
func (e *Engine) captureSession(ctx context.Context, t engine.Target) (*session, error) {
	cred, err := captureCred(t)
	if err != nil {
		return nil, err
	}
	s, err := e.connectAs(ctx, t, cred)
	if err != nil {
		return nil, err
	}
	switch {
	case s.hello.sharded():
		s.close(ctx)
		return nil, errors.New("target " + t.Name + " is a sharded cluster, which pentym does not capture")
	case !s.hello.replicaSet():
		s.close(ctx)
		return nil, errStandalone
	}
	return s, nil
}

// oplog returns the replica set's oplog. The server ignores read concern
// on the local database, so capture keeps to the majority commit point
// itself (see majority).
func (s *session) oplog() *mongo.Collection {
	return s.client.Database("local").Collection("oplog.rs")
}

// majority returns the replica set's majority commit point: the newest
// oplog entry a majority of members has written, which a failover cannot
// roll back. Capture stores no entry after it.
func (s *session) majority(ctx context.Context) (bson.Timestamp, error) {
	var h struct {
		LastWrite struct {
			MajorityOpTime struct {
				TS bson.Timestamp `bson:"ts"`
			} `bson:"majorityOpTime"`
		} `bson:"lastWrite"`
	}
	if err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&h); err != nil {
		return bson.Timestamp{}, fmt.Errorf("read the majority commit point: %w", err)
	}
	if h.LastWrite.MajorityOpTime.TS.IsZero() {
		return bson.Timestamp{}, errors.New("the server does not report a majority commit point")
	}
	return h.LastWrite.MajorityOpTime.TS, nil
}

// oplogEnd returns the timestamp of the oldest (first) or newest (last)
// entry in the oplog.
func oplogEnd(ctx context.Context, oplog *mongo.Collection, first bool) (bson.Timestamp, error) {
	dir := -1
	if first {
		dir = 1
	}
	var e struct {
		TS bson.Timestamp `bson:"ts"`
	}
	err := oplog.FindOne(ctx, bson.D{}, options.FindOne().
		SetSort(bson.D{{Key: "$natural", Value: dir}}).SetProjection(bson.D{{Key: "ts", Value: 1}})).Decode(&e)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return e.TS, errors.New("the oplog is empty")
	}
	if err != nil {
		return e.TS, fmt.Errorf("read the oplog: %w", err)
	}
	return e.TS, nil
}

// describe formats an oplog timestamp for a message: its time and
// position.
func describe(ts bson.Timestamp) string {
	return time.Unix(int64(ts.T), 0).UTC().Format(time.RFC3339) + " (" + string(Position(ts)) + ")"
}

// DetectGap reports whether the oplog still holds everything since a
// checkpoint: not if its oldest entry is newer than the checkpoint, when
// the oplog rolled over (PRD 7.3).
func (e *Engine) DetectGap(ctx context.Context, t engine.Target, checkpoint engine.Position) (*engine.Gap, error) {
	if checkpoint == "" {
		return nil, nil
	}
	cp, err := ParsePosition(checkpoint)
	if err != nil {
		return nil, err
	}
	s, err := e.captureSession(ctx, t)
	if err != nil {
		return nil, err
	}
	defer s.close(ctx)
	oldest, err := oplogEnd(ctx, s.oplog(), true)
	if err != nil {
		return nil, err
	}
	if cp.Before(oldest) {
		return &engine.Gap{From: checkpoint, To: Position(oldest),
			Reason: "the oplog rolled over: its oldest entry, from " + describe(oldest) + ", is newer than the checkpoint, " + describe(cp)}, nil
	}
	return nil, nil
}

// LogHealth reports the newest oplog entry capture may store (the
// majority commit point) and how far back the oplog reaches, for
// monitoring the oplog window (PRD 7.3).
func (e *Engine) LogHealth(ctx context.Context, t engine.Target) (*engine.LogHealth, error) {
	s, err := e.captureSession(ctx, t)
	if err != nil {
		return nil, err
	}
	defer s.close(ctx)
	oplog := s.oplog()
	oldest, err := oplogEnd(ctx, oplog, true)
	if err != nil {
		return nil, err
	}
	newest, err := oplogEnd(ctx, oplog, false)
	if err != nil {
		return nil, err
	}
	head, err := s.majority(ctx)
	if err != nil {
		return nil, err
	}
	return &engine.LogHealth{Oplog: true, Head: Position(head), Window: time.Duration(int64(newest.T)-int64(oldest.T)) * time.Second}, nil
}

// batch is oplog entries waiting to be stored.
type batch struct {
	data        []byte
	entries     int
	first, last bson.Timestamp
	// started is when the first entry arrived.
	started time.Time
}

// fits reports whether an entry of n bytes fits in the batch; an empty
// batch takes any entry.
func (b *batch) fits(n int) bool { return b.entries == 0 || len(b.data)+n <= BatchBytes }

// add appends an entry.
func (b *batch) add(entry []byte, ts bson.Timestamp, now time.Time) {
	if b.entries == 0 {
		b.first, b.started = ts, now
	}
	b.data = append(b.data, entry...)
	b.last = ts
	b.entries++
}

// full reports whether the batch reached a size limit.
func (b *batch) full() bool { return b.entries >= BatchEntries || len(b.data) >= BatchBytes }

// due reports whether the batch has waited FlushInterval.
func (b *batch) due(now time.Time) bool {
	return b.entries > 0 && now.Sub(b.started) >= FlushInterval
}

// segment takes the batch as a segment to store and empties it.
func (b *batch) segment() engine.Segment {
	s := engine.Segment{
		Kind: "oplog", First: Position(b.first), Last: Position(b.last), Checkpoint: Position(b.last), Data: b.data,
	}
	*b = batch{}
	return s
}

// Capture tails the target's oplog into sink until ctx ends (SDD 6.2),
// with a tailable cursor on local.oplog.rs rather than a change stream,
// whose output mongorestore --oplogReplay cannot read. It starts after
// the checkpoint's entry, or after the majority commit point when there
// is no checkpoint. Only entries a majority of the replica set has written
// are stored, so a failover never rolls back a stored entry. Entries are
// stored as they are in the oplog, BSON documents
// one after another, in batches of up to FlushInterval, BatchBytes or
// BatchEntries; the batch in progress is stored once more when capture
// stops. If the checkpoint's entry is no longer in the oplog, Capture
// returns engine.ErrGap.
func (e *Engine) Capture(ctx context.Context, t engine.Target, from engine.Position, sink engine.LogSink) error {
	s, err := e.captureSession(ctx, t)
	if err != nil {
		return err
	}
	defer s.close(ctx)
	oplog := s.oplog()
	var start bson.Timestamp
	if from != "" {
		if start, err = ParsePosition(from); err != nil {
			return err
		}
	} else if start, err = s.majority(ctx); err != nil {
		return err
	}
	cur, err := oplog.Find(ctx, bson.D{{Key: "ts", Value: bson.D{{Key: "$gte", Value: start}}}},
		options.Find().SetCursorType(options.TailableAwait).SetMaxAwaitTime(awaitWake))
	if err != nil {
		return fmt.Errorf("open the oplog: %w", err)
	}
	defer func() { _ = cur.Close(context.WithoutCancel(ctx)) }()
	e.logger().Info("capturing the oplog", "target", t.Name, "from", Position(start))

	var b batch
	store := func(ctx context.Context) error {
		if b.entries == 0 {
			return nil
		}
		return sink.Segment(ctx, b.segment())
	}
	stop := func() error {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
		defer cancel()
		return store(fctx)
	}
	majority, err := s.majority(ctx)
	if err != nil {
		return err
	}
	first := true
	for {
		if ctx.Err() != nil {
			return stop()
		}
		if b.due(time.Now()) {
			if err := store(ctx); err != nil {
				return err
			}
		}
		if !cur.TryNext(ctx) {
			if ctx.Err() != nil {
				return stop()
			}
			if err := cur.Err(); err != nil {
				var se mongo.ServerError
				if errors.As(err, &se) && se.HasErrorCode(codeCappedPositionLost) {
					// The oplog rolled past the cursor: store what arrived,
					// then report the gap.
					if serr := store(ctx); serr != nil {
						return serr
					}
					return fmt.Errorf("%w: the oplog rolled over while capture read it: %w", engine.ErrGap, err)
				}
				return fmt.Errorf("read the oplog: %w", err)
			}
			if cur.ID() == 0 {
				return errors.New("the server closed the oplog cursor")
			}
			continue
		}
		sec, inc, ok := cur.Current.Lookup("ts").TimestampOK()
		if !ok {
			return errors.New("an oplog entry has no timestamp")
		}
		ts := bson.Timestamp{T: sec, I: inc}
		if first && from != "" {
			first = false
			if !ts.Equal(start) {
				return fmt.Errorf("%w: the oplog no longer holds the checkpoint's entry, %s; its oldest is from %s", engine.ErrGap, describe(start), describe(ts))
			}
			continue // stored already
		}
		if !ts.After(start) {
			continue // before capture began
		}
		// Wait until a majority has written the entry.
		for ts.After(majority) {
			if b.due(time.Now()) {
				if err := store(ctx); err != nil {
					return err
				}
			}
			if majority, err = s.majority(ctx); err != nil {
				if ctx.Err() != nil {
					return stop()
				}
				return err
			}
			if ts.After(majority) {
				select {
				case <-ctx.Done():
					return stop()
				case <-time.After(majorityWake):
				}
			}
		}
		if e.Observer != nil {
			e.Observer.Entry(cur.Current)
		}
		if !b.fits(len(cur.Current)) {
			if err := store(ctx); err != nil {
				return err
			}
		}
		b.add(cur.Current, ts, time.Now())
		if b.full() {
			if err := store(ctx); err != nil {
				return err
			}
		}
	}
}
