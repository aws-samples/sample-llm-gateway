#!/usr/bin/env bash
# Account B (us-west-2): private VPC with a bedrock-runtime interface endpoint reachable only from
# account A's VPC over peering, and the IAM role account A will assume.
source "$(dirname "$0")/00-env.sh"
echo "== B: VPC"
if [ -z "${B_VPC:-}" ]; then
  save B_VPC "$(awsB ec2 create-vpc --cidr-block "$CIDR_B" --tag-specifications "$(tagspec vpc b)" --query Vpc.VpcId --output text)"
  awsB ec2 modify-vpc-attribute --vpc-id "$B_VPC" --enable-dns-support '{"Value":true}'
  awsB ec2 modify-vpc-attribute --vpc-id "$B_VPC" --enable-dns-hostnames '{"Value":true}'
fi
# No internet gateway, no NAT: this VPC has no egress by construction.
echo "== B: two private subnets (interface endpoints want two AZs)"
[ -n "${B_SUBNET_1:-}" ] || save B_SUBNET_1 "$(awsB ec2 create-subnet --vpc-id "$B_VPC" --cidr-block 10.1.1.0/24 --availability-zone "${REGION_B}a" --tag-specifications "$(tagspec subnet b-priv-a)" --query Subnet.SubnetId --output text)"
[ -n "${B_SUBNET_2:-}" ] || save B_SUBNET_2 "$(awsB ec2 create-subnet --vpc-id "$B_VPC" --cidr-block 10.1.2.0/24 --availability-zone "${REGION_B}b" --tag-specifications "$(tagspec subnet b-priv-b)" --query Subnet.SubnetId --output text)"
save B_RTB "$(awsB ec2 describe-route-tables --filters Name=vpc-id,Values="$B_VPC" Name=association.main,Values=true --query 'RouteTables[0].RouteTableId' --output text)"

echo "== B: endpoint security group (443 from A's CIDR only)"
if [ -z "${B_SG:-}" ]; then
  save B_SG "$(awsB ec2 create-security-group --vpc-id "$B_VPC" --group-name "$LAB-b-vpce" --description "bedrock-runtime VPCE: 443 from gateway VPC over peering" --tag-specifications "$(tagspec security-group b-vpce)" --query GroupId --output text)"
  awsB ec2 authorize-security-group-ingress --group-id "$B_SG" --ip-permissions "IpProtocol=tcp,FromPort=443,ToPort=443,IpRanges=[{CidrIp=$CIDR_A,Description=gateway-vpc-via-peering}]" >/dev/null
fi

echo "== B: bedrock-runtime interface endpoint (private DNS on; A cannot use it across the peering — that is the point of the lab)"
if [ -z "${B_BEDROCK_VPCE:-}" ]; then
  save B_BEDROCK_VPCE "$(awsB ec2 create-vpc-endpoint --vpc-id "$B_VPC" --vpc-endpoint-type Interface \
      --service-name "com.amazonaws.$REGION_B.bedrock-runtime" --subnet-ids "$B_SUBNET_1" "$B_SUBNET_2" \
      --security-group-ids "$B_SG" --private-dns-enabled --tag-specifications "$(tagspec vpc-endpoint b-bedrock-runtime)" \
      --query VpcEndpoint.VpcEndpointId --output text)"
fi
echo "   waiting for endpoint to become available"
until [ "$(awsB ec2 describe-vpc-endpoints --vpc-endpoint-ids "$B_BEDROCK_VPCE" --query 'VpcEndpoints[0].State' --output text)" = available ]; do sleep 10; done
# First DnsEntries item is the regional endpoint-specific name; that is what account A must put in its config.
save B_BEDROCK_VPCE_DNS "$(awsB ec2 describe-vpc-endpoints --vpc-endpoint-ids "$B_BEDROCK_VPCE" --query 'VpcEndpoints[0].DnsEntries[0].DnsName' --output text)"

echo "== B: cross-account role $XACCT_ROLE (trusts account A; tightened to the IRSA role by 30-a-irsa.sh)"
cat > "$OUT/b-trust.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::$ACCT_A:root"},"Action":"sts:AssumeRole"}]}
EOF
cat > "$OUT/b-bedrock-policy.json" <<'EOF'
{"Version":"2012-10-17","Statement":[{"Sid":"BedrockInvoke","Effect":"Allow",
  "Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
  "Resource":["arn:aws:bedrock:*::foundation-model/*","arn:aws:bedrock:*:*:inference-profile/*","arn:aws:bedrock:*:*:project/default"]}]}
EOF
if ! awsB iam get-role --role-name "$XACCT_ROLE" >/dev/null 2>&1; then
  awsB iam create-role --role-name "$XACCT_ROLE" --assume-role-policy-document "file://$OUT/b-trust.json" --tags Key=project,Value="$LAB" >/dev/null
fi
awsB iam put-role-policy --role-name "$XACCT_ROLE" --policy-name bedrock-invoke --policy-document "file://$OUT/b-bedrock-policy.json"
save B_XACCT_ROLE_ARN "arn:aws:iam::$ACCT_B:role/$XACCT_ROLE"
echo "== B done"
