# 运维手册

面向日常运维，覆盖模型新增、供应商接入、升级回滚、监控指标、日志格式与故障排查。

## 一、加模型：只动控制面

网关不认识任何模型名。客户端请求体里的 `model` 只是一个字符串，网关拿它去控制面下发的路由表里查候选供应商。
所以新增、下线、改优先级、调权重、切供应商，全部是控制面 `model-routes` 的数据变更，网关不用重启也不用改配置。

步骤：

1. 在控制面为新 `modelCode` 配路由：一个或多个 `providers`，每个指定 `providerCode`（必须是网关配置里已有的
   provider）、`providerModelCode`（供应商侧模型码）、`priority`、`weight`。
2. 确保控制面返回新的 ETag（`version` 变化）。
3. 等最多 `routes_poll_interval`（默认 30 秒），网关日志出现新的 `routes loaded`（带 version 和模型数），`/v1/models` 里能看到新模型，
   `llmgw_routes_models` 指标加一。
4. 用 smoke 脚本或 curl 发送一条请求验证。

Bedrock 上的模型码填跨区域 inference profile ID（`global.anthropic.claude-haiku-5` 这类），网关原样透传，不拼前缀。
同一个 `global.` 模型码可以同时路由到不同区域、不同账号的 Bedrock provider。

多级故障转移的配法：同一 `modelCode` 下放多个候选，`priority` 小的先试，同一 `priority` 内按 `weight` 加权随机。
例如主路 `priority: 10` 走跨账号 us-west-2，备路 `priority: 20` 走本账号东京，主路首包前失败（连接失败、超时、
429、5xx）自动切备路，客户端无感。

## 二、接新供应商：改网关配置，滚动重启

1. 在配置 `providers:` 下新增一段（字段见 [configuration.md](configuration.md)）：选认证方式，填 endpoints，
   凭证写 `secretsmanager://` 引用（推荐）或 `${ENV}` 从 K8s Secret 注入。
2. EKS 上：Secrets Manager 方式先把凭证写进 secret（放进已有 JSON secret 的新键即可，IAM 已按 `llm-gateway/*` 前缀授权），
   再更新 ConfigMap；明文方式则 K8s Secret 里加新的凭证键，Deployment 的 `env` 里加对应的 `valueFrom`。然后
   `kubectl rollout restart deployment/llm-gateway -n llm-gateway`。两副本滚动，`/readyz` 通过后才接流量。
3. 在控制面加路由引用新的 `providerCode`。
4. 用新路由的模型发一条请求，确认日志 `request completed` 里 `provider` 是新供应商、`usage_found` 为 true，控制面收到对应计量。

判断一个供应商能不能直接接：它需要提供 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 三种之一的
HTTP 接口，并且鉴权是 `Authorization: Bearer`、`x-api-key`、AWS SigV4 或无鉴权。
只提供部分协议的供应商只声明它有的 endpoint，路由到它不支持协议的请求会跳过它试下一个候选。

### 密钥的两种存法与轮转

网关自己的密钥只有三种：控制面 token、供应商 `api_key`、AssumeRole 的 `external_id`。每个字段独立选存法：

| 存法 | 配置写法 | 密钥落在哪 | 轮转方式 |
| --- | --- | --- | --- |
| Secrets Manager | `secretsmanager://<secret>#<键>` | AWS Secrets Manager，IRSA 角色按前缀授权 `GetSecretValue` | 改 secret 值，`kubectl rollout restart`，新副本启动时读新值 |
| 明文 | `${VAR}` | K8s Secret，以环境变量注入 Pod | 改 K8s Secret，`kubectl rollout restart` |

两种方式都是启动时读一次，运行中不刷新，所以轮转必然伴随一次滚动重启。控制面 token 的轮转顺序：控制面先同时接受新旧两个
token，改 secret，重启网关，确认日志 `secret resolved` 的 `version_id` 是新版本且 key-auth 正常，再让控制面废掉旧 token。
密钥值不进日志，`secret resolved` 只打字段名、secret id、JSON 键和版本号。

### 什么时候必须改代码

只有两种情况：

- **新认证方式**：比如 HMAC 签名。要在 `internal/provider` 加一个 `Authenticator` 实现并在 `Build` 里注册。
  Azure OpenAI 那种 URL 查询参数带 `api-version` 的不算，把查询参数写进 endpoint URL 即可。
- **新协议或协议变体**：比如 Gemini 原生 API，要在 `internal/protocol` 加协议识别、请求体改写、usage 解析、SSE 解析，
  在 `internal/proxy` 加路径映射。同一协议但 usage 字段缺失或改名的厂商也归这类，表现是计量 token 为 0，
  修法是在对应协议的 usage 解析里加兼容分支，改动小得多。

协议转换（客户端用 Anthropic 格式调 OpenAI 模型）不在上面两种里，它是第二阶段的范围，第一阶段明确不做。

### 本地验证外接 API 供应商

接 OpenAI 兼容或 Anthropic 格式的第三方 API，不需要 AWS 凭证，在本机就能把整条链路（鉴权、路由、模型码替换、流式透传、
计量）跑通。链路是 mock 控制面 + 网关 + 真实上游，仓库里有现成的样例：`configs/local-external.example.yaml`（网关配置）和
`configs/routes.external.example.json`（路由表）。

```bash
go build -o bin/gateway ./cmd/gateway && go build -o bin/mock-controlplane ./cmd/mock-controlplane

# 1. mock 控制面：-routes 指定路由表，modelCode 是客户端用的名字，providerModelCode 是供应商自己的模型名
./bin/mock-controlplane -listen :9090 -token mock-token -keys sk-demo-key -routes configs/routes.external.example.json &

# 2. 网关：api_key 从环境变量展开
MOONSHOT_API_KEY=<key> ./bin/gateway -config configs/local-external.example.yaml &

# 3. 非流式与流式各打一条
curl -s localhost:8080/v1/chat/completions -H 'Authorization: Bearer sk-demo-key' -H 'content-type: application/json' \
  -d '{"model":"kimi","messages":[{"role":"user","content":"hello"}]}'
curl -sN localhost:8080/v1/chat/completions -H 'Authorization: Bearer sk-demo-key' -H 'content-type: application/json' \
  -d '{"model":"kimi","stream":true,"messages":[{"role":"user","content":"hello"}]}'

# 4. 核对计量：每条请求一条记录，input / output token 应与供应商响应里的 usage 一致
curl -s localhost:9090/debug/usages | python3 -m json.tool

# 5. 可选：只跑 OpenAI chat 协议的冒烟（负向用例与计量核对照常；供应商有 Responses 端点时去掉 RESPONSES_MODELS="" 即可）
CLAUDE_MODELS="" RESPONSES_MODELS="" GPT_MODELS="kimi" GW=http://localhost:8080 CP=http://localhost:9090 KEY=sk-demo-key scripts/smoke.sh
```

改路由不用重启：`curl -X PUT localhost:9090/debug/routes -d @routes.json`，网关最多 `routes_poll_interval` 后拿到新表。
加第二家供应商就是配置里多一段 `providers.<code>`、路由里多一个候选，同一个 `modelCode` 挂两家并设不同 `priority` 就能在本地验证故障转移。

验证时看三处：网关日志 `request completed` 里 `provider` 是目标供应商、`usage_found` 为 true；`/debug/usages` 里的 token 数与
上游响应一致；流式响应的内容与直连供应商时逐字节相同（网关不改写流）。

几个前提：

- **网络出口**。OpenAI 与 Anthropic 的官方 API 不对部分地区（含中国香港）开放，本机直连会被拒，这是环境不是网关问题；
  国内厂商与各类 OpenAI 兼容网关通常没有限制。要验证官方 API，从受支持地区的机器发起，或在那里跑网关。
- **协议不转换**。OpenAI 兼容的供应商只能声明 `openai_chat` / `openai_responses`，Anthropic 格式的只能声明 `anthropic`。
  客户端用哪种格式进就只会路由到支持这种格式的候选，全部候选都不支持时返回 502，日志 `provider lacks endpoint for protocol`。
- **usage 字段按标准走**。解析规则见 [control-plane.md](control-plane.md) 的 usage 口径表。供应商把缓存命中放在自定义字段里
  （如 DeepSeek 的 `prompt_cache_hit_tokens`）时，这部分会计入 `input_tokens`，总量不差但缓存拆分为 0；要拆分得在
  `internal/protocol` 加兼容分支。供应商流式响应完全不带 usage 的，计量 token 为 0，`usage_found` 为 false。
- **`providerModelCode` 零加工**。填供应商要求的完整模型名，网关原样替换进请求体的 `model` 字段，不加前缀也不改大小写。

同一协议、字段稍有差异的模型不需要改代码，字段差异需要由客户端适配。典型例子：Bedrock 上的 GPT 模型要
`max_completion_tokens`，客户端传 `max_tokens` 会收到模型自己的 400，网关原样透传。

## 三、升级与回滚

- 镜像按 `tag@digest` 双写发布（如 `gateway:v0.6.1@sha256:...`，digest 取 ko 构建输出），Deployment 改 `image` 后 `kubectl apply`，滚动更新。
  拉取以 digest 为准，同名 tag 被重推也不会误用节点缓存的旧层；清单仍保留 `imagePullPolicy: Always`。
  发完用 `kubectl get pods -o jsonpath='{.items[*].status.containerStatuses[0].imageID}'` 核对 digest。
- 滚动更新时旧副本的退出顺序：收到 SIGTERM 后日志打 `shutting down`，`/readyz` 立即变 503，等 5 秒让 Service 摘掉它，
  然后关监听并等在途请求最多 `server.shutdown_timeout`（默认 280 秒，超时打 `shutdown deadline reached, in-flight requests cut`），
  最后用 10 秒刷计量队列，打 `bye`。Pod 的 `terminationGracePeriodSeconds` 是 300 秒，改 `shutdown_timeout` 时要一起改，
  保证 5 + shutdown_timeout + 10 不超过宽限期，否则会被 SIGKILL。
- 回滚 `kubectl rollout undo deployment/llm-gateway -n llm-gateway`。配置 ConfigMap 需另行还原，建议配置也进版本库。
- 升级前后各跑一遍 `scripts/smoke.sh`。改了协议解析的版本还要对照控制面收到的计量核对 token 数。
- 改了转发主流程、路由、计量队列的版本，额外跑 `go test -race ./... -count=1`，这一组包含故障注入场景（控制面挂起、上游超时、客户端断线、队列打满等），场景清单与已知问题见 [robustness-report.md](robustness-report.md)。
- 配置校验在启动时进行，配置错误的 Pod 无法启动、`/readyz` 不通过，旧副本继续服务，错误的配置不会被上线。

## 四、监控指标

`GET /metrics`，Prometheus 文本格式。

| 指标 | 类型 | 标签 | 含义与用法 |
| --- | --- | --- | --- |
| `llmgw_requests_total` | counter | `protocol` `provider` `status` | 请求量与状态码分布。`provider` 为空表示没到转发阶段就被拒（鉴权失败、无路由）。流式请求没跑完时 `status` 记 499 / 504 / 502（客户端断开 / 网关超时 / 上游中断），虽然客户端收到的响应头是 200 |
| `llmgw_request_duration_seconds` | histogram | `protocol` `provider` `model` | 端到端时延，流式含整个流。`model` 是供应商侧模型码 |
| `llmgw_ttft_seconds` | histogram | `protocol` `provider` `model` | 上游首字节时延。比 duration 更能反映供应商健康。同一供应商下不同模型差异很大（压测里 GPT-5.6 Sol 的首字是 Claude Sonnet 5 的数倍），按 `model` 分开看 |
| `llmgw_tokens_total` | counter | `provider` `model` `kind` | token 量，`kind` ∈ input / output / cache_read / cache_write / reasoning。`model` 是供应商侧模型码 |
| `llmgw_keyauth_rejects_total` | counter | `reason` | 控制面拒绝数，按 rejectReason |
| `llmgw_keyauth_errors_total` | counter | | 控制面不可达次数。非零且持续增长说明控制面有问题，客户端在收 503 |
| `llmgw_upstream_failovers_total` | counter | `provider` `reason` | 首包前放弃的尝试。`reason` ∈ transport / auth / 429 / 5xx 状态码。用于判断某个供应商是否不稳定 |
| `llmgw_metering_reports_total` | counter | `result` | 计量上报结果，`result` ∈ ok / duplicate / dropped / rejected。dropped 非零意味着计量有缺口 |
| `llmgw_metering_queue_depth` | gauge | | 待上报记录数。持续接近 `queue_size` 说明控制面上报接口跟不上 |
| `llmgw_routes_models` | gauge | | 当前路由快照里的模型数。骤降说明控制面下发的路由表少了模型 |
| `llmgw_routes_rejected_total` | counter | | 被网关拒收的路由快照数。控制面返回 200 但模型列表为空、而网关手里已有非空快照时拒收并沿用旧表，防止控制面故障把全部路由清空。持续增长说明控制面 model-routes 接口有问题 |

建议告警：

- `rate(llmgw_keyauth_errors_total[5m]) > 0` 持续 2 分钟：控制面鉴权接口不可用。
- `rate(llmgw_metering_reports_total{result="dropped"}[5m]) > 0`：计量丢失。
- `rate(llmgw_upstream_failovers_total[5m])` 按 provider 突增：该供应商异常。
- `histogram_quantile(0.99, sum by (le, provider, model) (rate(llmgw_ttft_seconds_bucket[5m])))` 超过阈值。阈值按模型定，PoC 冒烟里 Claude Sonnet 5 / Opus 5
  和 GPT-5.6 的非流式首字在 0.6 到 3.5 秒之间，正式环境先观察一到两周再定。
- `llmgw_routes_models == 0`：路由表为空（只会发生在启动时控制面就给了空表）。
- `increase(llmgw_routes_rejected_total[10m]) > 0`：控制面在下发空路由表，网关正在用旧表顶着。

这五条不必一次配齐。控制面不可达和计量丢失两条直接影响业务和账单，上线就配；供应商故障转移和首字时延需要基线，
观察后再定阈值，否则误报会多；路由为空是低频兜底，有余力再加。

## 五、日志

stdout，一行一条 JSON。级别由 `server.log_level` 控制。

每个请求结束记一条 `request completed`：

```json
{"time":"2026-09-03T01:31:09.654Z","level":"INFO","msg":"request completed",
 "request_id":"req_0b0e9c401abcdcfcbd57495e","protocol":"anthropic","model":"claude-sonnet-5-us","stream":false,
 "subject":"demo.user@example.com","provider":"bedrock-usw2-xacct","provider_model":"global.anthropic.claude-sonnet-5",
 "status":200,"duration_ms":1842,"ttft_ms":1841,"input_tokens":28,"output_tokens":16,"cache_read":0,"cache_write":0,
 "reasoning":0,"usage_found":true,"client_error":false,"truncated":""}
```

| 字段 | 说明 |
| --- | --- |
| `request_id` | 与响应头 `X-Request-Id`、计量的 `request_id` 三处一致 |
| `subject` | 控制面 key-auth 返回的 `subjectCode` |
| `provider` / `provider_model` | 最终提供响应的供应商与模型码 |
| `usage_found` | 是否从响应里解析到了 usage。`false` 时计量 token 为 0，多半是流被提前断开或供应商没返回 usage |
| `client_error` | 向客户端写响应时出错（客户端断开） |
| `truncated` | 流没有跑完时的原因：`client_disconnect`（上报 499）、`request_timeout`（上报 504）、`upstream_stream_error`（上报 502）。这三种记录 token 为 0、不计费；跑完的流为空字符串 |

其他关键日志：

- 启动：`secrets manager resolver ready`、`secret resolved`（每个 `secretsmanager://` 引用一条，带字段名与 `version_id`）、
  `aws identity resolved`（每个 aws_iam provider 一条，带 ARN）、`routes loaded`、`gateway listening`
- 路由：`routes loaded`（版本、模型数，启动和每次变更都打）、`routes poll failed, keeping last snapshot`（沿用旧快照；
  `err` 为 `empty route table rejected, keeping last snapshot` 时是控制面下发了空表被拒收）
- 退出：`shutting down`（带 `drain_delay`、`shutdown_timeout`）、`shutdown deadline reached, in-flight requests cut`、`metering shutdown deadline reached`、`bye`
- 鉴权：`key-auth unavailable`（返回 503）、`key-auth rejected`（原因、subject）
- 转发：`upstream transport error, failing over`、`upstream error status, failing over`（含上游 body 前 2KB）、
  `route references unconfigured provider`、`provider lacks endpoint for protocol`、`request failed`
- 计量：`usage report retry`、`usage report failed permanently`、`usage report rejected by control plane`、`metering queue full, report dropped`

日志里不打印客户端 key、供应商凭证和请求响应正文。

## 六、故障排查

| 现象 | 可能原因 | 怎么确认 | 处理 |
| --- | --- | --- | --- |
| 所有请求 503 `authorization service unavailable` | 控制面 key-auth 不可达、超时、返回非 `00000`、token 错 | `llmgw_keyauth_errors_total` 增长；日志 `key-auth unavailable` 带 err | 检查控制面健康、网络策略、`control_plane.token`；必要时放宽 `key_auth_timeout` |
| 某模型 404 `model "x" has no active route` | 控制面路由里没有这个 modelCode，或该模型 `providers` 为空 | `/v1/models` 看不到该模型；`llmgw_routes_models` | 在控制面补路由；看 `routes poll failed` 日志确认网关拿到了最新路由 |
| 某模型 502 `all upstream attempts failed: provider "x" not configured` | 路由引用了配置里没有的 providerCode | 日志 `route references unconfigured provider` | 网关配置加 provider 并重启，或改路由 |
| 某模型 502 `... does not serve openai_responses` | 路由把该模型配给了不支持这个协议的供应商 | 日志 `provider lacks endpoint for protocol` | 给 provider 补 endpoint，或路由换供应商 |
| 502 `... upstream status 5xx` / 504 | 所有候选都在首包前失败 | `llmgw_upstream_failovers_total` 按 provider 看；日志有上游 body 片段 | 供应商侧问题；考虑加备用候选 |
| 502 `upstream response body exceeds gateway limit` | 非流式响应体超过 64 MiB 上限，网关拒绝转发截断内容 | 日志 `upstream response body too large, rejecting` 带 `limit_bytes`/`upstream_status`；计量记 502、token 0 | 该响应确实过大；需要大响应时改用流式，或评估上限 |
| Bedrock 返回 401/403 `not authorized to perform: bedrock:InvokeModel on resource: ...project/default` | Responses API 需要 `project/default` 资源权限，IAM 策略未授予 | 只有 `/v1/responses` 失败，messages / chat 正常 | IAM 策略加 `arn:aws:bedrock:*:*:project/default`；跨账号时是对端角色的策略 |
| Bedrock 返回 400 `Access to OpenAI models is not allowed from unsupported countries...` | 调用方出口所在地不受支持 | 只有 GPT 系列失败，Claude 正常 | 把网关部署在受支持的区域；本地开发环境属于此情况 |
| GPT 模型 400 `Unsupported parameter: max_tokens` | 客户端传了 `max_tokens`，Bedrock 上的 GPT 要 `max_completion_tokens` | 客户端收到的 400 正文来自 Bedrock，明确提到 `max_tokens` | 客户端改字段。网关第一阶段不做转换 |
| 启动失败 `provider setup failed ... resolve aws credentials` | IRSA 配置不完整：ServiceAccount 缺 role-arn 注解、角色信任策略不正确、OIDC 未启用 | Pod 日志首行 `provider setup failed`，err 含 `resolve aws credentials` | 核对 `eksctl create iamserviceaccount` 输出与 `10-serviceaccount.yaml` |
| 启动失败 `secret resolution failed` | Secrets Manager 引用取不到：IAM 缺 `GetSecretValue`、secret 名或 JSON 键写错、私网环境没有 secretsmanager VPC Endpoint（表现为超时） | Pod 日志 `secret resolution failed`，err 里带字段名和 secret id；`AccessDeniedException` 是 IAM，`ResourceNotFoundException` 是名字，`context deadline exceeded` 是网络 | 对照 `deploy/eks/secrets-read-policy.json` 与 secret 名；私网看 private-networking.md 的 VPC 资源表 |
| 启动失败 `could not load initial routes`（重试 60 秒后退出） | 控制面不可达 | Pod CrashLoopBackOff，日志反复出现 `initial routes load failed, retrying` | 先修控制面连通性 |
| 计量 `dropped` 增长 | 控制面上报接口持续失败或队列满 | `llmgw_metering_queue_depth`、日志 `usage report failed permanently` | 检查控制面；调大 `queue_size` / `workers` 只能缓冲不能根治 |
| 计量 token 为 0 但请求 200 | 供应商没返回 usage，或流被客户端提前断开 | 日志 `usage_found: false` | 确认供应商 usage 字段格式；OpenAI 兼容端点是否忽略了 `stream_options` |
| 流式请求在 10 分钟处被切断 | `server.request_timeout` 到期 | 日志 `status: 504` 或客户端收到截断 | 调大 `request_timeout`，同时检查上游为何这么慢 |
| kubectl 连不上集群 | 集群 API 端点白名单只放行了特定出口 IP | `dial tcp ... i/o timeout` | `aws eks update-cluster-config --resources-vpc-config publicAccessCidrs=...` |
| NetworkPolicy 似乎没生效，Pod 能出公网 | VPC CNI 标准模式下新 Pod 启动头几秒策略尚未下发 | 探测 Pod `sleep 20` 后再测 | 属正常；要在这几秒内也拒绝流量，把 NodeClass `networkPolicy` 改为 `DefaultDeny`，见 private-networking.md |

排查一条具体请求：拿客户端收到的 `X-Request-Id`，在日志里搜 `request_id`，能看到 provider、状态、时延、token；
再拿同一个 id 去控制面查计量记录。`X-Upstream-Request-Id` 是供应商侧的请求 ID，找供应商支持时用。

## 七、mock 控制面

`cmd/mock-controlplane` 是控制面替身，实现三个网关接口加两个调试接口，正式环境不部署。

| 接口 | 说明 |
| --- | --- |
| `GET /debug/usages` | 返回收到的全部计量记录 |
| `DELETE /debug/usages` | 清空 |
| `PUT /debug/routes` | 用请求体替换路由表并生成新 ETag，测试路由热更新 |

启动参数：`-listen`、`-token`（或环境变量 `MOCK_CP_TOKEN`）、`-keys`（逗号分隔的合法 key，或 `MOCK_CP_KEYS`）、
`-token-file` / `-keys-file`（从文件读取，覆盖前两项；集群清单用它读挂载的 Secret，不经环境变量）、
`-routes <json>`（路由文件，未指定时使用内置的四个 Bedrock 模型）。以 `-disabled` 结尾的 key 返回 `KEY_DISABLED`，
以 `-quota0` 结尾的返回 `QUOTA_EXHAUSTED`，其他不在列表里的返回 `KEY_NOT_FOUND`，用来测负向路径。
