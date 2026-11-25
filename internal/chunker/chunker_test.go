// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package chunker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"

	"github.com/zeebo/blake3"
	"pgregory.net/rapid"
)

// smallParams make property tests and fuzzing fast while keeping every
// code path.
var smallParams = Params{Min: 256, Avg: 1024, Max: 4096}

var testKey = [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}

func randomData(seed uint64, n int) []byte {
	rng := rand.New(rand.NewPCG(seed, seed^0xabcdef)) // #nosec G404 -- test data
	b := make([]byte, n)
	for i := 0; i+8 <= n; i += 8 {
		binary.LittleEndian.PutUint64(b[i:], rng.Uint64())
	}
	return b
}

// tb is what the helpers need from *testing.T, *testing.B and *rapid.T.
type tb interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

// chunks splits data and returns copies of the chunks.
func chunks(t tb, data []byte, table *Table, p Params, r io.Reader) [][]byte {
	t.Helper()
	if r == nil {
		r = bytes.NewReader(data)
	}
	c, err := NewWithParams(r, table, p)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for {
		ch, err := c.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append([]byte(nil), ch...))
	}
}

// checkBounds checks every chunk but the last is within [Min, Max] and the
// last is at most Max.
func checkBounds(t tb, cs [][]byte, p Params) {
	t.Helper()
	for i, c := range cs {
		if len(c) > p.Max || (i < len(cs)-1 && len(c) < p.Min) || len(c) == 0 {
			t.Fatalf("chunk %d of %d has %d bytes, limits %d..%d", i, len(cs), len(c), p.Min, p.Max)
		}
	}
}

// shortReader returns at most a few bytes per Read, chosen by rng.
type shortReader struct {
	data []byte
	rng  *rapid.T
}

func (s *shortReader) Read(p []byte) (int, error) {
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	n := rapid.IntRange(1, 9000).Draw(s.rng, "read")
	n = min(n, len(p), len(s.data))
	copy(p, s.data[:n])
	s.data = s.data[n:]
	return n, nil
}

// TestPropertyReassembly: joining the chunks gives back the input, chunks
// respect the size limits, and boundaries do not depend on how the input
// arrives.
func TestPropertyReassembly(t *testing.T) {
	table := NewTable(testKey)
	rapid.Check(t, func(rt *rapid.T) {
		seed := rapid.Uint64().Draw(rt, "seed")
		n := rapid.IntRange(0, 64<<10).Draw(rt, "size")
		data := randomData(seed, n)
		if rapid.Bool().Draw(rt, "repetitive") {
			data = bytes.Repeat([]byte("pentymento "), n/11+1)[:n]
		}
		whole := chunks(rt, data, table, smallParams, nil)
		if !bytes.Equal(bytes.Join(whole, nil), data) {
			rt.Fatalf("reassembly differs from input")
		}
		checkBounds(rt, whole, smallParams)
		pieces := chunks(rt, data, table, smallParams, &shortReader{data: data, rng: rt})
		if len(pieces) != len(whole) {
			rt.Fatalf("short reads gave %d chunks, whole reads %d", len(pieces), len(whole))
		}
		for i := range whole {
			if !bytes.Equal(whole[i], pieces[i]) {
				rt.Fatalf("chunk %d differs between read patterns", i)
			}
		}
	})
}

func TestDefaultSizes(t *testing.T) {
	data := randomData(7, 64<<20)
	cs := chunks(t, data, NewTable(testKey), DefaultParams, nil)
	checkBounds(t, cs, DefaultParams)
	if !bytes.Equal(bytes.Join(cs, nil), data) {
		t.Fatal("reassembly differs from input")
	}
	avg := len(data) / len(cs)
	t.Logf("64 MiB in %d chunks, average %d KiB", len(cs), avg>>10)
	if avg < AvgSize/2 || avg > 2*AvgSize {
		t.Errorf("average chunk size %d is far from %d", avg, AvgSize)
	}
}

func TestShortStreams(t *testing.T) {
	table := NewTable(testKey)
	if cs := chunks(t, nil, table, DefaultParams, nil); len(cs) != 0 {
		t.Errorf("empty stream gave %d chunks", len(cs))
	}
	data := randomData(1, MinSize-1)
	if cs := chunks(t, data, table, DefaultParams, nil); len(cs) != 1 || len(cs[0]) != len(data) {
		t.Errorf("stream shorter than the minimum gave %d chunks", len(cs))
	}
	// Incompressible content with no boundary is cut at the maximum.
	zeros := make([]byte, 3*MaxSize+5)
	cs := chunks(t, zeros, table, DefaultParams, nil)
	checkBounds(t, cs, DefaultParams)
	t.Logf("12 MiB of zeros: %d chunks", len(cs))
}

// TestInsertionStability: inserting bytes changes only the chunks around
// the insertion; the rest are found again.
func TestInsertionStability(t *testing.T) {
	table := NewTable(testKey)
	data := randomData(3, 32<<20)
	before := chunks(t, data, table, DefaultParams, nil)
	for _, pos := range []int{0, 5 << 20, 17<<20 + 12345, len(data) - 100} {
		edited := append(append(append([]byte(nil), data[:pos]...), []byte("an inserted row of data")...), data[pos:]...)
		after := chunks(t, edited, table, DefaultParams, nil)
		known := map[string]bool{}
		for _, c := range before {
			known[string(c)] = true
		}
		changed := 0
		for _, c := range after {
			if !known[string(c)] {
				changed++
			}
		}
		if changed > 2 {
			t.Errorf("insertion at %d changed %d of %d chunks, want at most 2", pos, changed, len(after))
		}
	}
}

// TestPropertyInsertionStability checks, with small chunks and random
// edits, that an insertion disturbs boundaries only locally: every chunk
// that ends before the insertion is unchanged, and every byte more than
// 4×Max past the insertion is cut exactly as before. (Measured over many
// edits, boundaries resynchronise within 2×Max.)
func TestPropertyInsertionStability(t *testing.T) {
	table := NewTable(testKey)
	p := smallParams
	rapid.Check(t, func(rt *rapid.T) {
		data := randomData(rapid.Uint64().Draw(rt, "seed"), rapid.IntRange(8<<10, 128<<10).Draw(rt, "size"))
		pos := rapid.IntRange(0, len(data)).Draw(rt, "pos")
		ins := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(rt, "insert")
		edited := append(append(append([]byte(nil), data[:pos]...), ins...), data[pos:]...)
		a := chunks(rt, data, table, p, nil)
		b := chunks(rt, edited, table, p, nil)

		// Prefix: chunks ending at or before pos are identical, except the
		// last, which the end of the stream cut, not the content.
		off := 0
		for i, c := range a[:max(len(a)-1, 0)] {
			if off+len(c) > pos {
				break
			}
			if i >= len(b) || !bytes.Equal(c, b[i]) {
				rt.Fatalf("chunk %d ends before the insertion at %d but changed", i, pos)
			}
			off += len(c)
		}
		// Suffix: walk back while chunks match; the first difference must
		// lie within 4×Max of the insertion.
		i, j := len(a)-1, len(b)-1
		for i >= 0 && j >= 0 && bytes.Equal(a[i], b[j]) {
			i--
			j--
		}
		end := 0
		for k := 0; k <= j; k++ {
			end += len(b[k])
		}
		if limit := pos + len(ins) + 4*p.Max; end > limit {
			rt.Fatalf("insertion of %d bytes at %d disturbed boundaries up to byte %d, beyond %d", len(ins), pos, end, limit)
		}
	})
}

func TestKeyedTable(t *testing.T) {
	a, b := NewTable(testKey), NewTable(testKey)
	if *a != *b {
		t.Fatal("NewTable is not deterministic")
	}
	other := testKey
	other[0] ^= 1
	if *NewTable(other) == *a {
		t.Fatal("different keys gave the same gear table")
	}
	data := randomData(9, 16<<20)
	sizes := func(tb *Table) []int {
		var s []int
		for _, c := range chunks(t, data, tb, DefaultParams, nil) {
			s = append(s, len(c))
		}
		return s
	}
	if fmt.Sprint(sizes(a)) == fmt.Sprint(sizes(NewTable(other))) {
		t.Error("chunk sizes do not depend on the key")
	}
}

func TestReadError(t *testing.T) {
	boom := errors.New("disk on fire")
	c := New(io.MultiReader(bytes.NewReader(randomData(1, 1000)), &errorReader{boom}), NewTable(testKey))
	if _, err := c.Next(); !errors.Is(err, boom) {
		t.Errorf("Next = %v, want the reader's error", err)
	}
}

type errorReader struct{ err error }

func (e *errorReader) Read([]byte) (int, error) { return 0, e.err }

func TestParamsValidate(t *testing.T) {
	for _, p := range []Params{
		{Min: 10, Avg: 1024, Max: 4096},   // below the window
		{Min: 2048, Avg: 1024, Max: 4096}, // min above avg
		{Min: 256, Avg: 1000, Max: 4096},  // avg not a power of two
		{Min: 256, Avg: 1024, Max: 1024},  // max not above avg
	} {
		if _, err := NewWithParams(nil, NewTable(testKey), p); err == nil {
			t.Errorf("NewWithParams accepted %+v", p)
		}
	}
}

func TestChunkID(t *testing.T) {
	h := NewHasher(testKey)
	data := randomData(4, 1<<20)
	ref, _ := blake3.NewKeyed(testKey[:])
	_, _ = ref.Write(data)
	var want ID
	ref.Sum(want[:0])
	if got := h.Sum(data); got != want {
		t.Fatalf("Sum = %s, want keyed BLAKE3 %s", got, want)
	}
	// Reused hashers and concurrent use give the same answers.
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if h.Sum(data) != want {
					t.Error("concurrent Sum differs")
				}
			}
		}()
	}
	wg.Wait()

	other := testKey
	other[31] ^= 0xff
	if NewHasher(other).Sum(data) == want {
		t.Error("different keys gave the same chunk ID")
	}
	if blake3.Sum256(data) == want {
		t.Error("chunk ID equals the unkeyed hash")
	}
	if h.Sum(nil) == h.Sum([]byte{0}) {
		t.Error("empty and one-byte inputs collide")
	}
}

func TestIDText(t *testing.T) {
	id := NewHasher(testKey).Sum([]byte("x"))
	s := id.String()
	if len(s) != 64 || strings.ToLower(s) != s {
		t.Fatalf("String = %q", s)
	}
	back, err := ParseID(s)
	if err != nil || back != id {
		t.Fatalf("ParseID(String) = %v, %v", back, err)
	}
	for _, bad := range []string{"", s[:63], s + "0", strings.ToUpper(s), "g" + s[1:]} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) succeeded", bad)
		}
	}
}

func TestHasherHidesKey(t *testing.T) {
	h := NewHasher(testKey)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x"} {
		if out := fmt.Sprintf(verb, h); out != "[chunk-ID hasher]" {
			t.Errorf("%s of a hasher = %q", verb, out)
		}
	}
}

func FuzzChunker(f *testing.F) {
	f.Add([]byte{}, uint16(1))
	f.Add(bytes.Repeat([]byte{0}, 10000), uint16(4096))
	f.Add(randomData(1, 20000), uint16(7))
	table := NewTable(testKey)
	f.Fuzz(func(t *testing.T, data []byte, readSize uint16) {
		r := &fixedReader{data: data, n: int(readSize%8192) + 1}
		cs := chunks(t, data, table, smallParams, r)
		checkBounds(t, cs, smallParams)
		if !bytes.Equal(bytes.Join(cs, nil), data) {
			t.Fatal("reassembly differs from input")
		}
	})
}

// fixedReader returns n bytes per Read.
type fixedReader struct {
	data []byte
	n    int
}

func (f *fixedReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, io.EOF
	}
	n := min(f.n, len(p), len(f.data))
	copy(p, f.data[:n])
	f.data = f.data[n:]
	return n, nil
}

func FuzzParseID(f *testing.F) {
	f.Add(NewHasher(testKey).Sum(nil).String())
	f.Add("zz")
	f.Fuzz(func(t *testing.T, s string) {
		id, err := ParseID(s)
		if err == nil && id.String() != s {
			t.Fatalf("ParseID(%q) round-trips to %q", s, id.String())
		}
	})
}

func BenchmarkChunker(b *testing.B) {
	data := randomData(5, 32<<20)
	table := NewTable(testKey)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		c := New(bytes.NewReader(data), table)
		for {
			if _, err := c.Next(); err != nil {
				break
			}
		}
	}
}

func BenchmarkChunkID(b *testing.B) {
	data := randomData(6, 1<<20)
	h := NewHasher(testKey)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		h.Sum(data)
	}
}
