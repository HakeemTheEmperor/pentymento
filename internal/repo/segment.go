// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/compress"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
)

// MaxSegmentSize bounds one log segment or oplog batch. Postgres WAL
// segments are 16 MiB by default; oplog batches are flushed at 16 MB.
const MaxSegmentSize = 64 << 20

// Log segments (SDD 5.4) are individual objects: compressed as a whole
// (header byte and one zstd frame, raw when that saves under 3%), then
// sealed with their own data key wrapped to the public key (see crypto.md),
// bound to their object key. A segment can be read, expired or re-wrapped
// on its own.
//
//	wal/<target>/<timeline>/<segment>.zst.enc                 a complete WAL segment
//	wal/<target>/<timeline>/<segment>.partial-<lsn>.zst.enc   a partial segment, up to <lsn>
//	wal/<target>/<timeline>.history.zst.enc                   a timeline history file
//	oplog/<target>/<first>-<last>.zst.enc                     an oplog batch
const (
	walPrefixRoot   = "wal/"
	oplogPrefixRoot = "oplog/"
	segmentSuffix   = ".zst.enc"
)

func walPrefix(tid string) string   { return walPrefixRoot + tid + "/" }
func oplogPrefix(tid string) string { return oplogPrefixRoot + tid + "/" }

// Timeline is a Postgres timeline ID, written as 8 uppercase hex digits.
type Timeline uint32

func (t Timeline) String() string { return fmt.Sprintf("%08X", uint32(t)) }

// LSN is a Postgres log sequence number, written in keys as 16 uppercase
// hex digits so keys sort by position.
type LSN uint64

func (l LSN) String() string { return fmt.Sprintf("%016X", uint64(l)) }

// segmentNameRE is a Postgres WAL segment file name: timeline, log and
// segment, 8 uppercase hex digits each.
var segmentNameRE = regexp.MustCompile(`^[0-9A-F]{24}$`)

// WALSegment describes a stored WAL segment.
type WALSegment struct {
	Key      string
	Timeline Timeline
	// Name is the segment's Postgres file name, such as
	// 000000010000000000000003.
	Name string
	// Partial is true for a partial segment, holding WAL up to EndLSN.
	Partial bool
	EndLSN  LSN
	Size    int64
}

func walSegmentKey(tid string, tl Timeline, name string) string {
	return fmt.Sprintf("%s%s/%s%s", walPrefix(tid), tl, name, segmentSuffix)
}

func walPartialKey(tid string, tl Timeline, name string, end LSN) string {
	return fmt.Sprintf("%s%s/%s.partial-%s%s", walPrefix(tid), tl, name, end, segmentSuffix)
}

func timelineHistoryKey(tid string, tl Timeline) string {
	return fmt.Sprintf("%s%s.history%s", walPrefix(tid), tl, segmentSuffix)
}

// OplogTS is a MongoDB oplog timestamp: seconds and an increment.
type OplogTS struct {
	T uint32
	I uint32
}

// String writes the timestamp sortably: 10 zero-padded digits each.
func (ts OplogTS) String() string { return fmt.Sprintf("%010d.%010d", ts.T, ts.I) }

// Less reports whether ts is before o.
func (ts OplogTS) Less(o OplogTS) bool { return ts.T < o.T || (ts.T == o.T && ts.I < o.I) }

// OplogBatch describes a stored batch of oplog entries.
type OplogBatch struct {
	Key   string
	First OplogTS
	Last  OplogTS
	Size  int64
}

func oplogBatchKey(tid string, first, last OplogTS) string {
	return fmt.Sprintf("%s%s-%s%s", oplogPrefix(tid), first, last, segmentSuffix)
}

// PutWALSegment stores a complete WAL segment. Storing the same segment
// again succeeds: duplicates are harmless.
func (r *Repository) PutWALSegment(ctx context.Context, tid string, tl Timeline, name string, data []byte) error {
	if !segmentNameRE.MatchString(name) {
		return fmt.Errorf("%q is not a WAL segment name", name)
	}
	return r.putSegment(ctx, walSegmentKey(tid, tl, name), data)
}

// PutWALPartial stores the current partial segment, holding WAL up to end.
// Successive partials have distinct names; prune removes the ones a later
// partial or the complete segment supersedes.
func (r *Repository) PutWALPartial(ctx context.Context, tid string, tl Timeline, name string, end LSN, data []byte) error {
	if !segmentNameRE.MatchString(name) {
		return fmt.Errorf("%q is not a WAL segment name", name)
	}
	return r.putSegment(ctx, walPartialKey(tid, tl, name, end), data)
}

// PutTimelineHistory stores a Postgres timeline history file.
func (r *Repository) PutTimelineHistory(ctx context.Context, tid string, tl Timeline, data []byte) error {
	return r.putSegment(ctx, timelineHistoryKey(tid, tl), data)
}

// PutOplogBatch stores a batch of oplog entries from first to last.
func (r *Repository) PutOplogBatch(ctx context.Context, tid string, first, last OplogTS, data []byte) error {
	if last.Less(first) {
		return fmt.Errorf("oplog batch ends (%s) before it starts (%s)", last, first)
	}
	return r.putSegment(ctx, oplogBatchKey(tid, first, last), data)
}

func (r *Repository) putSegment(ctx context.Context, key string, data []byte) error {
	if !r.unlocked {
		return ErrLocked
	}
	if len(data) > MaxSegmentSize {
		return fmt.Errorf("log segment of %d bytes exceeds %d", len(data), MaxSegmentSize)
	}
	c, err := compress.New(compress.Default, MaxSegmentSize)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	b := c.Compress(nil, data)
	if !r.cfg.Insecure() {
		if b, err = crypto.SealObject(r.public, key, b); err != nil {
			return err
		}
	}
	err = r.be.Put(ctx, key, bytes.NewReader(b), backend.PutOptions{IfNotExists: true})
	if errors.Is(err, backend.ErrExists) {
		return nil // the same segment, stored before: duplicates are harmless
	}
	return err
}

// GetSegment returns a log segment's, timeline history's or oplog batch's
// content. It needs a full key slot.
func (r *Repository) GetSegment(ctx context.Context, key string) ([]byte, error) {
	if !r.CanRead() {
		return nil, ErrNoPrivateKey
	}
	b, err := readAll(ctx, r.be, key, MaxSegmentSize+(1<<20))
	if err != nil {
		return nil, err
	}
	if !r.cfg.Insecure() {
		if b, err = crypto.OpenObject(r.private, key, b); err != nil {
			return nil, fmt.Errorf("%s: %w: %w", key, ErrCorrupt, err)
		}
	}
	c, err := compress.New(compress.Default, MaxSegmentSize)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	out, err := c.Decompress(nil, b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", key, ErrCorrupt, err)
	}
	return out, nil
}

// TimelineHistoryKey returns the key of a timeline's history file.
func (r *Repository) TimelineHistoryKey(tid string, tl Timeline) string {
	return timelineHistoryKey(tid, tl)
}

var walKeyRE = regexp.MustCompile(`^wal/[0-9a-f]{16}/([0-9A-F]{8})/([0-9A-F]{24})(?:\.partial-([0-9A-F]{16}))?\.zst\.enc$`)

// WALSegments lists a target's stored WAL segments, complete and partial,
// in key order (timeline, then segment, then end position).
func (r *Repository) WALSegments(ctx context.Context, tid string) ([]WALSegment, error) {
	var out []WALSegment
	err := r.be.List(ctx, walPrefix(tid), func(oi backend.ObjectInfo) error {
		m := walKeyRE.FindStringSubmatch(oi.Key)
		if m == nil {
			return nil // timeline history or an unrelated object
		}
		tl, _ := strconv.ParseUint(m[1], 16, 32)
		s := WALSegment{Key: oi.Key, Timeline: Timeline(tl), Name: m[2], Size: oi.Size}
		if m[3] != "" {
			end, _ := strconv.ParseUint(m[3], 16, 64)
			s.Partial, s.EndLSN = true, LSN(end)
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

var oplogKeyRE = regexp.MustCompile(`^oplog/[0-9a-f]{16}/(\d{10})\.(\d{10})-(\d{10})\.(\d{10})\.zst\.enc$`)

// OplogBatches lists a target's stored oplog batches, oldest first.
func (r *Repository) OplogBatches(ctx context.Context, tid string) ([]OplogBatch, error) {
	var out []OplogBatch
	err := r.be.List(ctx, oplogPrefix(tid), func(oi backend.ObjectInfo) error {
		m := oplogKeyRE.FindStringSubmatch(oi.Key)
		if m == nil {
			return nil
		}
		n := make([]uint32, 4)
		for i := range n {
			v, err := strconv.ParseUint(m[i+1], 10, 32)
			if err != nil {
				return nil // out of range: not a batch this version wrote
			}
			n[i] = uint32(v)
		}
		out = append(out, OplogBatch{Key: oi.Key, First: OplogTS{n[0], n[1]}, Last: OplogTS{n[2], n[3]}, Size: oi.Size})
		return nil
	})
	return out, err
}
