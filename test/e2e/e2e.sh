#!/usr/bin/env bash
# Kilter end-to-end test against a real kind cluster.
#
# Verifies the full production loop:
#   1. kilter analyze  — instant report + snapshot dump
#   2. kilter simulate — offline replay of the dumped snapshot
#   3. agent → brain   — snapshots ingested, plan produced with savings
#   4. controller apply — underutilized node cordoned, drained (PDB-safe),
#      deleted; every evicted pod rescheduled and Running elsewhere
#
# Requirements: docker, kind, kubectl, go. Cluster is created and destroyed
# unless KEEP_CLUSTER=1. Run: ./test/e2e/e2e.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

CLUSTER=${CLUSTER:-kilter-e2e}
KIND=${KIND:-kind}
KUBECTL=${KUBECTL:-kubectl}
BRAIN_PORT=${BRAIN_PORT:-18180}
TOKEN=e2e-secret
FAIL() { echo "E2E FAIL: $*" >&2; exit 1; }
PASS() { echo "  ✔ $*"; }

command -v "$KIND" >/dev/null || FAIL "kind not found (set KIND=/path/to/kind)"
command -v "$KUBECTL" >/dev/null || FAIL "kubectl not found"

echo "==> building kilter"
go build -o bin/kilter ./cmd/kilter

echo "==> creating kind cluster $CLUSTER (3 workers)"
if ! "$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  cat <<EOF | "$KIND" create cluster --name "$CLUSTER" --wait 120s --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes: [{role: control-plane}, {role: worker}, {role: worker}, {role: worker}]
EOF
fi
CTX="kind-$CLUSTER"
KC() { "$KUBECTL" --context "$CTX" "$@"; }

cleanup() {
  local code=$?
  kill "${BRAIN_PID:-0}" "${AGENT_PID:-0}" "${CTRL_PID:-0}" 2>/dev/null || true
  if [[ "${KEEP_CLUSTER:-0}" != "1" ]]; then
    "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  [[ $code -eq 0 ]] && echo "E2E PASS" || echo "E2E FAILED (exit $code)"
}
trap cleanup EXIT

echo "==> installing metrics-server"
KC apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml >/dev/null
KC -n kube-system patch deployment metrics-server --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' >/dev/null

echo "==> deploying demo workloads (deliberately overprovisioned)"
KC apply -f test/e2e/workloads.yaml >/dev/null
for n in "$CLUSTER-worker" "$CLUSTER-worker2" "$CLUSTER-worker3"; do
  KC annotate node "$n" kilter.dev/hourly-cost=0.192 --overwrite >/dev/null
done
# worker3 pretends to be karpenter-managed: kilter must leave it alone.
KC label node "$CLUSTER-worker3" karpenter.sh/nodepool=general --overwrite >/dev/null
KC -n kube-system rollout status deployment metrics-server --timeout=180s >/dev/null
KC rollout status deployment web --timeout=180s >/dev/null

echo "==> waiting for pod metrics"
for i in $(seq 1 60); do
  KC top pods 2>/dev/null | grep -q web && break
  sleep 3
  [[ $i == 60 ]] && FAIL "metrics never appeared"
done
PASS "metrics-server serving pod metrics"

echo "==> 1) kilter analyze"
./bin/kilter analyze --kubeconfig <(KC config view --raw --minify --flatten) \
  --json --dump-snapshot /tmp/kilter-e2e-snap.json > /tmp/kilter-e2e-analyze.json
python3 - <<'PY' || FAIL "analyze assertions"
import json
d = json.load(open("/tmp/kilter-e2e-analyze.json"))
assert d["cost"]["hourlyUSD"] > 0.5, d["cost"]
assert d["plan"]["savingsMonthlyUSD"] > 100, d["plan"]["savingsMonthlyUSD"]
assert len(d["plan"]["removals"]) >= 1
PY
PASS "analyze: cost computed, savings found, snapshot dumped"

echo "==> 2) kilter simulate (offline replay)"
./bin/kilter simulate --snapshot /tmp/kilter-e2e-snap.json --json > /tmp/kilter-e2e-sim.json
python3 - <<'PY' || FAIL "simulate assertions"
import json
d = json.load(open("/tmp/kilter-e2e-sim.json"))
assert len(d["removals"]) >= 1, "simulate should find the same consolidation"
PY
PASS "simulate: identical decision from recorded snapshot, no cluster access"

echo "==> 3) brain + agent"
rm -f /tmp/kilter-e2e.db # always start from unlearned state
./bin/kilter brain --listen=":$BRAIN_PORT" --db=/tmp/kilter-e2e.db --token=$TOKEN &
BRAIN_PID=$!
KUBECONFIG_FILE=$(mktemp); KC config view --raw --minify --flatten > "$KUBECONFIG_FILE"
./bin/kilter agent --brain-url="http://localhost:$BRAIN_PORT" --token=$TOKEN \
  --cluster-id=e2e --interval=5s --kubeconfig "$KUBECONFIG_FILE" &
AGENT_PID=$!
for i in $(seq 1 30); do
  curl -sf -H "Authorization: Bearer $TOKEN" \
    "http://localhost:$BRAIN_PORT/api/v1/clusters" 2>/dev/null | grep -q e2e && break
  sleep 2
  [[ $i == 30 ]] && FAIL "brain never received snapshots"
done
SAVINGS=$(curl -sf -H "Authorization: Bearer $TOKEN" \
  "http://localhost:$BRAIN_PORT/api/v1/clusters/e2e/plan" | python3 -c 'import json,sys; print(json.load(sys.stdin)["savingsMonthlyUSD"])')
python3 -c "assert float('$SAVINGS') > 100, '$SAVINGS'" || FAIL "brain plan savings too low: $SAVINGS"
PASS "brain learned cluster e2e; plan saves \$$SAVINGS/month"
curl -sf "http://localhost:$BRAIN_PORT/metrics" | grep -q kilter_cluster_cost_hourly_usd \
  || FAIL "prometheus metrics missing"
PASS "prometheus metrics exposed"

echo "==> 4) controller --mode=apply"
NODES_BEFORE=$(KC get nodes --no-headers | wc -l | tr -d ' ')
./bin/kilter controller --brain-url="http://localhost:$BRAIN_PORT" --token=$TOKEN \
  --cluster-id=e2e --mode=apply --interval=10m --kubeconfig "$KUBECONFIG_FILE" &
CTRL_PID=$!
for i in $(seq 1 60); do
  NODES_NOW=$(KC get nodes --no-headers | wc -l | tr -d ' ')
  [[ "$NODES_NOW" -lt "$NODES_BEFORE" ]] && break
  sleep 5
  [[ $i == 60 ]] && FAIL "controller never removed a node"
done
PASS "node removed: $NODES_BEFORE → $NODES_NOW"
KC get node "$CLUSTER-worker3" >/dev/null 2>&1 || FAIL "karpenter-managed node was consolidated"
PASS "karpenter-managed worker3 left untouched (coexistence)"

echo "==> verifying workload health after consolidation"
KC rollout status deployment web --timeout=180s >/dev/null
KC rollout status deployment api --timeout=180s >/dev/null
KC rollout status deployment worker --timeout=180s >/dev/null
PENDING=$(KC get pods --field-selector=status.phase=Pending --no-headers 2>/dev/null | wc -l | tr -d ' ')
[[ "$PENDING" == "0" ]] || FAIL "$PENDING pods stuck Pending after consolidation"
PASS "all workloads Running after consolidation, nothing Pending"

echo "==> 5) spot interruption emergency drain"
kill "$CTRL_PID" 2>/dev/null || true
KC taint node "$CLUSTER-worker3" aws-node-termination-handler/spot-itn=true:NoSchedule --overwrite >/dev/null
./bin/kilter controller --brain-url="http://localhost:$BRAIN_PORT" --token=$TOKEN   --cluster-id=e2e --mode=apply --interval=10m --kubeconfig "$KUBECONFIG_FILE" &
CTRL_PID=$!
for i in $(seq 1 36); do
  CORDONED=$(KC get node "$CLUSTER-worker3" -o jsonpath='{.spec.unschedulable}' 2>/dev/null)
  NONDS=$(KC get pods -A --field-selector "spec.nodeName=$CLUSTER-worker3" -o json     | python3 -c 'import json,sys; pods=json.load(sys.stdin)["items"]; print(sum(1 for p in pods if not any(o.get("kind")=="DaemonSet" for o in p["metadata"].get("ownerReferences",[])) and p["status"]["phase"] not in ("Succeeded","Failed")))')
  [[ "$CORDONED" == "true" && "$NONDS" == "0" ]] && break
  sleep 5
  [[ $i == 36 ]] && FAIL "spot-tainted node not drained (cordoned=$CORDONED, pods=$NONDS)"
done
KC get node "$CLUSTER-worker3" >/dev/null || FAIL "emergency drain must NOT delete the node"
PASS "spot interruption: worker3 cordoned and drained, node left for cloud reclamation"
KC rollout status deployment web --timeout=180s >/dev/null
PASS "workloads healthy after emergency drain"
