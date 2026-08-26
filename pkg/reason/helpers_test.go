package reason

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

// The fixture every test in this package works over: one cluster, a handful
// of subjects, and a substrate whose contents are a pure function of the
// arguments below. No test in this package reads a wall clock.

var (
	t0      = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	tEnd    = t0.Add(72 * time.Hour)
	cluster = "prod"
)

func testScope() Scope {
	return Scope{Cluster: cluster, From: t0, To: tEnd}
}

// substrate builds an evidence.Memory holding events, decisions, samples and
// a cost timeline for the given container keys.
func substrate(t *testing.T, keys ...string) *evidence.Memory {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{"default/Deployment/payments-api/app", "default/Deployment/search/app"}
	}
	cfg := evidence.DefaultConfig()
	m, err := evidence.NewMemory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		s := evidence.SubjectRef{Cluster: cluster, Kind: evidence.SubjectContainer, Key: key}
		if err := m.Append(evidence.EvidenceEvent{
			At:       t0.Add(time.Duration(i+1) * time.Hour),
			Kind:     evidence.EventDeploy,
			Subject:  s,
			Severity: evidence.SeverityInfo,
			Attrs:    map[string]string{"image": "app:v1", "generation": "7"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.Append(evidence.EvidenceEvent{
			At:       t0.Add(time.Duration(i+2) * time.Hour),
			Kind:     evidence.EventOOMKill,
			Subject:  s,
			Severity: evidence.SeverityCritical,
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.RecordDecision(evidence.DecisionRecord{
			At:      t0.Add(time.Duration(i+3) * time.Hour),
			Subject: s,
			Kind:    evidence.DecisionRecommendation,
			Summary: "cpu 500m -> 210m",
		}); err != nil {
			t.Fatal(err)
		}
		for h := 0; h < 30; h++ {
			if err := m.ObserveSample(s, evidence.Sample{
				At:          t0.Add(time.Duration(h) * time.Hour),
				MilliCPU:    int64(100 + h),
				MemoryBytes: int64(64<<20 + h),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for h := 0; h < 24; h++ {
		if err := m.ObservePoint(cluster, evidence.TimelinePoint{
			At:             t0.Add(time.Duration(h) * time.Hour),
			CostUSDPerHour: 1.5 + float64(h)/100,
			Nodes:          10 + h%3,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// registry builds a registry over the fixture.
func registry(t *testing.T, m *evidence.Memory) *Registry {
	t.Helper()
	r, err := NewRegistry(RegistryConfig{
		Scope:    testScope(),
		Store:    m,
		Subjects: m.Subjects(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// call runs one tool with the given argument JSON.
func call(t *testing.T, r *Registry, tool, args string) Outcome {
	t.Helper()
	return r.Run(context.Background(), ToolCall{ID: "c1", Tool: tool, Args: json.RawMessage(args)})
}

// containerKey is the fixture's first subject key.
const containerKey = "default/Deployment/payments-api/app"

// scriptedProvider is the deterministic fake Provider §9's unit-6 test
// strategy calls for. A script is a list of turns; each Chat call returns the
// next one, and a script that runs out repeats its last turn forever — which
// is what makes "the loop is bounded by the budget and not by the transcript"
// testable.
type scriptedProvider struct {
	turns []ChatResponse
	calls int
	// seen records every request, so a test can assert what the model was
	// actually shown.
	seen []ChatRequest
	info ProviderInfo
	err  error
}

func (p *scriptedProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	p.seen = append(p.seen, req)
	p.calls++
	if p.err != nil {
		return ChatResponse{}, p.err
	}
	if len(p.turns) == 0 {
		return ChatResponse{}, nil
	}
	i := p.calls - 1
	if i >= len(p.turns) {
		i = len(p.turns) - 1
	}
	return p.turns[i], nil
}

func (p *scriptedProvider) Info() ProviderInfo {
	if p.info.Name == "" {
		return ProviderInfo{
			Name:          "scripted",
			Model:         "scripted-1",
			USDPerMInput:  3,
			USDPerMOutput: 15,
		}
	}
	return p.info
}

// toolTurn is a turn that asks for one tool call.
func toolTurn(id, tool, args string) ChatResponse {
	return ChatResponse{
		ToolCalls:  []ToolCall{{ID: id, Tool: tool, Args: json.RawMessage(args)}},
		Usage:      Usage{InputTokens: 1000, OutputTokens: 200},
		StopReason: "tool_use",
	}
}

// answerTurn is a final turn carrying a structured finding.
func answerTurn(answer string, handles ...string) ChatResponse {
	out := struct {
		Answer     string   `json:"answer"`
		Evidence   []string `json:"evidence"`
		Confidence string   `json:"confidence"`
	}{answer, handles, "medium"}
	if out.Evidence == nil {
		out.Evidence = []string{}
	}
	b, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return ChatResponse{
		Output:     b,
		Usage:      Usage{InputTokens: 1200, OutputTokens: 300},
		StopReason: "end_turn",
	}
}

// investigator builds a loop over the fixture with a scripted provider.
func investigator(t *testing.T, r *Registry, p Provider, b Budget) *Investigator {
	t.Helper()
	iv, err := New(Config{
		Provider: p,
		Registry: r,
		Clock:    StepClock(t0, time.Second),
		Budget:   b,
		Seed: []Candidate{
			{Subject: evidence.SubjectRef{Cluster: cluster, Kind: evidence.SubjectContainer, Key: containerKey},
				Score: 42, Note: "top saving"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return iv
}

// firstHandle runs the loop once with a tool turn and returns the handle the
// registry issued for the first citation of that call.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
