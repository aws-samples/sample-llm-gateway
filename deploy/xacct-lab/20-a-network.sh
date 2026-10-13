#!/usr/bin/env bash
# Account A (Tokyo): private VPC for the gateway cluster, every AWS API the gateway and the EKS
# nodes need exposed as a VPC endpoint (no IGW, no NAT), and VPC peering to account B.
source "$(dirname "$0")/00-env.sh"
: "${B_VPC:?run 10-b-bedrock-side.sh first}"

echo "== A: VPC"
if [ -z "${A_VPC:-}" ]; then
  save A_VPC "$(awsA ec2 create-vpc --cidr-block "$CIDR_A" --tag-specifications "$(tagspec vpc a)" --query Vpc.VpcId --output text)"
  awsA ec2 modify-vpc-attribute --vpc-id "$A_VPC" --enable-dns-support '{"Value":true}'
  awsA ec2 modify-vpc-attribute --vpc-id "$A_VPC" --enable-dns-hostnames '{"Value":true}'   # required for private DNS on endpoints
fi
echo "== A: two private subnets (EKS needs two AZs)"
[ -n "${A_SUBNET_1:-}" ] || save A_SUBNET_1 "$(awsA ec2 create-subnet --vpc-id "$A_VPC" --cidr-block 10.2.1.0/24 --availability-zone "${REGION_A}a" --tag-specifications "$(tagspec subnet a-priv-a)" --query Subnet.SubnetId --output text)"
[ -n "${A_SUBNET_2:-}" ] || save A_SUBNET_2 "$(awsA ec2 create-subnet --vpc-id "$A_VPC" --cidr-block 10.2.2.0/24 --availability-zone "${REGION_A}c" --tag-specifications "$(tagspec subnet a-priv-c)" --query Subnet.SubnetId --output text)"
# Tags EKS / Auto Mode use to pick subnets for internal load balancers.
awsA ec2 create-tags --resources "$A_SUBNET_1" "$A_SUBNET_2" --tags Key=kubernetes.io/role/internal-elb,Value=1 "Key=kubernetes.io/cluster/$CLUSTER,Value=shared"
save A_RTB "$(awsA ec2 describe-route-tables --filters Name=vpc-id,Values="$A_VPC" Name=association.main,Values=true --query 'RouteTables[0].RouteTableId' --output text)"

echo "== A: endpoint security group (443 from inside the VPC)"
if [ -z "${A_SG:-}" ]; then
  save A_SG "$(awsA ec2 create-security-group --vpc-id "$A_VPC" --group-name "$LAB-a-vpce" --description "interface endpoints: 443 from gateway VPC" --tag-specifications "$(tagspec security-group a-vpce)" --query GroupId --output text)"
  awsA ec2 authorize-security-group-ingress --group-id "$A_SG" --ip-permissions "IpProtocol=tcp,FromPort=443,ToPort=443,IpRanges=[{CidrIp=$CIDR_A,Description=gateway-vpc}]" >/dev/null
fi

echo "== A: interface endpoints, private DNS on"
# sts            : IRSA AssumeRoleWithWebIdentity + cross-account AssumeRole (the two-hop path studied in docs/cross-account-private-networking.md §3)
# bedrock-runtime: same-account same-region Bedrock, the control path
# secretsmanager : gateway reads control-plane token at startup
# ecr.api/ecr.dkr: nodes pull the gateway image from private ECR
# ec2, eks, eks-auth: EKS Auto Mode node bootstrap and node identity without NAT
for svc in sts bedrock-runtime secretsmanager ecr.api ecr.dkr ec2 eks eks-auth; do
  key="A_VPCE_$(echo "$svc" | tr '.-' '__' | tr a-z A-Z)"
  if [ -z "${!key:-}" ]; then
    save "$key" "$(awsA ec2 create-vpc-endpoint --vpc-id "$A_VPC" --vpc-endpoint-type Interface \
        --service-name "com.amazonaws.$REGION_A.$svc" --subnet-ids "$A_SUBNET_1" "$A_SUBNET_2" \
        --security-group-ids "$A_SG" --private-dns-enabled --tag-specifications "$(tagspec vpc-endpoint "a-$svc")" \
        --query VpcEndpoint.VpcEndpointId --output text)"
  fi
done
echo "== A: s3 gateway endpoint (ECR image layers live in S3)"
if [ -z "${A_VPCE_S3:-}" ]; then
  save A_VPCE_S3 "$(awsA ec2 create-vpc-endpoint --vpc-id "$A_VPC" --vpc-endpoint-type Gateway \
      --service-name "com.amazonaws.$REGION_A.s3" --route-table-ids "$A_RTB" --tag-specifications "$(tagspec vpc-endpoint a-s3)" \
      --query VpcEndpoint.VpcEndpointId --output text)"
fi

echo "== peering A(${REGION_A}) -> B(${REGION_B}), cross-account"
if [ -z "${PCX:-}" ]; then
  save PCX "$(awsA ec2 create-vpc-peering-connection --vpc-id "$A_VPC" --peer-vpc-id "$B_VPC" --peer-owner-id "$ACCT_B" --peer-region "$REGION_B" \
      --tag-specifications "$(tagspec vpc-peering-connection a-b)" --query VpcPeeringConnection.VpcPeeringConnectionId --output text)"
  sleep 5
  awsB ec2 accept-vpc-peering-connection --vpc-peering-connection-id "$PCX" >/dev/null
fi
until [ "$(awsA ec2 describe-vpc-peering-connections --vpc-peering-connection-ids "$PCX" --query 'VpcPeeringConnections[0].Status.Code' --output text)" = active ]; do sleep 5; done
awsA ec2 create-route --route-table-id "$A_RTB" --destination-cidr-block "$CIDR_B" --vpc-peering-connection-id "$PCX" >/dev/null 2>&1 || true
awsB ec2 create-route --route-table-id "$B_RTB" --destination-cidr-block "$CIDR_A" --vpc-peering-connection-id "$PCX" >/dev/null 2>&1 || true

echo "== A: waiting for interface endpoints"
for key in $(grep -o '^A_VPCE_[A-Z_]*=' "$IDS" | tr -d =); do
  [ "$key" = A_VPCE_S3 ] && continue
  until [ "$(awsA ec2 describe-vpc-endpoints --vpc-endpoint-ids "${!key}" --query 'VpcEndpoints[0].State' --output text)" = available ]; do sleep 10; done
done
echo "== A network done"
