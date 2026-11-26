// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// archiveBuilder writes a mongodump archive the way mongodump does: a
// magic number, a prelude block, then namespace blocks that may
// interleave, each closed by an EOF block.
type archiveBuilder struct{ buf bytes.Buffer }

func newArchive(namespaces ...[2]string) *archiveBuilder {
	a := &archiveBuilder{}
	_ = binary.Write(&a.buf, binary.LittleEndian, archiveMagic)
	a.doc(bson.D{{Key: "concurrent_collections", Value: int32(4)}, {Key: "version", Value: "0.1"},
		{Key: "server_version", Value: "8.0.4"}, {Key: "tool_version", Value: "100.10.0"}})
	for _, ns := range namespaces {
		a.doc(bson.D{{Key: "db", Value: ns[0]}, {Key: "collection", Value: ns[1]}, {Key: "metadata", Value: "{}"}})
	}
	a.term()
	return a
}

func (a *archiveBuilder) doc(d any) {
	b, err := bson.Marshal(d)
	if err != nil {
		panic(err)
	}
	a.buf.Write(b)
}

func (a *archiveBuilder) term() { _ = binary.Write(&a.buf, binary.LittleEndian, terminator) }

// block writes documents of one namespace.
func (a *archiveBuilder) block(db, coll string, docs ...any) *archiveBuilder {
	a.doc(bson.D{{Key: "db", Value: db}, {Key: "collection", Value: coll}, {Key: "EOF", Value: false}, {Key: "CRC", Value: int64(0)}})
	for _, d := range docs {
		a.doc(d)
	}
	a.term()
	return a
}

// eof closes a namespace.
func (a *archiveBuilder) eof(db, coll string) *archiveBuilder {
	a.doc(bson.D{{Key: "db", Value: db}, {Key: "collection", Value: coll}, {Key: "EOF", Value: true}, {Key: "CRC", Value: int64(42)}})
	a.term()
	return a
}

func (a *archiveBuilder) bytes() []byte { return a.buf.Bytes() }

func oplogEntry(t, i uint32) bson.D {
	return bson.D{{Key: "ts", Value: bson.Timestamp{T: t, I: i}}, {Key: "op", Value: "i"}, {Key: "ns", Value: "app.orders"}}
}

// sampleArchive is a replica-set dump: two interleaved collections, an
// empty one, and the oplog.
func sampleArchive() []byte {
	big := strings.Repeat("x", 3000)
	return newArchive([2]string{"app", "orders"}, [2]string{"app", "users"}, [2]string{"app", "empty"}).
		block("app", "orders", bson.D{{Key: "_id", Value: 1}, {Key: "note", Value: big}}).
		block("app", "users", bson.D{{Key: "_id", Value: "ada"}}).
		block("app", "orders", bson.D{{Key: "_id", Value: 2}}).
		eof("app", "users").eof("app", "orders").eof("app", "empty").
		block("", "oplog", oplogEntry(1732600000, 1), oplogEntry(1732600000, 2), oplogEntry(1732600005, 1)).
		eof("", "oplog").bytes()
}

func TestReadArchive(t *testing.T) {
	sum, err := ReadArchive(bytes.NewReader(sampleArchive()))
	if err != nil {
		t.Fatal(err)
	}
	if sum.ServerVersion != "8.0.4" || sum.ToolVersion != "100.10.0" || sum.Namespaces != 4 || sum.OplogEntries != 3 ||
		sum.OplogFirst != (bson.Timestamp{T: 1732600000, I: 1}) || sum.OplogLast != (bson.Timestamp{T: 1732600005, I: 1}) {
		t.Errorf("summary = %+v", sum)
	}
	if p := Position(sum.OplogLast); p != "1732600005:1" {
		t.Errorf("position = %s", p)
	}

	// A standalone dump has no oplog.
	plain := newArchive([2]string{"app", "orders"}).block("app", "orders", bson.D{{Key: "_id", Value: 1}}).eof("app", "orders").bytes()
	if sum, err := ReadArchive(bytes.NewReader(plain)); err != nil || sum.OplogEntries != 0 || sum.Namespaces != 1 {
		t.Errorf("without an oplog: %+v, %v", sum, err)
	}
	// A dump of an empty server is a prelude alone.
	if sum, err := ReadArchive(bytes.NewReader(newArchive().bytes())); err != nil || sum.Namespaces != 0 {
		t.Errorf("empty: %+v, %v", sum, err)
	}
}

// oneByteReader returns one byte per Read, as a slow pipe might.
type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) { return o.r.Read(p[:1]) }

func TestReadArchiveShortReads(t *testing.T) {
	sum, err := ReadArchive(oneByteReader{bytes.NewReader(sampleArchive())})
	if err != nil || sum.OplogEntries != 3 {
		t.Errorf("%+v, %v", sum, err)
	}
}

// TestReadArchiveTruncated cuts the archive at every length: each cut is
// refused, because it ends inside a document or block, or before a
// namespace that had documents is closed.
func TestReadArchiveTruncated(t *testing.T) {
	full := sampleArchive()
	for n := 0; n < len(full); n++ {
		if _, err := ReadArchive(bytes.NewReader(full[:n])); err == nil {
			// Cuts right after a closing EOF block, with nothing open,
			// are well-formed archives of fewer namespaces.
			if !wellFormedPrefix(full, n) {
				t.Fatalf("a cut at %d of %d bytes was accepted", n, len(full))
			}
		}
	}
}

// wellFormedPrefix reports whether full[:n] ends at a block boundary with
// no namespace open: after the prelude, or after the EOF blocks that
// close the collections, or after the oplog's EOF block.
func wellFormedPrefix(full []byte, n int) bool {
	a := newArchive([2]string{"app", "orders"}, [2]string{"app", "users"}, [2]string{"app", "empty"})
	ends := []int{a.buf.Len()}
	big := strings.Repeat("x", 3000)
	a.block("app", "orders", bson.D{{Key: "_id", Value: 1}, {Key: "note", Value: big}}).
		block("app", "users", bson.D{{Key: "_id", Value: "ada"}}).
		block("app", "orders", bson.D{{Key: "_id", Value: 2}}).
		eof("app", "users").eof("app", "orders")
	ends = append(ends, a.buf.Len())
	a.eof("app", "empty")
	ends = append(ends, a.buf.Len())
	if !bytes.HasPrefix(full, a.bytes()) {
		panic("the prefix builder drifted from sampleArchive")
	}
	for _, e := range ends {
		if n == e {
			return true
		}
	}
	return false
}

func TestReadArchiveMalformed(t *testing.T) {
	good := sampleArchive()
	withSize := func(size uint32) []byte {
		b := append([]byte(nil), newArchive().bytes()...)
		return binary.LittleEndian.AppendUint32(b, size)
	}
	noTS := newArchive().block("", "oplog", bson.D{{Key: "op", Value: "n"}}).eof("", "oplog").bytes()
	headerless := newArchive().bytes()
	headerless = binary.LittleEndian.AppendUint32(headerless, terminator)
	badZero := append([]byte(nil), good...)
	// The prelude header's last byte must be zero.
	preludeLen := binary.LittleEndian.Uint32(badZero[4:8])
	badZero[4+preludeLen-1] = 1
	for name, tt := range map[string]struct {
		data []byte
		want string
	}{
		"empty":         {nil, "empty or shorter than its magic number"},
		"wrong magic":   {append([]byte{1, 2, 3, 4}, good[4:]...), "wrong magic number"},
		"oversized":     {withSize(maxDocument + 1), "claims 16793601 bytes"},
		"undersized":    {withSize(4), "claims 4 bytes"},
		"no timestamp":  {noTS, "an oplog entry has no timestamp"},
		"headerless":    {headerless, "has no header"},
		"no zero byte":  {badZero, "does not end in a zero byte"},
		"no terminator": {good[:len(good)-4], "unexpected EOF"},
	} {
		_, err := ReadArchive(bytes.NewReader(tt.data))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
	if _, err := ReadArchive(bytes.NewReader(good[:len(good)-4])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a missing final terminator: %v", err)
	}
}
