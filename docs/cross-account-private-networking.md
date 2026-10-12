# 跨账号 · 无公网出口下的网关底层机制（实测版）

> **状态**：给 Odin 的初稿（2026-10-12）。所有数字和日志都来自本次搭建的两账号实验环境，脚本在 `deploy/xacct-lab/`，可重复跑。
> 面向客户的最终版由 Odin 在此基础上组织；本稿把机制、实验、结论、建议写全，供裁剪。
> 账号 ID、VPCE ID 一类环境值在最终版里请换成客户自己的或占位符。

## 0. 这份文档回答什么

上周客户部署 LLM Proxy 时在 **STS VPC Endpoint 对接**上卡了半天。根因不是某个配置写错，而是对三件事的理解有偏差：
IRSA 跨账号鉴权其实是**两跳** STS 调用；VPC Endpoint 的**私有 DNS 只在它所在的 VPC 内生效**；AWS SDK 默认用**一个 region**
同时决定"连哪个地址"和"签名用哪个区域"。这三件事叠在"无公网出口 + 跨区域 + 跨账号"的环境里，任何一处没对齐都是超时或 403，
而且报错指向的往往不是真正出问题的那一跳。

文档按 Odin 指定的拓扑复刻了客户环境（LLM Proxy 在 A 账号东京，STS VPCE 在东京，Bedrock VPCE 在 B 账号 us-west-2，两个 VPC
用 peering 打通，整套没有公网出口），在上面做了三组实验：**网络可达性**、**STS 两跳的各种配法**、**透传 vs SDK InvokeModel**。
每个结论后面都有实验编号可查。

## 1. 环境

### 1.1 拓扑

```
A 账号 · 东京 ap-northeast-1 · VPC 10.2.0.0/16 · 无 IGW / 无 NAT          B 账号 · us-west-2 · VPC 10.1.0.0/16 · 无 IGW / 无 NAT
┌──────────────────────────────────────────────────────────┐     ┌──────────────────────────────────────────────┐
│ EKS Auto Mode（仅私有子网 10.2.1/24 1a、10.2.2/24 1c）      │     │ 子网 10.1.1/24 (2a)、10.1.2/24 (2b)            │
│   LLM Proxy Pod ×2（IRSA 角色 llmgw-xacct-lab-gateway-irsa） │     │                                              │
│   mock 控制面 Pod                                         │     │ bedrock-runtime VPCE（私有 DNS 开）             │
│                                                          │     │   网卡 10.1.1.205 / 10.1.2.94                 │
│ 接口端点（私有 DNS 全开）：                                 │     │   安全组：仅放 10.2.0.0/16 的 443              │
│   sts  10.2.1.147 / 10.2.2.180                           │     │                                              │
│   bedrock-runtime  10.2.1.228 / 10.2.2.43                 │ ◄──►│ IAM 角色 llmgw-bedrock-xacct                  │
│   secretsmanager · ecr.api · ecr.dkr · ec2 · eks · eks-auth│ pcx │   信任：仅 A 的 IRSA 角色                       │
│ 网关端点：s3                                              │     │   权限：InvokeModel* 于 foundation-model/*、   │
│                                                          │     │         inference-profile/*、project/default  │
│ 路由：10.1.0.0/16 → pcx                                   │     │ 路由：10.2.0.0/16 → pcx                        │
│ NetworkPolicy：Pod 出向只放 10.0.0.0/8:443/53 与 Service CIDR│     │ 没有任何计算资源                               │
└──────────────────────────────────────────────────────────┘     └──────────────────────────────────────────────┘
```

与客户环境的对应：A = 客户跑 Proxy 的应用账号，B = 客户集中放模型配额的 AI 账号。B 里不跑任何程序，只有"被调用"所需的
VPCE 和 IAM 角色。

### 1.2 一次跨账号请求的完整路径

以 Claude Code 请求 `claude-sonnet-5-us`（路由到 B 的 `us.anthropic.claude-sonnet-5`）为例：

| 步 | 动作 | 从哪到哪 | 怎么找到目标 |
|---|---|---|---|
| ① | 鉴权 + 查路由 | Proxy → mock 控制面（集群内） | Service DNS |
| ② | **STS 跳 1**：IRSA `AssumeRoleWithWebIdentity`，用 Pod 的 ServiceAccount token 换 A 账号凭证 | Proxy → `sts.ap-northeast-1.amazonaws.com` → **10.2.1.147**（东京 STS VPCE） | 东京 VPC 私有 DNS 把公网域名解析成 VPCE 内网 IP |
| ③ | **STS 跳 2**：`AssumeRole` 到 B 的 `llmgw-bedrock-xacct` | 同上 | 同上 |
| ④ | 用 B 的凭证对请求做 SigV4 签名（区域 us-west-2），POST 到 B 的 Bedrock | Proxy → `vpce-09bd…rtsictp1.bedrock-runtime.us-west-2.vpce.amazonaws.com` → **10.1.1.205** → 经 peering 进 B VPC → VPCE → Bedrock | **VPCE 专属域名**（公网 DNS 可解析成 B 的内网 IP）+ 东京路由表 `10.1/16 → pcx` |
| ⑤ | 响应沿原路流回，Proxy 原样转给客户端，旁路抽 usage | — | — |
| ⑥ | 异步上报 token 用量 | Proxy → 控制面 | — |

全程没有一个包离开 AWS 内网。②③ 和 ④ 分别是客户踩坑和潜在踩坑的地方，下文分开讲。

### 1.3 建环境脚本

`deploy/xacct-lab/`，账号 ID 和 AWS CLI profile 由环境变量传入，幂等，可反复执行：

| 脚本 | 内容 |
|---|---|
| `10-b-bedrock-side.sh` | B：VPC、2 子网、安全组、bedrock-runtime VPCE、跨账号角色 |
| `20-a-network.sh` | A：VPC、2 子网、8 个接口端点 + s3 网关端点、peering 发起/接受/双向路由 |
| `30-a-eks.sh` | A：EKS Auto Mode、OIDC、IRSA 角色、ECR；收紧 B 角色信任到 IRSA 角色 |
| `40-a-deploy.sh` | ko 构建推送、渲染 k8s 清单（三个 provider：A 原生 / B 原生 / B InvokeModel）、apply |
| `50-smoke.sh` | 10 条路由冒烟 |
| `60-netprobe.sh` / `tools/netprobe` | 私网内解析域名、试连 URL 的探针（公共镜像在无出口 VPC 拉不下来，所以自己打一个） |
| `70-sts-experiments.sh` / `tools/stsprobe` | STS 两跳实验（§3） |
| `75-gateway-no-sts-region.sh` | 真网关漏配 `sts_region` 的对照（§3.5） |
| `80-transport-compare.sh` | 透传 vs InvokeModel 对比（§4） |
| `90-teardown.sh` | 按依赖顺序拆除（默认 dry-run，`CONFIRM=yes` 才删） |

从零到 10 条路由全通约 1.5 小时（EKS 建集群 12 分钟，其余是等端点 available）。运行成本约 $0.5/小时（EKS $0.10、2 节点 ~$0.17、
9 个接口端点 × 2 AZ ~$0.20，模型调用另计）。

## 2. 网络：无公网出口下怎么到达每个 AWS 服务

### 2.1 三个基本事实

**事实 1：AWS 服务不在你的 VPC 里。** Bedrock、STS、ECR 的服务器在 AWS 自己的网络，对外是公网 IP。无 IGW/NAT 的 VPC 默认一个都连不上——
连 EKS 节点都起不来（节点要拉镜像、注册、拿凭证）。所以本环境建了 9 个接口端点 + 1 个网关端点，缺任何一个对应功能就断。

**事实 2：接口型 VPC Endpoint 是放在你子网里的一张网卡。** 建 Bedrock VPCE 时指定两个子网，AWS 就在每个子网放一张网卡
（10.1.1.205、10.1.2.94），网卡另一头经 AWS 内部线路直连 Bedrock。对 VPC 里的机器来说，往 10.1.1.205:443 发 HTTPS 就是在调 Bedrock。
两个子网是为了跨两个可用区，一个机房故障另一张网卡还在。

**事实 3：程序配置里写的是域名，不是 IP。** 所以"能不能到 VPCE"最终取决于 DNS 把域名解析成什么。这是全部坑的交汇点。

### 2.2 私有 DNS 与专属域名

一个 VPCE 有两类名字：

| 名字 | 例子 | 谁解析能得到 VPCE 内网 IP |
|---|---|---|
| **服务公网域名** | `bedrock-runtime.us-west-2.amazonaws.com` | 全球 DNS 答公网 IP；**只有** VPCE 所在 VPC 开了"私有 DNS"后，**该 VPC 内**的解析器才答内网 IP |
| **VPCE 专属域名** | `vpce-09bdbfffcab83a5ce-rtsictp1.bedrock-runtime.us-west-2.vpce.amazonaws.com` | 登记在全球 DNS，**任何地方**解析都是内网 IP |

私有 DNS 的实现是给 VPCE 所在的 VPC 挂一个私有 hosted zone，所以**它不跨 VPC、不跨 peering**。东京 VPC 里的 Proxy 解析
`bedrock-runtime.us-west-2.amazonaws.com`，问的是东京 VPC 的解析器，B 那条私有 DNS 规则对它不存在。

**实测**（实验 N1：`60-netprobe.sh`，探针在私有节点、与网关同出向策略下跑）：

```
resolve sts.ap-northeast-1.amazonaws.com                 10.2.1.147 10.2.2.180      ← 东京 VPCE，私有 DNS 生效
resolve bedrock-runtime.ap-northeast-1.amazonaws.com     10.2.1.228 10.2.2.43       ← 同上
resolve sts.us-west-2.amazonaws.com                      44.248.101.77              ← 公网 IP：东京没有这个端点
resolve bedrock-runtime.us-west-2.amazonaws.com          44.227.6.224 54.244.180.81 …← 公网 IP：B 的私有 DNS 管不到东京
resolve vpce-09bd…rtsictp1.bedrock-runtime.us-west-2.vpce.amazonaws.com   10.1.1.205 10.1.2.94   ← 专属域名，哪里查都是内网

get https://checkip.amazonaws.com/                        ERR 6.0s context deadline exceeded     ← 无公网出口
get https://sts.us-west-2.amazonaws.com/                  ERR 6.0s                               ← 公网 IP 出不去
get https://bedrock-runtime.us-west-2.amazonaws.com/      ERR 6.0s
get https://vpce-09bd…vpce.amazonaws.com/                 HTTP 404 in 300ms  ← 经 peering 可达（404 是根路径，连通即可）
get https://bedrock-runtime.ap-northeast-1.amazonaws.com/ HTTP 404 in 12ms
```

### 2.3 由此得出的配置规则

| 要调的服务 | 它的 VPCE 在哪 | Proxy 配置里该写什么 |
|---|---|---|
| STS、东京 Bedrock、Secrets Manager | 和 Proxy **同一个 VPC** | 公网域名即可（私有 DNS 开着）。不开私有 DNS 就必须写专属域名 |
| us-west-2 Bedrock | **另一个 VPC**（经 peering） | **必须写专属域名**。写公网域名 = 解析成公网 IP = 超时（实验 C5） |

专属域名取 `aws ec2 describe-vpc-endpoints` 输出 `DnsEntries[0].DnsName`。SigV4 对这个 Host 签名，Bedrock 照常接受。

本网关对应配置（`bedrock-usw2-xacct` provider）：

```yaml
bedrock-usw2-xacct:
  auth: aws_iam
  region: us-west-2                       # Bedrock 签名区域
  sts_region: ap-northeast-1              # STS 区域（见 §3）
  role_arn: arn:aws:iam::<B>:role/llmgw-bedrock-xacct
  endpoints:
    anthropic:        "https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/anthropic/v1"
    openai_chat:      "https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/openai/v1"
    openai_responses: "https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/openai/v1"
```

## 3. 跨账号鉴权：STS 的两跳

### 3.1 为什么是两跳

Pod 没有静态密钥。它拿到 B 账号 Bedrock 调用权的过程：

```
Pod 内文件 /var/run/secrets/eks.amazonaws.com/serviceaccount/token（K8s 签发的 JWT）
   │  跳 1  AssumeRoleWithWebIdentity（不需要 AWS 凭证，不签名）
   ▼
A 账号临时凭证（角色 llmgw-xacct-lab-gateway-irsa）
   │  跳 2  AssumeRole（用跳 1 的凭证签名）
   ▼
B 账号临时凭证（角色 llmgw-bedrock-xacct）→ 签 Bedrock 请求
```

**关键点：跳 1 的 STS client 是 AWS SDK 在 `config.LoadDefaultConfig()` 内部自己创建的，应用代码摸不到它。**
跳 2 的 client 是应用自己 `sts.NewFromConfig()` 建的。两个 client、两套配置来源。

客户的 bug 就在这：他们把 STS VPCE 地址配给了自己建的那个 client（`BaseEndpoint`），跳 2 走了私网，但跳 1 的 client 没拿到这个设置，
照旧按默认规则去打公网 STS，无出口即超时。报错栈里失败的是 `AssumeRoleWithWebIdentity`，看上去像 IRSA 坏了。

### 3.2 实验设计

`tools/stsprobe` 按客户代码的形态写：`LoadDefaultConfig` → 自建 STS client → `stscreds.NewAssumeRoleProvider` → `bedrockruntime.InvokeModel`，
每个 HTTP 请求打印目标主机和结果。以网关的 IRSA 身份、在私有节点池、同出向策略下跑。可调的三个旋钮：

| 旋钮 | 作用范围 |
|---|---|
| `-sts-region`（= SDK 的 region） | 两跳都受影响：决定默认域名 `sts.<region>.amazonaws.com` 和签名区域 |
| `-sts-base`（= 自建 client 的 `BaseEndpoint`） | **只影响跳 2** |
| 环境变量 `AWS_ENDPOINT_URL_STS` | 进程内所有 STS client，**两跳都影响**；只改地址，不改签名区域 |

### 3.3 十组结果

STS VPCE 在东京、私有 DNS 开（C1–C6）或临时关掉（C7–C8，模拟客户环境）：

| # | 场景 | region | 跳 2 BaseEndpoint | AWS_ENDPOINT_URL_STS | 跳 1 实际去向 | 跳 2 | Bedrock | 结果 |
|---|---|---|---|---|---|---|---|---|
| C1 | 客户失败形态（区域=Bedrock 区域） | us-west-2 | 东京 VPCE | — | `sts.us-west-2` 公网 → **8s 超时** | 未到 | — | ✗ |
| C2/C6 | 客户修法但区域没改 | us-west-2 | 东京 VPCE | 东京 VPCE | 东京 VPCE 200 | **403 `SignatureDoesNotMatch: Credential should be scoped to a valid region`** | — | ✗ |
| **C3** | **本网关做法** | **ap-northeast-1** | — | — | `sts.ap-northeast-1` → 10.2.1.147 200 | 200 | 经 peering 200 | **✓** |
| C4 | 本网关漏配 `sts_region` | us-west-2 | — | — | 同 C1 | — | — | ✗ |
| C5 | Bedrock 写公网域名 | ap-northeast-1 | — | — | 200 | 200 | `bedrock-runtime.us-west-2` 公网 → **8s 超时** | ✗ |
| C7 | **客户真实形态**：STS VPCE 无私有 DNS，只设 BaseEndpoint | ap-northeast-1 | 东京 VPCE | — | `sts.ap-northeast-1` → **公网 IP** → 8s 超时 | 未到 | — | ✗ |
| **C8** | **客户修法**：+ 环境变量 | ap-northeast-1 | 东京 VPCE | 东京 VPCE | 东京 VPCE 200 | 200 | 200 | **✓** |
| C9 | 客户修法，**代码不设 region**，由 SDK 读 EKS webhook 注入的 `AWS_REGION` | （注入）ap-northeast-1 | 东京 VPCE | 东京 VPCE | 东京 VPCE 200 | 200 | 200 | ✓ |
| C10 | 同 C9，`AWS_REGION` 改为 us-west-2 | （env）us-west-2 | 东京 VPCE | 东京 VPCE | 东京 VPCE 200 | **403 SignatureDoesNotMatch** | — | ✗ |

C7 的原始报错，和客户现场一致（Odin 10/12 记进 operations.md 的那一行）：

```
operation error STS: AssumeRole, get identity: get credentials: failed to refresh cached credentials,
failed to retrieve credentials, operation error STS: AssumeRoleWithWebIdentity, https response error StatusCode: 0,
request send failed, Post "https://sts.ap-northeast-1.amazonaws.com/": context deadline exceeded
```

读法：最外层 `AssumeRole`（跳 2）失败 ← 因为取不到它要用的凭证 ← 跳 1 `AssumeRoleWithWebIdentity` 打的是**公网域名**。
对端是公网 IP（`44.248.101.77` 一类）就是私有 DNS 没生效或没开。

### 3.4 三个结论

1. **覆盖 STS 端点必须覆盖到跳 1。** `BaseEndpoint` 只管自己建的 client；要么用 `AWS_ENDPOINT_URL_STS`（进程级，SDK 内建 client 也认），
   要么给 STS VPCE 开私有 DNS 让公网域名直接解析到内网（此时什么端点都不用配）。
2. **端点覆盖不改签名区域。** C2/C6：`AWS_ENDPOINT_URL_STS` 把请求送到了东京 VPCE，但 SDK region 还是 us-west-2，签名区域错，东京 STS 回 403。
   跳 1 不签名所以躲过了，跳 2 一签就暴露。**用于 STS 的 region 必须是 STS VPCE 所在区域**，Bedrock 的 region 单独配。
   这条 Odin 的文档没写。C9/C10 进一步说明：代码不设 region 时，SDK 读的是 EKS Pod 身份 webhook 注入的 `AWS_REGION`（= 集群区域），
   在东京集群上恰好是对的；同一份代码把 `AWS_REGION` 改成 us-west-2 立刻 403。客户的修法如果是靠这个注入值，就是在靠巧合。
3. **Bedrock 跨 VPC 必须用专属域名**（C5），与 §2.3 一致。

### 3.5 本网关为什么没踩

`internal/provider/sigv4.go`：`sts_region` 单独成字段，传给 `LoadDefaultConfig(WithRegion(stsRegion))`，跳 1 和跳 2 的 client 都从这份 config
继承区域；Bedrock 签名区域用 `region` 字段另配。STS VPCE 开私有 DNS，公网域名解析进内网，不需要任何端点 URL。

回退链是 `sts_region` → `AWS_REGION` → `AWS_DEFAULT_REGION` → `region`。实验 75 用真网关镜像去掉 `sts_region`：

- 默认**仍能起**——EKS Pod 身份 webhook 自动注入 `AWS_REGION=ap-northeast-1`，回退链兜住了。
- 把 `AWS_REGION` 强行改成 us-west-2 后：`provider setup failed err=provider "bedrock-usw2-xacct": resolve aws credentials: request canceled,
  context deadline exceeded`，20 秒后进程退出。启动即失败、日志直指 provider，不会带病运行。

两层保险（显式配置 + 环境回退）对客户的意义：他们的代码一层都没有，全靠 SDK 默认行为和环境变量的巧合。

## 4. 客户的做法 vs 本网关：区别与建议

> 关于客户做法的描述来自 Odin 的转述（"没有做模型透传，是转换成 Go SDK 重新封装一次再发出去"）和他在本仓库按同一方式复刻的
> `bedrock_invoke` 路径，以及上周现场的报错文本。**我们没有看过客户的代码**，下面凡是"客户……"的表述请 Odin 按实际核对。

### 4.0 一览

| 维度 | 客户做法（SDK 网关） | 本网关 | 区别的后果 | 建议 |
|---|---|---|---|---|
| 到 Bedrock 的传输 | AWS SDK `InvokeModel` / `InvokeModelWithResponseStream`：改写 body、解二进制 event stream 再组 SSE | 透传 Bedrock 的协议兼容端点：只改 `model`，自己 SigV4，响应字节原样回 | 性能、计量无差（T3）；但 InvokeModel 不收 Responses 协议（T1）、端点错配时报错误导（T2）、新字段依赖改写覆盖 | **可不改**。架构能跑；下面三条单独处理 |
| 跨账号 STS | 自建 STS client 设 `BaseEndpoint`，跳 1 没覆盖；修后靠 `AWS_ENDPOINT_URL_STS`（Odin 转述） | `sts_region` 字段钉区域 + STS VPCE 私有 DNS，两跳自动走内网；回退链兜底 | 客户的修法**只在 STS 的 region 等于东京时成立**。代码不设 region 时，这个"东京"来自 EKS webhook 注入的 `AWS_REGION`（C9 通）；`AWS_REGION` 换成别的区域即 403（C10）；代码显式设 us-west-2 则一直 403（C2）。客户属于哪种**未知** | **必须改**：STS 的 region 显式钉到 VPCE 区域，不依赖注入值；开私有 DNS 或保留环境变量 |
| 多协议 / 协议转换 | **推断**：走 InvokeModel 就只能接 Anthropic Messages 和 OpenAI Chat 两种 body（T1 在本网关的 InvokeModel 路径上实测 Responses → 502）；客户是否另做了转换未知 | 三种协议原生直通；路由字段 `providerProtocol` 六向互转（Claude Code→GPT、Codex→Claude 已真机验收） | 若客户未做转换，Codex 这类只发 Responses 的客户端在他们网关上无法使用 | **建议补**：至少加 Responses→Chat 转换，或参考本仓库 `internal/protocol/translate` |
| 计量来源 | **推断**：usage 只能从 SDK 解出的 event stream 块里取 | 从上游原始字节解析，与透传该协议时一致 | 本网关两条路（透传 / `bedrock_invoke`）实测 usage 完全相同（T3）；客户那边是否所有协议、流式/非流式都取到了 usage，**未验证** | 请客户核对计量是否覆盖 Claude/GPT × 流式/非流式四种组合 |
| 上游错误可读性 | SDK 把非 JSON 响应包装成 `deserialization failed … '<'`（T2 实测，客户现场同款报错见 operations.md） | 透传上游原文 `<UnknownOperationException/>`（T2） | 端点配错时客户网关的报错看不出是端点错，排障慢 | 排障表 §5 给客户 |
| 配置模型 | SDK 默认一个 region 同时决定 STS 地址、STS 签名区域、Bedrock 地址、Bedrock 签名区域；要分开只能靠环境变量 / 逐 client 覆盖 | `region` / `sts_region` / `endpoints` 三个字段分管 Bedrock 签名区域、STS 区域、地址 | 跨区域跨账号时四件事必须分别对齐，少一个就是 C1/C2/C5 之一 | 文档化这几个旋钮，客户按表核对自己的代码 |
| 跨账号角色权限 | 未知 | 含 `project/default` | 缺它则 Responses 跨账号 401，Messages/Chat 正常（9/3 PoC 对端就是这样） | 提醒客户 B 账号角色加 `arn:aws:bedrock:*:*:project/default` |

证据等级：**实测** = 本环境跑出来的（有实验编号）；**推断** = 从 InvokeModel API 的性质或 Odin 的转述推出，客户实际代码可能不同，请 Odin 核对。

一句话给 Odin：**传输方式不必推翻，STS 配法必须改，协议覆盖建议补。** 展开见 §4.3。

### 4.1 两种传输方式是什么

Bedrock 在同一个主机 `bedrock-runtime.<region>.amazonaws.com` 上提供两套 API：

| | Bedrock 原生 API `InvokeModel` | 协议兼容端点 |
|---|---|---|
| URL | `/model/<id>/invoke(-with-response-stream)` | `/anthropic/v1/messages`、`/openai/v1/chat/completions`、`/openai/v1/responses` |
| 请求体 | Bedrock 自己定义（Claude 要 `anthropic_version`，不能带 `model`/`stream`） | 与 Anthropic / OpenAI 官方 API 一字不差 |
| 流式响应 | AWS 二进制 event stream | 标准 SSE |
| 有 AWS SDK 封装吗 | 有（`bedrockruntime`） | 没有——它是给 Anthropic/OpenAI 的 SDK 用的，那两家 SDK 不做 SigV4 |

**客户的做法**：解析客户端 JSON → 装进 SDK 结构 → `InvokeModel` → 解 event stream → 重组 SSE 回客户端。
**本网关默认做法（透传）**：只改 `model` 字段 → 自己 SigV4 → POST 到兼容端点 → 响应字节原样回客户端，旁路抽 usage。
**本网关也实现了客户的做法**（`endpoints.bedrock_invoke`，Odin 9/23），所以下面的对比在同一个二进制、同一条跨账号私网路径上做。

请求体实际差异（`adaptInvokeBody` 输出）：

```
透传发给 Bedrock（只改了 model）：
  {"model":"us.anthropic.claude-sonnet-5","max_tokens":200,"stream":true,"messages":[…]}
InvokeModel 发给 Bedrock：
  {"anthropic_beta":["interleaved-thinking-2025-05-14"],"anthropic_version":"bedrock-2023-05-31","max_tokens":200,"messages":[…]}
  （删 model / stream；补 anthropic_version；请求头 anthropic-beta 折进 body；GPT 非流式还要删 stream_options）
```

### 4.2 实测（实验 T1–T4，同一跨账号路径）

| 对比项 | 透传 | InvokeModel | 说明 |
|---|---|---|---|
| **OpenAI Responses 协议**（Codex 用） | `/v1/responses` 200 | **502 `does not serve openai_responses`** | InvokeModel 不收 Responses body。SDK 网关要服务 Codex 类客户端，必须先转成 Chat 格式 |
| **端点配错时的报错**（指到可达但不是 bedrock-runtime 的私网主机） | 404 `<UnknownOperationException/>`，上游原文 | 404 `deserialization failed … invalid character '<' looking for beginning of value` | SDK 硬按 JSON 解析 XML；**从报错看不出是端点错**。Odin operations.md 排障表那一行的实证 |
| 流式 TTFT p50（各 6 次） | Claude 1094ms / GPT 582ms | Claude 1076ms / GPT 549ms | 差 20–30ms，噪声级 |
| usage 计量 | 16/10、13/9 | 完全相同 | event stream 还原为 SSE 后计量路径复用 |
| 客户端收到的流 | 标准 Anthropic SSE | 逐帧一致 | 对客户端透明 |
| 新字段 / 新 beta | 自动通过 | 取决于 body 改写是否覆盖 | 多一层改写 = 多一处要跟 Bedrock 规则同步 |
| 凭证 / STS | 网关 `sts_region` + 私有 DNS | 复用同一凭证链（`bedrock_invoke` 实现如此）；**客户自己的 SDK 代码没有这层**，即 §3 的坑 | |

### 4.3 对"客户要不要改"的建议（供 Odin 定稿）

**架构层面不必劝客户重写**——InvokeModel 路径能跑、性能无差、计量一致，推翻它的收益不够。**但凭证这一段代码必须改**，另外两点建议补：

1. **STS 两跳**（§3.4 第 1、2 条）：现在能跑是巧合（Pod 的 `AWS_REGION` 恰好是东京），换个部署形态（`AWS_REGION` 不是东京、或 STS VPCE 私有 DNS 没开）就会再次卡住。
   最省事的是：STS VPCE 开私有 DNS + 用于 STS 的 region 显式钉到东京；不开私有 DNS 就用 `AWS_ENDPOINT_URL_STS` 且 region 仍钉东京。
2. **Responses 协议走不了 InvokeModel**。有 Codex 类用户就要在网关里做 Responses→Chat 转换（本网关的 `providerProtocol: openai_chat`）。
3. **端点配错时 SDK 的报错具有误导性**（`invalid character '<'`）。排障时看 RequestID 是否 UUID（到了某个 AWS 服务）、模型 ID 错是 400 JSON 不是这个。

如果客户愿意调整架构，透传兼容端点的优势是：零 body 改写、新模型新字段自动兼容、三种协议都能直通、报错原文可读；代价是要自己做 SigV4
（本仓库 `sigv4.go` 约 100 行，可直接参考）。

## 5. 排障对照表

| 现象 | 根因 | 实验 | 处理 |
|---|---|---|---|
| `AssumeRoleWithWebIdentity … Post "https://sts.<region>…": context deadline exceeded` | 跳 1 打公网 STS：region 不是 VPCE 所在区域，或 VPCE 没开私有 DNS 且只给自建 client 设了 BaseEndpoint | C1、C4、C7 | STS region 钉到 VPCE 区域；开私有 DNS 或设 `AWS_ENDPOINT_URL_STS` |
| `AssumeRole … 403 SignatureDoesNotMatch: Credential should be scoped to a valid region` | 端点改到东京但 region 仍是别的区域 | C2/C6 | 用于 STS 的 region 改为 VPCE 区域 |
| `InvokeModel … Post "https://bedrock-runtime.us-west-2…": context deadline exceeded`（凭证已成功） | 跨 VPC 用了公网域名，私有 DNS 不跨 peering | C5 | 写 VPCE 专属域名 |
| `deserialization failed … invalid character '<'` | SDK 的 Bedrock 端点指到了不是 bedrock-runtime 的主机 | T2 | 端点改为 `bedrock-runtime.<region>…` 或其 VPCE 专属域名 |
| `does not serve openai_responses` | Responses body 走到了 InvokeModel 路径 | T1 | 路由加 `providerProtocol: openai_chat` |
| 网关启动 `provider setup failed … resolve aws credentials: context deadline exceeded` | 本网关漏配 `sts_region` 且 `AWS_REGION` 不是 VPCE 区域 | 75 | 补 `sts_region` |
| Pod 起不来 / 拉不到镜像 | 缺 ecr.api / ecr.dkr / s3 / ec2 / eks / eks-auth 端点 | 建环境时 | 对照 §1.1 端点清单 |

## 6. 真实客户端验收（跨账号、经 peering）

| 客户端 | 路由 | 结果 |
|---|---|---|
| Claude Code → `/v1/messages` | `providerProtocol: openai_chat` → B 的 gpt-5.6-sol | 3 轮（首问→Read 工具→结果→回答）全 200，答案正确 |
| Codex → `/v1/responses` | `providerProtocol: anthropic` → B 的 claude-sonnet-5 | 2 轮（exec_command→结果→回答）全 200，答案正确 |
| curl 10 条路由 | 同账号 / 跨账号原生 / 跨账号 InvokeModel / 转换 / 两级故障转移 | 全 200，usage 完整 |

## 7. 备注

- Odin 10/12 文档里的 `AWS_ENDPOINT_URL_STS` 建议正确，但要补一句"region 也要是 VPCE 所在区域"（C2/C6 的 403）。
- 实验 75 显示本网关在 EKS 上即使漏配 `sts_region` 也能靠 webhook 注入的 `AWS_REGION` 兜住；要不要把这一点写进 configuration.md 的 `sts_region` 说明？
- 本次 B 账号的跨账号角色权限里含 `arn:aws:bedrock:*:*:project/default`，所以跨账号 `/v1/responses` 实测 200——这是第一次在跨账号路径上
  验证到 Responses 通。你 9/3 PoC 的对端账号（别人管理的那个 Bedrock 账号）角色缺这条，导致 Messages/Chat 200 而 Responses 401
  （private-networking.md §六、CHANGELOG 9/28 都记过）。客户的 B 账号通常也是另一个团队管，建议在最终文档里单列提醒。
- 环境仍在运行可随时复跑；`90-teardown.sh` 已写好未执行，拆除顺序：k8s 负载 → EKS 栈 → A 的端点/peering/VPC → B 的端点/角色/VPC → ECR。
- 本稿未写：IAM 策略原文、NetworkPolicy 原文、EKS Auto Mode 私有节点池细节——这些 `private-networking.md` 已有，最终版可引用或合并。
