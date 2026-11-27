// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultPITRWindow is how far back point-in-time recovery reaches when
// retention.pitr_window is not set (PRD 7.10).
const DefaultPITRWindow = 7 * 24 * time.Hour

// Retention configures what the repository keeps (PRD 7.10). Prune adds
// the snapshot rules, events and journal.
type Retention struct {
	// PITRWindow is how far back point-in-time recovery must reach; zero
	// means DefaultPITRWindow.
	PITRWindow Duration `yaml:"pitr_window"`
}

// Window is the configured PITR window.
func (r Retention) Window() time.Duration {
	if r.PITRWindow == 0 {
		return DefaultPITRWindow
	}
	return time.Duration(r.PITRWindow)
}

// CatchUp configures catch-up (PRD 5.3).
type CatchUp struct {
	// Auto lets status run catch-up itself when a target's lease has
	// expired; off, status prints the command instead.
	Auto bool `yaml:"auto"`
}

// Duration is a length of time written with days as well as Go's units,
// such as 7d, 36h or 1d12h.
type Duration time.Duration

// ParseDuration reads a Duration: whole days (d) followed by Go duration
// units (h, m, s), at least one of them, greater than zero.
func ParseDuration(s string) (time.Duration, error) {
	bad := fmt.Errorf("%q is not a duration; write it like 7d, 36h or 1d12h", s)
	var d time.Duration
	rest := s
	if days, after, ok := strings.Cut(s, "d"); ok {
		n, err := strconv.ParseUint(days, 10, 16)
		if err != nil || days == "" || days[0] == '+' {
			return 0, bad
		}
		d, rest = time.Duration(n)*24*time.Hour, after // #nosec G115 -- at most 65535 days
	}
	if rest != "" {
		if rest[0] < '0' || rest[0] > '9' {
			return 0, bad
		}
		r, err := time.ParseDuration(rest)
		if err != nil {
			return 0, bad
		}
		d += r
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q: a duration must be greater than zero", s)
	}
	return d, nil
}

// UnmarshalYAML reads a duration such as 7d.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a duration is a single value, such as 7d", n.Line)
	}
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// String writes the duration in the form ParseDuration reads.
func (d Duration) String() string {
	t := time.Duration(d)
	days := t / (24 * time.Hour)
	rest := t % (24 * time.Hour)
	out := ""
	if days > 0 {
		out = strconv.FormatInt(int64(days), 10) + "d"
	}
	if rest > 0 || days == 0 {
		r := rest.String() // such as 12h0m0s: drop the zero units
		if strings.HasSuffix(r, "m0s") {
			r = strings.TrimSuffix(r, "0s")
		}
		if strings.HasSuffix(r, "h0m") {
			r = strings.TrimSuffix(r, "0m")
		}
		out += r
	}
	return out
}
