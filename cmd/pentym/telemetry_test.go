// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spf13/cobra"
)

// demoResult is what the test-only demo command produced.
type demoResult struct {
	stdout, stderr string
	lastSuccess    float64
}

// runDemo runs a test-only command, built here and never shipped, that
// logs, records a job metric and shows progress, as a real command will.
// terminal pretends stderr is a terminal.
func runDemo(t *testing.T, terminal bool, args ...string) demoResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := newApp(strings.NewReader(""), &stdout, &stderr, func(k string) string {
		if k == "LANG" {
			return "en_US.UTF-8"
		}
		return ""
	})
	root := a.rootCommand()
	var res demoResult
	root.AddCommand(&cobra.Command{
		Use:    "telemetry-demo",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if terminal {
				a.printer.ErrTerminal = true
			}
			_, job := a.tel.StartJob(cmd.Context(), "test", "prod-pg", "backup")
			job.Log.Debug("streaming", "bytes", 512)

			p, err := a.progress("backup prod-pg", 1024)
			if err != nil {
				return err
			}
			p.Start()
			p.Add(512)
			p.Render()
			p.Done()

			job.End(nil)
			res.lastSuccess = testutil.ToFloat64(a.tel.Metrics.LastSuccess.WithLabelValues("prod-pg", "backup"))
			_, err = stdout.WriteString("result\n")
			return err
		},
	})
	root.SetArgs(append(args, "telemetry-demo"))
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("demo failed: %v", err)
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

func TestTelemetryModes(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		r := runDemo(t, true)
		if r.stdout != "result\n" {
			t.Errorf("stdout = %q", r.stdout)
		}
		if !strings.Contains(r.stderr, "\r… backup prod-pg: 512 B of 1.0 KiB") {
			t.Errorf("no progress line on stderr: %q", r.stderr)
		}
		if !strings.HasSuffix(r.stderr, "\r") {
			t.Errorf("progress line not erased: %q", r.stderr)
		}
		if strings.Contains(r.stderr, "job started") {
			t.Errorf("logs shown without --debug: %q", r.stderr)
		}
		if r.lastSuccess == 0 {
			t.Error("no job metric recorded")
		}
	})

	t.Run("log lines", func(t *testing.T) {
		r := runDemo(t, false)
		if r.stdout != "result\n" {
			t.Errorf("stdout = %q", r.stdout)
		}
		var progress int
		for _, line := range strings.Split(strings.TrimSpace(r.stderr), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("stderr line is not a JSON log line: %q", line)
			}
			if m["msg"] == "progress" {
				progress++
			}
		}
		if progress != 2 || r.lastSuccess == 0 {
			t.Errorf("progress lines = %d, metric = %v\n%s", progress, r.lastSuccess, r.stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		r := runDemo(t, true, "--json")
		if !strings.Contains(r.stderr, `"event":"progress"`) || !strings.Contains(r.stderr, `"event":"done"`) {
			t.Errorf("no JSON progress events on stderr: %q", r.stderr)
		}
		if strings.Contains(r.stdout, "event") {
			t.Errorf("progress events on stdout: %q", r.stdout)
		}
		if r.lastSuccess == 0 {
			t.Error("no job metric recorded")
		}
	})

	t.Run("debug logs carry the base fields", func(t *testing.T) {
		r := runDemo(t, false, "--debug", "--log-format=json")
		var found bool
		for _, line := range strings.Split(strings.TrimSpace(r.stderr), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) != nil || m["msg"] != "streaming" {
				continue
			}
			found = true
			if m["target"] != "prod-pg" || m["job"] != "backup" || m["component"] != "test" || len(m["run_id"].(string)) != 12 {
				t.Errorf("debug line fields = %v", m)
			}
		}
		if !found {
			t.Errorf("no debug line:\n%s", r.stderr)
		}
	})

	t.Run("quiet shows no progress", func(t *testing.T) {
		r := runDemo(t, true, "-q")
		if r.stderr != "" {
			t.Errorf("stderr with -q = %q", r.stderr)
		}
	})
}
