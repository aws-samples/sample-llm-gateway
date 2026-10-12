#!/usr/bin/env bash
# Tear the lab down in dependency order. Reads out/ids.env; prints every delete before doing it.
# Usage: ACCT_A=... ACCT_B=... ./90-teardown.sh            (add CONFIRM=yes to actually delete)
source "$(dirname "$0")/00-env.sh"
K="kubectl --context $CLUSTER"
run() { echo "  $*"; [ "${CONFIRM:-}" = yes ] && "$@" || true; }
[ "${CONFIRM:-}" = yes ] || echo "DRY RUN (set CONFIRM=yes to delete)"

echo "== 1. workloads (lets Auto Mode release the private nodes)"
run $K delete namespace llm-gateway --ignore-not-found --wait=true --timeout=5m
run $K delete nodepool private --ignore-not-found
run $K delete nodeclass private-only --ignore-not-found

echo "== 2. EKS cluster (eksctl stack: cluster, OIDC provider, Auto Mode node role)"
run env AWS_PROFILE="$PROFILE_A" eksctl delete cluster --name "$CLUSTER" --region "$REGION_A" --wait --disable-nodegroup-eviction

echo "== 3. account A: IRSA role, ECR, endpoints, peering routes, VPC"
if awsA iam get-role --role-name "$LAB-gateway-irsa" >/dev/null 2>&1; then
  run awsA iam delete-role-policy --role-name "$LAB-gateway-irsa" --policy-name gateway
  run awsA iam delete-role --role-name "$LAB-gateway-irsa"
fi
for r in sample-llm-gateway/gateway sample-llm-gateway/mock-controlplane sample-llm-gateway/netprobe sample-llm-gateway/stsprobe; do
  run awsA ecr delete-repository --repository-name "$r" --force
done
if [ -n "${A_VPC:-}" ]; then
  eps="$(awsA ec2 describe-vpc-endpoints --filters Name=vpc-id,Values="$A_VPC" --query 'VpcEndpoints[].VpcEndpointId' --output text)"
  [ -n "$eps" ] && run awsA ec2 delete-vpc-endpoints --vpc-endpoint-ids $eps
  [ -n "${PCX:-}" ] && run awsA ec2 delete-vpc-peering-connection --vpc-peering-connection-id "$PCX"
  echo "  waiting for A endpoints to go away"; [ "${CONFIRM:-}" = yes ] && sleep 60
  # eksctl's cluster security group and ENIs may linger a little; retry the VPC delete.
  for sg in $(awsA ec2 describe-security-groups --filters Name=vpc-id,Values="$A_VPC" --query 'SecurityGroups[?GroupName!=`default`].GroupId' --output text); do run awsA ec2 delete-security-group --group-id "$sg"; done
  for s in "${A_SUBNET_1:-}" "${A_SUBNET_2:-}"; do [ -n "$s" ] && run awsA ec2 delete-subnet --subnet-id "$s"; done
  run awsA ec2 delete-vpc --vpc-id "$A_VPC"
fi

echo "== 4. account B: role, endpoint, VPC"
if awsB iam get-role --role-name "$XACCT_ROLE" >/dev/null 2>&1; then
  run awsB iam delete-role-policy --role-name "$XACCT_ROLE" --policy-name bedrock-invoke
  run awsB iam delete-role --role-name "$XACCT_ROLE"
fi
if [ -n "${B_VPC:-}" ]; then
  [ -n "${B_BEDROCK_VPCE:-}" ] && run awsB ec2 delete-vpc-endpoints --vpc-endpoint-ids "$B_BEDROCK_VPCE"
  echo "  waiting for B endpoint to go away"; [ "${CONFIRM:-}" = yes ] && sleep 60
  [ -n "${B_SG:-}" ] && run awsB ec2 delete-security-group --group-id "$B_SG"
  for s in "${B_SUBNET_1:-}" "${B_SUBNET_2:-}"; do [ -n "$s" ] && run awsB ec2 delete-subnet --subnet-id "$s"; done
  run awsB ec2 delete-vpc --vpc-id "$B_VPC"
fi
echo "== done. If a VPC delete failed, re-run after a minute (lingering ENIs)."
