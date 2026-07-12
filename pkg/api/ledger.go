package api

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/actuate"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// LedgerEntry is one executed plan, recorded for audit and verification.
// Together with the cost timeline it makes savings *checkable*: the operator
// sees exactly what ran, what it claimed, and what the bill actually did.
type LedgerEntry struct {
	At      time.Time `json:"at"`
	Cluster string    `json:"cluster"`
	Mode    string    `json:"mode"` // dry-run | apply
	// Plan identity + money claims at execution time.
	Fingerprint             string  `json:"fingerprint"`
	Risk                    string  `json:"risk"`
	CostBeforeHourlyUSD     float64 `json:"costBeforeHourlyUSD"`
	ProjectedHourlyUSD      float64 `json:"projectedHourlyUSD"`
	ProjectedMonthlySavings float64 `json:"projectedMonthlySavings"`
	// What actually happened, step by step (includes From values → undo).
	Steps   []actuate.StepStatus `json:"steps"`
	Done    int                  `json:"done"`
	Failed  int                  `json:"failed"`
	Aborted bool                 `json:"aborted,omitempty"`
}

// CostPoint is one observation of the cluster's priced hourly cost.
type CostPoint struct {
	At        time.Time `json:"at"`
	HourlyUSD float64   `json:"hourlyUSD"`
}

// LedgerReport is the verification view: actions + the measured cost curve.
type LedgerReport struct {
	Entries      []LedgerEntry `json:"entries"`
	CostTimeline []CostPoint   `json:"costTimeline"`
	// RealizedMonthlyUSD compares the measured cost before the first applied
	// action with the latest measurement: (before − now) × 730. Transparent
	// math over observable numbers — judge it against your invoice.
	RealizedMonthlyUSD float64 `json:"realizedMonthlyUSD"`
	Method             string  `json:"method"`
}

const (
	ledgerLimit   = 200
	costHistLimit = 2016 // ~1 week at 5-minute snapshots
)

// ledgerState is per-cluster audit memory (persisted via store when present).
type ledgerState struct {
	mu       sync.Mutex
	entries  []LedgerEntry
	costHist []CostPoint
}

func (l *ledgerState) addCost(at time.Time, hourly float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.costHist = append(l.costHist, CostPoint{At: at, HourlyUSD: hourly})
	if len(l.costHist) > costHistLimit {
		l.costHist = l.costHist[len(l.costHist)-costHistLimit:]
	}
}

func (l *ledgerState) add(e LedgerEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	if len(l.entries) > ledgerLimit {
		l.entries = l.entries[len(l.entries)-ledgerLimit:]
	}
}

func (l *ledgerState) report() LedgerReport {
	l.mu.Lock()
	defer l.mu.Unlock()
	rep := LedgerReport{
		Entries:      append([]LedgerEntry(nil), l.entries...),
		CostTimeline: append([]CostPoint(nil), l.costHist...),
		Method:       "realized = (measured hourly cost before first applied action − latest measured hourly cost) × 730",
	}
	sort.Slice(rep.Entries, func(i, j int) bool { return rep.Entries[i].At.After(rep.Entries[j].At) })
	var firstApply *LedgerEntry
	for i := len(l.entries) - 1; i >= 0; i-- { // oldest applied entry
		if l.entries[i].Mode == "apply" && l.entries[i].Done > 0 {
			firstApply = &l.entries[i]
		}
	}
	if firstApply != nil && len(l.costHist) > 0 {
		latest := l.costHist[len(l.costHist)-1]
		rep.RealizedMonthlyUSD = (firstApply.CostBeforeHourlyUSD - latest.HourlyUSD) * pricing.HoursPerMonth
	}
	return rep
}

// ledgerFor returns (creating) a cluster's ledger. Caller must not hold b.mu.
func (b *Brain) ledgerFor(cluster string) *ledgerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgers[cluster]
	if l == nil {
		l = &ledgerState{}
		b.ledgers[cluster] = l
	}
	return l
}

// ---- approvals ----

// Approval marks a plan fingerprint as human-approved for execution.
type Approval struct {
	Fingerprint string    `json:"fingerprint"`
	ApprovedAt  time.Time `json:"approvedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// approvalTTL bounds how long an approval stays valid: the cluster drifts,
// and yesterday's approved plan is not today's plan.
const approvalTTL = 24 * time.Hour

type approvalState struct {
	mu  sync.Mutex
	byF map[string]Approval
}

func (a *approvalState) approve(fp string, now time.Time) Approval {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap := Approval{Fingerprint: fp, ApprovedAt: now, ExpiresAt: now.Add(approvalTTL)}
	a.byF[fp] = ap
	return ap
}

func (a *approvalState) approved(fp string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap, ok := a.byF[fp]
	if !ok {
		return false
	}
	if now.After(ap.ExpiresAt) {
		delete(a.byF, fp)
		return false
	}
	return true
}

func (a *approvalState) list(now time.Time) []Approval {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Approval
	for fp, ap := range a.byF {
		if now.After(ap.ExpiresAt) {
			delete(a.byF, fp)
			continue
		}
		out = append(out, ap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ApprovedAt.After(out[j].ApprovedAt) })
	return out
}

func (b *Brain) approvalsFor(cluster string) *approvalState {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.approvals[cluster]
	if a == nil {
		a = &approvalState{byF: map[string]Approval{}}
		b.approvals[cluster] = a
	}
	return a
}

// registerTrustRoutes adds ledger/report/approval endpoints.
func (b *Brain) registerTrustRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/clusters/{id}/reports", b.authWrite(func(w http.ResponseWriter, r *http.Request) {
		var e LedgerEntry
		if err := decodeBody(w, r, b.cfg.MaxBodyBytes, &e); err != nil {
			return
		}
		e.Cluster = r.PathValue("id")
		if e.At.IsZero() {
			e.At = time.Now().UTC()
		}
		b.ledgerFor(e.Cluster).add(e)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "recorded"})
	}))
	mux.HandleFunc("GET /api/v1/clusters/{id}/ledger", b.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, b.ledgerFor(r.PathValue("id")).report())
	}))
	mux.HandleFunc("GET /api/v1/clusters/{id}/approvals", b.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"approvals": b.approvalsFor(r.PathValue("id")).list(time.Now()),
		})
	}))
	mux.HandleFunc("POST /api/v1/clusters/{id}/approvals", b.authWrite(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Fingerprint string `json:"fingerprint"`
		}
		if err := decodeBody(w, r, 1<<20, &req); err != nil {
			return
		}
		if len(req.Fingerprint) < 8 {
			writeErr(w, http.StatusBadRequest, errBadFingerprint)
			return
		}
		ap := b.approvalsFor(r.PathValue("id")).approve(req.Fingerprint, time.Now())
		writeJSON(w, http.StatusOK, ap)
	}))
}

// Approved reports whether a fingerprint is currently approved for a cluster.
func (b *Brain) Approved(cluster, fingerprint string) bool {
	return b.approvalsFor(cluster).approved(fingerprint, time.Now())
}

// Ledger returns the cluster's audit report.
func (b *Brain) Ledger(cluster string) LedgerReport {
	return b.ledgerFor(cluster).report()
}

var errBadFingerprint = &fingerprintErr{}

type fingerprintErr struct{}

func (*fingerprintErr) Error() string { return "fingerprint must be at least 8 characters" }

var _ = model.Insight{} // keep import stable across edits
