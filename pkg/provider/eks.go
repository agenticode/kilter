package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/smithy-go"
)

// asgAPI is the minimal Auto Scaling surface EKS needs; satisfied by
// *autoscaling.Client and by test fakes.
type asgAPI interface {
	DescribeAutoScalingGroups(ctx context.Context, in *autoscaling.DescribeAutoScalingGroupsInput,
		opts ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	SetDesiredCapacity(ctx context.Context, in *autoscaling.SetDesiredCapacityInput,
		opts ...func(*autoscaling.Options)) (*autoscaling.SetDesiredCapacityOutput, error)
	TerminateInstanceInAutoScalingGroup(ctx context.Context, in *autoscaling.TerminateInstanceInAutoScalingGroupInput,
		opts ...func(*autoscaling.Options)) (*autoscaling.TerminateInstanceInAutoScalingGroupOutput, error)
}

// EKS manages nodes backed by EC2 Auto Scaling groups — which covers both
// EKS managed node groups (ASGs underneath) and self-managed groups. It
// discovers groups by the standard kubernetes.io/cluster/<name> tag, scales
// them via SetDesiredCapacity, and removes drained nodes with
// TerminateInstanceInAutoScalingGroup(decrement=true) so the group does not
// replace the capacity Kilter just freed.
type EKS struct {
	clusterName string
	asg         asgAPI
}

// NewEKS loads AWS credentials from the environment (IRSA, instance profile,
// env vars, shared config) and targets the given cluster's node groups.
func NewEKS(ctx context.Context, clusterName string) (*EKS, error) {
	// A blank name would build the tag "kubernetes.io/cluster/" (or with
	// stray whitespace) that matches no ASG, making Discover silently empty.
	if strings.TrimSpace(clusterName) == "" {
		return nil, fmt.Errorf("provider eks: cluster name required (--provider-config=<cluster-name>)")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("provider eks: load aws config: %w", err)
	}
	return &EKS{clusterName: clusterName, asg: autoscaling.NewFromConfig(cfg)}, nil
}

// newEKSWithClient is the test seam.
func newEKSWithClient(clusterName string, client asgAPI) *EKS {
	return &EKS{clusterName: clusterName, asg: client}
}

func (e *EKS) Name() string { return "eks" }

// Discover lists the cluster's ASGs. The returned node map is keyed by EC2
// instance ID (callers resolve node → instance via the node's providerID).
func (e *EKS) Discover(ctx context.Context) ([]NodeGroup, map[string]string, error) {
	tagKey := "kubernetes.io/cluster/" + e.clusterName
	var groups []NodeGroup
	nodes := map[string]string{}

	p := autoscaling.NewDescribeAutoScalingGroupsPaginator(e.asg, &autoscaling.DescribeAutoScalingGroupsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("provider eks: describe ASGs: %w", err)
		}
		for _, g := range page.AutoScalingGroups {
			owned := false
			for _, t := range g.Tags {
				if t.Key != nil && *t.Key == tagKey {
					owned = true
					break
				}
			}
			if !owned {
				continue
			}
			ng := NodeGroup{
				ID:      str(g.AutoScalingGroupName),
				Name:    str(g.AutoScalingGroupName),
				Min:     int(i32(g.MinSize)),
				Max:     int(i32(g.MaxSize)),
				Desired: int(i32(g.DesiredCapacity)),
			}
			seen := map[string]bool{}
			for _, inst := range g.Instances {
				if inst.InstanceId != nil {
					nodes[*inst.InstanceId] = ng.ID
				}
				if inst.InstanceType != nil && !seen[*inst.InstanceType] {
					seen[*inst.InstanceType] = true
					ng.InstanceTypes = append(ng.InstanceTypes, *inst.InstanceType)
				}
			}
			if mip := g.MixedInstancesPolicy; mip != nil && mip.InstancesDistribution != nil {
				d := mip.InstancesDistribution
				if d.OnDemandPercentageAboveBaseCapacity != nil && *d.OnDemandPercentageAboveBaseCapacity == 0 &&
					(d.OnDemandBaseCapacity == nil || *d.OnDemandBaseCapacity == 0) {
					ng.Spot = true
				}
			}
			groups = append(groups, ng)
		}
	}
	return groups, nodes, nil
}

// ScaleTo sets a group's desired capacity.
func (e *EKS) ScaleTo(ctx context.Context, groupID string, desired int) error {
	if groupID == "" {
		return fmt.Errorf("provider eks: empty group id")
	}
	// The API takes int32; int32(desired) would silently wrap anything past
	// MaxInt32 (e.g. 1<<32+5 → 5) into a wrong but plausible capacity.
	if desired < 0 || desired > math.MaxInt32 {
		return fmt.Errorf("provider eks: desired %d out of range [0, %d]", desired, math.MaxInt32)
	}
	d := int32(desired)
	honor := false
	_, err := e.asg.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: &groupID,
		DesiredCapacity:      &d,
		HonorCooldown:        &honor,
	})
	if err != nil {
		return fmt.Errorf("provider eks: scale %s to %d: %w", groupID, desired, err)
	}
	return nil
}

// TerminateNode terminates the node's instance and shrinks its group so the
// freed capacity is not replaced. Already-gone instances are success.
func (e *EKS) TerminateNode(ctx context.Context, nodeName, providerID string) error {
	instanceID, err := InstanceIDFromProviderID(providerID)
	if err != nil {
		return fmt.Errorf("provider eks: node %s: %w", nodeName, err)
	}
	decrement := true
	_, err = e.asg.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId:                     &instanceID,
		ShouldDecrementDesiredCapacity: &decrement,
	})
	if err != nil {
		if isInstanceGone(err) {
			return nil // idempotent: someone already terminated it
		}
		return fmt.Errorf("provider eks: terminate %s (%s): %w", nodeName, instanceID, err)
	}
	return nil
}

// InstanceIDFromProviderID extracts "i-…" from "aws:///us-east-1a/i-0abc…"
// (a bare "i-…" is also accepted). The part after "i-" must be lowercase hex,
// matching every ID EC2 issues; other last segments that merely start with
// "i-" (non-AWS providerIDs, English words) are rejected here rather than
// sent to the AWS API as instance IDs.
func InstanceIDFromProviderID(providerID string) (string, error) {
	parts := strings.Split(providerID, "/")
	last := parts[len(parts)-1]
	if !strings.HasPrefix(last, "i-") || len(last) < 4 || !isLowerHex(last[2:]) {
		return "", fmt.Errorf("cannot extract instance id from providerID %q", providerID)
	}
	return last, nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isInstanceGone reports whether err is the ASG API saying the instance no
// longer exists, making termination an idempotent success.
//
// That state comes back as a ValidationError — but so do refusals where the
// instance is still RUNNING: decrementing below the group's min size
// ("Terminating instance without replacement will violate group's min size
// constraint") and scale-in/termination protection. Those must stay errors;
// swallowing them would report capacity as freed while the instance keeps
// billing. The refusal check runs first because those messages also contain
// "terminat".
func isInstanceGone(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationError" {
		return false
	}
	msg := strings.ToLower(ae.ErrorMessage())
	for _, refusal := range []string{"violate", "min size", "protected"} {
		if strings.Contains(msg, refusal) {
			return false
		}
	}
	return strings.Contains(msg, "not found") || strings.Contains(msg, "terminat") ||
		strings.Contains(msg, "no managed instance")
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func i32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}
