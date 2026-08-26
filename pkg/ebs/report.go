package ebs

// Assessment and Report: one verdict per volume, always with its reason.
//
// domain.Recommendation cannot express a refusal — it requires a proposal, and
// a proposal must differ from the current spec — so the refusals live here,
// beside the proposals, in the shape pkg/ec2 already established for a cloud
// domain's report. [Domain.Recommend] is this report projected onto the
// volumes that have a proposal, which is what makes "silence is never an
// output" true and testable.

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Observation is what the metrics said about one volume.
type Observation struct {
	// IOPSPercentile and ThroughputPercentileMBps are the raw measured
	// percentiles, before headroom.
	IOPSPercentile           float64 `json:"iopsPercentile"`
	ThroughputPercentileMBps float64 `json:"throughputPercentileMBps"`
	// Demand is those percentiles with headroom applied — what a
	// configuration must clear.
	Demand Demand `json:"demand"`
	// Samples is the smaller of the two series' point counts.
	Samples int `json:"samples"`
	// Window is the interval the samples span.
	Window time.Duration `json:"window"`
	// Coverage is the fraction of expected datapoints actually present.
	Coverage float64 `json:"coverage"`
	// Floor is the parity floor the window and coverage earned.
	Floor Floor `json:"floor"`
	// Confidence is 0..1 with the same semantics as pkg/recommend.
	Confidence float64 `json:"confidence"`
	// BurstBalanceMinPct is the lowest observed gp2 burst balance; ok is false
	// when the series was not collected.
	BurstBalanceMinPct float64 `json:"burstBalanceMinPct,omitempty"`
	HasBurstBalance    bool    `json:"hasBurstBalance,omitempty"`
}

// Assessment is one volume's verdict: a proposal, or the reason there is none.
// Exactly one of Recommendation and Refusal is set.
type Assessment struct {
	Ref        domain.TargetRef `json:"ref"`
	Current    domain.Spec      `json:"current"`
	VolumeType string           `json:"volumeType"`
	SizeGiB    int64            `json:"sizeGiB"`
	Mode       string           `json:"mode"`

	Observed Observation `json:"observed"`
	// Parity is the parity math's output when it ran; nil when the volume was
	// refused before it could.
	Parity *ParityPlan `json:"parity,omitempty"`

	Recommendation *domain.Recommendation `json:"recommendation,omitempty"`
	Refusal        *Refusal               `json:"refusal,omitempty"`
	Evidence       []domain.Evidence      `json:"evidence,omitempty"`
}

// Report is the whole domain's verdict at one instant.
type Report struct {
	Domain      domain.Kind  `json:"domain"`
	Scope       string       `json:"scope"`
	At          time.Time    `json:"at"`
	Assessments []Assessment `json:"assessments,omitempty"`

	// Proposed counts assessments carrying an applicable proposal.
	Proposed int `json:"proposed"`
	// Suppressed counts proposals that exist but must not be applied.
	Suppressed int `json:"suppressed"`
	// Refused counts volumes with no proposal at all.
	Refused int `json:"refused"`
	// ClaimableMonthlyUSD is the sum of what may honestly be claimed: net,
	// post-commitment, applicable proposals only.
	ClaimableMonthlyUSD float64 `json:"claimableMonthlyUSD"`
	// GrossMonthlyUSD is the list-price sum over the same set — carried only
	// so a UI can show the fantasy beside the fact.
	GrossMonthlyUSD float64 `json:"grossMonthlyUSD"`
}

// Validate reports the first structural problem with a report. It is the
// contract the domain's own tests assert on it: every assessment resolves to
// exactly one of a proposal or a refusal, every proposal is a valid
// recommendation, and no claim exceeds its net.
func (r Report) Validate() error {
	for _, a := range r.Assessments {
		switch {
		case a.Ref.ID == "":
			return fmt.Errorf("ebs: assessment with no volume ID")
		case a.Recommendation == nil && a.Refusal == nil:
			return fmt.Errorf("ebs: assessment for %s is silent: no proposal and no reason", a.Ref.ID)
		case a.Recommendation != nil && a.Refusal != nil:
			return fmt.Errorf("ebs: assessment for %s carries both a proposal and a refusal", a.Ref.ID)
		case a.Refusal != nil && a.Refusal.Code == "":
			return fmt.Errorf("ebs: assessment for %s refuses without a code", a.Ref.ID)
		}
		if a.Recommendation != nil {
			if err := a.Recommendation.Validate(); err != nil {
				return fmt.Errorf("ebs: %w", err)
			}
		}
	}
	return nil
}

// Assess produces one verdict per known volume, in volume-ID order.
//
// The order of the checks is the order of the reasons: ownership first
// (a mode=off volume is not ours to think about), then applicability, then
// state, then evidence, then arithmetic, then economics. A volume refused for
// an earlier reason never reaches a later one, so the reported code is the
// most fundamental one and does not flap with the numbers.
func (d *Domain) Assess(now time.Time, ledger domain.Netter) Report {
	d.mu.Lock()
	defer d.mu.Unlock()

	rep := Report{Domain: Kind, Scope: d.scope, At: now}
	ids := make([]string, 0, len(d.volumes))
	for id := range d.volumes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		a := d.assess(d.volumes[id], now, ledger)
		rep.Assessments = append(rep.Assessments, a)
		switch {
		case a.Recommendation == nil:
			rep.Refused++
		case a.Recommendation.Suppressed:
			rep.Suppressed++
		default:
			rep.Proposed++
			rep.ClaimableMonthlyUSD += a.Recommendation.ClaimableMonthlyUSD()
			rep.GrossMonthlyUSD += a.Recommendation.GrossSavingsMonthlyUSD
		}
	}
	return rep
}

// refuse fills an assessment's refusal and returns it.
func refuse(a Assessment, code, format string, args ...any) Assessment {
	a.Refusal = &Refusal{Code: code, Reason: fmt.Sprintf(format, args...)}
	a.Recommendation = nil
	return a
}

// assess is one volume's verdict. Caller holds d.mu.
func (d *Domain) assess(v *volume, now time.Time, ledger domain.Netter) Assessment {
	a := Assessment{
		Ref:        v.Ref,
		Current:    v.Spec,
		VolumeType: v.Spec.Attr(AttrVolumeType),
		SizeGiB:    sizeOf(v.Spec),
		Mode:       d.modeOf(v.Labels),
	}

	// (1) Ownership. A volume tagged kilter.dev/mode=off is not ours.
	if a.Mode == guard.ModeOff {
		return refuse(a, ReasonModeOff, "%s=%s: this volume is opted out", TagKilterMode, guard.ModeOff)
	}

	// (2) Applicability. gp2 is the only type this unit converts.
	if a.VolumeType != VolumeTypeGP2 {
		switch a.VolumeType {
		case VolumeTypeGP3:
			return refuse(a, ReasonNotGP2,
				"already gp3: re-tuning provisioned IOPS/throughput on a gp3 volume is a later unit")
		case VolumeTypeIO1, VolumeTypeIO2:
			return refuse(a, ReasonNotGP2,
				"%s volume: the io1/io2 → gp3 move is advisory-only and deliberately deferred (§4.7)", a.VolumeType)
		case "":
			return refuse(a, ReasonNotGP2, "volume type is unknown: the inventory record is incomplete")
		default:
			return refuse(a, ReasonNotGP2, "%s volumes have no gp3 equivalent this unit can price", a.VolumeType)
		}
	}
	if a.SizeGiB <= 0 {
		return refuse(a, ReasonInvalidSize, "volume size is missing or unreadable in the inventory record")
	}

	// (3) State. These are AWS's rules, checked here so a plan is never built
	// on a step the actuator would have to refuse.
	switch st := v.Spec.Attr(AttrState); st {
	case VolumeStateAvailable, VolumeStateInUse:
	case "":
		return refuse(a, ReasonVolumeState, "volume state is unknown: the inventory record is incomplete")
	default:
		return refuse(a, ReasonVolumeState, "volume is %s: ModifyVolume needs %s or %s",
			st, VolumeStateAvailable, VolumeStateInUse)
	}
	if v.Spec.Attr(AttrMultiAttach) == "true" {
		return refuse(a, ReasonMultiAttach,
			"multi-attach is enabled: gp3 does not support it, so the conversion would be rejected")
	}
	for _, st := range splitList(v.Spec.Attr(AttrAttachmentState)) {
		if st == AttachmentAttaching || st == AttachmentDetaching {
			return refuse(a, ReasonAttachmentTransition,
				"an attachment is %s: the volume is mid-transition and its state is not settled", st)
		}
	}
	if st := v.Spec.Attr(AttrModificationState); st == ModificationModifying || st == ModificationOptimizing {
		return refuse(a, ReasonModificationInProgress,
			"a modification is already %s: AWS accepts one at a time", st)
	}
	if last, ok := d.lastModification(v); ok {
		if wait := d.cfg.Cooldown - now.Sub(last); wait > 0 {
			return refuse(a, ReasonCooldown,
				"last modified %s ago; the %s cooldown has %s left",
				now.Sub(last).Round(time.Minute), d.cfg.Cooldown, wait.Round(time.Minute))
		}
	}

	// (4) Evidence. No IOPS series ⇒ no parity claim, ever.
	obs, ref := d.observe(v)
	a.Observed = obs
	if ref != nil {
		a.Refusal = ref
		return a
	}

	// (5) Arithmetic.
	p, pref := d.cfg.Rates.PlanGP3(a.SizeGiB, obs.Demand, obs.Floor)
	a.Parity = &p
	a.Evidence = d.evidence(v, obs, p, now)
	if pref != nil {
		// A short window is the more actionable explanation: with a full week
		// of observation the same volume might convert profitably below gp2's
		// nameplate, so the operator is told to wait rather than told "no".
		if pref.Code == ReasonNoCheaperConfig && obs.Floor == FloorGP2Baseline {
			return refuse(a, ReasonInsufficientWindow,
				"%s; the observation window is %s of the %s required before provisioning below gp2's delivered baseline",
				pref.Reason, obs.Window.Round(time.Hour), d.cfg.MinWindow)
		}
		a.Refusal = pref
		return a
	}

	// (6) Economics.
	if p.DeltaMonthlyUSD < d.cfg.MinMonthlySavingsUSD {
		return refuse(a, ReasonBelowMinSavings,
			"gp3 at parity saves $%.2f/mo, below the $%.2f/mo floor: not worth a modification and its cooldown",
			p.DeltaMonthlyUSD, d.cfg.MinMonthlySavingsUSD)
	}

	rec := d.recommendation(v, a, p, obs, ledger)
	a.Recommendation = &rec
	return a
}

// lastModification is the newest of what the collector observed and what this
// process executed. Caller holds d.mu.
func (d *Domain) lastModification(v *volume) (time.Time, bool) {
	var out time.Time
	if s := v.Spec.Attr(AttrModificationAt); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			out = t
		}
	}
	if t, ok := d.applied[v.Ref.ID]; ok && t.After(out) {
		out = t
	}
	return out, !out.IsZero()
}

// observe turns the learned series into demand, or refuses. Caller holds d.mu.
func (d *Domain) observe(v *volume) (Observation, *Refusal) {
	var obs Observation
	for _, b := range v.Blind {
		if b == SampleIOPS {
			return obs, &Refusal{ReasonUnmeasured,
				"no IOPS series: the collector declared this volume blind, and an unmeasured volume gets no parity claim"}
		}
	}
	iops, okI := v.IOPS.percentile(d.cfg.IOPSPercentile)
	tput, okT := v.Tput.percentile(d.cfg.ThroughputPercentile)
	if !okI {
		return obs, &Refusal{ReasonUnmeasured,
			"no IOPS datapoints: an unmeasured volume gets no parity claim"}
	}
	if !okT {
		return obs, &Refusal{ReasonUnmeasured,
			"no throughput datapoints: gp3 provisions throughput separately, so IOPS alone cannot establish parity"}
	}
	obs.IOPSPercentile, obs.ThroughputPercentileMBps = iops, tput
	obs.Demand = Demand{
		IOPS:           iops * d.cfg.IOPSHeadroom,
		ThroughputMBps: tput * d.cfg.ThroughputHeadroom,
	}
	obs.Samples = len(v.IOPS.Points)
	if n := len(v.Tput.Points); n < obs.Samples {
		obs.Samples = n
	}
	obs.Window = v.IOPS.span()
	if w := v.Tput.span(); w < obs.Window {
		obs.Window = w
	}
	if obs.Window > 0 {
		expected := obs.Window.Seconds() / float64(PeriodSeconds)
		if expected > 0 {
			obs.Coverage = math.Min(1, float64(obs.Samples)/expected)
		}
	}
	obs.BurstBalanceMinPct, obs.HasBurstBalance = v.Burst.min()

	if obs.Samples < d.cfg.MinSamples {
		return obs, &Refusal{ReasonInsufficientSamples, fmt.Sprintf(
			"%d datapoint(s) over %s, below the %d required: too little evidence for a performance claim",
			obs.Samples, obs.Window.Round(time.Minute), d.cfg.MinSamples)}
	}

	// The §4.7 rule: provisioning BELOW gp2's delivered baseline requires a
	// window long enough to have seen the business peak, and enough of that
	// window's datapoints to trust it. Otherwise the proposal is floored, so a
	// thin observation can only ever produce a same-or-better volume.
	obs.Floor = FloorGP2Baseline
	if obs.Window >= d.cfg.MinWindow && obs.Coverage >= d.cfg.MinCoverage {
		obs.Floor = FloorMeasured
	}
	obs.Confidence = math.Min(1, obs.Window.Seconds()/d.cfg.MinWindow.Seconds()) *
		math.Min(1, obs.Coverage/d.cfg.MinCoverage)
	return obs, nil
}

// evidence states the facts behind a verdict. A recommendation with no
// evidence is a bug (domain.Recommendation.Validate says so), so this is never
// allowed to come back empty for a volume that reached the arithmetic.
func (d *Domain) evidence(v *volume, obs Observation, p ParityPlan, now time.Time) []domain.Evidence {
	win := obs.Window.Round(time.Minute).String()
	ev := []domain.Evidence{
		{
			Metric:  fmt.Sprintf("iops-p%d", int(d.cfg.IOPSPercentile*100)),
			Value:   fmt.Sprintf("%.0f", obs.IOPSPercentile),
			Window:  win,
			Samples: len(v.IOPS.Points),
			Source:  SourceCloudWatch,
			At:      now,
		},
		{
			Metric:  fmt.Sprintf("throughput-mbps-p%d", int(d.cfg.ThroughputPercentile*100)),
			Value:   fmt.Sprintf("%.1f", obs.ThroughputPercentileMBps),
			Window:  win,
			Samples: len(v.Tput.Points),
			Source:  SourceCloudWatch,
			At:      now,
		},
		{
			Metric: "gp2-delivered-baseline",
			Value: fmt.Sprintf("%d IOPS, %.0f MiB/s at %d GiB",
				p.GP2.BaselineIOPS, p.GP2.BaselineThroughputMBps, p.SizeGiB),
			Source: SourceDescribeVolume,
		},
		{
			Metric: "parity-floor",
			Value:  p.Floor.String(),
			Source: SourceRateCard,
		},
	}
	if obs.HasBurstBalance {
		ev = append(ev, domain.Evidence{
			Metric:  "burst-balance-min-pct",
			Value:   fmt.Sprintf("%.1f", obs.BurstBalanceMinPct),
			Window:  win,
			Samples: len(v.Burst.Points),
			Source:  SourceCloudWatch,
			At:      now,
		})
	}
	if p.NaiveDegrades {
		ev = append(ev, domain.Evidence{
			Metric: "naive-gp3-shortfall",
			Value: fmt.Sprintf("the free gp3 baseline (%d IOPS, %d MiB/s) would fall %.0f IOPS and %.0f MiB/s short",
				p.Naive.IOPS, p.Naive.ThroughputMBps, p.NaiveIOPSShortfall, p.NaiveThroughputShortfall),
			Source: SourceRateCard,
		})
	}
	return ev
}

// recommendation builds the proposal. Caller holds d.mu.
func (d *Domain) recommendation(v *volume, a Assessment, p ParityPlan, obs Observation,
	ledger domain.Netter) domain.Recommendation {

	proposed := v.Spec.
		WithAttr(AttrVolumeType, VolumeTypeGP3).
		WithAttr(AttrIOPS, itoa(int64(p.Config.IOPS))).
		WithAttr(AttrThroughputMBps, itoa(int64(p.Config.ThroughputMBps)))

	rec := domain.Recommendation{
		Target:            v.Ref,
		Current:           v.Spec,
		Proposed:          proposed,
		CurrentHourlyUSD:  p.CurrentMonthlyUSD / HoursPerMonth,
		ProposedHourlyUSD: p.ProposedMonthlyUSD / HoursPerMonth,
		Action:            domain.ActionInPlace,
		Risk:              riskOf(p),
		Confidence:        obs.Confidence,
		Evidence:          a.Evidence,
		Reason:            reasonFor(p, obs),
	}

	net := p.DeltaMonthlyUSD
	if ledger != nil {
		before := d.usageLine(v.Ref, p.CurrentMonthlyUSD)
		after := d.usageLine(v.Ref, p.ProposedMonthlyUSD)
		as := ledger.Net([]commit.UsageLine{before}, []commit.UsageLine{after})
		net = as.NetMonthlyUSD
		if as.Suppressed {
			rec.Suppressed, rec.SuppressCode = true, as.ReasonCode
			rec.ValidFrom = as.ValidFrom
			rec.Reason = as.Reason + "; " + rec.Reason
			if net > 0 {
				net = 0
			}
		}
	}
	rec.SetSavings(p.DeltaMonthlyUSD, net)

	// Policy outranks economics in the reported code: a mode=recommend volume
	// must read as mode=recommend even when its numbers are also blocked.
	if !rec.Suppressed && a.Mode == guard.ModeRecommend {
		rec.Suppressed, rec.SuppressCode = true, domain.SuppressModeRecommend
		rec.Reason = TagKilterMode + "=" + guard.ModeRecommend + ": reporting only; " + rec.Reason
	}
	return rec
}

func reasonFor(p ParityPlan, obs Observation) string {
	base := fmt.Sprintf(
		"convert %d GiB gp2 → gp3 at %d IOPS / %d MiB/s, clearing measured p99 of %.0f IOPS / %.0f MiB/s with headroom; $%.2f/mo gross",
		p.SizeGiB, p.Config.IOPS, p.Config.ThroughputMBps,
		obs.IOPSPercentile, obs.ThroughputPercentileMBps, p.DeltaMonthlyUSD)
	if p.Floor == FloorGP2Baseline {
		base += fmt.Sprintf("; floored at gp2's delivered baseline (%d IOPS, %.0f MiB/s) because the window is %s",
			p.GP2.BaselineIOPS, p.GP2.BaselineThroughputMBps, obs.Window.Round(time.Hour))
	}
	if p.NaiveDegrades {
		base += fmt.Sprintf("; the naive gp3 baseline would have cost $%.2f/mo but delivered %.0f fewer IOPS",
			p.NaiveMonthlyUSD, p.NaiveIOPSShortfall)
	}
	return base
}

func itoa(v int64) string { return fmt.Sprintf("%d", v) }

// splitList splits a comma-joined attribute back into its parts.
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
