package lambda

import (
	"strings"
	"testing"
	"time"
)

// A real tab-separated REPORT line, as the platform emits it.
const warmLine = "REPORT RequestId: 8f507cbc-5f2a-4d3e-9b1c-2a1f0e6d7c88\tDuration: 12.34 ms\t" +
	"Billed Duration: 13 ms\tMemory Size: 512 MB\tMax Memory Used: 74 MB\t"

// The same invocation, cold: Init Duration appears, and nothing else changes.
const coldLine = "REPORT RequestId: 4d1b0f39-77aa-4f5c-8f10-6c0a2b3d4e5f\tDuration: 12.34 ms\t" +
	"Billed Duration: 13 ms\tMemory Size: 512 MB\tMax Memory Used: 74 MB\tInit Duration: 143.21 ms\t"

func TestParseReportReadsEveryField(t *testing.T) {
	r, err := ParseReport(warmLine)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if r.RequestID != "8f507cbc-5f2a-4d3e-9b1c-2a1f0e6d7c88" {
		t.Errorf("RequestID = %q", r.RequestID)
	}
	if r.DurationMS != 12.34 || r.BilledDurationMS != 13 {
		t.Errorf("durations = %v / %v, want 12.34 / 13", r.DurationMS, r.BilledDurationMS)
	}
	if r.MemorySizeMB != 512 || r.MaxMemoryUsedMB != 74 {
		t.Errorf("memory = %d / %d, want 512 / 74", r.MemorySizeMB, r.MaxMemoryUsedMB)
	}
	if r.Cold() {
		t.Errorf("a line with no Init Duration is a warm invocation")
	}
}

// The trap the longest-first label scan exists to avoid: "Duration:" is a
// substring of "Billed Duration:", "Init Duration:" and "Restore Duration:".
// A naive scan assigns the init time — or the billed time — to the warm
// duration, which is a plausible, silent, completely wrong number.
func TestParseReportDoesNotConfuseDurationLabels(t *testing.T) {
	r, err := ParseReport(coldLine)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if r.DurationMS != 12.34 {
		t.Errorf("Duration = %v, want 12.34 (not the billed or init value)", r.DurationMS)
	}
	if r.BilledDurationMS != 13 {
		t.Errorf("Billed Duration = %v, want 13", r.BilledDurationMS)
	}
	if r.InitDurationMS != 143.21 {
		t.Errorf("Init Duration = %v, want 143.21", r.InitDurationMS)
	}
	if !r.Cold() {
		t.Errorf("a line with an Init Duration is a cold start")
	}
	if r.ColdOverheadMS() != 143.21 {
		t.Errorf("ColdOverheadMS = %v", r.ColdOverheadMS())
	}
}

func TestParseReportAcceptsSpaceSeparatedAndSnapStartForms(t *testing.T) {
	cases := []struct {
		name string
		line string
		want ReportRecord
	}{{
		name: "space separated",
		line: "REPORT RequestId: abc Duration: 1.20 ms Billed Duration: 2 ms Memory Size: 128 MB " +
			"Max Memory Used: 74 MB",
		want: ReportRecord{RequestID: "abc", DurationMS: 1.2, BilledDurationMS: 2,
			MemorySizeMB: 128, MaxMemoryUsedMB: 74},
	}, {
		name: "snapstart restore",
		line: "REPORT RequestId: xyz\tDuration: 5.00 ms\tBilled Duration: 6 ms\tMemory Size: 1024 MB\t" +
			"Max Memory Used: 200 MB\tRestore Duration: 312.00 ms\t",
		want: ReportRecord{RequestID: "xyz", DurationMS: 5, BilledDurationMS: 6,
			MemorySizeMB: 1024, MaxMemoryUsedMB: 200, RestoreDurationMS: 312},
	}, {
		name: "unknown trailing fields are ignored, not fatal",
		line: "REPORT RequestId: t1\tDuration: 3.00 ms\tBilled Duration: 3 ms\tMemory Size: 256 MB\t" +
			"Max Memory Used: 90 MB\tXRAY TraceId: 1-5f0e\tSegmentId: abcd\tSampled: true\t" +
			"SomeFutureField: 42 units\t",
		want: ReportRecord{RequestID: "t1", DurationMS: 3, BilledDurationMS: 3,
			MemorySizeMB: 256, MaxMemoryUsedMB: 90},
	}, {
		name: "unit attached to the number",
		line: "REPORT RequestId: t2 Duration: 3.00ms Billed Duration: 3ms Memory Size: 256MB " +
			"Max Memory Used: 90MB",
		want: ReportRecord{RequestID: "t2", DurationMS: 3, BilledDurationMS: 3,
			MemorySizeMB: 256, MaxMemoryUsedMB: 90},
	}, {
		name: "timeout status record",
		line: "REPORT RequestId: t3\tDuration: 3000.00 ms\tBilled Duration: 3000 ms\tMemory Size: 512 MB\t" +
			"Max Memory Used: 120 MB\tStatus: timeout\t",
		want: ReportRecord{RequestID: "t3", DurationMS: 3000, BilledDurationMS: 3000,
			MemorySizeMB: 512, MaxMemoryUsedMB: 120, Status: "timeout"},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseReport(tc.line)
			if err != nil {
				t.Fatalf("ParseReport: %v", err)
			}
			if got != tc.want {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// Malformed input must be DROPPED with a reason, never coerced into a number.
// Each case names the reason code it must produce.
func TestParseReportDropsMalformedLinesWithAReason(t *testing.T) {
	cases := []struct {
		name string
		line string
		code string
	}{
		{"not a report line", "START RequestId: abc Version: $LATEST", DropNotReport},
		{"empty", "", DropNotReport},
		{"prefix is not a whole word", "REPORTING: something happened", DropNotReport},
		{"customer log line mentioning REPORT", "  my handler wrote REPORT to stdout", DropNotReport},
		{"missing max memory used",
			"REPORT RequestId: a\tDuration: 1.00 ms\tBilled Duration: 1 ms\tMemory Size: 128 MB\t",
			DropMissingField},
		{"missing billed duration",
			"REPORT RequestId: a\tDuration: 1.00 ms\tMemory Size: 128 MB\tMax Memory Used: 10 MB\t",
			DropMissingField},
		{"duration is not a number",
			"REPORT RequestId: a\tDuration: NaN ms\tBilled Duration: 1 ms\tMemory Size: 128 MB\t" +
				"Max Memory Used: 10 MB\t", DropMalformedNumber},
		{"duration in seconds is not guessed at",
			"REPORT RequestId: a\tDuration: 1.5 s\tBilled Duration: 2 ms\tMemory Size: 128 MB\t" +
				"Max Memory Used: 10 MB\t", DropMalformedNumber},
		{"negative duration",
			"REPORT RequestId: a\tDuration: -5.00 ms\tBilled Duration: 1 ms\tMemory Size: 128 MB\t" +
				"Max Memory Used: 10 MB\t", DropMalformedNumber},
		{"memory below the platform minimum",
			"REPORT RequestId: a\tDuration: 1.00 ms\tBilled Duration: 1 ms\tMemory Size: 64 MB\t" +
				"Max Memory Used: 10 MB\t", DropImplausibleMemory},
		{"memory above the platform maximum",
			"REPORT RequestId: a\tDuration: 1.00 ms\tBilled Duration: 1 ms\tMemory Size: 99999 MB\t" +
				"Max Memory Used: 10 MB\t", DropImplausibleMemory},
		{"max used above configured is impossible",
			"REPORT RequestId: a\tDuration: 1.00 ms\tBilled Duration: 1 ms\tMemory Size: 128 MB\t" +
				"Max Memory Used: 900 MB\t", DropImplausibleMemory},
		{"duration beyond the 15-minute platform ceiling",
			"REPORT RequestId: a\tDuration: 900001.00 ms\tBilled Duration: 900001 ms\tMemory Size: 128 MB\t" +
				"Max Memory Used: 10 MB\t", DropImplausibleDuration},
		{"billed below measured is self-contradictory",
			"REPORT RequestId: a\tDuration: 500.00 ms\tBilled Duration: 3 ms\tMemory Size: 128 MB\t" +
				"Max Memory Used: 10 MB\t", DropInconsistent},
		{"truncated mid-number",
			"REPORT RequestId: a\tDuration: 12.3", DropMalformedNumber},
		{"truncated after a complete field",
			"REPORT RequestId: a\tDuration: 12.30 ms\t", DropMissingField},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := ParseReport(tc.line)
			if err == nil {
				t.Fatalf("expected a drop, got %+v", rec)
			}
			pe, ok := err.(*ParseError)
			if !ok {
				t.Fatalf("error is not a *ParseError: %T", err)
			}
			if pe.Code != tc.code {
				t.Fatalf("drop code = %q, want %q (%s)", pe.Code, tc.code, pe.Detail)
			}
			if rec != (ReportRecord{}) {
				t.Fatalf("a dropped line must yield the zero record, got %+v", rec)
			}
		})
	}
}

// A truncated measurement — max memory used at the configured ceiling — parses
// fine. It is evidence of RISK, and the risk is recognized at the record level.
func TestParseReportRecognizesTheCeiling(t *testing.T) {
	line := "REPORT RequestId: a\tDuration: 100.00 ms\tBilled Duration: 100 ms\tMemory Size: 512 MB\t" +
		"Max Memory Used: 512 MB\t"
	r, err := ParseReport(line)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if !r.AtCeiling(0.98) {
		t.Errorf("max used == configured must count as at-the-ceiling")
	}
	// Just under the ratio is not at the ceiling.
	loose := strings.Replace(line, "Max Memory Used: 512 MB", "Max Memory Used: 400 MB", 1)
	r2, err := ParseReport(loose)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if r2.AtCeiling(0.98) {
		t.Errorf("400/512 is not at the ceiling")
	}
}

func TestParseEventsAggregatesDropsAndSortsRecords(t *testing.T) {
	base := testNow
	evs := []LogEvent{
		{Timestamp: base.Add(2 * time.Second), Message: warmLine},
		{Timestamp: base.Add(1 * time.Second), Message: coldLine},
		{Timestamp: base, Message: "START RequestId: a Version: $LATEST"},
		{Timestamp: base, Message: "END RequestId: a"},
		{Timestamp: base, Message: "REPORT RequestId: bad\tDuration: oops ms\tBilled Duration: 1 ms\t" +
			"Memory Size: 128 MB\tMax Memory Used: 1 MB\t"},
		{Timestamp: base, Message: "REPORT RequestId: bad2\tDuration: 1.00 ms\tBilled Duration: 1 ms\t" +
			"Memory Size: 128 MB\t"},
	}
	recs, drops := ParseEvents(evs)
	if len(recs) != 2 {
		t.Fatalf("parsed %d records, want 2", len(recs))
	}
	if !recs[0].At.Before(recs[1].At) {
		t.Errorf("records must come back sorted by timestamp")
	}
	if !recs[0].Cold() || recs[1].Cold() {
		t.Errorf("cold/warm classification did not survive ParseEvents")
	}
	want := map[string]int{DropNotReport: 2, DropMalformedNumber: 1, DropMissingField: 1}
	got := map[string]int{}
	var lastCode string
	for _, d := range drops {
		got[d.Code] = d.Count
		if d.Code < lastCode {
			t.Errorf("drops are not sorted by code: %q after %q", d.Code, lastCode)
		}
		lastCode = d.Code
		if d.Sample == "" && d.Code != DropNotReport {
			t.Errorf("drop %q kept no sample line", d.Code)
		}
	}
	for code, n := range want {
		if got[code] != n {
			t.Errorf("drop %s = %d, want %d (all: %v)", code, got[code], n, got)
		}
	}
}

// The parser must never let a duplicated label overwrite the first value: a
// corrupt line is a drop, not a merge of two records.
func TestParseReportFirstOccurrenceWins(t *testing.T) {
	line := "REPORT RequestId: a\tDuration: 10.00 ms\tBilled Duration: 10 ms\tMemory Size: 512 MB\t" +
		"Max Memory Used: 100 MB\tDuration: 9999.00 ms\t"
	r, err := ParseReport(line)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if r.DurationMS != 10 {
		t.Errorf("Duration = %v, want the first occurrence (10)", r.DurationMS)
	}
}
