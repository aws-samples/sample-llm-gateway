# 控制面对接契约

网关只依赖控制面的三个接口，都挂在 `<base_url>/admin/gateway/` 下，统一带 token 头（默认 `X-HIGRESS-Token`）。
原始契约由控制面团队以 `.http` 用例和 OpenAPI 文档提供，不随本仓库分发，本文按网关实际实现的口径整理。

| 接口 | 方法 | 用途 | 网关调用时机 |
| --- | --- | --- | --- |
| `/admin/gateway/model-routes` | GET | 全量路由表 | 启动时一次，之后每 `routes_poll_interval` 轮询 |
| `/admin/gateway/key-auth` | POST | 合并闸门：key、模型白名单、RPM/TPM、额度 | 每个请求一次，在转发之前 |
| `/admin/gateway/usage/report` | POST | 计量上报 | 每个过了闸门的请求结束后异步一次 |

## model-routes

请求：

```
GET /admin/gateway/model-routes
X-HIGRESS-Token: <token>
If-None-Match: <上次的 ETag>      # 首次不带
```

响应：`200` 时响应体**不包 ApiResult**，直接是路由文档，并带 `ETag` 头；ETag 匹配时 `304` 且响应体为空。

```json
{
  "version": "2026-09-03T01:00:00Z-17",
  "models": [
    {
      "modelCode": "claude-sonnet-5",
      "providers": [
        { "providerCode": "bedrock-usw2-xacct", "providerModelCode": "global.anthropic.claude-sonnet-5", "priority": 10, "weight": 100 },
        { "providerCode": "bedrock",            "providerModelCode": "global.anthropic.claude-sonnet-5", "priority": 20, "weight": 100 }
      ]
    }
  ]
}
```

字段语义：

- `modelCode`：客户端在请求体 `model` 里写的名字，也是 key-auth 和计量里的 `model_code`。
- `providerCode`：必须与网关配置 `providers` 的 key 一致，否则该候选被跳过并记 `route references unconfigured provider` 日志。
- `providerModelCode`：转发时替换进请求体 `model` 的字符串，网关不做任何加工，计量里原样上报。
- `priority`：数值小的先试。同一模型的候选按 priority 分层，前一层全部失败（首包前）才试下一层。
- `weight`：同一层内按权重随机排序，权重越大越可能排前。0 或负数按 1 处理，不会导致候选被排除。
- `version`：网关只用作日志和 `/readyz` 展示，判断是否变化靠 ETag。

网关的容错：轮询失败（网络、5xx、解析错误）只记 warn 日志，继续用上一份快照；启动时若无法获取路由，重试 60 秒后退出。
所以控制面短暂不可用不影响已在运行的网关转发，只影响新路由的下发。

## key-auth

请求：

```
POST /admin/gateway/key-auth
X-HIGRESS-Token: <token>
Content-Type: application/json

{ "apiKey": "sk-xxx", "modelCode": "claude-sonnet-5" }
```

响应包 ApiResult：

```json
{
  "code": "00000",
  "msg": "ok",
  "data": {
    "valid": false,
    "rejectReason": "QUOTA_EXHAUSTED",
    "message": "monthly quota exhausted",
    "keyCode": "key_01H...",
    "subjectType": "USER",
    "subjectCode": "demo.user@example.com",
    "modelCode": "claude-sonnet-5",
    "remainingQuotaUsd": 0
  }
}
```

网关的处理：

- `code` 不是 `00000`、HTTP 非 2xx、网络错误、超过 `key_auth_timeout`：一律视为控制面不可用，客户端收到 **503**，
  计 `llmgw_keyauth_errors_total`。这是有意的 fail-closed，控制面不可用期间不会放行任何请求。
- `valid: true`：放行，`subjectCode` 进日志。
- `valid: false`：按 `rejectReason` 映射成客户端协议格式的错误，`message` 非空时作为错误消息，否则用 `rejectReason` 本身。

### 拒绝原因到 HTTP 的映射

| `rejectReason` | HTTP | OpenAI 格式 `error.type` / `error.code` | Anthropic 格式 `error.type` |
| --- | --- | --- | --- |
| `KEY_NOT_FOUND` | 401 | `authentication_error` / `invalid_api_key` | `authentication_error` |
| `KEY_DISABLED` | 401 | `authentication_error` / `invalid_api_key` | `authentication_error` |
| `MODEL_NOT_ALLOWED` | 403 | `permission_error` / `model_not_allowed` | `permission_error` |
| `MODEL_NO_PROVIDER` | 404 | `not_found_error` / `model_not_found` | `not_found_error` |
| `RPM_EXCEEDED` | 429 | `rate_limit_error` / `rate_limit_exceeded` | `rate_limit_error` |
| `TPM_EXCEEDED` | 429 | `rate_limit_error` / `rate_limit_exceeded` | `rate_limit_error` |
| `QUOTA_EXHAUSTED` | 429 | `rate_limit_error` / `insufficient_quota` | `rate_limit_error` |
| 其他未知值 | 403 | `permission_error` / `<原值>` | `permission_error` |

OpenAI 格式（chat 与 responses 共用）：

```json
{ "error": { "message": "monthly quota exhausted", "type": "rate_limit_error", "param": null, "code": "insufficient_quota" } }
```

Anthropic 格式：

```json
{ "type": "error", "error": { "type": "rate_limit_error", "message": "monthly quota exhausted" } }
```

网关自身产生的其他错误也用同样的两种格式：400（请求体不是 JSON、缺 `model`）、401（未携带 key）、404（模型没有路由）、
405、413、502（所有候选都失败）、503（控制面不可用）、504（`request_timeout` 到期）。上游返回的错误则原样透传，
状态码、响应体、`Content-Type` 都不改。

## usage/report

请求，字段全部 snake_case：

```
POST /admin/gateway/usage/report
X-HIGRESS-Token: <token>
Content-Type: application/json

{
  "request_id": "req_0b0e9c401abcdcfcbd57495e",
  "api_key": "sk-xxx",
  "model_code": "claude-sonnet-5",
  "provider_model_code": "global.anthropic.claude-sonnet-5",
  "start_time": "2026-09-03T01:31:07.812Z",
  "duration": 1842,
  "ttft": 1841,
  "status_code": 200,
  "input_tokens": 28,
  "output_tokens": 16,
  "total_tokens": 44,
  "cache_read_tokens": 0,
  "cache_write_tokens": 0,
  "reasoning_tokens": 0
}
```

| 字段 | 说明 |
| --- | --- |
| `request_id` | 网关生成，同时放在响应头 `X-Request-Id` 返回给客户端。控制面用它做幂等键，重复上报应返回 `duplicate: true` |
| `api_key` | 客户端原始 key |
| `model_code` / `provider_model_code` | 分别是客户端写的模型名和实际转发用的供应商模型码 |
| `start_time` | 网关收到请求的时刻，UTC，`YYYY-MM-DDTHH:mm:ss.SSSZ` |
| `duration` | 端到端毫秒数，流式包含整个流 |
| `ttft` | 上游第一个响应字节到达的毫秒数。非流式与 `duration` 接近 |
| `status_code` | 返回给客户端的 HTTP 状态码 |
| `input_tokens` | 未命中缓存的输入 token |
| `cache_read_tokens` | 命中缓存的输入 token |
| `cache_write_tokens` | 写入缓存的输入 token |
| `output_tokens` | 全部生成 token，含 reasoning / thinking |
| `reasoning_tokens` | output 中属于推理的部分，供应商未单独返回时为 0 |
| `total_tokens` | 以上五项之和减去 reasoning（即 input + cache_read + cache_write + output） |

响应包 ApiResult，`data` 为 `{ "accepted": true, "duplicate": false, "message": "" }`。

### 上报规则

- 过了 key-auth 的请求都上报，包括上游失败的：`status_code` 填实际返回值，token 全为 0。
- 没有跑完的流也按这个样子上报（客户已确认不计费）：`status_code` 为 504（被网关 `request_timeout` 切断）、499（客户端先断开）
  或 502（上游连接中途断掉），token 全为 0。判定依据是上游流是否以正常 EOF 结束且客户端收下了全部内容，只要有一方中断就算不完整。
  客户端拿到的 HTTP 状态仍是 200（响应头早已发出），499 / 504 / 502 只出现在计量、日志和 `llmgw_requests_total` 里。
- key-auth 拒绝的请求不上报，拒绝决定由控制面作出，无需再回传。
- 所有候选都在首包前失败时也上报一条，`provider_model_code` 是最后一个尝试的候选。
- 上报是异步的，客户端响应不依赖上报结果。网络错误、超时、5xx 按指数退避重试 `max_retries` 次后丢弃并计
  `llmgw_metering_reports_total{result="dropped"}`；控制面返回业务拒绝不重试，计 `result="rejected"`。
- 进程崩溃会丢队列里未发出的记录。控制面应保留对账或补算任务。

### usage 口径：三种协议的归一规则

三种协议的 token 统计方式不同，网关统一归一到与控制面价格卡一致的五项：

| 网关字段 | OpenAI chat | OpenAI responses | Anthropic messages |
| --- | --- | --- | --- |
| `input_tokens` | `prompt_tokens − prompt_tokens_details.cached_tokens − cache_write` | `input_tokens − input_tokens_details.cached_tokens − cache_write` | `input_tokens`（本身就不含缓存） |
| `cache_read_tokens` | `prompt_tokens_details.cached_tokens` | `input_tokens_details.cached_tokens` | `cache_read_input_tokens` |
| `cache_write_tokens` | 供应商扩展字段 `cache_write_tokens` / `cache_creation_input_tokens`，没有则 0 | 同左 | `cache_creation_input_tokens` |
| `output_tokens` | `completion_tokens` | `output_tokens` | `output_tokens` |
| `reasoning_tokens` | `completion_tokens_details.reasoning_tokens` | `output_tokens_details.reasoning_tokens` | 0（thinking 已含在 output 里，Anthropic 不单列） |

流式时的取值来源：

- OpenAI chat：最后一个带 `usage` 的 chunk。网关会给流式请求注入 `stream_options.include_usage: true`，否则 OpenAI 兼容端点在流式响应中不返回 usage。
- OpenAI responses：`response.completed`、`response.incomplete`、`response.failed` 事件里的 `response.usage`。
- Anthropic：`message_start` 里的 `usage`（input、cache）加 `message_delta` 里的 `usage`（output）。

流被客户端提前断开、被 `request_timeout` 截断、或上游连接中途断掉时，网关会取消对上游的请求（避免上游继续生成到 max_tokens）。
这三种情况最终 usage 都拿不到（供应商把它放在流末尾），按上面的规则上报 499 / 504 / 502 且 token 为 0，日志里 `truncated` 字段
分别是 `client_disconnect` / `request_timeout` / `upstream_stream_error`。供应商侧仍会按实际已生成的 token 计费，这部分差额由客户
承担，是客户确认的口径。流正常结束但供应商没返回 usage 时，`status_code` 为 200、token 为 0，日志里 `usage_found: false`。