// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// Style is a set of text attributes rendered with standard ANSI codes, so
// output follows the user's terminal theme (Brand 10.2).
type Style uint8

// Styles. Colours carry status only; bold marks target names and the next
// command; dim marks timestamps, IDs and hints.
const (
	Bold Style = 1 << iota
	Dim
	Green
	Yellow
	Red
)

// Plain is the empty style.
const Plain Style = 0

var styleCodes = []struct {
	s    Style
	code string
}{
	{Bold, "1"}, {Dim, "2"}, {Green, "32"}, {Yellow, "33"}, {Red, "31"},
}

// Paint wraps s in the ANSI codes for st when color is true.
func Paint(s string, st Style, color bool) string {
	if !color || st == Plain || s == "" {
		return s
	}
	var codes []string
	for _, c := range styleCodes {
		if st&c.s != 0 {
			codes = append(codes, c.code)
		}
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + s + "\x1b[0m"
}

// LevelStyle returns the colour for a status level.
func LevelStyle(l msg.Level) Style {
	switch l {
	case msg.LevelOK:
		return Green
	case msg.LevelWarning:
		return Yellow
	default:
		return Red
	}
}

// Symbol is a status or flow marker (Brand 10.3).
type Symbol int

// Symbols.
const (
	SymOK Symbol = iota
	SymWarning
	SymFailed
	SymNext
	SymProgress
)

var symbols = map[Symbol][2]string{
	// {Unicode, ASCII fallback}
	SymOK:       {"✓", "OK"},
	SymWarning:  {"!", "WARN"},
	SymFailed:   {"✗", "FAIL"},
	SymNext:     {"→", "->"},
	SymProgress: {"…", "..."},
}

// Text returns the symbol, or its ASCII fallback when unicode is false.
func (s Symbol) Text(unicode bool) string {
	pair := symbols[s]
	if unicode {
		return pair[0]
	}
	return pair[1]
}

// LevelSymbol returns the symbol for a status level.
func LevelSymbol(l msg.Level) Symbol {
	switch l {
	case msg.LevelOK:
		return SymOK
	case msg.LevelWarning:
		return SymWarning
	default:
		return SymFailed
	}
}
