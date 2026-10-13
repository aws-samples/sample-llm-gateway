#!/usr/bin/env bash
# STS two-hop experiments for the misconfigurations described in docs/cross-account-private-networking.md §3. Each case runs tools/stsprobe as a pod with
# the gateway's IRSA ServiceAccount and the same private-only egress policy, then prints the
# per-request HTTP log. Usage: ./70-sts-experiments.sh [case ...]   (default: all)
source "$(dirname "$0")/00-env.sh"
: "${A_ECR:?run 30-a-eks.sh first}"
REPO="$(cd "$HERE/../.." && pwd)"; K="kubectl --context $CLUSTER"
STS_VPCE_DNS="$(awsA ec2 describe-vpc-endpoints --vpc-endpoint-ids "$A_VPCE_STS" --query 'VpcEndpoints[0].DnsEntries[0].DnsName' --output text)"
save A_STS_VPCE_DNS "$STS_VPCE_DNS" >/dev/null

if [ -z "${STSPROBE_IMG:-}" ]; then
  awsA ecr describe-repositories --repository-names sample-llm-gateway/stsprobe >/dev/null 2>&1 || awsA ecr create-repository --repository-name sample-llm-gateway/stsprobe --tags Key=project,Value="$LAB" >/dev/null
  save STSPROBE_IMG "$( cd "$REPO"; eval "$(AWS_PROFILE="$PROFILE_A" aws configure export-credentials --format env)"; export AWS_REGION="$REGION_A" KO_DOCKER_REPO="$A_ECR"
      ~/go/bin/ko build --base-import-paths --platform=linux/amd64,linux/arm64 --tags lab ./tools/stsprobe 2>"$OUT/ko-stsprobe.log" | tail -1 )"
fi
$K apply -f "$HERE/out/k8s/05-network.yaml" >/dev/null   # ensure netprobe/gateway egress policies exist
$K apply -f - >/dev/null <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: stsprobe-egress-private-only, namespace: llm-gateway }
spec:
  podSelector: { matchLabels: { app: stsprobe } }
  policyTypes: [Egress]
  egress:
    - to: [{ ipBlock: { cidr: 10.0.0.0/8 } }]
      ports: [{ protocol: TCP, port: 443 }, { protocol: UDP, port: 53 }, { protocol: TCP, port: 53 }]
    - to: [{ ipBlock: { cidr: $A_SERVICE_CIDR } }]
EOF

# run NAME "ENV=VAL ..." -- probe args
run() { local name="$1" envs="$2"; shift 2
  local envjson="[]"
  [ -n "$envs" ] && envjson="$(printf '%s\n' $envs | python3 -c 'import sys,json;print(json.dumps([{"name":l.split("=",1)[0],"value":l.split("=",1)[1]} for l in sys.stdin.read().split()]))')"
  echo; echo "################ $name"; echo "# env: ${envs:-<none>}"; echo "# args: $*"
  $K -n llm-gateway delete pod stsprobe --ignore-not-found --wait >/dev/null 2>&1
  $K -n llm-gateway run stsprobe --rm -i --restart=Never --image="$STSPROBE_IMG" --labels=app=stsprobe \
     --overrides="{\"spec\":{\"serviceAccountName\":\"llm-gateway\",\"nodeSelector\":{\"network-tier\":\"private\"},\"containers\":[{\"name\":\"stsprobe\",\"image\":\"$STSPROBE_IMG\",\"args\":$(printf '%s\n' "$@" | python3 -c 'import sys,json;print(json.dumps(sys.stdin.read().split("\n")[:-1]))'),\"env\":$envjson}]}}" \
     2>&1 | grep -v "^pod .* deleted\|^If you don't see\|recorded in container logs" | tee -a "$OUT/sts-experiments.log" || true  # a failing probe is a result, not a script error
}
: > "$OUT/sts-experiments.log"
COMMON=(-role "$B_XACCT_ROLE_ARN" -bedrock-region "$REGION_B" -bedrock-base "https://$B_BEDROCK_VPCE_DNS" -timeout 8s)
ALL=("$@")
want() { local c="$1"; shift; [ $# -eq 0 ] || [[ " $* " == *" $c "* ]]; }

# C1 — misconfiguration A: region us-west-2 everywhere, STS VPCE given only to
#      the self-built client via BaseEndpoint. Hop 1 (IRSA) still goes to sts.us-west-2 (public).
want C1 "${ALL[@]}" && run "C1 misconfig A: BaseEndpoint on outer client only" "" \
  -sts-region "$REGION_B" -sts-base "https://$STS_VPCE_DNS" "${COMMON[@]}"

# C2 — process-wide endpoint override: AWS_ENDPOINT_URL_STS applies to every STS client, hop 1 included.
want C2 "${ALL[@]}" && run "C2 AWS_ENDPOINT_URL_STS for the whole process, region still us-west-2" "AWS_ENDPOINT_URL_STS=https://$STS_VPCE_DNS" \
  -sts-region "$REGION_B" -sts-base "https://$STS_VPCE_DNS" "${COMMON[@]}"

# C3 — the gateway's approach: pin the STS region to where the STS VPCE is and let private DNS do
#      the rest; no endpoint URL anywhere.
want C3 "${ALL[@]}" && run "C3 gateway approach: sts_region=Tokyo + private DNS, no endpoint config" "" \
  -sts-region "$REGION_A" "${COMMON[@]}"

# C4 — what C3 degrades to when STS region is left at the Bedrock region (the sts_region knob
#      missing): both hops go to the public us-west-2 STS.
want C4 "${ALL[@]}" && run "C4 gateway without sts_region: STS follows the signing region" "" \
  -sts-region "$REGION_B" "${COMMON[@]}"

# C5 — Bedrock reached by its public hostname instead of the VPCE-specific name: credentials are
#      fine (C3 settings), the model call times out because private DNS does not cross the peering.
want C5 "${ALL[@]}" && run "C5 Bedrock by public hostname across peering (no cross-VPC private DNS)" "" \
  -sts-region "$REGION_A" -role "$B_XACCT_ROLE_ARN" -bedrock-region "$REGION_B" -timeout 8s

# C6 — AWS_ENDPOINT_URL_STS with the region left at us-west-2: both hops reach the Tokyo VPCE, but
#      the request is signed for us-west-2 and the Tokyo STS rejects it. The endpoint override does
#      not change the signing region; the region must match the VPCE's region as well.
want C6 "${ALL[@]}" && run "C6 AWS_ENDPOINT_URL_STS but region still us-west-2 (signature scope mismatch)" "AWS_ENDPOINT_URL_STS=https://$STS_VPCE_DNS" \
  -sts-region "$REGION_B" "${COMMON[@]}"

# C7/C8 — environment without private DNS: pod region is Tokyo, but the STS VPCE has NO private DNS,
#      so sts.ap-northeast-1.amazonaws.com resolves to the public IP. Temporarily switch private DNS
#      off on the lab's STS endpoint (gateway pods keep cached credentials meanwhile), run both
#      configurations, then switch it back on.
if want C7 "${ALL[@]}" || want C8 "${ALL[@]}"; then
  echo; echo "== disabling private DNS on the Tokyo STS endpoint for C7/C8"
  awsA ec2 modify-vpc-endpoint --vpc-endpoint-id "$A_VPCE_STS" --no-private-dns-enabled >/dev/null
  until [ "$(awsA ec2 describe-vpc-endpoints --vpc-endpoint-ids "$A_VPCE_STS" --query 'VpcEndpoints[0].State' --output text)" = available ]; do sleep 5; done
  sleep 75   # let the VPC resolver / CoreDNS caches expire
  want C7 "${ALL[@]}" && run "C7 misconfig B: region Tokyo, STS VPCE without private DNS, BaseEndpoint on outer client only" "" \
    -sts-region "$REGION_A" -sts-base "https://$STS_VPCE_DNS" "${COMMON[@]}"
  want C8 "${ALL[@]}" && run "C8 same + AWS_ENDPOINT_URL_STS" "AWS_ENDPOINT_URL_STS=https://$STS_VPCE_DNS" \
    -sts-region "$REGION_A" -sts-base "https://$STS_VPCE_DNS" "${COMMON[@]}"
  echo; echo "== re-enabling private DNS on the Tokyo STS endpoint"
  awsA ec2 modify-vpc-endpoint --vpc-endpoint-id "$A_VPCE_STS" --private-dns-enabled >/dev/null
  until [ "$(awsA ec2 describe-vpc-endpoints --vpc-endpoint-ids "$A_VPCE_STS" --query 'VpcEndpoints[0].PrivateDnsEnabled' --output text)" = True ]; do sleep 5; done
fi

# C11/C12 — the gateway's own approach (region pinned to Tokyo, NO endpoint override) in the
#      no-private-DNS environment shape. C11 shows the approach
#      depends on private DNS; C12 shows AWS_ENDPOINT_URL_STS is the fallback the gateway also needs
#      there (the gateway reads it through LoadDefaultConfig like any SDK program).
if want C11 "${ALL[@]}" || want C12 "${ALL[@]}"; then
  echo; echo "== disabling private DNS on the Tokyo STS endpoint for C11/C12"
  awsA ec2 modify-vpc-endpoint --vpc-endpoint-id "$A_VPCE_STS" --no-private-dns-enabled >/dev/null
  until [ "$(awsA ec2 describe-vpc-endpoints --vpc-endpoint-ids "$A_VPCE_STS" --query 'VpcEndpoints[0].State' --output text)" = available ]; do sleep 5; done
  sleep 75
  want C11 "${ALL[@]}" && run "C11 gateway approach (sts_region=Tokyo, no endpoint) but STS VPCE WITHOUT private DNS" "" \
    -sts-region "$REGION_A" "${COMMON[@]}"
  want C12 "${ALL[@]}" && run "C12 same + AWS_ENDPOINT_URL_STS (what the gateway would also need there)" "AWS_ENDPOINT_URL_STS=https://$STS_VPCE_DNS" \
    -sts-region "$REGION_A" "${COMMON[@]}"
  echo; echo "== re-enabling private DNS on the Tokyo STS endpoint"
  awsA ec2 modify-vpc-endpoint --vpc-endpoint-id "$A_VPCE_STS" --private-dns-enabled >/dev/null
  until [ "$(awsA ec2 describe-vpc-endpoints --vpc-endpoint-ids "$A_VPCE_STS" --query 'VpcEndpoints[0].PrivateDnsEnabled' --output text)" = True ]; do sleep 5; done
fi

# C9/C10 — where does "region=Tokyo" come from when code does not set it? The endpoint override
#      (AWS_ENDPOINT_URL_STS) works only if the SDK region is Tokyo (C2 vs C8). With no region set in
#      code, the SDK reads AWS_REGION — which on EKS the pod-identity webhook injects as the CLUSTER
#      region. C9: rely on that injection. C10: same code, AWS_REGION overridden to us-west-2 (what a
#      deployment in another region, or an explicit env, would look like).
want C9 "${ALL[@]}" && run "C9 endpoint override, NO region in code: SDK reads webhook-injected AWS_REGION" "AWS_ENDPOINT_URL_STS=https://$STS_VPCE_DNS" \
  -sts-region "" -sts-base "https://$STS_VPCE_DNS" "${COMMON[@]}"
want C10 "${ALL[@]}" && run "C10 same code, AWS_REGION/AWS_DEFAULT_REGION=us-west-2" "AWS_ENDPOINT_URL_STS=https://$STS_VPCE_DNS AWS_REGION=$REGION_B AWS_DEFAULT_REGION=$REGION_B" \
  -sts-region "" -sts-base "https://$STS_VPCE_DNS" "${COMMON[@]}"

echo; echo "log: $OUT/sts-experiments.log"
