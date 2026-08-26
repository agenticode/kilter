// Package explain is Kilter's deterministic explanation plane: the full
// explain payload behind one recommendation, and `why-cost` — an additive,
// individually-citable decomposition of a cluster's cost change over a
// window (docs/design/reasoning-engine.md §4.5, implementation unit 4).
//
// Nothing here consults a model, a clock, or a network. It is what makes it
// safe to let a language model narrate cost stories later (§5): the model may
// only quote terms this package computed, against evidence IDs this package
// can resolve. A term the narrator cannot ground in an evidence ID is worse
// than a missing term, so this package never emits one — every Term, every
// Driver and the residual itself carry at least one ID, and BuildExplain and
// WhyCost fail loudly rather than return an uncitable number.
//
// # The invariant
//
// The decomposition satisfies, exactly and always:
//
//	sum(Terms) + Residual == Delta
//
// "Exactly" is meant literally: every amount is a [Micro], an int64 count of
// millionths of a dollar, so the sum is integer arithmetic — associative,
// order-independent, and byte-reproducible. Residual is computed last, as
// Delta minus the terms, and is reported as residual. A decomposition that
// quietly absorbed its error into the largest term would be a lie with a
// number attached; a large residual is merely an admission, and an admission
// is the only honest thing to do with cost you cannot account for.
//
// The float64 arithmetic that does exist — the share algebra inside a single
// term — is summed with [sumSorted] over a canonically ordered slice and
// quantized to Micro exactly once. Float addition is not associative, so a
// total that depends on the order groups arrived in is not a function of its
// inputs; pkg/ecs shipped exactly that bug this week. Sort, then sum.
//
// # Attribution order — and why it is this one
//
// Node count, instance mix, spot ratio and unit prices overlap: buying five
// more nodes *of a cheaper type* is simultaneously a volume change and a mix
// change, and whichever factor is moved first collects the interaction. There
// is no order-free answer, only a chosen one. This package chooses:
//
//		node-count → spot-ratio → instance-mix → pricing-catalog
//
//	  - **node-count first**, measured at the window's starting mix and
//	    starting prices. It is the one term an operator can check independently
//	    — node count is a number they already know, and the reference price is
//	    printed in the term's facts. Had mix gone first, node-count would be
//	    measured against a fleet shape nobody remembers, and the number an
//	    operator can verify would be the one they cannot reproduce.
//
//	  - **spot-ratio before instance-mix.** Capacity type is a *policy* —
//	    someone (Kilter, or an operator) decided to run more spot. Instance mix
//	    is largely a *consequence*: the autoscaler picks shapes. Attributing the
//	    interaction to the consequence keeps the policy term equal to "what
//	    flipping that policy would have cost at the old fleet shape", which is
//	    the counterfactual a human is actually checking.
//
//	  - **pricing-catalog last**, weighted at end-of-window node counts (a
//	    Paasche price index). A price change you did not cause should be
//	    measured against the fleet you actually run now, not one you retired
//	    mid-window. A group that appeared during the window has no price change
//	    to report — it borrows the other edge's price — so "catalog" always
//	    means "prices moved", never "the fleet moved".
//
// Order is not an implementation detail to be "fixed" later into a different
// set of numbers: TestAttributionOrderIsTheDocumentedOne pins it with a
// fixture where the alternative order gives a different, also-defensible
// answer, and [Attribution.Order] echoes the convention into every payload.
//
// # Why node-count has children instead of siblings
//
// Workload-set change and Kilter's own actions are *drivers of* node count,
// not independent factors of the price identity Cost = Σ nodes × price. Emitted
// as siblings they would double-count: the nodes a new namespace forced into
// existence are already inside the node-count term. They are therefore
// sub-attributions, in [Term.Of], with their own exact invariant —
// sum(Of) == parent — and their own explicit "unattributed" remainder. The
// nesting is exactly one level deep; a deeper tree is a tree nobody audits.
//
// Within node-count the order is again deliberate: Kilter's confirmed node
// actions come out first because they are *counted* from the ledger, while
// the workload-set/workload-scaling split is a *proportional inference*. Facts
// before inferences.
//
// # What is deliberately not attributed
//
// The realized cost move across a Kilter action's execution window is
// reported as a fact on the kilter-action term and never used as its value.
// Correlating a cost change with whatever action ran at the same time is the
// single most common way cost attribution lies (a deploy blamed for a cron
// job's cost); this package enumerates candidates deterministically and lets
// the number that is actually attributable — nodes Kilter is recorded as
// having removed, at a stated reference price — carry the term.
//
// # Units
//
// The decomposition is over the *hourly run rate*, not integrated spend. The
// timeline stores a rate, a rate is what a fleet change moves, and integrating
// it would silently assume the timeline has no gaps. Monthly projections are
// derived per term (×730) for readability and never summed.
//
// # Dependency direction
//
// stdlib, pkg/model, pkg/evidence, pkg/recommend and pkg/decision. Notably
// *not* pkg/api: the audit ledger lives there, above every decision package,
// so its entries reach this package through the local [LedgerAction]
// projection. FINDINGS.md specifies the field-by-field mapping the wiring
// must perform.
package explain
