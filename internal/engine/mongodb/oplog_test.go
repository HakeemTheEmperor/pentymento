// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func TestParsePosition(t *testing.T) {
	for _, ts := range []bson.Timestamp{{T: 0, I: 0}, {T: 1700000000, I: 7}, {T: 1<<32 - 1, I: 1<<32 - 1}} {
		got, err := ParsePosition(Position(ts))
		if err != nil || !got.Equal(ts) {
			t.Errorf("ParsePosition(%s) = %v, %v", Position(ts), got, err)
		}
	}
	for _, p := range []engine.Position{"", "1700000000", "1700000000:", ":1", "1/0/3000060", "4294967296:1", "1:-1", "a:b"} {
		if _, err := ParsePosition(p); err == nil {
			t.Errorf("ParsePosition(%q) succeeded", p)
		}
	}
}

// entry is an oplog entry of about n bytes.
func entry(t *testing.T, ts bson.Timestamp, n int) []byte {
	t.Helper()
	raw, err := bson.Marshal(bson.D{{Key: "ts", Value: ts}, {Key: "op", Value: "n"}, {Key: "o", Value: bson.D{{Key: "pad", Value: string(make([]byte, n))}}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBatchLimits(t *testing.T) {
	now := time.Unix(1700000000, 0)
	t.Run("entries", func(t *testing.T) {
		var b batch
		for i := range BatchEntries {
			if b.full() {
				t.Fatalf("full after %d entries", i)
			}
			b.add(entry(t, bson.Timestamp{T: 1, I: uint32(i + 1)}, 1), bson.Timestamp{T: 1, I: uint32(i + 1)}, now) // #nosec G115 -- small
		}
		if !b.full() {
			t.Fatalf("not full at %d entries", BatchEntries)
		}
		s := b.segment()
		if s.Kind != "oplog" || s.First != "1:1" || s.Last != engine.Position("1:10000") || s.Checkpoint != s.Last || s.Name != "" {
			t.Errorf("segment = %+v", s)
		}
		if b.entries != 0 || len(b.data) != 0 || b.due(now.Add(time.Hour)) {
			t.Errorf("the batch was not emptied: %+v", b)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		var b batch
		big := entry(t, bson.Timestamp{T: 2, I: 1}, 4<<20)
		for i := range 3 {
			b.add(big, bson.Timestamp{T: 2, I: uint32(i + 1)}, now) // #nosec G115 -- small
		}
		if b.full() {
			t.Fatal("full below the byte limit")
		}
		if b.fits(len(big)) {
			t.Fatal("a fourth entry fits past the byte limit")
		}
		if !b.fits(BatchBytes - len(b.data)) {
			t.Fatal("an entry up to the limit does not fit")
		}
		b.add(entry(t, bson.Timestamp{T: 2, I: 4}, BatchBytes-len(b.data)-64), bson.Timestamp{T: 2, I: 4}, now)
		if len(b.data) > BatchBytes {
			t.Fatalf("batch holds %d bytes", len(b.data))
		}
	})
	t.Run("an entry larger than a batch is stored alone", func(t *testing.T) {
		var b batch
		huge := entry(t, bson.Timestamp{T: 3, I: 1}, BatchBytes+1)
		if !b.fits(len(huge)) {
			t.Fatal("an empty batch refuses an entry")
		}
		b.add(huge, bson.Timestamp{T: 3, I: 1}, now)
		if !b.full() {
			t.Error("a batch past the byte limit is not full")
		}
	})
	t.Run("time", func(t *testing.T) {
		var b batch
		if b.due(now.Add(time.Hour)) {
			t.Error("an empty batch is due")
		}
		b.add(entry(t, bson.Timestamp{T: 4, I: 1}, 1), bson.Timestamp{T: 4, I: 1}, now)
		b.add(entry(t, bson.Timestamp{T: 4, I: 2}, 1), bson.Timestamp{T: 4, I: 2}, now.Add(9*time.Second))
		if b.due(now.Add(FlushInterval - time.Nanosecond)) {
			t.Error("due before the flush interval")
		}
		if !b.due(now.Add(FlushInterval)) {
			t.Error("not due at the flush interval")
		}
	})
}

// TestBatchIsBSON checks a batch is its entries, BSON documents one after
// another, as mongodump's oplog.bson holds them.
func TestBatchIsBSON(t *testing.T) {
	var b batch
	var want [][]byte
	for i := range 5 {
		ts := bson.Timestamp{T: 1700000000, I: uint32(i + 1)} // #nosec G115 -- small
		e := entry(t, ts, 10*i)
		want = append(want, e)
		b.add(e, ts, time.Now())
	}
	s := b.segment()
	rest := s.Data
	for i, w := range want {
		doc, r, ok := bsonDoc(rest)
		if !ok || !bytes.Equal(doc, w) {
			t.Fatalf("entry %d differs", i)
		}
		rest = r
	}
	if len(rest) != 0 {
		t.Fatalf("%d bytes after the entries", len(rest))
	}
	if s.First != "1700000000:1" || s.Last != "1700000000:5" {
		t.Errorf("segment covers %s to %s", s.First, s.Last)
	}
}

// bsonDoc splits the first BSON document off data.
func bsonDoc(data []byte) (doc, rest []byte, ok bool) {
	if len(data) < 5 {
		return nil, nil, false
	}
	n := int(int32(data[0]) | int32(data[1])<<8 | int32(data[2])<<16 | int32(data[3])<<24)
	if n < 5 || n > len(data) {
		return nil, nil, false
	}
	return data[:n], data[n:], true
}

func TestCaptureCredential(t *testing.T) {
	ref := func(user string) config.Credential { return config.Credential{User: user} }
	for _, tt := range []struct {
		name  string
		uri   string
		creds config.Credentials
		user  string
		fails bool
	}{
		{"replication user", "", config.Credentials{Backup: ref("pentym_backup"), Replication: ref("pentym_repl")}, "pentym_repl", false},
		{"falls back to the backup user", "", config.Credentials{Backup: ref("pentym_backup")}, "pentym_backup", false},
		{"client certificate", "mongodb://db/?authMechanism=MONGODB-X509&tls=true", config.Credentials{}, "", false},
		{"no user", "mongodb://db/", config.Credentials{}, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tg := engine.Target{Name: "prod-mg", Config: config.Target{Engine: "mongodb", URI: tt.uri, Credentials: tt.creds}}
			c, err := captureCred(tg)
			var me *msg.Error
			switch {
			case tt.fails && (!errors.As(err, &me) || me.Code != msg.ExitConfig):
				t.Errorf("captureCred = %v", err)
			case !tt.fails && (err != nil || c.User != tt.user):
				t.Errorf("captureCred = %+v, %v", c, err)
			}
		})
	}
}

// TestNoCheckpointNoGap checks a capture with nothing to continue from
// has no gap to report, without asking the server.
func TestNoCheckpointNoGap(t *testing.T) {
	gap, err := (&Engine{}).DetectGap(context.Background(), mgTarget("mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=1", ""), "")
	if gap != nil || err != nil {
		t.Errorf("DetectGap = %v, %v", gap, err)
	}
	if _, err := (&Engine{}).DetectGap(context.Background(), mgTarget("", ""), "not-a-position"); err == nil {
		t.Error("DetectGap accepted a position it cannot read")
	}
}

func TestStandaloneIsUnsupported(t *testing.T) {
	if !errors.Is(errStandalone, errors.ErrUnsupported) {
		t.Error("a standalone server's capture error is not ErrUnsupported, so the agent would treat it as a failure")
	}
}
