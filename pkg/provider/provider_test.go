package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/smithy-go"
)

func TestNewSelection(t *testing.T) {
	ctx := context.Background()
	if p, err := New(ctx, "", ""); err != nil || p.Name() != "none" {
		t.Fatalf("default provider: %v %v", p, err)
	}
	if p, err := New(ctx, "karpenter", ""); err != nil || p.Name() != "karpenter" {
		t.Fatalf("karpenter: %v %v", p, err)
	}
	if _, err := New(ctx, "webhook", "not a url"); err == nil {
		t.Fatal("bad webhook url must fail")
	}
	if _, err := New(ctx, "eks", ""); err == nil {
		t.Fatal("eks without cluster name must fail")
	}
	if _, err := New(ctx, "gce-nope", ""); err == nil {
		t.Fatal("unknown provider must fail")
	}
}

func TestNoneAndKarpenterAreSafeNoops(t *testing.T) {
	ctx := context.Background()
	for _, p := range []Provider{None{}, Karpenter{}} {
		if err := p.TerminateNode(ctx, "n1", "aws:///az/i-123"); err != nil {
			t.Fatalf("%s TerminateNode: %v", p.Name(), err)
		}
	}
	if err := (Karpenter{}).ScaleTo(ctx, "g", 3); err == nil {
		t.Fatal("karpenter ScaleTo must explain it is not applicable")
	}
}

func TestWebhookContract(t *testing.T) {
	t.Setenv("KILTER_PROVIDER_TOKEN", "hook-secret")
	var lastAuth string
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		action, _ := req["action"].(string)
		actions = append(actions, action)
		switch action {
		case "discover":
			json.NewEncoder(w).Encode(map[string]any{
				"groups": []NodeGroup{{ID: "pool-a", Name: "pool-a", Min: 1, Max: 10, Desired: 3}},
				"nodes":  map[string]string{"node-1": "pool-a"},
			})
		case "scale-to":
			if req["desired"].(float64) != 2 {
				http.Error(w, "wrong desired", 400)
			}
		case "terminate-node":
			if req["node"] != "node-1" || req["providerID"] != "custom://node-1" {
				http.Error(w, "wrong node", 400)
			}
		default:
			http.Error(w, "unknown", 400)
		}
	}))
	defer srv.Close()

	w, err := NewWebhook(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	groups, nodes, err := w.Discover(ctx)
	if err != nil || len(groups) != 1 || nodes["node-1"] != "pool-a" {
		t.Fatalf("discover: %v %v %v", groups, nodes, err)
	}
	if err := w.ScaleTo(ctx, "pool-a", 2); err != nil {
		t.Fatal(err)
	}
	if err := w.ScaleTo(ctx, "pool-a", -1); err == nil {
		t.Fatal("negative desired must fail client-side")
	}
	if err := w.TerminateNode(ctx, "node-1", "custom://node-1"); err != nil {
		t.Fatal(err)
	}
	if lastAuth != "Bearer hook-secret" {
		t.Fatalf("token not sent: %q", lastAuth)
	}
	// Non-2xx surfaces as an error.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "change window closed", http.StatusConflict)
	}))
	defer srv2.Close()
	w2, _ := NewWebhook(srv2.URL)
	if err := w2.TerminateNode(ctx, "n", "p"); err == nil {
		t.Fatal("409 must be an error")
	}
}

// ---- EKS with a fake ASG API ----

type fakeASG struct {
	groups       []types.AutoScalingGroup
	scaled       map[string]int32
	terminated   []string
	terminateErr error
}

func (f *fakeASG) DescribeAutoScalingGroups(ctx context.Context, in *autoscaling.DescribeAutoScalingGroupsInput,
	_ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	return &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: f.groups}, nil
}

func (f *fakeASG) SetDesiredCapacity(ctx context.Context, in *autoscaling.SetDesiredCapacityInput,
	_ ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error) {
	if f.scaled == nil {
		f.scaled = map[string]int32{}
	}
	f.scaled[*in.AutoScalingGroupName] = *in.DesiredCapacity
	return &autoscaling.SetDesiredCapacityOutput{}, nil
}

func (f *fakeASG) TerminateInstanceInAutoScalingGroup(ctx context.Context, in *autoscaling.TerminateInstanceInAutoScalingGroupInput,
	_ ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error) {
	if f.terminateErr != nil {
		return nil, f.terminateErr
	}
	if in.ShouldDecrementDesiredCapacity == nil || !*in.ShouldDecrementDesiredCapacity {
		return nil, fmt.Errorf("must decrement desired capacity")
	}
	f.terminated = append(f.terminated, *in.InstanceId)
	return &autoscaling.TerminateInstanceInAutoScalingGroupOutput{}, nil
}

func sp(s string) *string { return &s }
func ip(i int32) *int32   { return &i }

func testGroup(name, tagKey string, spot bool) types.AutoScalingGroup {
	g := types.AutoScalingGroup{
		AutoScalingGroupName: sp(name),
		MinSize:              ip(1), MaxSize: ip(10), DesiredCapacity: ip(3),
		Tags: []types.TagDescription{{Key: sp(tagKey)}},
		Instances: []types.Instance{
			{InstanceId: sp("i-aaa"), InstanceType: sp("m5.xlarge")},
			{InstanceId: sp("i-bbb"), InstanceType: sp("m5.xlarge")},
		},
	}
	if spot {
		zero := int32(0)
		g.MixedInstancesPolicy = &types.MixedInstancesPolicy{
			InstancesDistribution: &types.InstancesDistribution{
				OnDemandPercentageAboveBaseCapacity: &zero,
				OnDemandBaseCapacity:                &zero,
			},
		}
	}
	return g
}

func TestEKSDiscover(t *testing.T) {
	fake := &fakeASG{groups: []types.AutoScalingGroup{
		testGroup("ng-workers", "kubernetes.io/cluster/prod", false),
		testGroup("ng-spot", "kubernetes.io/cluster/prod", true),
		testGroup("other-cluster", "kubernetes.io/cluster/staging", false),
	}}
	e := newEKSWithClient("prod", fake)
	groups, nodes, err := e.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("want 2 owned groups, got %d", len(groups))
	}
	if !groups[1].Spot || groups[0].Spot {
		t.Fatalf("spot detection wrong: %+v", groups)
	}
	if nodes["i-aaa"] == "" {
		t.Fatal("instance→group mapping missing")
	}
	if groups[0].InstanceTypes[0] != "m5.xlarge" {
		t.Fatalf("instance types: %v", groups[0].InstanceTypes)
	}
}

func TestEKSScaleAndTerminate(t *testing.T) {
	fake := &fakeASG{}
	e := newEKSWithClient("prod", fake)
	ctx := context.Background()
	if err := e.ScaleTo(ctx, "ng-workers", 5); err != nil {
		t.Fatal(err)
	}
	if fake.scaled["ng-workers"] != 5 {
		t.Fatalf("scaled: %+v", fake.scaled)
	}
	if err := e.TerminateNode(ctx, "ip-10-0-1-7", "aws:///us-east-1a/i-0abc123"); err != nil {
		t.Fatal(err)
	}
	if len(fake.terminated) != 1 || fake.terminated[0] != "i-0abc123" {
		t.Fatalf("terminated: %v", fake.terminated)
	}
	if err := e.TerminateNode(ctx, "bad", "kind://docker/x"); err == nil {
		t.Fatal("non-aws providerID must fail")
	}
}

type apiErr struct{ code, msg string }

func (e apiErr) Error() string                 { return e.msg }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.msg }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestEKSTerminateIdempotent(t *testing.T) {
	fake := &fakeASG{terminateErr: apiErr{"ValidationError", "Instance Id not found - No managed instance found for instance ID: i-0abc"}}
	e := newEKSWithClient("prod", fake)
	if err := e.TerminateNode(context.Background(), "n", "aws:///az/i-0abc"); err != nil {
		t.Fatalf("already-gone instance must be success: %v", err)
	}
	// Other errors still surface.
	fake.terminateErr = apiErr{"AccessDenied", "not authorized"}
	if err := e.TerminateNode(context.Background(), "n", "aws:///az/i-0abc"); err == nil {
		t.Fatal("access denied must be an error")
	}
}

func TestInstanceIDParsing(t *testing.T) {
	if id, err := InstanceIDFromProviderID("aws:///us-east-1a/i-0123456789abcdef"); err != nil || id != "i-0123456789abcdef" {
		t.Fatalf("%v %v", id, err)
	}
	for _, bad := range []string{"", "aws:///us-east-1a/", "kind://docker/kind/kind-worker"} {
		if _, err := InstanceIDFromProviderID(bad); err == nil {
			t.Fatalf("%q should fail", bad)
		}
	}
}
