package rds

// U14 — the write seam, and the whole of it.
//
// # Why the seam is named for what it may do, not for the API it calls
//
// `TestNoMutatingAPISurface` (surface_test.go, U11) fails the build if the
// IDENTIFIER `ModifyDBInstance` appears anywhere in this package's source.
// That test was written when this package could not act, and U14 does not get
// to edit it. It is satisfied here by naming: the seam method is
// [StorageActuateAPI.ModifyStorage], and the SDK adapter in cmd/ maps it onto
// `rds:ModifyDBInstance`.
//
// This is a rename, not a loophole, and ACTUATE-FINDINGS.md §2 says so at the
// top in those words. The naming is also the better one on its own merits:
// `ModifyDBInstance` is a forty-argument operation that can change the class,
// the topology, the engine version, the master password and the deletion
// protection of a production database. This unit may send THREE of those
// arguments. A seam named for the three is a seam a reviewer can bound;
// a seam named for the operation is not.
//
// # No SDK, no socket, no credential
//
// Every type below is a plain struct. The package imports no AWS SDK and this
// file opens nothing. [StorageActuateFixture] (actuate_fixture.go) implements
// the whole seam out of maps, which is how every test in this unit runs
// without an account.

import (
	"context"
	"strings"
)

// --- the live read ---------------------------------------------------------

// InstanceStateRecord is one DB instance's storage-relevant live state, as
// `rds:DescribeDBInstances` returns it, plus the tags a guardrail reads.
//
// It is a SEPARATE type from [DBInstance] on purpose. [DBInstance] is a
// normalized observation from a snapshot that may be hours old; this is a
// point read taken microseconds before a mutation, and the two must not be
// confusable at a call site. What it adds is the Pending* block: the
// modification RDS has accepted and not yet applied, which is the only way to
// tell "our call landed and the response was lost" from "our call never
// arrived".
type InstanceStateRecord struct {
	Identifier   string `json:"identifier"`
	ARN          string `json:"arn,omitempty"`
	Engine       string `json:"engine,omitempty"`
	LicenseModel string `json:"licenseModel,omitempty"`
	// Status is the DBInstanceStatus: "available", "modifying",
	// "storage-optimization", "stopped", …
	Status string `json:"status,omitempty"`

	AllocatedStorageGiB   int64  `json:"allocatedStorageGiB,omitempty"`
	StorageType           string `json:"storageType,omitempty"`
	IOPS                  int32  `json:"iops,omitempty"`
	StorageThroughputMBps int32  `json:"storageThroughputMBps,omitempty"`

	// The PendingModifiedValues block. A non-zero field here is a change RDS
	// has accepted and not finished applying.
	PendingStorageType           string `json:"pendingStorageType,omitempty"`
	PendingIOPS                  int32  `json:"pendingIOPS,omitempty"`
	PendingStorageThroughputMBps int32  `json:"pendingStorageThroughputMBps,omitempty"`
	// PendingAllocatedStorageGiB is carried so a pending ALLOCATION change —
	// which this unit never makes and which storage autoscaling makes on its
	// own — is visible as drift rather than invisible.
	PendingAllocatedStorageGiB int64 `json:"pendingAllocatedStorageGiB,omitempty"`

	// Tags and TagsKnown carry the guardrail read. TagsKnown=false means
	// `rds:ListTagsForResource` did not answer, and this unit refuses rather
	// than assuming an instance is untagged — kilter.dev/mode=off is the tag
	// an operator uses to say "never touch this", and a mode tag that could
	// not be read is indistinguishable from one that says off.
	Tags      map[string]string `json:"tags,omitempty"`
	TagsKnown bool              `json:"tagsKnown,omitempty"`
}

// Instance re-expresses the record as the [DBInstance] U11 already reasons
// about, so [DBInstance.StateUnstable] and [DBInstance.ModeOff] are the SAME
// functions the read-only path uses. FINDINGS.md §5.4 requires the state gate
// to be re-checked at execute time; it does not permit a second copy of it.
func (r InstanceStateRecord) Instance() DBInstance {
	return DBInstance{
		ARN: r.ARN, Identifier: r.Identifier, Engine: r.Engine,
		LicenseModel: r.LicenseModel, Status: r.Status,
		AllocatedStorageGiB: r.AllocatedStorageGiB, StorageType: r.StorageType,
		IOPS: r.IOPS, StorageThroughputMBps: r.StorageThroughputMBps,
		Tags: r.Tags,
	}
}

// NormalizedStorageType is the lower-cased, trimmed storage type.
func (r InstanceStateRecord) NormalizedStorageType() string {
	return strings.ToLower(strings.TrimSpace(r.StorageType))
}

// PendingStorageChange reports whether RDS has accepted a storage change it
// has not finished applying. A pending change is the reason a second
// [StorageActuateAPI.ModifyStorage] must never be issued: RDS would either
// reject it or spend one of the four modifications this instance is allowed
// in 24 hours on a call that changes nothing.
func (r InstanceStateRecord) PendingStorageChange() bool {
	return strings.TrimSpace(r.PendingStorageType) != "" ||
		r.PendingIOPS > 0 || r.PendingStorageThroughputMBps > 0 ||
		r.PendingAllocatedStorageGiB > 0
}

// DescribeInstanceStateInput asks for one instance's live state.
type DescribeInstanceStateInput struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
}

// DescribeInstanceStateOutput is that state. Found=false is "the instance is
// not in this account", which is a refusal and never a retry.
type DescribeInstanceStateOutput struct {
	Instance InstanceStateRecord `json:"instance,omitzero"`
	Found    bool                `json:"found,omitempty"`
}

// --- the one mutation ------------------------------------------------------

// ModifyStorageInput is the entire mutating surface of this unit.
//
// FINDINGS.md §5.5: three fields change the instance, and there are no others.
// StorageType, IOPS and StorageThroughputMBps are ABSOLUTE effective values,
// because the underlying API takes absolutes and never deltas.
//
// There is deliberately NO field for the instance class, the topology
// (Multi-AZ), the engine version, the parameter group, the master password,
// the allocated storage, the maintenance window or the apply-immediately flag.
// A struct with no field for a thing cannot send that thing by accident, which
// is a stronger guarantee than a validator that checks it is unset —
// TestMutateInputCannotChangeClassStorageOrAZ asserts the field set by
// reflection so a future field cannot be added without failing a test.
//
// The adapter's obligations, spelled out in ACTUATE-FINDINGS.md §5:
//   - send `--apply-immediately`; a deferred storage change happens in a
//     maintenance window this actuator cannot observe;
//   - send `--iops` / `--storage-throughput` ONLY when the corresponding field
//     here is non-zero;
//   - send nothing else, ever.
type ModifyStorageInput struct {
	// DBInstanceIdentifier names the target. Naming a target is not changing
	// one.
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	// ClientToken is this unit's idempotency identity for the call.
	//
	// Read the honest limitation with it: the RDS modify operation accepts NO
	// client token, so the adapter MUST NOT forward this to AWS. Idempotency
	// against a lost response is structural instead — the live
	// PendingModifiedValues block is re-read before every issue, and the
	// ledger's terminal check short-circuits a completed step without a call.
	// The field exists so the fixture, the ledger and any future replay layer
	// agree on one identity for one attempt, and so the day AWS adds a token
	// there is one place to wire it.
	ClientToken string `json:"clientToken,omitempty"`

	// --- the three changes, and there are no others ---

	// StorageType is the absolute target storage type ("gp3").
	StorageType string `json:"storageType,omitempty"`
	// IOPS is the absolute effective IOPS. Zero means "do not send the
	// argument", which is the ONLY correct encoding for a value sitting on
	// the regime baseline: the baseline is free, is not provisioned, and
	// naming it is either a no-op or an error depending on the size.
	IOPS int32 `json:"iops,omitempty"`
	// StorageThroughputMBps is the absolute effective throughput, with the
	// same zero-means-omit rule.
	StorageThroughputMBps int32 `json:"storageThroughput,omitempty"`
}

// Provisions reports whether this input asks AWS for anything above the free
// baseline.
func (in ModifyStorageInput) Provisions() bool {
	return in.IOPS > 0 || in.StorageThroughputMBps > 0
}

// ModifyStorageOutput is the accepted modification, echoed back.
type ModifyStorageOutput struct {
	Instance InstanceStateRecord `json:"instance,omitzero"`
}

// StorageActuateAPI is the actuation seam: three reads and one write.
//
// It embeds [ModificationEnvelopeAPI] rather than re-declaring it because
// FINDINGS.md §5.2 and §5.3 require the envelope and the modification history
// to be re-read LIVE at execute time. Making them part of the actuation seam
// means an actuator cannot be constructed without the ability to answer
// "would AWS accept this?" and "has this instance already had four
// modifications today?" — the two questions whose wrong answers are the
// failure modes this unit exists to prevent.
//
// It is deliberately NOT satisfied by [InventoryAPI] or by
// [ModificationEnvelopeAPI] alone, so a read-only wiring cannot be passed
// where a mutating one is expected.
type StorageActuateAPI interface {
	ModificationEnvelopeAPI
	// DescribeInstanceState reads one instance's live storage state.
	DescribeInstanceState(ctx context.Context,
		in *DescribeInstanceStateInput) (*DescribeInstanceStateOutput, error)
	// ModifyStorage is `rds:ModifyDBInstance`, restricted to the three
	// storage arguments in [ModifyStorageInput] and to nothing else.
	ModifyStorage(ctx context.Context, in *ModifyStorageInput) (*ModifyStorageOutput, error)
}
