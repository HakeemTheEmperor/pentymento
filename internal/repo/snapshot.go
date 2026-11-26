// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/chunker"
	"github.com/HakeemTheEmperor/pentymento/internal/compress"
	"github.com/HakeemTheEmperor/pentymento/internal/workpool"
)

const snapshotsPrefix = "snapshots/"

func snapshotKey(id string) string { return snapshotsPrefix + id }

// Snapshot is a committed base backup (SDD 5.2). It never changes once
// written; verification results and pins are separate records.
type Snapshot struct {
	ID            string          `json:"id"`
	Target        string          `json:"target"`
	Engine        string          `json:"engine"`
	EngineVersion string          `json:"engine_version,omitempty"`
	StartPosition string          `json:"start_position,omitempty"`
	EndPosition   string          `json:"end_position,omitempty"`
	Timeline      string          `json:"timeline,omitempty"`
	Inventory     json.RawMessage `json:"inventory,omitempty"`
	Start         time.Time       `json:"start"`
	End           time.Time       `json:"end"`
	// Size is the stream's length in bytes; Chunks its number of chunks.
	Size   int64 `json:"size"`
	Chunks int64 `json:"chunks"`
	// List names the chunk-list blocks: the stream's chunk IDs, in order,
	// stored through the chunk store so a snapshot of any size is small.
	List []string `json:"list"`
	// Name names the main stream, such as "base.tar"; empty for a single
	// unnamed stream.
	Name string `json:"name,omitempty"`
	// Parts are further streams committed with the main one, each its own
	// chunk stream: a Postgres backup's tablespace archives and manifest.
	Parts []Part `json:"parts,omitempty"`
	// Method is how the engine took the backup, such as "native".
	Method string `json:"method,omitempty"`
	// SystemID identifies the database cluster or replica set.
	SystemID string `json:"system_id,omitempty"`
}

// Part is one further stream of a snapshot.
type Part struct {
	Name   string   `json:"name"`
	Size   int64    `json:"size"`
	Chunks int64    `json:"chunks"`
	List   []string `json:"list"`
}

// TotalSize is the length of every stream together.
func (s *Snapshot) TotalSize() int64 {
	n := s.Size
	for _, p := range s.Parts {
		n += p.Size
	}
	return n
}

// streams returns every stream as a part: the main one first.
func (s *Snapshot) streams() []Part {
	return append([]Part{{Name: s.Name, Size: s.Size, Chunks: s.Chunks, List: s.List}}, s.Parts...)
}

// Chunk-list blocks hold 32-byte chunk IDs back to back. A block ends
// after an ID whose first two bytes, as a big-endian number, are below 2
// (once it holds at least listMinIDs), or at listMaxIDs. Chunk IDs are
// keyed hashes, so blocks average about 32768 IDs (1 MiB) and their
// boundaries depend only on content: unchanged runs of chunks give
// unchanged blocks, which deduplicate between snapshots.
const (
	listMinIDs   = 1024
	listMaxIDs   = 131072
	listCutBelow = 2
)

func listBoundary(id chunker.ID, n int) bool {
	return n >= listMaxIDs || (n >= listMinIDs && binary.BigEndian.Uint16(id[:2]) < listCutBelow)
}

// WriteOptions configure WriteSnapshot.
type WriteOptions struct {
	// Level is the compression level.
	Level compress.Level
	// Workers bounds the goroutines hashing and compressing; 0 means
	// GOMAXPROCS.
	Workers int
	// Uploads bounds packs uploading at once; 0 means 2.
	Uploads int
	// BeforeCommit runs after the stream ends and before anything is
	// committed, such as waiting for the child process that produced the
	// stream. An error abandons the snapshot.
	BeforeCommit func() error
	// OnChunk, if set, is called for each chunk with its size and whether
	// it was new, for progress reporting.
	OnChunk func(size int, isNew bool)
}

// WriteStats describe a written snapshot.
type WriteStats struct {
	Bytes     int64 // stream length
	Chunks    int64
	NewChunks int64
	NewBytes  int64 // stored bytes of new chunks, compressed and encrypted
	Packs     int
}

// DedupRatio is the stream's bytes per stored byte.
func (s WriteStats) DedupRatio() float64 {
	if s.NewBytes == 0 {
		return 0
	}
	return float64(s.Bytes) / float64(s.NewBytes)
}

// WriteSnapshot streams src through the chunk store and commits a snapshot
// described by meta (SDD 6.1). Memory stays bounded whatever src's length.
// Index files are written before the snapshot object, and nothing is
// committed unless src ends cleanly and BeforeCommit succeeds; packs from
// an abandoned write are removed by a later prune.
//
// idx is the repository's index (LoadIndex); it gains the new chunks.
func (r *Repository) WriteSnapshot(ctx context.Context, idx *Index, src io.Reader, meta Snapshot, opts WriteOptions) (*Snapshot, WriteStats, error) {
	sw, err := r.NewSnapshotWriter(ctx, idx, meta, opts)
	if err != nil {
		return nil, WriteStats{}, err
	}
	defer sw.Close()
	if err := sw.WritePart(meta.Name, src); err != nil {
		return nil, sw.w.stats, err
	}
	return sw.Commit()
}

// SnapshotWriter writes a snapshot of one or more streams (parts), such as
// a Postgres backup's base.tar, tablespace archives and manifest. Each part
// is its own chunk stream; the first is the snapshot's main stream. Nothing
// is visible until Commit, which commits every part together. Close
// releases the writer whether or not it committed.
type SnapshotWriter struct {
	r     *Repository
	w     *snapWriter
	comp  *compress.Compressor
	meta  Snapshot
	parts []Part
	ctx   context.Context

	committed, closed bool
}

// NewSnapshotWriter starts a snapshot described by meta. idx is the
// repository's index (LoadIndex); it gains the new chunks.
func (r *Repository) NewSnapshotWriter(ctx context.Context, idx *Index, meta Snapshot, opts WriteOptions) (*SnapshotWriter, error) {
	if !r.unlocked {
		return nil, ErrLocked
	}
	comp, err := compress.New(opts.Level, chunker.MaxSize)
	if err != nil {
		return nil, err
	}
	uploads := opts.Uploads
	if uploads <= 0 {
		uploads = 2
	}
	w := &snapWriter{
		ctx: ctx, r: r, idx: idx, comp: comp, opts: opts,
		hasher: chunker.NewHasher(r.chunkID.Array()),
		pack:   r.newPack(),
		inPack: map[chunker.ID]bool{},
		upload: make(chan struct{}, uploads),
	}
	meta.ID = randomHex(16)
	meta.Start = r.now().UTC()
	meta.Parts = nil
	return &SnapshotWriter{r: r, w: w, comp: comp, meta: meta, ctx: ctx}, nil
}

// WritePart streams one part through the chunk store. Parts are written
// one after another; a part that fails leaves the snapshot uncommittable.
func (sw *SnapshotWriter) WritePart(name string, src io.Reader) error {
	if sw.committed || sw.closed {
		return errors.New("snapshot writer already finished")
	}
	for _, p := range sw.parts {
		if p.Name == name {
			return fmt.Errorf("snapshot part %q written twice", name)
		}
	}
	w := sw.w
	if err := w.err(); err != nil {
		return err
	}
	bytes0, chunks0 := w.stats.Bytes, w.stats.Chunks
	w.listIDs, w.listBuf, w.listN = nil, w.listBuf[:0], 0
	ch := chunker.New(src, chunker.NewTable(sw.r.chunkID.Array()))
	workers := w.opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	err := workpool.Run(sw.ctx, workers,
		func() ([]byte, bool, error) {
			c, err := ch.Next()
			if errors.Is(err, io.EOF) {
				return nil, false, nil
			}
			if err != nil {
				return nil, false, err
			}
			return append([]byte(nil), c...), true, nil
		},
		w.prepare,
		w.addData,
	)
	if err == nil {
		err = w.flushList()
	}
	if err != nil {
		w.setErr(err)
		return err
	}
	list := w.listIDs
	if list == nil {
		list = []string{}
	}
	sw.parts = append(sw.parts, Part{Name: name, Size: w.stats.Bytes - bytes0, Chunks: w.stats.Chunks - chunks0, List: list})
	return nil
}

// Describe updates the snapshot record before Commit, with what is known
// only once the stream has ended, such as its end position. The ID, start
// time and streams cannot be changed.
func (sw *SnapshotWriter) Describe(fn func(*Snapshot)) {
	m := sw.meta
	fn(&m)
	m.ID, m.Start, m.Parts = sw.meta.ID, sw.meta.Start, nil
	sw.meta = m
}

// Stats returns what has been written so far.
func (sw *SnapshotWriter) Stats() WriteStats { return sw.w.stats }

// Commit finishes the last pack, waits for the uploads, runs BeforeCommit,
// then writes the index files and the snapshot object, in that order.
func (sw *SnapshotWriter) Commit() (*Snapshot, WriteStats, error) {
	w := sw.w
	if sw.committed || sw.closed {
		return nil, w.stats, errors.New("snapshot writer already finished")
	}
	if len(sw.parts) == 0 {
		return nil, w.stats, errors.New("a snapshot needs at least one part")
	}
	err := w.finish(sw.ctx)
	if werr := w.waitUploads(); err == nil {
		err = werr
	}
	if err != nil {
		return nil, w.stats, err
	}
	if w.opts.BeforeCommit != nil {
		if err := w.opts.BeforeCommit(); err != nil {
			return nil, w.stats, fmt.Errorf("snapshot abandoned: %w", err)
		}
	}
	// Commit: index files first, then the snapshot.
	if _, err := sw.r.writeIndex(sw.ctx, sw.comp, w.pending); err != nil {
		return nil, w.stats, err
	}
	meta := sw.meta
	meta.End = sw.r.now().UTC()
	main := sw.parts[0]
	meta.Name, meta.Size, meta.Chunks, meta.List = main.Name, main.Size, main.Chunks, main.List
	if len(sw.parts) > 1 {
		meta.Parts = append([]Part(nil), sw.parts[1:]...)
	}
	if err := sw.r.putMeta(sw.ctx, snapshotKey(meta.ID), meta, backend.PutOptions{IfNotExists: true}); err != nil {
		return nil, w.stats, err
	}
	sw.committed = true
	return &meta, w.stats, nil
}

// Close waits for uploads still running and releases the writer. After a
// failed or abandoned write nothing is committed; the uploaded packs are
// removed by a later prune.
func (sw *SnapshotWriter) Close() {
	if sw.closed {
		return
	}
	sw.closed = true
	_ = sw.w.waitUploads()
	_ = sw.comp.Close()
}

// prepared is a chunk after hashing and, if new, compression.
type prepared struct {
	id         chunker.ID
	raw        []byte
	compressed []byte // nil when the chunk was already stored when hashed
}

type snapWriter struct {
	ctx    context.Context
	r      *Repository
	idx    *Index
	comp   *compress.Compressor
	hasher *chunker.Hasher
	opts   WriteOptions
	stats  WriteStats

	pack    *packBuilder
	inPack  map[chunker.ID]bool // chunks in the pack being built
	pending []packRecord        // packs not yet in an index file

	listBuf []byte // the current chunk-list block
	listN   int
	listIDs []string

	upload chan struct{} // bounds concurrent uploads
	wg     sync.WaitGroup
	errMu  sync.Mutex
	upErr  error
}

// prepare runs on the worker pool: hash, and compress only if new.
func (w *snapWriter) prepare(_ context.Context, c []byte) (prepared, error) {
	p := prepared{id: w.hasher.Sum(c), raw: c}
	if !w.idx.Has(p.id) {
		p.compressed = w.comp.Compress(nil, c)
	}
	return p, nil
}

// addData runs on one goroutine, in stream order.
func (w *snapWriter) addData(p prepared) error {
	w.stats.Bytes += int64(len(p.raw))
	w.stats.Chunks++
	isNew := w.store(p)
	if w.opts.OnChunk != nil {
		w.opts.OnChunk(len(p.raw), isNew)
	}
	w.listBuf = append(w.listBuf, p.id[:]...)
	w.listN++
	if listBoundary(p.id, w.listN) {
		return w.flushList()
	}
	return w.err()
}

// store adds a chunk to the current pack unless it is already stored, and
// reports whether it was new.
func (w *snapWriter) store(p prepared) bool {
	if w.inPack[p.id] || w.idx.Has(p.id) {
		return false
	}
	if p.compressed == nil {
		// Never skip storing a chunk the index lacks, whatever prepare saw.
		p.compressed = w.comp.Compress(nil, p.raw)
	}
	stored := w.pack.add(p.id, p.compressed, len(p.raw))
	w.inPack[p.id] = true
	w.stats.NewChunks++
	w.stats.NewBytes += int64(stored)
	if w.pack.size() >= PackSize {
		w.flushPack()
	}
	return true
}

// flushList stores the current chunk-list block as a chunk.
func (w *snapWriter) flushList() error {
	if w.listN == 0 {
		return w.err()
	}
	id := w.hasher.Sum(w.listBuf)
	if !w.inPack[id] && !w.idx.Has(id) {
		w.store(prepared{id: id, raw: w.listBuf, compressed: w.comp.Compress(nil, w.listBuf)})
	}
	w.listIDs = append(w.listIDs, id.String())
	w.listBuf, w.listN = w.listBuf[:0], 0
	return w.err()
}

// flushPack closes the current pack, uploads it in the background and
// records it in the index; every IndexFlushPacks packs it writes an index
// file.
func (w *snapWriter) flushPack() {
	if len(w.pack.entries) == 0 {
		return
	}
	data, id, rec, err := w.pack.finish()
	if err != nil {
		w.setErr(err)
		return
	}
	w.stats.Packs++
	w.upload <- struct{}{}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() { <-w.upload }()
		err := w.r.be.Put(w.ctx, packKey(id), bytes.NewReader(data), backend.PutOptions{IfNotExists: true})
		if err != nil && !errors.Is(err, backend.ErrExists) {
			w.setErr(fmt.Errorf("upload pack %s: %w", id, err))
		}
	}()
	w.idx.add(rec)
	w.pending = append(w.pending, rec)
	w.pack = w.r.newPack()
	clear(w.inPack)
	if len(w.pending) >= IndexFlushPacks {
		if err := w.waitUploads(); err != nil {
			return
		}
		if _, err := w.r.writeIndex(w.ctx, w.comp, w.pending); err != nil {
			w.setErr(err)
			return
		}
		w.pending = nil
	}
}

// finish stores the last chunk-list block and pack.
func (w *snapWriter) finish(_ context.Context) error {
	if err := w.flushList(); err != nil {
		return err
	}
	w.flushPack()
	return w.err()
}

func (w *snapWriter) waitUploads() error {
	w.wg.Wait()
	return w.err()
}

func (w *snapWriter) setErr(err error) {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	if w.upErr == nil {
		w.upErr = err
	}
}

func (w *snapWriter) err() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.upErr
}

// Snapshots returns every snapshot, in key order.
func (r *Repository) Snapshots(ctx context.Context) ([]Snapshot, error) {
	keys, err := r.listKeys(ctx, snapshotsPrefix)
	if err != nil {
		return nil, err
	}
	if r.cache != nil {
		r.cache.retain(snapshotsPrefix, keys)
	}
	out := make([]Snapshot, 0, len(keys))
	for _, k := range keys {
		var s Snapshot
		if err := r.getMetaCached(ctx, k, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// LoadSnapshot reads one snapshot.
func (r *Repository) LoadSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	if _, err := hex.DecodeString(id); err != nil || len(id) != 32 {
		return nil, fmt.Errorf("snapshot ID %q is malformed", id)
	}
	var s Snapshot
	if err := r.getMetaCached(ctx, snapshotKey(id), &s); err != nil {
		return nil, err
	}
	return &s, nil
}
