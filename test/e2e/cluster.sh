#!/usr/bin/env bash
# End-to-end test on a real cluster (EKS, GKE, ...): install the published
# chart, run the four failing demo workloads, and check every capture kind
# shows up with the right findings.
#
#   REGISTRY=123.dkr.ecr.us-east-2.amazonaws.com/lastwords-demo \
#   KUBECONFIG=~/.kube/eks test/e2e/cluster.sh [chart-version]
#
# REGISTRY must be pushable from here and pullable by the nodes. Images are
# built for the node architecture (PLATFORM, default linux/amd64).
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${REGISTRY:?set REGISTRY to a repository prefix the nodes can pull from}"
VERSION=${1:-}
PLATFORM=${PLATFORM:-linux/amd64}
TAG=${TAG:-e2e-$(date +%s)}   # set TAG and SKIP_BUILD=1 to use images already pushed
K=kubectl

if [[ -z "${SKIP_BUILD:-}" ]]; then
  echo "== build and push demo images ($PLATFORM)"
  for d in leaky-java leaky-go segfault panic; do
    docker buildx build -q --platform "$PLATFORM" -t "$REGISTRY/$d:$TAG" --push "examples/$d" >/dev/null
  done
fi

echo "== install lastwords ${VERSION:-latest} from ghcr"
helm upgrade --install lastwords oci://ghcr.io/arvo-ai/charts/lastwords ${VERSION:+--version "$VERSION"} \
  -n lastwords --create-namespace \
  --set coreDumps.enabled=true --set clusterName=e2e --wait --timeout 5m
$K -n lastwords rollout status ds/lastwords --timeout 3m

echo "== deploy failing workloads"
$K delete namespace shop --ignore-not-found --wait
sed -e "s#lastwords-demo/\([a-z-]*\):dev#$REGISTRY/\1:$TAG#" -e 's/imagePullPolicy: Never/imagePullPolicy: IfNotPresent/' \
  examples/demo.yaml | $K apply -f -

make build >/dev/null
LW=./dist/lastwords
want=(pressure oomkill crash coredump)
deadline=$((SECONDS + ${E2E_TIMEOUT:-600}))
while true; do
  kinds=$($LW list -n shop --since 1h -o json | python3 -c 'import json,sys; print(" ".join(sorted({c["kind"] for c in (json.load(sys.stdin) or [])})))')
  echo "captures so far: ${kinds:-none}"
  missing=0
  for k in "${want[@]}"; do [[ " $kinds " == *" $k "* ]] || missing=1; done
  [[ $missing == 0 ]] && break
  if (( SECONDS > deadline )); then echo "FAIL: missing capture kinds"; $LW list -n shop --since 1h; exit 1; fi
  sleep 15
done
$LW list -n shop --since 1h

echo "== check findings"
fail=0
check() { # kind, container, text the summary must contain
  local id
  id=$($LW list -n shop --since 1h --kind "$1" --container "$2" -o json |
    python3 -c 'import json,sys; c=json.load(sys.stdin) or []; print(c[0]["id"] if c else "")')
  if [[ -z "$id" ]]; then echo "FAIL: no $1 capture for $2"; fail=1; return; fi
  if $LW show "$id" | grep -q "$3"; then echo "ok   $1/$2 names $3"
  else echo "FAIL: $1/$2 ($id) does not mention $3"; $LW show "$id"; fail=1; fi
}
check pressure checkout CartSnapshot
check oomkill billing-api renderInvoice
check crash consumer panic
check coredump worker SIGSEGV
[[ $fail == 0 ]] && echo PASS || exit 1
