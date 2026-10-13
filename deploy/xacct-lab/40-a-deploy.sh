#!/usr/bin/env bash
# Account A: build the gateway + mock control plane images into A's ECR with ko, render the k8s
# manifests for this lab from deploy/k8s/, and apply them. Re-runnable.
source "$(dirname "$0")/00-env.sh"
: "${A_ECR:?run 30-a-eks.sh first}"
REPO="$(cd "$HERE/../.." && pwd)"
K8S="$OUT/k8s"; mkdir -p "$K8S"

echo "== build & push images (ko, multi-arch; ECR auth via ko's credential chain fed by the A profile)"
export KO_DOCKER_REPO="$A_ECR"
( eval "$(AWS_PROFILE="$PROFILE_A" aws configure export-credentials --format env)"; export AWS_REGION="$REGION_A"
  cd "$REPO"
  GW_IMG="$(~/go/bin/ko build --base-import-paths --platform=linux/amd64,linux/arm64 --tags lab ./cmd/gateway 2>"$OUT/ko-gateway.log" | tail -1)"
  CP_IMG="$(~/go/bin/ko build --base-import-paths --platform=linux/amd64,linux/arm64 --tags lab ./cmd/mock-controlplane 2>"$OUT/ko-mockcp.log" | tail -1)"
  echo "GW_IMG=$GW_IMG" > "$OUT/images.env"; echo "CP_IMG=$CP_IMG" >> "$OUT/images.env" )
source "$OUT/images.env"; echo "  gateway: $GW_IMG"; echo "  mock-cp: $CP_IMG"

echo "== render manifests"
# Node role / security group come from the default NodeClass eksctl created.
NODE_ROLE="$(kubectl --context "$CLUSTER" get nodeclass default -o jsonpath='{.spec.role}')"
NODE_SG="$(kubectl --context "$CLUSTER" get nodeclass default -o jsonpath='{.spec.securityGroupSelectorTerms[0].id}')"
TOKEN="${CP_TOKEN:-lab-token-$(openssl rand -hex 8)}"
API_KEY="${CLIENT_KEY:-sk-lab-key}"

cp "$REPO/deploy/k8s/00-namespace.yaml" "$K8S/00-namespace.yaml"
sed -e "s#arn:aws:iam::[0-9]*:role/[A-Za-z0-9_-]*#$A_IRSA_ROLE_ARN#" "$REPO/deploy/k8s/10-serviceaccount.yaml" > "$K8S/10-serviceaccount.yaml"
cat > "$K8S/15-secrets.yaml" <<EOF
apiVersion: v1
kind: Secret
metadata: { name: llm-gateway-secrets, namespace: llm-gateway }
type: Opaque
stringData:
  CP_GATEWAY_TOKEN: "$TOKEN"
  MOCK_API_KEYS: "$API_KEY"
EOF
python3 - "$REPO/deploy/k8s/05-network.yaml" "$NODE_ROLE" "$NODE_SG" "$A_SUBNET_1" "$A_SUBNET_2" "$A_SERVICE_CIDR" > "$K8S/05-network.yaml" <<'EOF'
import re,sys
src=open(sys.argv[1]).read(); role,sg,s1,s2,svc=sys.argv[2:7]
src=re.sub(r'role: eksctl-\S+', 'role: '+role, src)
src=re.sub(r'- id: sg-\S+', '- id: '+sg, src)
src=re.sub(r'- id: subnet-aaaaaaaaaaaaaaaaa.*', '- id: '+s1, src)
src=re.sub(r'- id: subnet-bbbbbbbbbbbbbbbbb.*', '- id: '+s2, src)
src=src.replace('172.20.0.0/16', svc)
sys.stdout.write(src)
EOF

# Routes: same-account Tokyo, cross-account us-west-2, failover pair, and protocol-translation
# variants on the cross-account path (Claude Code → GPT in B, Codex → Claude in B).
B=bedrock-usw2-xacct
python3 - "$REPO/deploy/k8s/20-mock-controlplane.yaml" "$CP_IMG" > "$K8S/20-mock-controlplane.yaml" <<EOF
import json,re,sys
src=open(sys.argv[1]).read()
routes={"version":"lab-1","models":[
 {"modelCode":"claude-sonnet-5",    "providers":[{"providerCode":"bedrock","providerModelCode":"global.anthropic.claude-sonnet-5","priority":10,"weight":100}]},
 {"modelCode":"gpt-5.6-sol",        "providers":[{"providerCode":"bedrock","providerModelCode":"global.openai.gpt-5.6-sol","priority":10,"weight":100}]},
 {"modelCode":"claude-sonnet-5-us", "providers":[{"providerCode":"$B","providerModelCode":"us.anthropic.claude-sonnet-5","priority":10,"weight":100}]},
 {"modelCode":"gpt-5.6-sol-us",     "providers":[{"providerCode":"$B","providerModelCode":"us.openai.gpt-5.6-sol","priority":10,"weight":100}]},
 {"modelCode":"claude-sonnet-5-ha", "providers":[{"providerCode":"$B","providerModelCode":"us.anthropic.claude-sonnet-5","priority":10,"weight":100},
                                                 {"providerCode":"bedrock","providerModelCode":"global.anthropic.claude-sonnet-5","priority":20,"weight":100}]},
 {"modelCode":"claude-via-gpt-us",  "providers":[{"providerCode":"$B","providerModelCode":"us.openai.gpt-5.6-sol","providerProtocol":"openai_chat","priority":10,"weight":100}]},
 {"modelCode":"gpt-5",              "providers":[{"providerCode":"$B","providerModelCode":"us.anthropic.claude-sonnet-5","providerProtocol":"anthropic","priority":10,"weight":100}]},
 {"modelCode":"claude-sonnet-5-us-invoke","providers":[{"providerCode":"bedrock-usw2-xacct-invoke","providerModelCode":"us.anthropic.claude-sonnet-5","priority":10,"weight":100}]},
 {"modelCode":"gpt-5.6-sol-us-invoke",    "providers":[{"providerCode":"bedrock-usw2-xacct-invoke","providerModelCode":"us.openai.gpt-5.6-sol","priority":10,"weight":100}]}
]}
body=json.dumps(routes,indent=2,ensure_ascii=False)
src=re.sub(r'routes\.json: \|\n(?:    .*\n)+', 'routes.json: |\n'+''.join('    '+l+'\n' for l in body.splitlines()), src)
src=re.sub(r'image: \S+mock-controlplane\S*', 'image: '+sys.argv[2], src)
sys.stdout.write(src)
EOF

python3 - "$REPO/deploy/k8s/30-gateway.yaml" "$GW_IMG" "$B_XACCT_ROLE_ARN" "$B_BEDROCK_VPCE_DNS" > "$K8S/30-gateway.yaml" <<'EOF'
import re,sys
src=open(sys.argv[1]).read(); img,role,vpce=sys.argv[2:5]
cfg = f"""  gateway.yaml: |
    server:
      listen: ":8080"
      log_level: info
    control_plane:
      base_url: "http://mock-controlplane.llm-gateway.svc.cluster.local:9090"
      token: "${{CP_GATEWAY_TOKEN}}"
      allow_insecure: true
      routes_poll_interval: 15s
    providers:
      # A, Tokyo, same account. Standard hostname -> Tokyo bedrock-runtime VPCE via private DNS.
      bedrock:
        auth: aws_iam
        region: ap-northeast-1
        endpoints:
          openai_chat:      "https://bedrock-runtime.ap-northeast-1.amazonaws.com/openai/v1"
          openai_responses: "https://bedrock-runtime.ap-northeast-1.amazonaws.com/openai/v1"
          anthropic:        "https://bedrock-runtime.ap-northeast-1.amazonaws.com/anthropic/v1"
      # B, us-west-2, cross-account: AssumeRole into B via the Tokyo STS VPCE (sts_region), sign for
      # us-west-2, send over peering to B's bedrock-runtime VPCE by its endpoint-specific name.
      bedrock-usw2-xacct:
        auth: aws_iam
        region: us-west-2
        sts_region: ap-northeast-1
        role_arn: {role}
        endpoints:
          openai_chat:      "https://{vpce}/openai/v1"
          openai_responses: "https://{vpce}/openai/v1"
          anthropic:        "https://{vpce}/anthropic/v1"
      # Same target through the SDK InvokeModel API (the transport used by SDK-based gateways) for side-by-side tests.
      bedrock-usw2-xacct-invoke:
        auth: aws_iam
        region: us-west-2
        sts_region: ap-northeast-1
        role_arn: {role}
        endpoints:
          bedrock_invoke: "https://{vpce}"
"""
src=re.sub(r'  gateway\.yaml: \|\n(?:    .*\n|\n)+?(?=---)', cfg, src, count=1)
src=re.sub(r'image: \S+/gateway\S*', 'image: '+img, src)
# token comes from the k8s Secret (no Secrets Manager in the lab config)
src=src.replace('          ports: [{ containerPort: 8080, name: http }]',
 '          env:\n            - name: CP_GATEWAY_TOKEN\n              valueFrom: { secretKeyRef: { name: llm-gateway-secrets, key: CP_GATEWAY_TOKEN } }\n          ports: [{ containerPort: 8080, name: http }]')
sys.stdout.write(src)
EOF

echo "== apply"
K="kubectl --context $CLUSTER"
$K apply -f "$K8S/00-namespace.yaml" -f "$K8S/10-serviceaccount.yaml" -f "$K8S/15-secrets.yaml"
$K apply -f "$K8S/05-network.yaml"
$K apply -f "$K8S/20-mock-controlplane.yaml" -f "$K8S/30-gateway.yaml"
echo "  CP_TOKEN=$TOKEN  CLIENT_KEY=$API_KEY" | tee "$OUT/creds.txt"
echo "== waiting for rollout (first private node has to come up)"
$K -n llm-gateway rollout status deploy/mock-controlplane --timeout=10m
$K -n llm-gateway rollout status deploy/llm-gateway --timeout=10m
$K -n llm-gateway get pods -o wide
echo "== A deploy done"
