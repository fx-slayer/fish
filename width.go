package main

import "strings"

const tabStop = 8

// wideRanges lists the code point ranges rendered two cells wide by terminals,
// derived from the East Asian Wide and Fullwidth categories.
var wideRanges = [][2]rune{
	{0x1100, 0x115F},
	{0x2E80, 0x303E},
	{0x3041, 0x33FF},
	{0x3400, 0x4DBF},
	{0x4E00, 0x9FFF},
	{0xA000, 0xA4CF},
	{0xA960, 0xA97F},
	{0xAC00, 0xD7A3},
	{0xF900, 0xFAFF},
	{0xFE10, 0xFE19},
	{0xFE30, 0xFE6F},
	{0xFF00, 0xFF60},
	{0xFFE0, 0xFFE6},
	{0x1F300, 0x1F64F},
	{0x1F900, 0x1F9FF},
	{0x20000, 0x2FFFD},
	{0x30000, 0x3FFFD},
}

// zeroRanges lists combining marks and format controls that occupy no cell.
var zeroRanges = [][2]rune{
	{0x0300, 0x036F},
	{0x200B, 0x200F},
	{0x2028, 0x202E},
	{0xFE00, 0xFE0F},
	{0xFEFF, 0xFEFF},
}

func inRanges(r rune, ranges [][2]rune) bool {
	for _, rg := range ranges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// runeWidth reports how many terminal cells r occupies. Tabs are not handled
// here because their width depends on the current column.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7F && r < 0xA0):
		return 0 // control characters are not rendered
	case inRanges(r, zeroRanges):
		return 0
	case inRanges(r, wideRanges):
		return 2
	default:
		return 1
	}
}

// stringWidth reports how many terminal cells s occupies when printed at column 0.
func stringWidth(s string) int {
	col := 0
	for _, r := range s {
		if r == '\t' {
			col += tabStop - col%tabStop
			continue
		}
		col += runeWidth(r)
	}
	return col
}

// foldLine splits s into chunks that each fit within width cells, mirroring how
// the terminal would wrap the line. An empty line yields one empty chunk so it
// still occupies a row. A single rune wider than width is emitted on its own
// chunk rather than looping forever.
func foldLine(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	if s == "" {
		return []string{""}
	}
	out := make([]string, 0, stringWidth(s)/width+1)
	var b strings.Builder
	col := 0
	for _, r := range s {
		w := runeWidth(r)
		if r == '\t' {
			w = tabStop - col%tabStop
		}
		if col+w > width && col > 0 {
			out = append(out, b.String())
			b.Reset()
			col = 0
			if r == '\t' {
				w = tabStop
			}
		}
		b.WriteRune(r)
		col += w
	}
	return append(out, b.String())
}

// truncateWidth cuts s so it occupies at most width cells, preventing the status
// line from wrapping onto a row it does not own.
func truncateWidth(s string, width int) string {
	if width < 1 {
		return ""
	}
	col := 0
	for i, r := range s {
		w := runeWidth(r)
		if r == '\t' {
			w = tabStop - col%tabStop
		}
		if col+w > width {
			return s[:i]
		}
		col += w
	}
	return s
}
