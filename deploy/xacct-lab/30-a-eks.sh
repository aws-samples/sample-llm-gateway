#!/usr/bin/env bash
# Account A (Tokyo): EKS Auto Mode cluster in the private-only VPC, OIDC for IRSA, the gateway's
# IRSA role (Bedrock in A + AssumeRole into B), and tighten B's role trust to that IRSA role.
source "$(dirname "$0")/00-env.sh"
: "${A_VPC:?run 20-a-network.sh first}"
MY_IP="${MY_IP:-$(curl -s --max-time 5 checkip.amazonaws.com)}"

echo "== A: EKS cluster $CLUSTER (Auto Mode, private subnets only, API reachable from $MY_IP)"
cat > "$OUT/cluster.yaml" <<EOF
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig
metadata:
  name: $CLUSTER
  region: $REGION_A
  version: "1.35"
  tags: { project: $LAB }
iam:
  withOIDC: true
autoModeConfig:
  enabled: true
  nodePools: ["general-purpose", "system"]
vpc:
  id: $A_VPC
  subnets:
    private:
      ${REGION_A}a: { id: $A_SUBNET_1 }
      ${REGION_A}c: { id: $A_SUBNET_2 }
  clusterEndpoints:
    publicAccess: true        # control plane is AWS-managed; this is for kubectl from the laptop only
    privateAccess: true       # nodes/pods reach the API server over VPC ENIs, no internet needed
  publicAccessCIDRs: ["$MY_IP/32"]
accessConfig:
  authenticationMode: API_AND_CONFIG_MAP
EOF
if ! awsA eks describe-cluster --name "$CLUSTER" >/dev/null 2>&1; then
  AWS_PROFILE="$PROFILE_A" eksctl create cluster -f "$OUT/cluster.yaml"
fi
awsA eks update-kubeconfig --name "$CLUSTER" --alias "$CLUSTER" >/dev/null
save A_OIDC "$(awsA eks describe-cluster --name "$CLUSTER" --query 'cluster.identity.oidc.issuer' --output text | sed 's#https://##')"
save A_SERVICE_CIDR "$(awsA eks describe-cluster --name "$CLUSTER" --query 'cluster.kubernetesNetworkConfig.serviceIpv4Cidr' --output text)"

echo "== A: IRSA role for the gateway (ServiceAccount llm-gateway/llm-gateway)"
IRSA_ROLE="$LAB-gateway-irsa"
cat > "$OUT/a-irsa-trust.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
  "Principal":{"Federated":"arn:aws:iam::$ACCT_A:oidc-provider/$A_OIDC"},
  "Action":"sts:AssumeRoleWithWebIdentity",
  "Condition":{"StringEquals":{"$A_OIDC:aud":"sts.amazonaws.com","$A_OIDC:sub":"system:serviceaccount:llm-gateway:llm-gateway"}}}]}
EOF
cat > "$OUT/a-irsa-policy.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Sid":"BedrockInvokeInA","Effect":"Allow","Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
  "Resource":["arn:aws:bedrock:*::foundation-model/*","arn:aws:bedrock:*:*:inference-profile/*","arn:aws:bedrock:*:*:project/default"]},
 {"Sid":"AssumeBedrockRoleInB","Effect":"Allow","Action":"sts:AssumeRole","Resource":"$B_XACCT_ROLE_ARN"},
 {"Sid":"ReadLabSecrets","Effect":"Allow","Action":"secretsmanager:GetSecretValue","Resource":"arn:aws:secretsmanager:$REGION_A:$ACCT_A:secret:llm-gateway/*"}]}
EOF
if ! awsA iam get-role --role-name "$IRSA_ROLE" >/dev/null 2>&1; then
  awsA iam create-role --role-name "$IRSA_ROLE" --assume-role-policy-document "file://$OUT/a-irsa-trust.json" --tags Key=project,Value="$LAB" >/dev/null
fi
awsA iam put-role-policy --role-name "$IRSA_ROLE" --policy-name gateway --policy-document "file://$OUT/a-irsa-policy.json"
save A_IRSA_ROLE_ARN "arn:aws:iam::$ACCT_A:role/$IRSA_ROLE"

echo "== B: tighten $XACCT_ROLE trust from account root to the IRSA role"
cat > "$OUT/b-trust.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"$A_IRSA_ROLE_ARN"},"Action":"sts:AssumeRole"}]}
EOF
awsB iam update-assume-role-policy --role-name "$XACCT_ROLE" --policy-document "file://$OUT/b-trust.json"

echo "== A: ECR repository"
# ko --base-import-paths pushes to <repo>/gateway and <repo>/mock-controlplane; ECR needs both to exist.
for r in sample-llm-gateway/gateway sample-llm-gateway/mock-controlplane; do
  awsA ecr describe-repositories --repository-names "$r" >/dev/null 2>&1 || awsA ecr create-repository --repository-name "$r" --tags Key=project,Value="$LAB" >/dev/null
done
save A_ECR "$ACCT_A.dkr.ecr.$REGION_A.amazonaws.com/sample-llm-gateway"
echo "== A EKS done"
