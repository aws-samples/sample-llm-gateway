// k6 压测脚本：按客户量级（约 3M TPM、100+ 并发连接）打网关，上游是真 Bedrock。
// 用法见 loadtest/README.md。环境变量：GW（网关地址）、KEY（客户端 key）、
// CLAUDE_MODEL / GPT_MODEL（modelCode）、TARGET_RPS（稳态每秒请求数，默认 20）、HOLD（稳态时长，默认 3m）、
// OVER_RPS（末段冲高速率，默认 1.5 倍）、GPT_SHARE（OpenAI 流量占比，默认 0.3）、MAX_OUTPUT_TOKENS（默认 300）。
import http from 'k6/http';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';

const GW = __ENV.GW || 'http://llm-gateway.llm-gateway.svc.cluster.local:8080';
const KEY = __ENV.KEY || 'sk-demo-key';
const CLAUDE_MODEL = __ENV.CLAUDE_MODEL || 'claude-sonnet-5';
const GPT_MODEL = __ENV.GPT_MODEL || 'gpt-5.6-sol';
const TARGET = Number(__ENV.TARGET_RPS || 20);
const HOLD = __ENV.HOLD || '3m';
const OVER = __ENV.OVER_RPS ? Number(__ENV.OVER_RPS) : Math.round(TARGET * 1.5);
const MAX_OUT = Number(__ENV.MAX_OUTPUT_TOKENS || 300);
const GPT_SHARE = Number(__ENV.GPT_SHARE || 0.3); // OpenAI chat 流量占比，0 表示只压 Anthropic 一路

// 约 2000 token 的输入：一段固定英文重复到 8000 字符左右，再加每次不同的 nonce 防 prompt cache。
const PARA = 'The gateway forwards each request to the configured provider without rewriting the protocol. ' +
  'It authenticates the caller against the control plane, picks a route by priority and weight, signs the upstream call, ' +
  'streams the response back to the client while parsing usage on the side, and reports token counts asynchronously. ';
let LONG = '';
while (LONG.length < 8000) LONG += PARA;

const ttfb = new Trend('llm_ttfb_ms', true);
const tokensIn = new Counter('llm_prompt_chars');
const errBody = new Counter('llm_error_responses');

function stages(rate) {
  return [
    { target: Math.round(rate / 2), duration: '30s' },
    { target: rate, duration: HOLD },
    { target: OVER, duration: '1m' },
  ];
}

function buildScenarios() {
  const sc = {};
  const aRate = Math.round(TARGET * (1 - GPT_SHARE));
  const gRate = Math.round(TARGET * GPT_SHARE);
  if (aRate > 0) sc.anthropic_stream = {
    executor: 'ramping-arrival-rate', exec: 'anthropic', timeUnit: '1s',
    startRate: 2, preAllocatedVUs: 150, maxVUs: 600, stages: stages(aRate),
  };
  if (gRate > 0) sc.openai_chat_stream = {
    executor: 'ramping-arrival-rate', exec: 'openaiChat', timeUnit: '1s',
    startRate: 1, preAllocatedVUs: 50, maxVUs: 200, stages: stages(gRate),
  };
  return sc;
}

export const options = {
  discardResponseBodies: false,
  scenarios: buildScenarios(),
  summaryTrendStats: ['avg', 'p(50)', 'p(90)', 'p(95)', 'p(99)', 'max'],
  thresholds: {
    'http_req_failed': ['rate<0.01'],
    'checks': ['rate>0.99'],
  },
};

function buildPrompt() {
  return `[${__VU}-${__ITER}-${Math.random().toString(36).slice(2)}] Summarize the following text in three sentences.\n\n${LONG}`;  // nosemgrep: nodejs_scan.javascript-crypto-rule-node_insecure_random_generator -- 只为打散提示缓存，非安全用途
}

export function anthropic() {
  const p = buildPrompt();
  tokensIn.add(p.length);
  const res = http.post(`${GW}/v1/messages`, JSON.stringify({
    model: CLAUDE_MODEL, max_tokens: MAX_OUT, stream: true,
    messages: [{ role: 'user', content: p }],
  }), {
    headers: { 'Content-Type': 'application/json', 'x-api-key': KEY, 'anthropic-version': '2023-06-01' },
    tags: { proto: 'anthropic' }, timeout: '120s',
  });
  ttfb.add(res.timings.waiting, { proto: 'anthropic' });
  const ok = check(res, {
    'anthropic 200': (r) => r.status === 200,
    'anthropic stream complete': (r) => r.status === 200 && r.body && r.body.indexOf('message_stop') !== -1,
  });
  if (!ok) { errBody.add(1); if (res.status !== 200) console.warn(`anthropic ${res.status}: ${String(res.body).slice(0, 200)}`); }
}

export function openaiChat() {
  const p = buildPrompt();
  tokensIn.add(p.length);
  const res = http.post(`${GW}/v1/chat/completions`, JSON.stringify({
    model: GPT_MODEL, max_completion_tokens: MAX_OUT, stream: true,
    stream_options: { include_usage: true },
    messages: [{ role: 'user', content: p }],
  }), {
    headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${KEY}` },
    tags: { proto: 'openai_chat' }, timeout: '120s',
  });
  ttfb.add(res.timings.waiting, { proto: 'openai_chat' });
  const ok = check(res, {
    'openai 200': (r) => r.status === 200,
    'openai stream complete': (r) => r.status === 200 && r.body && r.body.indexOf('[DONE]') !== -1,
  });
  if (!ok) { errBody.add(1); if (res.status !== 200) console.warn(`openai ${res.status}: ${String(res.body).slice(0, 200)}`); }
}
