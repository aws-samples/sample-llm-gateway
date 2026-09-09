# 配置参考

网关只有一个 YAML 配置文件，启动参数 `-config <path>`。文件里的 `${VAR}` 在解析 YAML 之前从环境变量展开，
未设置的变量展开为空串（随后会被校验拦住，例如 `control_plane.token` 为空会启动失败）。完整样例见
[configs/gateway.example.yaml](../configs/gateway.example.yaml)。

时长字段用 Go duration 写法：`2s`、`500ms`、`5m`、`1h30m`。

密钥类字段（`control_plane.token`、`providers.*.api_key`、`providers.*.external_id`）有两种存法：明文（直接写值，或
`${VAR}` 从环境变量注入），或写成 `secretsmanager://` 引用、启动时从 AWS Secrets Manager 读取。两种写法可以逐字段混用，
规则见 [secrets](#secrets)。

## server

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `listen` | `:8080` | 监听地址 |
| `read_timeout` | `60s` | 读完一个请求的请求头与请求体的时限。请求体读完之前 `request_timeout` 还没开始计时，这个值防止慢发 body 的连接无限期占用协程和内存。不约束响应，流式输出不受影响 |
| `shutdown_timeout` | `280s` | 收到 SIGTERM 后等在途请求结束的时间。要配合 Pod 的 `terminationGracePeriodSeconds`（清单里 300 秒）：先 5 秒摘流量，再等这个时长，最后 10 秒刷计量，三段之和不能超过宽限期 |
| `request_timeout` | `10m` | 单个请求从收到到结束的硬上限，含流式全程。超过后上游连接被切断，客户端收到截断的流 |
| `upstream_connect_timeout` | `5s` | 到上游的 TCP 连接超时。超时算「首包前失败」，会切下一个候选 |
| `upstream_response_header_timeout` | `5m` | 等上游返回响应头的时间。推理模型首字可能很慢，不宜设置过小。超时同样会切候选 |
| `max_request_body_bytes` | `33554432`（32 MiB） | 请求体上限，超过返回 413。请求体在 key-auth 之前整体读入内存（鉴权需要 body 里的 model 字段），所以 Pod 内存 limit 至少要容得下「并发上传数 × 这个值」，清单里 1Gi 约对应 30 个满体积并发。没有图片、PDF 这类大附件需求时可以降到 4 到 8 MiB |
| `max_failover_attempts` | `3` | 一次请求最多尝试的候选供应商数。路由里候选更多也只取前 N 个 |
| `log_level` | `info` | `debug` / `info` / `warn` / `error` |

## control_plane

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `base_url` | 必填 | 控制面根地址，网关在其后拼 `/admin/gateway/...`。必须是 http(s) URL；为 http 时需显式打开 `allow_insecure`（token 与客户 key 每次调用随行，明文会泄露） |
| `allow_insecure` | `false` | 允许 `base_url` 使用明文 http（如集群内 mock 控制面）。生产接真控制面请用 https 并保持 `false` |
| `token` | 必填 | 三个网关接口统一携带的 token。明文、`${VAR}` 或 `secretsmanager://` 引用（见 [secrets](#secrets)） |
| `token_header` | `X-HIGRESS-Token` | 携带 token 的头名 |
| `key_auth_timeout` | `2s` | 单次 key-auth 调用超时。超时视为控制面不可用，请求返回 503（fail-closed）。这是客户端感知的鉴权延迟上限，建议按控制面 P99 加余量设置 |
| `report_timeout` | `5s` | 单次 usage/report 调用超时。超时会重试，不影响客户端 |
| `routes_poll_interval` | `30s` | 轮询 model-routes 的间隔，带 `If-None-Match`，未变化时控制面返回 304 |

启动时网关会先同步一次路由，最多重试 60 秒，仍失败则退出，由编排系统重启。运行中轮询失败只记日志，沿用上一份快照。

## metering

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `queue_size` | `10000` | 内存队列容量。满了之后新记录直接丢弃并计入 `llmgw_metering_reports_total{result="dropped"}` |
| `workers` | `4` | 并发上报的 worker 数 |
| `max_retries` | `5` | 单条记录最多重试次数。退避从 200ms 起翻倍，封顶 5s。只对网络错误、超时、5xx 重试；控制面返回业务拒绝（非 `00000`）不重试 |

`queue_size × 单条记录约 400 字节` 就是队列的内存上界，默认配置约 4 MiB。优雅退出时在途请求排空之后会再用最多 10 秒
刷队列（`cmd/gateway/main.go` 里的常量，与 `server.shutdown_timeout` 各自独立计时），刷不完的记录丢弃并计入 dropped。

## secrets

密钥字段写成 `secretsmanager://<secret-id>[#<json-key>]` 时，网关启动时用 AWS 默认凭证链（EKS 上即 IRSA）调
`GetSecretValue` 取 `AWSCURRENT` 版本，把取到的值替换进配置，之后的流程与明文完全相同。写明文的字段不受影响，
配置里没有任何引用时不会碰 Secrets Manager，也不需要下面的权限。

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `region` | 依次取 `AWS_REGION`、`AWS_DEFAULT_REGION` | Secrets Manager 所在区域。EKS 上 IRSA 会注入 `AWS_REGION`，一般可省略；都取不到时启动失败 |
| `endpoint` | 空 | 可选。自定义端点 URL，只在 VPC Endpoint 未开私有 DNS 或经 peering 访问别的 VPC 时填 VPCE 专属域名。**填了必须是 https**（防止密钥走明文），无明文逃生开关 |
| `timeout` | `10s` | 单次 `GetSecretValue` 超时 |

引用写法：

| 写法 | 含义 |
| --- | --- |
| `secretsmanager://llm-gateway/poc` | secret 名，整个 `SecretString` 就是值 |
| `secretsmanager://llm-gateway/poc#CP_GATEWAY_TOKEN` | `SecretString` 必须是 JSON 对象，取其中 `CP_GATEWAY_TOKEN` 键的字符串值 |
| `secretsmanager://arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:name-AbCdEf#key` | 用完整 ARN，跨账号 secret 只能这样写 |

- 同一个 secret 被多个字段引用只取一次，推荐把一套环境的密钥放进一个 JSON secret，按键引用。
- 取到的值会去掉首尾空白（避免从文件导入 secret 时带上的换行）。取不到、键不存在、值不是字符串、值为空、
  secret 只有二进制内容，都会启动失败，日志 `secret resolution failed`。
- 每个引用解析成功打一行 `secret resolved`，带字段名、secret id、JSON 键和 `version_id`，不打印值。
- 只在启动时读取一次。轮转 secret 后 `kubectl rollout restart deployment/llm-gateway -n llm-gateway`，新副本读新值、
  `/readyz` 通过后才接流量。控制面 token 轮转要先让控制面同时接受新旧两个值，再重启网关，再废旧值。
- IAM：IRSA 角色需要对被引用的 secret 有 `secretsmanager:GetSecretValue`，样例 [deploy/eks/secrets-read-policy.json](../deploy/eks/secrets-read-policy.json)
  限定到 `llm-gateway/*` 前缀。secret 用自定义 KMS 密钥加密时还要 `kms:Decrypt`。
- 全私网部署要在网关 VPC 加 `com.amazonaws.<region>.secretsmanager` 接口端点，见 [private-networking.md](private-networking.md)。

## providers

`providers` 是一个 map，key 就是控制面路由里引用的 `providerCode`。每个 provider 声明自己支持哪些协议、用什么方式鉴权。

| 字段 | 适用 | 说明 |
| --- | --- | --- |
| `auth` | 全部 | `bearer`（`Authorization: Bearer <api_key>`）、`x-api-key`（`x-api-key: <api_key>`）、`aws_iam`（SigV4）、`none`。默认 `bearer` |
| `api_key` | bearer / x-api-key | 必填。明文、`${SOME_ENV}` 或 `secretsmanager://` 引用（见 [secrets](#secrets)） |
| `endpoints` | 全部 | 至少一项。key 只能是 `openai_chat`、`openai_responses`、`anthropic`，value 是 base URL，网关在其后分别拼 `/chat/completions`、`/responses`、`/messages`。没声明的协议不会被路由到这个 provider |
| `region` | aws_iam | 必填。SigV4 签名区域，即 Bedrock 端点所在区域 |
| `sts_region` | aws_iam | 可选。STS 调用（IRSA 换凭证、AssumeRole）走的区域，默认依次取 `AWS_REGION`、`AWS_DEFAULT_REGION`、`region`。网关所在区域与端点区域不同且要求全私网时必须显式填网关所在区域 |
| `role_arn` | aws_iam | 可选。先 AssumeRole 到这个角色再签名，用于 Bedrock 在另一个账号的场景。会话名固定为 `llm-gateway-<providerCode>` |
| `external_id` | aws_iam | 可选。AssumeRole 的 ExternalId。同样支持 `secretsmanager://` 引用 |

`aws_iam` 的基础凭证来自 AWS SDK 默认凭证链，优先级依次是环境变量、共享配置文件、Web Identity（EKS IRSA 注入的
`AWS_ROLE_ARN` + `AWS_WEB_IDENTITY_TOKEN_FILE`）、容器凭证、实例元数据。启动时每个 `aws_iam` provider 会解析一次凭证，
解析失败直接退出；成功后输出一行 `aws identity resolved` 日志，带解析出的身份 ARN，用于核对跨账号身份是否正确。
凭证由 SDK 自动缓存和刷新，AssumeRole 的临时凭证也一样。

### 样例

```yaml
providers:
  # Amazon Bedrock，本账号，网关所在区域
  bedrock:
    auth: aws_iam
    region: ap-northeast-1
    endpoints:
      openai_chat:      "https://bedrock-runtime.ap-northeast-1.amazonaws.com/openai/v1"
      openai_responses: "https://bedrock-runtime.ap-northeast-1.amazonaws.com/openai/v1"
      anthropic:        "https://bedrock-runtime.ap-northeast-1.amazonaws.com/anthropic/v1"

  # Amazon Bedrock，另一个账号、另一个区域，经 VPC peering 访问对端 VPC Endpoint
  bedrock-usw2-xacct:
    auth: aws_iam
    region: us-west-2
    sts_region: ap-northeast-1
    role_arn: arn:aws:iam::<bedrock-account>:role/<cross-account-role>
    endpoints:
      anthropic:        "https://vpce-xxxx-yyyy.bedrock-runtime.us-west-2.vpce.amazonaws.com/anthropic/v1"
      openai_chat:      "https://vpce-xxxx-yyyy.bedrock-runtime.us-west-2.vpce.amazonaws.com/openai/v1"
      openai_responses: "https://vpce-xxxx-yyyy.bedrock-runtime.us-west-2.vpce.amazonaws.com/openai/v1"

  # api_key 存在 Secrets Manager 的 JSON secret 里，按键引用
  openai:
    auth: bearer
    api_key: "secretsmanager://llm-gateway/prod#OPENAI_API_KEY"
    endpoints:
      openai_chat:      "https://api.openai.com/v1"
      openai_responses: "https://api.openai.com/v1"

  # 整个 SecretString 就是 api_key
  anthropic:
    auth: x-api-key
    api_key: "secretsmanager://llm-gateway/anthropic-api-key"
    endpoints:
      anthropic:        "https://api.anthropic.com/v1"

  # 明文方式：从环境变量注入。只提供 OpenAI 兼容 chat 的厂商，路由把它配给 /v1/responses 或 /v1/messages 的请求会被跳过
  moonshot:
    auth: bearer
    api_key: "${MOONSHOT_API_KEY}"
    endpoints:
      openai_chat:      "https://api.moonshot.ai/v1"
```

### Bedrock 模型码的填写规则

网关对 `providerModelCode` 不做任何加工：控制面配置的字符串原样写入请求体 `model` 字段，计量也上报同一个字符串。
Bedrock 上请填跨区域 inference profile ID，例如 `global.anthropic.claude-sonnet-5`、`global.openai.gpt-5.6-sol`。
`global.` profile 可以从任何支持的区域端点调用，路由到不同区域的 provider 时不需要换模型码。

## 校验规则

启动时会检查并在不满足时退出：

- `control_plane.base_url` 非空且为 http(s) URL（为 http 时需 `allow_insecure: true`）、`control_plane.token` 非空
- `secrets.endpoint` 若填写必须是 https URL
- 至少一个 provider
- `bearer` / `x-api-key` 必须有 `api_key`
- `aws_iam` 必须有 `region`；`role_arn` 若填必须以 `arn:aws` 开头
- 每个 provider 至少一个 endpoint，endpoint 名只能是三个协议名之一，URL 必须带 scheme 和 host
- `aws_iam` provider 的凭证必须能解析（IRSA 配置不完整时在启动阶段即失败，而不是等到第一个请求）
- `secretsmanager://` 引用必须能取到、值非空（在 provider 初始化之前解析，失败即退出）

## Kubernetes 中的配置放置方式

配置文件放 ConfigMap。密钥两种放法，参考 [deploy/k8s/30-gateway.yaml](../deploy/k8s/30-gateway.yaml)：

- **Secrets Manager**（PoC 环境采用）：密钥字段写 `secretsmanager://` 引用，Pod 不需要注入任何密钥环境变量，
  也不需要 K8s Secret；靠 IRSA 角色的 `GetSecretValue` 权限读取。
- **明文**：密钥字段写 `${VAR}`，Deployment 的 `env` 里用 `valueFrom.secretKeyRef` 从 K8s Secret 注入。

改 ConfigMap 或轮转 secret 后都需要 `kubectl rollout restart deployment/llm-gateway` 才会生效，因为密钥解析和 provider
初始化都在启动时进行。
