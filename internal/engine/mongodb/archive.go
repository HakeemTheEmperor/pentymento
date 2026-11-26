// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The mongodump archive format (MongoDB Database Tools): a magic number,
// then blocks. Each block is a header document, body documents and a
// terminator. The first block is the prelude, whose body describes the
// collections; every later block holds documents of one namespace, and a
// header with EOF set closes a namespace. Collections are interleaved
// when mongodump dumps several at once.
const (
	archiveMagic uint32 = 0x8199e26d
	terminator   uint32 = 0xffffffff
	// maxDocument bounds one document: MongoDB's 16 MiB limit plus the
	// headroom the server allows internally.
	maxDocument = 16<<20 + 16<<10
)

// ArchiveSummary is what reading a mongodump archive found.
type ArchiveSummary struct {
	// ServerVersion and ToolVersion are from the prelude.
	ServerVersion, ToolVersion string
	// Namespaces counts the namespaces the archive closed.
	Namespaces int
	// OplogEntries counts the oplog entries (mongodump --oplog); First
	// and Last are the first and last entries' timestamps.
	OplogEntries          int64
	OplogFirst, OplogLast bson.Timestamp
}

// archiveReader reads an archive one document at a time, reusing its
// buffer, so memory stays bounded whatever the archive's size.
type archiveReader struct {
	r   *bufio.Reader
	buf []byte
	// off is the offset of the next byte, for error messages.
	off int64
}

// next reads a document or a terminator. At a clean end of input it
// returns io.EOF; an end inside a document is io.ErrUnexpectedEOF.
func (a *archiveReader) next() (doc bson.Raw, term bool, err error) {
	var head [4]byte
	n, err := io.ReadFull(a.r, head[:])
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			return nil, false, io.EOF
		}
		return nil, false, io.ErrUnexpectedEOF
	}
	at := a.off
	a.off += 4
	size := binary.LittleEndian.Uint32(head[:])
	if size == terminator {
		return nil, true, nil
	}
	if size < 5 || size > maxDocument {
		return nil, false, fmt.Errorf("a document at offset %d claims %d bytes", at, size)
	}
	if cap(a.buf) < int(size) {
		a.buf = make([]byte, size)
	}
	a.buf = a.buf[:size]
	copy(a.buf, head[:])
	if _, err := io.ReadFull(a.r, a.buf[4:]); err != nil {
		return nil, false, io.ErrUnexpectedEOF
	}
	a.off += int64(size) - 4
	if a.buf[size-1] != 0 {
		return nil, false, fmt.Errorf("the document at offset %d does not end in a zero byte", at)
	}
	return bson.Raw(a.buf), false, nil
}

// header reads a block's header document; io.EOF means the archive ended
// cleanly between blocks.
func (a *archiveReader) header() (bson.Raw, error) {
	doc, term, err := a.next()
	if err != nil {
		return nil, err
	}
	if term {
		return nil, fmt.Errorf("a block at offset %d has no header", a.off-4)
	}
	if err := doc.Validate(); err != nil {
		return nil, fmt.Errorf("a block header is not valid BSON: %w", err)
	}
	return doc, nil
}

// body calls fn with each body document of a block, up to its terminator.
func (a *archiveReader) body(fn func(bson.Raw) error) error {
	for {
		doc, term, err := a.next()
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		if err != nil || term {
			return err
		}
		if err := fn(doc); err != nil {
			return err
		}
	}
}

func lookupString(doc bson.Raw, key string) string {
	s, _ := doc.Lookup(key).StringValueOK()
	return s
}

// ReadArchive reads a whole mongodump archive and summarizes it. It checks
// the archive's structure (magic number, blocks, documents and that every
// namespace that had documents was closed) and the oplog entries'
// timestamps; other documents are only framed, not decoded. An error
// means the archive is not one mongodump finished writing.
func ReadArchive(r io.Reader) (*ArchiveSummary, error) {
	a := &archiveReader{r: bufio.NewReaderSize(r, 256<<10)}
	var magic [4]byte
	if _, err := io.ReadFull(a.r, magic[:]); err != nil {
		return nil, errors.New("the archive is empty or shorter than its magic number")
	}
	a.off = 4
	if binary.LittleEndian.Uint32(magic[:]) != archiveMagic {
		return nil, errors.New("not a mongodump archive: wrong magic number")
	}
	prelude, err := a.header()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("the prelude: %w", err)
	}
	sum := &ArchiveSummary{ServerVersion: lookupString(prelude, "server_version"), ToolVersion: lookupString(prelude, "tool_version")}
	if err := a.body(func(bson.Raw) error { return nil }); err != nil {
		return nil, fmt.Errorf("the prelude: %w", err)
	}
	open := map[string]bool{}
	for {
		h, err := a.header()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		db, coll := lookupString(h, "db"), lookupString(h, "collection")
		ns := db + "." + coll
		if eof, _ := h.Lookup("EOF").BooleanOK(); eof {
			delete(open, ns)
			sum.Namespaces++
		} else {
			open[ns] = true
		}
		oplog := db == "" && coll == "oplog"
		err = a.body(func(doc bson.Raw) error {
			if !oplog {
				return nil
			}
			if err := doc.Validate(); err != nil {
				return fmt.Errorf("an oplog entry is not valid BSON: %w", err)
			}
			t, i, ok := doc.Lookup("ts").TimestampOK()
			if !ok {
				return errors.New("an oplog entry has no timestamp")
			}
			ts := bson.Timestamp{T: t, I: i}
			if sum.OplogEntries == 0 {
				sum.OplogFirst = ts
			}
			sum.OplogLast = ts
			sum.OplogEntries++
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("namespace %s: %w", ns, err)
		}
	}
	if len(open) > 0 {
		names := make([]string, 0, len(open))
		for ns := range open {
			names = append(names, ns)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("the archive ends before namespace %s is complete", names[0])
	}
	return sum, nil
}
