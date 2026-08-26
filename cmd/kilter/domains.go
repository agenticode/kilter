package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	domec2 "github.com/agenticode/kilter/pkg/domain/ec2"
	domecs "github.com/agenticode/kilter/pkg/domain/ecs"
	"github.com/agenticode/kilter/pkg/domain/fargate"
	domlambda "github.com/agenticode/kilter/pkg/domain/lambda"
	domrds "github.com/agenticode/kilter/pkg/domain/rds"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/model"
	kcommit "github.com/agenticode/kilter/pkg/pricing/commit"
	krds "github.com/agenticode/kilter/pkg/rds"
)

// `kilter domains` is where eight packages of decision logic become reachable
// from the binary.
//
// Everything it reads is a RECORDED SNAPSHOT, with exactly one exception:
// --rds-region dials AWS through pkg/provider's read-only SDK adapters to fill
// the RDS snapshot that would otherwise come from --rds-fixture. Without that
// flag no network call is made and no credential is read. No clock is read
// inside a decision either way: the decision time comes from --now (defaulting
// to the wall clock once, at the top) and is threaded through every domain.
// That is not a testing convenience; it is design invariant 2 — the brain's
// DECISION path stays stdlib-and-intra-repo, and collection is the only place
// an SDK appears.
//
// The command prints refusals as prominently as recommendations. On a real
// account most of the output IS refusals — every Lambda function on a
// never-power-tuned fleet, every gp2 volume in the 334-375 GiB band, every
// instance with no memory signal — and a report that showed only the
// recommendations would let a reader conclude the engine found nothing, which
// is a different claim from "the engine declined to guess, and here is what it
// would need".

const domainsUsage = `kilter domains — run every compute domain over recorded snapshots

Usage:
  kilter domains list    [flags]   registered domains, health, actuation
  kilter domains report  [flags]   observe -> recommend -> report (all domains)
  kilter domains plan    [flags]   build executable steps (never executes)

Input is recorded snapshots; this command makes no cloud call.

Flags:
  --snapshot PATH        domain snapshot JSON; repeatable, routed by its "domain" field
  --kube-snapshot PATH   cluster snapshot JSON (kilter analyze --dump-snapshot) for k8s-fargate
  --rds-fixture PATH     recorded RDS account; runs the real rds collector (repeatable)
  --rds-region REGION    collect RDS LIVE from this region (needs AWS credentials; repeatable)
  --rds-rates PATH       RDS rate override JSON; layered over the shipped unverified table
  --rds-window DUR       RDS observation window (default 336h); clamped to CloudWatch retention
  --rds-parity           also assess gp2/gp3 storage parity (reads the modification envelope)
  --rds-parity-rates P   verified provisioned-IOPS/throughput rates; without it parity refuses
                         to call its arithmetic a saving
  --commitments PATH     RI/Savings-Plan inventory JSON (kilter pricing sync-commitments)
  --catalog PATH         pricing catalog JSON (default: embedded)
  --domain KIND          restrict to one domain; repeatable (%s)
  --scope SCOPE          default target scope for snapshots that carry none
  --region REGION        region label for commitment usage lines
  --now RFC3339          decision time (default: now)
  --json                 machine-readable output
  --rds-detail           also print pkg/rds's own refusals-first report

plan-only flags:
  --max-steps N          cap the plan
  --window SPEC          change window, e.g. "Sat 02:00-06:00"
  --freeze               refuse all actuation
  --breaker-open         refuse all actuation (simulated circuit breaker)
`

func runDomains(args []string) error { return runDomainsTo(os.Stdout, args) }

// runDomainsTo is the testable entry point: everything the command prints goes
// to w, so an integration test can drive the real wiring over recorded
// snapshots and assert on the bytes a user would see.
func runDomainsTo(w io.Writer, args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(w, domainsUsage, strings.Join(kindNames(), ", "))
		return fmt.Errorf("domains: a subcommand is required")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runDomainsList(w, rest)
	case "report":
		return runDomainsReport(w, rest)
	case "plan":
		return runDomainsPlan(w, rest)
	case "help", "-h", "--help":
		fmt.Fprintf(w, domainsUsage, strings.Join(kindNames(), ", "))
		return nil
	default:
		fmt.Fprintf(w, domainsUsage, strings.Join(kindNames(), ", "))
		return fmt.Errorf("domains: unknown subcommand %q", sub)
	}
}

func kindNames() []string {
	ks := domain.Kinds()
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return out
}

// domainFlags is the input every subcommand shares.
type domainFlags struct {
	snapshots   repeatedFlag
	kubeSnaps   repeatedFlag
	rdsFixtures repeatedFlag
	// rdsRegions is the LIVE sibling of rdsFixtures: one collector, one
	// RDSAPI and one CloudWatchAPI per region, merged into one domain.
	rdsRegions repeatedFlag
	kinds      repeatedFlag
	rdsRates   string
	// rdsParityRates prices the two gp3 knobs the rate card does not cover.
	rdsParityRates string
	rdsWindow      time.Duration
	rdsDetail      bool
	rdsParity      bool
	commitments    string
	catalog        string
	scope          string
	region         string
	now            string
	jsonOut        bool

	maxSteps    int
	window      string
	freeze      bool
	breakerOpen bool
}

// repeatedFlag collects a repeatable string flag, preserving order.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func (df *domainFlags) bind(fs *flag.FlagSet, withPlan bool) {
	fs.Var(&df.snapshots, "snapshot", "domain snapshot JSON (repeatable)")
	fs.Var(&df.kubeSnaps, "kube-snapshot", "cluster snapshot JSON for k8s-fargate (repeatable)")
	fs.Var(&df.rdsFixtures, "rds-fixture", "recorded RDS account JSON, run through the real collector (repeatable)")
	fs.Var(&df.rdsRegions, "rds-region", "collect RDS live from this region (requires AWS credentials; repeatable)")
	fs.StringVar(&df.rdsRates, "rds-rates", "", "RDS rate override JSON (pkg/rds LoadRates format)")
	fs.DurationVar(&df.rdsWindow, "rds-window", 14*24*time.Hour, "RDS observation window")
	fs.BoolVar(&df.rdsDetail, "rds-detail", false, "also print pkg/rds's own refusals-first report")
	fs.BoolVar(&df.rdsParity, "rds-parity", false, "assess gp2/gp3 storage parity (reads the RDS modification envelope)")
	fs.StringVar(&df.rdsParityRates, "rds-parity-rates", "", "verified provisioned-IOPS/throughput rates JSON")
	fs.Var(&df.kinds, "domain", "restrict to one domain kind (repeatable)")
	fs.StringVar(&df.commitments, "commitments", "", "RI/Savings-Plan inventory JSON")
	fs.StringVar(&df.catalog, "catalog", "", "pricing catalog JSON (default: embedded)")
	fs.StringVar(&df.scope, "scope", "", "default target scope (accountID/region or clusterID)")
	fs.StringVar(&df.region, "region", "", "region label for commitment usage lines")
	fs.StringVar(&df.now, "now", "", "decision time as RFC3339 (default: now)")
	fs.BoolVar(&df.jsonOut, "json", false, "emit machine-readable JSON")
	if !withPlan {
		return
	}
	fs.IntVar(&df.maxSteps, "max-steps", 0, "cap the plan (0 = unlimited)")
	fs.StringVar(&df.window, "window", "", `change window, e.g. "Sat 02:00-06:00"`)
	fs.BoolVar(&df.freeze, "freeze", false, "refuse all actuation")
	fs.BoolVar(&df.breakerOpen, "breaker-open", false, "refuse all actuation (simulated breaker)")
}

// runtime is one fully-wired brain: the registry, the commitment ledger, and
// whatever could not be learned, kept rather than discarded.
type runtime struct {
	Registry *domain.Registry
	Ledger   *domain.Ledger
	Now      time.Time
	// Warnings are collection-time problems. A snapshot for a domain nobody
	// registered lands here rather than being dropped: silently discarding
	// collected data is indistinguishable from a broken collector.
	Warnings []string
	// rds is kept only so --rds-detail can render pkg/rds's own report, whose
	// layout puts refusals first. Every other consumer goes through the
	// registry.
	rds *domrds.Domain
}

// buildRuntime wires the registry, feeds it every recorded snapshot, and
// builds the account-wide commitment ledger.
func buildRuntime(df *domainFlags) (*runtime, error) {
	now, err := parseNow(df.now)
	if err != nil {
		return nil, err
	}
	wanted, err := wantedKinds(df.kinds)
	if err != nil {
		return nil, err
	}
	catalog, err := loadCatalog(df.catalog)
	if err != nil {
		return nil, err
	}

	rt := &runtime{Registry: domain.NewRegistry(), Now: now}

	// Registration is explicit and total: every domain this binary knows how
	// to build is registered, and one that never receives a snapshot reports
	// itself report-only with the reason. That is deliberately louder than
	// omitting it — "kilter has no Lambda support" and "kilter has Lambda
	// support and no Lambda data" are different answers.
	if wanted[domain.EC2] {
		d, err := domec2.New(domec2.Config{
			Scope: df.scope, Region: df.region, Catalog: catalog,
			// No EBS actuator exists in this build: there is no SDK adapter
			// for ec2:ModifyVolume yet, and claiming actuation without one
			// would be a promise the binary cannot keep.
			VolumeActuationAvailable: false,
		})
		if err != nil {
			return nil, err
		}
		if err := rt.Registry.Register(d); err != nil {
			return nil, err
		}
	}
	if wanted[domain.ECSFargate] {
		d, err := domecs.New(domecs.Config{Scope: df.scope})
		if err != nil {
			return nil, err
		}
		if err := rt.Registry.Register(d); err != nil {
			return nil, err
		}
	}
	if wanted[domain.Lambda] {
		d, err := domlambda.New(domlambda.Config{})
		if err != nil {
			return nil, err
		}
		if err := rt.Registry.Register(d); err != nil {
			return nil, err
		}
	}
	if wanted[domain.K8sFargate] {
		cfg := fargate.DefaultConfig()
		cfg.Scope, cfg.Region = df.scope, df.region
		d, err := fargate.New(cfg)
		if err != nil {
			return nil, err
		}
		if err := rt.Registry.Register(d); err != nil {
			return nil, err
		}
	}
	// RDS. It registers like any other domain and then refuses everything,
	// which is the deliverable rather than a gap: the class is where the money
	// is and changing it is a failover, allocated storage cannot shrink, and
	// FreeableMemory is MemAvailable. Its Recommend() is empty by construction,
	// so the whole output arrives through the Refuser seam.
	//
	// --rds-parity additionally fills pkg/rds's StorageParity seam, which is
	// nil by default. Nil is not a hole: the sizer then refuses every
	// instance's storage with no-storage-performance-model, so a report that
	// did not assess parity SAYS it did not on every line.
	var rdsDomain *domrds.Domain
	var rdsParity *rdsParitySeam
	if wanted[domain.RDS] {
		d, seam, err := newRDSDomain(df, now)
		if err != nil {
			return nil, err
		}
		if err := rt.Registry.Register(d); err != nil {
			return nil, err
		}
		rdsDomain, rt.rds, rdsParity = d, d, seam
	}

	// Feed it. A snapshot that cannot be read is fatal (a path the operator
	// typed is wrong); a snapshot for a domain nobody registered is a warning
	// (the data is real, this process just has no organ for it).
	inv, err := loadInventory(df.commitments)
	if err != nil {
		return nil, err
	}
	for _, path := range df.snapshots {
		snap, err := loadDomainSnapshot(path, df.scope)
		if err != nil {
			return nil, err
		}
		if snap.Commitments != nil && inv == nil {
			inv = snap.Commitments
		}
		if err := rt.Registry.Learn(snap); err != nil {
			if errors.Is(err, domain.ErrNotRegistered) {
				rt.Warnings = append(rt.Warnings,
					fmt.Sprintf("%s: snapshot for domain %q, which is not registered here", path, snap.Domain))
				continue
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	for _, path := range df.kubeSnaps {
		snap, err := loadClusterSnapshot(path, df.scope)
		if err != nil {
			return nil, err
		}
		if err := rt.Registry.Learn(snap); err != nil {
			if errors.Is(err, domain.ErrNotRegistered) {
				rt.Warnings = append(rt.Warnings,
					fmt.Sprintf("%s: cluster snapshot, but k8s-fargate is not registered here", path))
				continue
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	// The RDS collection loop (pkg/rds/FINDINGS.md §6.3), over a recorded
	// account. Observe() takes the native snapshot rather than the generic
	// projection because domain.Sample has no truncation flag: a series
	// flattened into samples arrives looking complete, and a truncated
	// DatabaseConnections series that looks complete is an idle verdict
	// manufactured out of silence.
	//
	// §6.5 (in absorbRDS): snap.Reservations is already
	// []commit.ReservedDBInstance and goes straight into the account-wide
	// inventory. An RDS line is absorbed by a Reserved DB Instance and by
	// nothing else — no Savings Plan of any type covers RDS — so appending
	// cannot disturb what --commitments contributed for the other domains.
	for _, path := range df.rdsFixtures {
		if rdsDomain == nil {
			rt.Warnings = append(rt.Warnings,
				fmt.Sprintf("%s: RDS fixture supplied, but the rds domain is not registered here", path))
			continue
		}
		snap, envs, warns, err := collectRDSFixture(context.Background(), path, rdsOptions(df, df.region, now))
		if err != nil {
			return nil, rdsFailure(err, warns)
		}
		rt.Warnings = append(rt.Warnings, warns...)
		if inv, err = absorbRDS(rdsDomain, rdsParity, snap, envs, inv, path); err != nil {
			return nil, err
		}
	}
	// The LIVE sibling of the loop above (cmd/WIRING-FINDINGS.md §6.1). One
	// RDSAPI, one CloudWatchAPI and one collector per region, merged into the
	// same domain — and everything downstream is identical, because a live
	// snapshot is the same type as a recorded one.
	for _, region := range df.rdsRegions {
		if rdsDomain == nil {
			rt.Warnings = append(rt.Warnings,
				fmt.Sprintf("--rds-region %s: supplied, but the rds domain is not registered here", region))
			continue
		}
		snap, envs, warns, err := collectRDSLive(context.Background(), rdsOptions(df, region, now))
		if err != nil {
			// A collection that failed halfway still learned which region and
			// which permission. buildRuntime returns nil on error, so the only
			// channel that survives is the error itself.
			return nil, rdsFailure(err, warns)
		}
		rt.Warnings = append(rt.Warnings, warns...)
		if inv, err = absorbRDS(rdsDomain, rdsParity, snap, envs, inv, "rds "+region); err != nil {
			return nil, err
		}
	}

	ledger, ledgerWarnings := buildLedger(rt.Registry, inv, now)
	rt.Ledger = ledger
	rt.Warnings = append(rt.Warnings, ledgerWarnings...)
	sort.Strings(rt.Warnings)
	return rt, nil
}

// buildLedger constructs the ACCOUNT-WIDE commitment ledger.
//
// Account-wide is the whole point. pkg/pricing/commit documents that assessing
// only the affected usage lines is a correctness bug — Compute Savings Plans
// absorb usage account-wide, so a partial view understates absorption and
// overstates the saving (§4.4 ex.3). A domain knows only its own targets, so
// the brain splices: every domain that can project its priced inventory into
// usage lines contributes to one baseline, and every domain's net savings are
// computed against that.
//
// The two passes are deliberate and cheap. The first asks the instance domain
// what it currently costs — a figure that does not depend on any commitment,
// because it is the on-demand rate of what is running today. The second nets
// every domain's proposed change against that whole picture.
func buildLedger(reg *domain.Registry, inv *kcommit.Inventory, now time.Time) (*domain.Ledger, []string) {
	var lines []kcommit.UsageLine
	var warnings []string
	seen := map[string]domain.Kind{}
	for _, k := range reg.Kinds() {
		d, ok := reg.Get(k)
		if !ok {
			continue
		}
		for _, l := range usageLinesOf(d, now) {
			if why, bad := badBaselineLine(l, seen); bad {
				warnings = append(warnings, fmt.Sprintf(
					"%s: dropped a usage line from the account-wide baseline (%s)", k, why))
				continue
			}
			seen[l.ID] = k
			lines = append(lines, l)
		}
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].ID < lines[j].ID })
	return domain.NewLedger(inv, kcommit.Usage{Lines: lines}), warnings
}

// badBaselineLine is the output check on the usage-line seam.
//
// It is the same hole j1-wire closed one level down. Registry.PlanSteps
// filtered its INPUT and not its output, so a domain could return a step
// labelled with another domain's name and borrow that domain's actuator; here
// a domain returns lines that go straight into the ACCOUNT-WIDE commitment
// baseline, which is what every OTHER domain's net savings are computed
// against. Three ways that goes wrong, all silent:
//
//   - An EMPTY ID never matches in domain.Ledger's splice and is always
//     appended, so two anonymous lines for one resource double-count the usage
//     available to absorb a commitment — which OVERSTATES absorption and
//     therefore overstates savings.
//   - A DUPLICATE ID from a second domain replaces the first domain's line
//     rather than adding to it, so one domain silently rewrites another's
//     contribution.
//   - A non-positive or non-finite rate or quantity prices real usage at
//     nothing, which again makes a commitment look more absorbed than it is.
//
// Dropping is the conservative direction: fewer baseline lines means less
// usage to absorb a commitment, means more apparent stranding, means a
// smaller claimed saving. A drop is never silent — it lands in the collection
// warnings the CLI prints.
func badBaselineLine(l kcommit.UsageLine, seen map[string]domain.Kind) (string, bool) {
	switch {
	case l.ID == "":
		return fmt.Sprintf("no ID (%s %s); an anonymous line cannot be spliced and would double-count",
			l.Kind, l.InstanceType), true
	case seen[l.ID] != "":
		return fmt.Sprintf("ID %q already contributed by domain %q", l.ID, seen[l.ID]), true
	case math.IsNaN(l.ODRate) || math.IsInf(l.ODRate, 0) || l.ODRate <= 0:
		return fmt.Sprintf("ID %q has rate %v", l.ID, l.ODRate), true
	case math.IsNaN(l.Quantity) || math.IsInf(l.Quantity, 0) || l.Quantity <= 0:
		return fmt.Sprintf("ID %q has quantity %v", l.ID, l.Quantity), true
	}
	return "", false
}

// usageLiner is any domain (or composite part) that can project its priced
// inventory into account-wide usage lines.
type usageLiner interface {
	UsageLines(now time.Time, ledger domain.Netter) []kcommit.UsageLine
}

// usageLinesOf reaches through a composite to find the parts that can
// contribute a baseline.
//
// It passes a NIL ledger on purpose. The baseline is what the account costs
// today at on-demand rates, which is what a commitment is applied *to*; asking
// for it while netting against the commitments would be circular.
func usageLinesOf(d domain.Domain, now time.Time) []kcommit.UsageLine {
	if u, ok := d.(usageLiner); ok {
		return u.UsageLines(now, nil)
	}
	if c, ok := d.(*domain.Composite); ok {
		var out []kcommit.UsageLine
		for _, p := range c.Parts() {
			out = append(out, usageLinesOf(p.Domain, now)...)
		}
		return out
	}
	return nil
}

func parseNow(s string) (time.Time, error) {
	if s == "" {
		// The clock is read exactly once, here, and passed down. Nothing
		// below this line calls time.Now.
		return time.Now(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--now: %w", err)
	}
	return t, nil
}

// wantedKinds resolves --domain into a set, defaulting to every kind this
// binary can build.
func wantedKinds(sel []string) (map[domain.Kind]bool, error) {
	buildable := map[domain.Kind]bool{
		domain.EC2:        true,
		domain.ECSFargate: true,
		domain.Lambda:     true,
		domain.K8sFargate: true,
		domain.RDS:        true,
	}
	if len(sel) == 0 {
		return buildable, nil
	}
	out := map[domain.Kind]bool{}
	for _, s := range sel {
		k := domain.Kind(strings.TrimSpace(s))
		if !k.Valid() {
			return nil, fmt.Errorf("--domain %q: unknown domain (known: %s)",
				s, strings.Join(kindNames(), ", "))
		}
		if !buildable[k] {
			return nil, fmt.Errorf("--domain %q: known but not wired into this binary "+
				"(see cmd/FINDINGS.md); wired: %s", s, strings.Join(wiredNames(buildable), ", "))
		}
		out[k] = true
	}
	return out, nil
}

func wiredNames(m map[domain.Kind]bool) []string {
	var out []string
	for _, k := range domain.Kinds() {
		if m[k] {
			out = append(out, string(k))
		}
	}
	return out
}

func loadInventory(path string) (*kcommit.Inventory, error) {
	if path == "" {
		return nil, nil
	}
	inv, err := kcommit.LoadInventoryFile(path)
	if err != nil {
		return nil, fmt.Errorf("--commitments %s: %w", path, err)
	}
	return inv, nil
}

// loadDomainSnapshot reads one recorded domain snapshot.
func loadDomainSnapshot(path, scope string) (*domain.Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--snapshot: %w", err)
	}
	var snap domain.Snapshot
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		// Unknown fields are rejected rather than ignored: a snapshot written
		// by a newer collector may carry evidence this build cannot read, and
		// acting on the half we understood is how a partial view becomes a
		// wrong recommendation.
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !snap.Domain.Valid() {
		return nil, fmt.Errorf("%s: snapshot names unknown domain %q (known: %s)",
			path, snap.Domain, strings.Join(kindNames(), ", "))
	}
	if snap.Scope == "" {
		snap.Scope = scope
	}
	return &snap, nil
}

// loadClusterSnapshot reads a model.ClusterSnapshot — the format
// `kilter analyze --dump-snapshot` and `kilter simulate` already use — and
// wraps it for the k8s-fargate domain.
func loadClusterSnapshot(path, scope string) (*domain.Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--kube-snapshot: %w", err)
	}
	var cs model.ClusterSnapshot
	if err := json.Unmarshal(raw, &cs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s := scope
	if cs.ClusterID != "" {
		s = cs.ClusterID
	}
	return &domain.Snapshot{
		Domain:    domain.K8sFargate,
		Scope:     s,
		Timestamp: cs.Timestamp,
		Cluster:   &cs,
	}, nil
}

// ---------------------------------------------------------------- list

func runDomainsList(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("domains list", flag.ExitOnError)
	var df domainFlags
	df.bind(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rt, err := buildRuntime(&df)
	if err != nil {
		return err
	}
	health := rt.Registry.Health(rt.Now)
	if df.jsonOut {
		rows := make([]map[string]any, 0, len(health))
		for _, h := range health {
			rows = append(rows, map[string]any{
				"health":     h,
				"actuatable": rt.Registry.CanActuate(h.Kind),
			})
		}
		return writeJSON(w, map[string]any{
			"at": rt.Now.UTC(), "domains": rows, "warnings": rt.Warnings,
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "kilter domains — %s\n\n", rt.Now.UTC().Format(time.RFC3339))
	if len(health) == 0 {
		b.WriteString("  no domains registered\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	t := &table{header: []string{"DOMAIN", "READY", "ACTUATION", "TARGETS", "STATE"}}
	for _, h := range health {
		t.add(string(h.Kind), yesNo(h.Ready), actuationLabel(rt.Registry, h),
			fmt.Sprintf("%d", h.Targets), reasonOrOK(h))
	}
	b.WriteString(t.render("  "))
	writeWarnings(&b, rt.Warnings)
	_, err = io.WriteString(w, b.String())
	return err
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func actuationLabel(reg *domain.Registry, h domain.Health) string {
	switch {
	case h.ReportOnly:
		return "report-only"
	case !reg.CanActuate(h.Kind):
		return "no actuator"
	default:
		return "wired"
	}
}

func reasonOrOK(h domain.Health) string {
	if h.Reason == "" {
		return "ok"
	}
	return h.Reason
}

// ---------------------------------------------------------------- report

func runDomainsReport(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("domains report", flag.ExitOnError)
	var df domainFlags
	df.bind(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rt, err := buildRuntime(&df)
	if err != nil {
		return err
	}

	// Summarize runs recommend + refusals across every registered domain and
	// rolls up the money. Every term in ClaimableMonthlyUSD came out of a
	// domain's commitment waterfall through the ledger built above.
	rep := domain.Summarize(rt.Now, rt.Registry, rt.Ledger)
	if err := rep.Validate(); err != nil {
		// A report that violates its own invariants is a bug in a domain, and
		// the honest response is to fail loudly rather than print a number
		// somebody might put in a business case.
		return fmt.Errorf("aggregate report failed validation (this is a bug): %w", err)
	}
	// --rds-detail adds pkg/rds's own report. It is a SECOND rendering of the
	// same findings, and it earns its place because the layouts disagree on
	// purpose: the aggregate leads with money and lists refusals under it,
	// while pkg/rds leads with the refusals and puts the money second. In this
	// domain the refusal IS the finding, and a reader who sees a dollar figure
	// first reads the refusal as a caveat on a recommendation that does not
	// exist.
	var rdsReport *krds.Report
	if df.rdsDetail && rt.rds != nil {
		rdsReport = rt.rds.Report(rt.Now, rt.Ledger)
		if rdsReport != nil {
			if err := rdsReport.Validate(); err != nil {
				return fmt.Errorf("rds report failed validation (this is a bug): %w", err)
			}
		}
	}

	if df.jsonOut {
		out := map[string]any{"report": rep, "warnings": rt.Warnings}
		if rdsReport != nil {
			out["rds"] = rdsReport
		}
		return writeJSON(w, out)
	}
	if err := rep.WriteText(w); err != nil {
		return err
	}
	if rdsReport != nil {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		if err := rdsReport.WriteText(w); err != nil {
			return err
		}
	}
	var b strings.Builder
	writeWarnings(&b, rt.Warnings)
	_, err = io.WriteString(w, b.String())
	return err
}

// ---------------------------------------------------------------- plan

func runDomainsPlan(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("domains plan", flag.ExitOnError)
	var df domainFlags
	df.bind(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rt, err := buildRuntime(&df)
	if err != nil {
		return err
	}
	g, err := buildGuard(&df, rt.Now)
	if err != nil {
		return err
	}

	recs := rt.Registry.Recommend(rt.Now, rt.Ledger)
	plans := rt.Registry.BuildPlans(recs, g)
	if df.jsonOut {
		return writeJSON(w, map[string]any{
			"at": rt.Now.UTC(), "plans": plans, "warnings": rt.Warnings,
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "kilter domains plan — %s\n\n", rt.Now.UTC().Format(time.RFC3339))
	if len(plans) == 0 {
		b.WriteString("  no domains registered\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	for _, p := range plans {
		fmt.Fprintf(&b, "  %s\n", p.Kind)
		switch {
		case p.RefusalCode != "":
			// A refusal is the product. It is printed with the same weight as
			// a plan, and its code is stable enough to grep for.
			fmt.Fprintf(&b, "    refused (%s): %s\n", p.RefusalCode, p.Refusal)
		default:
			fmt.Fprintf(&b, "    %d step(s), %d disruptive, fingerprint %s\n",
				len(p.Steps), p.Disruptive(), p.Fingerprint)
			for _, s := range p.Steps {
				fmt.Fprintf(&b, "      %2d. %-10s %-40s %s -> %s\n",
					s.Seq, s.Action, s.Target.ID, s.From.Canonical(), s.To.Canonical())
			}
		}
		if p.Suppressed > 0 {
			fmt.Fprintf(&b, "    %d recommendation(s) withheld: reported, never applied\n", p.Suppressed)
		}
		if len(p.Steps) > 0 && !p.Actuatable {
			// The distinction that keeps this honest: steps exist for review,
			// and no actuator exists to run them.
			fmt.Fprintf(&b, "    NOT RUNNABLE: no actuator is wired for %s in this build\n", p.Kind)
		}
	}
	writeWarnings(&b, rt.Warnings)
	_, err = io.WriteString(w, b.String())
	return err
}

// buildGuard assembles the guardrail context a plan is built under.
func buildGuard(df *domainFlags, now time.Time) (domain.Guard, error) {
	g := domain.Guard{
		Now:         now,
		Freeze:      df.freeze,
		BreakerOpen: df.breakerOpen,
		MaxSteps:    df.maxSteps,
	}
	if df.window != "" {
		w, err := guard.ParseWindows(df.window)
		if err != nil {
			return domain.Guard{}, fmt.Errorf("--window: %w", err)
		}
		g.Windows = w
	}
	return g, nil
}

func writeWarnings(b *strings.Builder, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	b.WriteString("\n  Collection warnings\n")
	for _, w := range warnings {
		fmt.Fprintf(b, "    %s\n", w)
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
