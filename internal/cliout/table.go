// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"io"
	"strings"
	"unicode/utf8"
)

// columnGap is the space between table columns.
const columnGap = 2

// Column is a table column. When a table is wider than the terminal, the
// columns with the lowest Priority are dropped first (the rightmost first
// among equals) instead of wrapping (Brand 10.4). At least one column is
// always kept.
type Column struct {
	Header   string
	Priority int
}

// Cell is a table cell: plain text with an optional style.
type Cell struct {
	Text  string
	Style Style
}

// Table is an aligned table with a header row and no borders.
type Table struct {
	Columns []Column
	Rows    [][]Cell
}

// RenderTable writes t to w. width is the terminal width, or 0 for no
// limit. color enables ANSI styles.
func RenderTable(w io.Writer, t Table, width int, color bool) error {
	keep := visibleColumns(t, width)
	widths := columnWidths(t, keep)

	var b strings.Builder
	writeRow := func(cells []Cell) {
		for i, c := range keep {
			var cell Cell
			if c < len(cells) {
				cell = cells[c]
			}
			b.WriteString(Paint(cell.Text, cell.Style, color))
			if i < len(keep)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-textWidth(cell.Text)+columnGap))
			}
		}
		b.WriteString("\n")
	}
	header := make([]Cell, len(t.Columns))
	for i, c := range t.Columns {
		header[i] = Cell{Text: c.Header}
	}
	writeRow(header)
	for _, r := range t.Rows {
		writeRow(r)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// visibleColumns returns the indexes of the columns that fit in width.
func visibleColumns(t Table, width int) []int {
	keep := make([]int, len(t.Columns))
	for i := range keep {
		keep[i] = i
	}
	for width > 0 && len(keep) > 1 && totalWidth(columnWidths(t, keep)) > width {
		drop := len(keep) - 1
		for i := len(keep) - 1; i >= 0; i-- {
			if t.Columns[keep[i]].Priority < t.Columns[keep[drop]].Priority {
				drop = i
			}
		}
		keep = append(keep[:drop], keep[drop+1:]...)
	}
	return keep
}

func columnWidths(t Table, keep []int) []int {
	widths := make([]int, len(keep))
	for i, c := range keep {
		widths[i] = textWidth(t.Columns[c].Header)
		for _, r := range t.Rows {
			if c < len(r) {
				widths[i] = max(widths[i], textWidth(r[c].Text))
			}
		}
	}
	return widths
}

func totalWidth(widths []int) int {
	total := 0
	for _, w := range widths {
		total += w
	}
	return total + columnGap*(len(widths)-1)
}

// textWidth is the display width of s, counted in runes. Wide East Asian
// characters are not accounted for.
func textWidth(s string) int { return utf8.RuneCountInString(s) }
