package rds

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// The growth finding's tests are mostly about the REFUSALS, because the
// refusals are the product. Every gate in growth.go has a test that pins its
// code, and the two collapses the design exists to prevent — "not enough
// history" read as "flat", and a growth read as a shrink opportunity — each
// have a test of their own.

// obs builds one observation d before testEnd.
func obs(before time.Duration, gib int64) StorageObservation {
	return StorageObservation{At: testEnd.Add(-before), AllocatedGiB: gib}
}

// evenHistory lays n observations evenly across span, ending at testEnd, with
// allocation taken from alloc(i). Evenly spaced so the irregularity gate is
// not the thing under test unless a test makes it so.
func evenHistory(n int, span time.Duration, alloc func(i int) int64) StorageHistory {
	h := make(StorageHistory, 0, n)
	if n < 2 {
		for i := 0; i < n; i++ {
			h = append(h, obs(span, alloc(i)))
		}
		return h
	}
	// Instants are computed as span·i/(n−1) rather than i·(span/(n−1)) so the
	// endpoints are EXACT: a test of the minimum-span boundary must not be
	// decided by a truncated division.
	for i := 0; i < n; i++ {
		off := time.Duration(int64(span) * int64(i) / int64(n-1))
		h = append(h, obs(span-off, alloc(i)))
	}
	return h
}

func flatHistory(n int, span time.Duration, gib int64) StorageHistory {
	return evenHistory(n, span, func(int) int64 { return gib })
}

// growInstance is the instance every growth test measures: gp2, so the shipped
// storage rate can put a dollar on the finding.
func growInstance(gib int64) DBInstance {
	return DBInstance{
		ARN: "arn:aws:rds:us-east-1:1234:db:grow", Identifier: "grow",
		Class: "db.m6i.large", Engine: "postgres", LicenseModel: LicenseGPL,
		StorageType: StorageGP2, AllocatedStorageGiB: gib,
	}
}

func growAssess(h StorageHistory, alloc int64) GrowthVerdict {
	return AssessStorageGrowth(growInstance(alloc), h, DefaultGrowthPolicy(), DefaultRates())
}

// --- The two gates, and the arithmetic behind them --------------------------

// Fewer than MinObservations instants ⇒ nothing is measured, whatever the
// span. Three snapshots a month apart are still three snapshots.
func TestGrowthRefusesTooFewObservations(t *testing.T) {
	for n := 0; n < DefaultGrowthMinObservations; n++ {
		h := evenHistory(n, 60*24*time.Hour, func(i int) int64 { return 100 + int64(i)*50 })
		v := growAssess(h, 250)
		if v.Measured() {
			t.Fatalf("%d observations produced a measurement; two samples are not a trend", n)
		}
		if v.Measurement != nil {
			t.Fatalf("%d observations produced a non-nil Measurement", n)
		}
		wantState := GrowthTooFewObservations
		if n == 0 {
			wantState = GrowthNoHistory
		}
		if v.State != wantState {
			t.Fatalf("%d observations gave state %q, want %q", n, v.State, wantState)
		}
		if v.Code != ReasonGrowthInsufficientObservations {
			t.Fatalf("%d observations gave code %q, want %q", n, v.Code, ReasonGrowthInsufficientObservations)
		}
		if _, ok := v.GrewGiB(); ok {
			t.Fatalf("%d observations reported a growth", n)
		}
	}
	// One more clears it, over the same span.
	h := evenHistory(DefaultGrowthMinObservations, 60*24*time.Hour, func(i int) int64 { return 100 + int64(i)*50 })
	if v := growAssess(h, 250); !v.Measured() {
		t.Fatalf("%d observations still refused: %s", DefaultGrowthMinObservations, v.Code)
	}
}

// Enough observations, too little elapsed time. A dense hour of snapshots
// describes an hour; the remedy is time, not sampling, and the code says so
// separately from the count gate.
func TestGrowthRefusesTooShortASpan(t *testing.T) {
	h := evenHistory(48, DefaultGrowthMinSpan-time.Hour, func(i int) int64 { return 100 + int64(i) })
	v := growAssess(h, 147)
	if v.Measured() || v.Measurement != nil {
		t.Fatal("a span below the minimum produced a measurement")
	}
	if v.State != GrowthSpanTooShort || v.Code != ReasonGrowthSpanTooShort {
		t.Fatalf("state %q code %q, want %q / %q", v.State, v.Code, GrowthSpanTooShort, ReasonGrowthSpanTooShort)
	}
	if v.Code == ReasonGrowthInsufficientObservations {
		t.Fatal("a short span was reported as too few observations; the two have different remedies")
	}
	// Exactly the minimum span clears it.
	h = evenHistory(48, DefaultGrowthMinSpan, func(i int) int64 { return 100 + int64(i) })
	if v := growAssess(h, 147); !v.Measured() {
		t.Fatalf("a span of exactly %s was refused: %s", DefaultGrowthMinSpan, v.Code)
	}
}

// THE collapse this design exists to prevent. "We have not looked long enough"
// and "we looked and it never moved" must not be reachable from one another by
// reading a zero.
func TestNotEnoughHistoryIsNotFlat(t *testing.T) {
	short := growAssess(flatHistory(2, 60*24*time.Hour, 200), 200)
	brief := growAssess(flatHistory(48, 24*time.Hour, 200), 200)
	measured := growAssess(flatHistory(48, 30*24*time.Hour, 200), 200)

	if measured.State != GrowthFlat || !measured.Measured() {
		t.Fatalf("a well-observed unchanging allocation gave state %q, want %q", measured.State, GrowthFlat)
	}
	for _, v := range []GrowthVerdict{short, brief} {
		if v.State == GrowthFlat {
			t.Fatalf("state %q reported an unmeasured history as flat", v.State)
		}
		if v.Measured() {
			t.Fatalf("state %q claims to be measured", v.State)
		}
		if v.Measurement != nil {
			t.Fatalf("state %q carries a Measurement a caller could read a zero out of", v.State)
		}
		// The two-return accessor is the structural half: a caller that
		// ignores ok gets 0, and has to have ignored ok on purpose.
		if _, ok := v.GrewGiB(); ok {
			t.Fatalf("state %q answered GrewGiB", v.State)
		}
	}
	// And flat DOES answer, with a real zero that means something.
	got, ok := measured.GrewGiB()
	if !ok || got != 0 {
		t.Fatalf("a measured flat history gave (%d, %v), want (0, true)", got, ok)
	}
	// The three states are pairwise distinct, so no caller can merge them.
	if short.State == brief.State || short.State == measured.State || brief.State == measured.State {
		t.Fatalf("states collide: %q / %q / %q", short.State, brief.State, measured.State)
	}
}

// Measurement is non-nil if and only if the state is a measured one. This
// biconditional is what the rest of the package is allowed to rely on.
func TestGrowthMeasurementExistsExactlyWhenMeasured(t *testing.T) {
	cases := []StorageHistory{
		nil,
		{},
		flatHistory(1, 0, 200),
		flatHistory(3, 60*24*time.Hour, 200),
		flatHistory(48, time.Hour, 200),
		flatHistory(48, 30*24*time.Hour, 200),
		evenHistory(48, 30*24*time.Hour, func(i int) int64 { return 200 + int64(i)*10 }),
		// Non-monotone: refused, so no measurement.
		{obs(40*24*time.Hour, 400), obs(30*24*time.Hour, 300), obs(20*24*time.Hour, 300), obs(0, 300)},
		// Same instant, two allocations.
		{obs(40*24*time.Hour, 100), obs(40*24*time.Hour, 200), obs(20*24*time.Hour, 200), obs(0, 200)},
	}
	for i, h := range cases {
		v := growAssess(h, 200)
		if v.State.Measured() != (v.Measurement != nil) {
			t.Fatalf("case %d: state %q Measured()=%v but Measurement!=nil is %v",
				i, v.State, v.State.Measured(), v.Measurement != nil)
		}
		if v.State == GrowthUnevaluated {
			t.Fatalf("case %d: AssessStorageGrowth returned the zero state; it always answers", i)
		}
		if !v.State.Measured() && v.Code == "" {
			t.Fatalf("case %d: state %q carries no refusal code", i, v.State)
		}
		if !v.State.Measured() && v.Reason == "" {
			t.Fatalf("case %d: state %q carries no reason", i, v.State)
		}
	}
}

// A decreasing allocation, or one instant carrying two allocations, is not one
// instance's history. It is refused whole rather than repaired, and no
// reduction is inferred from the decrease.
func TestGrowthRefusesAnInconsistentHistory(t *testing.T) {
	shrank := evenHistory(10, 30*24*time.Hour, func(i int) int64 {
		if i == 7 {
			return 100 // an allocation no RDS API can produce
		}
		return 200 + int64(i)*10
	})
	conflict := flatHistory(10, 30*24*time.Hour, 200)
	conflict = append(conflict, StorageObservation{At: conflict[4].At, AllocatedGiB: 999})

	for name, h := range map[string]StorageHistory{"shrank": shrank, "same-instant": conflict} {
		v := growAssess(h, 300)
		if v.State != GrowthInconsistent {
			t.Fatalf("%s: state %q, want %q", name, v.State, GrowthInconsistent)
		}
		if v.Code != ReasonGrowthHistoryInconsistent {
			t.Fatalf("%s: code %q, want %q", name, v.Code, ReasonGrowthHistoryInconsistent)
		}
		if v.Measurement != nil {
			t.Fatalf("%s: a contradictory history was averaged into a measurement", name)
		}
		if !strings.Contains(v.Reason, "You can't reduce the amount of storage") {
			t.Fatalf("%s: the refusal does not quote the ratchet: %s", name, v.Reason)
		}
	}
}

// --- The finding itself: a refusal with a size ------------------------------

// A measured growth produces a REFUSAL carrying the size, an advisory carrying
// the permanent cost, and nothing that could be read as an available
// reduction.
func TestGrowthIsARefusalWithASize(t *testing.T) {
	h := evenHistory(30, 30*24*time.Hour, func(i int) int64 { return 200 + int64(i/6)*40 })
	v := growAssess(h, 360)
	if v.State != GrowthGrew {
		t.Fatalf("state %q, want %q", v.State, GrowthGrew)
	}
	if v.Code != ReasonGrowthIsNotReclaimable {
		t.Fatalf("code %q, want %q", v.Code, ReasonGrowthIsNotReclaimable)
	}
	grew, ok := v.GrewGiB()
	if !ok || grew != 160 {
		t.Fatalf("grew %d (ok=%v), want 160", grew, ok)
	}
	if v.Measurement.Steps != 4 {
		t.Fatalf("steps = %d, want 4", v.Measurement.Steps)
	}
	if v.Measurement.LargestStepGiB != 40 {
		t.Fatalf("largest step = %d, want 40", v.Measurement.LargestStepGiB)
	}
	if v.Measurement.MonthlyUSD <= 0 {
		t.Fatal("the refusal carries no size in dollars; the magnitude IS the finding")
	}
	// The prose must read as a permanent cost, never as an opportunity.
	if !strings.Contains(v.Reason, "not a reduction that is available") {
		t.Fatalf("the refusal does not say the reduction is unavailable: %s", v.Reason)
	}
	for _, banned := range []string{"could be reduced", "recommend shrink", "reclaim", "downsize the storage"} {
		if strings.Contains(strings.ToLower(v.Reason), banned) {
			t.Fatalf("the growth refusal contains %q; it must not read as a shrink proposal: %s",
				banned, v.Reason)
		}
	}
}

// --- The rate, and the two ways it is withheld ------------------------------

// One autoscaling step inside a long, dense window is one event. The size is
// still stated; the rate is not.
func TestSingleStepGrowthStatesTheSizeAndWithholdsTheRate(t *testing.T) {
	h := evenHistory(60, 30*24*time.Hour, func(i int) int64 {
		if i < 30 {
			return 200
		}
		return 400
	})
	v := growAssess(h, 400)
	if v.State != GrowthGrew {
		t.Fatalf("state %q, want %q", v.State, GrowthGrew)
	}
	if v.Measurement.Steps != 1 {
		t.Fatalf("steps = %d, want 1", v.Measurement.Steps)
	}
	if grew, _ := v.GrewGiB(); grew != 200 {
		t.Fatalf("grew %d, want 200 — the SIZE does not depend on the rate gate", grew)
	}
	if v.RateCode != ReasonGrowthRateNotProjectable {
		t.Fatalf("rate code %q, want %q", v.RateCode, ReasonGrowthRateNotProjectable)
	}
	if _, ok := v.Rate(); ok {
		t.Fatal("a rate was projected from a single step; one event is not a recurrence")
	}
	if v.Measurement.Projection != nil {
		t.Fatal("Projection is non-nil with the rate refused")
	}
	// Two steps over the same span clears the gate.
	h2 := evenHistory(60, 30*24*time.Hour, func(i int) int64 { return 200 + int64(i/20)*100 })
	v2 := growAssess(h2, 400)
	if v2.RateCode != "" {
		t.Fatalf("two steps were still refused a rate: %s", v2.RateReason)
	}
	if _, ok := v2.Rate(); !ok {
		t.Fatal("two steps over a full span produced no rate")
	}
}

// The retained history is THINNED, so the instants cluster. When most of the
// span sits inside one gap nobody observed, the growth could have arrived
// entirely inside it and no rate is drawn through it — but the irregularity is
// reported rather than smoothed.
func TestIrregularSamplingWithholdsTheRateAndIsReported(t *testing.T) {
	// Two dense clusters 24 days apart: plenty of observations, plenty of
	// span, three increases, and 80 % of the span unobserved.
	var h StorageHistory
	for i := 0; i < 6; i++ {
		h = append(h, obs(30*24*time.Hour-time.Duration(i)*time.Hour, 200+int64(i)*10))
	}
	for i := 0; i < 6; i++ {
		h = append(h, obs(5*24*time.Hour-time.Duration(i)*time.Hour, 400+int64(i)*10))
	}
	v := growAssess(h, 450)
	if v.State != GrowthGrew {
		t.Fatalf("state %q, want %q", v.State, GrowthGrew)
	}
	if v.Sampling.Regular {
		t.Fatalf("a history with an %s gap across a %s span was called regular",
			v.Sampling.MaxGap, v.Sampling.Span)
	}
	if v.Sampling.LargestGapFraction <= DefaultGrowthMaxGapFraction {
		t.Fatalf("largest gap fraction = %v, want > %v", v.Sampling.LargestGapFraction,
			DefaultGrowthMaxGapFraction)
	}
	if v.RateCode != ReasonGrowthRateNotProjectable {
		t.Fatalf("rate code %q, want %q", v.RateCode, ReasonGrowthRateNotProjectable)
	}
	if !strings.Contains(v.RateReason, "thinned") {
		t.Fatalf("the rate refusal does not name the thinning: %s", v.RateReason)
	}
	// The size survives the rate refusal, and the irregularity is stated.
	if grew, _ := v.GrewGiB(); grew != 250 {
		t.Fatalf("grew %d, want 250", grew)
	}
	if !strings.Contains(describeSampling(v.Sampling), "not evenly spaced") {
		t.Fatal("the sampling description smooths the irregularity away")
	}
}

// The rate divides by the ACTUAL span between the first and last instants, not
// by an assumed cadence times the observation count. The history below is
// deliberately front-loaded so the two answers differ.
func TestGrowthRateUsesTheActualInstants(t *testing.T) {
	// 20 observations across 30 days: 14 crammed into the first 4½ days at 8 h
	// apart, then 6 stragglers. The count and the span-in-days differ, so
	// growth÷count and growth÷span are distinguishable answers.
	day := 24 * time.Hour
	var h StorageHistory
	for i := 0; i < 14; i++ {
		gib := int64(200)
		if i >= 7 {
			gib = 230
		}
		h = append(h, obs(30*day-time.Duration(i)*8*time.Hour, gib))
	}
	for i, before := range []time.Duration{24 * day, 20 * day, 16 * day, 12 * day, 6 * day, 0} {
		gib := int64(260)
		if i >= 3 {
			gib = 290
		}
		h = append(h, obs(before, gib))
	}
	v := growAssess(h, 290)
	if v.State != GrowthGrew {
		t.Fatalf("state %q (%s), want %q", v.State, v.Code, GrowthGrew)
	}
	p, ok := v.Rate()
	if !ok {
		t.Fatalf("no rate projected: %s", v.RateReason)
	}
	grew, _ := v.GrewGiB()
	wantDays := v.Sampling.Span.Hours() / 24
	if math.Abs(p.SpanDays-wantDays) > 1e-9 {
		t.Fatalf("SpanDays = %v, want the actual span %v", p.SpanDays, wantDays)
	}
	if want := float64(grew) / wantDays; math.Abs(p.GiBPerDay-want) > 1e-9 {
		t.Fatalf("GiBPerDay = %v, want %v", p.GiBPerDay, want)
	}
	// And it is NOT the naive per-sample figure.
	if naive := float64(grew) / float64(v.Sampling.Observations); math.Abs(p.GiBPerDay-naive) < 1e-9 {
		t.Fatal("the rate equals growth ÷ observation count; the instants were not used")
	}
	// MinGap and MaxGap bracket the mean rather than equalling it, which is
	// the fact a reader needs in order to weigh the rate at all.
	if !(v.Sampling.MinGap < v.Sampling.MeanGap && v.Sampling.MeanGap < v.Sampling.MaxGap) {
		t.Fatalf("gaps min=%s mean=%s max=%s do not describe an irregular history",
			v.Sampling.MinGap, v.Sampling.MeanGap, v.Sampling.MaxGap)
	}
}

// --- Structural guarantees --------------------------------------------------

// A rate is a claim about the future and must never carry money. Asserted by
// reflection so the next person to add a field argues with a test.
func TestGrowthProjectionCarriesNoMoney(t *testing.T) {
	for _, name := range structFieldNames(t, GrowthProjection{}) {
		lower := strings.ToLower(name)
		for _, banned := range []string{"usd", "cost", "price", "saving", "dollar", "monthly"} {
			if strings.Contains(lower, banned) {
				t.Errorf("rds.GrowthProjection has a field %q. A growth rate is a forecast over a step "+
					"function whose steps this package does not observe the cause of; attaching money to "+
					"it would let an unverified projection become a claim", name)
			}
		}
	}
	if (GrowthProjection{GiBPerDay: 1000}).Claimable() {
		t.Error("a growth projection claims to be claimable")
	}
	if !strings.Contains((GrowthProjection{GiBPerDay: 1.5, Steps: 3, SpanDays: 30}).String(), "[unverified]") {
		t.Error("a rendered projection does not carry its [unverified] marker")
	}
}

// The measurement names a COST and never a reduction — the same discipline
// TestStorageVerdictNamesNoSaving applies to the floor, applied to the ratchet
// over time.
func TestGrowthNamesNoSaving(t *testing.T) {
	for _, v := range []any{GrowthMeasurement{}, GrowthVerdict{}, GrowthTotals{}, GrowthSampling{}} {
		for _, name := range structFieldNames(t, v) {
			lower := strings.ToLower(name)
			for _, banned := range []string{"saving", "reclaim", "shrink", "reduc", "recover"} {
				if strings.Contains(lower, banned) && !strings.Contains(lower, "unrecoverable") {
					t.Errorf("%T has a field %q; allocated storage is a one-way ratchet and a field named "+
						"after a reduction would be a promise no RDS API can keep", v, name)
				}
			}
		}
	}
}

// A caller may demand MORE evidence than this package requires and may never
// demand less: a two-sample policy is exactly the claim §7.4 deferred.
func TestGrowthPolicyCannotBeLoosened(t *testing.T) {
	loose := GrowthPolicy{MinObservations: 2, MinSpan: time.Minute, MinSteps: 1,
		MaxGapFraction: 0.99, MaxObservations: 1 << 20}
	got := loose.normalized()
	if got != DefaultGrowthPolicy() {
		t.Fatalf("a loosened policy survived normalization: %+v", got)
	}
	if (GrowthPolicy{}).normalized() != DefaultGrowthPolicy() {
		t.Fatal("the zero policy does not normalize to the shipped bar")
	}
	// Stricter survives.
	strict := GrowthPolicy{MinObservations: 30, MinSpan: 90 * 24 * time.Hour, MinSteps: 5,
		MaxGapFraction: 0.1, MaxObservations: 100}
	if strict.normalized() != strict {
		t.Fatalf("a stricter policy was relaxed: %+v", strict.normalized())
	}
	// And a stricter policy actually refuses what the default accepts.
	h := evenHistory(10, 20*24*time.Hour, func(i int) int64 { return 200 + int64(i)*10 })
	if v := growAssess(h, 290); !v.Measured() {
		t.Fatalf("the default policy refused a clean history: %s", v.Code)
	}
	v := AssessStorageGrowth(growInstance(290), h, strict, DefaultRates())
	if v.Measured() {
		t.Fatal("a stricter policy measured a history it should have refused")
	}
}

// --- The history container --------------------------------------------------

func TestStorageHistoryAppendIsIdempotentBoundedAndOrdered(t *testing.T) {
	var h StorageHistory
	for i := 0; i < 10; i++ {
		h = h.Append(obs(time.Duration(10-i)*24*time.Hour, 100+int64(i)), 0)
	}
	if len(h) != 10 {
		t.Fatalf("len = %d, want 10", len(h))
	}
	// Re-appending the same instants replaces rather than doubles: re-ingesting
	// a history is a no-op, the same rule pkg/store's SaveSnapshotAt applies.
	again := h
	for _, o := range h {
		again = again.Append(o, 0)
	}
	if len(again) != 10 {
		t.Fatalf("re-appending doubled the history to %d", len(again))
	}
	if b1, _ := json.Marshal(h); string(b1) != mustJSON(t, again) {
		t.Fatal("re-appending an identical history changed it")
	}
	// Out-of-order arrival sorts.
	shuf := StorageHistory{obs(0, 300), obs(20*24*time.Hour, 100), obs(10*24*time.Hour, 200)}
	var built StorageHistory
	for _, o := range shuf {
		built = built.Append(o, 0)
	}
	for i := 1; i < len(built); i++ {
		if !built[i-1].At.Before(built[i].At) {
			t.Fatal("Append did not order the history by instant")
		}
	}
	// Zero instants and non-positive allocations are dropped: "we did not
	// look" is not an observation of zero storage.
	junk := built.Append(StorageObservation{AllocatedGiB: 500}, 0).
		Append(StorageObservation{At: testEnd.Add(-time.Hour)}, 0)
	if len(junk) != len(built) {
		t.Fatalf("Append accepted a junk observation: %d → %d", len(built), len(junk))
	}
	// The bound drops the OLDEST.
	bounded := h
	for i := 0; i < 5; i++ {
		bounded = bounded.Append(obs(time.Duration(i)*time.Hour, 500), 4)
	}
	if len(bounded) != 4 {
		t.Fatalf("bounded len = %d, want 4", len(bounded))
	}
	if !bounded[len(bounded)-1].At.Equal(h[len(h)-1].At.UTC()) && bounded[0].At.Before(h[0].At) {
		t.Fatal("the bound dropped the newest rather than the oldest")
	}
	// A truncated history says so.
	long := flatHistory(20, 30*24*time.Hour, 200)
	s := long.Sampling(GrowthPolicy{MaxObservations: 5})
	if !s.Truncated {
		t.Fatal("a history longer than the read bound did not report itself truncated")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Input ORDER must not reach the verdict: a history delivered backwards, or
// interleaved, measures the same.
func TestGrowthIsShuffleInvariant(t *testing.T) {
	base := evenHistory(24, 30*24*time.Hour, func(i int) int64 { return 200 + int64(i/6)*25 })
	want := mustJSON(t, growAssess(base, 275))
	for pi, order := range permutations(len(base)) {
		got := mustJSON(t, growAssess(permute(base, order), 275))
		if got != want {
			t.Fatalf("permutation %d produced a different verdict; input order reached the output", pi)
		}
	}
}

// --- Report integration -----------------------------------------------------

// withHistory attaches a history to every target in a snapshot.
func withHistory(snap *Snapshot, h func(id string) StorageHistory) *Snapshot {
	for i := range snap.Targets {
		snap.Targets[i].StorageHistory = h(snap.Targets[i].Instance.Identifier)
	}
	return snap
}

// A fleet with no persisted history produces NO growth suppression: an unwired
// caller is one fact about the wiring, not N facts about N instances. The
// verdict is still recorded, typed, on every assessment.
func TestNoHistoryAddsNoSuppression(t *testing.T) {
	rep := assess(t, collect(t, mixedFixture()), nil)
	for _, code := range []string{
		ReasonGrowthInsufficientObservations, ReasonGrowthSpanTooShort,
		ReasonGrowthHistoryInconsistent, ReasonGrowthIsNotReclaimable,
		ReasonGrowthRateNotProjectable,
	} {
		if n := rep.Totals.RefusedByCode[code]; n != 0 {
			t.Fatalf("an unwired fleet emitted %q ×%d; the wiring gap is not a per-instance finding",
				code, n)
		}
	}
	modelled := 0
	for _, a := range rep.Assessments {
		if a.Excluded() {
			// Exclusions fire alone and never reach the growth gate.
			if a.Growth.State != GrowthUnevaluated {
				t.Fatalf("%s is excluded but carries a growth verdict %q", a.Target.ID, a.Growth.State)
			}
			continue
		}
		modelled++
		if a.Growth.State != GrowthNoHistory {
			t.Fatalf("%s: growth state %q, want %q", a.Target.ID, a.Growth.State, GrowthNoHistory)
		}
		if a.Growth.Reason == "" {
			t.Fatalf("%s: the no-history verdict states no reason", a.Target.ID)
		}
	}
	if got := rep.StorageGrowth(); got.NoHistory != modelled || got.Measured != 0 || got.Refused != 0 {
		t.Fatalf("totals %+v, want NoHistory=%d and nothing measured or refused", got, modelled)
	}
}

// The end-to-end shape: a growing instance produces a refusal, an advisory
// carrying the permanent cost, a valid report, and NOT ONE PROPOSAL or dollar
// of savings anywhere.
func TestGrowthReachesTheReportAsARefusalAndNeverAsAProposal(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("grew", "db.m6i.large", "postgres", withStorage(600, 2000, StorageGP2)),
			rec("still", "db.m6i.large", "postgres", withStorage(200, 0, StorageGP2)),
			rec("young", "db.m6i.large", "postgres", withStorage(300, 0, StorageGP2)),
		},
		Metrics: mergeMetrics(
			metricsFor("grew", 30, 10, 8<<30, 100*GiB),
			metricsFor("still", 30, 10, 8<<30, 50*GiB),
			metricsFor("young", 30, 10, 8<<30, 50*GiB),
		),
	}
	snap := withHistory(collect(t, f), func(id string) StorageHistory {
		switch id {
		case "grew":
			return evenHistory(30, 30*24*time.Hour, func(i int) int64 { return 200 + int64(i/6)*100 })
		case "still":
			return flatHistory(30, 30*24*time.Hour, 200)
		default:
			return flatHistory(2, 30*24*time.Hour, 300)
		}
	})
	rep := assess(t, snap, nil)

	grew := must(t, rep, "grew")
	wantCode(t, grew, ReasonGrowthIsNotReclaimable)
	ad := wantAdvisory(t, grew, AdvisoryStorageGrew)
	if ad.MonthlyUSD <= 0 {
		t.Fatal("the growth advisory carries no magnitude")
	}
	if ad.Actuatable() || ad.Action() != "advisory" {
		t.Fatal("the growth advisory claims to be actuatable")
	}
	if !strings.Contains(ad.Caveat, "permanent cost") || !strings.Contains(ad.Caveat, "not a saving") {
		t.Fatalf("the growth advisory's caveat does not say it is unrecoverable: %s", ad.Caveat)
	}
	// The ratchet refusal that was already there is still there, unweakened.
	wantCode(t, grew, ReasonStorageCannotShrink)
	wantCode(t, grew, ReasonStorageAutoscalingRatchet)

	still := must(t, rep, "still")
	if still.Growth.State != GrowthFlat {
		t.Fatalf("still: growth state %q, want %q", still.Growth.State, GrowthFlat)
	}
	for _, code := range []string{ReasonGrowthIsNotReclaimable, ReasonGrowthInsufficientObservations,
		ReasonGrowthSpanTooShort, ReasonGrowthRateNotProjectable} {
		wantNoCode(t, still, code)
	}

	young := must(t, rep, "young")
	wantCode(t, young, ReasonGrowthInsufficientObservations)
	if young.Growth.Measured() {
		t.Fatal("young: two observations were measured")
	}

	// Nothing proposed, nothing claimed, anywhere.
	if rep.Totals.Proposals != 0 {
		t.Fatalf("%d proposals; a growth measurement must never become one", rep.Totals.Proposals)
	}
	if rep.Totals.GrossSavingsMonthlyUSD != 0 || rep.Totals.NetSavingsMonthlyUSD != 0 {
		t.Fatalf("the report claims savings: gross %v net %v",
			rep.Totals.GrossSavingsMonthlyUSD, rep.Totals.NetSavingsMonthlyUSD)
	}
	for _, a := range rep.Assessments {
		if a.Proposal != nil {
			t.Fatalf("%s carries a proposal", a.Target.ID)
		}
	}
	// The roll-up counts refusals separately from measurements, so an operator
	// cannot read "1 measured" as "3 instances checked".
	tot := rep.StorageGrowth()
	if tot.Measured != 2 || tot.Grew != 1 || tot.Flat != 1 || tot.Refused != 1 {
		t.Fatalf("totals %+v, want measured 2 (1 grew, 1 flat) and 1 refused", tot)
	}
	if tot.GrewGiB != 400 || tot.UnrecoverableGrowthMonthlyUSD <= 0 {
		t.Fatalf("totals %+v, want 400 GiB with a cost", tot)
	}
	// The text report renders without depending on anything unordered.
	var b strings.Builder
	if err := rep.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ReasonGrowthIsNotReclaimable) {
		t.Fatal("the growth refusal does not reach the text report")
	}
}

// Every growth code is a stable, distinct, storable string, and none collides
// with a code this package already ships.
func TestGrowthCodesAreDistinctFromEveryExistingCode(t *testing.T) {
	existing := []string{
		ReasonAuroraNotSupported, ReasonClusterMemberNotSupported, ReasonModeOff, ReasonUnknownEngine,
		ReasonUnknownInstanceClass, ReasonEngineNotPriced, ReasonUnknownDeployment, ReasonUnverifiedRate,
		ReasonInstanceClassIsAFailover, ReasonFreeableMemoryIsPageCache, ReasonBufferPoolScalesWithClass,
		ReasonMemorySemanticsUnencoded, ReasonStorageCannotShrink, ReasonStorageAutoscalingRatchet,
		ReasonReplicaIsFailoverCapacity, ReasonMultiAZIsAvailabilityPosture, ReasonInsufficientWindow,
		ReasonNoMetricEvidence, ReasonTruncatedMetrics, ReasonSizeFlexibilityExcluded,
		ReasonInstanceStateUnstable, ReasonNoStoragePerformanceModel, ReasonCommitmentNegative,
		ReasonCommitmentNeutral, AdvisoryIdleInstance, AdvisoryIdleReadReplica, AdvisoryStorageFloor,
		AdvisoryStorageAutoscaling, AdvisoryMultiAZMultiplier, AdvisoryUnverifiedRate,
		AdvisoryReservationStranding,
	}
	seen := map[string]bool{}
	for _, c := range existing {
		seen[c] = true
	}
	added := map[string]string{
		"ReasonGrowthInsufficientObservations": ReasonGrowthInsufficientObservations,
		"ReasonGrowthSpanTooShort":             ReasonGrowthSpanTooShort,
		"ReasonGrowthHistoryInconsistent":      ReasonGrowthHistoryInconsistent,
		"ReasonGrowthIsNotReclaimable":         ReasonGrowthIsNotReclaimable,
		"ReasonGrowthRateNotProjectable":       ReasonGrowthRateNotProjectable,
		"AdvisoryStorageGrew":                  AdvisoryStorageGrew,
	}
	for name, code := range added {
		if code == "" {
			t.Errorf("%s is empty", name)
			continue
		}
		if code != strings.ToLower(code) || strings.ContainsAny(code, " _") {
			t.Errorf("%s = %q: codes are lower-case and hyphenated so they are safe to store and group on",
				name, code)
		}
		if seen[code] {
			t.Errorf("%s = %q collides with a code this package already ships; two codes with one value "+
				"silently merge two findings", name, code)
		}
		seen[code] = true
	}
	// The states are distinct too, and none is the zero value by accident.
	states := map[GrowthState]bool{}
	for _, s := range []GrowthState{GrowthNoHistory, GrowthTooFewObservations, GrowthSpanTooShort,
		GrowthInconsistent, GrowthFlat, GrowthGrew} {
		if s == GrowthUnevaluated {
			t.Errorf("state %q equals the unevaluated zero value", s)
		}
		if states[s] {
			t.Errorf("state %q is declared twice", s)
		}
		states[s] = true
	}
}

// FuzzGrowthNeverClaimsASaving drives arbitrary histories — including
// nonsensical, decreasing, zero-span and enormous ones — through the whole
// path and asserts the invariants that must hold over every one of them.
func FuzzGrowthNeverClaimsASaving(f *testing.F) {
	f.Add(4, int64(100), int64(50), int64(3600), int64(0))
	f.Add(0, int64(0), int64(0), int64(0), int64(0))
	f.Add(50, int64(1), int64(-1), int64(1), int64(1))
	f.Add(9, int64(1<<40), int64(1<<40), int64(1), int64(1<<40))

	f.Fuzz(func(t *testing.T, n int, start, step, gapSec, jitter int64) {
		if n < 0 {
			n = -n
		}
		n %= 200
		if gapSec < 0 {
			gapSec = -gapSec
		}
		gapSec %= 90 * 24 * 3600
		h := make(StorageHistory, 0, n)
		at := testEnd.Add(-200 * 24 * time.Hour)
		alloc := start % (1 << 20)
		for i := 0; i < n; i++ {
			h = append(h, StorageObservation{At: at, AllocatedGiB: alloc})
			at = at.Add(time.Duration(gapSec+int64(i)*(jitter%3600)) * time.Second)
			alloc += step % (1 << 16)
		}
		inst := growInstance(alloc)
		v := AssessStorageGrowth(inst, h, DefaultGrowthPolicy(), DefaultRates())

		if v.State == GrowthUnevaluated {
			t.Fatal("AssessStorageGrowth returned the unevaluated zero state; it always answers")
		}
		if v.State.Measured() != (v.Measurement != nil) {
			t.Fatalf("state %q: Measured()=%v but Measurement!=nil is %v",
				v.State, v.State.Measured(), v.Measurement != nil)
		}
		if !v.State.Measured() && v.Code == "" {
			t.Fatalf("state %q carries no refusal code", v.State)
		}
		if m := v.Measurement; m != nil {
			if m.GrewGiB < 0 {
				t.Fatalf("a negative growth %d escaped; a decrease is inconsistent, never a reduction",
					m.GrewGiB)
			}
			if m.MonthlyUSD < 0 || !finite(m.MonthlyUSD) {
				t.Fatalf("growth cost %v is not a finite non-negative magnitude", m.MonthlyUSD)
			}
			if (m.GrewGiB == 0) != (v.State == GrowthFlat) {
				t.Fatalf("state %q with GrewGiB %d: flat and grew must partition the measured states",
					v.State, m.GrewGiB)
			}
			if p := m.Projection; p != nil {
				if !finite(p.GiBPerDay) || p.GiBPerDay < 0 {
					t.Fatalf("projected rate %v is not a finite non-negative number", p.GiBPerDay)
				}
				if p.Claimable() {
					t.Fatal("a projection claims to be claimable")
				}
				if p.Steps < DefaultGrowthMinSteps {
					t.Fatalf("a rate was projected from %d step(s)", p.Steps)
				}
				if v.Sampling.Span < DefaultGrowthMinSpan {
					t.Fatalf("a rate was projected across %s", v.Sampling.Span)
				}
			}
		}
		if v.Sampling.Observations < DefaultGrowthMinObservations && v.State.Measured() {
			t.Fatalf("%d observations were measured", v.Sampling.Observations)
		}

		// And through the sizer: still no proposal, still a valid report.
		snap := &Snapshot{Domain: Kind, Window: testWindow(), Timestamp: testEnd, Targets: []Target{{
			Ref:            domain.TargetRef{Domain: Kind, Scope: "1234/us-east-1", ID: inst.ARN, Name: inst.Identifier},
			Instance:       inst,
			StorageHistory: h,
		}}}
		s, err := NewSizer(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		rep := s.Assess(testNow, snap, nil)
		if err := rep.Validate(); err != nil {
			t.Fatalf("a growth history produced an invalid report: %v", err)
		}
		if rep.Totals.Proposals != 0 || rep.Totals.GrossSavingsMonthlyUSD != 0 {
			t.Fatalf("a growth history produced %d proposals and %v of savings",
				rep.Totals.Proposals, rep.Totals.GrossSavingsMonthlyUSD)
		}
	})
}

// The persistence loop, end to end and without a store: Restore → fold the
// prior histories in → Observe → Checkpoint, repeated until the bar is
// cleared. This is exactly the sequence GROWTH-FINDINGS.md §6 asks cmd/ for,
// and it is the reason `AttributeStorageGrowth` was unreachable until now.
func TestHistorySurvivesTheCheckpointLoopAndReachesAVerdict(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("ratchet", "db.m6i.large", "postgres", withStorage(200, 2000, StorageGP2)),
		},
		Metrics: metricsFor("ratchet", 30, 10, 8<<30, 20*GiB),
	}
	cfg := DefaultConfig()
	var blob []byte
	// Twenty daily runs. Storage autoscales twice along the way — nothing
	// Kilter did, nothing CloudTrail recorded.
	for day := 0; day < 20; day++ {
		d, err := NewDomain(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Restore(blob); err != nil {
			t.Fatalf("day %d: Restore: %v", day, err)
		}
		switch day {
		case 7:
			f.Instances[0].AllocatedStorage = 260
		case 14:
			f.Instances[0].AllocatedStorage = 330
		}
		snap := collect(t, f)
		snap.Timestamp = testEnd.Add(-time.Duration(19-day) * 24 * time.Hour)

		// The two calls cmd/ owes, and the only two.
		RecordStorageHistory(snap, d.StorageHistories(), cfg.Growth)
		if err := d.Observe(snap); err != nil {
			t.Fatalf("day %d: Observe: %v", day, err)
		}
		if blob, err = d.Checkpoint(); err != nil {
			t.Fatalf("day %d: Checkpoint: %v", day, err)
		}

		rep := d.Report(testNow, nil)
		if err := rep.Validate(); err != nil {
			t.Fatalf("day %d: %v", day, err)
		}
		a := must(t, rep, "ratchet")
		switch {
		case day < DefaultGrowthMinObservations-1:
			wantCode(t, a, ReasonGrowthInsufficientObservations)
			if a.Growth.Measured() {
				t.Fatalf("day %d: %d observations were measured", day, day+1)
			}
		case day < 14:
			// Enough observations from day 3 on; the SPAN gate holds alone
			// until day 14, which is the point of having two codes.
			if a.Growth.Measured() {
				t.Fatalf("day %d: measured across only %s", day, a.Growth.Sampling.Span)
			}
			wantCode(t, a, ReasonGrowthSpanTooShort)
		default:
			wantNoCode(t, a, ReasonGrowthSpanTooShort)
			wantNoCode(t, a, ReasonGrowthInsufficientObservations)
			if a.Growth.State != GrowthGrew {
				t.Fatalf("day %d: state %q, want %q", day, a.Growth.State, GrowthGrew)
			}
		}
	}

	// The final verdict: 130 GiB across 19 days in two steps, refused with its
	// size, and a rate that survives both gates.
	d, err := NewDomain(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Restore(blob); err != nil {
		t.Fatal(err)
	}
	a := must(t, d.Report(testNow, nil), "ratchet")
	wantCode(t, a, ReasonGrowthIsNotReclaimable)
	grew, ok := a.Growth.GrewGiB()
	if !ok || grew != 130 {
		t.Fatalf("grew %d (ok=%v), want 130", grew, ok)
	}
	if a.Growth.Measurement.Steps != 2 {
		t.Fatalf("steps = %d, want 2", a.Growth.Measurement.Steps)
	}
	if p, ok := a.Growth.Rate(); !ok {
		t.Fatalf("no rate from two steps across 19 days: %s", a.Growth.RateReason)
	} else if p.SpanDays < 18 || p.SpanDays > 20 {
		t.Fatalf("rate span %v days, want ~19", p.SpanDays)
	}
	// A rerun of the same day is idempotent: it must not manufacture a step.
	snap := collect(t, f)
	snap.Timestamp = testEnd
	RecordStorageHistory(snap, d.StorageHistories(), cfg.Growth)
	if err := d.Observe(snap); err != nil {
		t.Fatal(err)
	}
	again := must(t, d.Report(testNow, nil), "ratchet")
	if again.Growth.Sampling.Observations != a.Growth.Sampling.Observations {
		t.Fatalf("re-recording the same instant changed the observation count: %d → %d",
			a.Growth.Sampling.Observations, again.Growth.Sampling.Observations)
	}
	if g, _ := again.Growth.GrewGiB(); g != grew {
		t.Fatalf("re-recording changed the growth: %d → %d", grew, g)
	}
	// StorageHistories hands out copies; mutating one cannot reach the domain.
	hs := d.StorageHistories()
	for id := range hs {
		hs[id][0].AllocatedGiB = 999999
	}
	if g, _ := must(t, d.Report(testNow, nil), "ratchet").Growth.GrewGiB(); g != grew {
		t.Fatal("StorageHistories leaked the domain's own slices")
	}
}
