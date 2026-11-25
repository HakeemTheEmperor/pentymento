// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package telemetry

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric label names. Labels are per target and job (plus op, kind, type
// and level where SDD 10.2 names them), never per table or collection.
const (
	LabelTarget = "target"
	LabelJob    = "job"
	LabelOp     = "op"
	LabelKind   = "kind"
	LabelType   = "type"
	LabelLevel  = "level"
)

// JobDurationBuckets cover a quick check up to a long base backup.
var JobDurationBuckets = []float64{1, 5, 15, 30, 60, 300, 900, 1800, 3600, 7200, 14400, 28800, 86400}

// BackendRequestBuckets cover small metadata calls up to large pack
// uploads, in seconds.
var BackendRequestBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// Metrics holds every metric from PRD 7.13 in a private registry. Values
// are set by the components named in SDD 10.2.
type Metrics struct {
	registry *prometheus.Registry

	CaptureLag             *prometheus.GaugeVec     // {target}
	PGSlotRetainedWAL      *prometheus.GaugeVec     // {target}
	MongoOplogWindow       *prometheus.GaugeVec     // {target}
	PITRWindow             *prometheus.GaugeVec     // {target}
	LastSuccess            *prometheus.GaugeVec     // {target, job}
	JobDuration            *prometheus.HistogramVec // {job}
	JobFailures            *prometheus.CounterVec   // {target, job}
	RepoBytes              *prometheus.GaugeVec     // {kind}
	DedupRatio             prometheus.Gauge
	BackendRequestDuration *prometheus.HistogramVec // {op}
	BackendRequestErrors   *prometheus.CounterVec   // {op}
	UploadQueueDepth       prometheus.Gauge
	EventsDetected         *prometheus.CounterVec // {type}
	LeaseHeld              *prometheus.GaugeVec   // {target}
	VerifyLastResult       *prometheus.GaugeVec   // {target, level}
}

// NewMetrics declares every metric in a new registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		CaptureLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pentym_capture_lag_seconds",
			Help: "Seconds between the newest change in the database and the newest change captured.",
		}, []string{LabelTarget}),
		PGSlotRetainedWAL: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pentym_pg_slot_retained_wal_bytes",
			Help: "Bytes of WAL the Postgres replication slot makes the server retain.",
		}, []string{LabelTarget}),
		MongoOplogWindow: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pentym_mongo_oplog_window_seconds",
			Help: "Seconds of history in the MongoDB oplog, from its oldest to its newest entry.",
		}, []string{LabelTarget}),
		PITRWindow: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pentym_pitr_window_seconds",
			Help: "Length of the actual point-in-time recovery window, gaps included.",
		}, []string{LabelTarget}),
		LastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pentym_last_success_timestamp_seconds",
			Help: "Unix time of the last successful run of a job.",
		}, []string{LabelTarget, LabelJob}),
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pentym_job_duration_seconds",
			Help:    "Duration of jobs, in seconds.",
			Buckets: JobDurationBuckets,
		}, []string{LabelJob}),
		JobFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pentym_job_failures_total",
			Help: "Number of failed job runs.",
		}, []string{LabelTarget, LabelJob}),
		RepoBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pentym_repo_bytes",
			Help: "Bytes stored in the repository, by kind.",
		}, []string{LabelKind}),
		DedupRatio: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pentym_dedup_ratio",
			Help: "Ratio of logical snapshot bytes to bytes stored after deduplication.",
		}),
		BackendRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pentym_backend_request_duration_seconds",
			Help:    "Duration of storage backend requests, in seconds.",
			Buckets: BackendRequestBuckets,
		}, []string{LabelOp}),
		BackendRequestErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pentym_backend_request_errors_total",
			Help: "Number of failed storage backend requests.",
		}, []string{LabelOp}),
		UploadQueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pentym_upload_queue_depth",
			Help: "Number of objects waiting to be uploaded.",
		}),
		EventsDetected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pentym_events_detected_total",
			Help: "Number of destructive events detected, by type.",
		}, []string{LabelType}),
		LeaseHeld: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pentym_lease_held",
			Help: "1 while this agent holds the target's capture lease, otherwise 0.",
		}, []string{LabelTarget}),
		VerifyLastResult: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pentym_verify_last_result",
			Help: "Result of the last verify at a level: 1 passed, 0 failed.",
		}, []string{LabelTarget, LabelLevel}),
	}
	m.registry.MustRegister(
		m.CaptureLag, m.PGSlotRetainedWAL, m.MongoOplogWindow, m.PITRWindow,
		m.LastSuccess, m.JobDuration, m.JobFailures, m.RepoBytes, m.DedupRatio,
		m.BackendRequestDuration, m.BackendRequestErrors, m.UploadQueueDepth,
		m.EventsDetected, m.LeaseHeld, m.VerifyLastResult,
	)
	return m
}

// Registry returns the registry the metrics are declared in.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler serves the metrics in the Prometheus text format, for /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
