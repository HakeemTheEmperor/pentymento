// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package telemetry

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
)

// ProgressMode is how progress is shown (PRD 7.13, Brand 10.4).
type ProgressMode int

// Progress modes.
const (
	// ProgressOff shows nothing (for example with -q).
	ProgressOff ProgressMode = iota
	// ProgressTerminal redraws one stderr line.
	ProgressTerminal
	// ProgressLog writes an info log line now and then.
	ProgressLog
	// ProgressJSON writes one JSON event per line.
	ProgressJSON
)

// Default intervals between progress updates.
const (
	TerminalInterval = 250 * time.Millisecond
	LogInterval      = 30 * time.Second
	JSONInterval     = time.Second
)

// ProgressOptions configures a Progress.
type ProgressOptions struct {
	Mode ProgressMode
	// W receives the terminal line or JSON events (stderr).
	W io.Writer
	// Logger receives log-mode lines.
	Logger *slog.Logger
	// Label names the work, such as "backup prod-pg".
	Label string
	// Total is the expected number of bytes, or 0 when unknown.
	Total int64
	// Unicode selects Unicode symbols on a terminal.
	Unicode bool
	// Interval overrides the mode's default update interval.
	Interval time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Progress reports the bytes processed by a long operation: bytes, rate,
// time remaining and deduplication so far.
type Progress struct {
	opts  ProgressOptions
	start time.Time

	bytes atomic.Int64
	dedup atomic.Uint64 // float64 bits

	mu       sync.Mutex // serialises rendering
	lastLen  int        // width of the last terminal line, to erase it
	started  atomic.Bool
	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

// NewProgress returns a Progress; call Start to update it periodically and
// Done when the work ends.
func NewProgress(o ProgressOptions) *Progress {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Interval == 0 {
		switch o.Mode {
		case ProgressTerminal:
			o.Interval = TerminalInterval
		case ProgressLog:
			o.Interval = LogInterval
		default:
			o.Interval = JSONInterval
		}
	}
	if o.Logger == nil {
		o.Logger = Discard()
	}
	return &Progress{opts: o, start: o.Now(), stop: make(chan struct{}), stopped: make(chan struct{})}
}

// Add records n more bytes processed.
func (p *Progress) Add(n int64) { p.bytes.Add(n) }

// SetDedup records the deduplication ratio so far (logical bytes per
// stored byte).
func (p *Progress) SetDedup(ratio float64) { p.dedup.Store(math.Float64bits(ratio)) }

// Start updates the progress every interval until Done.
func (p *Progress) Start() {
	if !p.started.CompareAndSwap(false, true) {
		return
	}
	if p.opts.Mode == ProgressOff {
		close(p.stopped)
		return
	}
	go func() {
		defer close(p.stopped)
		t := time.NewTicker(p.opts.Interval)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.Render()
			}
		}
	}()
}

// Done stops the updates and writes the final state: the terminal line is
// erased, so the command's result follows on a clean line; log and JSON
// modes write a last update marked done.
func (p *Progress) Done() {
	p.stopOnce.Do(func() { close(p.stop) })
	if p.started.Load() {
		<-p.stopped
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.opts.Mode {
	case ProgressTerminal:
		if p.lastLen > 0 {
			_, _ = io.WriteString(p.opts.W, "\r"+strings.Repeat(" ", p.lastLen)+"\r")
			p.lastLen = 0
		}
	case ProgressLog, ProgressJSON:
		p.write(p.snapshot(), true)
	}
}

// Render writes one update now.
func (p *Progress) Render() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.write(p.snapshot(), false)
}

type snapshot struct {
	bytes, total int64
	elapsed      time.Duration
	rate         float64 // bytes per second
	eta          time.Duration
	hasETA       bool
	dedup        float64
}

func (p *Progress) snapshot() snapshot {
	s := snapshot{
		bytes:   p.bytes.Load(),
		total:   p.opts.Total,
		elapsed: p.opts.Now().Sub(p.start),
		dedup:   math.Float64frombits(p.dedup.Load()),
	}
	if s.elapsed > 0 {
		s.rate = float64(s.bytes) / s.elapsed.Seconds()
	}
	if s.total > 0 && s.rate > 0 && s.bytes <= s.total {
		s.eta = time.Duration(float64(s.total-s.bytes) / s.rate * float64(time.Second))
		s.hasETA = true
	}
	return s
}

func (p *Progress) write(s snapshot, done bool) {
	switch p.opts.Mode {
	case ProgressTerminal:
		line := p.terminalLine(s)
		pad := ""
		if n := p.lastLen - len([]rune(line)); n > 0 {
			pad = strings.Repeat(" ", n)
		}
		_, _ = io.WriteString(p.opts.W, "\r"+line+pad)
		p.lastLen = len([]rune(line))
	case ProgressLog:
		attrs := []any{"label", p.opts.Label, "bytes", s.bytes, "bytes_per_second", math.Round(s.rate), "done", done}
		if s.total > 0 {
			attrs = append(attrs, "total", s.total)
		}
		if s.hasETA {
			attrs = append(attrs, "eta_seconds", math.Round(s.eta.Seconds()))
		}
		if s.dedup > 0 {
			attrs = append(attrs, "dedup_ratio", round2(s.dedup))
		}
		p.opts.Logger.Info("progress", attrs...)
	case ProgressJSON:
		_ = json.NewEncoder(p.opts.W).Encode(p.jsonEvent(s, done))
	}
}

// terminalLine renders "… backup prod-pg: 1.2 GiB of 50.0 GiB · 85.3 MiB/s
// · 9m 32s left · dedupe 3.1×".
func (p *Progress) terminalLine(s snapshot) string {
	dot, times := " · ", "×"
	if !p.opts.Unicode {
		dot, times = " - ", "x"
	}
	parts := []string{cliout.Bytes(s.bytes)}
	if s.total > 0 {
		parts[0] += " of " + cliout.Bytes(s.total)
	}
	parts = append(parts, cliout.Bytes(int64(s.rate))+"/s")
	if s.hasETA {
		parts = append(parts, cliout.Duration(s.eta)+" left")
	}
	if s.dedup > 0 {
		parts = append(parts, fmt.Sprintf("dedupe %.1f%s", s.dedup, times))
	}
	line := cliout.SymProgress.Text(p.opts.Unicode) + " "
	if p.opts.Label != "" {
		line += p.opts.Label + ": "
	}
	return line + strings.Join(parts, dot)
}

// ProgressEvent is one --json progress event, written to stderr.
type ProgressEvent struct {
	Time           string   `json:"time"`
	Event          string   `json:"event"`
	Bytes          int64    `json:"bytes"`
	Total          *int64   `json:"total,omitempty"`
	BytesPerSecond float64  `json:"bytes_per_second"`
	ETASeconds     *float64 `json:"eta_seconds,omitempty"`
	DedupRatio     *float64 `json:"dedup_ratio,omitempty"`
}

func (p *Progress) jsonEvent(s snapshot, done bool) ProgressEvent {
	e := ProgressEvent{
		Time:           cliout.JSONTime(p.opts.Now()),
		Event:          "progress",
		Bytes:          s.bytes,
		BytesPerSecond: math.Round(s.rate),
	}
	if done {
		e.Event = "done"
	}
	if s.total > 0 {
		t := s.total
		e.Total = &t
	}
	if s.hasETA {
		eta := math.Round(s.eta.Seconds())
		e.ETASeconds = &eta
	}
	if s.dedup > 0 {
		d := round2(s.dedup)
		e.DedupRatio = &d
	}
	return e
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
