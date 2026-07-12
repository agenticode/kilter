package provider

import (
	"context"
	"errors"
	"fmt"
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
	if clusterName == "" {
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
	if desired < 0 {
		return fmt.Errorf("provider eks: negative desired %d", desired)
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

// InstanceIDFromProviderID extracts "i-…" from "aws:///us-east-1a/i-0abc…".
func InstanceIDFromProviderID(providerID string) (string, error) {
	parts := strings.Split(providerID, "/")
	last := parts[len(parts)-1]
	if !strings.HasPrefix(last, "i-") || len(last) < 4 {
		return "", fmt.Errorf("cannot extract instance id from providerID %q", providerID)
	}
	return last, nil
}

// isInstanceGone matches the ValidationError the ASG API returns for
// instances that were already terminated or detached.
func isInstanceGone(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() == "ValidationError" {
		msg := strings.ToLower(ae.ErrorMessage())
		return strings.Contains(msg, "not found") || strings.Contains(msg, "terminat") ||
			strings.Contains(msg, "no managed instance")
	}
	return false
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
