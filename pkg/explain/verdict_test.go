package explain

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/patterns"
	"github.com/agenticode/kilter/pkg/recommend"
)

// readoutWire mirrors recommend.Verdict's wire shape. It exists because the
// decision verdict inside a recommend.Verdict is unexported and reachable
// only through Decision()'s comma-ok — deliberately, so nothing can
// manufacture one — and UnmarshalJSON is the seam's own supported way to
// produce a computed readout. Nothing in this package needs it; the tests
// need it to exercise the branch production will take once
// pkg/recommend/VERDICT-FINDINGS.md §7 lands.
type readoutWire struct {
	Key            model.ContainerKey        `json:"key"`
	Disposition    recommend.Disposition     `json:"disposition"`
	CurrentRequest model.Resources           `json:"currentRequest"`
	CurrentLimit   model.Resources           `json:"currentLimit"`
	Samples        int                       `json:"samples"`
	Window         time.Duration             `json:"window"`
	Rec            *recommend.Recommendation `json:"recommendation,omitempty"`
	VerdictState   recommend.VerdictState    `json:"verdictState"`
	Decision       *decision.Verdict         `json:"verdict,omitempty"`
}

func decodeReadout(t *testing.T, w readoutWire) recommend.Verdict {
	t.Helper()
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal readout: %v", err)
	}
	var rv recommend.Verdict
	if err := json.Unmarshal(raw, &rv); err != nil {
		t.Fatalf("unmarshal readout: %v", err)
	}
	return rv
}

// notComputedReadout is what recommend.Verdicts returns for every container
// today: a real disposition, and no decision verdict at all.
func notComputedReadout() recommend.Verdict {
	return recommend.Verdict{
		Key:         explainKey,
		Disposition: recommend.DispositionInsufficientHistory,
		Samples:     12,
		Window:      hours(3),
	}
}

// TestReadoutWithoutAVerdictIsNotComputedNotRefused is the whole point of the
// bridge. The production path reached a branch; it did not reach a judgement.
// The payload must be able to say the first without implying the second.
func TestReadoutWithoutAVerdictIsNotComputedNotRefused(t *testing.T) {
	rv := notComputedReadout()
	if _, ok := rv.Decision(); ok {
		t.Fatal("fixture is wrong: this readout must carry no decision verdict")
	}
	req := explainRequest(t)
	req.Verdict, req.Rec = nil, nil
	req.RecVerdict = &rv

	ex := mustExplain(t, req)

	if got := ex.VerdictState(); got != VerdictNotComputed {
		t.Fatalf("VerdictState = %q, want %q", got, VerdictNotComputed)
	}
	if ex.Action != ActionUnknown {
		t.Errorf("Action = %q, want %q: an uncomputed verdict has no action", ex.Action, ActionUnknown)
	}
	if ex.Refusal != nil {
		t.Errorf("Refusal = %+v, want nil: a disposition is not a refusal", ex.Refusal)
	}
	if ex.Refused() {
		t.Error("Refused() must be false: the engine reached no judgement to refuse with")
	}
	if ex.Confidence != nil {
		t.Errorf("Confidence = %+v, want nil: no verdict, no scored basis", ex.Confidence)
	}
	if ex.VerdictOrigin == nil || ex.VerdictOrigin.Disposition != string(recommend.DispositionInsufficientHistory) {
		t.Fatalf("the disposition must be reported: %+v", ex.VerdictOrigin)
	}
	if ex.VerdictOrigin.Samples != 12 || ex.VerdictOrigin.Window != hours(3) {
		t.Errorf("the history the disposition was reached on must come through: %+v", ex.VerdictOrigin)
	}
	note := strings.Join(ex.Notes, " ")
	if !strings.Contains(note, "no decision verdict was computed") ||
		!strings.Contains(note, "An absent verdict is not a refusal") {
		t.Errorf("the note must state the absence and disclaim the refusal: %v", ex.Notes)
	}
	prose := ex.Prose()
	if !strings.Contains(prose, "Verdict: not computed") ||
		!strings.Contains(prose, `"insufficient-history"`) {
		t.Errorf("prose must name the branch production took:\n%s", prose)
	}
	if strings.Contains(prose, "Verdict: refuse") {
		t.Fatalf("prose reads as a refusal:\n%s", prose)
	}
}

// TestNoDispositionIsEverRenderedAsARefusalCode walks all four dispositions.
// insufficient-history the disposition and CodeInsufficientHistory the
// refusal share a name and a default threshold and are not the same fact
// (pkg/recommend/VERDICT-FINDINGS.md §1.2).
func TestNoDispositionIsEverRenderedAsARefusalCode(t *testing.T) {
	for _, d := range []recommend.Disposition{
		recommend.DispositionRecommended,
		recommend.DispositionNeverObserved,
		recommend.DispositionInsufficientHistory,
		recommend.DispositionNoSignificantChange,
	} {
		t.Run(string(d), func(t *testing.T) {
			rv := notComputedReadout()
			rv.Disposition = d
			if d == recommend.DispositionRecommended {
				rv.Rec = explainRec()
			}
			req := explainRequest(t)
			req.Verdict, req.Rec = nil, nil
			req.RecVerdict = &rv

			ex := mustExplain(t, req)

			if ex.Refusal != nil || ex.Refused() {
				t.Fatalf("disposition %q produced a refusal: %+v", d, ex.Refusal)
			}
			if ex.Action != ActionUnknown {
				t.Errorf("disposition %q set Action to %q", d, ex.Action)
			}
			for _, drv := range ex.Drivers {
				if drv.Kind == DriverRefusal {
					t.Errorf("disposition %q produced a refusal driver: %+v", d, drv)
				}
			}
			raw, err := json.Marshal(ex)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(raw), `"refusal"`) {
				t.Errorf("disposition %q put a refusal on the wire: %s", d, raw)
			}
		})
	}
}

// TestReadoutSuppliesTheServedRecommendation: when the disposition is
// "recommended" the readout carries the very Recommendation production
// served, so the caller does not have to fetch it a second way and risk a
// different answer.
func TestReadoutSuppliesTheServedRecommendation(t *testing.T) {
	rv := notComputedReadout()
	rv.Disposition = recommend.DispositionRecommended
	rv.Rec = explainRec()

	req := explainRequest(t)
	req.Verdict, req.Rec = nil, nil
	req.RecVerdict = &rv

	ex := mustExplain(t, req)

	if ex.Sizing == nil {
		t.Fatal("the readout's recommendation must become the payload's sizing")
	}
	if ex.Sizing.Container != explainKey.Container || ex.Sizing.Reason != rv.Rec.Reason {
		t.Errorf("sizing does not match the served recommendation: %+v", ex.Sizing)
	}
	if ex.Sizing.TargetRequest != rv.Rec.TargetRequest {
		t.Errorf("target request = %+v, want %+v", ex.Sizing.TargetRequest, rv.Rec.TargetRequest)
	}
}

// TestComputedVerdictIsCopiedNotRecomputed is the trap, pinned.
//
// The readout carries a verdict of "act". The evidence in the store, fed to
// decision.Evaluate as a caller would assemble it, refuses — there is a
// deploy inside the soak window. If this package ever re-evaluated instead of
// reporting, the payload would say "refuse" and this test would say so.
func TestComputedVerdictIsCopiedNotRecomputed(t *testing.T) {
	served := decision.Verdict{
		Action: decision.ActionAct,
		Confidence: decision.Compose(
			decision.TermHistoryDepth(120, 200),
			decision.TermWindowSpan(hours(30), hours(48)),
		),
	}
	// A second opinion over the same subject's evidence, computed the way a
	// caller tempted to "fill in the gap" would compute it.
	secondOpinion := decision.Evaluate(decision.Evidence{
		Samples:    120,
		Window:     hours(30),
		LastSample: t0.Add(hours(30)),
		Class:      patterns.ClassSteady,
		LastChange: t0.Add(hours(28)), // the deploy the fixture store records
	}, decision.DefaultConfig(), t0.Add(hours(30)))
	if secondOpinion == nil {
		t.Fatal("fixture is wrong: the second opinion must disagree for this test to mean anything")
	}
	if secondOpinion.Code != decision.CodePostChangeSoak {
		t.Logf("second opinion refuses with %q", secondOpinion.Code)
	}

	rv := decodeReadout(t, readoutWire{
		Key: explainKey, Disposition: recommend.DispositionRecommended,
		Samples: 120, Window: hours(30), Rec: explainRec(),
		VerdictState: recommend.VerdictComputed, Decision: &served,
	})
	source, ok := rv.Decision()
	if !ok {
		t.Fatal("fixture is wrong: this readout must carry a computed verdict")
	}

	req := explainRequest(t)
	req.Verdict, req.Rec = nil, nil
	req.RecVerdict = &rv

	ex := mustExplain(t, req)

	if got := ex.VerdictState(); got != VerdictComputed {
		t.Fatalf("VerdictState = %q, want %q", got, VerdictComputed)
	}
	if ex.Action != string(decision.ActionAct) {
		t.Fatalf("Action = %q, want %q — the payload re-derived a disposition instead of reporting one",
			ex.Action, decision.ActionAct)
	}
	if ex.Refusal != nil {
		t.Fatalf("Refusal = %+v: this is the second opinion leaking into the payload", ex.Refusal)
	}
	// Byte-for-byte the verdict the readout carried, not one shaped like it.
	if ex.Confidence == nil || !reflect.DeepEqual(*ex.Confidence, source.Confidence) {
		t.Errorf("confidence = %+v, want the readout's %+v", ex.Confidence, source.Confidence)
	}
	if ex.VerdictOrigin != nil {
		t.Errorf("a computed verdict needs no origin, Action proves it: %+v", ex.VerdictOrigin)
	}
	if ex.Refused() {
		t.Error("Refused() must be false for an act verdict")
	}
}

// TestTheThreeStatesAreDistinguishable is the type-level claim: refused, not
// computed and unknown differ in fields a consumer can switch on, in Go and
// on the wire.
func TestTheThreeStatesAreDistinguishable(t *testing.T) {
	refusalVerdict := decision.Verdict{
		Action:     decision.ActionRefuse,
		Confidence: decision.Compose(decision.TermHistoryDepth(120, 200)),
		Refusal: &decision.Refusal{
			Code:   decision.CodePostChangeSoak,
			Detail: "deploy 6h ago; steady soak is 6h",
			Until:  t0.Add(hours(30)),
		},
	}
	rv := notComputedReadout()

	cases := []struct {
		name      string
		mut       func(*ExplainRequest)
		wantState VerdictState
		wantAct   string
		refused   bool
		refusal   bool
		origin    bool
	}{
		{"refused", func(r *ExplainRequest) { r.Verdict = &refusalVerdict },
			VerdictComputed, string(decision.ActionRefuse), true, true, false},
		{"not computed", func(r *ExplainRequest) { r.Verdict, r.RecVerdict = nil, &rv },
			VerdictNotComputed, ActionUnknown, false, false, true},
		{"unknown", func(r *ExplainRequest) { r.Verdict, r.RecVerdict = nil, nil },
			VerdictUnknown, ActionUnknown, false, false, false},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := explainRequest(t)
			req.Rec = nil
			tc.mut(&req)
			ex := mustExplain(t, req)

			if got := ex.VerdictState(); got != tc.wantState {
				t.Errorf("VerdictState = %q, want %q", got, tc.wantState)
			}
			if ex.Action != tc.wantAct {
				t.Errorf("Action = %q, want %q", ex.Action, tc.wantAct)
			}
			if ex.Refused() != tc.refused {
				t.Errorf("Refused() = %v, want %v", ex.Refused(), tc.refused)
			}
			if (ex.Refusal != nil) != tc.refusal {
				t.Errorf("Refusal present = %v, want %v", ex.Refusal != nil, tc.refusal)
			}
			if (ex.VerdictOrigin != nil) != tc.origin {
				t.Errorf("VerdictOrigin present = %v, want %v", ex.VerdictOrigin != nil, tc.origin)
			}
			// The prose a human reads must differ too: three states that
			// render identically are three states nobody can act on.
			line := verdictLine(ex.Prose())
			if prev, dup := seen[line]; dup {
				t.Errorf("state %q renders exactly like %q: %q", tc.name, prev, line)
			}
			seen[line] = tc.name
		})
	}
	if len(seen) != 3 {
		t.Errorf("the three states produced %d distinct verdict lines", len(seen))
	}
}

// verdictLine extracts the "Verdict: ..." line from a prose rendering.
func verdictLine(prose string) string {
	for _, l := range strings.Split(prose, "\n") {
		if strings.HasPrefix(l, "Verdict:") {
			return l
		}
	}
	return ""
}

// TestVerdictStateSurvivesJSON: a consumer reading the payload back must
// still be able to tell the two absences apart.
func TestVerdictStateSurvivesJSON(t *testing.T) {
	rv := notComputedReadout()
	req := explainRequest(t)
	req.Verdict, req.Rec = nil, nil
	req.RecVerdict = &rv

	raw, err := json.Marshal(mustExplain(t, req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Explanation
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := back.VerdictState(); got != VerdictNotComputed {
		t.Errorf("decoded VerdictState = %q, want %q", got, VerdictNotComputed)
	}
	if back.Refused() {
		t.Error("a decoded not-computed payload must not read as refused")
	}
	if !strings.Contains(string(raw), `"state": "not-computed"`) &&
		!strings.Contains(string(raw), `"state":"not-computed"`) {
		t.Errorf("the not-computed state must be explicit on the wire: %s", raw)
	}
}

// TestTwoVerdictSourcesAreRefused: a payload with two sources for one
// disposition would have to pick, and picking silently is how a payload ends
// up reporting a verdict nobody reached.
func TestTwoVerdictSourcesAreRefused(t *testing.T) {
	rv := notComputedReadout()
	req := explainRequest(t)
	req.RecVerdict = &rv // explainRequest already sets Verdict
	_, err := BuildExplain(req)
	if err == nil {
		t.Fatal("supplying both a Verdict and a RecVerdict must fail")
	}
	if !strings.Contains(err.Error(), "two sources for one disposition") {
		t.Errorf("error %q does not say why", err)
	}
}

// TestVerdictWithoutAnActionIsRejected: the zero decision.Verdict used to
// render as a blank verdict — a payload asserting a disposition that is not
// one of the three.
func TestVerdictWithoutAnActionIsRejected(t *testing.T) {
	req := explainRequest(t)
	req.Verdict = &decision.Verdict{}
	_, err := BuildExplain(req)
	if err == nil {
		t.Fatal("a verdict with no action must be rejected, not rendered blank")
	}
	if !strings.Contains(err.Error(), "is not a verdict") {
		t.Errorf("error %q does not say why", err)
	}
	req.Verdict = &decision.Verdict{Action: decision.Action("maybe")}
	if _, err := BuildExplain(req); err == nil {
		t.Fatal("an action outside pkg/decision's three must be rejected")
	}
}

// TestReadoutMustBeAboutTheSubject: attributing one container's disposition
// to another subject is the same fabrication as inventing it.
func TestReadoutMustBeAboutTheSubject(t *testing.T) {
	other := notComputedReadout()
	other.Key = containerKey("payments", "api", "sidecar")

	req := explainRequest(t)
	req.Verdict, req.Rec = nil, nil
	req.RecVerdict = &other
	_, err := BuildExplain(req)
	if err == nil {
		t.Fatal("a readout about another container must be rejected")
	}
	if !strings.Contains(err.Error(), "may only explain the container it is about") {
		t.Errorf("error %q does not say why", err)
	}

	rv := notComputedReadout()
	req = explainRequest(t)
	req.Verdict, req.Rec = nil, nil
	req.RecVerdict = &rv
	req.Subject = workloadSubject("payments", "api")
	if _, err := BuildExplain(req); err == nil {
		t.Fatal("a container readout must not be attached to a workload subject")
	}
}

// TestRecAndReadoutMustAgree: the sizing and the disposition must come from
// the same answer.
func TestRecAndReadoutMustAgree(t *testing.T) {
	t.Run("silent disposition with a Rec", func(t *testing.T) {
		rv := notComputedReadout() // insufficient-history: Rec is nil
		req := explainRequest(t)
		req.Verdict = nil
		req.RecVerdict = &rv // Rec is still explainRec()
		_, err := BuildExplain(req)
		if err == nil {
			t.Fatal("a silent disposition alongside a recommendation must be rejected")
		}
		if !strings.Contains(err.Error(), "different answers") {
			t.Errorf("error %q does not say why", err)
		}
	})
	t.Run("two different sizings", func(t *testing.T) {
		rv := notComputedReadout()
		rv.Disposition = recommend.DispositionRecommended
		served := explainRec()
		rv.Rec = served
		stale := explainRec()
		stale.TargetRequest.MilliCPU = 999

		req := explainRequest(t)
		req.Verdict = nil
		req.Rec = stale
		req.RecVerdict = &rv
		_, err := BuildExplain(req)
		if err == nil {
			t.Fatal("two disagreeing sizings must be rejected, not silently reconciled")
		}
		if !strings.Contains(err.Error(), "differently") {
			t.Errorf("error %q does not say why", err)
		}
	})
	t.Run("agreeing sizings pass", func(t *testing.T) {
		rv := notComputedReadout()
		rv.Disposition = recommend.DispositionRecommended
		rv.Rec = explainRec()
		req := explainRequest(t)
		req.Verdict = nil
		req.Rec = explainRec()
		req.RecVerdict = &rv
		if _, err := BuildExplain(req); err != nil {
			t.Fatalf("identical sizings must be accepted: %v", err)
		}
	})
}

// TestNoSecondEvaluationInThisPackage is the trap as a structural check
// rather than a promise. pkg/explain may name pkg/decision's types; it may
// not call its computations. A new call fails this test until someone
// justifies it, which is the point — pkg/recommend/FINDINGS.md §6.4-1 refused
// a second evaluation site and this keeps the refusal enforced from the other
// side of the seam.
func TestNoSecondEvaluationInThisPackage(t *testing.T) {
	// Type conversions read as calls in the AST. Only these are allowed.
	allowed := map[string]map[string]bool{
		"decision":  {"Action": true, "RefusalCode": true},
		"recommend": {},
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	var checked, found int
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			checked++
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				ok2, watched := allowed[ident.Name]
				if !watched {
					return true
				}
				found++
				if !ok2[sel.Sel.Name] {
					t.Errorf("%s: %s.%s is a computation in another package's plane; "+
						"an explanation reports the answer the engine reached, it does not reach one",
						fset.Position(call.Pos()), ident.Name, sel.Sel.Name)
				}
				return true
			})
			_ = name
		}
	}
	if checked == 0 {
		t.Fatal("the scan read no files, so it proves nothing")
	}
	if found == 0 {
		t.Fatal("the scan found no decision/recommend selector at all; it is not looking where it thinks")
	}
}
