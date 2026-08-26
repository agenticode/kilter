package explain

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/patterns"
	"github.com/agenticode/kilter/pkg/recommend"
)

var explainKey = containerKey("payments", "api", "server")

func explainSubject() evidence.SubjectRef { return evidence.ContainerSubject(testCluster, explainKey) }

// explainStore builds a subject with a full case file: two days of hourly
// usage, an OOMKill, a deploy, sustained throttling, and a journalled
// recommendation.
func explainStore(t *testing.T) *evidence.Memory {
	t.Helper()
	mem, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	subj := explainSubject()
	for h := 0; h < 30; h++ {
		for s := 0; s < 4; s++ {
			at := t0.Add(time.Duration(h)*time.Hour + time.Duration(s)*15*time.Minute)
			smp := evidence.Sample{
				At:            at,
				MilliCPU:      int64(400 + 10*h + 5*s),
				MemoryBytes:   int64(1<<28) + int64(h)<<20,
				ThrottleRatio: 0.12,
			}
			if h == 12 && s == 0 {
				smp.OOMs = 1
			}
			if err := mem.ObserveSample(subj, smp); err != nil {
				t.Fatalf("ObserveSample: %v", err)
			}
		}
	}
	events := []evidence.EvidenceEvent{
		ev(t0.Add(hours(12)), evidence.EventOOMKill, evidence.SeverityCritical, subj,
			map[string]string{"container": "server"}),
		ev(t0.Add(hours(6)), evidence.EventDeploy, evidence.SeverityInfo,
			workloadSubject("payments", "api"), map[string]string{"replicas": "12"}),
		ev(t0.Add(hours(20)), evidence.EventThrottleHigh, evidence.SeverityWarning, subj,
			map[string]string{"ratio": "0.12"}),
		ev(t0.Add(hours(21)), evidence.EventHPAScale, evidence.SeverityInfo,
			workloadSubject("payments", "api"), map[string]string{"replicas": "14"}),
	}
	for _, e := range events {
		if err := mem.Append(e); err != nil {
			t.Fatalf("Append %s: %v", e.Kind, err)
		}
	}
	if err := mem.RecordDecision(evidence.DecisionRecord{
		At: t0.Add(hours(24)), Subject: subj, Kind: evidence.DecisionRecommendation,
		Summary: "hold memory at the OOM floor", Fingerprint: "f00dcafe",
		Payload: json.RawMessage(`{"target":"512Mi"}`),
	}); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	return mem
}

func explainRec() *recommend.Recommendation {
	return &recommend.Recommendation{
		Key:            explainKey,
		CurrentRequest: model.Resources{MilliCPU: 1000, MemoryBytes: 512 << 20},
		TargetRequest:  model.Resources{MilliCPU: 700, MemoryBytes: 512 << 20},
		CurrentLimit:   model.Resources{MilliCPU: 2000, MemoryBytes: 1024 << 20},
		TargetLimit:    model.Resources{MilliCPU: 1400, MemoryBytes: 1024 << 20},
		Confidence:     0.72,
		Samples:        120,
		WindowHours:    30,
		OOMCount:       1,
		Class:          patterns.ClassSteady,
		Reason:         "p95 cpu 690m with 20% headroom; memory held at the OOM floor",
	}
}

func explainRequest(t *testing.T) ExplainRequest {
	t.Helper()
	return ExplainRequest{
		Cluster: testCluster,
		Subject: explainSubject(),
		From:    t0,
		To:      t0.Add(hours(30)),
		Store:   explainStore(t),
		Rec:     explainRec(),
		Verdict: &decision.Verdict{
			Action: decision.ActionRecommendOnly,
			Confidence: decision.Compose(
				decision.TermHistoryDepth(120, 200),
				decision.TermWindowSpan(hours(30), hours(48)),
				decision.TermPostChangeSoak(hours(24), hours(6)),
			),
		},
		SavingsMonthlyUSD: 18.42,
		SavingsKnown:      true,
	}
}

func mustExplain(t *testing.T, req ExplainRequest) *Explanation {
	t.Helper()
	ex, err := BuildExplain(req)
	if err != nil {
		t.Fatalf("BuildExplain: %v", err)
	}
	return ex
}

func TestExplainGolden(t *testing.T) {
	ex := mustExplain(t, explainRequest(t))
	goldenJSON(t, "explain_recommendation", ex)
	goldenText(t, "explain_recommendation_prose", ex.Prose())
}

// TestExplainIsDeterministic runs the same request against independently
// built stores. Map iteration inside the substrate, the dossier, or the
// driver assembly would show up here.
func TestExplainIsDeterministic(t *testing.T) {
	var want []byte
	for i := 0; i < 32; i++ {
		got, err := json.Marshal(mustExplain(t, explainRequest(t)))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if i == 0 {
			want = got
			continue
		}
		if string(got) != string(want) {
			t.Fatalf("run %d differed\n got: %s\nwant: %s", i, got, want)
		}
	}
}

// TestEveryDriverIsGrounded is the §5.7 rule applied to the explain payload:
// a reason a narrator cannot cite must not be in the payload at all.
func TestEveryDriverIsGrounded(t *testing.T) {
	req := explainRequest(t)
	ex := mustExplain(t, req)
	if len(ex.Drivers) == 0 {
		t.Fatal("no drivers produced")
	}
	for _, d := range ex.Drivers {
		if len(d.Evidence) == 0 {
			t.Errorf("driver %s/%s carries no evidence id", d.Kind, d.Name)
		}
		if len(d.Evidence) > maxEvidencePerTerm {
			t.Errorf("driver %s cites %d ids, cap is %d", d.Kind, len(d.Evidence), maxEvidencePerTerm)
		}
	}
	if err := ex.Verify(Resolver{Store: req.Store}); err != nil {
		t.Fatalf("explanation does not verify: %v", err)
	}
}

// TestUngroundedDriversAreDroppedAndCounted: with an empty store the engine
// still has a verdict and a recommendation, but nothing to cite. The payload
// must state the decision and ground none of it — loudly.
func TestUngroundedDriversAreDroppedAndCounted(t *testing.T) {
	empty, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	req := explainRequest(t)
	req.Store = empty
	ex := mustExplain(t, req)
	if len(ex.Drivers) != 0 {
		t.Errorf("drivers survived with no evidence at all: %+v", ex.Drivers)
	}
	if ex.Ungrounded == 0 {
		t.Error("dropped drivers were not counted")
	}
	if len(ex.Citations) != 0 {
		t.Errorf("citations = %v, want none", ex.Citations)
	}
	joined := strings.Join(ex.Notes, " ")
	if !strings.Contains(joined, "dropped") || !strings.Contains(joined, "no evidence is stored") {
		t.Errorf("notes must state both the drop and the absence: %v", ex.Notes)
	}
	// The decision itself is still reported: dropping reasons is not the
	// same as hiding the answer.
	if ex.Sizing == nil || ex.Action != string(decision.ActionRecommendOnly) {
		t.Error("the verdict and sizing must survive even when nothing grounds them")
	}
	if err := ex.Verify(Resolver{Store: empty}); err != nil {
		t.Fatalf("an explanation citing nothing must still verify: %v", err)
	}
}

func TestExplainRefusalGetsChangeEvidence(t *testing.T) {
	req := explainRequest(t)
	until := t0.Add(hours(30))
	req.Verdict = &decision.Verdict{
		Action:     decision.ActionRefuse,
		Confidence: decision.Compose(decision.TermHistoryDepth(120, 200)),
		Refusal: &decision.Refusal{
			Code:   decision.CodePostChangeSoak,
			Detail: "deploy 6h ago; steady soak is 6h",
			Until:  until,
		},
	}
	ex := mustExplain(t, req)
	if ex.Action != string(decision.ActionRefuse) {
		t.Fatalf("Action = %q, want refuse", ex.Action)
	}
	var refusal *Driver
	for i := range ex.Drivers {
		if ex.Drivers[i].Kind == DriverRefusal {
			refusal = &ex.Drivers[i]
		}
	}
	if refusal == nil {
		t.Fatal("no refusal driver")
	}
	if refusal != &ex.Drivers[0] {
		t.Error("the refusal must lead the driver list: it is what stopped the decision")
	}
	if !anyIDContains(refusal.Evidence, "/deploy@") {
		t.Errorf("a post-change-soak refusal must cite the change: %v", refusal.Evidence)
	}
	if !strings.Contains(ex.Prose(), "Clears no earlier than") {
		t.Error("prose must state when a bounded refusal clears")
	}
}

func TestExplainWithoutVerdict(t *testing.T) {
	req := explainRequest(t)
	req.Verdict = nil
	ex := mustExplain(t, req)
	if ex.Action != ActionUnknown {
		t.Errorf("Action = %q, want %q", ex.Action, ActionUnknown)
	}
	if ex.Confidence != nil {
		t.Error("confidence must be absent without a verdict")
	}
	if !strings.Contains(ex.Prose(), "Verdict: none recorded") {
		t.Error("prose must say the verdict is missing rather than imply one")
	}
}

func TestExplainWithoutRecommendation(t *testing.T) {
	req := explainRequest(t)
	req.Rec = nil
	ex := mustExplain(t, req)
	if ex.Sizing != nil {
		t.Error("sizing must be absent without a recommendation")
	}
	if len(ex.Drivers) == 0 {
		t.Error("usage-history drivers should survive without a recommendation")
	}
}

// TestSavingsOnlyWhenPriced: this package prices nothing. A zero savings
// figure is a real answer and must be distinguishable from "not supplied".
func TestSavingsOnlyWhenPriced(t *testing.T) {
	req := explainRequest(t)
	req.SavingsKnown = false
	req.SavingsMonthlyUSD = 99
	ex := mustExplain(t, req)
	if ex.Sizing.SavingsMonthlyUSD != nil {
		t.Errorf("savings = %v, want absent when unpriced", *ex.Sizing.SavingsMonthlyUSD)
	}
	req.SavingsKnown = true
	req.SavingsMonthlyUSD = 0
	ex = mustExplain(t, req)
	if ex.Sizing.SavingsMonthlyUSD == nil || *ex.Sizing.SavingsMonthlyUSD != 0 {
		t.Error("an explicit zero must be reported, not swallowed")
	}
}

func TestExplainThrottlingAndOOMDrivers(t *testing.T) {
	ex := mustExplain(t, explainRequest(t))
	kinds := map[string]Driver{}
	for _, d := range ex.Drivers {
		kinds[d.Kind] = d
	}
	oom, ok := kinds[DriverOOMFloor]
	if !ok {
		t.Fatal("no oom-floor driver despite an OOMKill in the window")
	}
	if !anyIDContains(oom.Evidence, "/oomkill@") {
		t.Errorf("oom-floor cites %v, none of them the OOMKill", oom.Evidence)
	}
	thr, ok := kinds[DriverThrottled]
	if !ok {
		t.Fatal("no cpu-throttled driver despite a 12% throttle ratio")
	}
	if !anyIDContains(thr.Evidence, "/throttle-high@") && !anyIDContains(thr.Evidence, "dig/") {
		t.Errorf("cpu-throttled cites %v, none of them throttling evidence", thr.Evidence)
	}
	if _, ok := kinds[DriverPrior]; !ok {
		t.Error("no prior-decision driver despite a journalled recommendation")
	}
}

func TestExplainHPAGuardNeedsHPAEvidence(t *testing.T) {
	req := explainRequest(t)
	rec := explainRec()
	rec.CPUSkipped = true
	req.Rec = rec
	ex := mustExplain(t, req)
	var guard *Driver
	for i := range ex.Drivers {
		if ex.Drivers[i].Kind == DriverHPAGuard {
			guard = &ex.Drivers[i]
		}
	}
	if guard == nil {
		t.Fatal("no hpa-cpu-guard driver despite CPUSkipped")
	}
	if !anyIDContains(guard.Evidence, "/hpa-scale@") && !anyIDContains(guard.Evidence, "dec/") {
		t.Errorf("hpa-cpu-guard cites %v, none of it HPA evidence", guard.Evidence)
	}
}

func TestExplainValidation(t *testing.T) {
	base := explainRequest(t)
	cases := []struct {
		name string
		mut  func(*ExplainRequest)
		want string
	}{
		{"no store", func(r *ExplainRequest) { r.Store = nil }, "evidence store"},
		{"no subject", func(r *ExplainRequest) { r.Subject = evidence.SubjectRef{} }, "needs a subject"},
		{"zero from", func(r *ExplainRequest) { r.From = time.Time{} }, "bounded window"},
		{"zero to", func(r *ExplainRequest) { r.To = time.Time{} }, "bounded window"},
		{"inverted", func(r *ExplainRequest) { r.From, r.To = r.To, r.From }, "empty or inverted"},
		{"NaN savings", func(r *ExplainRequest) { r.SavingsMonthlyUSD = math.NaN() }, "not a usable amount"},
		{"Inf savings", func(r *ExplainRequest) { r.SavingsMonthlyUSD = math.Inf(1) }, "not a usable amount"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := explainRequest(t)
			tc.mut(&req)
			_, err := BuildExplain(req)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := BuildExplain(base); err != nil {
		t.Fatalf("the unmutated request must still pass: %v", err)
	}
}

// TestExplainSubjectClusterDefaulting: a caller who names the cluster once
// should not have to repeat it inside the subject.
func TestExplainSubjectClusterDefaulting(t *testing.T) {
	req := explainRequest(t)
	req.Subject = evidence.SubjectRef{Kind: evidence.SubjectContainer, Key: explainKey.String()}
	ex := mustExplain(t, req)
	if ex.Subject.Cluster != testCluster {
		t.Errorf("subject cluster = %q, want %q", ex.Subject.Cluster, testCluster)
	}
	if len(ex.Citations) == 0 {
		t.Error("defaulted subject resolved to nothing")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.0B"},
		{1023, "1023.0B"},
		{1024, "1.0KiB"},
		{1536, "1.5KiB"},
		{1 << 30, "1.0GiB"},
		{math.NaN(), "n/a"},
		{-1, "n/a"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
