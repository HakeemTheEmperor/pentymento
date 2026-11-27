// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"7d": 7 * 24 * time.Hour, "36h": 36 * time.Hour, "1d12h": 36 * time.Hour, "90m": 90 * time.Minute,
		"2d30m": 48*time.Hour + 30*time.Minute, "1s": time.Second,
	} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
		if s := Duration(got).String(); s != in && in != "36h" && in != "90m" {
			t.Errorf("Duration(%v).String() = %q, want %q", got, s, in)
		}
		if back, err := ParseDuration(Duration(got).String()); err != nil || back != got {
			t.Errorf("%q does not read back: %v, %v", Duration(got).String(), back, err)
		}
	}
	for _, in := range []string{"", "d", "7", "7days", "-1d", "+1d", "1d-1h", "0d", "0s", "1w", "1dd", "d12h", "1.5d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) succeeded", in)
		}
	}
}

func TestRetentionAndCatchUp(t *testing.T) {
	dir := files(t, map[string]string{"config.yaml": "retention:\n  pitr_window: 3d\ncatch_up:\n  auto: true\n"})
	c, err := Load(filepath.Join(dir, "config.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Retention.Window() != 72*time.Hour || !c.CatchUp.Auto {
		t.Errorf("retention = %v, catch_up = %+v", c.Retention.Window(), c.CatchUp)
	}
	c, err = Load(filepath.Join(dir, "config.yaml"), []string{"PENTYM_RETENTION__PITR_WINDOW=12h", "PENTYM_CATCH_UP__AUTO=false"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Retention.Window() != 12*time.Hour || c.CatchUp.Auto {
		t.Errorf("overridden: retention = %v, catch_up = %+v", c.Retention.Window(), c.CatchUp)
	}

	empty := files(t, map[string]string{"config.yaml": "targets: {}\n"})
	c, err = Load(filepath.Join(empty, "config.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Retention.Window() != DefaultPITRWindow || c.CatchUp.Auto {
		t.Errorf("defaults: retention = %v, catch_up = %+v", c.Retention.Window(), c.CatchUp)
	}

	bad := files(t, map[string]string{"config.yaml": "retention:\n  pitr_window: 1 week\n"})
	if _, err := Load(filepath.Join(bad, "config.yaml"), nil); err == nil || !strings.Contains(err.Error(), "not a duration") {
		t.Errorf("a bad window = %v", err)
	}
}
