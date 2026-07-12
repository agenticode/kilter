# The trust package: guardrails, approvals, verifiable savings

Automated cost optimizers fail adoption reviews on four fears. Kilter ships a
first-class answer to each — not as settings buried in a console, but as
plain Kubernetes annotations, auditable APIs, and CLI verbs.

## Fear 1: "It will touch things it shouldn't"

Per-workload / per-namespace **modes**, resolved workload-first:

```yaml
metadata:
  annotations:
    kilter.dev/mode: "off"        # off | recommend | apply
```

- `off` — Kilter neither resizes nor moves these pods; their nodes are never
  consolidated out from under them. It doesn't even list recommendations.
- `recommend` — full learning and visibility, zero automation.
- `apply` — full automation (the default; the controller's own
  `--mode=dry-run` global gate still applies on top).

**Change windows** confine node surgery to operator-chosen hours:

```console
$ kilter controller --mode=apply --change-windows "Mon-Fri 22:00-06:00"
```

Resizes (non-disruptive: in-place or rolling) run anytime; cordon/evict/
delete wait for the window.

**The freeze switch** stops everything at once, GitOps-visible:

```console
$ kubectl annotate namespace kube-system kilter.dev/freeze=true
```

## Fear 2: "It will optimize during an incident"

The **circuit breaker** checks cluster health before every plan execution:
too many NotReady nodes (>20%) or a Pending-pod surge (>10) opens the breaker
— Kilter keeps observing and recommending but stops acting, and says why in
its logs. Spot-interruption emergency drains still run (those machines are
dying regardless); a freeze stops even those.

## Fear 3: "The claimed savings are marketing math"

Commercial optimizers charge a percentage of savings they themselves compute.
Kilter's **audit ledger** makes the math checkable:

```console
$ kilter ledger

  WHEN         MODE   FINGERPRINT       STEPS  COST BEFORE  PROJECTED  CLAIMED/MO
  07-12 21:04  apply  3f9c2a81d0e47b66  4 ok   $0.768/h     $0.576/h   $140.16

  measured cost now: $0.576/h   realized: $140.16/month
  realized = (measured hourly cost before first applied action − latest measured hourly cost) × 730
```

Every executed plan is recorded with what it claimed *and* the measured cost
timeline around it (`GET /api/v1/clusters/{id}/ledger`). The realized number
is computed from observable prices with the formula printed next to it —
judge it against your invoice, not our dashboard.

## Fear 4: "I can't undo what it did"

Every ledger entry stores the **From values** of everything it changed:

```console
$ kilter undo --cluster prod            # or --dry-run to preview
  ✔ undo: web back to 2000m/4.0Gi
  ✔ undo: uncordon node-c
```

Resizes revert exactly; cordons lift. Deleted nodes and completed evictions
are irreversible by nature and reported as such — Kilter never pretends
otherwise. (Automatic revert on OOM/crashloop regression is separate and
always on.)

## Bonus: humans in the loop, when you want them

`--require-approval` turns the controller into a proposer. Plans carry a
deterministic **fingerprint** of their content (same intended actions → same
fingerprint, timestamps irrelevant), and nothing executes until someone runs:

```console
$ kilter plan                        # review the steps + fingerprint
$ kilter approve 3f9c2a81d0e47b66    # valid 24h, per cluster, write-token only
```

If the cluster drifts and the plan's content changes, the fingerprint changes
and the old approval no longer applies — you approve *actions*, not a moment
in time.
