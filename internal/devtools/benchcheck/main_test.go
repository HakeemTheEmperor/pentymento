// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"strings"
	"testing"
)

// sample is benchstat -format csv output, trimmed.
const sample = `goos: linux
goarch: amd64
pkg: github.com/HakeemTheEmperor/pentymento/internal/chunker
cpu: AMD EPYC
,base.txt,,head.txt,,,
,sec/op,CI,sec/op,CI,vs base,P
Chunker-4,0.0282,1%,0.0339,2%,+20.00%,p=0.002 n=6
ChunkID-4,0.000492,1%,0.000493,1%,~,p=0.818 n=6
Faster-4,0.0300,1%,0.0200,1%,-33.33%,p=0.002 n=6
Small-4,0.0300,1%,0.0320,1%,+6.67%,p=0.002 n=6
geomean,0.0037,,0.0041,,+11.00%,

,base.txt,,head.txt,,,
,B/s,CI,B/s,CI,vs base,P
Chunker-4,9.9e+08,1%,7.0e+08,1%,-29.29%,p=0.002 n=6
pkg: github.com/HakeemTheEmperor/pentymento/internal/compress
,base.txt,,head.txt,,,
,sec/op,CI,sec/op,CI,vs base,P
Compress/default-4,0.0009,1%,0.0011,1%,+22.22%,p=0.002 n=6
New-4,,,0.0010,1%,,
`

func TestParse(t *testing.T) {
	regs, err := parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 2 {
		t.Fatalf("got %d regressions, want 2: %+v", len(regs), regs)
	}
	if regs[0].name != "Chunker-4" || regs[0].change != 20 || !strings.HasSuffix(regs[0].pkg, "/chunker") {
		t.Errorf("first = %+v", regs[0])
	}
	if regs[1].name != "Compress/default-4" || !strings.HasSuffix(regs[1].pkg, "/compress") {
		t.Errorf("second = %+v", regs[1])
	}
}

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(strings.NewReader(sample), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "::warning title=Benchmark regression::github.com/HakeemTheEmperor/pentymento/internal/chunker Chunker-4 is 20.0% slower (p=0.002 n=6)") {
		t.Errorf("output:\n%s", out.String())
	}
	out.Reset()
	_ = run(strings.NewReader(""), &out)
	if !strings.Contains(out.String(), "no time regressions") {
		t.Errorf("empty input: %q", out.String())
	}
}
