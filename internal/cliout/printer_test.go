// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestColorEnabled(t *testing.T) {
	tests := []struct {
		name string
		mode string
		tty  bool
		env  map[string]string
		want bool
	}{
		{"auto on a terminal", ColorAuto, true, nil, true},
		{"empty mode is auto", "", true, nil, true},
		{"auto when piped", ColorAuto, false, nil, false},
		{"NO_COLOR", ColorAuto, true, map[string]string{"NO_COLOR": "1"}, false},
		{"NO_COLOR empty is ignored", ColorAuto, true, map[string]string{"NO_COLOR": ""}, true},
		{"CI", ColorAuto, true, map[string]string{"CI": "true"}, false},
		{"TERM=dumb", ColorAuto, true, map[string]string{"TERM": "dumb"}, false},
		{"always when piped", ColorAlways, false, nil, true},
		{"always beats NO_COLOR and CI", ColorAlways, false, map[string]string{"NO_COLOR": "1", "CI": "true"}, true},
		{"never on a terminal", ColorNever, true, nil, false},
	}
	for _, tt := range tests {
		if got := ColorEnabled(tt.mode, tt.tty, env(tt.env)); got != tt.want {
			t.Errorf("%s: ColorEnabled = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestLocaleIsUTF8(t *testing.T) {
	tests := []struct {
		env  map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{"LANG": "en_US.UTF-8"}, true},
		{map[string]string{"LANG": "C.utf8"}, true},
		{map[string]string{"LANG": "C"}, false},
		{map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, false},
		{map[string]string{"LC_CTYPE": "en_GB.UTF-8", "LANG": "C"}, true},
	}
	for _, tt := range tests {
		if got := localeIsUTF8(env(tt.env)); got != tt.want {
			t.Errorf("localeIsUTF8(%v) = %v, want %v", tt.env, got, tt.want)
		}
	}
}

func TestPaint(t *testing.T) {
	if got := Paint("x", Red|Bold, false); got != "x" {
		t.Errorf("Paint without colour = %q", got)
	}
	if got := Paint("x", Red|Bold, true); got != "\x1b[1;31mx\x1b[0m" {
		t.Errorf("Paint(Red|Bold) = %q", got)
	}
	if got := Paint("", Red, true); got != "" {
		t.Errorf("Paint of empty text = %q", got)
	}
	if got := Paint("x", Plain, true); got != "x" {
		t.Errorf("Paint(Plain) = %q", got)
	}
}

func TestSymbols(t *testing.T) {
	tests := []struct {
		s              Symbol
		unicode, ascii string
	}{
		{SymOK, "✓", "OK"},
		{SymWarning, "!", "WARN"},
		{SymFailed, "✗", "FAIL"},
		{SymNext, "→", "->"},
		{SymProgress, "…", "..."},
	}
	for _, tt := range tests {
		if got := tt.s.Text(true); got != tt.unicode {
			t.Errorf("Symbol(%d).Text(true) = %q, want %q", tt.s, got, tt.unicode)
		}
		if got := tt.s.Text(false); got != tt.ascii {
			t.Errorf("Symbol(%d).Text(false) = %q, want %q", tt.s, got, tt.ascii)
		}
	}
	if LevelSymbol(msg.LevelWarning) != SymWarning || LevelStyle(msg.LevelFailed) != Red || LevelStyle(msg.LevelOK) != Green {
		t.Error("level symbol or style mapping is wrong")
	}
}

func TestNewPrinter(t *testing.T) {
	var out, errOut bytes.Buffer
	if _, err := NewPrinter(strings.NewReader(""), &out, &errOut, Settings{Color: "sometimes"}); err == nil {
		t.Error("NewPrinter accepted --color=sometimes")
	}

	// Buffers are not terminals: no colour by default, no prompts.
	p, err := NewPrinter(strings.NewReader(""), &out, &errOut, Settings{Getenv: env(map[string]string{"LANG": "en_US.UTF-8"})})
	if err != nil {
		t.Fatal(err)
	}
	if p.ColorOut || p.ColorErr || p.Interactive || p.Width != 0 {
		t.Errorf("non-terminal printer = %+v", p)
	}

	// --color=always forces colour, except on JSON stdout.
	p, _ = NewPrinter(strings.NewReader(""), &out, &errOut, Settings{Color: ColorAlways, JSON: true, Getenv: env(nil)})
	if p.ColorOut || !p.ColorErr {
		t.Errorf("always with JSON: ColorOut=%v ColorErr=%v, want false, true", p.ColorOut, p.ColorErr)
	}

	// --ascii forces the fallback.
	p, _ = NewPrinter(strings.NewReader(""), &out, &errOut, Settings{ASCII: true, Getenv: env(map[string]string{"LANG": "en_US.UTF-8"})})
	if p.Unicode {
		t.Error("--ascii left Unicode on")
	}
}

func TestPrinterTime(t *testing.T) {
	wat := time.FixedZone("WAT", 3600)
	p := &Printer{Location: wat, Now: func() time.Time { return time.Date(2026, 11, 22, 15, 0, 0, 0, wat) }}
	if got := p.Time(time.Date(2026, 11, 22, 13, 3, 12, 0, time.UTC)); got != "14:03:12 WAT" {
		t.Errorf("Printer.Time = %q", got)
	}
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{" YES \r\n", true},
		{"n\n", false},
		{"\n", false},
		{"", false},
		{"yep\n", false},
	}
	for _, tt := range tests {
		var stderr bytes.Buffer
		p := &Printer{Stderr: &stderr, Interactive: true}
		p.SetStdin(strings.NewReader(tt.input))
		got, err := p.Confirm("continue?")
		if err != nil || got != tt.want {
			t.Errorf("Confirm(%q) = %v, %v; want %v", tt.input, got, err, tt.want)
		}
		if stderr.String() != "continue? [y/N] " {
			t.Errorf("prompt = %q", stderr.String())
		}
	}
}

func TestConfirmTyped(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"prod-pg\n", true},
		{"  prod-pg  \n", true},
		{"prod-mg\n", false},
		{"PROD-PG\n", false},
		{"y\n", false},
		{"", false},
	}
	for _, tt := range tests {
		var stderr bytes.Buffer
		p := &Printer{Stderr: &stderr, Interactive: true}
		p.SetStdin(strings.NewReader(tt.input))
		got, err := p.ConfirmTyped("prod-pg")
		if err != nil || got != tt.want {
			t.Errorf("ConfirmTyped(%q) = %v, %v; want %v", tt.input, got, err, tt.want)
		}
		if stderr.String() != "type prod-pg to continue: " {
			t.Errorf("prompt = %q", stderr.String())
		}
	}
}

func TestConfirmNotInteractive(t *testing.T) {
	var stderr bytes.Buffer
	p := &Printer{Stderr: &stderr}
	p.SetStdin(strings.NewReader("y\n"))
	if ok, err := p.Confirm("continue?"); ok || !errors.Is(err, ErrNotInteractive) {
		t.Errorf("Confirm without a terminal = %v, %v", ok, err)
	}
	if ok, err := p.ConfirmTyped("prod-pg"); ok || !errors.Is(err, ErrNotInteractive) {
		t.Errorf("ConfirmTyped without a terminal = %v, %v", ok, err)
	}
	if stderr.Len() != 0 {
		t.Errorf("a prompt was shown without a terminal: %q", stderr.String())
	}
}

func TestSuccessiveConfirms(t *testing.T) {
	p := &Printer{Stderr: &bytes.Buffer{}, Interactive: true}
	p.SetStdin(strings.NewReader("y\nn\n"))
	first, _ := p.Confirm("first?")
	second, _ := p.Confirm("second?")
	if !first || second {
		t.Errorf("answers = %v, %v; want true, false", first, second)
	}
}
