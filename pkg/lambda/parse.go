package lambda

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The REPORT line is a log format, not an API. AWS has added fields to it over
// the years (Init Duration in 2019, Restore Duration for SnapStart, Status and
// Error Type for the timeout/failure records) and separates them with tabs in
// some emitters and plain spaces in others. Everything in this file is written
// on that premise:
//
//   - Unknown fields are ignored, not fatal. A future field must not break
//     evidence intake for the fields we do understand.
//   - Labels are matched longest-first, because "Duration:" is a substring of
//     "Billed Duration:", "Init Duration:" and "Restore Duration:". Matching
//     naively assigns the init time to the warm duration — a silent, plausible,
//     completely wrong number.
//   - Anything that does not parse into a PLAUSIBLE number is dropped with a
//     reason code. The alternative — coercing it — produces a wrong duration or
//     a wrong memory floor, and a wrong floor is an OOM.

// Drop reason codes, one per way a line can fail to be evidence.
const (
	// DropNotReport: the line is not a REPORT record at all (START, END, a
	// customer log line, an empty string).
	DropNotReport = "not-a-report-line"
	// DropMissingField: a REPORT line without one of the four fields this
	// package needs (Duration, Billed Duration, Memory Size, Max Memory Used).
	DropMissingField = "missing-field"
	// DropMalformedNumber: a field whose value is not a finite number, or
	// carries a unit this package will not guess at.
	DropMalformedNumber = "malformed-number"
	// DropImplausibleMemory: a memory value outside Lambda's documented
	// 128–10,240 MB range, or a max-memory-used above the configured size.
	DropImplausibleMemory = "implausible-memory"
	// DropImplausibleDuration: a duration outside [0, 15 min], the platform
	// maximum, generously extended for the init phase.
	DropImplausibleDuration = "implausible-duration"
	// DropInconsistent: internally contradictory — billed duration below the
	// measured duration it is supposed to be the rounding-up of.
	DropInconsistent = "inconsistent-record"
)

// ParseError is what [ParseReport] returns for a line it refuses.
type ParseError struct {
	Code   string
	Detail string
}

func (e *ParseError) Error() string { return e.Code + ": " + e.Detail }

func dropErr(code, format string, args ...any) *ParseError {
	return &ParseError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// LogEvent is one CloudWatch Logs event: the platform's timestamp and the raw
// message. The timestamp is the event's, never parsed out of the message —
// REPORT lines do not carry one.
type LogEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

// ReportRecord is one parsed REPORT line: a single invocation, at a single,
// KNOWN memory setting. That last part is why this record is the load-bearing
// evidence of the package — Memory Size is the configured memory *at the time
// of that invocation*, so a function that was retuned during the window yields
// more than one measured operating point, and only then can a bill be compared.
type ReportRecord struct {
	At        time.Time `json:"at"`
	RequestID string    `json:"requestId,omitempty"`
	// DurationMS is the handler's wall time. It EXCLUDES the init phase, which
	// is reported separately.
	DurationMS float64 `json:"durationMS"`
	// BilledDurationMS is what AWS bills, at 1 ms granularity. It is the
	// authority for cost: never recompute it from DurationMS.
	BilledDurationMS float64 `json:"billedDurationMS"`
	MemorySizeMB     int64   `json:"memorySizeMB"`
	MaxMemoryUsedMB  int64   `json:"maxMemoryUsedMB"`
	// InitDurationMS is the cold-start initialization time, present only on the
	// first invocation of an execution environment. It is billed differently
	// from warm duration and MUST NOT be averaged into it — see [Cold].
	InitDurationMS float64 `json:"initDurationMS,omitempty"`
	// RestoreDurationMS is the SnapStart restore phase, which is a cold start
	// by another name and is treated as one.
	RestoreDurationMS float64 `json:"restoreDurationMS,omitempty"`
	// Status is the platform status on newer REPORT records ("timeout",
	// "error"); empty on a normal record.
	Status string `json:"status,omitempty"`
}

// Cold reports whether this invocation paid an initialization cost. A cold
// record never contributes to warm duration statistics: init time is a
// different phase, billed under different rules, and averaging it into the
// warm mean inflates every cost estimate derived from it — most damagingly the
// comparison BETWEEN two memory settings, where an unequal cold-start mix
// invents a cost difference that memory had nothing to do with.
func (r ReportRecord) Cold() bool { return r.InitDurationMS > 0 || r.RestoreDurationMS > 0 }

// ColdOverheadMS is the initialization time this record paid.
func (r ReportRecord) ColdOverheadMS() float64 { return r.InitDurationMS + r.RestoreDurationMS }

// AtCeiling reports whether max memory used reached the configured memory at
// the given ratio. A measurement at the ceiling may have been TRUNCATED by the
// limit rather than bounded by demand.
func (r ReportRecord) AtCeiling(ratio float64) bool {
	if r.MemorySizeMB <= 0 {
		return false
	}
	if r.MaxMemoryUsedMB >= r.MemorySizeMB {
		return true
	}
	return float64(r.MaxMemoryUsedMB) >= ratio*float64(r.MemorySizeMB)
}

// Drop is an aggregated parse failure: how many lines failed one way, and one
// truncated sample so a human can see what the log actually looked like.
type Drop struct {
	Code   string `json:"code"`
	Count  int    `json:"count"`
	Detail string `json:"detail,omitempty"`
	Sample string `json:"sample,omitempty"`
}

// maxSampleBytes bounds the sample kept from a rejected line, so a pathological
// log line cannot bloat a stored report.
const maxSampleBytes = 200

// SortReports orders records by (timestamp, request ID) so aggregation and
// output cannot depend on delivery order.
func SortReports(rs []ReportRecord) {
	sort.SliceStable(rs, func(i, j int) bool {
		if !rs[i].At.Equal(rs[j].At) {
			return rs[i].At.Before(rs[j].At)
		}
		return rs[i].RequestID < rs[j].RequestID
	})
}

// ParseEvents parses a batch of log events into records plus aggregated drops.
// Both results are deterministic: records sorted by (timestamp, request ID),
// drops sorted by reason code.
//
// Lines that are not REPORT records at all (START, END, application output) are
// counted under [DropNotReport] but are not a problem — a log group is full of
// them. The codes that matter are the ones that mean a REPORT line was
// malformed.
func ParseEvents(events []LogEvent) ([]ReportRecord, []Drop) {
	var recs []ReportRecord
	agg := map[string]*Drop{}
	for _, ev := range events {
		rec, err := ParseReport(ev.Message)
		if err != nil {
			pe, ok := err.(*ParseError)
			code, detail := DropNotReport, err.Error()
			if ok {
				code, detail = pe.Code, pe.Detail
			}
			d := agg[code]
			if d == nil {
				d = &Drop{Code: code, Detail: detail, Sample: truncate(ev.Message)}
				agg[code] = d
			}
			d.Count++
			continue
		}
		rec.At = ev.Timestamp
		recs = append(recs, rec)
	}
	SortReports(recs)

	if len(agg) == 0 {
		return recs, nil
	}
	drops := make([]Drop, 0, len(agg))
	for _, d := range agg {
		drops = append(drops, *d)
	}
	sort.Slice(drops, func(i, j int) bool { return drops[i].Code < drops[j].Code })
	return recs, drops
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxSampleBytes {
		return s
	}
	return s[:maxSampleBytes] + "…"
}

// Field labels, in the order they must be matched: longest first, so that
// "Billed Duration:" claims its text before the bare "Duration:" scanner runs.
var reportLabels = []string{
	"Restore Duration:",
	"Billed Duration:",
	"Max Memory Used:",
	"Init Duration:",
	"XRAY TraceId:",
	"Memory Size:",
	"Error Type:",
	"RequestId:",
	"Duration:",
	"SegmentId:",
	"Sampled:",
	"Status:",
}

// reportPrefix is what every REPORT record starts with.
const reportPrefix = "REPORT"

type fieldMatch struct {
	label string
	start int // index of the label
	end   int // index just past the label's colon
}

// ParseReport parses one REPORT log line.
//
// It never panics and never returns a record containing a negative, NaN or
// infinite number: every numeric field is range-checked against the documented
// platform limits before the record exists. FuzzParseReport pins both.
func ParseReport(msg string) (ReportRecord, error) {
	line := normalizeWhitespace(msg)
	trimmed := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(trimmed, reportPrefix) {
		return ReportRecord{}, dropErr(DropNotReport, "line does not start with %q", reportPrefix)
	}
	// Guard against "REPORTING: ..." — the prefix must be a whole word.
	if rest := trimmed[len(reportPrefix):]; rest != "" && rest[0] != ' ' {
		return ReportRecord{}, dropErr(DropNotReport, "line does not start with %q", reportPrefix)
	}

	fields := scanFields(line)
	var rec ReportRecord
	rec.RequestID = firstToken(fields["RequestId:"])
	rec.Status = strings.TrimSpace(fields["Status:"])

	var err error
	if rec.DurationMS, err = requireMS(fields, "Duration:"); err != nil {
		return ReportRecord{}, err
	}
	if rec.BilledDurationMS, err = requireMS(fields, "Billed Duration:"); err != nil {
		return ReportRecord{}, err
	}
	if rec.MemorySizeMB, err = requireMB(fields, "Memory Size:"); err != nil {
		return ReportRecord{}, err
	}
	if rec.MaxMemoryUsedMB, err = requireMB(fields, "Max Memory Used:"); err != nil {
		return ReportRecord{}, err
	}
	if rec.InitDurationMS, err = optionalMS(fields, "Init Duration:"); err != nil {
		return ReportRecord{}, err
	}
	if rec.RestoreDurationMS, err = optionalMS(fields, "Restore Duration:"); err != nil {
		return ReportRecord{}, err
	}

	// Plausibility. Every check below rejects a line rather than clamping it:
	// a clamped value is indistinguishable from a measured one downstream.
	if rec.MemorySizeMB < MinMemoryMB || rec.MemorySizeMB > MaxMemoryMB {
		return ReportRecord{}, dropErr(DropImplausibleMemory,
			"Memory Size %d MB is outside Lambda's %d–%d MB range", rec.MemorySizeMB, MinMemoryMB, MaxMemoryMB)
	}
	if rec.MaxMemoryUsedMB > rec.MemorySizeMB {
		return ReportRecord{}, dropErr(DropImplausibleMemory,
			"Max Memory Used %d MB exceeds the configured %d MB, which the platform cannot report",
			rec.MaxMemoryUsedMB, rec.MemorySizeMB)
	}
	maxMS := float64(MaxTimeoutSeconds) * 1000
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"Duration", rec.DurationMS},
		{"Billed Duration", rec.BilledDurationMS},
		{"Init Duration", rec.InitDurationMS},
		{"Restore Duration", rec.RestoreDurationMS},
	} {
		if f.v > maxMS {
			return ReportRecord{}, dropErr(DropImplausibleDuration,
				"%s %.3f ms exceeds the %d s platform maximum", f.name, f.v, MaxTimeoutSeconds)
		}
	}
	// Billed duration is the measured duration rounded UP to the millisecond,
	// so it can never be materially below it. One millisecond of slack absorbs
	// the rounding; anything more is a corrupt line, not a rounding artifact.
	if rec.BilledDurationMS+1 < rec.DurationMS {
		return ReportRecord{}, dropErr(DropInconsistent,
			"Billed Duration %.3f ms is below Duration %.3f ms", rec.BilledDurationMS, rec.DurationMS)
	}
	return rec, nil
}

// normalizeWhitespace turns tabs, newlines and carriage returns into spaces so
// tab-separated and space-separated emitters parse identically.
func normalizeWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' {
			return ' '
		}
		return r
	}, s)
}

// scanFields extracts label → raw-value pairs. Labels are matched longest-first
// over an ASCII-lowered copy (which preserves byte offsets, unlike
// strings.ToLower on non-ASCII input), and each match consumes its own byte
// range so a shorter label cannot re-match inside a longer one. Each value runs
// to the start of the next matched label.
func scanFields(line string) map[string]string {
	lower := asciiLower(line)
	consumed := make([]bool, len(line))
	var matches []fieldMatch

	for _, label := range reportLabels {
		l := asciiLower(label)
		from := 0
		for {
			i := strings.Index(lower[from:], l)
			if i < 0 {
				break
			}
			at := from + i
			from = at + 1
			if overlaps(consumed, at, at+len(l)) {
				continue
			}
			markConsumed(consumed, at, at+len(l))
			matches = append(matches, fieldMatch{label: label, start: at, end: at + len(l)})
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].start < matches[j].start })

	out := make(map[string]string, len(matches))
	for i, m := range matches {
		end := len(line)
		if i+1 < len(matches) {
			end = matches[i+1].start
		}
		v := strings.TrimSpace(line[m.end:end])
		// First occurrence wins: a duplicated label in a corrupt line must not
		// let the later value overwrite the earlier one.
		if _, dup := out[m.label]; !dup {
			out[m.label] = v
		}
	}
	return out
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func overlaps(consumed []bool, lo, hi int) bool {
	for i := lo; i < hi && i < len(consumed); i++ {
		if consumed[i] {
			return true
		}
	}
	return false
}

func markConsumed(consumed []bool, lo, hi int) {
	for i := lo; i < hi && i < len(consumed); i++ {
		consumed[i] = true
	}
}

func firstToken(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// requireMS reads a required millisecond field.
func requireMS(fields map[string]string, label string) (float64, error) {
	raw, ok := fields[label]
	if !ok {
		return 0, dropErr(DropMissingField, "REPORT line has no %q field", strings.TrimSuffix(label, ":"))
	}
	return parseMS(label, raw)
}

// optionalMS reads an optional millisecond field; absent reads as 0.
func optionalMS(fields map[string]string, label string) (float64, error) {
	raw, ok := fields[label]
	if !ok {
		return 0, nil
	}
	return parseMS(label, raw)
}

// parseMS parses "12.34 ms". The unit is required to be milliseconds: Lambda
// emits nothing else, and guessing at a unit we have not seen would scale a
// duration by a thousand.
func parseMS(label, raw string) (float64, error) {
	num, unit, err := splitValue(label, raw)
	if err != nil {
		return 0, err
	}
	if unit != "ms" {
		return 0, dropErr(DropMalformedNumber, "%q has unit %q; only milliseconds are understood",
			strings.TrimSuffix(label, ":"), unit)
	}
	v, err := parseFloat(label, num)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, dropErr(DropMalformedNumber, "%q is negative (%v)", strings.TrimSuffix(label, ":"), v)
	}
	return v, nil
}

// requireMB parses "512 MB" into whole megabytes.
func requireMB(fields map[string]string, label string) (int64, error) {
	raw, ok := fields[label]
	if !ok {
		return 0, dropErr(DropMissingField, "REPORT line has no %q field", strings.TrimSuffix(label, ":"))
	}
	num, unit, err := splitValue(label, raw)
	if err != nil {
		return 0, err
	}
	if unit != "mb" {
		return 0, dropErr(DropMalformedNumber, "%q has unit %q; only MB is understood",
			strings.TrimSuffix(label, ":"), unit)
	}
	v, err := parseFloat(label, num)
	if err != nil {
		return 0, err
	}
	if v < 0 || v > float64(MaxMemoryMB) {
		return 0, dropErr(DropImplausibleMemory, "%q is %v MB, outside 0–%d MB",
			strings.TrimSuffix(label, ":"), v, MaxMemoryMB)
	}
	return int64(v), nil
}

// splitValue splits "12.34 ms" into ("12.34", "ms"), lowercasing the unit. The
// unit may be attached ("512MB") or separated ("512 MB"); anything after it is
// ignored, because a trailing XRAY field is not our business.
func splitValue(label, raw string) (num, unit string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", dropErr(DropMissingField, "%q has no value", strings.TrimSuffix(label, ":"))
	}
	i := 0
	for i < len(raw) {
		c := raw[i]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			i++
			continue
		}
		break
	}
	num = raw[:i]
	unit = strings.ToLower(firstToken(raw[i:]))
	if num == "" {
		return "", "", dropErr(DropMalformedNumber, "%q has no numeric part (%q)",
			strings.TrimSuffix(label, ":"), truncate(raw))
	}
	return num, unit, nil
}

func parseFloat(label, num string) (float64, error) {
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, dropErr(DropMalformedNumber, "%q is not a number (%q)",
			strings.TrimSuffix(label, ":"), truncate(num))
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, dropErr(DropMalformedNumber, "%q is not finite (%q)",
			strings.TrimSuffix(label, ":"), truncate(num))
	}
	return v, nil
}
