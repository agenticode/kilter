package lambda

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// testNow is fixed: nothing in this package reads a clock, and the tests prove
// it by never supplying a moving one.
var testNow = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

const (
	testSpan   = 48 * time.Hour
	testARN    = "arn:aws:lambda:us-east-1:123456789012:function:checkout"
	testName   = "checkout"
	testScope  = "123456789012/us-east-1"
	testRegion = "us-east-1"
)

func testStart() time.Time { return testNow.Add(-testSpan) }

type fnOpt func(*Function)

func withTag(k, v string) fnOpt {
	return func(f *Function) {
		if f.Tags == nil {
			f.Tags = map[string]string{}
		}
		f.Tags[k] = v
	}
}

func withArch(a string) fnOpt       { return func(f *Function) { f.Architecture = a } }
func withPackage(p string) fnOpt    { return func(f *Function) { f.PackageType = p } }
func withProvisioned(n int64) fnOpt { return func(f *Function) { f.ProvisionedConcurrency = n } }

func fn(memMB int64, opts ...fnOpt) Function {
	f := Function{
		ARN: testARN, Name: testName, MemoryMB: memMB, TimeoutSec: 30,
		Architecture: ArchX86, Runtime: "python3.13", PackageType: PackageZip,
		LastModified: testStart(),
	}
	for _, o := range opts {
		o(&f)
	}
	return f
}

// point describes one measured operating point for a test fixture: n
// invocations at memoryMB whose billed duration is billedMS and whose peak
// memory is maxUsedMB. coldEvery > 0 makes every n-th invocation a cold start.
type point struct {
	memoryMB  int64
	maxUsedMB int64
	billedMS  float64
	n         int
	coldEvery int
	initMS    float64
}

// events renders the points as REPORT log events spread across span.
func events(span time.Duration, pts ...point) []LogEvent {
	var out []LogEvent
	for i, p := range pts {
		init := p.initMS
		if init == 0 {
			init = 400
		}
		out = append(out, SyntheticReports(
			mustID(i), testNow.Add(-span), span, p.n, p.memoryMB, p.maxUsedMB, p.billedMS, init, p.coldEvery)...)
	}
	SortEvents(out)
	return out
}

func mustID(i int) string { return string(rune('a' + i)) }

// target builds a Target from raw log events, exactly as the collector would.
func target(f Function, evs []LogEvent, series ...Series) Target {
	t := Target{
		Ref:      domain.TargetRef{Domain: Kind, Scope: testScope, ID: f.ARN, Name: f.Name},
		Function: f,
		Series:   series,
	}
	t.Reports, t.Drops = ParseEvents(evs)
	SortTargets([]Target{t})
	return t
}

func snapOf(span time.Duration, ts ...Target) *Snapshot {
	s := &Snapshot{
		Domain: Kind, Scope: testScope, Region: testRegion, Timestamp: testNow,
		Window:  Window{Start: testNow.Add(-span), End: testNow},
		Targets: ts,
	}
	SortTargets(s.Targets)
	return s
}

// assessTarget runs the sizer over one target and returns its assessment,
// checking the report's own invariants on the way out. Every test goes through
// here, so no test can accidentally observe a report that Validate would
// reject.
func assessTarget(t *testing.T, cfg Config, ledger domain.Netter, span time.Duration, tgt Target) Assessment {
	t.Helper()
	s, err := NewSizer(cfg)
	if err != nil {
		t.Fatalf("NewSizer: %v", err)
	}
	snap := snapOf(span, tgt)
	rep := s.Assess(testNow, snap, ledger)
	if err := rep.Validate(); err != nil {
		t.Fatalf("report invariants violated: %v", err)
	}
	a, ok := rep.For(tgt.Ref.ID)
	if !ok {
		t.Fatalf("no assessment for %s", tgt.Ref.ID)
	}
	return a
}

// one runs the default configuration over one function and its REPORT lines.
func one(t *testing.T, f Function, span time.Duration, pts []point, series ...Series) Assessment {
	t.Helper()
	return assessTarget(t, DefaultConfig(), nil, span, target(f, events(span, pts...), series...))
}

// onlySuppression asserts that exactly one suppression fired, with the given
// code. "Fires alone" is the point: a reason code that only ever appears in a
// crowd is not a reason, it is a mood.
func onlySuppression(t *testing.T, a Assessment, code string) Suppression {
	t.Helper()
	if a.Proposal != nil {
		t.Fatalf("expected a refusal, got a proposal for %s", fmtMB(a.Proposal.MemoryMB))
	}
	if len(a.Suppressions) != 1 {
		t.Fatalf("expected exactly one suppression (%s), got %d: %v", code, len(a.Suppressions), codes(a))
	}
	if a.Suppressions[0].Code != code {
		t.Fatalf("suppression code = %q, want %q (reason: %s)",
			a.Suppressions[0].Code, code, a.Suppressions[0].Reason)
	}
	if a.Suppressions[0].Reason == "" {
		t.Fatalf("suppression %s has no reason; a code without prose is not an explanation", code)
	}
	return a.Suppressions[0]
}

func codes(a Assessment) []string {
	out := make([]string, 0, len(a.Suppressions))
	for _, s := range a.Suppressions {
		out = append(out, s.Code)
	}
	return out
}

func hasAdvisory(t *testing.T, a Assessment, code string) Advisory {
	t.Helper()
	ad, ok := a.AdvisoryFor(code)
	if !ok {
		t.Fatalf("expected advisory %q, got %v", code, advisoryCodes(a))
	}
	return ad
}

func noAdvisory(t *testing.T, a Assessment, code string) {
	t.Helper()
	if _, ok := a.AdvisoryFor(code); ok {
		t.Fatalf("did not expect advisory %q", code)
	}
}

func advisoryCodes(a Assessment) []string {
	out := make([]string, 0, len(a.Advisories))
	for _, ad := range a.Advisories {
		out = append(out, ad.Code)
	}
	return out
}

// invocationSeries builds a CloudWatch Invocations series summing to total.
func invocationSeries(span time.Duration, total float64) Series {
	const n = 24
	return Series{
		Metric: MetricInvocations, Stat: "Sum", Source: SourceCloudWatch, PeriodSeconds: 300,
		Points: SyntheticMetric(testNow.Add(-span), span/n, n, total/n),
	}
}
