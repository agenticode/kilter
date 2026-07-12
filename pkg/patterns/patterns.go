// Package patterns classifies workload behavior online and adapts sizing
// policy per class — the AIOps layer between raw telemetry and decisions.
//
// Every container gets a lightweight detector (bounded ring buffer, O(1)
// ingest). From it we derive interpretable features — coefficient of
// variation, 24h-lag autocorrelation, trend slope, spike rate, idle
// fraction — and classify the workload:
//
//	steady   flat demand           → tight sizing (small headroom)
//	diurnal  daily seasonality     → normal sizing (histogram covers peaks)
//	bursty   heavy-tailed spikes   → generous headroom, higher percentile
//	batch    mostly idle, bursts   → size for active windows, lower percentile
//	growing  sustained up-trend    → predictive headroom for near-term growth
//
// Classes are re-evaluated continuously: a workload that changes behavior in
// production migrates classes and its policy follows — self-learning, no
// offline training loop required. For cold starts (no samples yet) the
// package exposes priors derived from published production-trace research
// (Google Borg, Alibaba cluster traces), clearly labeled as priors.
package patterns

import (
	"fmt"
	"math"
	"time"
)

// Class is a learned workload behavior category.
type Class string

const (
	ClassUnknown Class = "unknown"
	ClassSteady  Class = "steady"
	ClassDiurnal Class = "diurnal"
	ClassBursty  Class = "bursty"
	ClassBatch   Class = "batch"
	ClassGrowing Class = "growing"
)

// Features are the interpretable signals behind a classification.
// Explainability is an AIOps requirement: every automated decision must be
// able to say why.
type Features struct {
	Samples     int     `json:"samples"`
	Mean        float64 `json:"mean"`
	CV          float64 `json:"cv"`          // stddev / mean
	AutoCorr24h float64 `json:"autoCorr24h"` // lag-24h autocorrelation, -1..1
	TrendPerDay float64 `json:"trendPerDay"` // linear slope as fraction of mean per day
	SpikeRate   float64 `json:"spikeRate"`   // fraction of samples > 2× mean
	IdleFrac    float64 `json:"idleFrac"`    // fraction of samples < 10% of p95
	MedianRatio float64 `json:"medianRatio"` // median / p95: near 0 = truly idle between runs
}

// capacity: 48h at 5-minute samples.
const ringCap = 576

// minClassifySamples before leaving ClassUnknown.
const minClassifySamples = 48

// Detector ingests usage samples for one series. Not safe for concurrent
// use; owners (recommender state) serialize access.
type Detector struct {
	vals  [ringCap]float64
	times [ringCap]int64 // unix seconds
	n     int            // total ever added
	head  int            // next write position
}

// Add ingests one sample.
func (d *Detector) Add(t time.Time, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return
	}
	d.vals[d.head] = v
	d.times[d.head] = t.Unix()
	d.head = (d.head + 1) % ringCap
	d.n++
}

// size returns how many valid samples are in the ring.
func (d *Detector) size() int {
	if d.n < ringCap {
		return d.n
	}
	return ringCap
}

// series returns samples oldest→newest.
func (d *Detector) series() ([]float64, []int64) {
	sz := d.size()
	vals := make([]float64, sz)
	times := make([]int64, sz)
	start := 0
	if d.n >= ringCap {
		start = d.head
	}
	for i := 0; i < sz; i++ {
		idx := (start + i) % ringCap
		vals[i] = d.vals[idx]
		times[i] = d.times[idx]
	}
	return vals, times
}

// Analyze computes features and a classification.
func (d *Detector) Analyze() (Class, Features) {
	vals, times := d.series()
	f := Features{Samples: len(vals)}
	if len(vals) < minClassifySamples {
		return ClassUnknown, f
	}

	// Mean / stddev / spike rate / p95-based idle fraction.
	var sum float64
	for _, v := range vals {
		sum += v
	}
	f.Mean = sum / float64(len(vals))
	var sq float64
	spikes := 0
	for _, v := range vals {
		dv := v - f.Mean
		sq += dv * dv
		if f.Mean > 0 && v > 2*f.Mean {
			spikes++
		}
	}
	std := math.Sqrt(sq / float64(len(vals)))
	if f.Mean > 0 {
		f.CV = std / f.Mean
	}
	f.SpikeRate = float64(spikes) / float64(len(vals))

	p95 := percentile(vals, 0.95)
	idle := 0
	for _, v := range vals {
		if v < 0.10*p95 {
			idle++
		}
	}
	f.IdleFrac = float64(idle) / float64(len(vals))
	if p95 > 0 {
		f.MedianRatio = percentile(vals, 0.5) / p95
	}

	// Trend: least-squares slope over time, normalized to fraction/day.
	f.TrendPerDay = slopePerDay(vals, times, f.Mean)

	// Diurnal: autocorrelation at 24h lag (needs ≥ ~36h of history).
	f.AutoCorr24h = autoCorrAtLag(vals, times, 24*3600)

	return classify(f), f
}

func classify(f Features) Class {
	switch {
	case f.Mean <= 0:
		return ClassBatch // never uses anything measurable outside bursts
	case f.TrendPerDay > 0.10 && f.Samples >= minClassifySamples:
		return ClassGrowing
	// Batch means truly idle between runs (median ≈ 0 vs peaks). A workload
	// with a real serving baseline plus spikes is bursty, not batch.
	case f.IdleFrac > 0.6 && f.MedianRatio < 0.02:
		return ClassBatch
	case f.AutoCorr24h > 0.5:
		return ClassDiurnal
	case f.CV > 1.0 || f.SpikeRate > 0.05:
		return ClassBursty
	default:
		return ClassSteady
	}
}

// Policy is the class-specific sizing posture applied on top of the
// operator's base config.
type Policy struct {
	CPUPercentile  float64
	CPUHeadroom    float64
	MemoryHeadroom float64
	Note           string
}

// PolicyFor returns the adaptive policy for a class. Base values are the
// operator's configured defaults; classes tighten or loosen them.
func PolicyFor(c Class, baseCPUPercentile, baseCPUHeadroom, baseMemHeadroom float64) Policy {
	p := Policy{CPUPercentile: baseCPUPercentile, CPUHeadroom: baseCPUHeadroom, MemoryHeadroom: baseMemHeadroom}
	switch c {
	case ClassSteady:
		p.CPUHeadroom = math.Max(1.05, baseCPUHeadroom*0.92)
		p.MemoryHeadroom = math.Max(1.10, baseMemHeadroom*0.95)
		p.Note = "steady: tight sizing"
	case ClassBursty:
		p.CPUPercentile = math.Min(0.99, baseCPUPercentile+0.03)
		p.CPUHeadroom = baseCPUHeadroom * 1.20
		p.MemoryHeadroom = baseMemHeadroom * 1.15
		p.Note = "bursty: extra headroom for heavy-tailed spikes"
	case ClassBatch:
		p.CPUPercentile = math.Max(0.85, baseCPUPercentile-0.05)
		p.Note = "batch: sized for active windows"
	case ClassGrowing:
		p.CPUHeadroom = baseCPUHeadroom * 1.15
		p.MemoryHeadroom = baseMemHeadroom * 1.20
		p.Note = "growing: predictive headroom for up-trend"
	case ClassDiurnal:
		p.Note = "diurnal: daily cycle covered by decaying histogram"
	default:
		p.Note = "unknown: defaults until enough history"
	}
	return p
}

// ---- Cold-start priors (trace-derived) ----

// Production-trace priors. Sources: Google Borg trace analyses report
// cluster utilization of 20–40% of allocation most of the time (2011 and
// 2019 traces); Alibaba co-located cluster traces show 40–50% CPU
// utilization with pronounced diurnal cycles. We use these only when a
// workload has no observed samples, always labeled as prior-based.
const (
	PriorUtilizationLow  = 0.20
	PriorUtilizationMid  = 0.40
	PriorUtilizationHigh = 0.55
)

// PriorWasteEstimate estimates, from requests alone, the plausible range of
// currently wasted request capacity — used by instant analyze before any
// metrics exist. Returns low/high fractions of requests likely reclaimable.
func PriorWasteEstimate() (low, high float64, source string) {
	return 1 - PriorUtilizationHigh, 1 - PriorUtilizationLow,
		"prior from Google Borg / Alibaba production traces (20–55% typical utilization)"
}

// ---- small math helpers ----

func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := make([]float64, len(vals))
	copy(cp, vals)
	// insertion-free selection: simple sort is fine at ring sizes
	sortFloats(cp)
	idx := int(p * float64(len(cp)-1))
	return cp[idx]
}

func sortFloats(a []float64) {
	// shell sort: tiny, no imports, fine for n ≤ 576
	for gap := len(a) / 2; gap > 0; gap /= 2 {
		for i := gap; i < len(a); i++ {
			tmp := a[i]
			j := i
			for ; j >= gap && a[j-gap] > tmp; j -= gap {
				a[j] = a[j-gap]
			}
			a[j] = tmp
		}
	}
}

// slopePerDay fits y = a + b·t and returns b normalized to fraction of mean
// per day. Returns 0 when the fit is meaningless.
func slopePerDay(vals []float64, times []int64, mean float64) float64 {
	if len(vals) < 2 || mean <= 0 {
		return 0
	}
	t0 := times[0]
	var sx, sy, sxx, sxy float64
	n := float64(len(vals))
	for i, v := range vals {
		x := float64(times[i]-t0) / 86400.0 // days
		sx += x
		sy += v
		sxx += x * x
		sxy += x * v
	}
	den := n*sxx - sx*sx
	if den <= 1e-12 {
		return 0
	}
	b := (n*sxy - sx*sy) / den
	return b / mean
}

// autoCorrAtLag computes autocorrelation at the given lag in seconds,
// pairing each sample with the nearest sample ~lag earlier. Requires
// coverage of at least 1.5× the lag; returns 0 otherwise.
func autoCorrAtLag(vals []float64, times []int64, lagSec int64) float64 {
	if len(vals) < minClassifySamples {
		return 0
	}
	span := times[len(times)-1] - times[0]
	if span < lagSec+lagSec/2 {
		return 0
	}
	var mean float64
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))

	// Two-pointer pairing on the sorted-by-time series.
	var num, den float64
	j := 0
	pairs := 0
	tol := lagSec / 8
	for i := range vals {
		target := times[i] - lagSec
		for j < len(vals)-1 && times[j] < target {
			j++
		}
		// nearest of j and j-1
		k := j
		if j > 0 && abs64(times[j-1]-target) < abs64(times[j]-target) {
			k = j - 1
		}
		if abs64(times[k]-target) > tol || k >= i {
			continue
		}
		num += (vals[i] - mean) * (vals[k] - mean)
		pairs++
	}
	if pairs < minClassifySamples/2 {
		return 0
	}
	for _, v := range vals {
		den += (v - mean) * (v - mean)
	}
	if den <= 1e-12 {
		return 0
	}
	// Scale den to the number of pairs actually used.
	den *= float64(pairs) / float64(len(vals))
	r := num / den
	if r > 1 {
		r = 1
	}
	if r < -1 {
		r = -1
	}
	return r
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// String renders features compactly for decision reasons.
func (f Features) String() string {
	return fmt.Sprintf("cv=%.2f ac24=%.2f trend=%+.0f%%/d spikes=%.0f%% idle=%.0f%%",
		f.CV, f.AutoCorr24h, f.TrendPerDay*100, f.SpikeRate*100, f.IdleFrac*100)
}
