// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Command benchcheck reads benchstat CSV output (benchstat -format csv
// base.txt head.txt) on stdin and flags time regressions beyond 10% as
// GitHub Actions warning annotations (PRD 10.3). benchstat marks changes
// that are not statistically significant (p >= 0.05) with "~", so only
// significant changes are flagged. It never fails: shared CI runners are
// too noisy for a regression to block a pull request.
//
// Usage:
//
//	benchstat -format csv base.txt head.txt | go run ./internal/devtools/benchcheck
package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// threshold is the time regression, in percent, worth flagging.
const threshold = 10.0

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "benchcheck:", err)
	}
}

// regression is a benchmark that got significantly slower.
type regression struct {
	pkg, name string
	change    float64
	p         string
}

func run(in io.Reader, out io.Writer) error {
	regs, err := parse(in)
	if err != nil {
		return err
	}
	for _, r := range regs {
		_, _ = fmt.Fprintf(out, "::warning title=Benchmark regression::%s %s is %.1f%% slower (%s)\n", r.pkg, r.name, r.change, r.p)
	}
	if len(regs) == 0 {
		_, _ = fmt.Fprintf(out, "no time regressions beyond %.0f%%\n", threshold)
	}
	return nil
}

// parse finds sec/op rows whose "vs base" change exceeds the threshold.
func parse(in io.Reader) ([]regression, error) {
	r := csv.NewReader(in)
	r.FieldsPerRecord = -1
	var (
		regs []regression
		pkg  string
		unit string
	)
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return regs, nil
		}
		if err != nil {
			return regs, err
		}
		switch {
		case len(rec) == 1 && strings.HasPrefix(rec[0], "pkg: "):
			pkg = strings.TrimPrefix(rec[0], "pkg: ")
		case len(rec) >= 7 && rec[0] == "" && rec[1] != "" && rec[2] == "CI":
			unit = rec[1]
		case len(rec) >= 7 && rec[0] != "" && rec[0] != "geomean" && unit == "sec/op":
			change := strings.TrimSuffix(rec[5], "%")
			v, err := strconv.ParseFloat(change, 64)
			if err != nil {
				continue // "~": not significant, or no base to compare with
			}
			if v > threshold {
				regs = append(regs, regression{pkg: pkg, name: rec[0], change: v, p: rec[6]})
			}
		}
	}
}
