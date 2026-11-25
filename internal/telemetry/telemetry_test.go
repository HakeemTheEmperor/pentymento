// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/goleak"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// jsonLines decodes newline-separated JSON objects.
func jsonLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestLogBaseFields(t *testing.T) {
	var buf bytes.Buffer
	log, err := NewLogger(&buf, FormatJSON, false, slog.LevelDebug)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("plain")
	log.With(KeyTarget, "prod-pg").Info("with target")
	log.Info("inline", KeyJob, "backup")
	log.With(KeyComponent, "repo").WithGroup("pack").Info("grouped", "size", 3)
	log.WithGroup("g").With(KeyTarget, "inner").Info("target inside group")

	lines := jsonLines(t, buf.String())
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5:\n%s", len(lines), buf.String())
	}
	for i, l := range lines {
		for _, k := range baseKeys {
			if _, ok := l[k]; !ok {
				t.Errorf("line %d lacks %q: %v", i, k, l)
			}
		}
	}
	if lines[1][KeyTarget] != "prod-pg" || lines[2][KeyJob] != "backup" || lines[3][KeyComponent] != "repo" {
		t.Errorf("fields not kept: %v", lines)
	}
	if g, ok := lines[3]["pack"].(map[string]any); !ok || g["size"] != float64(3) {
		t.Errorf("group lost: %v", lines[3])
	}
	// A key passed in the call is not also added empty.
	if strings.Count(strings.Split(buf.String(), "\n")[2], `"job"`) != 1 {
		t.Errorf("job key duplicated: %s", strings.Split(buf.String(), "\n")[2])
	}
}

func TestLogFormats(t *testing.T) {
	tests := []struct {
		format string
		tty    bool
		json   bool
	}{
		{FormatJSON, true, true},
		{FormatText, false, false},
		{"", true, false},
		{"", false, true},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		log, err := NewLogger(&buf, tt.format, tt.tty, slog.LevelInfo)
		if err != nil {
			t.Fatal(err)
		}
		log.Info("hello", KeyTarget, "prod-pg")
		isJSON := strings.HasPrefix(buf.String(), "{")
		if isJSON != tt.json {
			t.Errorf("format %q tty %v: output %q", tt.format, tt.tty, buf.String())
		}
		if !tt.json && !strings.Contains(buf.String(), "target=prod-pg job=\"\" run_id=\"\" component=\"\"") {
			t.Errorf("text line lacks base fields: %q", buf.String())
		}
	}
	if _, err := NewLogger(io.Discard, "xml", false, slog.LevelInfo); err == nil {
		t.Error("NewLogger accepted format xml")
	}
}

func TestLogLevel(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewLogger(&buf, FormatJSON, false, slog.LevelInfo)
	log.Debug("hidden")
	log.Info("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Errorf("level filter: %q", buf.String())
	}
	Discard().Error("nothing") // must not panic
}

// TestSecretLeakLogs logs secrets through every slog path of the
// configured handlers.
func TestSecretLeakLogs(t *testing.T) {
	canaries := leaktest.New(t, 1)
	s := secrets.New([]byte(canaries[0]))
	var outs []string
	for _, f := range []string{FormatJSON, FormatText} {
		var buf bytes.Buffer
		log, _ := NewLogger(&buf, f, false, slog.LevelDebug)
		log.With("password", s).WithGroup("g").Info("x", "s", s, "p", &s)
		log.Error("failed", "error", secrets.ScrubError(errors.New("auth "+string(canaries[0])), s))
		outs = append(outs, buf.String())
	}
	leaktest.Scan(t, "log lines", canaries, outs...)
}

// wantMetrics lists every metric of PRD 7.13 with its labels (SDD 10.2).
var wantMetrics = map[string][]string{
	"pentym_capture_lag_seconds":              {"target"},
	"pentym_pg_slot_retained_wal_bytes":       {"target"},
	"pentym_mongo_oplog_window_seconds":       {"target"},
	"pentym_pitr_window_seconds":              {"target"},
	"pentym_last_success_timestamp_seconds":   {"job", "target"},
	"pentym_job_duration_seconds":             {"job"},
	"pentym_job_failures_total":               {"job", "target"},
	"pentym_repo_bytes":                       {"kind"},
	"pentym_dedup_ratio":                      nil,
	"pentym_backend_request_duration_seconds": {"op"},
	"pentym_backend_request_errors_total":     {"op"},
	"pentym_upload_queue_depth":               nil,
	"pentym_events_detected_total":            {"type"},
	"pentym_lease_held":                       {"target"},
	"pentym_verify_last_result":               {"level", "target"},
}

func TestMetricsDeclared(t *testing.T) {
	m := NewMetrics()
	// Vectors appear in a scrape once a label set is used.
	m.CaptureLag.WithLabelValues("a").Set(1)
	m.PGSlotRetainedWAL.WithLabelValues("a").Set(1)
	m.MongoOplogWindow.WithLabelValues("a").Set(1)
	m.PITRWindow.WithLabelValues("a").Set(1)
	m.LastSuccess.WithLabelValues("a", "backup").Set(1)
	m.JobDuration.WithLabelValues("backup").Observe(1)
	m.JobFailures.WithLabelValues("a", "backup").Inc()
	m.RepoBytes.WithLabelValues("packs").Set(1)
	m.DedupRatio.Set(2)
	m.BackendRequestDuration.WithLabelValues("put").Observe(0.1)
	m.BackendRequestErrors.WithLabelValues("put").Inc()
	m.UploadQueueDepth.Set(3)
	m.EventsDetected.WithLabelValues("drop").Inc()
	m.LeaseHeld.WithLabelValues("a").Set(1)
	m.VerifyLastResult.WithLabelValues("a", "quick").Set(1)

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, f := range families {
		var labels []string
		for _, l := range f.GetMetric()[0].GetLabel() {
			labels = append(labels, l.GetName())
		}
		sort.Strings(labels)
		got[f.GetName()] = labels
		if f.GetHelp() == "" {
			t.Errorf("%s has no help text", f.GetName())
		}
	}
	if len(got) != len(wantMetrics) {
		t.Errorf("registry has %d metrics, want %d: %v", len(got), len(wantMetrics), got)
	}
	for name, labels := range wantMetrics {
		g, ok := got[name]
		if !ok {
			t.Errorf("metric %s not declared", name)
			continue
		}
		if strings.Join(g, ",") != strings.Join(labels, ",") {
			t.Errorf("%s labels = %v, want %v", name, g, labels)
		}
		for _, l := range g {
			if l == "table" || l == "collection" {
				t.Errorf("%s is labelled per %s", name, l)
			}
		}
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `pentym_lease_held{target="a"} 1`) {
		t.Errorf("/metrics output lacks pentym_lease_held:\n%s", rec.Body.String())
	}
}

func TestJob(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewLogger(&buf, FormatJSON, false, slog.LevelInfo)
	tel := New(log, nil)
	clock := time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC)
	tel.Now = func() time.Time { return clock }

	ctx, job := tel.StartJob(context.Background(), "ops", "prod-pg", "backup")
	if JobFrom(ctx) != job || JobFrom(context.Background()) != nil {
		t.Error("JobFrom does not return the job")
	}
	if len(job.RunID) != 12 || strings.Trim(job.RunID, "0123456789abcdef") != "" {
		t.Errorf("run ID %q is not 12 hex characters", job.RunID)
	}
	if NewRunID() == job.RunID {
		t.Error("run IDs repeat")
	}
	job.Log.Info("working")
	clock = clock.Add(90 * time.Second)
	job.End(nil)

	_, failed := tel.StartJob(context.Background(), "ops", "prod-pg", "verify")
	failed.End(errors.New("L3 mismatch"))

	m := tel.Metrics
	if v := testutil.ToFloat64(m.LastSuccess.WithLabelValues("prod-pg", "backup")); v != float64(clock.Unix()) {
		t.Errorf("last success = %v", v)
	}
	if v := testutil.ToFloat64(m.JobFailures.WithLabelValues("prod-pg", "verify")); v != 1 {
		t.Errorf("failures = %v", v)
	}
	if n := testutil.CollectAndCount(m.JobDuration); n != 2 {
		t.Errorf("duration series = %d, want 2", n)
	}

	lines := jsonLines(t, buf.String())
	if len(lines) != 5 {
		t.Fatalf("got %d log lines, want 5:\n%s", len(lines), buf.String())
	}
	for _, l := range lines[:3] {
		if l[KeyTarget] != "prod-pg" || l[KeyJob] != "backup" || l[KeyRunID] != job.RunID || l[KeyComponent] != "ops" {
			t.Errorf("job line lacks its fields: %v", l)
		}
	}
	if lines[2]["msg"] != "job finished" || lines[4]["msg"] != "job failed" || lines[4]["error"] != "L3 mismatch" || lines[4]["level"] != "ERROR" {
		t.Errorf("end lines = %v / %v", lines[2], lines[4])
	}
	if c := tel.Component("agent"); c == nil {
		t.Error("Component returned nil")
	}
}

// fakeClock returns a clock that advances only when told to.
func fakeClock() (func() time.Time, func(time.Duration)) {
	now := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

func TestProgressTerminal(t *testing.T) {
	now, advance := fakeClock()
	var buf bytes.Buffer
	p := NewProgress(ProgressOptions{Mode: ProgressTerminal, W: &buf, Label: "backup prod-pg", Total: 50 << 30, Unicode: true, Now: now})
	advance(10 * time.Second)
	p.Add(1 << 30)
	p.SetDedup(3.14)
	p.Render()
	want := "\r… backup prod-pg: 1.0 GiB of 50.0 GiB · 102.4 MiB/s · 8m 10s left · dedupe 3.1×"
	if buf.String() != want {
		t.Errorf("line =\n%q\nwant\n%q", buf.String(), want)
	}

	// A shorter line pads over the longer one; Done erases the line.
	buf.Reset()
	p2 := NewProgress(ProgressOptions{Mode: ProgressTerminal, W: &buf, Unicode: false, Now: now})
	advance(time.Second)
	p2.Add(2048)
	p2.Render()
	first := buf.String()
	if first != "\r... 2.0 KiB - 2.0 KiB/s" {
		t.Errorf("ASCII, no total: %q", first)
	}
	p2.lastLen += 5 // pretend the previous line was longer
	buf.Reset()
	p2.Render()
	if !strings.HasSuffix(buf.String(), "     ") {
		t.Errorf("no padding over a longer line: %q", buf.String())
	}
	buf.Reset()
	p2.Done()
	if got := buf.String(); got != "\r"+strings.Repeat(" ", len(first)-1+5)+"\r" && !strings.HasPrefix(got, "\r ") {
		t.Errorf("Done did not erase the line: %q", got)
	}
}

func TestProgressLog(t *testing.T) {
	now, advance := fakeClock()
	var buf bytes.Buffer
	log, _ := NewLogger(&buf, FormatJSON, false, slog.LevelInfo)
	p := NewProgress(ProgressOptions{Mode: ProgressLog, Logger: log, Label: "backup prod-pg", Total: 1000, Now: now})
	advance(2 * time.Second)
	p.Add(500)
	p.Render()
	p.Done()
	lines := jsonLines(t, buf.String())
	if len(lines) != 2 {
		t.Fatalf("got %d lines:\n%s", len(lines), buf.String())
	}
	l := lines[0]
	if l["msg"] != "progress" || l["bytes"] != float64(500) || l["total"] != float64(1000) ||
		l["bytes_per_second"] != float64(250) || l["eta_seconds"] != float64(2) || l["done"] != false || l["label"] != "backup prod-pg" {
		t.Errorf("progress line = %v", l)
	}
	if lines[1]["done"] != true {
		t.Errorf("final line = %v", lines[1])
	}
}

func TestProgressJSON(t *testing.T) {
	now, advance := fakeClock()
	var buf bytes.Buffer
	p := NewProgress(ProgressOptions{Mode: ProgressJSON, W: &buf, Now: now})
	advance(4 * time.Second)
	p.Add(4096)
	p.SetDedup(2)
	p.Render()
	p.Done()
	lines := jsonLines(t, buf.String())
	if len(lines) != 2 {
		t.Fatalf("got %d events:\n%s", len(lines), buf.String())
	}
	e := lines[0]
	if e["event"] != "progress" || e["bytes"] != float64(4096) || e["bytes_per_second"] != float64(1024) ||
		e["dedup_ratio"] != float64(2) || e["time"] != "2026-11-25T13:00:04Z" {
		t.Errorf("event = %v", e)
	}
	if _, ok := e["total"]; ok {
		t.Error("unknown total was included")
	}
	if _, ok := e["eta_seconds"]; ok {
		t.Error("eta without a total was included")
	}
	if lines[1]["event"] != "done" {
		t.Errorf("last event = %v", lines[1])
	}
}

func TestProgressOff(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress(ProgressOptions{Mode: ProgressOff, W: &buf})
	p.Start()
	p.Add(10)
	p.Done()
	if buf.Len() != 0 {
		t.Errorf("off mode wrote %q", buf.String())
	}
}

// TestProgressTicks runs the real ticker; goleak in TestMain checks that
// Done stops its goroutine.
func TestProgressTicks(t *testing.T) {
	var buf syncBuffer
	p := NewProgress(ProgressOptions{Mode: ProgressJSON, W: &buf, Interval: 5 * time.Millisecond})
	p.Start()
	p.Start() // a second Start is ignored
	p.Add(1)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), `"event":"progress"`) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	p.Done()
	p.Done() // a second Done is harmless
	if !strings.Contains(buf.String(), `"event":"progress"`) || !strings.Contains(buf.String(), `"event":"done"`) {
		t.Errorf("ticker output = %q", buf.String())
	}

	// Done without Start does not block.
	NewProgress(ProgressOptions{Mode: ProgressJSON, W: io.Discard}).Done()
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestMetricsDocumented checks docs/reference/metrics.md lists exactly the
// declared metrics.
func TestMetricsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/reference/metrics.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^[|] `(pentym_[a-z_]+)`").FindAllStringSubmatch(string(doc), -1) {
		documented[m[1]] = true
	}
	for name := range wantMetrics {
		if !documented[name] {
			t.Errorf("%s is missing from docs/reference/metrics.md", name)
		}
		delete(documented, name)
	}
	for name := range documented {
		t.Errorf("docs/reference/metrics.md lists %s, which is not declared", name)
	}
}
