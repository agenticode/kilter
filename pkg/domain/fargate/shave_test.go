package fargate

import (
	"math"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// cliff is the §4.1.1 fixture: 1 vCPU / 8 GB requested (billed 2 vCPU / 9 GB)
// against a 7 GiB peak, i.e. a pod whose 256 MB of overhead costs it 59 %.
func cliff() wl {
	return wl{name: "api", containers: []ctr{{
		name: "app", cpuReq: 1000, memReq: 8 * gib, cpuUse: 400, memUse: 7 * gib,
	}}}
}

// TestBoundaryShaveGates is the suppression matrix. The shave is the one lever
// here that can take a healthy pod down, so each gate is asserted to VETO on its
// own, against a positive control that differs only in that gate.
//
// "A shave that saves $2/mo and OOMs a pod is a net loss" — so the failure mode
// these cases pin is the expensive one: emitting on thin evidence.
func TestBoundaryShaveGates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		samples int
		window  time.Duration
		at      time.Duration // offset from `now` at which Recommend is called
		mutate  func(*Config)
		wl      func() wl
		want    bool // expect a recommendation
		why     string
	}{
		{
			name:    "positive control: ample history, quiet workload",
			samples: 300, window: 72 * time.Hour, wl: cliff, want: true,
		},
		{
			name:    "too few samples",
			samples: 100, window: 72 * time.Hour, wl: cliff, want: false,
			why: "100 samples is below the 120-sample gate",
		},
		{
			name:    "samples exactly at the gate but window too short",
			samples: 300, window: 12 * time.Hour, wl: cliff, want: false,
			why: "12h of observation is below the 24h gate",
		},
		{
			name:    "both hard gates cleared but confidence too low",
			samples: 130, window: 25 * time.Hour, wl: cliff, want: false,
			why: "history-depth 0.54 × window-span 0.52 = 0.28, below the 0.80 threshold",
		},
		{
			name:    "comfortably above every gate",
			samples: 220, window: 45 * time.Hour, wl: cliff, want: true,
		},
		{
			name:    "stale data: the newest sample is older than the freshness horizon",
			samples: 300, window: 72 * time.Hour, at: 3 * time.Hour, wl: cliff, want: false,
			why: "freshness decays to 0 past MaxSampleAge, vetoing confidence",
		},
		{
			name:    "a container ever seen OOM-killed is never shaved",
			samples: 300, window: 72 * time.Hour, want: false,
			why: "an OOM is permanent evidence that the samples do not capture demand",
			wl: func() wl {
				w := cliff()
				w.containers[0].oom = true
				return w
			},
		},
		{
			name:    "the shave would land inside the noise band of the peak",
			samples: 300, window: 72 * time.Hour, want: false,
			why: "peak 7.6 GiB + 10 % band = 8.36 GiB, above the 7.75 GiB boundary",
			wl: func() wl {
				w := cliff()
				w.containers[0].memUse = 7*gib + 614*mib // 7.6 GiB
				return w
			},
		},
		{
			name:    "an init container sets the floor, so shaving the app cannot move the tier",
			samples: 300, window: 72 * time.Hour, want: false,
			why: "the effective request is max(app, init); init pins it at 8 GiB",
			wl: func() wl {
				w := cliff()
				w.init = model.Resources{MilliCPU: 1000, MemoryBytes: 8 * gib}
				return w
			},
		},
		{
			name:    "saving below the dollar floor is not worth a rolling restart",
			samples: 300, window: 72 * time.Hour, wl: cliff, want: false,
			why:    "MinShaveMonthlyUSD raised above the $32.79/mo this shave yields",
			mutate: func(c *Config) { c.MinShaveMonthlyUSD = 100 },
		},
		{
			name:    "a stricter confidence threshold vetoes an otherwise clean shave",
			samples: 220, window: 45 * time.Hour, wl: cliff, want: false,
			why:    "0.86 observed confidence is below a 0.95 threshold",
			mutate: func(c *Config) { c.MinShaveConfidence = 0.95 },
		},
		{
			name:    "a wider noise band vetoes an otherwise clean shave",
			samples: 300, window: 72 * time.Hour, wl: cliff, want: false,
			why:    "a 20 % band puts the floor at 8.4 GiB, above the boundary",
			mutate: func(c *Config) { c.NoiseBandFraction = 0.20 },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			muts := []func(*Config){noPolicyMoves}
			if tc.mutate != nil {
				muts = append(muts, tc.mutate)
			}
			d := newDomain(t, muts...)
			w := tc.wl()
			learn(t, d, cluster(now, tc.samples, tc.window, w))
			recs := d.Recommend(now.Add(tc.at), nil)
			if tc.want {
				rec := only(t, recs, w.ref())
				if got := rec.Proposed.Attr(AttrChange); got != ChangeBoundaryShave {
					t.Fatalf("change = %q, want a boundary shave", got)
				}
			} else {
				if len(recs) != 0 {
					t.Fatalf("shave emitted despite the gate (%s):%s", tc.why, describe(recs))
				}
			}
		})
	}
}

// TestShaveNeverProposesBelowThePeakPlusBand sweeps peaks across a tier
// boundary and asserts the two properties that make a shave survivable: the
// proposal always clears the observed peak by the noise band, and it never
// increases anything.
func TestShaveNeverProposesBelowThePeakPlusBand(t *testing.T) {
	for peakMiB := int64(1000); peakMiB <= 8000; peakMiB += 137 {
		d := newDomain(t, noPolicyMoves)
		w := cliff()
		w.containers[0].memUse = peakMiB * mib
		learn(t, d, cluster(now, 300, 72*time.Hour, w))

		recs := d.Recommend(now, nil)
		if len(recs) == 0 {
			continue // vetoed; the gate tests cover why
		}
		rec := recs[0]
		cs, err := Containers(rec.Proposed)
		if err != nil {
			t.Fatal(err)
		}
		floor := peakMiB * mib
		floor += int64(math.Ceil(0.10 * float64(floor))) // the default band; σ is 0 here
		if cs[0].Requests.MemoryBytes < floor {
			t.Fatalf("peak %d MiB: proposed %d bytes is inside the noise band (floor %d)",
				peakMiB, cs[0].Requests.MemoryBytes, floor)
		}
		if cs[0].Requests.MemoryBytes > 8*gib || cs[0].Requests.MilliCPU > 1000 {
			t.Fatalf("peak %d MiB: a shave increased a request: %+v", peakMiB, cs[0].Requests)
		}
		if rec.ProposedHourlyUSD >= rec.CurrentHourlyUSD {
			t.Fatalf("peak %d MiB: shave does not save money", peakMiB)
		}
	}
}

// TestNoiseBandWidensWithVolatility pins the σ half of the band: a workload
// whose memory swings widely needs more headroom than a flat 10 %, and the
// floor is the wider of the two.
func TestNoiseBandWidensWithVolatility(t *testing.T) {
	d := newDomain(t)
	quiet := &stat{Samples: 300, PeakBytes: 4 * gib, PeakAt: now, LastBytes: 4 * gib}
	// Welford accumulators for a series with σ = 512 MiB.
	volatile := &stat{Samples: 300, PeakBytes: 4 * gib, PeakAt: now, LastBytes: 4 * gib,
		Mean: float64(2 * gib), M2: float64(512*mib) * float64(512*mib) * 299}

	quietFloor := d.requiredMemory(quiet, now)
	volatileFloor := d.requiredMemory(volatile, now)
	if want := 4*gib + int64(math.Ceil(0.10*float64(4*gib))); quietFloor != want {
		t.Fatalf("quiet floor = %d, want peak+10%% = %d", quietFloor, want)
	}
	if volatileFloor <= quietFloor {
		t.Fatalf("volatility did not widen the band: %d vs %d", volatileFloor, quietFloor)
	}
	if want := 4*gib + 3*512*mib; volatileFloor != want {
		t.Fatalf("volatile floor = %d, want peak+3σ = %d", volatileFloor, want)
	}
	// σ is a sample standard deviation: one sample cannot produce one.
	if got := (&stat{Samples: 1, PeakBytes: gib, PeakAt: now}).stddev(); got != 0 {
		t.Errorf("stddev from one sample = %v, want 0", got)
	}
}

// TestObservedPeakRelaxesWithAge: an old peak should not pin a workload
// forever, but it must hold long enough that a weekly cycle re-arms it. The
// relaxation is pkg/decision's, shared with the OOM floor.
func TestObservedPeakRelaxesWithAge(t *testing.T) {
	d := newDomain(t)
	s := &stat{Samples: 300, PeakBytes: 8 * gib, PeakAt: now, LastBytes: 1 * gib}
	if got := s.effectivePeak(now, d.cfg.Floors); got != 8*gib {
		t.Fatalf("fresh peak = %d, want the full %d", got, 8*gib)
	}
	if got := s.effectivePeak(now.Add(13*24*time.Hour), d.cfg.Floors); got != 8*gib {
		t.Fatalf("peak relaxed inside the hold window: %d", got)
	}
	old := s.effectivePeak(now.Add(120*24*time.Hour), d.cfg.Floors)
	if old >= 8*gib || old < 1*gib {
		t.Fatalf("aged peak = %d, want it between the recent level and the peak", old)
	}
	// Never below what is currently observed.
	if s.effectivePeak(now.Add(10*365*24*time.Hour), d.cfg.Floors) < 1*gib {
		t.Fatal("peak decayed below the observed level")
	}
	// No peak, no floor.
	if got := (&stat{}).effectivePeak(now, d.cfg.Floors); got != 0 {
		t.Errorf("empty stat produced floor %d", got)
	}
	if got := (*stat)(nil).effectivePeak(now, d.cfg.Floors); got != 0 {
		t.Errorf("nil stat produced floor %d", got)
	}
}

// TestGarbageUsageCannotLowerTheFloor: a collector emitting negative memory or
// zero timestamps is broken, and dropping the samples (rather than clamping
// them) is what stops broken telemetry from authorizing a shave.
func TestGarbageUsageCannotLowerTheFloor(t *testing.T) {
	s := &stat{}
	s.observe(4*gib, now)
	before := *s
	s.observe(-1, now)
	s.observe(1*gib, time.Time{})
	if s.Samples != before.Samples || s.PeakBytes != before.PeakBytes || s.Mean != before.Mean {
		t.Fatalf("garbage sample was absorbed: %+v vs %+v", *s, before)
	}
}
