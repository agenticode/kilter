package rds

import (
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/domain"
)

// Report.Validate is the package's own contract with itself. A test that only
// asserts "the happy path validates" proves nothing about it, so this one
// walks every clause and asserts each rejects the thing it exists to reject.
//
// If a clause is added to Validate without a case here, this test's final
// count check fails, which is the point: the gate and its coverage move
// together.
func TestValidateCatchesEachViolation(t *testing.T) {
	// A known-good report, produced by the real path.
	good := func(t *testing.T) *Report {
		t.Helper()
		f := &Fixture{
			Instances: []DBInstanceRecord{
				rec("aa", "db.r6i.xlarge", "postgres", withStorage(500, 0, StorageGP2)),
				rec("bb", "db.r6i.large", "aurora-mysql", withCluster("c1")),
			},
			Clusters: []DBClusterRecord{{DBClusterIdentifier: "c1", Engine: "aurora-mysql"}},
			Metrics: mergeMetrics(
				metricsFor("aa", 30, 12, 24<<30, 100*GiB),
				metricsFor("bb", 30, 12, 8<<30, 40*GiB),
			),
		}
		return assess(t, collect(t, f), nil)
	}
	if err := good(t).Validate(); err != nil {
		t.Fatalf("the baseline report is already invalid: %v", err)
	}

	// idx of the modelled (non-excluded) assessment, resolved rather than
	// assumed so a fixture reorder cannot silently point these at the wrong one.
	modelled := func(r *Report) int {
		for i, a := range r.Assessments {
			if !a.Excluded() {
				return i
			}
		}
		t.Fatal("the baseline report has no modelled assessment")
		return -1
	}
	excluded := func(r *Report) int {
		for i, a := range r.Assessments {
			if a.Excluded() {
				return i
			}
		}
		t.Fatal("the baseline report has no excluded assessment")
		return -1
	}

	cases := []struct {
		name    string
		break_  func(r *Report)
		wantErr string
	}{
		{"wrong domain", func(r *Report) { r.Domain = "ec2" }, "report domain"},
		{"assessments out of order", func(r *Report) {
			r.Assessments[0], r.Assessments[1] = r.Assessments[1], r.Assessments[0]
		}, "not sorted"},
		{"no evidence", func(r *Report) {
			r.Assessments[modelled(r)].Evidence = nil
		}, "no evidence"},
		{"no reason at all", func(r *Report) {
			r.Assessments[modelled(r)].Suppressions = nil
		}, "silence is not an output"},
		{"suppression with no code", func(r *Report) {
			i := modelled(r)
			r.Assessments[i].Suppressions[0].Code = ""
		}, "no code"},
		{"suppression with no reason", func(r *Report) {
			i := modelled(r)
			r.Assessments[i].Suppressions[0].Reason = ""
		}, "no reason"},
		{"exclusion does not fire alone", func(r *Report) {
			i := excluded(r)
			r.Assessments[i].Suppressions = append(r.Assessments[i].Suppressions,
				Suppression{Code: ReasonStorageCannotShrink, Reason: "storage cannot shrink"})
		}, "fires\nalone"},
		{"excluded instance carries an advisory", func(r *Report) {
			i := excluded(r)
			r.Assessments[i].Advisories = []Advisory{{Code: AdvisoryIdleInstance, Message: "m", Caveat: "c"}}
		}, "excluded but carries"},
		{"excluded instance carries a proposal", func(r *Report) {
			i := excluded(r)
			r.Assessments[i].Proposal = validProposal()
		}, "excluded"},
		{"advisory without a caveat", func(r *Report) {
			i := modelled(r)
			r.Assessments[i].Advisories = []Advisory{{Code: AdvisoryIdleInstance, Message: "m"}}
		}, "no caveat"},
		{"advisory with a negative magnitude", func(r *Report) {
			i := modelled(r)
			r.Assessments[i].Advisories = []Advisory{
				{Code: AdvisoryIdleInstance, Message: "m", Caveat: "c", MonthlyUSD: -5}}
		}, "negative magnitude"},
		{"proposal shrinks allocated storage", func(r *Report) {
			i := modelled(r)
			p := validProposal()
			p.AllocatedStorageGiB = r.Assessments[i].Instance.AllocatedStorageGiB - 1
			r.Assessments[i].Proposal = p
		}, "can only ever grow"},
		{"proposal is not advisory", func(r *Report) {
			i := modelled(r)
			p := validProposal()
			p.Action = domain.ActionInPlace
			r.Assessments[i].Proposal = p
		}, "advisory only"},
		{"proposal claims from an unverified rate", func(r *Report) {
			i := modelled(r)
			p := validProposal()
			p.RateProvenance = RateUnverified
			r.Assessments[i].Proposal = p
		}, "unverified"},
		{"proposal claims net above gross", func(r *Report) {
			i := modelled(r)
			p := validProposal()
			p.NetSavingsMonthlyUSD = p.GrossSavingsMonthlyUSD + 1
			r.Assessments[i].Proposal = p
		}, "net can never exceed"},
		{"proposal claims nothing", func(r *Report) {
			i := modelled(r)
			p := validProposal()
			p.NetSavingsMonthlyUSD = 0
			r.Assessments[i].Proposal = p
		}, "must be net-positive"},
		{"proposal has no confidence", func(r *Report) {
			i := modelled(r)
			p := validProposal()
			p.Confidence = 0
			r.Assessments[i].Proposal = p
		}, "confidence"},
		{"proposal states no reason", func(r *Report) {
			i := modelled(r)
			p := validProposal()
			p.Reason = ""
			r.Assessments[i].Proposal = p
		}, "states no reason"},
		{"totals do not match", func(r *Report) { r.Totals.Instances = 99 }, "totals do not match"},
		{"totals money does not match", func(r *Report) { r.Totals.CurrentMonthlyUSD += 1 },
			"totals do not match"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := good(t)
			// Deep-copy the slices the mutation touches, so cases cannot leak
			// into one another through shared backing arrays.
			r.Assessments = deepCopyAssessments(r.Assessments)
			tc.break_(r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a report with %s", tc.name)
			}
			for _, want := range strings.Split(tc.wantErr, "\n") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate rejected %q for the wrong reason:\n got: %v\nwant substring: %q",
						tc.name, err, want)
				}
			}
		})
	}

	// Coverage guard. Every `return fmt.Errorf` in Validate/validateProposal is
	// a rejection clause, and each needs a case above — minus the clauses that
	// are structurally unreachable today and are named, with their reason, in
	// unreachableClauses. A new clause with no case trips this.
	clauses := countValidateClauses(t)
	if got := len(cases) + len(unreachableClauses); got < clauses {
		t.Errorf("Validate has %d rejection clauses; this test exercises %d and documents %d as "+
			"unreachable. A clause with neither a case nor an entry in unreachableClauses is a gate "+
			"nobody has checked", clauses, len(cases), len(unreachableClauses))
	}
	for _, why := range unreachableClauses {
		if why == "" {
			t.Error("an unreachable clause is listed without a reason")
		}
	}
}

// unreachableClauses are Validate clauses that cannot fire today. They are
// kept as backstops for the units that will relax the clause above them, and
// each is listed with the reason it cannot be reached — so the list is a
// reviewable decision rather than a fudge factor on the count.
var unreachableClauses = map[string]string{
	"advisory claims to be actuatable": "Advisory.Actuatable() is a method returning a constant false, " +
		"so no serialized form and no struct literal can make it true. The clause guards against a " +
		"future FIELD replacing the method.",
	"proposal claims a disruptive action class": "the clause above it already rejects every action " +
		"class except domain.ActionAdvisory, which is non-disruptive by definition. It is the backstop " +
		"for U13/U14, which relax that clause to allow ActionInPlace on storage performance.",
}

func validProposal() *Proposal {
	return &Proposal{
		StorageType: StorageGP3, IOPS: 12000, StorageThroughputMBps: 500,
		Action: domain.ActionAdvisory, Risk: RiskLow, Confidence: 0.8,
		Reason:                 "measured parity at a lower rate",
		GrossSavingsMonthlyUSD: 20, NetSavingsMonthlyUSD: 15, RateProvenance: RateOperator,
	}
}

func deepCopyAssessments(in []Assessment) []Assessment {
	out := append([]Assessment(nil), in...)
	for i := range out {
		out[i].Suppressions = append([]Suppression(nil), out[i].Suppressions...)
		out[i].Advisories = append([]Advisory(nil), out[i].Advisories...)
		out[i].Evidence = append([]domain.Evidence(nil), out[i].Evidence...)
	}
	return out
}

// countValidateClauses counts the distinct error returns in report.go's
// validation path, so the coverage guard above measures the real gate rather
// than a number someone typed.
func countValidateClauses(t *testing.T) int {
	t.Helper()
	_, src := packageFiles(t)
	body, ok := src["report.go"]
	if !ok {
		t.Fatal("report.go not found")
	}
	start := strings.Index(body, "func (r *Report) Validate()")
	if start < 0 {
		t.Fatal("Validate not found in report.go")
	}
	end := strings.Index(body, "\nfunc nearly(")
	if end < 0 {
		end = len(body)
	}
	return strings.Count(body[start:end], "return fmt.Errorf(")
}

// A proposal that is well-formed passes, so the gate is not simply "reject
// everything" — which would be a gate that never has to be right.
func TestValidateAcceptsAWellFormedProposal(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("ok", "db.r6i.xlarge", "postgres", withStorage(500, 0, StorageGP3))},
		Metrics:   metricsFor("ok", 30, 12, 24<<30, 100*GiB),
	}
	rep := assess(t, collect(t, f), nil)
	rep.Assessments = deepCopyAssessments(rep.Assessments)
	i := 0
	p := validProposal()
	// Growing the allocation is allowed; shrinking is not.
	p.AllocatedStorageGiB = rep.Assessments[i].Instance.AllocatedStorageGiB
	rep.Assessments[i].Proposal = p
	rep.Totals = rep.computeTotals()
	if err := rep.Validate(); err != nil {
		t.Fatalf("Validate rejected a well-formed storage-performance proposal: %v", err)
	}
	p.AllocatedStorageGiB = rep.Assessments[i].Instance.AllocatedStorageGiB + 100
	if err := rep.Validate(); err != nil {
		t.Fatalf("Validate rejected a proposal that GROWS storage; only shrinking is impossible: %v", err)
	}
}
