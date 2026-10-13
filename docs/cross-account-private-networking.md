# 跨账号、无公网出口部署：机制说明与实测

本文是 [private-networking.md](private-networking.md) 的延伸：在两个 AWS 账号上搭建一套**完全没有公网出口**的参考环境——网关在账号 A 的东京
VPC，Bedrock 在账号 B 的 us-west-2 VPC，两 VPC 以 peering 互通——逐项实测网关到达每个 AWS 服务的路径、跨账号凭证的获取过程，以及
透传原生协议端点与经 AWS SDK `InvokeModel` 调用这两种传输方式的差异。每个结论都标注了对应的实验编号，建环境与实验脚本在
`deploy/xacct-lab/`，可重复执行。

文中出现的 IP、VPC Endpoint 域名、资源 ID 均为占位符；按脚本重建后以实际输出为准。

在这一组合下，本文复现的部署失败根因都不是某个值写错，而是三件事没有对齐——未对齐的表现是超时或 403，且报错指向的常常不是真正出问题的那一跳：

1. IRSA 跨账号鉴权是**两跳** STS 调用，其中第一跳的 STS client 由 SDK 内部创建，应用代码不直接持有它（§3）；
2. VPC Endpoint 的**私有 DNS 只在该 Endpoint 所在的 VPC 内生效**，不跨 VPC、不跨 peering（§2）；
3. AWS SDK 默认用**同一个 region** 决定"连哪个地址"和"签名用哪个区域"，而跨区域时二者需要的值不同（§3.4）。

## 1. 参考环境

### 1.1 拓扑

```
账号 A · 东京 ap-northeast-1 · VPC 10.2.0.0/16 · 无 IGW / 无 NAT           账号 B · us-west-2 · VPC 10.1.0.0/16 · 无 IGW / 无 NAT
┌──────────────────────────────────────────────────────────┐     ┌──────────────────────────────────────────────┐
│ EKS Auto Mode（仅私有子网 10.2.1/24 ·1a、10.2.2/24 ·1c）     │     │ 子网 10.1.1/24 ·2a、10.1.2/24 ·2b              │
│   网关 Pod ×2（IRSA 角色 <gateway-irsa-role>）              │     │                                              │
│   mock 控制面 Pod                                         │     │ bedrock-runtime 接口端点（私有 DNS 开）          │
│                                                          │     │   两个 AZ 各一张网卡 10.1.x.x                   │
│ 接口端点（私有 DNS 全开）：                                 │     │   安全组：仅放 10.2.0.0/16 的 443              │
│   sts、bedrock-runtime、secretsmanager、                   │     │                                              │
│   ecr.api、ecr.dkr、ec2、eks、eks-auth                     │ ◄──►│ IAM 角色 <bedrock-xacct-role>                 │
│ 网关端点：s3                                              │ pcx │   信任：仅 A 的 IRSA 角色                       │
│                                                          │     │   权限：InvokeModel* 于 foundation-model/*、   │
│ 路由：10.1.0.0/16 → pcx                                   │     │         inference-profile/*、project/default  │
│ NetworkPolicy：Pod 出向只放 10/8 的 443/9090/53 与 Service CIDR│   │ 路由：10.2.0.0/16 → pcx；无计算资源             │
└──────────────────────────────────────────────────────────┘     └──────────────────────────────────────────────┘
```

账号 A 对应运行网关的应用账号，账号 B 对应集中持有模型访问权限的账号。B 中不运行任何程序，只有被调用所需的 VPC Endpoint 和 IAM 角色。

### 1.2 一次跨账号请求的完整路径

以 Anthropic Messages 协议请求 `claude-sonnet-5-us`（路由到 B 的 `us.anthropic.claude-sonnet-5`）为例：

| 步 | 动作 | 从哪到哪 | 怎么找到目标 |
|---|---|---|---|
| ① | 鉴权 + 查路由 | 网关 → 控制面（集群内） | Service DNS |
| ② | **STS 跳 1**：IRSA `AssumeRoleWithWebIdentity`，用 Pod 的 ServiceAccount token 换 A 账号凭证 | 网关 → `sts.ap-northeast-1.amazonaws.com` → 东京 STS 端点网卡（10.2.x.x） | 东京 VPC 私有 DNS 把服务公网域名解析成端点内网 IP |
| ③ | **STS 跳 2**：`AssumeRole` 到 B 的 `<bedrock-xacct-role>` | 同上 | 同上 |
| ④ | 用 B 的凭证对请求做 SigV4 签名（区域 us-west-2），POST 到 B 的 Bedrock | 网关 → `vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com` → B 端点网卡（10.1.x.x）→ 经 peering 进 B VPC → Bedrock | **端点专属域名**（从东京 VPC 解析即得 B 的内网 IP，N1）+ 东京路由表 `10.1/16 → pcx` |
| ⑤ | 响应沿原路流回，网关原样转给客户端，旁路抽 usage | — | — |
| ⑥ | 异步上报 token 用量 | 网关 → 控制面 | — |

全程没有数据包离开 AWS 内网。②③ 与 ④ 是最容易误配的两处，§3、§2 分别展开。

### 1.3 脚本

`deploy/xacct-lab/`，账号 ID 与 AWS CLI profile 由环境变量传入（`ACCT_A`、`ACCT_B`、`PROFILE_A`、`PROFILE_B`）；已存在的资源跳过创建，可重复执行；产出写入
`out/`（已 gitignore）。

| 脚本 | 内容 |
|---|---|
| `10-b-bedrock-side.sh` | B：VPC、2 子网、安全组、bedrock-runtime 接口端点、跨账号角色 |
| `20-a-network.sh` | A：VPC、2 子网、8 个接口端点 + s3 网关端点、peering 发起/接受/双向路由 |
| `30-a-eks.sh` | A：EKS Auto Mode、OIDC、IRSA 角色、ECR；收紧 B 角色信任到 IRSA 角色 |
| `40-a-deploy.sh` | ko 构建推送、渲染 k8s 清单（三个 provider：A 原生 / B 原生 / B InvokeModel）、apply |
| `50-smoke.sh` | 10 条路由冒烟 |
| `60-netprobe.sh` / `tools/netprobe` | 私网内解析域名、试连 URL 的探针（无出口 VPC 拉不到公共镜像，故自带） |
| `70-sts-experiments.sh` / `tools/stsprobe` | STS 两跳实验（§3） |
| `75-gateway-no-sts-region.sh` | 网关镜像在缺省 `sts_region` 时的行为（§3.5） |
| `76-gateway-no-private-dns.sh` | 网关镜像在 STS 端点无私有 DNS 时，有无 `AWS_ENDPOINT_URL_STS` 的对照（§3.5） |
| `80-transport-compare.sh` | 透传 vs InvokeModel 对比（§4） |
| `90-teardown.sh` | 按依赖顺序拆除（默认 dry-run，`CONFIRM=yes` 才删） |

从零到 10 条路由全通约 2 小时（EKS 建集群约 11 分钟，其余主要是等端点 available 与节点拉起）。运行成本按公开价目估算约 $0.5/小时
（EKS、2 个节点、9 个接口端点 × 2 AZ；模型调用另计）。

## 2. 网络：无公网出口下怎么到达每个 AWS 服务

### 2.1 前提

无 IGW/NAT 的 VPC 到不了任何 AWS 服务的公网地址，网关依赖的 STS、Bedrock、Secrets Manager 和节点启动依赖的 ECR、EC2、EKS、S3 都要经 VPC Endpoint 进入。
接口型端点在指定的每个子网放一张 ENI；程序配置里写的是域名，所以能否到达端点取决于 DNS 把域名解析到哪——本节的问题全部出在这一步。
本环境的 9 个接口端点 + 1 个网关端点（§1.1）按 EKS Auto Mode 与网关的依赖建齐，未逐个拆除验证"缺哪个会断什么"，清单依据 EKS 文档与 private-networking.md 的记录。

### 2.2 私有 DNS 与端点专属域名

一个 VPC Endpoint 有两类名字：

| 名字 | 示例 | 谁解析能得到端点内网 IP |
|---|---|---|
| **服务公网域名** | `bedrock-runtime.us-west-2.amazonaws.com` | 全球 DNS 答公网 IP；**仅当**端点所在 VPC 开了"私有 DNS"，**该 VPC 内**的解析器才答内网 IP |
| **端点专属域名** | `vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com` | 登记在全球 DNS，从其他 VPC（本次实测：跨区域、跨账号的东京 VPC）解析亦为内网 IP |

私有 DNS 由挂在端点所在 VPC 上的私有 hosted zone 实现，所以**不跨 VPC、不跨 peering**。东京 VPC 内的网关解析
`bedrock-runtime.us-west-2.amazonaws.com` 问的是东京 VPC 的解析器，B 那条私有 DNS 规则对它不存在。

**实测**（N1：`60-netprobe.sh`，探针在私有节点、与网关同出向策略下运行）：

```
resolve sts.ap-northeast-1.amazonaws.com                 10.2.x.x 10.2.x.x    ← 东京端点，私有 DNS 生效
resolve bedrock-runtime.ap-northeast-1.amazonaws.com     10.2.x.x 10.2.x.x    ← 同上
resolve sts.us-west-2.amazonaws.com                      <公网 IP>            ← 东京没有这个端点
resolve bedrock-runtime.us-west-2.amazonaws.com          <公网 IP> …          ← B 的私有 DNS 管不到东京
resolve vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com   10.1.x.x 10.1.x.x   ← 专属域名，跨 VPC 仍解析为内网 IP

get https://checkip.amazonaws.com/                        ERR 6.0s context deadline exceeded   ← 无公网出口
get https://sts.us-west-2.amazonaws.com/                  ERR 6.0s                             ← 公网 IP 无法到达
get https://bedrock-runtime.us-west-2.amazonaws.com/      ERR 6.0s
get https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/   HTTP 404 in 300ms   ← 经 peering 可达（根路径 404 即连通）
get https://bedrock-runtime.ap-northeast-1.amazonaws.com/ HTTP 404 in 12ms
```

### 2.3 配置规则

| 要调的服务 | 端点在哪 | 网关配置里该写什么 |
|---|---|---|
| STS、东京 Bedrock、Secrets Manager | 与网关**同一个 VPC** | 服务公网域名即可（私有 DNS 开着）。未开私有 DNS 则须改为专属域名（Bedrock 的 `endpoints`、Secrets Manager 的 `endpoint` 字段）或设 SDK 的服务级端点变量 `AWS_ENDPOINT_URL_<SERVICE>`；本文仅对 STS 实测（§3.5） |
| us-west-2 Bedrock | **另一个 VPC**（经 peering） | **必须写端点专属域名**。写公网域名即解析为公网 IP，超时（C5） |

专属域名取 `aws ec2 describe-vpc-endpoints` 输出的 `DnsEntries[0].DnsName`。SigV4 对该 Host 签名，Bedrock 正常接受。

对应 provider 配置：

```yaml
bedrock-usw2-xacct:
  auth: aws_iam
  region: us-west-2                       # Bedrock 签名区域
  sts_region: ap-northeast-1              # STS 区域（§3）
  role_arn: arn:aws:iam::<account-b>:role/<bedrock-xacct-role>
  endpoints:
    anthropic:        "https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/anthropic/v1"
    openai_chat:      "https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/openai/v1"
    openai_responses: "https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/openai/v1"
```

## 3. 跨账号鉴权：STS 的两跳

### 3.1 为什么是两跳

Pod 没有静态密钥。取得 B 账号 Bedrock 调用权的过程：

```
Pod 内文件 /var/run/secrets/eks.amazonaws.com/serviceaccount/token（Kubernetes 签发的 JWT）
   │  跳 1  AssumeRoleWithWebIdentity（不需要 AWS 凭证，不签名）
   ▼
A 账号临时凭证（IRSA 角色）
   │  跳 2  AssumeRole（用跳 1 的凭证签名）
   ▼
B 账号临时凭证（<bedrock-xacct-role>）→ 签 Bedrock 请求
```

**跳 1 的 STS client 由 AWS SDK 在 `config.LoadDefaultConfig()` 内部创建，应用代码不直接持有它，`BaseEndpoint` 这类针对自建 client 的设置覆盖不到它；
跳 2 的 client 由应用自己 `sts.NewFromConfig()` 创建。** 两个 client、两套配置来源。一种误配是把 STS 端点地址只配给自建的 client（`BaseEndpoint`）：跳 2 走了私网，
跳 1 仍按默认规则去打服务公网域名，无出口即失败；报错栈里失败的是 `AssumeRoleWithWebIdentity`，看上去像 IRSA 本身坏了。

### 3.2 实验设计

`tools/stsprobe` 按"基于 AWS SDK 自行封装的网关"这一实现形态编写：`LoadDefaultConfig` → 自建 STS client →
`stscreds.NewAssumeRoleProvider` → `bedrockruntime.InvokeModel`，逐个 HTTP 请求打印目标主机与结果。以网关的 IRSA 身份、在私有节点池、同出向策略下运行。
三个可调项：

| 可调项 | 作用范围 |
|---|---|
| `-sts-region`（= SDK 的 region） | 两跳都受影响：决定默认域名 `sts.<region>.amazonaws.com` 与签名区域 |
| `-sts-base`（= 自建 client 的 `BaseEndpoint`） | **只影响跳 2** |
| 环境变量 `AWS_ENDPOINT_URL_STS` | 进程内所有 STS client，**两跳都影响**；只改地址，不改签名区域 |

### 3.3 结果（C1–C12）

STS 端点在东京。C1–C6、C9–C10 私有 DNS 开；C7–C8、C11–C12 临时关闭私有 DNS（模拟未开私有 DNS 的环境）。C6 与 C2 配置相同，表中合并。

| # | 场景 | region | 跳 2 BaseEndpoint | AWS_ENDPOINT_URL_STS | 跳 1 实际去向 | 跳 2 | Bedrock | 结果 |
|---|---|---|---|---|---|---|---|---|
| C1 | 误配 A：region 取 Bedrock 区域，端点只配给自建 client | us-west-2 | 东京端点 | — | `sts.us-west-2` 公网 → **8s 超时** | 未到 | — | ✗ |
| C2/C6 | 进程级端点变量，但 region 仍是 Bedrock 区域 | us-west-2 | 东京端点 | 东京端点 | 东京端点 200 | **403 `SignatureDoesNotMatch: Credential should be scoped to a valid region`** | — | ✗ |
| **C3** | **本网关配法**：STS 区域钉东京，不配端点 | **ap-northeast-1** | — | — | `sts.ap-northeast-1` → 10.2.x.x 200 | 200 | 经 peering 200 | **✓** |
| C4 | 本网关缺省 `sts_region` | us-west-2 | — | — | 同 C1 | — | — | ✗ |
| C5 | Bedrock 写服务公网域名 | ap-northeast-1 | — | — | 200 | 200 | `bedrock-runtime.us-west-2` 公网 → **8s 超时** | ✗ |
| C7 | 误配 B：STS 端点**无私有 DNS**，端点只配给自建 client | ap-northeast-1 | 东京端点 | — | `sts.ap-northeast-1` → **8s 超时**（域名不再解析到端点） | 未到 | — | ✗ |
| **C8** | C7 + 进程级端点变量 | ap-northeast-1 | 东京端点 | 东京端点 | 东京端点 200 | 200 | 200 | **✓** |
| C9 | C8 但**代码不设 region**，由 SDK 读 EKS webhook 注入的 `AWS_REGION` | （注入）ap-northeast-1 | 东京端点 | 东京端点 | 东京端点 200 | 200 | 200 | ✓ |
| C10 | 同 C9，`AWS_REGION` 改为 us-west-2 | （env）us-west-2 | 东京端点 | 东京端点 | 东京端点 200 | **403 SignatureDoesNotMatch** | — | ✗ |
| C11 | 本网关配法，但 STS 端点**无私有 DNS**（探针；网关镜像对照见 §3.5 G1） | ap-northeast-1 | — | — | `sts.ap-northeast-1` → **8s 超时** | 未到 | — | ✗ |
| C12 | C11 + 进程级端点变量（探针；网关镜像对照见 §3.5 G2） | ap-northeast-1 | — | 东京端点 | 东京端点 200 | 200 | 200 | ✓ |

C7 的原始报错（本环境的 NetworkPolicy 丢包，表现为超时；出口被主动拒绝的环境可能表现为 `connection reset by peer`）：

```
operation error STS: AssumeRole, get identity: get credentials: failed to refresh cached credentials,
failed to retrieve credentials, operation error STS: AssumeRoleWithWebIdentity, https response error StatusCode: 0,
request send failed, Post "https://sts.ap-northeast-1.amazonaws.com/": context deadline exceeded
```

读法：最外层 `AssumeRole`（跳 2）失败 ← 取不到它要用的凭证 ← 跳 1 `AssumeRoleWithWebIdentity` 打的是服务公网域名而不是端点专属域名。
此时在同一网络位置解析该域名（如 `tools/netprobe`），若得到公网 IP，即私有 DNS 未开或未生效。

### 3.4 四个结论

1. **覆盖 STS 端点必须覆盖到跳 1。** `BaseEndpoint` 只管自建 client；要么用 `AWS_ENDPOINT_URL_STS`（进程级，SDK 内建 client 也读），
   要么给 STS 端点开私有 DNS 让服务公网域名直接解析到内网（此时不需要任何端点配置）。
2. **"钉区域"只在 STS 端点开了私有 DNS 时够用。** C11：本网关配法放到未开私有 DNS 的环境同样超时；C12：此时同样需要 `AWS_ENDPOINT_URL_STS`。
   私有 DNS 是否开启决定是否需要端点变量；区域是否显式钉住决定加了端点变量后是否 403（结论 3）。
3. **端点覆盖不改签名区域。** C2/C6：`AWS_ENDPOINT_URL_STS` 把请求送到了东京端点，但 SDK region 仍是 us-west-2，签名区域错，东京 STS 回 403。
   跳 1 不签名所以不受影响，跳 2 一签即暴露。**用于 STS 的 region 必须是 STS 端点所在区域**，Bedrock 的 region 单独配置。
   C9/C10 进一步说明：代码不设 region 时，SDK 读的是 EKS Pod 身份 webhook 注入的 `AWS_REGION`（= 集群区域），与端点区域一致时能通；
   同一份代码把 `AWS_REGION` 改成 us-west-2 立刻 403。依赖注入值而非显式配置，等于与部署所在集群的区域隐式耦合。
4. **Bedrock 跨 VPC 必须用端点专属域名**（C5），与 §2.3 一致。

### 3.5 本网关的配法与边界

`internal/provider/sigv4.go`：`sts_region` 单独成字段，传给 `LoadDefaultConfig(WithRegion(stsRegion))`，跳 1 和跳 2 的 client 都从这份 config
继承区域；Bedrock 签名区域用 `region` 字段另配。**前提是 STS 端点开了私有 DNS**，服务公网域名解析进内网，才不需要任何端点 URL。

私有 DNS 未开时本网关同样连不上 STS。用网关镜像在同一形态下复验（`76-gateway-no-private-dns.sh`），结果与 C11/C12 一致：

- G1：出厂配置（`sts_region: ap-northeast-1`），STS 端点关私有 DNS → 20 秒后 `provider setup failed … resolve aws credentials: context deadline exceeded`，进程退出。
- G2：同上 + Deployment env `AWS_ENDPOINT_URL_STS=https://<sts-endpoint-dns>` → 1 秒内全部 provider `aws identity resolved`，跨账号身份为 B 的
  `assumed-role/<bedrock-xacct-role>/…`，正常监听。

`sigv4.go` 没有读取该变量的代码；是 SDK 的 `LoadDefaultConfig` 自动读取并应用到它创建的所有 STS client（含 IRSA 内建的那个）。

回退链为 `sts_region` → `AWS_REGION` → `AWS_DEFAULT_REGION` → `region`。实验 75 用网关镜像去掉 `sts_region`：

- 默认**仍能启动**——EKS Pod 身份 webhook 注入 `AWS_REGION=ap-northeast-1`，由回退链补足。
- 把 `AWS_REGION` 改成 us-west-2 后：`provider setup failed err=provider "bedrock-usw2-xacct": resolve aws credentials: request canceled,
  context deadline exceeded`，20 秒后进程退出。启动即失败、日志直指 provider，不会以错误配置继续运行。

## 4. 透传原生端点 vs 经 AWS SDK `InvokeModel`

> 下文"SDK 网关"指把客户端 JSON 解析后装进 AWS SDK 结构、经 `InvokeModel` / `InvokeModelWithResponseStream` 调用 Bedrock、再把 event stream
> 还原为 SSE 的网关实现形态。本仓库通过 `endpoints.bedrock_invoke` 实现了同一传输方式，以下对比在同一个二进制、同一条跨账号私网路径上进行。
> 对其他 SDK 网关实现的描述标为"推断"，以其实际代码为准。

### 4.1 两种传输方式

Bedrock 在同一主机 `bedrock-runtime.<region>.amazonaws.com` 上提供两套 API：

| | Bedrock 原生 API `InvokeModel` | 协议兼容端点 |
|---|---|---|
| URL | `/model/<id>/invoke(-with-response-stream)` | `/anthropic/v1/messages`、`/openai/v1/chat/completions`、`/openai/v1/responses` |
| 请求体 | Bedrock 定义（Claude 需 `anthropic_version`，不带 `model`；非流式不带 `stream:true`，见 operations.md） | 与 Anthropic / OpenAI 官方 API 同格式 |
| 流式响应 | AWS 二进制 event stream | 标准 SSE |
| AWS SDK 封装 | 有（`bedrockruntime`） | 无。本网关用 Go 标准库发 HTTP，签名用 `aws-sdk-go-v2` 的 signer 组件（`sigv4.go`） |

请求体实际差异（`adaptInvokeBody` 输出）：

```
透传发给 Bedrock（只改 model）：
  {"model":"us.anthropic.claude-sonnet-5","max_tokens":200,"stream":true,"messages":[…]}
InvokeModel 发给 Bedrock：
  {"anthropic_beta":["interleaved-thinking-2025-05-14"],"anthropic_version":"bedrock-2023-05-31","max_tokens":200,"messages":[…]}
  （删 model / stream；补 anthropic_version；请求头 anthropic-beta 折进 body；GPT 非流式还要删 stream_options）
```

### 4.2 实测（T1–T4，同一跨账号路径）

| 对比项 | 透传 | InvokeModel | 说明 |
|---|---|---|---|
| **OpenAI Responses 协议** | `/v1/responses` 200 | **502 `does not serve openai_responses`**（本网关在 `CanInvoke` 处拒，请求未出网关） | 底层原因（T1b，CLI 直接把 Responses body 发给 B 的 InvokeModel）：**400 `ValidationException … missing field 'messages'`**——由报错推断 InvokeModel 按 Chat Completions 结构反序列化 GPT 请求体 |
| **端点配错时的报错**（指到可达但不是 bedrock-runtime 的私网主机） | 404 `<UnknownOperationException/>`，上游原文 | 404 `deserialization failed … invalid character '<' looking for beginning of value` | SDK 按 JSON 解析 XML 失败；报错看不出是端点错 |
| 流式 TTFT p50（各 6 次） | Claude 1094ms / GPT 582ms | Claude 1076ms / GPT 549ms | 差 20–30ms，噪声级 |
| usage 计量 | 16/10、13/9 | 完全相同 | event stream 还原为 SSE 后计量路径复用 |
| 客户端收到的流 | 标准 Anthropic SSE | 开头几帧（message_start / content_block_start…）格式一致 | 未逐帧全量比对；§6 的真实客户端验收走的是透传路由 |
| 新字段 / 新 beta | 原样转发 | 取决于 body 改写是否覆盖 | 推断，未实测 |
| 凭证 / STS | `sts_region` + 私有 DNS | 复用同一凭证链（`bedrock_invoke` 实现如此），两条路在 STS 上无差 | — |

### 4.3 选型建议

传输方式层面两者在本次测试范围内表现相当：TTFT 无可观测差异、计量一致，两种方式可以并存。选择 InvokeModel 的实现需要额外处理以下几件事：

1. **STS 两跳**（§3.4）：用于 STS 的 region **显式**钉到 STS 端点所在区域，不依赖注入值；STS 端点开私有 DNS 则无需端点变量，未开则设 `AWS_ENDPOINT_URL_STS`。
   本网关同样适用（G1/G2）。SDK 默认用一个 region 同时决定 STS 地址、STS 签名区域、Bedrock 地址、Bedrock 签名区域，跨区域跨账号时四件事须分别对齐，
   少一个即 C1/C2/C5 之一；本网关以 `region` / `sts_region` / `endpoints` 三个字段分管。
2. **Responses 协议**：InvokeModel 不接受，需先转为 Chat 格式（本仓库 `providerProtocol: openai_chat` 即此用途，转换逻辑见 `internal/protocol/translate`）。
3. **端点错配的排障**：`invalid character '<'` 表示 SDK 收到的不是 JSON，端点指到的不是 bedrock-runtime（T2）。
4. **跨账号角色权限**：B 账号角色需含 `arn:aws:bedrock:*:*:project/default`。缺它则 Responses 跨账号 401、Messages/Chat 正常——依据 private-networking.md 的记录；
   本环境只验证了含该项时 200。与传输方式无关，两种实现都需要。

透传兼容端点的优势：零 body 改写、新字段不经改写原样转发、三种协议直通、上游报错原文可读；代价是自行做 SigV4（`sigv4.go` 约 110 行）。

## 5. 排障对照表

| 现象 | 根因 | 实验 | 处理 |
|---|---|---|---|
| `AssumeRoleWithWebIdentity … Post "https://sts.<region>…": context deadline exceeded`（出口被主动拒绝的环境可能为 `connection reset`，未实测） | 跳 1 打公网 STS：region 不是端点所在区域，或端点未开私有 DNS 且只给自建 client 设了 BaseEndpoint | C1、C4、C7、G1 | STS region 钉到端点区域；开私有 DNS 或设 `AWS_ENDPOINT_URL_STS` |
| `AssumeRole … 403 SignatureDoesNotMatch: Credential should be scoped to a valid region` | 端点改到东京但 region 仍是别的区域 | C2/C6、C10 | 用于 STS 的 region 改为端点区域 |
| `InvokeModel … Post "https://bedrock-runtime.us-west-2…": context deadline exceeded`（凭证已成功） | 跨 VPC 用了服务公网域名，私有 DNS 不跨 peering | C5 | 写端点专属域名 |
| `deserialization failed … invalid character '<'` | SDK 的 Bedrock 端点指到了不是 bedrock-runtime 的主机 | T2 | 端点改为 `bedrock-runtime.<region>…` 或其专属域名 |
| `does not serve openai_responses`（本网关）/ `ValidationException … missing field 'messages'`（SDK 直发） | Responses body 走到了 InvokeModel 路径 | T1 / T1b | 路由加 `providerProtocol: openai_chat`；SDK 网关需先把 Responses 转为 Chat |
| 网关启动 `provider setup failed … resolve aws credentials: context deadline exceeded` | 缺省 `sts_region` 且 `AWS_REGION` 不是端点区域；或 STS 端点无私有 DNS 且未设 `AWS_ENDPOINT_URL_STS` | 75、G1 | 补 `sts_region`；设 `AWS_ENDPOINT_URL_STS` |
| 跨账号 `/v1/responses` 401，Messages / Chat 正常 | B 账号角色缺 `arn:aws:bedrock:*:*:project/default`（依据 private-networking.md 记录） | 本环境未复现 401；含该项时实测 200 | 角色策略补该资源 |
| Pod 起不来 / 拉不到镜像 | 缺 ecr.api / ecr.dkr / s3 / ec2 / eks / eks-auth 端点 | 未实测（本环境一次建齐） | 对照 §1.1 端点清单 |

## 6. 真实客户端验收（跨账号、经 peering）

| 客户端 | 路由 | 结果 |
|---|---|---|
| Claude Code → `/v1/messages` | `providerProtocol: openai_chat` → B 的 gpt-5.6-sol | 多轮（含 Read 工具调用与结果回传）全 200，答案正确 |
| Codex → `/v1/responses` | `providerProtocol: anthropic` → B 的 claude-sonnet-5 | 2 轮（exec_command → 结果 → 回答）全 200，答案正确 |
| curl 10 条路由 | 同账号 / 跨账号原生 / 跨账号 InvokeModel / 协议转换 / 两级故障转移 | 全 200，usage 完整 |

## 7. 对现有文档的补充建议

- configuration.md `sts_region` 说明旁补一句：STS 端点未开私有 DNS 时，在 Deployment 增加 `AWS_ENDPOINT_URL_STS=https://<sts-endpoint-dns>`，无需改代码（G2）。
- operations.md 的 `AWS_ENDPOINT_URL_STS` 条目补一句：SDK region 须同时为端点所在区域，否则 403（C2/C10）。
- private-networking.md 跨账号角色权限处强调 `project/default`：本环境含该项，跨账号 `/v1/responses` 首次实测 200。
- 本文未重复 private-networking.md 已有的 IAM 策略原文、NetworkPolicy 原文、EKS Auto Mode 私有节点池细节。
