package main

import (
	"strings"
	"testing"
)

func TestTableAlignment(t *testing.T) {
	tb := &table{header: []string{"A", "LONG-HEADER", "C"}}
	tb.add("x", "y", "z")
	tb.add("longer-cell", "\x1b[32mgreen\x1b[0m", "w")
	out := tb.render("  ")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 { // header, rule, 2 rows
		t.Fatalf("lines: %d\n%s", len(lines), out)
	}
	// ANSI codes must not break alignment: strip them and compare column starts.
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = stripANSI(l)
	}
	cIdx := strings.Index(plain[0], "C")
	for _, row := range plain[2:] {
		if len(row) <= cIdx {
			t.Fatalf("row too short: %q", row)
		}
	}
	if !strings.Contains(plain[1], "─") {
		t.Fatal("header rule missing")
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case in:
			if r == 'm' {
				in = false
			}
		case r == '\x1b':
			in = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestVisibleLen(t *testing.T) {
	if visibleLen("\x1b[32mgreen\x1b[0m") != 5 {
		t.Fatal("ANSI codes must not count")
	}
	if visibleLen("plain") != 5 {
		t.Fatal("plain text length wrong")
	}
}

func TestFormatters(t *testing.T) {
	cases := map[string]string{
		cpuStr(250):         "250m",
		cpuStr(2000):        "2",
		mibOrGib(512 << 20): "512Mi",
		mibOrGib(3 << 30):   "3.0Gi",
		resStr(500, 1<<30):  "500m/1.0Gi",
		usd(140.157):        "$140.16",
		pct(0.334):          "33%",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}
