# pkg/actuate hardening findings

Session scope: pkg/actuate only. All changes verified with
`gofmt && go vet && go build ./... && go test -race -count=1 ./pkg/actuate/...`
and the full `go test -race -short ./...` (all packages green).

## Bugs found and fixed

1. **`ownedBy` matched sibling deployments by name prefix** (worst bug).
   Deployment `api` matched ReplicaSet `api-gateway-<hash>` via
   `strings.HasPrefix`, so with `InPlaceResize` on, resizing `api` patched the
   *running pods of a different workload* with the wrong resources — instant
   under-provisioning of an innocent service. Fixed by reconstructing the exact
   ReplicaSet name from the pod's `pod-template-hash` label
   (`<deployment>-<hash>`). Verified fail-before/pass-after with `TestOwnedBy`
   and `TestInPlaceResizeTargetsOnlyOwnedPods`.

2. **`DeleteNode` silently swallowed the providerID read error.**
   `Get` failure (e.g. transient apiserver error) left providerID empty and
   deleted the Node object anyway. The Node object is the only record of the
   providerID; EKS `TerminateNode` cannot work without it — the instance would
   bill forever with no retry path. Now, with a real provider, a non-NotFound
   read error fails the step *before* the delete (next reconcile retries);
   NotFound still proceeds so termination is confirmed, never assumed. With
   `provider.None` the old tolerant behavior is kept (no ID needed). Verified
   fail-before/pass-after with `TestDeleteNodeProviderIDReadFailure`.

3. **Unknown `Mode` strings executed as apply.** `Config{Mode: "aply"}` fell
   through the `== ModeDryRun` check and mutated the cluster. `New` now rejects
   any mode other than dry-run/apply (empty still defaults to dry-run).
   `TestNewRejectsUnknownMode`.

4. **Dry-run/apply parity for unknown step types.** Dry-run reported unknown
   steps as `dry-run` (counted toward `Done`); apply skipped them. Previews now
   skip them identically. `TestUnknownStepSkippedInBothModes`.

5. **Garbage resizes were silently reported "done".** Negative quantities were
   dropped by `resourcesToK8s`, producing a no-op patch recorded as success in
   the ledger; an empty container name would strategic-merge-*append* a broken
   nameless container to the pod template. `validateResize` now rejects both,
   in apply and in dry-run previews (so garbage surfaces on the preview).
   `TestResizeWorkloadRejectsGarbage`, `TestDryRunFlagsGarbageResize`.

6. **`ExecutePlan(ctx, nil)` panicked** on nil plan; now returns an empty
   stamped report. `TestExecutePlanNilPlan`.

7. **Silent error swallows in `resizePodsInPlace`** (pod list failure, patch
   marshal) now log a warning naming the workload; behavior stays best-effort
   by design since the template rollout converges regardless.

8. **`EvictPod` accepted refs with empty namespace or name** (`"ns/"`, `"/p"`).
   Now rejected before the budget check. `TestEvictPodMalformedRefDoesNotConsumeBudget`.

## Tests added (~20 cases beyond the fixes above)

- PDB 429 retry loop: retry-then-success attempt count, gives-up-after-4 with
  the 429 surfaced, and context cancellation during backoff (the backoff base
  is now an unexported `evictBackoff` field so tests don't sleep 10s).
- `WaitNodeEmpty`: mirror-pod annotation exemption; context cancellation wins
  over a long drain timeout.
- `DeleteNode`: already-gone node still confirms termination with the provider.
- Zero-field resize semantics: memory-only resize leaves the cpu request
  untouched (documented: zero ⇒ "leave unchanged", required by the undo path).
- `Config.withDefaults` boundary table (zero/negative → defaults, explicit kept).
- `resourcesToK8s` table incl. int64 extremes, plus `FuzzResourcesToK8s`
  asserting every emitted quantity parses and round-trips exactly (a wrong
  `m` suffix would silently resize by 1000×). Fuzzed 20s / ~560k execs clean.
- `ExecutePlan` with already-canceled context aborts before any step.

## Invariants documented (comments/godoc)

- Status constants (`StatusDone` etc.) replacing magic strings; JSON values
  unchanged, so pkg/api ledger consumers are unaffected.
- `Report.Done` includes dry-run previews; aborted plans omit never-run steps.
- Budget slot is consumed per eviction attempt even if the pod is already gone
  (conservative: can only slow us down, never over-disrupt).
- providerID-before-delete ordering in `DeleteNode` and why.
- `ownedBy` false negatives are safe (rollout converges); false positives are not.

## Deliberately left undone

- **Empty (all-zero) resizes are still accepted as no-ops**: the controller's
  regression-revert path (`cmd/kilter/controller.go:129`) calls
  `ResizeWorkload` with `FromReq/FromLim`, which are legitimately all-zero when
  the original workload had no requests/limits. Rejecting them would break that
  caller; note the revert is then a silent no-op — a latent issue in the
  *controller's* revert semantics, outside this package's scope.
- **Direct mutator calls ignore dry-run mode** (`a.Cordon(...)` on a dry-run
  actuator still cordons). Only `ExecutePlan` gates on mode. Changing exported
  method contracts mid-flight risks breaking concurrent work in cmd/kilter
  (trust.go calls mutators directly); flagged rather than changed.
- `resizePodsInPlace` lists all pods in the namespace (no label selector) —
  inefficient on huge namespaces but correct; picking selectors would require
  reading the live workload spec, a second API call on a best-effort path.
- The `Provider.Name() != "none"` string check in `DeleteNode` trusts the
  provider naming contract (a custom provider named "none" would be skipped);
  that contract is documented in pkg/provider.
