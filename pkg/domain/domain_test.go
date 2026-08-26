package domain

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

func TestKindsAreClosedAndSorted(t *testing.T) {
	ks := Kinds()
	for i := 1; i < len(ks); i++ {
		if ks[i-1] >= ks[i] {
			t.Fatalf("Kinds() not sorted: %v", ks)
		}
	}
	for _, k := range ks {
		if !k.Valid() {
			t.Errorf("%q reported invalid but is listed", k)
		}
	}
	if Kind("k8s-fargate ").Valid() || Kind("").Valid() || Kind("quantum-annealer").Valid() {
		t.Error("unknown kinds must not validate")
	}
	// The returned slice is a copy: mutating it cannot corrupt the table.
	ks[0] = "mutated"
	if Kinds()[0] == "mutated" {
		t.Error("Kinds() handed out the backing array")
	}
}

func TestActionClassSemantics(t *testing.T) {
	for _, tc := range []struct {
		a          ActionClass
		valid      bool
		disruptive bool
	}{
		{ActionInPlace, true, false},
		{ActionRolling, true, true},
		{ActionStopStart, true, true},
		{ActionAdvisory, true, false},
		{ActionClass("resize"), false, false},
		{"", false, false},
	} {
		if got := tc.a.Valid(); got != tc.valid {
			t.Errorf("%q.Valid() = %v, want %v", tc.a, got, tc.valid)
		}
		if got := tc.a.Disruptive(); got != tc.disruptive {
			t.Errorf("%q.Disruptive() = %v, want %v", tc.a, got, tc.disruptive)
		}
	}
}

func TestSpecAttrsAreOrderIndependent(t *testing.T) {
	a := Spec{
		Resources: model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30},
		Attrs:     map[string]string{"z": "1", "a": "2", "m": "3"},
	}
	b := Spec{
		Resources: model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30},
		Attrs:     map[string]string{"m": "3", "a": "2", "z": "1"},
	}
	if !a.Equal(b) {
		t.Fatal("specs with the same attrs must compare equal")
	}
	// Canonical() and AttrKeys() must not vary with Go's randomized map order:
	// hammer them and require a single distinct result.
	want := a.Canonical()
	for i := 0; i < 200; i++ {
		if got := b.Canonical(); got != want {
			t.Fatalf("Canonical() varies with map order: %q vs %q", got, want)
		}
		if got := strings.Join(b.AttrKeys(), ","); got != "a,m,z" {
			t.Fatalf("AttrKeys() = %q, want sorted", got)
		}
	}
	if a.Attr("a") != "2" || a.Attr("missing") != "" {
		t.Error("Attr lookup wrong")
	}
	if (Spec{}).AttrKeys() != nil || (Spec{}).Canonical() == "" {
		t.Error("zero Spec should have no keys but still render")
	}
}

func TestSpecWithAttrCopiesTheMap(t *testing.T) {
	base := Spec{Attrs: map[string]string{"tier": "a"}}
	derived := base.WithAttr("tier", "b").WithAttr("extra", "x")
	if base.Attr("tier") != "a" || base.Attr("extra") != "" {
		t.Fatalf("WithAttr mutated its receiver: %v", base.Attrs)
	}
	if derived.Attr("tier") != "b" || derived.Attr("extra") != "x" {
		t.Fatalf("WithAttr lost data: %v", derived.Attrs)
	}
	// A Spec recorded in a Step must not change when its source does.
	base.Attrs["tier"] = "mutated"
	if derived.Attr("tier") != "b" {
		t.Error("derived Spec shares the source map")
	}
}

func TestTargetRefCompareIsTotal(t *testing.T) {
	refs := []TargetRef{
		{Domain: EC2, Scope: "acct", ID: "i-1"},
		{Domain: K8sFargate, Scope: "a", ID: "Deployment/default/api"},
		{Domain: K8sFargate, Scope: "a", ID: "Deployment/default/api", Name: "z"},
		{Domain: K8sFargate, Scope: "a", ID: "Deployment/other/api"},
		{Domain: K8sFargate, Scope: "b", ID: "Deployment/default/api"},
	}
	for i := range refs {
		if refs[i].Compare(refs[i]) != 0 {
			t.Errorf("%v is not equal to itself", refs[i])
		}
		for j := range refs {
			if i == j {
				continue
			}
			if c1, c2 := refs[i].Compare(refs[j]), refs[j].Compare(refs[i]); c1 != -c2 {
				t.Errorf("Compare not antisymmetric for %v/%v: %d vs %d", refs[i], refs[j], c1, c2)
			}
		}
	}
	// String excludes Name: two refs differing only by display name are one target.
	if refs[1].String() != refs[2].String() {
		t.Error("String() must not include Name")
	}
}

// TestSetSavingsEnforcesNetLessThanGross is the invariant this package exists
// to protect. Commitments can only ever make a change worth LESS than its list
// price suggests, so a net above gross is arithmetically impossible.
func TestSetSavingsEnforcesNetLessThanGross(t *testing.T) {
	for _, tc := range []struct {
		name               string
		gross, net         float64
		wantGross, wantNet float64
	}{
		{"net below gross is kept", 100, 40, 100, 40},
		{"net equal to gross is kept", 100, 100, 100, 100},
		{"net above gross is clamped", 100, 140, 100, 100},
		{"negative gross (a growth) keeps a lower net", -50, -80, -50, -80},
		{"negative gross clamps an optimistic net", -50, 10, -50, -50},
		{"NaN gross becomes zero", math.NaN(), 5, 0, 0},
		{"NaN net becomes zero", 10, math.NaN(), 10, 0},
		{"+Inf net is clamped to gross", 10, math.Inf(1), 10, 0},
		{"-Inf gross becomes zero and clamps net", math.Inf(-1), 5, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r Recommendation
			r.SetSavings(tc.gross, tc.net)
			if r.GrossSavingsMonthlyUSD != tc.wantGross || r.NetSavingsMonthlyUSD != tc.wantNet {
				t.Fatalf("SetSavings(%v,%v) = (%v,%v), want (%v,%v)",
					tc.gross, tc.net, r.GrossSavingsMonthlyUSD, r.NetSavingsMonthlyUSD, tc.wantGross, tc.wantNet)
			}
			if r.NetSavingsMonthlyUSD > r.GrossSavingsMonthlyUSD {
				t.Fatal("net exceeds gross")
			}
		})
	}
}

// FuzzSetSavings hammers the same invariant with arbitrary float64 pairs: no
// input, however hostile, may leave Net above Gross or either field non-finite.
func FuzzSetSavings(f *testing.F) {
	f.Add(0.0, 0.0)
	f.Add(1.0, 2.0)
	f.Add(-1e300, 1e300)
	f.Add(math.MaxFloat64, math.MaxFloat64)
	f.Fuzz(func(t *testing.T, gross, net float64) {
		var r Recommendation
		r.SetSavings(gross, net)
		if r.NetSavingsMonthlyUSD > r.GrossSavingsMonthlyUSD {
			t.Fatalf("net %v > gross %v (inputs %v, %v)",
				r.NetSavingsMonthlyUSD, r.GrossSavingsMonthlyUSD, gross, net)
		}
		for _, v := range []float64{r.GrossSavingsMonthlyUSD, r.NetSavingsMonthlyUSD} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("non-finite savings %v from (%v, %v)", v, gross, net)
			}
		}
		if r.ClaimableMonthlyUSD() < 0 {
			t.Fatalf("claimable is negative: %v", r.ClaimableMonthlyUSD())
		}
		if c := r.ClaimableMonthlyUSD(); c > 0 && c > r.GrossSavingsMonthlyUSD {
			t.Fatalf("claimable %v exceeds gross %v", c, r.GrossSavingsMonthlyUSD)
		}
	})
}

func TestClaimableRefusesSuppressedAndNegative(t *testing.T) {
	r := Recommendation{Suppressed: true, SuppressCode: SuppressCommitmentNegative}
	r.SetSavings(100, 90)
	if got := r.ClaimableMonthlyUSD(); got != 0 {
		t.Errorf("suppressed recommendation claims $%v", got)
	}
	r.Suppressed = false
	if got := r.ClaimableMonthlyUSD(); got != 90 {
		t.Errorf("claimable = %v, want 90", got)
	}
	r.SetSavings(-5, -5)
	if got := r.ClaimableMonthlyUSD(); got != 0 {
		t.Errorf("cost increase claims $%v", got)
	}
}

func validRec() Recommendation {
	r := Recommendation{
		Target:   TargetRef{Domain: K8sFargate, Scope: "c1", ID: "Deployment/default/api"},
		Current:  Spec{Attrs: map[string]string{"tier": "2vCPU 9GB"}},
		Proposed: Spec{Attrs: map[string]string{"tier": "1vCPU 8GB"}},
		Action:   ActionRolling,
		Evidence: []Evidence{{Metric: "tier", Value: "x", Source: SourceQuantizer}},
	}
	r.SetSavings(10, 10)
	return r
}

func TestRecommendationValidate(t *testing.T) {
	if err := validRec().Validate(); err != nil {
		t.Fatalf("valid recommendation rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Recommendation)
		want string
	}{
		{"no domain", func(r *Recommendation) { r.Target.Domain = "" }, "no target domain"},
		{"no id", func(r *Recommendation) { r.Target.ID = "" }, "no target ID"},
		{"bad action", func(r *Recommendation) { r.Action = "teleport" }, "invalid action"},
		{"no evidence", func(r *Recommendation) { r.Evidence = nil }, "no evidence"},
		{"confidence out of range", func(r *Recommendation) { r.Confidence = 1.5 }, "confidence"},
		{"NaN confidence", func(r *Recommendation) { r.Confidence = math.NaN() }, "confidence"},
		{"net above gross", func(r *Recommendation) { r.NetSavingsMonthlyUSD = 99 }, "net"},
		{"suppressed without code", func(r *Recommendation) { r.Suppressed = true }, "suppressed without a code"},
		{"no change", func(r *Recommendation) { r.Proposed = r.Current }, "proposes no change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := validRec()
			tc.mut(&r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestSortRecommendationsIsDeterministicUnderShuffle(t *testing.T) {
	base := []Recommendation{
		{Target: TargetRef{Domain: K8sFargate, Scope: "a", ID: "Deployment/ns/b"}, Action: ActionRolling},
		{Target: TargetRef{Domain: K8sFargate, Scope: "a", ID: "Deployment/ns/a"}, Action: ActionRolling},
		{Target: TargetRef{Domain: EC2, Scope: "acct", ID: "i-2"}, Action: ActionStopStart},
		{Target: TargetRef{Domain: EC2, Scope: "acct", ID: "i-1"}, Action: ActionStopStart},
		{Target: TargetRef{Domain: K8sFargate, Scope: "a", ID: "Deployment/ns/a"}, Action: ActionAdvisory,
			Proposed: Spec{Attrs: map[string]string{"tier": "z"}}},
	}
	want := append([]Recommendation(nil), base...)
	SortRecommendations(want)

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		got := append([]Recommendation(nil), base...)
		rng.Shuffle(len(got), func(a, b int) { got[a], got[b] = got[b], got[a] })
		SortRecommendations(got)
		for j := range got {
			if got[j].Target != want[j].Target || got[j].Action != want[j].Action {
				t.Fatalf("iteration %d: order differs at %d: %v vs %v", i, j, got[j].Target, want[j].Target)
			}
		}
	}
}

func TestStepKeyIsStableAndContentAddressed(t *testing.T) {
	ref := TargetRef{Domain: K8sFargate, Scope: "c1", ID: "Deployment/default/api", Name: "default/api"}
	from := Spec{Resources: model.Resources{MilliCPU: 2000}, Attrs: map[string]string{"b": "2", "a": "1"}}
	fromReordered := Spec{Resources: model.Resources{MilliCPU: 2000}, Attrs: map[string]string{"a": "1", "b": "2"}}
	to := Spec{Resources: model.Resources{MilliCPU: 1000}}

	k := StepKey(ref, from, to)
	if len(k) != 16 {
		t.Fatalf("key %q has length %d", k, len(k))
	}
	for i := 0; i < 100; i++ {
		if got := StepKey(ref, fromReordered, to); got != k {
			t.Fatalf("step key depends on attribute map order: %q vs %q", got, k)
		}
	}
	// The display name is not identity, but every other component is.
	named := ref
	named.Name = "renamed"
	if StepKey(named, from, to) != k {
		t.Error("step key changed with the display name")
	}
	if StepKey(ref, from, from) == k {
		t.Error("step key ignored the target spec")
	}
	if StepKey(TargetRef{Domain: EC2, Scope: "c1", ID: "Deployment/default/api"}, from, to) == k {
		t.Error("step key ignored the domain")
	}
}

func TestFingerprintCoversOrder(t *testing.T) {
	s1 := Step{Seq: 1, Key: "a", Action: ActionRolling, Risk: "low"}
	s2 := Step{Seq: 2, Key: "b", Action: ActionRolling, Risk: "low"}
	f := Fingerprint([]Step{s1, s2})
	if f == Fingerprint([]Step{s2, s1}) {
		t.Error("fingerprint ignores step order")
	}
	if f != Fingerprint([]Step{s1, s2}) {
		t.Error("fingerprint is not deterministic")
	}
	if Fingerprint(nil) == "" {
		t.Error("empty plan should still fingerprint")
	}
}

func TestRecommendationJSONRoundTrip(t *testing.T) {
	r := validRec()
	r.ValidFrom = time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	r.Confidence = 0.5
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back Recommendation
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Target != r.Target || back.NetSavingsMonthlyUSD != r.NetSavingsMonthlyUSD ||
		!back.ValidFrom.Equal(r.ValidFrom) || back.Action != r.Action {
		t.Fatalf("round trip lost data:\n%+v\n%+v", r, back)
	}
}
