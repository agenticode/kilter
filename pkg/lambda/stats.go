package lambda

import (
	"math"
	"sort"
	"time"
)

// MemoryPoint is everything measured at ONE memory setting. It is the atom of
// this package's honesty: a cost claim needs a MemoryPoint at the setting it
// claims about, and no MemoryPoint can be manufactured from another one.
//
// Warm and cold invocations are kept apart throughout. Duration statistics are
// warm-only, because init time is a different phase billed under different
// rules; mixing them makes two memory settings with different cold-start mixes
// look like they have different durations when only the mix differed.
type MemoryPoint struct {
	MemoryMB int64 `json:"memoryMB"`
	Warm     int   `json:"warm"`
	Cold     int   `json:"cold"`

	// MeanBilledMS is the mean BILLED duration over warm invocations. Billed,
	// not measured: AWS's rounding is part of the bill, and for sub-millisecond
	// functions it is most of it.
	MeanBilledMS float64 `json:"meanBilledMS"`
	P95BilledMS  float64 `json:"p95BilledMS"`
	MaxBilledMS  float64 `json:"maxBilledMS"`
	// MeanDurationMS is the mean measured (unrounded) warm handler duration,
	// carried for the human, never for the bill.
	MeanDurationMS float64 `json:"meanDurationMS"`
	// MeanInitMS is the mean initialization time over COLD invocations only.
	MeanInitMS float64 `json:"meanInitMS,omitempty"`

	// MaxMemoryUsedMB is the largest max-memory-used seen at this setting.
	MaxMemoryUsedMB int64 `json:"maxMemoryUsedMB"`
	// AtCeiling marks a setting where max-memory-used reached the configured
	// memory: the measurement may have been TRUNCATED by the limit rather than
	// bounded by demand, so the true peak here is unknown and at least this
	// large. A point at the ceiling is never a proposal target.
	AtCeiling bool `json:"atCeiling,omitempty"`

	First time.Time `json:"first,omitzero"`
	Last  time.Time `json:"last,omitzero"`
}

// Samples is the total number of invocations measured at this setting.
func (p MemoryPoint) Samples() int { return p.Warm + p.Cold }

// GBSecondsPerInvocation is the billable quantity of one warm invocation here.
func (p MemoryPoint) GBSecondsPerInvocation() float64 {
	return GBSeconds(p.MemoryMB, p.MeanBilledMS)
}

// aggregate folds REPORT records into one MemoryPoint per distinct memory
// setting, sorted by memory. Grouping by Memory Size is what makes a retuned
// function — or a deliberate power-tuning trial — into comparable evidence.
func aggregate(recs []ReportRecord, ceilingRatio float64) []MemoryPoint {
	type acc struct {
		p           MemoryPoint
		billed      []float64
		durationSum float64
		initSum     float64
	}
	byMem := map[int64]*acc{}
	for _, r := range recs {
		if r.MemorySizeMB <= 0 {
			continue
		}
		a := byMem[r.MemorySizeMB]
		if a == nil {
			a = &acc{p: MemoryPoint{MemoryMB: r.MemorySizeMB, First: r.At, Last: r.At}}
			byMem[r.MemorySizeMB] = a
		}
		if !r.At.IsZero() {
			if a.p.First.IsZero() || r.At.Before(a.p.First) {
				a.p.First = r.At
			}
			if r.At.After(a.p.Last) {
				a.p.Last = r.At
			}
		}
		if r.MaxMemoryUsedMB > a.p.MaxMemoryUsedMB {
			a.p.MaxMemoryUsedMB = r.MaxMemoryUsedMB
		}
		if r.AtCeiling(ceilingRatio) {
			a.p.AtCeiling = true
		}
		if r.Cold() {
			a.p.Cold++
			a.initSum += r.ColdOverheadMS()
			continue
		}
		a.p.Warm++
		a.billed = append(a.billed, r.BilledDurationMS)
		a.durationSum += r.DurationMS
		if r.BilledDurationMS > a.p.MaxBilledMS {
			a.p.MaxBilledMS = r.BilledDurationMS
		}
	}

	out := make([]MemoryPoint, 0, len(byMem))
	for _, a := range byMem {
		if a.p.Warm > 0 {
			var sum float64
			for _, b := range a.billed {
				sum += b
			}
			a.p.MeanBilledMS = sum / float64(a.p.Warm)
			a.p.MeanDurationMS = a.durationSum / float64(a.p.Warm)
			a.p.P95BilledMS = percentile(a.billed, 0.95)
		}
		if a.p.Cold > 0 {
			a.p.MeanInitMS = a.initSum / float64(a.p.Cold)
		}
		out = append(out, a.p)
	}
	// Sorted by memory setting: every downstream comparison, and every byte of
	// rendered output, is therefore independent of map iteration order.
	sort.Slice(out, func(i, j int) bool { return out[i].MemoryMB < out[j].MemoryMB })
	return out
}

// percentile is nearest-rank over a sorted copy. Nearest rank, not
// interpolation: an interpolated duration is a duration nobody measured, and
// this package does not act on durations nobody measured.
func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	switch {
	case p <= 0:
		return s[0]
	case p >= 1:
		return s[len(s)-1]
	}
	rank := int(math.Ceil(float64(len(s)) * p))
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

// Observation is everything the sizer read about one function, and everything
// it could not.
type Observation struct {
	// Window is the span the surviving REPORT records actually cover — the
	// evidence window, which may be much shorter than the window the collector
	// asked for.
	Window Window `json:"window"`
	// RateHours is the span used to convert observed invocations into a rate.
	RateHours float64 `json:"rateHours"`

	Records int `json:"records"`
	Dropped int `json:"dropped"`
	Warm    int `json:"warm"`
	Cold    int `json:"cold"`
	// ColdShare is cold ÷ total. Above [Config.MaxColdShare] the warm mean
	// stops describing the bill.
	ColdShare float64 `json:"coldShare"`

	// Invocations is the count the cost model scales by, and InvocationSource
	// says where it came from: the CloudWatch Invocations sum is authoritative,
	// the parsed record count is a floor (a log query may sample or truncate).
	Invocations        float64 `json:"invocations"`
	InvocationSource   string  `json:"invocationSource"`
	InvocationsPerHour float64 `json:"invocationsPerHour"`
	// ReportCoverage is parsed records ÷ invocations, capped at 1.
	ReportCoverage float64 `json:"reportCoverage"`

	// Points is one entry per measured memory setting, sorted by memory.
	Points []MemoryPoint `json:"points,omitempty"`
	// CurrentIndex indexes Points for the function's configured memory, or -1
	// when the current setting was never measured — in which case the current
	// bill itself is unmeasured.
	CurrentIndex int `json:"currentIndex"`

	// MaxMemoryUsedMB is the largest max-memory-used across every setting.
	MaxMemoryUsedMB int64 `json:"maxMemoryUsedMB"`
	// MemoryFloorMB is the smallest setting that clears it with headroom.
	MemoryFloorMB int64 `json:"memoryFloorMB"`
	// AtCeiling marks the truncation risk: the CURRENT setting's
	// max-memory-used sat at the configured memory.
	AtCeiling bool `json:"atCeiling,omitempty"`
	// TruncatedPoints counts settings whose measurement hit the ceiling.
	TruncatedPoints int `json:"truncatedPoints,omitempty"`

	ProvisionedConcurrency int64 `json:"provisionedConcurrency,omitempty"`
	// Partial marks a metric series CloudWatch did not deliver in full.
	Partial bool `json:"partial,omitempty"`
}

// Current returns the measured point at the function's configured memory.
func (o Observation) Current() (MemoryPoint, bool) {
	if o.CurrentIndex < 0 || o.CurrentIndex >= len(o.Points) {
		return MemoryPoint{}, false
	}
	return o.Points[o.CurrentIndex], true
}

// UsablePoints returns the points with at least minSamples warm invocations
// and no truncated memory measurement — the settings a cost claim may rest on.
func (o Observation) UsablePoints(minSamples int) []MemoryPoint {
	var out []MemoryPoint
	for _, p := range o.Points {
		if p.Warm >= minSamples {
			out = append(out, p)
		}
	}
	return out
}

// PointAt returns the measured point at a memory setting.
func (o Observation) PointAt(memoryMB int64) (MemoryPoint, bool) {
	i := sort.Search(len(o.Points), func(i int) bool { return o.Points[i].MemoryMB >= memoryMB })
	if i < len(o.Points) && o.Points[i].MemoryMB == memoryMB {
		return o.Points[i], true
	}
	return MemoryPoint{}, false
}

// observe builds the Observation for one target. It is pure and reads no clock.
func observe(t Target, snapWindow Window, cfg Config) Observation {
	obs := Observation{CurrentIndex: -1}
	for _, d := range t.Drops {
		// Non-REPORT lines are the normal contents of a log group, not a
		// failure to parse evidence; counting them as drops would make every
		// healthy function look damaged.
		if d.Code == DropNotReport {
			continue
		}
		obs.Dropped += d.Count
	}
	obs.Records = len(t.Reports)
	obs.ProvisionedConcurrency = t.Function.ProvisionedConcurrency

	for _, s := range t.Series {
		if s.Partial {
			obs.Partial = true
		}
	}
	// Provisioned concurrency is also visible as a metric: a function whose
	// configuration the collector could not read still gets caught here.
	if s, ok := t.SeriesFor(MetricProvisionedConcurrentExecutions); ok {
		if m, has := s.Max(); has && m > 0 && obs.ProvisionedConcurrency == 0 {
			obs.ProvisionedConcurrency = int64(math.Ceil(m))
		}
	}

	if len(t.Reports) > 0 {
		obs.Window = Window{Start: t.Reports[0].At, End: t.Reports[len(t.Reports)-1].At}
	}
	obs.Points = aggregate(t.Reports, cfg.CeilingRatio)
	for i, p := range obs.Points {
		obs.Warm += p.Warm
		obs.Cold += p.Cold
		if p.MaxMemoryUsedMB > obs.MaxMemoryUsedMB {
			obs.MaxMemoryUsedMB = p.MaxMemoryUsedMB
		}
		if p.AtCeiling {
			obs.TruncatedPoints++
		}
		if p.MemoryMB == t.Function.MemoryMB {
			obs.CurrentIndex = i
			obs.AtCeiling = p.AtCeiling
		}
	}
	if total := obs.Warm + obs.Cold; total > 0 {
		obs.ColdShare = float64(obs.Cold) / float64(total)
	}
	// A truncated measurement is a lower bound on demand, so it belongs in the
	// floor: including it can only push the floor UP, which is the safe
	// direction. What it must not do is license a downsize — that is the
	// memory-at-ceiling refusal's job.
	obs.MemoryFloorMB = MemoryFloorMB(obs.MaxMemoryUsedMB, cfg.MemHeadroom, cfg.MemoryStepMB)

	// Invocation rate. CloudWatch's Invocations sum is authoritative; the
	// parsed record count is a floor, because a Logs query can sample, page out
	// or be filtered.
	obs.Invocations = float64(obs.Records)
	obs.InvocationSource = "report-count"
	if s, ok := t.SeriesFor(MetricInvocations); ok && len(s.Points) > 0 {
		if sum := s.Sum(); sum > 0 {
			obs.Invocations = sum
			obs.InvocationSource = SourceCloudWatch
		}
	}
	obs.RateHours = snapWindow.Hours()
	if obs.RateHours <= 0 {
		obs.RateHours = obs.Window.Hours()
	}
	if obs.RateHours > 0 {
		obs.InvocationsPerHour = obs.Invocations / obs.RateHours
	}
	if obs.Invocations > 0 {
		obs.ReportCoverage = math.Min(1, float64(obs.Records)/obs.Invocations)
	}
	return obs
}
