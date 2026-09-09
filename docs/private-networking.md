# 跨区域、跨账号、全私网访问 Bedrock

本文覆盖 EKS 上的完整部署步骤（IRSA），以及 PoC 里验证过的最复杂拓扑：网关在东京 EKS，模型在 us-west-2、属于另一个
AWS 账号，全部流量不出 AWS 私网。

## 拓扑

```
东京 VPC 10.2/16                                              us-west-2 VPC 10.1/16
┌────────────────────────────────────────────────┐            ┌───────────────────────────────┐
│ 私有子网节点池（NodePool private）                │            │                               │
│   网关 Pod（NetworkPolicy 只放 10/8 出向）         │            │                               │
│      │                                          │            │                               │
│      ├─ Secrets Manager VPCE（私有 DNS）          │            │                               │
│      │    启动时读控制面 token / 供应商 api_key     │            │                               │
│      ├─ STS VPCE（私有 DNS）                      │            │                               │
│      │    AssumeRoleWithWebIdentity（IRSA）       │            │                               │
│      │    AssumeRole → Bedrock 账号的角色          │  peering   │  bedrock-runtime VPCE          │
│      ├─ SigV4(us-west-2) ────────────────────────┼───────────►│  vpce-xxxx.…vpce.amazonaws.com │
│      └─ SigV4(ap-northeast-1) ─► 东京 bedrock-runtime VPCE（本账号，私有 DNS）                       │
└────────────────────────────────────────────────┘            └───────────────────────────────┘
```

两条路径共用一个网关、一份 IRSA 身份：

- **本账号、本区域**：`bedrock` provider，域名 `bedrock-runtime.ap-northeast-1.amazonaws.com` 被东京 VPC 内的
  VPC Endpoint 私有 DNS 解析成 10.2.x，不出公网。
- **跨账号、跨区域**：`bedrock-usw2-xacct` provider，IRSA 身份先 AssumeRole 到 Bedrock 账号的角色，再对 us-west-2 签名，
  请求经 VPC peering 打到对端 VPC 里的 Bedrock VPCE。

## 一、集群与身份

### 集群

`deploy/eks/cluster.yaml` 是 PoC 用的 eksctl 规格：EKS Auto Mode、开 OIDC、复用已有 VPC、API 端点公网访问只放行指定 CIDR。
换环境要改 VPC / 子网 ID、`publicAccessCIDRs`、`tags`。

```bash
eksctl create cluster -f deploy/eks/cluster.yaml
```

出口 IP 变了之后 kubectl 会超时，更新白名单：

```bash
aws eks update-cluster-config --name llm-gateway-poc --region ap-northeast-1 \
  --resources-vpc-config endpointPublicAccess=true,endpointPrivateAccess=true,publicAccessCidrs=<新IP>/32
```

### IAM 策略与 IRSA

`deploy/eks/bedrock-invoke-policy.json`：

| 语句 | 动作 | 资源 | 用途 |
| --- | --- | --- | --- |
| BedrockInvoke | `bedrock:InvokeModel`、`bedrock:InvokeModelWithResponseStream` | `foundation-model/*`、`inference-profile/*`、`project/default` | 调模型。`project/default` 是 Responses API 鉴权用的，缺了只有 `/v1/responses` 报 401 |
| AssumeCrossAccountBedrockRole | `sts:AssumeRole` | 对端账号的角色 ARN | 跨账号场景才需要 |

`deploy/eks/secrets-read-policy.json`（密钥存 Secrets Manager 时才需要）：

| 语句 | 动作 | 资源 | 用途 |
| --- | --- | --- | --- |
| ReadGatewaySecrets | `secretsmanager:GetSecretValue` | `secret:llm-gateway/*` | 启动时读控制面 token、供应商 api_key。前缀限定，别的系统的 secret 读不到。secret 用自定义 KMS 密钥时再加 `kms:Decrypt` |

```bash
aws iam create-policy --policy-name llm-gateway-poc-bedrock-invoke \
  --policy-document file://deploy/eks/bedrock-invoke-policy.json
aws iam create-policy --policy-name llm-gateway-poc-secrets-read \
  --policy-document file://deploy/eks/secrets-read-policy.json
eksctl create iamserviceaccount --cluster llm-gateway-poc --region ap-northeast-1 \
  --namespace llm-gateway --name llm-gateway --role-name llm-gateway-poc-bedrock-invoke \
  --attach-policy-arn arn:aws:iam::<account>:policy/llm-gateway-poc-bedrock-invoke \
  --attach-policy-arn arn:aws:iam::<account>:policy/llm-gateway-poc-secrets-read --approve

# 密钥本体：一个 JSON secret 放一套环境的全部密钥，配置里按键引用
aws secretsmanager create-secret --region ap-northeast-1 --name llm-gateway/poc \
  --secret-string '{"CP_GATEWAY_TOKEN":"<token>"}'
```

`eksctl create iamserviceaccount` 会创建角色（信任策略指向集群 OIDC provider 并限定 `system:serviceaccount:llm-gateway:llm-gateway`）
并给 ServiceAccount 打上 `eks.amazonaws.com/role-arn` 注解。`deploy/k8s/10-serviceaccount.yaml` 里也写了这个注解，
换账号要改。Pod 里 SDK 默认凭证链读到 `AWS_ROLE_ARN` + `AWS_WEB_IDENTITY_TOKEN_FILE` 后自动换取临时凭证，没有静态密钥，没有 sidecar。

更新已有策略时先拉线上版本再改，避免覆盖别人的改动：

```bash
aws iam get-policy-version --policy-arn <arn> --version-id $(aws iam get-policy --policy-arn <arn> --query Policy.DefaultVersionId --output text)
# 对比、修改本地文件后
aws iam create-policy-version --policy-arn <arn> --policy-document file://deploy/eks/bedrock-invoke-policy.json --set-as-default
```

托管策略最多保留 5 个版本，满了要先删旧版本。

### 跨账号角色（对端账号）

对端账号需要一个角色，信任策略允许网关的 IRSA 角色：

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "AWS": "arn:aws:iam::<gateway-account>:role/llm-gateway-poc-bedrock-invoke" },
    "Action": "sts:AssumeRole"
  }]
}
```

权限策略与上面的 BedrockInvoke 语句相同，**务必包含 `arn:aws:bedrock:*:*:project/default`**，否则 Anthropic Messages 和
Chat Completions 正常、Responses API 返回 401 `access_denied`。PoC 时对端角色正是缺了这条。

## 二、镜像

```bash
export KO_DOCKER_REPO=<account>.dkr.ecr.<region>.amazonaws.com/sample-llm-gateway
ko build --base-import-paths --platform=linux/amd64,linux/arm64 --tags=v0.2.0 ./cmd/gateway ./cmd/mock-controlplane
```

ko 不需要 Docker daemon，直接用 Go 交叉编译并推送，多架构镜像让 Auto Mode 自由选 amd64 / arm64 节点。ko 内置 ECR 凭证链，
不要把 `ecr get-login-password` 的输出写进任何文件。有 Docker 的环境可用 `deploy/Dockerfile`。基础镜像是
`public.ecr.aws/amazonlinux/amazonlinux:2023-minimal`：带 CA 证书、无 shell、以 uid 65532 运行。

## 三、网络：让流量只走私网

`deploy/k8s/05-network.yaml` 包含四个对象，按需应用。

### 1. 开启 Network Policy 控制器

EKS Auto Mode 的 Network Policy 控制器默认关闭，靠 kube-system 里的 `amazon-vpc-cni` ConfigMap 打开：

```yaml
data:
  enable-network-policy-controller: "true"
```

应用后几秒内控制器选主，之后 NetworkPolicy 对象才会被翻译成节点上的 eBPF 规则（`kubectl get policyendpoints -A` 能看到）。

### 2. 只选私有子网的 NodeClass

Auto Mode 默认 NodeClass 会把节点放进集群拿到的任意子网，包括公有子网。PoC 里第一批节点就落在了公有子网，那里的路由表
没有到 us-west-2 的 peering 路由。`private-only` NodeClass 的 `subnetSelectorTerms` 只列两个私有子网，`role` 和
`securityGroupSelectorTerms` 沿用默认 NodeClass 的值（`kubectl get nodeclass default -o yaml` 可查），换集群要改。

### 3. NodePool 与 nodeSelector

NodePool `private` 引用上面的 NodeClass，节点带标签 `network-tier: private`。网关和 mock 控制面的 Deployment 用
`nodeSelector: { network-tier: private }` 固定调度到该节点池。不需要私网约束的环境可以不应用 `05-network.yaml` 并删掉 `nodeSelector`。

网关 Deployment 还带节点反亲和（硬约束：两副本不同节点）和跨可用区分布（软约束），所以该节点池至少会起两个节点，
Auto Mode 会按需自动补；NodeClass 只选了两个私有子网，分别在两个可用区，正常情况下两副本各占一个可用区。

### 4. NetworkPolicy 出向白名单

选中 `app: llm-gateway` 的 Pod，只放行：

- 到 `10.0.0.0/8` 的 TCP 443（各 VPC Endpoint、peering 对端 VPCE）、TCP 9090（mock 控制面 Pod）、UDP/TCP 53（CoreDNS Pod）
- 到 `172.20.0.0/16`（集群 Service CIDR）的全部端口

公网地址（比如 `bedrock-runtime.us-west-2.amazonaws.com` 或 `sts.us-west-2.amazonaws.com` 的公网解析结果）一律丢弃。
换集群要核对 Service CIDR（`aws eks describe-cluster --query cluster.kubernetesNetworkConfig.serviceIpv4Cidr`）。
真实控制面在集群外的话，把它的地址加进白名单。

VPC CNI 标准模式下，新 Pod 启动头几秒策略尚未下发，这期间流量按 NodeClass 的 `networkPolicy` 默认值放行。网关是长驻进程
不受影响；用调试 Pod 验证时先 `sleep 20` 再测，否则会误判「策略没生效」。要在这几秒内也拒绝公网流量，把 NodeClass 的
`networkPolicy` 改成 `DefaultDeny`，代价是该节点上所有没被任何 NetworkPolicy 选中的 Pod 会被全部拒绝，需要给 mock 控制面等也补策略。

### 需要的 VPC 资源

| 资源 | 位置 | 用途 |
| --- | --- | --- |
| `com.amazonaws.<region>.bedrock-runtime` 接口端点，开私有 DNS | 网关 VPC | 本账号本区域的 Bedrock 调用 |
| `com.amazonaws.<region>.sts` 接口端点，开私有 DNS | 网关 VPC | IRSA 换凭证、跨账号 AssumeRole |
| `com.amazonaws.<region>.secretsmanager` 接口端点，开私有 DNS | 网关 VPC | 启动时读 `secretsmanager://` 引用的密钥。密钥全用明文方式时不需要 |
| `ecr.api`、`ecr.dkr` 接口端点 + `s3` 网关端点 | 网关 VPC | 节点拉私有 ECR 镜像 |
| `bedrock-runtime` 接口端点 | 对端区域 VPC | 跨区域 Bedrock 调用 |
| VPC peering，两侧路由表互指 | 两个 VPC | 东京私有子网路由表加 `10.1.0.0/16 → pcx`，对端加 `10.2.0.0/16 → pcx` |
| 端点安全组放行网关 VPC CIDR 的 443 | 各端点 | peering 流量保持源 IP，直接用对端 VPC CIDR 写规则 |

## 四、网关配置要点

```yaml
providers:
  bedrock:
    auth: aws_iam
    region: ap-northeast-1
    endpoints:
      anthropic:        "https://bedrock-runtime.ap-northeast-1.amazonaws.com/anthropic/v1"
      openai_chat:      "https://bedrock-runtime.ap-northeast-1.amazonaws.com/openai/v1"
      openai_responses: "https://bedrock-runtime.ap-northeast-1.amazonaws.com/openai/v1"
  bedrock-usw2-xacct:
    auth: aws_iam
    region: us-west-2
    sts_region: ap-northeast-1
    role_arn: arn:aws:iam::<bedrock-account>:role/<cross-account-role>
    endpoints:
      anthropic:        "https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/anthropic/v1"
      openai_chat:      "https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/openai/v1"
      openai_responses: "https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/openai/v1"
```

- **VPC Endpoint 的私有 DNS 只在同一 VPC 内生效。** 东京 Pod 解析 `bedrock-runtime.us-west-2.amazonaws.com` 得到的是公网 IP。
  经 peering 访问对端区域的 VPCE 必须写 VPCE 专属域名（`aws ec2 describe-vpc-endpoints` 的 `DnsEntries` 第一项），
  SigV4 对这个 Host 签名，Bedrock 照常接受。
- **STS 也要走私网。** SDK 默认把 STS 打到「签名区域」的端点，也就是 us-west-2 公网。`sts_region: ap-northeast-1` 把
  IRSA 换凭证和 AssumeRole 都固定到东京，命中东京 VPC 的 STS 端点。少了这项，NetworkPolicy 会把 STS 请求丢掉，网关启动失败。
- **启动日志核对身份。** 每个 `aws_iam` provider 启动时打 `aws identity resolved`，跨账号 provider 的 `arn` 应该是对端账号的
  `assumed-role/<role>/llm-gateway-<providerCode>`。

## 五、验证方法

1. 网关和控制面 port-forward 后跑 smoke，模型列表换成跨账号路由：

   ```bash
   kubectl port-forward -n llm-gateway svc/llm-gateway 8080:8080 &
   kubectl port-forward -n llm-gateway svc/mock-controlplane 9090:9090 &
   CLAUDE_MODELS="claude-sonnet-5-us claude-opus-5-us" GPT_MODELS="gpt-5.6-sol-us gpt-5.6-luna-us" \
     GW=http://localhost:8080 CP=http://localhost:9090 KEY=sk-demo-key scripts/smoke.sh
   ```

2. 起一个带同样标签、落同一节点池的探测 Pod，确认 DNS 解析到私网地址、VPCE 可达、公网被拒：

   ```bash
   kubectl run netprobe -n llm-gateway --labels=app=llm-gateway --restart=Never \
     --image=public.ecr.aws/amazonlinux/amazonlinux:2023 \
     --overrides='{"spec":{"nodeSelector":{"network-tier":"private"}}}' --command -- bash -c '
       sleep 25
       getent hosts bedrock-runtime.ap-northeast-1.amazonaws.com        # 期望 10.2.x
       getent hosts sts.ap-northeast-1.amazonaws.com                    # 期望 10.2.x
       getent hosts vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com   # 期望 10.1.x
       curl -s -o /dev/null -w "vpce %{http_code}\n" --max-time 15 https://vpce-<id>-<hash>.bedrock-runtime.us-west-2.vpce.amazonaws.com/anthropic/v1/messages   # 期望立即返回 4xx
       curl -s --max-time 8 https://checkip.amazonaws.com || echo "public blocked"   # 期望 blocked
     '
   kubectl logs -n llm-gateway netprobe; kubectl delete pod -n llm-gateway netprobe
   ```

3. `kubectl get policyendpoints -n llm-gateway -o yaml` 里 `podSelectorEndpoints` 应列出全部网关 Pod。

## 六、PoC 实测记录（2026-09-03）

环境：东京 EKS Auto Mode，网关 Pod 在私有子网节点池；peering 到 us-west-2 VPC；对端账号的跨账号角色。

| 路由 | `/v1/messages` | `/v1/chat/completions` | `/v1/responses` |
| --- | --- | --- | --- |
| 跨账号 us-west-2：Claude Sonnet 5 / Opus 5 / GPT-5.6 Sol / Luna | 200，流式与非流式 | 200，流式与非流式 | 401（对端角色缺 `project/default`） |
| 东京本账号同四模型 | 200 | 200 | 200 |
| `claude-sonnet-5-ha`（主跨账号 `priority 10`，备东京 `priority 20`） | 200，走主路 | | |

- 网关启动日志中跨账号 provider 身份为 `arn:aws:sts::<bedrock-account>:assumed-role/<cross-account-role>/llm-gateway-bedrock-usw2-xacct`。
- 所有成功调用的计量都带完整 token 数，`successful reports missing tokens: 0`。
- 探测 Pod：东京 bedrock-runtime、STS、us-west-2 VPCE 域名全部解析到 10.x；未签名请求打 us-west-2 VPCE 0.3 秒收到 400；
  `checkip.amazonaws.com`、公网 `bedrock-runtime.us-west-2.amazonaws.com`、公网 `sts.us-west-2.amazonaws.com` 全部超时被丢。
- Bedrock 上的 GPT 模型经 peering 从 us-west-2 VPCE 调用未触发调用方所在地限制（Pod 出口在东京区域内）。
- 同日压测结果（3M TPM、约 100 在途连接、GPT 模型在两条路由上的并发差异）见 [../loadtest/README.md](../loadtest/README.md)。
- Secrets Manager 路径：带 `app: llm-gateway` 标签、用网关 ServiceAccount 的探测 Pod 里，`secretsmanager.ap-northeast-1.amazonaws.com`
  解析到两个私有子网的 10.2.x 地址（新建的 secretsmanager 接口端点），`GetSecretValue llm-gateway/poc` 以 IRSA 角色身份成功，
  NetworkPolicy 无需改动（443 到 10/8 已放行）。随后 v0.4.0 上线，两副本启动日志各一条 `secret resolved`（同一 `version_id`），
  容器无任何密钥环境变量，集群内冒烟 17 项全部通过。

## 七、拆除 PoC

```bash
kubectl delete -f deploy/k8s/          # 含 NodePool，Auto Mode 会回收私网节点
eksctl delete cluster --name llm-gateway-poc --region ap-northeast-1
aws iam delete-policy --policy-arn arn:aws:iam::<account>:policy/llm-gateway-poc-bedrock-invoke   # 先删非默认版本
aws iam delete-policy --policy-arn arn:aws:iam::<account>:policy/llm-gateway-poc-secrets-read
aws secretsmanager delete-secret --region ap-northeast-1 --secret-id llm-gateway/poc --force-delete-without-recovery
aws ec2 delete-vpc-endpoints --region ap-northeast-1 --vpc-endpoint-ids <secretsmanager 端点 id>   # 本项目建的，带 Project=llm-gateway-poc 标签
```

VPC、子网、VPC Endpoint、peering、NAT 如果是复用其他系统的资源，不应一并删除。PoC 里只有 secretsmanager 端点是本项目
在共用 VPC 里新建的（按 `Project=llm-gateway-poc` 标签识别），其余 VPC 层资源都是借用的。
