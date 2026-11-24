// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024*1024 - 1, "1.0 MiB"},
		{16 * 1024 * 1024, "16.0 MiB"},
		{19756849561, "18.4 GiB"},
		{5 << 40, "5.0 TiB"},
		{-2048, "-2.0 KiB"},
	}
	for _, tt := range tests {
		if got := Bytes(tt.n); got != tt.want {
			t.Errorf("Bytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{0, "0s"},
		{999 * time.Millisecond, "0s"},
		{4 * time.Second, "4s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m 0s"},
		{2*time.Minute + 14*time.Second, "2m 14s"},
		{2*time.Hour + 14*time.Minute + 59*time.Second, "2h 14m"},
		{7 * 24 * time.Hour, "7d 0h"},
		{6*24*time.Hour + 22*time.Hour + 30*time.Minute, "6d 22h"},
	}
	for _, tt := range tests {
		if got := Duration(tt.d); got != tt.want {
			t.Errorf("Duration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestTime(t *testing.T) {
	wat := time.FixedZone("WAT", 3600)
	now := time.Date(2026, 11, 22, 15, 0, 0, 0, wat)
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"same day", time.Date(2026, 11, 22, 14, 3, 12, 0, wat), "14:03:12 WAT"},
		{"same day from UTC", time.Date(2026, 11, 22, 13, 3, 12, 0, time.UTC), "14:03:12 WAT"},
		{"other day", time.Date(2026, 11, 21, 3, 12, 0, 0, wat), "2026-11-21 03:12:00 WAT"},
		{"UTC date differs from local date", time.Date(2026, 11, 21, 23, 30, 0, 0, time.UTC), "00:30:00 WAT"},
	}
	for _, tt := range tests {
		if got := Time(tt.t, now, wat); got != tt.want {
			t.Errorf("%s: Time = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestJSONTime(t *testing.T) {
	wat := time.FixedZone("WAT", 3600)
	got := JSONTime(time.Date(2026, 11, 22, 14, 3, 12, 500, wat))
	if want := "2026-11-22T13:03:12Z"; got != want {
		t.Errorf("JSONTime = %q, want %q", got, want)
	}
}
