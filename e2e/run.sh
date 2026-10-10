#!/usr/bin/env bash
# End-to-end test: needs a kind cluster (kubectl context set), a built
# binary at $KRD_BIN and jq. Deploys Prometheus + the base workloads,
# waits for real cAdvisor data, then checks the head manifests against it.
set -euo pipefail
cd "$(dirname "$0")"
KRD_BIN=${KRD_BIN:-../kube-rightsize-diff}
OUT=${OUT:-/tmp/krd-e2e}
mkdir -p "$OUT"

kubectl apply -f prometheus.yaml -f workload-base.yaml
kubectl -n monitoring rollout status deploy/prometheus --timeout=180s
kubectl -n e2e rollout status deploy/idler --timeout=180s
kubectl -n e2e rollout status deploy/spinner --timeout=180s

kubectl -n monitoring port-forward svc/prometheus 9090:9090 >"$OUT/port-forward.log" 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
for _ in $(seq 30); do curl -sf localhost:9090/-/ready >/dev/null && break; sleep 2; done

echo "waiting for cAdvisor series for namespace e2e..."
q='count(container_cpu_usage_seconds_total{namespace="e2e",container="app"})'
for i in $(seq 60); do
  n=$(curl -s --data-urlencode "query=$q" localhost:9090/api/v1/query | jq -r '.data.result[0].value[1] // "0"')
  [[ "$n" -ge 3 ]] && break
  [[ $i -eq 60 ]] && { echo "no cAdvisor data"; curl -s localhost:9090/api/v1/targets | jq '.data.activeTargets[] | {health, lastError}'; exit 1; }
  sleep 5
done
echo "series present ($n); collecting 2 minutes of history"
sleep 120

set +e
"$KRD_BIN" --head workload-head.yaml --base workload-base.yaml \
  --prometheus-url http://localhost:9090 --window 2m --resolution 10s --rate-interval 1m \
  --format markdown --output "$OUT/report.md" --json-output "$OUT/report.json" --fail-on risk
code=$?
set -e
cat "$OUT/report.md"

fail() { echo "E2E FAIL: $*"; exit 1; }
[[ $code -eq 2 ]] || fail "expected exit 2 from --fail-on risk, got $code"
r() { jq -r --arg w "$1" '.results[] | select(.workload == $w)' "$OUT/report.json"; }

[[ "$(r idler | jq '.usage.pods')" == "2" ]] || fail "idler should have usage from 2 pods"
r idler | jq -e '.findings | map(.rule) | index("cpu-over-provisioned")' >/dev/null ||
  fail "idler (1 core request, ~0 used) should be cpu-over-provisioned"
r spinner | jq -e '.usage.cpu_cores > 0.2' >/dev/null || fail "spinner should show real CPU use"
r spinner | jq -e '.findings | map(.rule) | index("cpu-request-below-usage")' >/dev/null ||
  fail "spinner (10m request, busy loop) should be cpu-request-below-usage"
echo "E2E PASS"
