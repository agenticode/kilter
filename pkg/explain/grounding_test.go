package explain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

// emptyStore is a substrate that has never heard of anything.
func emptyStore(t *testing.T) *evidence.Memory {
	t.Helper()
	mem, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	return mem
}

// typoSubject is the shape of cmd/BRAINWIRE-FINDINGS.md §6.1's observation: a
// container name that does not exist, differing from a real one by a
// character. Nothing about it is malformed, so validation cannot catch it.
func typoSubject() evidence.SubjectRef {
	return evidence.ContainerSubject(testCluster, containerKey("payments", "api", "serve"))
}

// TestAbsentSubjectIsTypedNotJustNoted is the §6.1 gap. The payload was
// already honest in prose; what it lacked was a signal a route could turn
// into a 422 and a shell into a non-zero exit.
func TestAbsentSubjectIsTypedNotJustNoted(t *testing.T) {
	req := explainRequest(t)
	req.Store = explainStore(t)
	req.Subject = typoSubject()
	req.Rec, req.Verdict = nil, nil

	ex := mustExplain(t, req)

	if got := ex.GroundingState(); got != GroundingAbsent {
		t.Fatalf("GroundingState = %q, want %q", got, GroundingAbsent)
	}
	err := ex.GroundingError()
	if err == nil {
		t.Fatal("an unknown subject produced no error; this is the 200-and-exit-0 bug")
	}
	if !errors.Is(err, ErrNoEvidence) {
		t.Errorf("error %v does not match ErrNoEvidence, so callers cannot switch on it", err)
	}
	var ne *NoEvidenceError
	if !errors.As(err, &ne) {
		t.Fatalf("error %v is not a *NoEvidenceError", err)
	}
	if ne.Subject != ex.Subject || !ne.From.Equal(ex.From) || !ne.To.Equal(ex.To) {
		t.Errorf("error names %s over [%v,%v), payload is %s over [%v,%v)",
			ne.Subject, ne.From, ne.To, ex.Subject, ex.From, ex.To)
	}
	// The payload is still returned. The route may answer 422 with it; the
	// point is that it now has the choice.
	if len(ex.Notes) == 0 || !strings.Contains(strings.Join(ex.Notes, " "), "no evidence is stored") {
		t.Errorf("the note that always said this must survive: %v", ex.Notes)
	}
	if err := ex.Verify(Resolver{Store: req.Store}); err != nil {
		t.Fatalf("an ungrounded payload must still verify: %v", err)
	}
}

// TestThinEvidenceIsNotAbsent is the other half, and the one that makes the
// fix safe: a subject the store DOES know, whose records simply ground no
// driver, must keep its payload and must not raise the error.
func TestThinEvidenceIsNotAbsent(t *testing.T) {
	mem := emptyStore(t)
	subj := explainSubject()
	// One OOMKill and nothing else. It is a real record about a real
	// subject, and no driver can stand on it without a recommendation to
	// attach the OOM floor to — so the payload cites nothing.
	if err := mem.Append(ev(t0.Add(hours(3)), evidence.EventOOMKill, evidence.SeverityCritical,
		subj, map[string]string{"container": "server"})); err != nil {
		t.Fatalf("Append: %v", err)
	}
	req := explainRequest(t)
	req.Store, req.Rec, req.Verdict = mem, nil, nil

	ex := mustExplain(t, req)

	if got := ex.GroundingState(); got != GroundingThin {
		t.Fatalf("GroundingState = %q, want %q (grounding=%+v)", got, GroundingThin, ex.Grounding)
	}
	if err := ex.GroundingError(); err != nil {
		t.Fatalf("thin evidence must not be an error, it is an answer: %v", err)
	}
	if len(ex.Citations) != 0 {
		t.Fatalf("fixture is wrong: expected an uncitable payload, got %v", ex.Citations)
	}
	if ex.Grounding == nil || ex.Grounding.Events != 1 {
		t.Errorf("the stored event must be counted: %+v", ex.Grounding)
	}
	joined := strings.Join(ex.Notes, " ")
	if strings.Contains(joined, "no evidence is stored") {
		t.Errorf("a subject with a stored event must not be told it has no evidence: %v", ex.Notes)
	}
	if !strings.Contains(joined, "but no driver could be grounded in it") {
		t.Errorf("the thin case needs its own sentence: %v", ex.Notes)
	}
}

// TestThinHistoryStaysGrounded pins the path the fix must not break: five
// samples is thin history by any standard — the recommender wants thirty —
// and it is a perfectly good explanation that cites its digests.
func TestThinHistoryStaysGrounded(t *testing.T) {
	mem := emptyStore(t)
	subj := explainSubject()
	for i := 0; i < 5; i++ {
		if err := mem.ObserveSample(subj, evidence.Sample{
			At: t0.Add(time.Duration(i) * 40 * time.Minute), MilliCPU: 100, MemoryBytes: 1 << 20,
		}); err != nil {
			t.Fatalf("ObserveSample: %v", err)
		}
	}
	req := explainRequest(t)
	req.Store, req.Rec, req.Verdict = mem, nil, nil

	ex := mustExplain(t, req)

	if got := ex.GroundingState(); got != GroundingGrounded {
		t.Fatalf("GroundingState = %q, want %q: thin is not absent and neither is small",
			got, GroundingGrounded)
	}
	if err := ex.GroundingError(); err != nil {
		t.Fatalf("two samples is an answer, not an error: %v", err)
	}
	if ex.Grounding != nil {
		t.Errorf("a grounded payload carries no grounding report, its citations are the report: %+v", ex.Grounding)
	}
	if len(ex.Citations) == 0 {
		t.Error("five samples must still produce a citable usage-history driver")
	}
}

// TestUsageOutsideTheRequestedTierIsThinNotAbsent is a bug this unit found
// while drawing the boundary. The usage summary queries every stored tier;
// the usage-history driver may only cite digests of the REQUESTED tier. A
// subject whose history has not yet rolled into an hourly digest therefore
// gets a payload with a populated Usage block, real sample counts — and zero
// citations. Under the old rule (citations == 0) it was told "no evidence is
// stored for this subject in this window", while carrying that subject's
// usage two fields higher up. The statement was simply false.
func TestUsageOutsideTheRequestedTierIsThinNotAbsent(t *testing.T) {
	mem := emptyStore(t)
	subj := explainSubject()
	for i := 0; i < 2; i++ {
		if err := mem.ObserveSample(subj, evidence.Sample{
			At: t0.Add(time.Duration(i) * time.Minute), MilliCPU: 100, MemoryBytes: 1 << 20,
		}); err != nil {
			t.Fatalf("ObserveSample: %v", err)
		}
	}
	req := explainRequest(t)
	req.Store, req.Rec, req.Verdict = mem, nil, nil
	req.DigestTier = evidence.TierHourly // the default; two samples have not rolled into one

	ex := mustExplain(t, req)

	if ex.Usage.Samples != 2 {
		t.Fatalf("fixture is wrong: usage reports %d samples, want 2", ex.Usage.Samples)
	}
	if len(ex.Citations) != 0 {
		t.Fatalf("fixture is wrong: expected no citable hourly digest, got %v", ex.Citations)
	}
	if got := ex.GroundingState(); got != GroundingThin {
		t.Fatalf("GroundingState = %q, want %q", got, GroundingThin)
	}
	if err := ex.GroundingError(); err != nil {
		t.Fatalf("a subject with two stored samples is not an unknown subject: %v", err)
	}
	if strings.Contains(strings.Join(ex.Notes, " "), "no evidence is stored") {
		t.Errorf("the payload states this subject's usage; it may not also say there is none: %v", ex.Notes)
	}
	if ex.Grounding == nil || ex.Grounding.UsageWindows == 0 {
		t.Errorf("the unsuppressable usage witness must be counted: %+v", ex.Grounding)
	}
}

// TestAbsenceIsNeverClaimedAboutASectionNobodyAskedFor: a caller who
// suppresses a section must not be told the subject does not exist. The
// dossier reports what its caps dropped, and that report is proof of
// existence.
func TestAbsenceIsNeverClaimedAboutASectionNobodyAskedFor(t *testing.T) {
	mem := emptyStore(t)
	subj := explainSubject()
	// Events only: no samples, so the usage summary — the witness a caller
	// cannot suppress — is empty and the suppressed section is the only
	// evidence there is.
	for i := 0; i < 3; i++ {
		if err := mem.Append(ev(t0.Add(hours(i+1)), evidence.EventOOMKill, evidence.SeverityCritical,
			subj, map[string]string{"container": "server"})); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	req := explainRequest(t)
	req.Store, req.Rec, req.Verdict = mem, nil, nil
	req.MaxEvents, req.MaxDecisions, req.MaxDigests = -1, -1, -1

	ex := mustExplain(t, req)

	if ex.Grounding == nil || ex.Grounding.Withheld != 3 {
		t.Fatalf("the three withheld events must be counted: %+v", ex.Grounding)
	}
	if got := ex.GroundingState(); got != GroundingThin {
		t.Fatalf("GroundingState = %q, want %q: a cap is not an absence", got, GroundingThin)
	}
	if err := ex.GroundingError(); err != nil {
		t.Fatalf("suppressing a section must not manufacture a 422: %v", err)
	}
}

// TestParentEventsDoNotGroundTheSubject is the sharp edge of the boundary: a
// container that never ran, under a workload that deploys. The payload can
// cite the parent's deploy — BuildExplain borrows those on purpose — but the
// citation describes the workload. The subject still has no history, and the
// answer is still 422.
func TestParentEventsDoNotGroundTheSubject(t *testing.T) {
	mem := emptyStore(t)
	if err := mem.Append(ev(t0.Add(hours(6)), evidence.EventDeploy, evidence.SeverityInfo,
		workloadSubject("payments", "api"), map[string]string{"replicas": "12"})); err != nil {
		t.Fatalf("Append: %v", err)
	}
	req := explainRequest(t)
	req.Store, req.Rec, req.Verdict = mem, nil, nil
	req.Subject = typoSubject() // a container of that same real workload

	ex := mustExplain(t, req)

	if len(ex.Citations) == 0 {
		t.Fatal("fixture is wrong: the parent's deploy should have been borrowed")
	}
	if ex.Grounding == nil || ex.Grounding.ParentEvents != 1 {
		t.Fatalf("the borrowed event must be counted separately: %+v", ex.Grounding)
	}
	if ex.Grounding.Any() {
		t.Errorf("a borrowed event must not count as the subject's own record: %+v", ex.Grounding)
	}
	if got := ex.GroundingState(); got != GroundingAbsent {
		t.Fatalf("GroundingState = %q, want %q: the workload's history is not the container's",
			got, GroundingAbsent)
	}
	if ex.GroundingError() == nil {
		t.Error("a container with only its parent's events must still be a 422")
	}
	note := strings.Join(ex.Notes, " ")
	if !strings.Contains(note, "no evidence is stored") || !strings.Contains(note, "parent workload") {
		t.Errorf("the note must say the citations are borrowed: %v", ex.Notes)
	}
}

// TestGroundingArithmetic pins the boundary itself, independently of any
// store: which counts mean which state.
func TestGroundingArithmetic(t *testing.T) {
	cases := []struct {
		name      string
		g         Grounding
		citations int
		want      GroundingState
	}{
		{"nothing at all", Grounding{}, 0, GroundingAbsent},
		{"borrowed events only", Grounding{ParentEvents: 4}, 4, GroundingAbsent},
		{"one digest", Grounding{Digests: 1}, 0, GroundingThin},
		{"one event", Grounding{Events: 1}, 0, GroundingThin},
		{"one decision", Grounding{Decisions: 1}, 0, GroundingThin},
		{"one usage window", Grounding{UsageWindows: 1}, 0, GroundingThin},
		{"samples without a window count", Grounding{Samples: 7}, 0, GroundingThin},
		{"withheld by a cap", Grounding{Withheld: 2}, 0, GroundingThin},
		{"cited", Grounding{UsageWindows: 1}, 1, GroundingGrounded},
		{"cited over withheld", Grounding{Withheld: 9}, 3, GroundingGrounded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.g.stateFor(tc.citations); got != tc.want {
				t.Errorf("stateFor(%d) on %+v = %q, want %q", tc.citations, tc.g, got, tc.want)
			}
		})
	}
}

// TestGroundingStateNeverSilentlyClaimsAbsence: a payload that never computed
// grounding reports unknown, and unknown does not raise the error. Absence is
// a computed fact and only a computed fact may be asserted.
func TestGroundingStateNeverSilentlyClaimsAbsence(t *testing.T) {
	handBuilt := &Explanation{Subject: explainSubject(), Action: ActionUnknown}
	if got := handBuilt.GroundingState(); got != GroundingUnknown {
		t.Errorf("GroundingState = %q, want %q", got, GroundingUnknown)
	}
	if err := handBuilt.GroundingError(); err != nil {
		t.Errorf("a payload that did not compute absence must not assert it: %v", err)
	}
	cited := &Explanation{Subject: explainSubject(), Citations: []ID{"dig/1/x@1"}}
	if got := cited.GroundingState(); got != GroundingGrounded {
		t.Errorf("GroundingState = %q, want %q", got, GroundingGrounded)
	}
}

// TestGroundingSurvivesJSON: the state a caller acts on must survive the wire,
// because pkg/api serves this payload and cmd/kilter reads it back.
func TestGroundingSurvivesJSON(t *testing.T) {
	req := explainRequest(t)
	req.Store, req.Rec, req.Verdict = emptyStore(t), nil, nil
	req.Subject = typoSubject()
	ex := mustExplain(t, req)

	raw, err := json.Marshal(ex)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Explanation
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := back.GroundingState(); got != GroundingAbsent {
		t.Errorf("decoded GroundingState = %q, want %q", got, GroundingAbsent)
	}
	if back.GroundingError() == nil {
		t.Error("a decoded absent payload must still be an error")
	}
	if !strings.Contains(string(raw), `"grounding"`) {
		t.Error("the absent state must be visible to a JSON consumer, not only to Go")
	}
}

// TestGroundedPayloadIsByteIdenticalToBefore: the report is omitted from a
// grounded payload, which is what keeps the golden fixtures byte-identical
// and every existing consumer unaffected.
func TestGroundedPayloadCarriesNoNewBytes(t *testing.T) {
	ex := mustExplain(t, explainRequest(t))
	if ex.Grounding != nil || ex.VerdictOrigin != nil {
		t.Fatalf("a grounded, verdict-carrying payload must gain no fields: %+v / %+v",
			ex.Grounding, ex.VerdictOrigin)
	}
	raw, err := json.Marshal(ex)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{`"grounding"`, `"verdictOrigin"`} {
		if strings.Contains(string(raw), k) {
			t.Errorf("%s must be absent from a grounded payload", k)
		}
	}
}

// TestAbsentPayloadIsDeterministic: the new fields are computed from counts,
// so they must not introduce map iteration or a clock.
func TestAbsentPayloadIsDeterministic(t *testing.T) {
	var want []byte
	for i := 0; i < 16; i++ {
		req := explainRequest(t)
		req.Subject = typoSubject()
		got, err := json.Marshal(mustExplain(t, req))
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
