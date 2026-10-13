#!/usr/bin/env bash
# Run tools/netprobe inside the private node pool under the same egress policy as the gateway, to
# show what the gateway can and cannot reach. Usage: ./60-netprobe.sh [-resolve ...] [-get ...]
source "$(dirname "$0")/00-env.sh"
: "${A_ECR:?run 30-a-eks.sh first}"
REPO="$(cd "$HERE/../.." && pwd)"; K="kubectl --context $CLUSTER"
if [ -z "${PROBE_IMG:-}" ]; then
  awsA ecr describe-repositories --repository-names sample-llm-gateway/netprobe >/dev/null 2>&1 || awsA ecr create-repository --repository-name sample-llm-gateway/netprobe --tags Key=project,Value="$LAB" >/dev/null
  save PROBE_IMG "$( cd "$REPO"; eval "$(AWS_PROFILE="$PROFILE_A" aws configure export-credentials --format env)"; export AWS_REGION="$REGION_A" KO_DOCKER_REPO="$A_ECR"
      ~/go/bin/ko build --base-import-paths --platform=linux/amd64,linux/arm64 --tags lab ./tools/netprobe 2>"$OUT/ko-netprobe.log" | tail -1 )"
fi
# Same egress rules as the gateway, selected by app=netprobe (reusing app=llm-gateway would trip the gateway's anti-affinity).
$K apply -f - >/dev/null <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: netprobe-egress-private-only, namespace: llm-gateway }
spec:
  podSelector: { matchLabels: { app: netprobe } }
  policyTypes: [Egress]
  egress:
    - to: [{ ipBlock: { cidr: 10.0.0.0/8 } }]
      ports: [{ protocol: TCP, port: 443 }, { protocol: UDP, port: 53 }, { protocol: TCP, port: 53 }]
    - to: [{ ipBlock: { cidr: $A_SERVICE_CIDR } }]
EOF
$K -n llm-gateway delete pod netprobe --ignore-not-found --wait >/dev/null 2>&1
$K -n llm-gateway run netprobe --rm -i --restart=Never --image="$PROBE_IMG" --labels=app=netprobe \
   --overrides='{"spec":{"nodeSelector":{"network-tier":"private"},"automountServiceAccountToken":false}}' \
   -- "$@" 2>&1 | grep -v "^pod .* deleted"
