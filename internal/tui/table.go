package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// width is the number of terminal cells s occupies, escape sequences excluded.
func width(s string) int { return ansi.StringWidth(s) }

// truncRight shortens s to at most w cells, marking the cut with an ellipsis at the end.
func truncRight(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// truncLeft shortens s to at most w cells, keeping the end: for a path, the end is the part
// that says which directory it is.
func truncLeft(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := width(s)
	if n <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return ansi.TruncateLeft(s, n-w+1, "…")
}

// padRight pads s with spaces to exactly w cells, truncating it if it is wider.
func padRight(s string, w int) string {
	s = truncRight(s, w)
	if n := width(s); n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}

func padLeft(s string, w int) string {
	s = truncRight(s, w)
	if n := width(s); n < w {
		s = strings.Repeat(" ", w-n) + s
	}
	return s
}

// spread puts left and right on one line of exactly w cells, with right flush right. When
// they do not both fit, left is truncated first.
func spread(left, right string, w int) string {
	rw := width(right)
	if rw >= w {
		return padRight(right, w)
	}
	return padRight(left, w-rw-1) + " " + right
}

// column is one column of a table.
type column struct {
	title string
	width int  // fixed width, or the minimum for a flex column
	flex  bool // takes what is left over
	right bool // right-aligned
}

// cell is one table cell: plain text and the style it is drawn in.
type cell struct {
	text  string
	style lipgloss.Style
}

// layout assigns each column its width in a table total cells wide, with gap cells
// between columns. Flex columns share what the fixed ones leave, never less than their
// minimum; if even that does not fit, the line is cut at the edge rather than wrapped.
func layout(cols []column, total, gap int) []int {
	ws := make([]int, len(cols))
	fixed, flex := 0, 0
	for i, c := range cols {
		ws[i] = c.width
		fixed += c.width
		if c.flex {
			flex++
		}
	}
	fixed += gap * (len(cols) - 1)
	if flex == 0 || total <= fixed {
		return ws
	}
	extra := total - fixed
	for i, c := range cols {
		if !c.flex {
			continue
		}
		share := extra / flex
		ws[i] += share
		extra -= share
		flex--
	}
	return ws
}

// renderRow draws one row of cells in the given widths, cut to total cells.
func renderRow(cells []cell, cols []column, ws []int, gap, total int) string {
	var b strings.Builder
	for i, c := range cells {
		if i >= len(cols) {
			break
		}
		if i > 0 {
			b.WriteString(strings.Repeat(" ", gap))
		}
		var text string
		if cols[i].right {
			text = padLeft(c.text, ws[i])
		} else {
			text = padRight(c.text, ws[i])
		}
		b.WriteString(c.style.Render(text))
	}
	return cutTo(b.String(), total)
}

// cutTo cuts a styled line at w cells and pads it to exactly w.
func cutTo(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) > w {
		s = ansi.Truncate(s, w, "")
	}
	if n := width(s); n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}

// visibleWindow returns the first row to show so that row sel is inside a window of h rows
// starting near offset.
func visibleWindow(offset, sel, n, h int) int {
	if h <= 0 || n <= h {
		return 0
	}
	if sel < offset {
		offset = sel
	}
	if sel >= offset+h {
		offset = sel - h + 1
	}
	if offset > n-h {
		offset = n - h
	}
	if offset < 0 {
		offset = 0
	}
	return offset
}
