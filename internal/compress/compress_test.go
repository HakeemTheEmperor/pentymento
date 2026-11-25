// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package compress

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const maxChunk = 4 << 20

func newCompressor(t testing.TB, l Level) *Compressor {
	t.Helper()
	c, err := New(l, maxChunk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func random(seed uint64, n int) []byte {
	rng := rand.New(rand.NewPCG(seed, 2)) // #nosec G404 -- test data
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32()) // #nosec G115 -- truncation intended
	}
	return b
}

// text is compressible, database-like content.
func text(n int) []byte {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteString("INSERT INTO orders (id, customer, total) VALUES (")
		b.WriteString(strings.Repeat("7", i%9+1))
		b.WriteString(", 'customer', 19.99);\n")
	}
	return []byte(b.String()[:n])
}

func TestLevels(t *testing.T) {
	for _, name := range []string{"fastest", "default", "better", "best"} {
		l, err := ParseLevel(name)
		if err != nil || l.String() != name {
			t.Errorf("ParseLevel(%q) = %v, %v", name, l, err)
		}
	}
	if _, err := ParseLevel("ultra"); err == nil {
		t.Error("ParseLevel accepted ultra")
	}
	if _, err := New(Level(9), maxChunk); err == nil {
		t.Error("New accepted an unknown level")
	}
	if Level(9).String() != "Level(9)" {
		t.Error("unknown level String")
	}
}

func TestRoundTrip(t *testing.T) {
	inputs := map[string][]byte{
		"empty":         {},
		"one byte":      {42},
		"text":          text(1 << 20),
		"random":        random(1, 1<<20),
		"zeros 4 MiB":   make([]byte, maxChunk),
		"random 4 MiB":  random(2, maxChunk),
		"mostly random": append(random(3, 900<<10), text(100<<10)...),
	}
	for _, l := range []Level{Fastest, Default, Better, Best} {
		c := newCompressor(t, l)
		for name, in := range inputs {
			out := c.Compress(nil, in)
			back, err := c.Decompress(nil, out)
			if err != nil || !bytes.Equal(back, in) {
				t.Errorf("%s/%s: round trip failed: %v", l, name, err)
			}
		}
	}
}

func TestHeaderAndSkipRule(t *testing.T) {
	c := newCompressor(t, Default)
	compressed := c.Compress(nil, text(1<<20))
	if compressed[0] != HeaderZstd || len(compressed) > (1<<20)/4 {
		t.Errorf("text: header 0x%02x, %d bytes", compressed[0], len(compressed))
	}
	raw := c.Compress(nil, random(1, 1<<20))
	if raw[0] != HeaderNone || len(raw) != 1<<20+1 {
		t.Errorf("random: header 0x%02x, %d bytes; want stored raw", raw[0], len(raw))
	}
	if e := c.Compress(nil, nil); !bytes.Equal(e, []byte{HeaderNone}) {
		t.Errorf("empty: %x", e)
	}
	// Appending keeps what dst already held.
	if got := c.Compress([]byte("prefix"), []byte{1}); !bytes.HasPrefix(got, []byte("prefix")) {
		t.Errorf("Compress dropped dst: %q", got)
	}
}

// TestSkipThreshold checks the 3% rule at its boundary: data that
// compresses to just under and just over 97% of its size.
func TestSkipThreshold(t *testing.T) {
	c := newCompressor(t, Default)
	n := 1 << 20
	for _, compressiblePart := range []int{n / 100, n / 10} {
		in := append(random(5, n-compressiblePart), make([]byte, compressiblePart)...)
		out := c.Compress(nil, in)
		zstdSize := len(c.enc.EncodeAll(in, nil))
		wantRaw := zstdSize*100 > n*97
		if gotRaw := out[0] == HeaderNone; gotRaw != wantRaw {
			t.Errorf("%d%% zeros: zstd size %d of %d, stored raw = %v, want %v", compressiblePart*100/n, zstdSize, n, gotRaw, wantRaw)
		}
	}
}

func TestDecompressErrors(t *testing.T) {
	c := newCompressor(t, Default)
	good := c.Compress(nil, text(64<<10))

	big, err := New(Default, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = big.Close() }()
	bomb := big.Compress(nil, make([]byte, 8<<20)) // 8 MiB of zeros: tiny compressed
	if bomb[0] != HeaderZstd || len(bomb) > 4096 {
		t.Fatalf("bomb is %d bytes", len(bomb))
	}

	tests := map[string][]byte{
		"empty":            {},
		"unknown header":   {0x7f, 1, 2},
		"truncated zstd":   good[:len(good)/2],
		"garbage zstd":     append([]byte{HeaderZstd}, random(9, 1000)...),
		"raw too large":    append([]byte{HeaderNone}, make([]byte, maxChunk+1)...),
		"decompress bomb":  bomb,
		"zstd header only": {HeaderZstd},
	}
	for name, in := range tests {
		out, err := c.Decompress([]byte("keep"), in)
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
		if string(out) != "keep" {
			t.Errorf("%s: dst changed to %d bytes", name, len(out))
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	c := newCompressor(t, Default)
	in := text(256 << 10)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				back, err := c.Decompress(nil, c.Compress(nil, in))
				if err != nil || !bytes.Equal(back, in) {
					t.Error("concurrent round trip failed")
				}
			}
		}()
	}
	wg.Wait()
}

func FuzzDecompress(f *testing.F) {
	c, err := New(Default, 1<<20)
	if err != nil {
		f.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	f.Add([]byte{})
	f.Add([]byte{HeaderNone, 1, 2, 3})
	f.Add(c.Compress(nil, text(10000)))
	f.Add(c.Compress(nil, make([]byte, 100000)))
	enc, _ := zstd.NewWriter(nil)
	f.Add(append([]byte{HeaderZstd}, enc.EncodeAll(make([]byte, 2<<20), nil)...))
	_ = enc.Close()
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := c.Decompress(nil, in)
		if err == nil && len(out) > 1<<20 {
			t.Fatalf("decoded %d bytes past the limit", len(out))
		}
		if err != nil && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("error not ErrCorrupt: %v", err)
		}
	})
}

// FuzzRoundTrip checks that any input survives compression.
func FuzzRoundTrip(f *testing.F) {
	c, err := New(Fastest, 1<<20)
	if err != nil {
		f.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	f.Add([]byte("hello"))
	f.Add(text(5000))
	f.Fuzz(func(t *testing.T, in []byte) {
		back, err := c.Decompress(nil, c.Compress(nil, in))
		if err != nil || !bytes.Equal(back, in) {
			t.Fatalf("round trip of %d bytes failed: %v", len(in), err)
		}
	})
}

func benchData() []byte {
	// Half database-like text, half random: neither best nor worst case.
	return append(text(512<<10), random(7, 512<<10)...)
}

func BenchmarkCompress(b *testing.B) {
	data := benchData()
	for _, l := range []Level{Fastest, Default, Better, Best} {
		b.Run(l.String(), func(b *testing.B) {
			c := newCompressor(b, l)
			b.SetBytes(int64(len(data)))
			var dst []byte
			for b.Loop() {
				dst = c.Compress(dst[:0], data)
			}
		})
	}
}

func BenchmarkDecompress(b *testing.B) {
	c := newCompressor(b, Default)
	in := c.Compress(nil, benchData())
	b.SetBytes(int64(len(benchData())))
	var dst []byte
	for b.Loop() {
		var err error
		if dst, err = c.Decompress(dst[:0], in); err != nil {
			b.Fatal(err)
		}
	}
}
