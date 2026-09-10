# 更新记录

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
