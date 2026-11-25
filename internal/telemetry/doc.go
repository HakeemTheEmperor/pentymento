// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package telemetry provides logging, metrics and progress reporting
// (PRD 7.13, SDD 10.1–10.3).
//
//   - Logs use log/slog. Every line carries target, job, run_id and
//     component, empty when not set. The agent logs to stdout; one-shot
//     commands log to stderr, and only with --debug.
//   - Metrics are declared once in a private Prometheus registry with the
//     names from PRD 7.13; labels are per target and job, never per table.
//   - Progress is one redrawn stderr line on a terminal, an info log line
//     every 30 s otherwise, or JSON events on stderr with --json.
//
// Telemetry.StartJob is the single entry point for an operation's logging
// and metrics, so tracing can be added there later without changing
// callers.
package telemetry
