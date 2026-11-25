// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"

	"github.com/zeebo/blake3"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/chunker"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
)

// DefaultCheckSample is the share of packs and log segments check reads in
// full unless told otherwise.
const DefaultCheckSample = 0.05

// CheckOptions choose what Check covers.
type CheckOptions struct {
	// Target limits the check to one target's snapshots, the packs they
	// use and its log segments; empty checks the whole repository.
	Target string
	// Sample is the share of packs and log segments read in full and
	// verified chunk by chunk, above 0 and at most 1; at least one of each
	// is read. Full reads them all.
	Sample float64
	Full   bool
	// Rand picks the sample; nil picks at random.
	Rand *rand.Rand
}

// Check steps, in order.
const (
	CheckSlots     = "slots"
	CheckIndex     = "index"
	CheckPacks     = "packs"
	CheckSnapshots = "snapshots"
	CheckData      = "data"
	CheckSegments  = "segments"
)

// Problem severities.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Problem kinds.
const (
	ProblemMissing   = "missing"   // a referenced object does not exist
	ProblemCorrupt   = "corrupt"   // an object fails a hash, tag or format check
	ProblemUnindexed = "unindexed" // a pack no index file lists
	ProblemStray     = "stray"     // an object that should not be where it is
	ProblemSlots     = "slots"     // only one way to unlock the repository
)

// Problem is one thing check found.
type Problem struct {
	// Step is the part of the check that found it: slots, index, packs,
	// snapshots, data or segments.
	Step     string `json:"step"`
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Object   string `json:"object"`
	Detail   string `json:"detail"`
}

// CheckReport is what Check found. Corruption is reported as problems,
// not as Check's error.
type CheckReport struct {
	Slots          int       `json:"slots"`
	FullSlots      int       `json:"full_slots"`
	IndexFiles     int       `json:"index_files"`
	IndexedPacks   int       `json:"indexed_packs"`
	IndexedChunks  int       `json:"indexed_chunks"`
	PacksChecked   int       `json:"packs_checked"`
	Snapshots      int       `json:"snapshots"`
	SnapshotChunks int64     `json:"snapshot_chunks"`
	PacksRead      int       `json:"packs_read"`
	ChunksVerified int       `json:"chunks_verified"`
	Segments       int       `json:"segments"`
	SegmentsRead   int       `json:"segments_read"`
	Problems       []Problem `json:"problems"`
}

// Errors counts the problems of error severity.
func (c *CheckReport) Errors() int {
	n := 0
	for _, p := range c.Problems {
		if p.Severity == SeverityError {
			n++
		}
	}
	return n
}

func (c *checker) add(severity, kind, object, detail string) {
	c.rep.Problems = append(c.rep.Problems, Problem{Step: c.step, Severity: severity, Kind: kind, Object: object, Detail: detail})
}

// checker holds one check's state.
type checker struct {
	r       *Repository
	opts    CheckOptions
	rep     *CheckReport
	idx     *Index
	entries map[PackID][]Entry // each indexed pack's entries, from the index
	present map[PackID]int64   // packs in storage, with their sizes
	used    map[PackID]bool    // packs the checked snapshots use
	step    string
}

// Check verifies the repository's integrity at verify level L0 (SDD 6.5):
// key slots and keys open, index files match their names and decrypt,
// every pack the index lists exists and is long enough, every chunk a
// snapshot references is indexed, and a sample of packs (or all, with
// Full) is read in full, each chunk decrypted and checked against its ID.
// Log segments are sampled the same way. It needs a full slot; the caller
// holds a shared lock so prune cannot delete what is being checked.
func (r *Repository) Check(ctx context.Context, opts CheckOptions) (*CheckReport, error) {
	if !r.CanRead() {
		return nil, ErrNoPrivateKey
	}
	if opts.Sample <= 0 || opts.Sample > 1 {
		opts.Sample = DefaultCheckSample
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) // #nosec G404 -- sampling, not security
	}
	c := &checker{
		r: r, opts: opts, rep: &CheckReport{Problems: []Problem{}}, idx: newIndex(),
		entries: map[PackID][]Entry{}, present: map[PackID]int64{}, used: map[PackID]bool{},
	}
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{CheckSlots, c.slots}, {CheckIndex, c.index}, {CheckPacks, c.packs},
		{CheckSnapshots, c.snapshots}, {CheckData, c.readPacks}, {CheckSegments, c.segments},
	}
	for _, s := range steps {
		c.step = s.name
		if err := s.run(ctx); err != nil {
			return c.rep, err
		}
	}
	return c.rep, nil
}

// slots checks every slot parses and counts the full ones.
func (c *checker) slots(ctx context.Context) error {
	if c.r.cfg.Insecure() {
		return nil
	}
	keys, err := c.r.listKeys(ctx, slotPrefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		b, err := readAll(ctx, c.r.be, k, 64<<10)
		if err != nil {
			return err
		}
		s, err := crypto.ParseSlot(b)
		if err == nil && slotKey(s.ID) != k {
			err = fmt.Errorf("it holds slot %s", s.ID)
		}
		if err != nil {
			c.add(SeverityError, ProblemCorrupt, k, "key slot is unreadable: "+err.Error())
			continue
		}
		c.rep.Slots++
		if s.Scope == crypto.ScopeFull {
			c.rep.FullSlots++
		}
	}
	if c.rep.FullSlots < 2 {
		c.add(SeverityWarning, ProblemSlots, slotPrefix,
			fmt.Sprintf("only %d full key slot; losing it would make every backup unreadable", c.rep.FullSlots))
	}
	for _, k := range c.r.badKeys {
		c.add(SeverityError, ProblemCorrupt, k, "previous private key does not open; data wrapped to it cannot be read")
	}
	return nil
}

// index reads every index file.
func (c *checker) index(ctx context.Context) error {
	err := c.r.eachIndexFile(ctx, func(key string, packs []packRecord, err error) error {
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				return err
			}
			c.add(SeverityError, ProblemCorrupt, key, "index file is unreadable: "+err.Error())
			return nil
		}
		c.rep.IndexFiles++
		for _, p := range packs {
			c.idx.add(p)
			if _, ok := c.entries[p.id]; !ok {
				c.entries[p.id] = p.entries
			}
		}
		return nil
	})
	c.rep.IndexedPacks, c.rep.IndexedChunks = c.idx.Packs(), c.idx.Len()
	return err
}

// packs checks every indexed pack exists and is long enough for its
// chunks; unindexed packs and stray objects are warnings. With a target,
// only the target's packs are checked, after its snapshots.
func (c *checker) packs(ctx context.Context) error {
	objs, strays, err := c.r.listPacks(ctx)
	if err != nil {
		return err
	}
	for _, p := range objs {
		c.present[p.id] = p.size
	}
	if c.opts.Target != "" {
		return nil
	}
	for _, k := range strays {
		c.add(SeverityWarning, ProblemStray, k, "object under data/ is not named like a pack")
	}
	for _, p := range objs {
		if _, ok := c.entries[p.id]; !ok {
			c.add(SeverityWarning, ProblemUnindexed, p.key,
				"pack is in no index file; pentym repo rebuild-index adds it, prune removes it if unused")
		}
	}
	for _, id := range sortedPacks(c.entries) {
		c.checkPresent(id)
	}
	return nil
}

func (c *checker) checkPresent(id PackID) {
	c.rep.PacksChecked++
	size, ok := c.present[id]
	if !ok {
		c.add(SeverityError, ProblemMissing, packKey(id),
			fmt.Sprintf("pack is missing; %d indexed chunks are lost", len(c.entries[id])))
		return
	}
	for _, e := range c.entries[id] {
		if int64(e.Offset)+int64(e.Length) > size-packFooterSize {
			c.add(SeverityError, ProblemCorrupt, packKey(id),
				fmt.Sprintf("pack is %d bytes, too short for chunk %s at %d+%d", size, e.ID, e.Offset, e.Length))
			return
		}
	}
}

// snapshots checks every snapshot (or the target's) decrypts, its chunk
// lists read back, and every chunk it references is indexed.
func (c *checker) snapshots(ctx context.Context) error {
	keys, err := c.r.listKeys(ctx, snapshotsPrefix)
	if err != nil {
		return err
	}
	cr, err := c.r.newChunkReader(c.idx)
	if err != nil {
		return err
	}
	defer cr.close()
	for _, k := range keys {
		var s Snapshot
		if err := c.r.getMeta(ctx, k, &s); err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				continue // forgotten meanwhile
			}
			c.add(SeverityError, ProblemCorrupt, k, "snapshot is unreadable: "+err.Error())
			continue
		}
		if c.opts.Target != "" && s.Target != c.opts.Target {
			continue
		}
		c.rep.Snapshots++
		if err := c.snapshot(ctx, cr, k, &s); err != nil {
			return err
		}
	}
	if c.opts.Target != "" {
		c.step = CheckPacks // the target's packs are known only now
		for _, id := range sortedPacks(c.used) {
			c.checkPresent(id)
		}
	}
	return nil
}

func (c *checker) snapshot(ctx context.Context, cr *chunkReader, key string, s *Snapshot) error {
	var chunks int64
	missing := 0
	for _, lid := range s.List {
		id, err := chunker.ParseID(lid)
		if err != nil {
			c.add(SeverityError, ProblemCorrupt, key, "chunk list ID is malformed: "+err.Error())
			return nil
		}
		loc, _, ok := c.idx.Lookup(id)
		if !ok {
			c.add(SeverityError, ProblemMissing, key, fmt.Sprintf("chunk list block %s is not in the index", id))
			return nil
		}
		c.used[loc.Pack] = true
		block, err := cr.read(ctx, id)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, backend.ErrNotFound) {
				return err
			}
			c.add(SeverityError, ProblemCorrupt, key, fmt.Sprintf("chunk list block %s is unreadable: %v", id, err))
			return nil
		}
		if len(block)%32 != 0 {
			c.add(SeverityError, ProblemCorrupt, key, fmt.Sprintf("chunk list block %s has a partial ID", id))
			return nil
		}
		for off := 0; off < len(block); off += 32 {
			var cid chunker.ID
			copy(cid[:], block[off:])
			chunks++
			loc, _, ok := c.idx.Lookup(cid)
			if !ok {
				missing++
				continue
			}
			c.used[loc.Pack] = true
		}
	}
	c.rep.SnapshotChunks += chunks
	switch {
	case missing > 0:
		c.add(SeverityError, ProblemMissing, key, fmt.Sprintf("%d of %d chunks are not in the index", missing, chunks))
	case chunks != s.Chunks:
		c.add(SeverityError, ProblemCorrupt, key, fmt.Sprintf("chunk lists hold %d chunks, the snapshot records %d", chunks, s.Chunks))
	}
	return nil
}

// sampleOf picks the share of items to read in full.
func (c *checker) sampleOf(n int) []int {
	if c.opts.Full {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	k := min(n, max(1, int(math.Ceil(float64(n)*c.opts.Sample))))
	if n == 0 {
		k = 0
	}
	pick := c.opts.Rand.Perm(n)[:k]
	sort.Ints(pick)
	return pick
}

// readPacks reads the sampled packs in full and verifies them.
func (c *checker) readPacks(ctx context.Context) error {
	var candidates []PackID
	src := c.entries
	if c.opts.Target != "" {
		src = map[PackID][]Entry{}
		for id := range c.used {
			src[id] = c.entries[id]
		}
	}
	for _, id := range sortedPacks(src) {
		if _, ok := c.present[id]; ok {
			candidates = append(candidates, id)
		}
	}
	cr, err := c.r.newChunkReader(c.idx)
	if err != nil {
		return err
	}
	defer cr.close()
	for _, i := range c.sampleOf(len(candidates)) {
		if err := c.readPack(ctx, cr, candidates[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *checker) readPack(ctx context.Context, cr *chunkReader, id PackID) error {
	key := packKey(id)
	data, err := readAll(ctx, c.r.be, key, packCapacity+trailerMaxBytes+packFooterSize)
	if errors.Is(err, backend.ErrNotFound) {
		return nil // deleted meanwhile; prune holds the exclusive lock, so this is rare
	}
	if err != nil {
		return err
	}
	c.rep.PacksRead++
	if PackID(blake3.Sum256(data)) != id {
		c.add(SeverityError, ProblemCorrupt, key, "pack content does not match its name")
		return nil
	}
	t, err := c.r.readTrailer(data)
	if err != nil {
		c.add(SeverityError, ProblemCorrupt, key, "pack trailer is unreadable: "+err.Error())
		return nil
	}
	if !sameEntries(t.entries, c.entries[id]) {
		c.add(SeverityError, ProblemCorrupt, key, "pack trailer does not match the index")
	}
	if len(c.entries[id]) == 0 {
		return nil
	}
	wrapped := c.idx.keys[id]
	bad := 0
	for _, e := range c.entries[id] {
		if int64(e.Offset)+int64(e.Length) > int64(len(data)) {
			bad++
			continue
		}
		loc := Location{Pack: id, Offset: e.Offset, Length: e.Length, RawLen: e.RawLen}
		if _, err := cr.decode(e.ID, loc, wrapped, data[e.Offset:e.Offset+e.Length]); err != nil {
			if !errors.Is(err, ErrCorrupt) {
				return err
			}
			bad++
			continue
		}
		c.rep.ChunksVerified++
	}
	if bad > 0 {
		c.add(SeverityError, ProblemCorrupt, key, fmt.Sprintf("%d of %d chunks do not decrypt or match their IDs", bad, len(c.entries[id])))
	}
	return nil
}

// sameEntries compares a trailer's entries with the index's, in any order.
func sameEntries(a, b []Entry) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[Entry]int, len(a))
	for _, e := range a {
		seen[e]++
	}
	for _, e := range b {
		if seen[e] == 0 {
			return false
		}
		seen[e]--
	}
	return true
}

// segments counts the log segments (the target's, or all) and reads the
// sampled ones in full.
func (c *checker) segments(ctx context.Context) error {
	prefixes := []string{walPrefixRoot, oplogPrefixRoot}
	if c.opts.Target != "" {
		prefixes = []string{walPrefix(c.opts.Target), oplogPrefix(c.opts.Target)}
	}
	var keys []string
	for _, p := range prefixes {
		k, err := c.r.listKeys(ctx, p)
		if err != nil {
			return err
		}
		for _, key := range k {
			if strings.HasSuffix(key, segmentSuffix) {
				keys = append(keys, key)
			} else {
				c.add(SeverityWarning, ProblemStray, key, "object is not named like a log segment")
			}
		}
	}
	c.rep.Segments = len(keys)
	for _, i := range c.sampleOf(len(keys)) {
		_, err := c.r.GetSegment(ctx, keys[i])
		switch {
		case errors.Is(err, backend.ErrNotFound):
			continue // expired meanwhile
		case errors.Is(err, ErrCorrupt):
			c.add(SeverityError, ProblemCorrupt, keys[i], "log segment is unreadable: "+err.Error())
		case err != nil:
			return err
		}
		c.rep.SegmentsRead++
	}
	return nil
}

func sortedPacks[V any](m map[PackID]V) []PackID {
	out := make([]PackID, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i][:], out[j][:]) < 0 })
	return out
}
