#!/usr/bin/env bash
# 端到端冒烟：4 个模型 x 协议 x 流式/非流式 + 负向用例，
# 最后核对控制面是否收到计量上报。
# Usage: GW=http://localhost:8080 CP=http://localhost:9090 KEY=sk-demo-key scripts/smoke.sh
set -u
GW=${GW:-http://localhost:8080}
CP=${CP:-http://localhost:9090}
KEY=${KEY:-sk-demo-key}
# 可用环境变量覆盖被测模型列表（空格分隔），例如跨账号路由：CLAUDE_MODELS="claude-sonnet-5-us" GPT_MODELS="gpt-5.6-sol-us"
# 显式设为空串就跳过那组协议（只测一家外接供应商时用），例如 CLAUDE_MODELS="" RESPONSES_MODELS="" GPT_MODELS="kimi"
CLAUDE_MODELS=${CLAUDE_MODELS-"claude-sonnet-5 claude-opus-5"}
GPT_MODELS=${GPT_MODELS-"gpt-5.6-sol gpt-5.6-luna"}
RESPONSES_MODELS=${RESPONSES_MODELS-$GPT_MODELS}
C1=${CLAUDE_MODELS%% *}; G1=${GPT_MODELS%% *}
# 负向用例与 x-api-key 用例各需要一个能路由的模型：优先用各自协议的第一个模型；某个列表为空（只测一种协议）时借用另一个
if [ -n "$C1" ]; then A_PATH=/v1/messages; A_BODY="{\"model\":\"$C1\",\"max_tokens\":5,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
else A_PATH=/v1/chat/completions; A_BODY="{\"model\":\"$G1\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"; fi
if [ -n "$G1" ]; then O_PATH=/v1/chat/completions; O_BODY="{\"model\":\"$G1\",\"messages\":[{\"role\":\"user\",\"content\":\"x\"}]}"
else O_PATH=/v1/messages; O_BODY="{\"model\":\"$C1\",\"max_tokens\":5,\"messages\":[{\"role\":\"user\",\"content\":\"x\"}]}"; fi
PASS=0; FAIL=0
out=/tmp/smoke_out.$$

check() { # name expected_code actual_code extra
  if [ "$2" = "$3" ]; then PASS=$((PASS+1)); printf '  PASS %-46s HTTP %s %s\n' "$1" "$3" "${4:-}";
  else FAIL=$((FAIL+1)); printf '  FAIL %-46s HTTP %s (want %s) %s\n' "$1" "$3" "$2" "${4:-}"; head -c 400 "$out"; echo; fi
}

post() { # path key body -> prints http code; body in $out
  curl -sS -o "$out" -w '%{http_code}' -X POST "$GW$1" -H "Authorization: Bearer $2" \
    -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" --max-time 300 -d "$3"
}

curl -sS -X DELETE "$CP/debug/usages" >/dev/null 2>&1 || true

echo "== Anthropic /v1/messages"
for m in $CLAUDE_MODELS; do
  code=$(post /v1/messages "$KEY" "{\"model\":\"$m\",\"max_tokens\":40,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: hello from $m\"}]}")
  check "$m messages non-stream" 200 "$code" "$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d["content"][0]["text"][:40], d["usage"])' "$out" 2>/dev/null)"
  code=$(post /v1/messages "$KEY" "{\"model\":\"$m\",\"max_tokens\":40,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"Count 1 to 5\"}]}")
  check "$m messages stream" 200 "$code" "events=$(grep -c '^event:' "$out") delta_usage=$(grep -o '"output_tokens":[0-9]*' "$out" | tail -1)"
done

echo "== OpenAI /v1/chat/completions"
for m in $GPT_MODELS; do
  code=$(post /v1/chat/completions "$KEY" "{\"model\":\"$m\",\"max_completion_tokens\":60,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: hello from $m\"}]}")
  check "$m chat non-stream" 200 "$code" "$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(repr(d["choices"][0]["message"]["content"][:40]), d["usage"].get("prompt_tokens"), d["usage"].get("completion_tokens"))' "$out" 2>/dev/null)"
  code=$(post /v1/chat/completions "$KEY" "{\"model\":\"$m\",\"max_completion_tokens\":60,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"Count 1 to 5\"}]}")
  check "$m chat stream" 200 "$code" "chunks=$(grep -c '^data:' "$out") usage_chunk=$(grep -c '"completion_tokens"' "$out")"
done

echo "== OpenAI /v1/responses"
for m in $RESPONSES_MODELS; do
  code=$(post /v1/responses "$KEY" "{\"model\":\"$m\",\"max_output_tokens\":60,\"input\":\"Reply with exactly: hello from $m\"}")
  check "$m responses non-stream" 200 "$code" "$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d["status"], d["usage"].get("input_tokens"), d["usage"].get("output_tokens"))' "$out" 2>/dev/null)"
  code=$(post /v1/responses "$KEY" "{\"model\":\"$m\",\"max_output_tokens\":60,\"stream\":true,\"input\":\"Count 1 to 5\"}")
  check "$m responses stream" 200 "$code" "events=$(grep -c '^event:' "$out") completed=$(grep -c 'response.completed' "$out")"
done

echo "== Negative cases"
code=$(post "$A_PATH" "sk-nope" "$A_BODY")
check "unknown key -> 401 ($A_PATH)" 401 "$code" "$(head -c 120 "$out")"
code=$(post "$O_PATH" "sk-other-disabled" "$O_BODY")
check "disabled key -> 401 ($O_PATH)" 401 "$code" "$(head -c 120 "$out")"
code=$(post "$O_PATH" "$KEY" '{"model":"model-nope","messages":[{"role":"user","content":"x"}],"max_tokens":5}')
check "unknown model -> 404" 404 "$code" "$(head -c 120 "$out")"
code=$(post "$O_PATH" "$KEY" '{"messages":[{"role":"user","content":"x"}]}')
check "missing model -> 400" 400 "$code"
code=$(curl -sS -o "$out" -w '%{http_code}' -X POST "$GW$A_PATH" -H "x-api-key: $KEY" -H "anthropic-version: 2023-06-01" -H "Content-Type: application/json" --max-time 120 -d "$A_BODY")
check "x-api-key header accepted" 200 "$code"

sleep 2
echo "== Usage reports received by control plane"
curl -sS "$CP/debug/usages" | python3 -c '
import json,sys
rows=json.load(sys.stdin)
print(f"  {len(rows)} reports")
print(f"  {"model":16} {"provider_model":36} {"st":>3} {"in":>5} {"out":>5} {"cr":>4} {"cw":>4} {"rs":>4} {"ttft":>6} {"dur":>6}")
for r in rows:
    print(f"  {r["model_code"]:16} {r["provider_model_code"]:36} {r["status_code"]:>3} {r["input_tokens"]:>5} {r["output_tokens"]:>5} {r["cache_read_tokens"]:>4} {r["cache_write_tokens"]:>4} {r["reasoning_tokens"]:>4} {r["ttft"]:>6} {r["duration"]:>6}")
bad=[r for r in rows if r["status_code"]==200 and (r["input_tokens"]==0 or r["output_tokens"]==0)]
print(f"  successful reports missing tokens: {len(bad)}")
sys.exit(1 if bad else 0)
' || FAIL=$((FAIL+1))

echo; echo "PASS=$PASS FAIL=$FAIL"; rm -f "$out"; [ "$FAIL" = 0 ]
