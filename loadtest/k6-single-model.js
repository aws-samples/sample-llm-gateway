// 单模型并发梯度探测：固定 VU 数逐级抬高，每个请求打印状态与首字节时间，用于定位某个模型从哪个并发开始变慢。
// 环境变量：GW、KEY、MODEL、PROTO（anthropic | openai_chat）、STAGES（如 "1:45s,5:45s,20:60s,50:60s"）、MAX_OUTPUT_TOKENS。
import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

const GW = __ENV.GW || 'http://llm-gateway.llm-gateway.svc.cluster.local:8080';
const KEY = __ENV.KEY || 'sk-demo-key';
const MODEL = __ENV.MODEL || 'gpt-5.6-sol';
const PROTO = __ENV.PROTO || 'openai_chat';
const MAX_OUT = Number(__ENV.MAX_OUTPUT_TOKENS || 300);
const STAGES = (__ENV.STAGES || '1:45s,5:45s,20:60s,50:60s').split(',').map((s) => {
  const [t, d] = s.split(':'); return { target: Number(t), duration: d };
});

const PARA = 'The gateway forwards each request to the configured provider without rewriting the protocol. ' +
  'It authenticates the caller against the control plane, picks a route by priority and weight, signs the upstream call, ' +
  'streams the response back to the client while parsing usage on the side, and reports token counts asynchronously. ';
let LONG = '';
while (LONG.length < 8000) LONG += PARA;

const ttfb = new Trend('llm_ttfb_ms', true);

export const options = {
  scenarios: { probe: { executor: 'ramping-vus', startVUs: 1, stages: STAGES, gracefulRampDown: '10s', gracefulStop: '130s' } },
  summaryTrendStats: ['avg', 'p(50)', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export default function () {
  const p = `[${__VU}-${__ITER}-${Math.random().toString(36).slice(2)}] Summarize the following text in three sentences.\n\n${LONG}`;  // nosemgrep: nodejs_scan.javascript-crypto-rule-node_insecure_random_generator -- 只为打散提示缓存，非安全用途
  let res;
  const t0 = Date.now();
  if (PROTO === 'anthropic') {
    res = http.post(`${GW}/v1/messages`, JSON.stringify({ model: MODEL, max_tokens: MAX_OUT, stream: true, messages: [{ role: 'user', content: p }] }),
      { headers: { 'Content-Type': 'application/json', 'x-api-key': KEY, 'anthropic-version': '2023-06-01' }, timeout: '120s' });
  } else {
    res = http.post(`${GW}/v1/chat/completions`, JSON.stringify({ model: MODEL, max_completion_tokens: MAX_OUT, stream: true, stream_options: { include_usage: true }, messages: [{ role: 'user', content: p }] }),
      { headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${KEY}` }, timeout: '120s' });
  }
  ttfb.add(res.timings.waiting);
  const active = __ENV.K6_ACTIVE || '';
  console.log(`REQ t=${Math.round((t0 - __ENV.START_MS) / 1000)} status=${res.status} ttfb_ms=${Math.round(res.timings.waiting)} dur_ms=${Math.round(res.timings.duration)} rid=${res.headers['X-Request-Id'] || ''}`);  // nosemgrep: no-stringify-keys, javascript.lang.correctness.no-stringify-keys -- 污点规则误报：请求体 JSON.stringify 让 res 被整体标记，这里只是按头名读响应头
  check(res, { 'status 200': (r) => r.status === 200 });
}
