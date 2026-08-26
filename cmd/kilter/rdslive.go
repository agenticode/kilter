package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	domrds "github.com/agenticode/kilter/pkg/domain/rds"
	kcommit "github.com/agenticode/kilter/pkg/pricing/commit"
	"github.com/agenticode/kilter/pkg/provider"
	krds "github.com/agenticode/kilter/pkg/rds"
)

// The two RDS paths cmd/WIRING-FINDINGS.md §6.1 and §6.4 left unreachable: a
// live account, and the storage-parity seam.
//
// Both are READ-ONLY. Seven AWS operations are reachable from here and every
// one of them is a GET — `pkg/provider`'s TestNoMutatingSDKSurface is the
// code-side proof, and nothing in this file imports pkg/rds's actuator,
// names ApprovedStep or ModifyStorage, or gives the binary any way to reach
// them. Making an actuator reachable is a separate, separately-approved
// decision.
//
// # What is a sibling of what
//
// `--rds-fixture` is NOT replaced. It is how the collector is exercised
// without an account, it is what every test in this package drives, and the
// live path is a second source feeding the same domain:
//
//	--rds-fixture PATH    a recorded account, through the real collector
//	--rds-region  REGION  a live account, through pkg/provider's SDK adapters
//	--rds-parity          additionally read the modification envelope and run
//	                      pkg/rds's storage-parity engine over it
//
// Everything after collection is identical for both: Observe, the reservation
// splice into the account-wide inventory, the parity envelope. absorbRDS is
// that shared tail, written once so the two loops cannot drift.
//
// # The one degradation cmd/ has to implement itself
//
// RDS-ADAPTER-FINDINGS.md §3 is not symmetric, and the asymmetry is the whole
// content of this file's error handling: a NIL MetricsAPI degrades (complete
// report, every instance refusing with no-metric-evidence) while a FAILING one
// aborts the collection. `cloudwatch:GetMetricData` is documented optional, so
// a credential without it must get the degraded report rather than an
// AccessDeniedException — which means one retry with the seam dropped, gated
// on provider.IsAccessDenied so a throttle or a timeout stays an error.
//
// Every other optional seam already degrades inside pkg/rds and is wired
// unconditionally: a denied DescribeReservedDBInstances warns and nets to
// gross, a denied DescribeDBClusters warns and falls back to the more cautious
// cluster-member exclusion, a denied ListTagsForResource warns by name and
// leaves the kilter.dev/mode guardrail unevaluated, and a denied
// DescribeValidDBInstanceModifications or DescribeEvents leaves that
// instance's envelope unknown so its provisioning proposals refuse. Not one of
// those is turned into a hard failure here, and not one of them is turned into
// silence either.

// rdsEnvelopeWindow is the event history the parity seam reads.
//
// It must exceed StorageModificationWindow: the question the events answer is
// "have there been four storage modifications in the last 24 hours", and a
// window that cannot contain 24 hours cannot rule one out — pkg/rds's
// EnvelopeCollector says exactly that in a warning if you hand it a shorter
// one. 48 h is RDS-ADAPTER-FINDINGS.md §6.4's figure: twice the period, so the
// oldest modification that still blocks is comfortably inside it.
const rdsEnvelopeWindow = 48 * time.Hour

// rdsCollectOptions is one RDS collection, recorded or live.
type rdsCollectOptions struct {
	// Scope is the accountID/region every target ref is stamped with.
	Scope string
	// Region selects the rate-card row that prices every instance, and on the
	// live path it is also the region the SDK clients talk to. The two must be
	// the same value or every dollar in the report is confidently wrong.
	Region string
	// Now is the decision time. It comes from --now and is never a clock read
	// inside a decision.
	Now time.Time
	// Span is the requested observation window. pkg/rds clamps it to
	// CloudWatch retention; this package does not re-derive that.
	Span time.Duration
	// Parity requests the modification envelope and enables pkg/rds's
	// storage-parity seam. Off by default, and its absence is visible in the
	// report — see rdsParitySeam.
	Parity bool
}

func rdsOptions(df *domainFlags, region string, now time.Time) rdsCollectOptions {
	return rdsCollectOptions{
		Scope: df.scope, Region: region, Now: now, Span: df.rdsWindow, Parity: df.rdsParity,
	}
}

// rdsLiveSeams is one region's live read surface: the four pkg/rds interfaces,
// plus the adapters' own Notes().
//
// Three of the four are the SAME *provider.RDSAPI, because one credential
// answers all three rds: seams — exactly as rds.Fixture does. They stay
// separate fields because a caller may hold one permission and not another,
// and the right behaviour then is a degraded report rather than a missing one.
type rdsLiveSeams struct {
	// Region is what the adapter reports back, not what was asked for, and it
	// is what CollectorConfig.Region is set from.
	Region     string
	Inventory  krds.InventoryAPI
	Metrics    krds.MetricsAPI
	Commitment krds.CommitmentAPI
	Envelope   krds.ModificationEnvelopeAPI
	// Notes returns the degradations the seam structs have no field for — an
	// unaddressable instance, a keyless tag, an undated event, a result
	// CloudWatch did not identify. RDS-ADAPTER-FINDINGS.md §6.2: rendering
	// them is not optional, because a degradation nobody can see is a
	// degradation that did not happen.
	Notes func() []string
}

// newRDSLiveSeams is the constructor seam.
//
// It is a variable so the tests in this package can drive the whole live path
// — the retry, the notes, the envelope collection, the warning strings — over
// pkg/rds's own fixtures. No test in this package reads a credential, opens
// ~/.aws, sets an AWS_* variable or touches a socket, and a test that could
// reach the network from a developer laptop with a stale profile would be a
// failed unit rather than a slow one.
var newRDSLiveSeams = dialRDS

// dialRDS builds the live adapters. This is the only function in cmd/ that
// reads an AWS credential for RDS.
func dialRDS(ctx context.Context, region string) (rdsLiveSeams, error) {
	inv, err := provider.NewRDSAPI(ctx, region)
	if err != nil {
		return rdsLiveSeams{}, err
	}
	// RDS-ADAPTER-FINDINGS.md §6.5: the two adapters MUST be paired on the
	// same region. A metric is published in the region its database lives in,
	// so a cross-region pairing returns empty series for every instance —
	// which reads as an account full of idle databases rather than as a
	// misconfiguration.
	cw, err := provider.NewCloudWatchAPI(ctx, inv.Region())
	if err != nil {
		return rdsLiveSeams{}, err
	}
	return rdsLiveSeams{
		Region:    inv.Region(),
		Inventory: inv, Metrics: cw, Commitment: inv, Envelope: inv,
		Notes: func() []string { return append(inv.Notes(), cw.Notes()...) },
	}, nil
}

// collectRDSLive runs the real collector over a live account in one region.
//
// The window is [now-span, now] and is then CLAMPED by pkg/rds, because
// 1-minute CloudWatch datapoints live 15 days. The clamp is pkg/rds's job and
// this function does not re-derive it; it reports it, because a snapshot that
// claims a 30-day window and holds 15 days of data is a lie told by omission.
//
// Warnings are returned even on the error path. A collection that failed
// halfway still learned things — which region, which permission, which clamp —
// and dropping them because the run ended badly is how an operator ends up
// debugging an AccessDeniedException with no idea which of seven calls raised
// it.
func collectRDSLive(ctx context.Context, opts rdsCollectOptions) (
	*krds.Snapshot, []krds.Envelope, []string, error) {

	seams, err := newRDSLiveSeams(ctx, opts.Region)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("--rds-region %s: %w", opts.Region, err)
	}
	label := "rds " + seams.Region
	var warnings []string
	note := func(format string, args ...any) {
		warnings = append(warnings, label+": "+fmt.Sprintf(format, args...))
	}

	cfg := krds.DefaultCollectorConfig(krds.Window{Start: opts.Now.Add(-opts.Span), End: opts.Now})
	// The SAME region the clients talk to, per RDS-ADAPTER-FINDINGS.md §6.1.
	cfg.Scope, cfg.Region = opts.Scope, seams.Region

	collect := func(metrics krds.MetricsAPI) (*krds.Snapshot, krds.Window, error) {
		c, err := krds.NewCollector(seams.Inventory, metrics, seams.Commitment, cfg)
		if err != nil {
			return nil, krds.Window{}, err
		}
		snap, err := c.Collect(ctx)
		return snap, c.Window(), err
	}

	snap, observed, err := collect(seams.Metrics)
	// The ONE seam that must be dropped rather than propagated. Everything
	// else that can be denied already degrades inside pkg/rds, and a hard
	// failure here would turn a documented degraded report into no report.
	if err != nil && isMetricsAccessDenied(err) {
		note("this credential does not hold cloudwatch:GetMetricData, so the metrics seam was " +
			"dropped and every instance is reported without CloudWatch evidence and refuses with " +
			krds.ReasonNoMetricEvidence + " — the inventory is complete and no verdict is drawn " +
			"from the silence")
		snap, observed, err = collect(nil)
	}
	if err != nil {
		// Loud, by design. DescribeDBInstances is the one hard dependency:
		// with no inventory there is nothing to report on, and a report that
		// silently covered fewer databases than the account holds is worse
		// than no report at all.
		return nil, nil, warnings, fmt.Errorf("%s: %w", label, err)
	}

	if observed != cfg.Window {
		note("observation window clamped to %s (1-minute CloudWatch datapoints live %s)",
			observed.String(), krds.RetentionAtOneMinute)
	}
	for _, n := range seams.Notes() {
		note("%s", n)
	}
	for _, w := range snap.Warnings {
		note("%s", w)
	}

	envs, ewarns, err := collectRDSEnvelopes(ctx, opts, seams.Envelope, rdsIdentifiers(snap), label)
	warnings = append(warnings, ewarns...)
	if err != nil {
		return nil, nil, warnings, fmt.Errorf("%s: %w", label, err)
	}
	return snap, envs, warnings, nil
}

// isMetricsAccessDenied is the exact predicate the one retry is gated on.
//
// BOTH halves matter. provider.IsAccessDenied matches only permission denials,
// so a throttle or a timeout stays an error rather than being recorded as a
// permission the credential actually holds; and the message test keeps the
// retry pointed at the metrics seam, so a denied DescribeDBInstances — which
// must fail loudly — can never be quietly downgraded into "no CloudWatch".
// pkg/rds wraps that one failure as `rds: get metric data: %w`.
func isMetricsAccessDenied(err error) bool {
	return err != nil && provider.IsAccessDenied(err) &&
		strings.Contains(err.Error(), "get metric data")
}

// rdsIdentifiers is the instance list the envelope seam is asked about, in the
// snapshot's own order. The collector sorts and de-duplicates it.
func rdsIdentifiers(snap *krds.Snapshot) []string {
	if snap == nil {
		return nil
	}
	out := make([]string, 0, len(snap.Targets))
	for _, t := range snap.Targets {
		if id := strings.TrimSpace(t.Instance.Identifier); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// collectRDSEnvelopes reads the provisioning envelope and the recent
// storage-modification history, and is a no-op unless --rds-parity asked for
// it. A nil api is legal and yields a wholly unknown set, which refuses every
// provisioning proposal by name rather than assuming a ceiling.
func collectRDSEnvelopes(ctx context.Context, opts rdsCollectOptions,
	api krds.ModificationEnvelopeAPI, ids []string, label string) ([]krds.Envelope, []string, error) {

	if !opts.Parity {
		return nil, nil, nil
	}
	c := krds.NewEnvelopeCollector(api, krds.EnvelopeCollectorConfig{
		Window: krds.Window{Start: opts.Now.Add(-rdsEnvelopeWindow), End: opts.Now},
	})
	envs, err := c.Collect(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	warns := make([]string, 0, len(envs.Warnings))
	for _, w := range envs.Warnings {
		warns = append(warns, label+": "+w)
	}
	return envs.All(), warns, nil
}

// --- the storage-parity seam ----------------------------------------------

// rdsParitySeam is the cmd/-side holder for pkg/rds's StorageParity seam
// (U13), and it exists to break one ordering problem.
//
// rds.Config.Parity is fixed when the domain is CONSTRUCTED, and the parity
// engine needs the modification envelopes, which can only be read once the
// inventory names the instances — which happens after the domain is
// registered. So the domain is registered holding this indirection, and every
// collected source folds its envelopes in through observe(). The envelopes are
// accumulated across sources and the engine is rebuilt, because one domain can
// be fed several fixtures and several live regions and AssessParity is called
// once at report time, after all of them.
//
// # Why the nil branch is not a nil return
//
// StorageParity's third result is "I declined to look at all", and the sizer
// reads it literally: ok=false emits NEITHER a proposal NOR a suppression, and
// it does not fall back to the no-storage-performance-model refusal either,
// because that lives in the else-branch of `cfg.Parity != nil`. A holder that
// returned ok=false while unfilled would therefore produce a report with a
// missing dimension and no line saying so — the exact failure this seam is
// supposed to make impossible. So it returns a suppression instead, and says
// which of the two states produced it.
type rdsParitySeam struct {
	now  time.Time
	perf krds.PerformanceRates
	// envelopes is every source's contribution, in collection order.
	// krds.NewEnvelopes sorts and de-duplicates them.
	envelopes []krds.Envelope
	inner     krds.StorageParity
}

// observe folds one source's envelopes in and rebuilds the engine. A nil
// receiver is the "--rds-parity was not passed" case and is a no-op, so both
// collection loops can call it unconditionally.
func (s *rdsParitySeam) observe(envs []krds.Envelope) error {
	if s == nil {
		return nil
	}
	s.envelopes = append(s.envelopes, envs...)
	p, err := krds.NewParity(krds.ParityConfig{
		Now:       s.now,
		Envelopes: krds.NewEnvelopes(s.envelopes),
		// MinWindow, Headroom, MinConfidence and Percentile are pkg/rds's
		// policy and are deliberately not re-stated here. A second set of
		// thresholds in cmd/ would be a second answer to a question that must
		// have one.
		Performance: s.perf,
	})
	if err != nil {
		return err
	}
	s.inner = p
	return nil
}

// AssessParity implements krds.StorageParity by delegation.
func (s *rdsParitySeam) AssessParity(inst krds.DBInstance, e krds.Engine, series []krds.Series,
	card krds.RateCard) (*krds.Proposal, []krds.Suppression, bool) {

	if s == nil || s.inner == nil {
		return nil, []krds.Suppression{{
			Code: krds.ReasonNoStoragePerformanceModel,
			Reason: fmt.Sprintf(
				"storage-performance parity was requested for %s but no parity engine was built, so "+
					"this instance's storage is unassessed. This is a wiring bug rather than a finding, "+
					"and it is stated rather than skipped: a report missing a dimension it does not "+
					"mention reads as a report that looked and found nothing", inst.DisplayName()),
		}}, true
	}
	return s.inner.AssessParity(inst, e, series, card)
}

// rdsPerformanceRatesFile is the on-disk shape of --rds-parity-rates.
//
// It is a cmd/-side projection of rds.PerformanceRates with one field
// deliberately missing: `provenance`. rds.LoadRates stamps every loaded row
// operator-supplied and gives the file no way to name its own provenance, for
// the reason that provenance is the single gate between "this sizes an
// opportunity" and "this is a saving somebody can put in a business case". A
// second rate loader that let a file type the word "verified" would be a way
// to promote a guess to a claim, so an unknown field here is an error and the
// stamp is applied by this code.
type rdsPerformanceRatesFile struct {
	// ProvisionedIOPSMonthUSD is charged per IOPS above the regime baseline.
	ProvisionedIOPSMonthUSD float64 `json:"provisionedIOPSMonthUSD"`
	// ProvisionedThroughputMonthUSD is charged per MiB/s above the regime
	// baseline.
	ProvisionedThroughputMonthUSD float64 `json:"provisionedThroughputMonthUSD"`
}

// loadRDSPerformanceRates resolves the two gp3 knobs the RateCard does not
// price.
//
// The zero value means rds.DefaultPerformanceRates, every figure of which is
// `unverified` — pkg/rds/FINDINGS.md §7 could not retrieve the RDS
// provisioned-IOPS and provisioned-throughput rates from AWS. That is not a
// failure mode: parity still runs, still does the arithmetic and still reports
// the magnitude, and then refuses to call it a saving under
// `unverified-rate`. Supplying this file is what unblocks the claim.
func loadRDSPerformanceRates(path string) (krds.PerformanceRates, error) {
	if path == "" {
		return krds.PerformanceRates{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return krds.PerformanceRates{}, fmt.Errorf("--rds-parity-rates: %w", err)
	}
	var f rdsPerformanceRatesFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return krds.PerformanceRates{}, fmt.Errorf("--rds-parity-rates %s: %w", path, err)
	}
	out := krds.PerformanceRates{
		ProvisionedIOPSMonthUSD:       f.ProvisionedIOPSMonthUSD,
		ProvisionedThroughputMonthUSD: f.ProvisionedThroughputMonthUSD,
		Provenance:                    krds.RateOperator,
	}
	// Validated at the boundary, so a bad override fails where it was typed
	// rather than producing a quietly wrong report.
	if err := out.Validate(); err != nil {
		return krds.PerformanceRates{}, fmt.Errorf("--rds-parity-rates %s: %w", path, err)
	}
	return out, nil
}

// newRDSDomain builds the rds domain, with the parity seam when it was asked
// for.
//
// Without --rds-parity this is exactly domrds.New and rds.Config.Parity stays
// nil, which is what makes the seam's absence VISIBLE: pkg/rds's sizer emits
// no-storage-performance-model for every instance, so a report that did not
// assess parity says so on every line rather than looking complete.
//
// With it, the domain has to be built through rds.NewDomain directly, because
// domrds.Config carries Scope, Region and Rates and has no Parity field —
// pkg/domain/rds is outside this unit's scope. The wrapper is reconstructed
// around the result so Domain.UsageLines still contributes to the
// account-wide commitment baseline; see cmd/RDSLIVE-FINDINGS.md for the
// one-field change that would remove this.
func newRDSDomain(df *domainFlags, now time.Time) (*domrds.Domain, *rdsParitySeam, error) {
	card, err := loadRDSRates(df.rdsRates)
	if err != nil {
		return nil, nil, err
	}
	if !df.rdsParity {
		d, err := domrds.New(domrds.Config{Scope: df.scope, Region: df.region, Rates: card})
		return d, nil, err
	}
	perf, err := loadRDSPerformanceRates(df.rdsParityRates)
	if err != nil {
		return nil, nil, err
	}
	seam := &rdsParitySeam{now: now, perf: perf}
	// Built before any collection, so the engine exists even if no source is
	// ever supplied. Its envelopes are empty then, and an empty envelope set
	// refuses every provisioning proposal with provisioning-envelope-unknown —
	// which is the honest answer, and a louder one than silence.
	if err := seam.observe(nil); err != nil {
		return nil, nil, fmt.Errorf("--rds-parity: %w", err)
	}
	sc := krds.DefaultConfig()
	sc.Scope, sc.Region = df.scope, df.region
	if len(card.Classes) > 0 {
		sc.Rates = card
	}
	sc.Parity = seam
	d, err := krds.NewDomain(sc)
	if err != nil {
		return nil, nil, fmt.Errorf("domain/rds: %w", err)
	}
	return &domrds.Domain{Domain: d}, seam, nil
}

// rdsFailure attaches what a failed collection had already learned to the
// error that ended it.
//
// buildRuntime returns nil on error, so anything appended to runtime.Warnings
// on the way to a failure is discarded — which is how an operator ends up
// staring at an AccessDeniedException with no idea which of seven calls raised
// it, or that the run had already fallen back from a denied GetMetricData
// before it died of something else. A collection that failed halfway still
// learned things, and they belong in the only channel that survives.
func rdsFailure(err error, warnings []string) error {
	if err == nil || len(warnings) == 0 {
		return err
	}
	var b strings.Builder
	b.WriteString(err.Error())
	b.WriteString("\n\nwhat the collection had already learned before it failed:")
	for _, w := range warnings {
		b.WriteString("\n  - ")
		b.WriteString(w)
	}
	return errors.New(b.String())
}

// absorbRDS is the tail both collection loops share: observe the snapshot,
// fold in the envelopes, splice the reservations into the account-wide
// inventory.
//
// It is one function rather than two copies because the reservation splice is
// the part with a money consequence — an RDS line is absorbed by a Reserved DB
// Instance and by nothing else, no Savings Plan of any type covers RDS — and
// two copies of it would be two chances for the live path and the recorded
// path to disagree about the same account.
func absorbRDS(d *domrds.Domain, p *rdsParitySeam, snap *krds.Snapshot,
	envs []krds.Envelope, inv *kcommit.Inventory, label string) (*kcommit.Inventory, error) {

	if err := d.Observe(snap); err != nil {
		return inv, fmt.Errorf("%s: %w", label, err)
	}
	if err := p.observe(envs); err != nil {
		return inv, fmt.Errorf("%s: %w", label, err)
	}
	if len(snap.Reservations) > 0 {
		if inv == nil {
			inv = &kcommit.Inventory{}
		}
		inv.ReservedDBs = append(inv.ReservedDBs, snap.Reservations...)
	}
	return inv, nil
}
