#!/usr/bin/env bash
# One request per route through a port-forward to the gateway. Usage: GW=http://127.0.0.1:18080 KEY=sk-lab-key ./50-smoke.sh
set -uo pipefail
GW="${GW:-http://127.0.0.1:18080}"; KEY="${KEY:-sk-lab-key}"
t() { local label="$1" path="$2" body="$3"
  printf "%-40s " "$label"
  curl -s --max-time 90 -o /tmp/smoke.json -w 'HTTP %{http_code} %{time_total}s  ' "$GW$path" -H "Authorization: Bearer $KEY" -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' -d "$body"
  python3 -c "import json,sys
d=json.load(open('/tmp/smoke.json'))
if 'content' in d: print(repr(d['content'][0].get('text','')[:50]), d.get('usage',{}).get('output_tokens'))
elif 'choices' in d: print(repr(d['choices'][0]['message']['content'][:50]), d.get('usage',{}).get('completion_tokens'))
elif 'output' in d: print(repr(next((c['text'] for i in d['output'] if i['type']=='message' for c in i['content']),'')[:50]), d.get('usage',{}).get('output_tokens'))
else: print(json.dumps(d)[:120])" 2>/dev/null || head -c 120 /tmp/smoke.json; echo; }
A='{"model":"%s","max_tokens":200,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"Say hi in 3 words."}]}'
C='{"model":"%s","max_completion_tokens":200,"reasoning_effort":"none","messages":[{"role":"user","content":"Say hi in 3 words."}]}'
R='{"model":"%s","store":false,"reasoning":{"effort":"none"},"input":"Say hi in 3 words."}'
t "A same-acct Tokyo Claude"              /v1/messages         "$(printf "$A" claude-sonnet-5)"
t "A same-acct Tokyo GPT (chat)"          /v1/chat/completions "$(printf "$C" gpt-5.6-sol)"
t "B xacct usw2 Claude, native endpoint"  /v1/messages         "$(printf "$A" claude-sonnet-5-us)"
t "B xacct usw2 GPT, native (chat)"       /v1/chat/completions "$(printf "$C" gpt-5.6-sol-us)"
t "B xacct usw2 GPT, native (responses)"  /v1/responses        "$(printf "$R" gpt-5.6-sol-us)"
t "B xacct Claude via InvokeModel"        /v1/messages         "$(printf "$A" claude-sonnet-5-us-invoke)"
t "B xacct GPT via InvokeModel (chat)"    /v1/chat/completions "$(printf "$C" gpt-5.6-sol-us-invoke)"
t "translate: Anthropic client -> B GPT"  /v1/messages         "$(printf "$A" claude-via-gpt-us)"
t "translate: Responses client -> B Claude" /v1/responses      "$(printf "$R" gpt-5)"
t "HA: prefer B, fall back to A"          /v1/messages         "$(printf "$A" claude-sonnet-5-ha)"
