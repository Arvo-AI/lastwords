#!/usr/bin/env bash
# End-to-end test on a local kind cluster: install lastwords, run four failing
# workloads, and check that every kind of capture shows up.
set -euo pipefail
cd "$(dirname "$0")/../.."
CLUSTER=lastwords
CTX=kind-$CLUSTER
K="kubectl --context $CTX"

if ! kind get clusters | grep -qx $CLUSTER; then
  kind create cluster --config test/e2e/kind.yaml
fi

echo "== build images"
docker build -q -t ghcr.io/arvo-ai/lastwords:e2e . >/dev/null
for d in leaky-java leaky-go segfault panic; do
  docker build -q -t lastwords-demo/$d:dev examples/$d >/dev/null
done
kind load docker-image --name $CLUSTER ghcr.io/arvo-ai/lastwords:e2e lastwords-demo/leaky-java:dev lastwords-demo/leaky-go:dev lastwords-demo/segfault:dev lastwords-demo/panic:dev

echo "== install lastwords"
helm --kube-context $CTX upgrade --install lastwords deploy/helm/lastwords -n lastwords --create-namespace \
  --set image.tag=e2e --set image.pullPolicy=Never \
  --set coreDumps.enabled=true --set coreDumps.hostDir=/var/lib/lastwords-kind \
  --set clusterName=kind-e2e --wait --timeout 3m
# Same tag, new image: restart so the agent runs the build we just loaded.
$K -n lastwords rollout restart ds/lastwords
$K -n lastwords rollout status ds/lastwords --timeout 2m

echo "== deploy failing workloads"
$K delete namespace shop --ignore-not-found --wait
$K apply -f examples/demo.yaml

make build >/dev/null
LW="./dist/lastwords --context $CTX"
want=(pressure oomkill crash coredump)

# Core dumps: on most kernels core_pattern is node-wide and the segfaulting demo
# produces a real core. Some local VMs (OrbStack 7.x) namespace core_pattern per
# container, so we also push a synthetic core through the installed helper to
# check the helper -> pickup -> pod matching path everywhere.
NODE=$CLUSTER-control-plane
for _ in $(seq 1 30); do
  PID=$(docker exec $NODE sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = billing-api ] && basename $p && break; done' || true)
  [[ -n "$PID" ]] && break; sleep 2
done
docker exec $NODE sh -c "head -c 2000000 /dev/urandom | /var/lib/lastwords/bin/lastwords core-handler $PID 11 \$(date +%s) billing-api"
deadline=$((SECONDS + ${E2E_TIMEOUT:-300}))
while true; do
  kinds=$($LW list -n shop --since 1h -o json | python3 -c 'import json,sys; print(" ".join(sorted({c["kind"] for c in (json.load(sys.stdin) or [])})))')
  echo "captures so far: ${kinds:-none}"
  missing=0
  for k in "${want[@]}"; do [[ " $kinds " == *" $k "* ]] || missing=1; done
  [[ $missing == 0 ]] && break
  if (( SECONDS > deadline )); then echo "FAIL: missing capture kinds"; $LW list -n shop --since 1h; exit 1; fi
  sleep 10
done
$LW list -n shop --since 1h
echo "PASS"
