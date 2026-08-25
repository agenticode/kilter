package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

// ---- isInstanceGone: gone vs. refused-but-still-running ----

func TestIsInstanceGoneClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		gone bool
	}{
		{"not found", apiErr{"ValidationError", "Instance Id not found - No managed instance found for instance ID: i-0abc"}, true},
		{"already terminated", apiErr{"ValidationError", "Instance i-0abc is already terminated"}, true},
		{"wrapped not found", fmt.Errorf("terminate: %w", apiErr{"ValidationError", "no managed instance found"}), true},
		// ValidationError is also how the API REFUSES terminations that leave
		// the instance running. These must never be classified as gone.
		{"min size violation", apiErr{"ValidationError",
			"Terminating instance without replacement will violate group's min size constraint. " +
				"Either set shouldDecrementDesiredCapacity to false or lower the group's min size."}, false},
		{"scale-in protection", apiErr{"ValidationError", "Instance i-0abc is protected from termination."}, false},
		{"scale-in protection alt", apiErr{"ValidationError", "The instance i-0abc is protected from scale-in."}, false},
		{"other code", apiErr{"AccessDenied", "not authorized"}, false},
		{"plain error", fmt.Errorf("dial tcp: connection refused"), false},
		{"nil-ish empty message", apiErr{"ValidationError", ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isInstanceGone(tc.err); got != tc.gone {
				t.Fatalf("isInstanceGone(%v) = %v, want %v", tc.err, got, tc.gone)
			}
		})
	}
}

func TestEKSTerminateMinSizeViolationSurfaces(t *testing.T) {
	fake := &fakeASG{terminateErr: apiErr{"ValidationError",
		"Terminating instance without replacement will violate group's min size constraint. " +
			"Either set shouldDecrementDesiredCapacity to false or lower the group's min size."}}
	e := newEKSWithClient("prod", fake)
	err := e.TerminateNode(context.Background(), "n", "aws:///az/i-0abc")
	if err == nil {
		t.Fatal("min-size violation means the instance is STILL RUNNING; swallowing it reports savings that never happened")
	}
	if len(fake.terminated) != 0 {
		t.Fatalf("nothing should be recorded terminated: %v", fake.terminated)
	}
}

// ---- EKS ScaleTo bounds ----

func TestEKSScaleToBounds(t *testing.T) {
	fake := &fakeASG{}
	e := newEKSWithClient("prod", fake)
	ctx := context.Background()

	if err := e.ScaleTo(ctx, "g", -1); err == nil {
		t.Fatal("negative desired must fail")
	}
	// int32(desired) would silently wrap for values past MaxInt32:
	// 1<<32+5 becomes 5 — a wrong but plausible-looking capacity.
	if err := e.ScaleTo(ctx, "g", int(int64(math.MaxInt32)+1)); err == nil {
		t.Fatal("desired past MaxInt32 must fail, not wrap")
	}
	if err := e.ScaleTo(ctx, "", 1); err == nil {
		t.Fatal("empty group id must fail client-side")
	}
	if len(fake.scaled) != 0 {
		t.Fatalf("rejected calls must never reach the API: %v", fake.scaled)
	}
	if err := e.ScaleTo(ctx, "g", math.MaxInt32); err != nil {
		t.Fatalf("MaxInt32 exactly is representable: %v", err)
	}
	if fake.scaled["g"] != math.MaxInt32 {
		t.Fatalf("scaled: %+v", fake.scaled)
	}
}

// ---- providerID parsing: adversarial inputs ----

func TestInstanceIDParsingHardened(t *testing.T) {
	good := []struct{ in, want string }{
		{"aws:///us-east-1a/i-0123456789abcdef", "i-0123456789abcdef"},
		{"aws:///i-0abcdef1", "i-0abcdef1"},          // no-AZ form
		{"i-0123456789abcdef", "i-0123456789abcdef"}, // bare instance ID
	}
	for _, tc := range good {
		if id, err := InstanceIDFromProviderID(tc.in); err != nil || id != tc.want {
			t.Fatalf("%q: got %q, %v", tc.in, id, err)
		}
	}
	bad := []string{
		"",
		"aws:///us-east-1a/",
		"aws:///us-east-1a/i-0abc123/", // trailing slash
		"kind://docker/kind/kind-worker",
		"i-am-not-aws",       // starts with i- but is not an instance ID
		"i-🙂🙂🙂",              // non-hex garbage
		"aws:///az/i-0ABC12", // EC2 issues lowercase hex only
		"aws:///az/i--0abc",
		"aws:///az/i-",
	}
	for _, in := range bad {
		if id, err := InstanceIDFromProviderID(in); err == nil {
			t.Fatalf("%q must fail, got %q", in, id)
		}
	}
}

func FuzzInstanceIDFromProviderID(f *testing.F) {
	for _, seed := range []string{
		"aws:///us-east-1a/i-0123456789abcdef", "i-0abc", "kind://docker/x",
		"", "aws:///az/", "i-", "i-🙂", "///i-ffff", "aws:///az/I-0ABC",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, providerID string) {
		id, err := InstanceIDFromProviderID(providerID)
		if err != nil {
			return
		}
		if !strings.HasPrefix(id, "i-") || len(id) < 4 {
			t.Fatalf("accepted malformed id %q from %q", id, providerID)
		}
		for _, c := range id[2:] {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("accepted non-hex id %q from %q", id, providerID)
			}
		}
		if !strings.Contains(providerID, id) {
			t.Fatalf("id %q not a substring of input %q", id, providerID)
		}
	})
}

// ---- EKS Discover: pagination, spot detection, nil safety ----

func TestEKSDiscoverPagination(t *testing.T) {
	fake := &fakeASG{pages: [][]types.AutoScalingGroup{
		{testGroup("ng-a", "kubernetes.io/cluster/prod", false)},
		{testGroup("ng-b", "kubernetes.io/cluster/prod", false)},
		{testGroup("ng-other", "kubernetes.io/cluster/staging", false)},
	}}
	e := newEKSWithClient("prod", fake)
	groups, _, err := e.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].ID != "ng-a" || groups[1].ID != "ng-b" {
		t.Fatalf("groups across pages: %+v", groups)
	}
}

func TestEKSDiscoverSpotDetection(t *testing.T) {
	pct := func(v int32) *int32 { return &v }
	cases := []struct {
		name string
		mip  *types.MixedInstancesPolicy
		spot bool
	}{
		{"no policy", nil, false},
		{"all spot", &types.MixedInstancesPolicy{InstancesDistribution: &types.InstancesDistribution{
			OnDemandPercentageAboveBaseCapacity: pct(0), OnDemandBaseCapacity: pct(0)}}, true},
		{"spot above base, on-demand base", &types.MixedInstancesPolicy{InstancesDistribution: &types.InstancesDistribution{
			OnDemandPercentageAboveBaseCapacity: pct(0), OnDemandBaseCapacity: pct(2)}}, false},
		{"all on-demand", &types.MixedInstancesPolicy{InstancesDistribution: &types.InstancesDistribution{
			OnDemandPercentageAboveBaseCapacity: pct(100)}}, false},
		{"percentage unset", &types.MixedInstancesPolicy{InstancesDistribution: &types.InstancesDistribution{}}, false},
		{"empty distribution", &types.MixedInstancesPolicy{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := testGroup("ng", "kubernetes.io/cluster/prod", false)
			g.MixedInstancesPolicy = tc.mip
			fake := &fakeASG{groups: []types.AutoScalingGroup{g}}
			groups, _, err := newEKSWithClient("prod", fake).Discover(context.Background())
			if err != nil || len(groups) != 1 {
				t.Fatalf("%v %v", groups, err)
			}
			if groups[0].Spot != tc.spot {
				t.Fatalf("spot = %v, want %v", groups[0].Spot, tc.spot)
			}
		})
	}
}

func TestEKSDiscoverNilFieldsSafe(t *testing.T) {
	// A group with every optional field nil must not panic; the tag is the
	// only thing that admits it.
	fake := &fakeASG{groups: []types.AutoScalingGroup{{
		Tags:      []types.TagDescription{{Key: sp("kubernetes.io/cluster/prod")}},
		Instances: []types.Instance{{InstanceId: nil, InstanceType: sp("m5.large")}, {InstanceId: sp("i-ccc")}},
	}}}
	groups, nodes, err := newEKSWithClient("prod", fake).Discover(context.Background())
	if err != nil || len(groups) != 1 {
		t.Fatalf("%v %v", groups, err)
	}
	g := groups[0]
	if g.Min != 0 || g.Max != 0 || g.Desired != 0 || g.ID != "" {
		t.Fatalf("nil numeric fields should read as zero: %+v", g)
	}
	if _, ok := nodes[""]; ok {
		t.Fatal("nil instance IDs must not create a map entry")
	}
	if nodes["i-ccc"] != g.ID {
		t.Fatalf("nodes: %v", nodes)
	}
}

func TestNewEKSRejectsBlankClusterName(t *testing.T) {
	for _, name := range []string{"", "   ", "\t\n"} {
		if _, err := New(context.Background(), "eks", name); err == nil {
			t.Fatalf("cluster name %q must be rejected", name)
		}
	}
}

// ---- Webhook: discovery payload validation ----

func webhookServing(t *testing.T, discoverResp any) *Webhook {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(discoverResp)
	}))
	t.Cleanup(srv.Close)
	wh, err := NewWebhook(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return wh
}

func TestWebhookDiscoverValidation(t *testing.T) {
	ctx := context.Background()
	bad := []struct {
		name string
		resp any
	}{
		{"empty group id", map[string]any{"groups": []NodeGroup{{ID: "", Name: "x", Max: 1}}}},
		{"duplicate group id", map[string]any{"groups": []NodeGroup{{ID: "a", Max: 1}, {ID: "a", Max: 1}}}},
		{"negative min", map[string]any{"groups": []NodeGroup{{ID: "a", Min: -1, Max: 1}}}},
		{"negative desired", map[string]any{"groups": []NodeGroup{{ID: "a", Max: 5, Desired: -3}}}},
		{"max below min", map[string]any{"groups": []NodeGroup{{ID: "a", Min: 5, Max: 2}}}},
		{"empty node name", map[string]any{"nodes": map[string]string{"": "a"}}},
		{"empty node group", map[string]any{"nodes": map[string]string{"n1": ""}}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := webhookServing(t, tc.resp).Discover(ctx); err == nil {
				t.Fatal("structurally invalid discovery must be rejected, not acted on")
			}
		})
	}
	// Desired outside [Min,Max] is a legitimate transient state and must pass.
	ok := map[string]any{
		"groups": []NodeGroup{{ID: "a", Min: 1, Max: 3, Desired: 7}},
		"nodes":  map[string]string{"n1": "not-listed"}, // unlisted group refs allowed
	}
	groups, nodes, err := webhookServing(t, ok).Discover(ctx)
	if err != nil || len(groups) != 1 || nodes["n1"] != "not-listed" {
		t.Fatalf("%v %v %v", groups, nodes, err)
	}
}

func TestWebhookDiscoverMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>gateway timeout</html>")
	}))
	defer srv.Close()
	wh, _ := NewWebhook(srv.URL)
	if _, _, err := wh.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("malformed body must be a decode error, got %v", err)
	}
}

func TestWebhookDiscoverTruncatedBody(t *testing.T) {
	// Declare a large Content-Length but write only a fragment; the client
	// sees an unexpected EOF mid-body, which must surface — not feed a
	// truncated payload into decisions.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		fmt.Fprint(w, `{"groups":`)
	}))
	defer srv.Close()
	wh, _ := NewWebhook(srv.URL)
	if _, _, err := wh.Discover(context.Background()); err == nil {
		t.Fatal("truncated discover body must be an error")
	}
}

func TestWebhookTerminateIdentifiers(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	wh, _ := NewWebhook(srv.URL)
	ctx := context.Background()

	// providerID may be empty: the Node object can be gone before its
	// providerID was read, and the endpoint resolves by name.
	if err := wh.TerminateNode(ctx, "node-1", ""); err != nil {
		t.Fatalf("empty providerID with node name must work: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected one call, got %d", hits.Load())
	}
	// Both empty identifies nothing — reject client-side.
	if err := wh.TerminateNode(ctx, "", ""); err == nil {
		t.Fatal("terminate with no identifiers must fail")
	}
	if hits.Load() != 1 {
		t.Fatalf("rejected call must not reach the endpoint (hits=%d)", hits.Load())
	}
}

func TestWebhookScaleToEmptyGroup(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	wh, _ := NewWebhook(srv.URL)
	if err := wh.ScaleTo(context.Background(), "", 3); err == nil {
		t.Fatal("empty group id must fail client-side")
	}
	if hits.Load() != 0 {
		t.Fatal("rejected call must not reach the endpoint")
	}
}

func TestWebhookErrorBodyBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, strings.Repeat("x", 1<<20), http.StatusInternalServerError)
	}))
	defer srv.Close()
	wh, _ := NewWebhook(srv.URL)
	err := wh.TerminateNode(context.Background(), "n", "p")
	if err == nil {
		t.Fatal("500 must be an error")
	}
	if len(err.Error()) > 400 {
		t.Fatalf("error must not embed unbounded response bodies (len=%d)", len(err.Error()))
	}
}

func TestWebhookNoTokenNoHeader(t *testing.T) {
	t.Setenv("KILTER_PROVIDER_TOKEN", "")
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	wh, _ := NewWebhook(srv.URL)
	if err := wh.TerminateNode(context.Background(), "n", "p"); err != nil {
		t.Fatal(err)
	}
	if got, _ := auth.Load().(string); got != "" {
		t.Fatalf("no token configured but Authorization sent: %q", got)
	}
}
