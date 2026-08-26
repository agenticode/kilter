package recommend

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/model"
)

// This file implements the `Recommender.Verdicts(snap)` seam that
// pkg/backtest asked for by name (pkg/backtest/backtest.go, FINDINGS.md §2)
// and that cmd/WIRING-FINDINGS.md §6.4 blocked `kilter explain` on.
//
// It deliberately does NOT return []decision.Verdict, and the reason is the
// whole point of the seam. `pkg/recommend` does not compute a
// decision-quality verdict today: not one of pkg/decision's eight refusal
// predicates is evaluated anywhere on the production recommendation path,
// and no Action is chosen (act vs recommend-only is pkg/plan's threshold,
// applied later and elsewhere). Calling decision.Evaluate from here, with
// evidence assembled on the spot, would produce a second, parallel
// evaluation whose answer can differ from the one production actually
// served — an explain payload citing a verdict nobody acted on. §6.4 refuses
// that trade explicitly and so does this file.
//
// What production DOES reach, for every container it considered, is a
// Disposition: it recommended, or it stayed silent for one of three
// specific reasons. Those dispositions are real, they are currently
// invisible outside the package, and they are what this seam exposes. The
// decision-quality verdict is exposed as a typed absence — VerdictNotComputed
// — which a caller cannot accidentally read as "computed, and the answer is
// no". See VERDICT-FINDINGS.md.

// Disposition is what the production recommendation path actually did with
// one container it considered on a given snapshot. It is a report of a
// branch taken, not a judgement: no Disposition is a decision.Refusal, and
// none of them carries a refusal Code, Detail or Until.
type Disposition string

const (
	// DispositionRecommended: a Recommendation was produced and Rec holds
	// it. Exactly the containers Recommendations reports.
	DispositionRecommended Disposition = "recommended"
	// DispositionNeverObserved: the container is eligible in this snapshot
	// but the recommender holds no record of it at all — no observed
	// snapshot has ever contained it. ObserveSnapshot registers state for
	// every container of every pod it sees, with or without usage, so this
	// means the snapshot handed to Verdicts was not itself observed. A pod
	// that was observed but has learned nothing reports
	// DispositionInsufficientHistory with Samples 0 instead; that is the
	// collector gap, and the two are not the same fact.
	DispositionNeverObserved Disposition = "never-observed"
	// DispositionInsufficientHistory: the container is known but its
	// learned history falls short of Config.MinSamples or Config.MinWindow
	// (Samples 0 included), so the recommender stayed silent. This gate resembles decision.CodeInsufficientHistory and at
	// default config uses the same numbers, but it is not that refusal:
	// the two thresholds live in two independently settable Configs, and
	// this path produces no Refusal value at all.
	DispositionInsufficientHistory Disposition = "insufficient-history"
	// DispositionNoSignificantChange: history was sufficient and sizing ran,
	// but both dimensions landed within Config.MinChangeRatio of the current
	// request, so the recommendation was suppressed as churn.
	//
	// Invariant this label depends on: recommendOne returns nil exactly at
	// the churn-suppression check and nowhere else. A future early return
	// added to recommendOne must add a Disposition here with it.
	DispositionNoSignificantChange Disposition = "no-significant-change"
)

// VerdictState says whether a decision-quality verdict (pkg/decision) exists
// for a container on the production path. It is a separate axis from
// Disposition on purpose: "we never evaluated the refusal predicates" and
// "we evaluated them and refused" are different facts, and a payload that
// cannot tell them apart is a payload that will eventually claim the second
// while meaning the first.
type VerdictState string

const (
	// VerdictNotComputed: no decision.Verdict exists. This is what every
	// Verdict reports today, because pkg/recommend evaluates no refusal
	// predicate. It is NOT decision.ActionRefuse and must never be
	// rendered as one.
	VerdictNotComputed VerdictState = "not-computed"
	// VerdictComputed: a decision.Verdict was reached on the production
	// path and Decision returns it. Nothing produces this state yet; it is
	// the shape the seam commits to, so that when the evidence inputs
	// arrive the assignment happens here and in no other call site.
	VerdictComputed VerdictState = "computed"
)

// Verdict is one considered container's readout of the production path:
// which container, what the recommender did, the history it did it on, and
// whether a decision-quality verdict exists for it.
//
// The decision.Verdict is deliberately not an exported field. It is reachable
// only through Decision, whose comma-ok result a caller has to look at — so
// "absent" cannot be silently read as a zero-valued verdict whose Action
// happens to compare unequal to everything.
type Verdict struct {
	Key         model.ContainerKey `json:"key"`
	Disposition Disposition        `json:"disposition"`

	// CurrentRequest and CurrentLimit are the container's sizing as the
	// snapshot reported it, for every disposition — including the ones with
	// no Rec to read them off.
	CurrentRequest model.Resources `json:"currentRequest"`
	CurrentLimit   model.Resources `json:"currentLimit"`

	// Samples, Window, FirstSample and LastSample are the learned history
	// the disposition was reached on. Zero for DispositionNeverObserved.
	// These are the counters pkg/backtest's learnState mirrors today.
	Samples     int           `json:"samples"`
	Window      time.Duration `json:"window"`
	FirstSample time.Time     `json:"firstSample,omitzero"`
	LastSample  time.Time     `json:"lastSample,omitzero"`

	// Rec is non-nil if and only if Disposition is DispositionRecommended,
	// and is byte-for-byte the Recommendation Recommendations reports for
	// this key on this snapshot.
	Rec *Recommendation `json:"recommendation,omitempty"`

	// state and dec are unexported so that the only way to a decision
	// verdict is Decision's comma-ok. See the type comment.
	state VerdictState
	dec   *decision.Verdict
}

// State reports whether a decision-quality verdict exists for this container.
func (v Verdict) State() VerdictState {
	if v.state == "" {
		return VerdictNotComputed
	}
	return v.state
}

// Decision returns the decision-quality verdict the production path reached
// for this container, and whether one exists at all.
//
// ok is false whenever State is VerdictNotComputed, which is every verdict
// this package produces today. When ok is false the returned Verdict is the
// zero value, whose Action is the empty string — equal to none of
// decision.ActionAct, decision.ActionRecommendOnly or decision.ActionRefuse,
// so a caller that ignores ok still cannot land on a disposition by
// accident. Callers rendering a payload should treat !ok as "unknown"
// (explain.ActionUnknown), never as refusal.
func (v Verdict) Decision() (decision.Verdict, bool) {
	if v.State() != VerdictComputed || v.dec == nil {
		return decision.Verdict{}, false
	}
	return *v.dec, true
}

// verdictJSON is the wire shape. verdictState is always present and the
// verdict object is absent unless one was computed, so a JSON consumer
// reading `.verdict.action` gets nothing rather than a falsy disposition.
type verdictJSON struct {
	Key            model.ContainerKey `json:"key"`
	Disposition    Disposition        `json:"disposition"`
	CurrentRequest model.Resources    `json:"currentRequest"`
	CurrentLimit   model.Resources    `json:"currentLimit"`
	Samples        int                `json:"samples"`
	Window         time.Duration      `json:"window"`
	FirstSample    time.Time          `json:"firstSample,omitzero"`
	LastSample     time.Time          `json:"lastSample,omitzero"`
	Rec            *Recommendation    `json:"recommendation,omitempty"`
	VerdictState   VerdictState       `json:"verdictState"`
	Decision       *decision.Verdict  `json:"verdict,omitempty"`
}

// MarshalJSON emits the verdict state explicitly and omits the decision
// verdict unless one exists.
func (v Verdict) MarshalJSON() ([]byte, error) {
	out := verdictJSON{
		Key: v.Key, Disposition: v.Disposition,
		CurrentRequest: v.CurrentRequest, CurrentLimit: v.CurrentLimit,
		Samples: v.Samples, Window: v.Window,
		FirstSample: v.FirstSample, LastSample: v.LastSample,
		Rec: v.Rec, VerdictState: v.State(),
	}
	if dec, ok := v.Decision(); ok {
		out.Decision = &dec
	}
	return json.Marshal(out)
}

// UnmarshalJSON restores a verdict, keeping the two states distinct across
// the wire: a payload carrying no verdict object decodes to
// VerdictNotComputed regardless of what its verdictState field claimed, so a
// hand-edited or truncated document cannot manufacture a disposition.
func (v *Verdict) UnmarshalJSON(b []byte) error {
	var in verdictJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	*v = Verdict{
		Key: in.Key, Disposition: in.Disposition,
		CurrentRequest: in.CurrentRequest, CurrentLimit: in.CurrentLimit,
		Samples: in.Samples, Window: in.Window,
		FirstSample: in.FirstSample, LastSample: in.LastSample,
		Rec: in.Rec, state: VerdictNotComputed,
	}
	if in.Decision != nil {
		dec := *in.Decision
		v.state, v.dec = VerdictComputed, &dec
	}
	return nil
}

// Verdicts reports what the production recommendation path did with every
// container it considered on snap — the seam pkg/backtest asked for.
//
// "Considered" is exactly Recommendations' eligibility filter: containers of
// Running pods, excluding bare pods and Job/CronJob, deduplicated by
// container key. Ineligible containers are absent from the result, because
// the recommender never looked at them. This is the filter pkg/backtest
// reimplements in eligibleContainers.
//
// The result is sorted by Key.String() and is a pure function of snap and
// the recommender's learned state — no clock, no map-iteration order. A nil
// snapshot returns nil.
//
// Agreement with Recommendations is the contract: for every returned
// Verdict, Disposition == DispositionRecommended if and only if
// Recommendations reports that key on the same snapshot, and Rec then equals
// that Recommendation exactly. Both derive their answer from the same
// recommendOne call on the same locked state, and
// TestVerdictsAndRecommendationsCannotDisagree pins them together.
func (r *Recommender) Verdicts(snap *model.ClusterSnapshot) []Verdict {
	if snap == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	hpaCPU := hpaCPUWorkloads(snap)

	// Eligibility, byte-for-byte Recommendations'. Later replicas overwrite
	// earlier ones for the same key, as there — mid-rollout divergence is a
	// known wart (FINDINGS.md), and reproducing it is the point: this seam
	// reports what production does, not what it should do.
	type current struct{ req, lim model.Resources }
	currents := map[model.ContainerKey]current{}
	for i := range snap.Pods {
		pod := &snap.Pods[i]
		if pod.Phase != "" && pod.Phase != "Running" {
			continue
		}
		switch pod.Workload.Kind {
		case model.KindBarePod, model.KindJob, model.KindCronJob:
			continue
		}
		for _, c := range pod.Containers {
			key := model.ContainerKey{Workload: pod.Workload, Container: c.Name}
			currents[key] = current{req: c.Requests, lim: c.Limits}
		}
	}

	keys := make([]model.ContainerKey, 0, len(currents))
	for key := range currents {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	out := make([]Verdict, 0, len(keys))
	for _, key := range keys {
		cur := currents[key]
		v := Verdict{
			Key:            key,
			CurrentRequest: cur.req,
			CurrentLimit:   cur.lim,
			state:          VerdictNotComputed,
		}

		st := r.states[key]
		if st == nil {
			v.Disposition = DispositionNeverObserved
			out = append(out, v)
			continue
		}
		window := st.lastSample.Sub(st.firstSample)
		v.Samples, v.Window = st.samples, window
		v.FirstSample, v.LastSample = st.firstSample, st.lastSample

		if st.samples < r.cfg.MinSamples || window < r.cfg.MinWindow {
			v.Disposition = DispositionInsufficientHistory
			out = append(out, v)
			continue
		}

		hpaOwner, hpaOnCPU := hpaCPU[key.Workload]
		if rec := r.recommendOne(key, st, cur.req, cur.lim, hpaOnCPU, hpaOwner, window); rec != nil {
			v.Disposition, v.Rec = DispositionRecommended, rec
		} else {
			v.Disposition = DispositionNoSignificantChange
		}
		out = append(out, v)
	}
	return out
}
