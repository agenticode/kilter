# Security Policy

## Reporting

Please report suspected vulnerabilities privately via GitHub Security
Advisories ("Report a vulnerability" on the repository's Security tab).
You'll get an acknowledgement within 72 hours.

## Scope notes for operators

- The brain API mutates nothing by itself, but its data drives controllers
  that do. **Always set a token** (`KILTER_TOKEN` / helm `token`) and keep the
  brain off the public internet; put TLS in front (ingress/mesh) — the brain
  itself speaks plain HTTP.
- The agent is read-only (see its ClusterRole). The controller's RBAC is the
  minimum for its job: patch deployments/statefulsets, create evictions,
  patch/delete nodes. Grant `apply` mode deliberately.
- The container runs as nonroot on distroless with a read-only filesystem and
  all capabilities dropped; the binary is static (CGO disabled).
- Snapshots contain workload names, labels and usage numbers — treat brain
  storage (`kilter.db`) with the same care as cluster audit logs.
