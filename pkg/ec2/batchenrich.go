package ec2

// AWS Batch enrichment — U15b (docs/design/rds-batch-assessment.md §3.2, §3.3,
// §5 U15).
//
// # Why this is three advisories and not a domain
//
// "There is no additional charge for AWS Batch" [verified]: every Batch dollar
// is an EC2 instance-hour that pkg/ec2 already owns. And `desiredvCpus` is
// managed by AWS "between the minimum and maximum values based on job queue
// demand" — the compute environment *is* an autoscaler, so there is no target
// for a rightsizer to size. What is left is a handful of one-integer
// configuration findings that cost money, cannot be measured from metrics, and
// must never be changed by kilter. That is an advisory, and this file emits
// three of them:
//
//   - the priced `minvCpus` idle floor — capacity bought and held whether or
//     not any job exists, and the reason U15a suppresses Batch instances;
//   - `BEST_FIT`, the default allocation strategy, which "keeps costs lower
//     but can limit scaling" and blocks infrastructure updates entirely;
//   - a non-empty `bidPercentage`, against AWS's own "for most use cases, we
//     recommend leaving this field empty".
//
// # Nothing here is actuatable, structurally
//
// Every finding is an [Advisory], and [Advisory.Actuatable] returns false for
// all of them, always. That is not politeness. All three fields live on a
// Batch-managed resource, changing any of them is an *infrastructure update*
// through the Batch API, and AWS warns that modifying Batch-managed resources
// by hand "can result in unexpected behavior, including INVALID compute
// environments ... or unexpected costs" [verified]. A `BEST_FIT` compute
// environment cannot receive an infrastructure update at all, so "fixing" it
// is a create-migrate-delete of the compute environment, not a knob.
//
// # The seam is optional
//
// [CollectorConfig.Batch] defaults to nil and [Collector.Collect] then produces
// exactly the snapshot it produced before this file existed. A Batch API that
// errors, or an account with no compute environments, degrades to a warning and
// no advisories — never to a failed collection, because Batch is enrichment and
// the EC2 report is the deliverable. Pinned by TestBatchEnrichmentIsOptional.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Advisory codes emitted by this file. All three are report-scope: they
// describe a compute environment, not an instance, and this package has no
// verified way to map an EC2 instance to the compute environment that launched
// it (see FINDINGS.md §Batch).
const (
	AdvisoryBatchMinVCpusFloor = "batch-minvcpus-floor"
	AdvisoryBatchBestFit       = "batch-best-fit"
	AdvisoryBatchBidPercentage = "batch-bid-percentage"
)

// Compute environment types. Only the two EC2-backed ones carry the fields
// this file reads: `minvCpus`, `allocationStrategy` and `bidPercentage` all
// document "This parameter isn't applicable to jobs that are running on
// Fargate resources" [verified], and Fargate capacity is pkg/ecs's subject
// anyway.
const (
	BatchCETypeEC2                 = "EC2"
	BatchCETypeSpot                = "SPOT"
	BatchCETypeFargate             = "FARGATE"
	BatchCETypeFargateSpot         = "FARGATE_SPOT"
	BatchCETypeECSManagedInstances = "ECS_MANAGED_INSTANCES"
)

// Compute environment management and state, from DescribeComputeEnvironments.
const (
	BatchManaged   = "MANAGED"
	BatchUnmanaged = "UNMANAGED"
	BatchEnabled   = "ENABLED"
	BatchDisabled  = "DISABLED"
)

// AllocationStrategyBestFit is the default for an ECS-backed compute
// environment: "For Amazon ECS compute environments, if this parameter isn't
// specified, the BEST_FIT allocation strategy is used by default" [verified].
// An empty string therefore means BEST_FIT, not "unset" — reading it as unset
// would silently hide the finding on every compute environment that never
// named a strategy, which is exactly the population the finding is about.
const AllocationStrategyBestFit = "BEST_FIT"

// ComputeResource mirrors the Batch API type of the same name. Field names
// track the API's so a recorded fixture reads like the response it came from.
type ComputeResource struct {
	Type string `json:"type,omitempty"`
	// AllocationStrategy is empty when unspecified — which means BEST_FIT on
	// an ECS-backed compute environment. Use [ComputeResource.Strategy].
	AllocationStrategy string `json:"allocationStrategy,omitempty"`
	// MinvCpus is "the minimum number of vCPUs that a compute environment
	// should maintain (even if the compute environment is DISABLED)"
	// [verified]. It is the one standing cost in a compute environment.
	MinvCpus int `json:"minvCpus,omitempty"`
	MaxvCpus int `json:"maxvCpus,omitempty"`
	// DesiredvCpus is managed by AWS and is deliberately never reported as a
	// finding: "AWS Batch modifies this value between the minimum and maximum
	// values based on job queue demand" [verified].
	DesiredvCpus int `json:"desiredvCpus,omitempty"`
	// BidPercentage is the Spot price cap as a percentage of on-demand. Zero
	// means the field was left empty, which AWS recommends and which defaults
	// to 100 %.
	BidPercentage int      `json:"bidPercentage,omitempty"`
	InstanceTypes []string `json:"instanceTypes,omitempty"`
}

// Strategy returns the effective allocation strategy, resolving the documented
// empty-means-BEST_FIT default for EC2-backed compute environments.
func (cr ComputeResource) Strategy() string {
	if s := strings.TrimSpace(cr.AllocationStrategy); s != "" {
		return s
	}
	if cr.EC2Backed() {
		return AllocationStrategyBestFit
	}
	return ""
}

// EC2Backed reports whether this compute resource bills as EC2 instance-hours
// and carries the fields this file reads.
func (cr ComputeResource) EC2Backed() bool {
	return cr.Type == BatchCETypeEC2 || cr.Type == BatchCETypeSpot
}

// ComputeEnvironmentDetail mirrors the Batch API type of the same name,
// narrowed to the fields an insight reads.
type ComputeEnvironmentDetail struct {
	ComputeEnvironmentName string `json:"computeEnvironmentName"`
	ComputeEnvironmentARN  string `json:"computeEnvironmentArn,omitempty"`
	Type                   string `json:"type,omitempty"`   // MANAGED | UNMANAGED
	State                  string `json:"state,omitempty"`  // ENABLED | DISABLED
	Status                 string `json:"status,omitempty"` // VALID | INVALID | ...
	// ComputeResources is absent on an unmanaged compute environment, where
	// AWS Batch provisions nothing and there is nothing to report.
	ComputeResources *ComputeResource `json:"computeResources,omitempty"`
}

// Managed reports whether AWS Batch provisions this environment's capacity.
func (ce ComputeEnvironmentDetail) Managed() bool {
	return strings.EqualFold(ce.Type, BatchManaged) && ce.ComputeResources != nil
}

// ComputeEnvironmentOrder attaches a compute environment to a job queue.
type ComputeEnvironmentOrder struct {
	Order              int    `json:"order"`
	ComputeEnvironment string `json:"computeEnvironment"` // name or ARN
}

// JobQueueDetail mirrors the Batch API type of the same name. Queues answer
// the question the floor advisory has to answer honestly: is there anything
// attached to this compute environment that could use the capacity it holds?
type JobQueueDetail struct {
	JobQueueName            string                    `json:"jobQueueName"`
	JobQueueARN             string                    `json:"jobQueueArn,omitempty"`
	State                   string                    `json:"state,omitempty"` // ENABLED | DISABLED
	Priority                int                       `json:"priority,omitempty"`
	ComputeEnvironmentOrder []ComputeEnvironmentOrder `json:"computeEnvironmentOrder,omitempty"`
}

// DescribeComputeEnvironmentsInput is the paginating request.
type DescribeComputeEnvironmentsInput struct {
	NextToken  string `json:"nextToken,omitempty"`
	MaxResults int32  `json:"maxResults,omitempty"`
}

// DescribeComputeEnvironmentsOutput is one page. An empty NextToken ends it.
type DescribeComputeEnvironmentsOutput struct {
	ComputeEnvironments []ComputeEnvironmentDetail `json:"computeEnvironments,omitempty"`
	NextToken           string                     `json:"nextToken,omitempty"`
}

// DescribeJobQueuesInput is the paginating request.
type DescribeJobQueuesInput struct {
	NextToken  string `json:"nextToken,omitempty"`
	MaxResults int32  `json:"maxResults,omitempty"`
}

// DescribeJobQueuesOutput is one page.
type DescribeJobQueuesOutput struct {
	JobQueues []JobQueueDetail `json:"jobQueues,omitempty"`
	NextToken string           `json:"nextToken,omitempty"`
}

// BatchAPI is the optional, read-only AWS Batch seam: two describes, no
// mutation, matching the `batch:DescribeComputeEnvironments` and
// `batch:DescribeJobQueues` IAM actions. Like [InventoryAPI] and [MetricsAPI]
// it is a plain Go interface over plain Go structs, so this package still
// links no AWS SDK and makes no network call.
type BatchAPI interface {
	DescribeComputeEnvironments(ctx context.Context, in *DescribeComputeEnvironmentsInput) (*DescribeComputeEnvironmentsOutput, error)
	DescribeJobQueues(ctx context.Context, in *DescribeJobQueuesInput) (*DescribeJobQueuesOutput, error)
}

// BatchInventory is what the optional seam delivered, carried on a [Snapshot].
// Both slices are sorted by name, so a Batch API that pages in a different
// order cannot change a byte of the report.
type BatchInventory struct {
	ComputeEnvironments []ComputeEnvironmentDetail `json:"computeEnvironments,omitempty"`
	JobQueues           []JobQueueDetail           `json:"jobQueues,omitempty"`
	// Partial marks an enrichment the collector could not finish — a page
	// budget exhausted, or a describe that failed. Unlike a partial *metric*
	// series this suppresses nothing: it downgrades the advisories that were
	// produced, and says so.
	Partial bool `json:"partial,omitempty"`
}

// collectBatch reads both describes, tolerating everything. It returns
// warnings instead of an error on purpose: Batch is enrichment, and a
// permissions gap on batch:DescribeComputeEnvironments must not cost the
// operator their EC2 report.
func collectBatch(ctx context.Context, api BatchAPI, maxPages int) (*BatchInventory, []string) {
	bi := &BatchInventory{}
	var warns []string

	token := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			bi.Partial = true
			warns = append(warns, fmt.Sprintf(
				"aws batch: stopped after %d pages of DescribeComputeEnvironments; the compute-environment "+
					"advisories below cover only what was read", maxPages))
			break
		}
		out, err := api.DescribeComputeEnvironments(ctx, &DescribeComputeEnvironmentsInput{NextToken: token})
		if err != nil {
			bi.Partial = true
			warns = append(warns, fmt.Sprintf(
				"aws batch: DescribeComputeEnvironments failed (%v); AWS Batch findings are omitted from this "+
					"report, and the EC2 assessments are unaffected", err))
			break
		}
		if out == nil {
			bi.Partial = true
			warns = append(warns, "aws batch: DescribeComputeEnvironments returned nothing")
			break
		}
		bi.ComputeEnvironments = append(bi.ComputeEnvironments, out.ComputeEnvironments...)
		if token = out.NextToken; token == "" {
			break
		}
	}

	token = ""
	for page := 0; ; page++ {
		if page >= maxPages {
			bi.Partial = true
			warns = append(warns, fmt.Sprintf(
				"aws batch: stopped after %d pages of DescribeJobQueues", maxPages))
			break
		}
		out, err := api.DescribeJobQueues(ctx, &DescribeJobQueuesInput{NextToken: token})
		if err != nil {
			bi.Partial = true
			warns = append(warns, fmt.Sprintf(
				"aws batch: DescribeJobQueues failed (%v); compute-environment findings are still reported, "+
					"without the queues attached to them", err))
			break
		}
		if out == nil {
			bi.Partial = true
			break
		}
		bi.JobQueues = append(bi.JobQueues, out.JobQueues...)
		if token = out.NextToken; token == "" {
			break
		}
	}

	bi.sort()
	if len(bi.ComputeEnvironments) == 0 && !bi.Partial {
		return nil, warns
	}
	return bi, warns
}

func (b *BatchInventory) sort() {
	sort.SliceStable(b.ComputeEnvironments, func(i, j int) bool {
		return b.ComputeEnvironments[i].ComputeEnvironmentName < b.ComputeEnvironments[j].ComputeEnvironmentName
	})
	sort.SliceStable(b.JobQueues, func(i, j int) bool {
		return b.JobQueues[i].JobQueueName < b.JobQueues[j].JobQueueName
	})
}

// queuesFor lists the job queues attached to a compute environment, by queue
// name, sorted, each marked with its state. A queue references its compute
// environments by name or by ARN, so both are matched.
func (b *BatchInventory) queuesFor(ce ComputeEnvironmentDetail) (attached []string, enabled int) {
	if b == nil {
		return nil, 0
	}
	for _, q := range b.JobQueues {
		for _, o := range q.ComputeEnvironmentOrder {
			ref := strings.TrimSpace(o.ComputeEnvironment)
			if ref == "" || (ref != ce.ComputeEnvironmentName &&
				(ce.ComputeEnvironmentARN == "" || ref != ce.ComputeEnvironmentARN)) {
				continue
			}
			if strings.EqualFold(q.State, BatchEnabled) {
				enabled++
				attached = append(attached, q.JobQueueName)
			} else {
				attached = append(attached, q.JobQueueName+" (disabled)")
			}
			break
		}
	}
	sort.Strings(attached)
	return attached, enabled
}

// vcpuRate is the cheapest catalog price per vCPU-hour among the instance
// types a compute environment declares, and the type that set it.
type vcpuRate struct {
	hourlyPerVCPU float64
	basis         string
}

// floorRate prices one vCPU-hour for a compute environment.
//
// It resolves the declared `instanceTypes` — which may be exact types
// ("c5.8xlarge") or bare families ("c5") — against the pricing catalog and
// takes the CHEAPEST resulting $/vCPU-hour. Cheapest, deliberately: the
// resulting floor cost is then a lower bound, so the advisory understates what
// the operator is paying rather than overstating what they could save. It is
// the same direction pkg/ec2 takes everywhere else — under-claim, never
// over-claim.
//
// AWS's own instance-selection bundles (`optimal`, `default_x86_64`,
// `default_arm64`) resolve to families that vary by region and that AWS
// "periodically updates ... to newer, more cost-effective options" [verified].
// They are not resolved here, and a compute environment that uses only those
// gets an unpriced advisory rather than a made-up number.
func (s *Sizer) floorRate(cr ComputeResource) (vcpuRate, bool) {
	declared := map[string]bool{}
	for _, t := range cr.InstanceTypes {
		if t = strings.TrimSpace(t); t != "" {
			declared[t] = true
		}
	}
	if len(declared) == 0 {
		return vcpuRate{}, false
	}
	var best vcpuRate
	found := false
	// Candidates is sorted by (price, provider, name), so the first strict
	// improvement wins every tie the same way on every run.
	for _, it := range s.cat.Candidates(s.cfg.Provider, "") {
		if !declared[it.Name] && !declared[it.Family] {
			continue
		}
		if it.MilliCPU <= 0 || it.HourlyUSD <= 0 {
			continue
		}
		rate := it.HourlyUSD / (float64(it.MilliCPU) / 1000)
		if !found || rate < best.hourlyPerVCPU {
			best, found = vcpuRate{hourlyPerVCPU: rate, basis: it.Name}, true
		}
	}
	return best, found
}

// batchInsights turns the optional Batch inventory into report-scope
// advisories. It proposes nothing, prices nothing as a saving, and reads no
// metric — there is no per-job CPU or memory signal without paid Container
// Insights, and a per-job series is too short for any evidence gate this
// package has (§3.4).
func (s *Sizer) batchInsights(snap *Snapshot) []Advisory {
	if snap == nil || snap.Batch == nil {
		return nil
	}
	inv := snap.Batch
	ces := append([]ComputeEnvironmentDetail(nil), inv.ComputeEnvironments...)
	sort.SliceStable(ces, func(i, j int) bool {
		return ces[i].ComputeEnvironmentName < ces[j].ComputeEnvironmentName
	})

	var out []Advisory
	for _, ce := range ces {
		if !ce.Managed() || !ce.ComputeResources.EC2Backed() {
			continue
		}
		cr := *ce.ComputeResources
		if ad, ok := s.minVCpusAdvisory(ce, cr, inv); ok {
			out = append(out, ad)
		}
		if ad, ok := bestFitAdvisory(ce, cr); ok {
			out = append(out, ad)
		}
		if ad, ok := bidPercentageAdvisory(ce, cr); ok {
			out = append(out, ad)
		}
	}
	return out
}

// minVCpusAdvisory prices the idle floor — §3.2's "entire Batch cost finding".
func (s *Sizer) minVCpusAdvisory(ce ComputeEnvironmentDetail, cr ComputeResource,
	inv *BatchInventory) (Advisory, bool) {

	if cr.MinvCpus <= 0 {
		return Advisory{}, false
	}
	queues, enabled := inv.queuesFor(ce)

	var msg strings.Builder
	fmt.Fprintf(&msg, "AWS Batch compute environment %q (%s, %s) holds a floor of %d vCPU",
		ce.ComputeEnvironmentName, cr.Type, orNone(ce.State), cr.MinvCpus)
	if cr.MinvCpus != 1 {
		msg.WriteString("s")
	}
	msg.WriteString(" (minvCpus). AWS maintains that capacity \"even if the compute environment is DISABLED\", " +
		"so it bills whether or not a job ever runs")

	var gross float64
	rate, priced := s.floorRate(cr)
	if priced {
		gross = float64(cr.MinvCpus) * rate.hourlyPerVCPU * HoursPerMonth
		fmt.Fprintf(&msg, ". At the cheapest declared instance type's rate (%s, %s/vCPU-hour) that is at least "+
			"%s/hour, %s/month", rate.basis, fmtUSD(rate.hourlyPerVCPU),
			fmtUSD(float64(cr.MinvCpus)*rate.hourlyPerVCPU), fmtUSD(gross))
	} else {
		fmt.Fprintf(&msg, ". It could not be priced here: this compute environment declares instance types %q, "+
			"and none of them resolve to a catalog entry (AWS's `optimal`/`default_*` bundles pick families "+
			"that vary by region and change over time)", strings.Join(cr.InstanceTypes, ", "))
	}
	switch {
	case len(queues) == 0:
		msg.WriteString(". No job queue is attached to it at all, so nothing can currently use that capacity")
	case enabled == 0:
		fmt.Fprintf(&msg, ". Every job queue attached to it is disabled (%s), so nothing can currently submit "+
			"work to that capacity", strings.Join(queues, ", "))
	default:
		fmt.Fprintf(&msg, ". Attached job queues: %s", strings.Join(queues, ", "))
	}
	if strings.EqualFold(ce.State, BatchDisabled) {
		msg.WriteString(". The compute environment itself is DISABLED and the floor is being held anyway — " +
			"that is the documented behaviour, not a bug")
	}

	caveat := "advisory only, and the figure above is NOT claimed as a saving. minvCpus buys job START LATENCY " +
		"— instances that are already warm when work arrives — and kilter does not measure job start latency, " +
		"queue wait time, or what either is worth to you, so it cannot tell you whether this floor is waste or " +
		"the thing your deadlines depend on. Only an operator who knows the queue's SLO can. "
	if priced {
		caveat += "The cost is a LOWER bound: it prices every floor vCPU at the cheapest instance type the " +
			"compute environment declares, so the real bill is this or higher. "
	}
	caveat += "Lowering or removing the floor is an UpdateComputeEnvironment call against the AWS Batch API, " +
		"never an EC2 resize: AWS warns that modifying Batch-managed instances, Auto Scaling groups or ECS " +
		"clusters by hand causes INVALID compute environments and unexpected costs. kilter does not make that " +
		"call and has no actuator that could."

	return Advisory{
		Code:    AdvisoryBatchMinVCpusFloor,
		Message: msg.String(),
		Caveat:  caveat,
		// Gross is the list-price fantasy — what removing the floor outright
		// would stop costing. Net stays zero because the trade is capacity for
		// latency and kilter has measured only one side of it, so the report's
		// advisory-savings total does not absorb a dollar of this.
		GrossSavingsMonthlyUSD: gross,
		NetSavingsMonthlyUSD:   0,
	}, true
}

// bestFitAdvisory reports the default allocation strategy and its two
// documented costs. It is a report, permanently: changing allocationStrategy is
// an infrastructure update, and a BEST_FIT compute environment cannot receive
// one.
func bestFitAdvisory(ce ComputeEnvironmentDetail, cr ComputeResource) (Advisory, bool) {
	if cr.Strategy() != AllocationStrategyBestFit {
		return Advisory{}, false
	}
	how := fmt.Sprintf("is set to %s", AllocationStrategyBestFit)
	if strings.TrimSpace(cr.AllocationStrategy) == "" {
		how = fmt.Sprintf("names no allocationStrategy, which means %s — it is the documented default for an "+
			"Amazon ECS compute environment", AllocationStrategyBestFit)
	}
	return Advisory{
		Code: AdvisoryBatchBestFit,
		Message: fmt.Sprintf(
			"AWS Batch compute environment %q (%s) %s. AWS documents both of its costs: it \"keeps costs lower "+
				"but can limit scaling\" — jobs wait rather than start on a second-choice instance type — and "+
				"\"compute resources that use a BEST_FIT allocation strategy don't support infrastructure "+
				"updates and can't update some parameters\", so this compute environment cannot be modified in "+
				"place at all, including to change its AMI",
			ce.ComputeEnvironmentName, cr.Type, how),
		Caveat: "advisory only, and no saving is claimed: whether BEST_FIT is costing you depends on queue " +
			"wait time and instance-type availability, neither of which kilter observes. Moving off it is an " +
			"infrastructure update that BEST_FIT itself forbids, so the change is a create-new-compute-" +
			"environment, re-point-the-queue, delete-old migration — an operator decision on a resource AWS " +
			"Batch assumes full control of, and one kilter will never make for you.",
	}, true
}

// bidPercentageAdvisory reports a Spot price cap set against AWS's own advice.
func bidPercentageAdvisory(ce ComputeEnvironmentDetail, cr ComputeResource) (Advisory, bool) {
	if cr.Type != BatchCETypeSpot || cr.BidPercentage <= 0 {
		return Advisory{}, false
	}
	return Advisory{
		Code: AdvisoryBatchBidPercentage,
		Message: fmt.Sprintf(
			"AWS Batch SPOT compute environment %q sets bidPercentage=%d%%, so an instance launches only while "+
				"the Spot price is under %d%% of that type's on-demand price. AWS's own guidance is the "+
				"remediation: \"if you leave this field empty, the default value is 100%% of the On-Demand "+
				"price. For most use cases, we recommend leaving this field empty.\" You always pay the market "+
				"price, never the cap, so a low cap buys no discount — it only removes pools from consideration, "+
				"and is a common cause of a queue that mysteriously never scales",
			ce.ComputeEnvironmentName, cr.BidPercentage, cr.BidPercentage),
		Caveat: "advisory only, and no saving is claimed: kilter observes neither Spot pool prices nor queue " +
			"depth, so it cannot tell you whether this cap is currently blocking launches or has never once " +
			"bound. It is an availability finding with a cost consequence, not a priced change. Clearing the " +
			"field is an UpdateComputeEnvironment infrastructure update kilter never performs.",
	}, true
}

// BatchFixture replays recorded Batch responses through [BatchAPI], the way
// [Fixture] replays the two EC2 seams. It is deliberately a separate type: the
// Batch seam is optional, and an account recording that has never seen Batch
// should not grow two empty fields because of it.
type BatchFixture struct {
	// Pages are literal recorded DescribeComputeEnvironments pages, in order.
	Pages []DescribeComputeEnvironmentsOutput `json:"pages,omitempty"`
	// QueuePages are literal recorded DescribeJobQueues pages, in order.
	QueuePages []DescribeJobQueuesOutput `json:"queuePages,omitempty"`
	// CEFailAt and QueueFailAt fail the Nth call (1-based) with a transport
	// error. Zero disables. A Batch API that fails is the ordinary case in an
	// account whose role lacks batch:Describe*, so it is a first-class fixture
	// state, not an edge case.
	CEFailAt    int `json:"ceFailAt,omitempty"`
	QueueFailAt int `json:"queueFailAt,omitempty"`

	ceCalls    int
	queueCalls int
}

// DescribeComputeEnvironments replays the recorded compute-environment pages.
func (f *BatchFixture) DescribeComputeEnvironments(ctx context.Context,
	in *DescribeComputeEnvironmentsInput) (*DescribeComputeEnvironmentsOutput, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("ec2 batch fixture: nil DescribeComputeEnvironments input")
	}
	f.ceCalls++
	if f.CEFailAt > 0 && f.ceCalls == f.CEFailAt {
		return nil, fmt.Errorf("ec2 batch fixture: injected failure on DescribeComputeEnvironments call %d", f.ceCalls)
	}
	i, err := pageIndex(in.NextToken)
	if err != nil {
		return nil, err
	}
	if i >= len(f.Pages) {
		return &DescribeComputeEnvironmentsOutput{}, nil
	}
	out := f.Pages[i]
	out.NextToken = nextPageToken(i, len(f.Pages))
	return &out, nil
}

// DescribeJobQueues replays the recorded job-queue pages.
func (f *BatchFixture) DescribeJobQueues(ctx context.Context,
	in *DescribeJobQueuesInput) (*DescribeJobQueuesOutput, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("ec2 batch fixture: nil DescribeJobQueues input")
	}
	f.queueCalls++
	if f.QueueFailAt > 0 && f.queueCalls == f.QueueFailAt {
		return nil, fmt.Errorf("ec2 batch fixture: injected failure on DescribeJobQueues call %d", f.queueCalls)
	}
	i, err := pageIndex(in.NextToken)
	if err != nil {
		return nil, err
	}
	if i >= len(f.QueuePages) {
		return &DescribeJobQueuesOutput{}, nil
	}
	out := f.QueuePages[i]
	out.NextToken = nextPageToken(i, len(f.QueuePages))
	return &out, nil
}

// nextPageToken continues [Fixture]'s pagination convention — the token is the
// index of the next page — so both fixtures decode with the same pageIndex.
func nextPageToken(i, pages int) string {
	if i+1 >= pages {
		return ""
	}
	return strconv.Itoa(i + 1)
}
