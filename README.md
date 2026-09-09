# sample-llm-gateway

> A deliberately thin LLM pass-through gateway in Go: OpenAI Chat / Responses and Anthropic Messages in, the same protocol out, with Amazon Bedrock over SigV4 / IRSA (cross-region, cross-account, fully private) or any HTTP model provider behind it. Auth, quota and metering are delegated to an existing control plane through three HTTP endpoints. Documentation is in Chinese.

一个刻意做得很薄的 LLM 转发器，Go 编写，交付给已有「模型网关控制面」的团队使用。它位于 OpenAI / Anthropic SDK
客户端与模型供应商之间：供应商可以是 Amazon Bedrock（IAM 原生鉴权，支持跨区域、跨账号、全私网），也可以是任何
HTTP 形态的模型服务（OpenAI、Anthropic、Moonshot、各类 MaaS）。鉴权、额度、计量都交给已有控制面，网关通过三个
HTTP 接口与之对接。

网关**不做协议转换**：以 OpenAI Chat Completions 进来的请求，以 OpenAI Chat Completions 出去；`/v1/messages` 进来
就以 Anthropic Messages 出去。网关对请求和响应的全部改动见 [网关改了什么](#网关改了什么)。

```
客户端 ──▶ /v1/chat/completions ─┐                       ┌─▶ bedrock-runtime（SigV4，IRSA，可跨账号/跨区域/VPC Endpoint）
           /v1/responses         ├─▶ sample-llm-gateway ─┼─▶ api.openai.com（Bearer）
           /v1/messages          ┘         │  ▲          └─▶ 任意 OpenAI / Anthropic 兼容的 HTTP 端点
                                           ▼  │
                               控制面：model-routes / key-auth / usage-report
```

## 文档导航

| 文档 | 读者 | 内容 |
| --- | --- | --- |
| 本文 | 先读 | 定位与边界、请求链路、快速开始、日常运维改动落在哪一层、目录结构 |
| [docs/configuration.md](docs/configuration.md) | 部署与运维 | 配置文件全部字段、默认值、环境变量展开规则 |
| [docs/control-plane.md](docs/control-plane.md) | 控制面开发 | 三个接口的请求响应、拒绝原因到 HTTP 状态码的映射、usage 字段口径 |
| [docs/operations.md](docs/operations.md) | 运维 | 加模型、加供应商、升级回滚、监控指标、日志字段、故障排查 |
| [docs/private-networking.md](docs/private-networking.md) | 网络与安全 | 跨区域、跨账号、VPC Endpoint 全私网部署，PoC 实测结果 |
| [docs/robustness-report.md](docs/robustness-report.md) | 上线评审 | 故障注入与并发测试的场景、结果、发现的问题与加固建议 |
| [CHANGELOG.md](CHANGELOG.md) | 全部 | 各版本变更、升级注意事项 |

## 定位与边界

网关负责的事：

- 识别三种入口协议，取出客户 key 和 `model`，向控制面做一次合并鉴权（key、模型白名单、RPM/TPM、额度）。
- 按控制面下发的路由表选供应商，把 `model` 改成供应商侧模型码，注入供应商凭证，原样转发。
- 首个响应字节之前的失败自动切到下一个候选供应商。
- 从响应（含 SSE 流）里解析 token 用量，归一后异步上报控制面。
- 暴露 Prometheus 指标、JSON 日志、健康检查、`/v1/models`。

网关不负责的事，原因各不相同：

- 不发 key、不存 key、不算钱、不做额度扣减。这些控制面已经有了，`key-auth` 返回放行即放行，网关不再做二次判断。
- 不做协议转换、不改请求字段、不补默认参数。这是第一阶段的产品决定，客户端需自行满足所选模型的字段要求。
- 不做流中重试。流一旦开始客户端已经收到部分内容，重试会造成重复输出和重复计费，所以上游错误原样透传。
- 不持久化计量。进程内队列加重试，崩溃会丢队列里的记录，依赖控制面对账兜底。
- 不暴露公网入口。Service 是 ClusterIP，入口方式（Ingress / ALB / 内网直连）由部署方决定。

## 请求链路

1. 按路径识别协议：`/v1/chat/completions`、`/v1/responses`、`/v1/messages`。允许多一段前缀，如 `/openai/v1/chat/completions`。
2. 从 `Authorization: Bearer <key>` 或 `x-api-key: <key>` 取客户 key，从请求体顶层取 `model` 和 `stream`。
3. 调 `POST /admin/gateway/key-auth`。控制面不可达或超时直接返回 503（fail-closed），拒绝原因按客户端协议格式映射成 401 / 403 / 404 / 429。
4. 在路由快照里查该模型：候选按 `priority` 分层（数值小的先试），同层内按 `weight` 加权随机排序，最多试 `max_failover_attempts` 个。
5. 把 `model` 改成候选的 `providerModelCode`，只透传白名单里的请求头，注入供应商凭证，转发到 `<endpoint>/chat/completions|/responses|/messages`。
6. 候选失败且还没给客户端写过任何字节时切下一个：连接失败、首包前超时、鉴权失败、HTTP 429、HTTP 5xx。所有候选都失败返回 502（超时 504）。
7. 响应原样流回客户端，同时从 JSON 或 SSE 事件里解析 usage。
8. 计量记录进内存队列，后台 worker 异步上报，失败指数退避重试。过了 key-auth 的请求都上报，上游失败也报（token 为零、`status_code` 照填）。

这八步里第 6 步是最常被问到的取舍。切换只能发生在首个响应字节之前，因为一旦开始向客户端写流，HTTP 状态码已经发出、
客户端可能已经在渲染内容，此时换供应商重发会产生重复输出和重复计费，网关又无法撤回已发出的字节。所以流中的上游错误
原样透传给客户端，由客户端决定是否重试。另一个容易踩的地方是第 3 步的 fail-closed：控制面不可用时所有请求都会 503，
这是有意为之，代价是控制面成了转发路径上的强依赖，`key_auth_timeout` 要按控制面的 P99 延迟加余量设置。

## 快速开始（本地）

前置：Go 1.26、能访问 Amazon Bedrock 的 AWS 凭证（本地可通过 `aws sso login` 或环境变量提供）。

```bash
go build -o bin/gateway ./cmd/gateway && go build -o bin/mock-controlplane ./cmd/mock-controlplane

# 1. 起一个控制面替身：三个网关接口 + 调试接口，内置四个 Bedrock 模型的路由
./bin/mock-controlplane -listen :9090 -token mock-token -keys sk-demo-key &

# 2. 起网关，配置里的 ${CP_BASE_URL} / ${CP_GATEWAY_TOKEN} 从环境变量展开
#    样例里 openai / anthropic 的 api_key 是 Secrets Manager 引用，本地没有这些 secret 时先注释掉这两个 provider
CP_BASE_URL=http://127.0.0.1:9090 CP_GATEWAY_TOKEN=mock-token ./bin/gateway -config configs/gateway.example.yaml &

# 3. 打一条
curl -s http://localhost:8080/v1/messages -H 'Authorization: Bearer sk-demo-key' -H 'content-type: application/json' \
  -d '{"model":"claude-sonnet-5","max_tokens":50,"messages":[{"role":"user","content":"hello"}]}'

# 4. 看控制面收到的计量
curl -s http://localhost:9090/debug/usages | python3 -m json.tool

# 5. 全量冒烟：四个模型、三种协议、流式与非流式、负向用例、计量核对
GW=http://localhost:8080 CP=http://localhost:9090 KEY=sk-demo-key scripts/smoke.sh
```

`configs/gateway.example.yaml` 里的 Bedrock provider 指向 ap-northeast-1，模型码用 `global.` 跨区域 inference profile，
换区域改 `region` 和三个 `endpoints` 即可。Bedrock 上的 OpenAI 模型（GPT 系列）对调用方所在地有限制，从不受支持的
地区发起会收到 400 `validation_error`，这时 Claude 正常、GPT 报错是环境问题不是网关问题。

不接 Bedrock、只想本地验证 OpenAI 兼容或 Anthropic 格式的第三方 API 时，不需要 AWS 凭证，用
`configs/local-external.example.yaml` 加 `configs/routes.external.example.json`，步骤见
[docs/operations.md](docs/operations.md) 的「本地验证外接 API 供应商」。

## 日常运维：改动落在哪一层

**加模型只动控制面。**网关自己不认识任何模型名，全靠 `model-routes` 下发的
`modelCode → providerCode + providerModelCode`。在控制面加一条路由，网关最多 `routes_poll_interval`（默认 30 秒）
后生效，不用重启。Bedrock 上新增模型时，`providerModelCode` 填对应的 `global.` inference profile ID。

**接新供应商改网关配置并滚动重启一次。**在 `providers:` 下加一段：认证方式选 `bearer` /
`x-api-key` / `aws_iam` / `none` 之一，填 `endpoints` 和凭证，然后控制面路由就可以引用这个 `providerCode`。
凭证和 SigV4 签名器在启动时初始化，目前没有 provider 热加载，EKS 上改 ConfigMap 后 `kubectl rollout restart` 即可，
两副本滚动无中断。

**需要改代码的只有两种情况**：上游要一种现有四种之外的认证方式（如 HMAC 签名、URL 查询参数带
`api-version`）；或者上游的协议形态不是 OpenAI Chat / Responses / Anthropic Messages 三种之一，或 usage 字段不按
这三家的格式返回。详细步骤见 [docs/operations.md](docs/operations.md)。

## 网关改了什么

| 方向 | 改动 | 原因 |
| --- | --- | --- |
| 请求体 | `model` 替换为路由里的 `providerModelCode`，原样不加工 | 客户端看到的模型名与供应商侧模型码解耦，同一名字可以路由到不同供应商 |
| 请求体（OpenAI chat 且 `stream: true`） | 合并进 `stream_options.include_usage: true` | OpenAI 兼容端点在流式响应中默认不返回 usage |
| 请求头 | 只透传 `Accept`、`anthropic-version`、`anthropic-beta`、`openai-beta`；客户端鉴权头一律剥掉，`User-Agent` 换成网关自己的 | 客户 key 不能泄露给供应商，供应商凭证也不能被客户端指定 |
| 请求头（Anthropic） | 缺 `anthropic-version` 时补 `2023-06-01` | Bedrock 的 Anthropic 接口要求该头必填 |
| 响应头 | 透传 `Content-Type`、`Cache-Control`；新增 `X-Request-Id`（网关生成，也是计量的 `request_id`）和 `X-Upstream-Request-Id`（上游的 `x-request-id` / `request-id` / `x-amzn-requestid`） | 客户端拿 `X-Request-Id` 能在网关日志和控制面计量里找到同一条记录 |

请求体的嵌套内容按原始字节携带，不重新编码，所以 `messages`、`tools`、`input` 等字段里的任何内容都不会被改写或丢字段。

## 部署概览

镜像用 [ko](https://ko.build) 构建（无需 Docker daemon，也提供了 `deploy/Dockerfile`，两个基础镜像都按 digest 钉死），基础镜像是 Amazon ECR Public 的
`amazonlinux:2023-minimal`，无 shell，非 root 运行。Dockerfile 里的 `HEALTHCHECK` 用网关自带的 `-healthcheck` 参数探 `/healthz`（镜像没有 curl），
Kubernetes 不读这条，用清单里的 readiness / liveness probe。清单里的镜像写成 `tag@digest`，digest 取 ko 输出，发新版两处一起改。EKS 上用 IRSA 给 Pod 授权调 Bedrock，不需要任何静态 AWS 密钥。
网关自己的密钥（控制面 token、第三方供应商 api_key）支持两种存法：明文（`${ENV}` 从 K8s Secret 注入）或
AWS Secrets Manager（配置里写 `secretsmanager://<secret>#<键>`，启动时经 IRSA 读取，Pod 不注入任何密钥环境变量）。
PoC 环境采用后者。

```bash
export KO_DOCKER_REPO=<account>.dkr.ecr.<region>.amazonaws.com/sample-llm-gateway
ko build --base-import-paths --platform=linux/amd64,linux/arm64 --tags=v0.6.1 ./cmd/gateway ./cmd/mock-controlplane

eksctl create cluster -f deploy/eks/cluster.yaml
aws iam create-policy --policy-name llm-gateway-poc-bedrock-invoke --policy-document file://deploy/eks/bedrock-invoke-policy.json
aws iam create-policy --policy-name llm-gateway-poc-secrets-read --policy-document file://deploy/eks/secrets-read-policy.json
eksctl create iamserviceaccount --cluster llm-gateway-poc --region ap-northeast-1 \
  --namespace llm-gateway --name llm-gateway --role-name llm-gateway-poc-bedrock-invoke \
  --attach-policy-arn arn:aws:iam::<account>:policy/llm-gateway-poc-bedrock-invoke \
  --attach-policy-arn arn:aws:iam::<account>:policy/llm-gateway-poc-secrets-read --approve
# 网关的密钥进 Secrets Manager（JSON，配置里按键引用）
aws secretsmanager create-secret --region ap-northeast-1 --name llm-gateway/poc --secret-string '{"CP_GATEWAY_TOKEN":"<token>"}'
# 这个 K8s Secret 只给 mock 控制面用（它要校验同一个 token 并持有测试 key），正式环境不需要
kubectl create secret generic llm-gateway-secrets -n llm-gateway \
  --from-literal=CP_GATEWAY_TOKEN=<token> --from-literal=MOCK_API_KEYS=sk-demo-key
kubectl apply -f deploy/k8s/
```

`deploy/k8s/` 的文件按序号应用：命名空间、ServiceAccount、私网节点池与 NetworkPolicy（`05-network.yaml`，可选）、
mock 控制面（正式环境删掉，把 ConfigMap 里的 `control_plane.base_url` 指向真实控制面）、网关。IAM 策略只给
`bedrock:InvokeModel` / `InvokeModelWithResponseStream`，资源限定 foundation model、inference profile 和 Responses API
鉴权用的 `project/default`；跨账号场景再加一条 `sts:AssumeRole`；密钥走 Secrets Manager 时另加一条限定 `llm-gateway/*`
前缀的 `secretsmanager:GetSecretValue`。

网关 Deployment 两副本，带节点反亲和（两副本必须在不同节点，硬约束）、跨可用区分布（软约束）和
PodDisruptionBudget（`minAvailable: 1`），节点维护或 Auto Mode 整合节点时始终有一个副本在服务。

完整的 IRSA、私有子网、VPC Endpoint、跨账号步骤见 [docs/private-networking.md](docs/private-networking.md)。

## 可观测性一览

- `GET /healthz` 存活；`GET /readyz` 路由快照加载完成后才就绪。
- `GET /metrics` Prometheus 文本格式，指标全部以 `llmgw_` 开头。
- stdout 一行一条 JSON 日志，每个请求一条 `request completed`，带 `request_id`、subject、provider、状态码、时延、token 数。
- `GET /v1/models` 按 OpenAI list 格式返回当前路由快照里的模型（不鉴权）。

指标表、日志字段、排障手册见 [docs/operations.md](docs/operations.md)。

## 测试与 CI

- `go test ./...`：协议解析（三种协议的 usage 提取、流式解析）、路由（优先级分层、权重）、转发主流程
  （用 httptest 伪造控制面和上游：改写、流式 usage、`include_usage` 注入、5xx 切换、拒绝格式、负向路径）。
- `scripts/smoke.sh`：端到端冒烟，需要真实 Bedrock。默认测 Claude Sonnet 5 / Opus 5 的 messages、GPT-5.6 Sol / Luna
  的 chat 与 responses，各跑流式与非流式，再跑负向用例，最后核对控制面是否为每次成功调用收到了 token。
  被测模型列表可用 `CLAUDE_MODELS` / `GPT_MODELS` 环境变量覆盖。
- `loadtest/`：k6 压测脚本与 Job，集群内直接打网关 Service。用法与 2026-09-03 的 PoC 结果（3M TPM、约 100 在途连接）
  见 [loadtest/README.md](loadtest/README.md)。
- CI：`.github/workflows/ci.yml`（GitHub Actions）与 `.gitlab-ci.yml` 同一套检查，都跑 `go vet`、`go test -race`、`govulncheck`、构建；
  GitLab 侧另加自带的 SAST（semgrep）与 Secret Detection。semgrep 误报用行内 `nosemgrep` 注释压（k6 脚本的 Math.random 非安全用途、
  网关按配置地址发请求的 G107）。改协议层必跑单测，改转发主流程必跑冒烟，单测过不等于链路通。
- 部署清单里的账号号（`123456789012` / `111122223333`）、VPC / 子网 / 安全组 ID、VPC Endpoint 域名、出口 IP 都是占位，
  按 [docs/private-networking.md](docs/private-networking.md) 换成自己环境的值。

## 已知限制（第一阶段）

- 不做协议转换。客户端给 Bedrock 上的 GPT 模型发 `max_tokens` 会收到模型自己的 400（它要的是 `max_completion_tokens`）。
- 计量在内存队列里。进程崩溃会丢掉尚未发出的记录，依赖控制面的对账任务兜底。
- 故障转移只发生在首个响应字节之前。流一旦开始，错误原样透传。
- 流被 `request_timeout` 截断时，网关不会往流里插入自己的错误事件（不做协议转换），客户端收到的是一次正常结束的连接。
  接入方要靠协议自身的结束标记判断完整性：Anthropic 看 `message_stop`，OpenAI chat 看 `[DONE]`，Responses 看 `response.completed`。
- 没跑完的流不计费：客户端中途断开上报 499，被 `request_timeout` 切断上报 504，上游连接中途断掉上报 502，token 全 0；
  供应商按实际已生成的 token 收费，这部分差额由运营方承担（客户确认的口径）。
- provider 配置改动和密钥轮转都需要滚动重启（密钥只在启动时读一次），路由改动不需要。
- 路由里引用了配置中不存在的 provider，或该 provider 不支持进入的协议时，这个候选会被跳过；若所有候选都因此不可用，
  客户端收到 502 而不是 400，日志里有 `route references unconfigured provider` / `provider lacks endpoint for protocol`。
- Bedrock 上的 OpenAI 模型有调用方所在地限制，网关必须部署在受支持的区域。

## 目录结构

```
cmd/gateway/              网关入口：加载配置、初始化 provider、拉路由、起 HTTP 服务、优雅退出
cmd/mock-controlplane/    控制面替身：三个网关接口 + /debug/usages + /debug/routes，本地与集群冒烟用
internal/config/          YAML 配置结构、${ENV} 展开、默认值、校验
internal/controlplane/    控制面 HTTP 客户端：model-routes（ETag）、key-auth、usage/report（ApiResult 解包）
internal/router/          路由快照（原子替换）、候选排序、后台轮询
internal/provider/        供应商注册表；bearer / x-api-key / none / aws_iam（SigV4，支持 AssumeRole）
internal/secrets/         密钥解析：secretsmanager:// 引用 → AWS Secrets Manager（启动时一次，按 secret 缓存）
internal/protocol/        协议识别、请求体改写、三种协议的 usage 解析、SSE 流解析、错误格式
internal/proxy/           转发主流程：鉴权闸门、路由、故障转移、流式 tee、计量上报、/v1/models
internal/metering/        计量队列：有界 channel、worker、指数退避、满队列丢弃计数
internal/observability/   JSON 日志、Prometheus 指标
configs/                  gateway.example.yaml（全字段注释样例）
deploy/eks/               eksctl 集群规格、Bedrock 调用与 Secrets Manager 读取的 IAM 策略
deploy/k8s/               命名空间、ServiceAccount（IRSA）、私网节点池与 NetworkPolicy、mock 控制面、网关
scripts/smoke.sh          端到端冒烟
loadtest/                 k6 压测脚本、Job 与 PoC 结果
```

## Security

安全问题不要提 issue，按 [CONTRIBUTING](CONTRIBUTING.md#security-issue-notifications) 里的方式报告。

## License

本项目使用 MIT-0 License，见 [LICENSE](LICENSE)。

---

以下为 AWS 样例代码声明，按原文保留。

```
###################
This sample code is provided to you as AWS Content under the AWS Customer Agreement,
or the relevant written agreement between you and AWS (whichever applies). You should
not use this sample code in your production accounts, or on production, or other
critical data. You are responsible for testing, securing, and optimizing the sample
code as appropriate for production grade use based on your specific quality control
practices and standards.
####################
```
