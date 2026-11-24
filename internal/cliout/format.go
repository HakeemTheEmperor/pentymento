// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"fmt"
	"math"
	"time"
)

var byteUnits = []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// Bytes formats a size in binary units: "512 B", "1.0 KiB", "18.4 GiB".
func Bytes(n int64) string {
	if n < 0 {
		return "-" + Bytes(-n)
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	unit := ""
	for _, u := range byteUnits {
		v /= 1024
		unit = u
		if math.Round(v*10)/10 < 1024 {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", v, unit)
}

// Duration formats a duration with its two largest units, truncated:
// "4s", "2m 14s", "2h 14m", "7d 0h". Negative durations count as zero.
func Duration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	s := int64(d / time.Second)
	days, s := s/86400, s%86400
	hours, s := s/3600, s%3600
	mins, secs := s/60, s%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm %ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// Time formats t in loc for human output with the zone abbreviation:
// "14:03:12 WAT" when t falls on the same local day as now, otherwise
// "2026-11-21 03:12:00 WAT".
func Time(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	if ty == ny && tm == nm && td == nd {
		return t.Format("15:04:05 MST")
	}
	return t.Format("2006-01-02 15:04:05 MST")
}

// JSONTime formats t for --json output: RFC 3339 in UTC.
func JSONTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
