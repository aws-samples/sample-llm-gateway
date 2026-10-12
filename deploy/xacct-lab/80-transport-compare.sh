#!/usr/bin/env bash
# Pass-through (native Bedrock protocol endpoints) vs SDK InvokeModel transport, on the same
# cross-account private path. Adds two deliberately mis-pointed providers (a reachable private host
# that is NOT bedrock-runtime) to compare error readability, then runs the comparison matrix.
# Usage: ./80-transport-compare.sh            (GW/KEY/N env as in 50-smoke.sh)
source "$(dirname "$0")/00-env.sh"
: "${A_STS_VPCE_DNS:?run 70-sts-experiments.sh first (records the STS VPCE DNS name)}"
K="kubectl --context $CLUSTER -n llm-gateway"; GW="${GW:-http://127.0.0.1:18080}"; KEY="${KEY:-sk-lab-key}"; N="${N:-6}"
LOG="$OUT/transport-compare.log"; : > "$LOG"
say() { echo "$@" | tee -a "$LOG"; }

echo "== add mis-pointed providers + routes (idempotent)"
$K get cm llm-gateway-config -o jsonpath='{.data.gateway\.yaml}' > "$OUT/gateway.yaml.cur"
if ! grep -q wronghost "$OUT/gateway.yaml.cur"; then
cat >> "$OUT/gateway.yaml.cur" <<EOF
  # --- error-readability experiment: same private host (the Tokyo STS VPCE), which is reachable
  #     but is not bedrock-runtime. Native path forwards whatever it answers; InvokeModel path
  #     goes through the SDK's response parser.
  usw2-native-wronghost:
    auth: aws_iam
    region: us-west-2
    sts_region: ap-northeast-1
    role_arn: $B_XACCT_ROLE_ARN
    endpoints:
      anthropic: "https://$A_STS_VPCE_DNS/anthropic/v1"
  usw2-invoke-wronghost:
    auth: aws_iam
    region: us-west-2
    sts_region: ap-northeast-1
    role_arn: $B_XACCT_ROLE_ARN
    endpoints:
      bedrock_invoke: "https://$A_STS_VPCE_DNS"
EOF
  $K create cm llm-gateway-config --from-file=gateway.yaml="$OUT/gateway.yaml.cur" --dry-run=client -o yaml | $K apply -f - >/dev/null
  $K get cm mock-controlplane-routes -o jsonpath='{.data.routes\.json}' | python3 -c '
import json,sys
r=json.load(sys.stdin); r["version"]="lab-2"
r["models"]+=[{"modelCode":"wrong-native","providers":[{"providerCode":"usw2-native-wronghost","providerModelCode":"us.anthropic.claude-sonnet-5","priority":10,"weight":100}]},
              {"modelCode":"wrong-invoke","providers":[{"providerCode":"usw2-invoke-wronghost","providerModelCode":"us.anthropic.claude-sonnet-5","priority":10,"weight":100}]}]
print(json.dumps(r,indent=2))' > "$OUT/routes.json.cur"
  $K create cm mock-controlplane-routes --from-file=routes.json="$OUT/routes.json.cur" --dry-run=client -o yaml | $K apply -f - >/dev/null
  $K rollout restart deploy/mock-controlplane deploy/llm-gateway >/dev/null
  $K rollout status deploy/mock-controlplane --timeout=5m >/dev/null; $K rollout status deploy/llm-gateway --timeout=5m >/dev/null
  echo "   restarted; re-establish the port-forward then re-run this script"; exit 0
fi

req() { # req PATH BODY -> prints "HTTP code time reqid" and saves body to /tmp/tc.json
  curl -s --max-time 120 -o /tmp/tc.json -D /tmp/tc.hdr -w '%{http_code} %{time_total}' "$GW$1" -H "Authorization: Bearer $KEY" -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' -d "$2"
  printf ' %s\n' "$(grep -i '^x-request-id' /tmp/tc.hdr | awk '{print $2}' | tr -d '\r')"
}
A='{"model":"%s","max_tokens":200,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"Say hi in 3 words."}]}'
C='{"model":"%s","max_completion_tokens":200,"reasoning_effort":"none","messages":[{"role":"user","content":"Say hi in 3 words."}]}'
R='{"model":"%s","store":false,"reasoning":{"effort":"none"},"input":"Say hi in 3 words."}'

say; say "################ T1  OpenAI Responses protocol through the InvokeModel transport"
say "# InvokeModel accepts Anthropic Messages and OpenAI Chat bodies only; a Responses body has nowhere to go."
say "  native  /v1/responses gpt-5.6-sol-us        -> $(req /v1/responses "$(printf "$R" gpt-5.6-sol-us)")"; say "          $(head -c 160 /tmp/tc.json)"
say "  invoke  /v1/responses gpt-5.6-sol-us-invoke -> $(req /v1/responses "$(printf "$R" gpt-5.6-sol-us-invoke)")"; say "          $(head -c 300 /tmp/tc.json)"
say "  (fix for Codex-style clients on the invoke path: route with providerProtocol=openai_chat)"
say "# T1b: the underlying reason — a Responses body sent straight to InvokeModel in account B:"
awsB bedrock-runtime invoke-model --model-id us.openai.gpt-5.6-sol --body '{"input":"Say hi in 3 words.","max_output_tokens":50,"store":false}' --cli-binary-format raw-in-base64-out /tmp/tc-t1b.json 2>&1 | grep -o 'An error occurred.*' | cut -c1-260 | sed 's/^/  /' | tee -a "$LOG"

say; say "################ T2  error readability: provider endpoint points at a reachable private host that is not bedrock-runtime"
say "  native  wrong-native -> $(req /v1/messages "$(printf "$A" wrong-native)")"; say "          $(head -c 400 /tmp/tc.json | tr '\n' ' ')"
say "  invoke  wrong-invoke -> $(req /v1/messages "$(printf "$A" wrong-invoke)")"; say "          $(head -c 400 /tmp/tc.json | tr '\n' ' ')"

say; say "################ T3  streaming: TTFT and usage, $N requests per path (gateway log fields)"
stream() { # stream LABEL PATH MODEL BODYFMT
  local ids=(); for i in $(seq 1 "$N"); do
    body="$(printf "$4" "$3")"; body="${body%\}},\"stream\":true}"
    ids+=("$(curl -s --max-time 120 -o /dev/null -D /tmp/tc.hdr "$GW$2" -H "Authorization: Bearer $KEY" -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' -d "$body"; grep -i '^x-request-id' /tmp/tc.hdr | awk '{print $2}' | tr -d '\r')")
  done
  sleep 2
  $K logs deploy/llm-gateway --all-pods=true --since=10m 2>/dev/null | python3 -c "
import sys,json,statistics
ids=set(sys.argv[1:]); rows=[]
for l in sys.stdin:
    l=l[l.find('{'):] if l.startswith('[pod/') else l   # --all-pods prefixes each line with [pod/...]
    try: d=json.loads(l)
    except: continue
    if d.get('msg')=='request completed' and d.get('request_id') in ids: rows.append(d)
if not rows: print('  $1: no log rows matched'); sys.exit()
tt=[r['ttft_ms'] for r in rows]; du=[r['duration_ms'] for r in rows]
print(f'  {\"$1\":<34} n={len(rows)} status={sorted(set(r[\"status\"] for r in rows))} ttft_ms p50={statistics.median(tt):.0f} min={min(tt)} max={max(tt)}  duration_ms p50={statistics.median(du):.0f}  usage_found={all(r[\"usage_found\"] for r in rows)} in/out={rows[0][\"input_tokens\"]}/{rows[0][\"output_tokens\"]}')" "${ids[@]}" | tee -a "$LOG"
}
stream "Claude native (anthropic endpoint)"  /v1/messages         claude-sonnet-5-us        "$A"
stream "Claude via InvokeModel"              /v1/messages         claude-sonnet-5-us-invoke "$A"
stream "GPT native (chat endpoint)"          /v1/chat/completions gpt-5.6-sol-us            "$C"
stream "GPT via InvokeModel"                 /v1/chat/completions gpt-5.6-sol-us-invoke     "$C"

say; say "################ T4  what the client receives: stream framing parity (first 3 frames, Claude)"
for m in claude-sonnet-5-us claude-sonnet-5-us-invoke; do
  body="$(printf "$A" $m)"; body="${body%\}},\"stream\":true}"
  say "  --- $m"; { curl -sN --max-time 60 "$GW/v1/messages" -H "Authorization: Bearer $KEY" -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' -d "$body" || true; } | head -6 | cut -c1-150 | sed 's/^/      /' | tee -a "$LOG"
done
say; say "log: $LOG"
