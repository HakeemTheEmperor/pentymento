// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

// Log formats.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// Fields every log line carries (SDD 10.1).
const (
	KeyTarget    = "target"
	KeyJob       = "job"
	KeyRunID     = "run_id"
	KeyComponent = "component"
)

var baseKeys = []string{KeyTarget, KeyJob, KeyRunID, KeyComponent}

// NewLogger returns a logger writing to w at level. format is FormatText,
// FormatJSON or "" for text on a terminal (tty) and JSON otherwise. Every
// line carries target, job, run_id and component.
func NewLogger(w io.Writer, format string, tty bool, level slog.Leveler) (*slog.Logger, error) {
	if format == "" {
		format = FormatJSON
		if tty {
			format = FormatText
		}
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch format {
	case FormatText:
		h = slog.NewTextHandler(w, opts)
	case FormatJSON:
		h = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("log format must be text or json, not %q", format)
	}
	return slog.New(&baseFieldsHandler{h: h, have: map[string]bool{}}), nil
}

// Discard is a logger that writes nothing.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// baseFieldsHandler adds any of the base fields a record lacks, as empty
// strings, so every line has the same keys.
type baseFieldsHandler struct {
	h    slog.Handler
	have map[string]bool // base fields already added with WithAttrs
}

func (b *baseFieldsHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return b.h.Enabled(ctx, l)
}

func (b *baseFieldsHandler) Handle(ctx context.Context, r slog.Record) error {
	present := map[string]bool{}
	r.Attrs(func(a slog.Attr) bool {
		present[a.Key] = true
		return true
	})
	var missing []slog.Attr
	for _, k := range baseKeys {
		if !b.have[k] && !present[k] {
			missing = append(missing, slog.String(k, ""))
		}
	}
	if len(missing) > 0 {
		r = r.Clone()
		r.AddAttrs(missing...)
	}
	return b.h.Handle(ctx, r)
}

func (b *baseFieldsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	have := make(map[string]bool, len(b.have)+len(attrs))
	for k := range b.have {
		have[k] = true
	}
	for _, a := range attrs {
		have[a.Key] = true
	}
	return &baseFieldsHandler{h: b.h.WithAttrs(attrs), have: have}
}

// WithGroup adds the missing base fields before opening the group, so they
// stay at the top level of each line.
func (b *baseFieldsHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return b
	}
	var missing []slog.Attr
	have := map[string]bool{}
	for _, k := range baseKeys {
		have[k] = true
		if !b.have[k] {
			missing = append(missing, slog.String(k, ""))
		}
	}
	h := b.h
	if len(missing) > 0 {
		h = h.WithAttrs(missing)
	}
	return &baseFieldsHandler{h: h.WithGroup(name), have: have}
}
