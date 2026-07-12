package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// Minimal ANSI styling that degrades to plain text when not a TTY.
type styler struct{ on bool }

func newStyler() styler {
	return styler{on: term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == ""}
}

func (s styler) wrap(code, text string) string {
	if !s.on {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s styler) bold(t string) string   { return s.wrap("1", t) }
func (s styler) dim(t string) string    { return s.wrap("2", t) }
func (s styler) green(t string) string  { return s.wrap("32", t) }
func (s styler) yellow(t string) string { return s.wrap("33", t) }
func (s styler) red(t string) string    { return s.wrap("31", t) }
func (s styler) cyan(t string) string   { return s.wrap("36", t) }

// table renders aligned columns.
type table struct {
	header []string
	rows   [][]string
}

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) render(indent string) string {
	all := append([][]string{t.header}, t.rows...)
	widths := make([]int, len(t.header))
	for _, row := range all {
		for i, c := range row {
			if i < len(widths) && visibleLen(c) > widths[i] {
				widths[i] = visibleLen(c)
			}
		}
	}
	var b strings.Builder
	for ri, row := range all {
		b.WriteString(indent)
		for i, c := range row {
			pad := widths[i] - visibleLen(c) + 2
			b.WriteString(c)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
		b.WriteString("\n")
		if ri == 0 {
			b.WriteString(indent)
			for i, w := range widths {
				b.WriteString(strings.Repeat("─", w))
				if i < len(widths)-1 {
					b.WriteString("  ")
				}
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// visibleLen ignores ANSI escape sequences.
func visibleLen(s string) int {
	n, in := 0, false
	for _, r := range s {
		switch {
		case in:
			if r == 'm' {
				in = false
			}
		case r == '\x1b':
			in = true
		default:
			n++
		}
	}
	return n
}

func usd(v float64) string { return fmt.Sprintf("$%.2f", v) }

func pct(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }

func mibOrGib(bytes int64) string {
	if bytes >= 1<<30 {
		return fmt.Sprintf("%.1fGi", float64(bytes)/float64(1<<30))
	}
	return fmt.Sprintf("%dMi", bytes>>20)
}

func cpuStr(milli int64) string {
	if milli >= 1000 && milli%1000 == 0 {
		return fmt.Sprintf("%d", milli/1000)
	}
	return fmt.Sprintf("%dm", milli)
}

func resStr(milli, bytes int64) string {
	return cpuStr(milli) + "/" + mibOrGib(bytes)
}
