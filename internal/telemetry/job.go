// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"
)

// Telemetry is the logging and metrics facade every operation uses. Tracing
// (LATER) is added here, in StartJob and Job.End.
type Telemetry struct {
	Logger  *slog.Logger
	Metrics *Metrics
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// New returns a Telemetry. A nil logger discards logs; nil metrics declares
// a new registry.
func New(logger *slog.Logger, metrics *Metrics) *Telemetry {
	if logger == nil {
		logger = Discard()
	}
	if metrics == nil {
		metrics = NewMetrics()
	}
	return &Telemetry{Logger: logger, Metrics: metrics}
}

func (t *Telemetry) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Component returns a logger for a component outside any job.
func (t *Telemetry) Component(name string) *slog.Logger {
	return t.Logger.With(KeyComponent, name)
}

// Job is one run of an operation (backup, verify, prune, check, …) on a
// target.
type Job struct {
	// RunID identifies this run in logs, child-process output and the
	// journal.
	RunID  string
	Target string
	Name   string
	// Log carries target, job, run_id and component on every line.
	Log *slog.Logger

	t     *Telemetry
	start time.Time
}

type jobKey struct{}

// StartJob starts a run of job on target by component, logs its start and
// returns a context carrying it.
func (t *Telemetry) StartJob(ctx context.Context, component, target, job string) (context.Context, *Job) {
	j := &Job{
		RunID:  NewRunID(),
		Target: target,
		Name:   job,
		t:      t,
		start:  t.now(),
	}
	j.Log = t.Logger.With(KeyTarget, target, KeyJob, job, KeyRunID, j.RunID, KeyComponent, component)
	j.Log.Info("job started")
	return context.WithValue(ctx, jobKey{}, j), j
}

// JobFrom returns the job a context carries, or nil.
func JobFrom(ctx context.Context) *Job {
	j, _ := ctx.Value(jobKey{}).(*Job)
	return j
}

// End records the run's duration and result: on success the last-success
// time, on failure the failure count. err must already be scrubbed of
// secrets.
func (j *Job) End(err error) {
	now := j.t.now()
	d := now.Sub(j.start)
	m := j.t.Metrics
	m.JobDuration.WithLabelValues(j.Name).Observe(d.Seconds())
	if err != nil {
		m.JobFailures.WithLabelValues(j.Target, j.Name).Inc()
		j.Log.Error("job failed", "duration", d, "error", err.Error())
		return
	}
	m.LastSuccess.WithLabelValues(j.Target, j.Name).Set(float64(now.Unix()))
	j.Log.Info("job finished", "duration", d)
}

// NewRunID returns a random 12-character lowercase hex run ID.
func NewRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}
