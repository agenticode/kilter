# Contributing to Kilter

Thanks for helping keep clusters in kilter. The bar here is simple: **no bugs
tolerated — repeated testing is the way.**

## Ground rules

- `./test.sh` (gofmt, vet, race tests, build) must pass before every commit.
- New decision logic ships with tests that prove its behavior, including the
  ugly edges (empty inputs, boundaries, garbage). If it does math, consider a
  fuzz test — one already caught a real percentile bug (see git history).
- The decision engine (`pkg/model` → `pkg/plan`) stays **free of Kubernetes
  imports**. Collectors translate in, actuators translate out. This is what
  keeps the whole brain testable in milliseconds — please don't break it.
- stdlib-first. A new dependency needs a strong reason in the PR description.
- Safety behaviors (PDB, budgets, cooldowns, revert) are never opt-out flags
  in disguise. If you need to weaken one, make the default the safe side.

## Development

```console
$ ./test.sh                # fast local CI
$ make bench               # decision-engine benchmarks
$ ./test/e2e/e2e.sh        # full loop on a kind cluster (needs docker)
$ KEEP_CLUSTER=1 ./test/e2e/e2e.sh   # keep the cluster for poking around
```

## Commit style

Small, focused commits: `feat(binpack): …`, `fix(recommend): …`,
`test(e2e): …`, `docs: …`. If a commit fixes a bug, say how it was found and
what the failure looked like.

## Reporting decisions, not just bugs

If Kilter made a decision you disagree with, attach the snapshot:

```console
$ kilter analyze --dump-snapshot snapshot.json
```

Snapshots make decisions perfectly reproducible via `kilter simulate` —
that's the fastest possible path to a fix.
