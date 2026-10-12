#!/usr/bin/env bash
# Start one extra gateway pod whose cross-account providers lack `sts_region`, capture its startup
# log, and remove it. Shows what the gateway itself reports when STS follows the signing region.
source "$(dirname "$0")/00-env.sh"
source "$OUT/images.env"
K="kubectl --context $CLUSTER -n llm-gateway"
$K get cm llm-gateway-config -o jsonpath='{.data.gateway\.yaml}' | sed '/sts_region: ap-northeast-1/d' | sed 's/^/    /' \
 | { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: { name: llm-gateway-config-nosts, namespace: llm-gateway }\ndata:\n  gateway.yaml: |\n'; cat; } | $K apply -f - >/dev/null
$K apply -f - >/dev/null <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: gw-nosts-egress, namespace: llm-gateway }
spec:
  podSelector: { matchLabels: { app: llm-gateway-nosts } }
  policyTypes: [Egress]
  egress:
    - to: [{ ipBlock: { cidr: 10.0.0.0/8 } }]
      ports: [{ protocol: TCP, port: 443 }, { protocol: TCP, port: 9090 }, { protocol: UDP, port: 53 }, { protocol: TCP, port: 53 }]
    - to: [{ ipBlock: { cidr: $A_SERVICE_CIDR } }]
EOF
$K delete pod gw-nosts --ignore-not-found --wait >/dev/null 2>&1
# AWS_REGION_OVERRIDE: the EKS pod-identity webhook injects AWS_REGION=<cluster region>, which the
# gateway's fallback chain (sts_region > AWS_REGION > AWS_DEFAULT_REGION > provider region) picks up.
# Set AWS_REGION_OVERRIDE=us-west-2 to defeat that fallback and see the failure mode.
EXTRA_ENV=""; [ -n "${AWS_REGION_OVERRIDE:-}" ] && EXTRA_ENV=",{\"name\":\"AWS_REGION\",\"value\":\"$AWS_REGION_OVERRIDE\"},{\"name\":\"AWS_DEFAULT_REGION\",\"value\":\"$AWS_REGION_OVERRIDE\"}"
$K run gw-nosts --restart=Never --image="$GW_IMG" --labels=app=llm-gateway-nosts --overrides="{\"spec\":{\"serviceAccountName\":\"llm-gateway\",\"nodeSelector\":{\"network-tier\":\"private\"},\"containers\":[{\"name\":\"gw\",\"image\":\"$GW_IMG\",\"args\":[\"-config\",\"/etc/llm-gateway/gateway.yaml\"],\"env\":[{\"name\":\"CP_GATEWAY_TOKEN\",\"valueFrom\":{\"secretKeyRef\":{\"name\":\"llm-gateway-secrets\",\"key\":\"CP_GATEWAY_TOKEN\"}}}$EXTRA_ENV],\"volumeMounts\":[{\"name\":\"c\",\"mountPath\":\"/etc/llm-gateway\"}]}],\"volumes\":[{\"name\":\"c\",\"configMap\":{\"name\":\"llm-gateway-config-nosts\"}}]}}" >/dev/null
sleep "${WAIT:-90}"
echo "== pod:"; $K get pod gw-nosts --no-headers
echo "== injected AWS_* env:"; $K get pod gw-nosts -o json | python3 -c "import sys,json; [print('  ',e['name'],'=',e.get('value','(ref)')) for e in json.load(sys.stdin)['spec']['containers'][0]['env'] if e['name'].startswith('AWS_')]"
echo "== startup log (sts_region removed from cross-account providers${AWS_REGION_OVERRIDE:+, AWS_REGION=$AWS_REGION_OVERRIDE}):"
$K logs gw-nosts 2>&1 | tee "$OUT/gateway-no-sts-region.log" | python3 -c "
import sys,json
for l in sys.stdin:
    try: d=json.loads(l)
    except: print(l.rstrip()); continue
    print(d['time'][11:19], d['level'], d['msg'], *(f'{k}={d[k]}' for k in ('provider','arn','sts_region','sign_region','err') if k in d))"
$K delete pod gw-nosts cm llm-gateway-config-nosts networkpolicy gw-nosts-egress --ignore-not-found >/dev/null
