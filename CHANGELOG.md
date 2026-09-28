# 更新记录

## 2026-09-28（未发新镜像）

### 路由样例与冒烟覆盖 GPT-6 三档

- mock 控制面内置路由与 `deploy/k8s/20-mock-controlplane.yaml` 新增 `gpt-6-astra` / `gpt-6-sol` / `gpt-6-luna`（东京 `bedrock`）及对应 `-us` 跨账号路由，
  `providerModelCode` 用 `global.openai.gpt-6-*`；路由版本改为 `poc-xacct-2`。网关代码对模型码零加工，本次无代码行为变化。
- `scripts/smoke.sh` 默认 `GPT_MODELS` 扩为 gpt-5.6 两档 + gpt-6 三档；docs/private-networking.md 跨账号冒烟示例同步。
- 东京 PoC 实测：本账号 7 模型 29 项全过、计量 25 条无缺失；跨账号 GPT-6 Chat 流式/非流式全 200，`/v1/responses` 跨账号仍 401
  （对端角色缺 `project/default`，与 gpt-5.6 一致）；`bedrock_invoke` 路径 `global.openai.gpt-6-astra` 透传与 Responses→Chat 转换均 200。

## 2026-09-23（未发新镜像）

### 新增：经 SDK InvokeModel 调 Bedrock（`endpoints.bedrock_invoke`）

- `aws_iam` provider 可配 `endpoints.bedrock_invoke`（bedrock-runtime 主机，不带路径）。目标协议是 `anthropic` / `openai_chat` 且该 provider
  没有对应原生端点时，网关用 AWS SDK 的 `InvokeModel` / `InvokeModelWithResponseStream` 调用，与基于 AWS SDK 的网关同一条路径；原生端点优先，已有配置不受影响。
- body 仍是目标协议原生 JSON：删 `model` / `stream`，Claude 补 `anthropic_version: bedrock-2023-05-31` 并把 `anthropic-beta` 头并入 `anthropic_beta`，
  GPT 非流式去掉 `stream_options`。流式 event-stream 还原成原生端点的 SSE，透传、协议转换、计量对这条路径无感知。
- Bedrock 状态码保留（429 / 5xx 照常故障转移），错误 body 按目标协议格式、`message` 带 SDK 原始错误文本；流中途异常先发该协议的 error 事件再按上游中断计量；
  连接失败走故障转移。SDK 自带重试关闭，复用网关上游 HTTP 客户端与 `aws_iam` 凭证链。
- 新依赖 `github.com/aws/aws-sdk-go-v2/service/bedrockruntime`。
- 文档：configuration.md 新增「经 SDK InvokeModel 调 Bedrock」；operations.md 新增接入要点（含引导 SDK 网关接 GPT 的核对清单）与
  `deserialization failed ... invalid character '<'` 排查条目；README 新增 Bedrock InvokeModel transport 节。

### 合入 GitHub #2：Chat Completions / Responses / Messages 协议转换

- 路由候选可选字段 `providerProtocol`（`openai_chat` / `openai_responses` / `anthropic`），与入站协议不同时网关在三种协议间双向转换请求、
  非流式响应与 SSE 流，六个方向全部支持；不填该字段的路由行为不变（纯透传）。已用 Claude Code → GPT、Codex → Claude 等真实客户端在 Bedrock 上验收。
- 新增配置 `server.default_max_tokens`（默认 8192）：转换到 Anthropic 且客户端未带 max_tokens 时补入。
- 新增指标 `llmgw_translations_total{inbound,target,result}`、`llmgw_translation_defaults_total{field}`。
- 转换路由上：上游错误按客户端协议重渲染（状态码不变）；`previous_response_id` 返 400；流中途转换失败返客户端协议的 error 事件并按中断计量（新增 truncated 原因 `translation_error`）。
- mock 控制面路由结构体增加 `providerProtocol`；新增开发工具 `tools/recordproxy`（录制真实流量作 golden fixture，不进镜像）。
- 文档：README 新增 Protocol translation 节并撤销「不做协议转换」声明；新增 docs/protocol-translation-design.md、docs/protocol-translation-report.md。

### 合入 GitHub #1：代码审查修复（第二批）

- 负数 token（部分 OpenAI 兼容服务用 -1 表示未知）统一置 0，避免 Prometheus Counter.Add panic 丢计量。
- 控制面客户端：不跟随重定向（防 token 与 apiKey 被带到别的主机）；model-routes 缺 `models` 键时报错而不是当空表；429 / 408 按可重试处理；
  key-auth 只解码网关用到的字段。
- Bedrock 凭证缓存提前 5 分钟（10% 抖动）刷新，避免临近过期签名导致 403 `ExpiredTokenException`。

## 2026-09-10（不发新镜像）

- 清零 GitHub CodeQL 报出的 5 条 `go/clear-text-logging`（high）。都是误报路径，但顺手把两处消掉：
  - `internal/secrets`：`ParseRef` 解析 `secretsmanager://` 引用失败时不再把原始字段值写进错误文本。CodeQL 追的是
    `providers.*.api_key` 流进 `secret resolution failed` 日志这条路径；实际只有 `secretsmanager://` 开头的引用才会走到 `ParseRef`，
    带出来的不是明文密钥，但去掉后一样能定位（错误仍带字段名，如 `providers.openai.api_key: empty secret id`）。
  - mock 控制面：日志、`keyCode`、存下的 `api_key` 一直只带 key 尾 4 位，这次把做截断的函数从 `tail` 改名 `maskKey`，
    让扫描器识别为脱敏（CodeQL 按函数名判断，`mask` / `redact` 一类才算 barrier）。行为不变。
- 代码逻辑、配置字段、接口、指标名均无变化，镜像仍为 `v0.6.1`。

## 2026-09-09（不发新镜像）

- 仓库同步发布到 GitHub `aws-samples/sample-llm-gateway`，补 `LICENSE`（MIT-0）、`CONTRIBUTING.md`、`CODE_OF_CONDUCT.md`，README 加 Security / License 段与英文简介。
- 新增 `.github/workflows/ci.yml`（GitHub Actions），与 `.gitlab-ci.yml` 同一套 `go vet` / `go test -race` / `govulncheck` / 构建；action 按 commit SHA 钉死。
- 部署清单与文档里的真实环境值（账号号、VPC / 子网 / 安全组 ID、跨账号角色名、VPC Endpoint 域名、集群出口 IP、eksctl 生成的节点角色名）全部换成占位，
  `deploy/eks/cluster.yaml`、`deploy/k8s/05-network.yaml`、`10-serviceaccount.yaml`、`30-gateway.yaml` 与 IAM 策略文件部署前要按自己环境填。
- 代码、配置字段、接口、日志与指标名均无变化，镜像仍为 `v0.6.1`。

## v0.6.1（2026-09-08）

镜像 tag `v0.6.1`（网关与 mock 控制面）。本版本不改转发行为，内容是安全扫描（checkov / semgrep）报出的部署清单与容器加固项，共 48 条，全部清零。

### 部署清单加固（deploy/k8s、loadtest/job.yaml）

- mock 控制面与 k6 压测 Job 补齐与网关一致的安全上下文：非 root（UID 65532 / 12345）、`seccompProfile: RuntimeDefault`、
  `allowPrivilegeEscalation: false`、只读根文件系统、`capabilities drop ALL`；补 readiness / liveness 探针、整数核 CPU limit（小数核会被 CFS 限流）、`imagePullPolicy: Always`。
- 三个工作负载都关闭 `automountServiceAccountToken`（均不访问 K8s API；网关的 IRSA 凭证由 EKS webhook 单独注入，不受影响）。
- 网关补 CPU limit（`2`，request 的 20 倍，只防单副本失控）。
- 镜像一律 `tag@digest` 双写，包括 `grafana/k6`；升级时两处一起改，digest 取 ko 输出。
- mock 控制面与 k6 各加一条 NetworkPolicy：mock 只收集群内 Pod 与节点地址的 9090 入向、不外联；k6 只出向 VPC 内地址与 Service CIDR。
- mock 控制面的 token 与合法 key 改为挂载 Secret 文件读取（新增 `-token-file` / `-keys-file` 参数），不再经环境变量注入。

### 镜像

- `deploy/Dockerfile` 两个基础镜像按 digest 钉死；新增 `HEALTHCHECK`，使用网关新增的 `-healthcheck` 参数探 `/healthz`（镜像内无 curl）。
  Kubernetes 部署不读这条，仍用清单里的探针。

### 代码

- 网关新增 `-healthcheck` 命令行参数：按 `-config` 里的监听端口探本机 `/healthz`，200 返回 0，否则返回 1。
- semgrep 误报以行内 `nosemgrep` 注释标注理由：转发上游 JSON 正文的 `w.Write`、mock 调试接口的明文回显、加权选路用的 `math/rand`、
  k6 脚本读响应头被污点规则误判。k6 脚本里名为 `prompt` 的函数改名 `buildPrompt`，避免与浏览器 `prompt()` 规则冲突。

### 升级注意

- 集群升级到本版本时 mock 控制面清单要求 Secret `llm-gateway-secrets` 含 `CP_GATEWAY_TOKEN` 与 `MOCK_API_KEYS` 两个键（原来也是这两个键，只是改成挂文件）。
- 其余配置字段、接口、日志与指标名不变。

## v0.6.0（2026-09-08）

镜像 tag `v0.6.0`。相对 2026-09-03 交付版本（v0.5.1）的变化如下。

### 修复

- **计量队列关停竞态。**原实现在关停时关闭数据 channel，若某个请求在 HTTP 排空期结束后才完成，且此时计量上报正在重试，最后的计量入队会触发 `send on closed channel` panic。进程不会退出（`net/http` 会 recover），但该连接被直接关闭，客户端收到 EOF，且这条请求的用量报告丢失。现改为独立的关停信号加 worker 排空，数据 channel 不再关闭，关停后的入队返回 false 且不计入 dropped。该问题已在 PoC 集群复现并验证修复（`internal/metering/queue.go`）。
- **非流式响应超限不再静默截断。**上游非流式响应体超过 64 MiB 上限时返回 502 `upstream_body_too_large`，日志 `upstream response body too large, rejecting`，计量按 502、token 为 0 记录。此前会把截断后的正文连同上游 200 一起返回，客户端拿到的是不完整的 JSON。

### 安全加固

- `control_plane.base_url` 必须是绝对 http(s) URL。使用明文 http 需显式设置 `control_plane.allow_insecure: true`（默认 `false`），启动时打印告警 `control_plane.base_url uses plaintext http`。控制面 token 与客户 api key 每次调用都随行，生产接真控制面请用 https。
- `secrets.endpoint` 若填写必须是 https URL，没有明文逃生开关。

### 新增

- 本地验证外接 API 供应商（OpenAI 兼容或 Anthropic 格式的第三方 API）的最小配置与路由样例：`configs/local-external.example.yaml`、`configs/routes.external.example.json`。不需要 AWS 凭证，步骤见 [docs/operations.md](docs/operations.md)「本地验证外接 API 供应商」。
- `scripts/smoke.sh` 支持把 `CLAUDE_MODELS` 或 `GPT_MODELS` 显式设为空串跳过对应协议，新增 `RESPONSES_MODELS` 变量单独控制 Responses API 用例。

### 升级注意

- 现有配置若 `control_plane.base_url` 为 http（例如集群内的 mock 控制面），升级到本版本必须加 `allow_insecure: true`，否则启动失败，报错 `control_plane.base_url uses plaintext http`。`deploy/k8s/30-gateway.yaml` 与 `configs/gateway.example.yaml` 已带该字段。
- 其余配置字段、接口、日志与指标名不变，可直接替换镜像滚动升级。

### 测试与扫描

- 新增 `internal/config/config_test.go`、`internal/metering/queue_test.go`（含并发关停不 panic 用例）与非流式超限用例，`go test -race ./...` 通过。
- CI 的 SAST（semgrep）、Secret Detection、govulncheck 均为零发现。

## v0.5.1（2026-09-03）

首次交付版本。功能范围见 [README.md](README.md)，故障注入与并发测试结果见 [docs/robustness-report.md](docs/robustness-report.md)。
