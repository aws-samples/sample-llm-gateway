#!/usr/bin/env bash
# Cross-account private-networking lab: gateway in account A (Tokyo), Bedrock in account B
# (us-west-2), VPC peering between them, no internet egress anywhere.
#
# Fill in the two accounts and the AWS CLI profiles that can administer them. Every other value
# is derived or written to out/ids.env by the scripts as resources are created.
set -euo pipefail

: "${ACCT_A:?set ACCT_A (gateway account id)}"
: "${ACCT_B:?set ACCT_B (bedrock account id)}"
PROFILE_A="${PROFILE_A:-llmgw-a}"
PROFILE_B="${PROFILE_B:-llmgw-b}"

REGION_A=ap-northeast-1          # gateway, STS VPCE, EKS
REGION_B=us-west-2               # Bedrock VPCE
CIDR_A=10.2.0.0/16
CIDR_B=10.1.0.0/16
LAB=llmgw-xacct-lab              # Name/tag prefix on every resource
CLUSTER=llmgw-lab
XACCT_ROLE=llmgw-bedrock-xacct   # role in B that A assumes

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$HERE/out"; mkdir -p "$OUT"
IDS="$OUT/ids.env"; touch "$IDS"
# shellcheck disable=SC1090
source "$IDS"

awsA() { AWS_PROFILE="$PROFILE_A" aws --region "$REGION_A" "$@"; }
awsB() { AWS_PROFILE="$PROFILE_B" aws --region "$REGION_B" "$@"; }
save() { # save KEY VALUE  -> persists to ids.env and exports
  local k="$1" v="$2"
  grep -v "^$k=" "$IDS" > "$IDS.tmp" || true; echo "$k=$v" >> "$IDS.tmp"; mv "$IDS.tmp" "$IDS"
  export "$k=$v"; echo "  $k=$v"
}
tagspec() { echo "ResourceType=$1,Tags=[{Key=Name,Value=$LAB-$2},{Key=project,Value=$LAB}]"; }
