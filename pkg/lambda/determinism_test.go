package lambda

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// A report must be a function of what was observed, not of the order it
// arrived in. This shuffles every input ordering the package touches — targets,
// log events within a function, metric series, metric datapoints and tags — and
// requires the serialized report to be byte-identical.
//
// Go randomizes map iteration deliberately, so a single missed sort shows up
// here as a flake rather than as a silent difference between two operators'
// screens.
func TestReportIsShuffleInvariant(t *testing.T) {
	build := func(reverse bool) []byte {
		t.Helper()
		var targets []Target
		for i, spec := range []struct {
			name string
			mem  int64
			pts  []point
			tags map[string]string
		}{
			{"alpha", 1024, []point{
				{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
				{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
			}, map[string]string{"team": "payments", "env": "prod", "cost-center": "42"}},
			{"beta", 512, []point{
				{memoryMB: 512, maxUsedMB: 512, billedMS: 90, n: 1200},
			}, map[string]string{"team": "search"}},
			{"gamma", 2048, []point{
				{memoryMB: 2048, maxUsedMB: 900, billedMS: 60, n: 800, coldEvery: 20, initMS: 350},
				{memoryMB: 1024, maxUsedMB: 900, billedMS: 200, n: 800, coldEvery: 20, initMS: 350},
			}, map[string]string{"team": "risk", "kilter.dev/mode": "recommend"}},
			{"delta", 1024, nil, nil},
		} {
			f := fn(spec.mem)
			f.Name = spec.name
			f.ARN = "arn:aws:lambda:us-east-1:123456789012:function:" + spec.name
			f.Tags = spec.tags
			if i%2 == 1 {
				f.Architecture = ArchARM
			}
			evs := events(testSpan, spec.pts...)
			series := []Series{
				invocationSeries(testSpan, 5000),
				{Metric: MetricErrors, Stat: "Sum", Source: SourceCloudWatch,
					Points: SyntheticMetric(testStart(), time.Hour, 6, 0)},
			}
			if reverse {
				evs = reverseEvents(evs)
				series = []Series{series[1], series[0]}
				for si := range series {
					series[si].Points = reversePoints(series[si].Points)
				}
			}
			targets = append(targets, target(f, evs, series...))
		}
		if reverse {
			for i, j := 0, len(targets)-1; i < j; i, j = i+1, j-1 {
				targets[i], targets[j] = targets[j], targets[i]
			}
		}
		s, err := NewSizer(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		snap := &Snapshot{
			Domain: Kind, Scope: testScope, Region: testRegion, Timestamp: testNow,
			Window: Window{Start: testStart(), End: testNow}, Targets: targets,
			Warnings: []string{"b warning", "a warning", "b warning"},
		}
		if reverse {
			snap.Warnings = []string{"b warning", "b warning", "a warning"}
		}
		rep := s.Assess(testNow, snap, nil)
		if err := rep.Validate(); err != nil {
			t.Fatalf("report invariants violated: %v", err)
		}
		b, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		if err := rep.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		return append(b, text.String()...)
	}

	// Run each ordering twice: map iteration order varies between runs inside
	// one process, so a single comparison could pass by luck.
	forward, reverse := build(false), build(true)
	if string(forward) != string(build(false)) {
		t.Fatalf("the same input produced two different reports in one process")
	}
	if string(forward) != string(reverse) {
		t.Fatalf("input order changed the report; %d vs %d bytes", len(forward), len(reverse))
	}
}

func reverseEvents(in []LogEvent) []LogEvent {
	out := make([]LogEvent, len(in))
	for i, e := range in {
		out[len(in)-1-i] = e
	}
	return out
}

func reversePoints(in []Point) []Point {
	out := make([]Point, len(in))
	for i, p := range in {
		out[len(in)-1-i] = p
	}
	return out
}

// The specs a report renders must not depend on map order either — the
// domain seam hashes them into step keys and fingerprints.
func TestSpecRenderingIsOrderIndependent(t *testing.T) {
	f := fn(1024)
	a := SpecFor(f, 512, ArchX86)
	b := domain.Spec{Resources: a.Resources, Attrs: map[string]string{}}
	keys := a.AttrKeys()
	for i := len(keys) - 1; i >= 0; i-- {
		b.Attrs[keys[i]] = a.Attrs[keys[i]]
	}
	if a.Canonical() != b.Canonical() {
		t.Fatalf("canonical spec depends on insertion order:\n%s\n%s", a.Canonical(), b.Canonical())
	}
}

// Checkpoints are persisted and compared; two checkpoints of the same learned
// state must be byte-identical.
func TestCheckpointIsDeterministic(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	tgt := target(fn(1024, withTag("team", "payments"), withTag("env", "prod")), events(testSpan,
		point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 400},
		point{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 400},
	))
	if err := d.Observe(snapOf(testSpan, tgt)); err != nil {
		t.Fatal(err)
	}
	first, err := d.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := d.Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(again) {
			t.Fatalf("checkpoint %d differs from the first", i)
		}
	}
}
