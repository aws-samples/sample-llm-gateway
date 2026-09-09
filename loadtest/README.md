# 压测

用 k6 在集群内直接打网关 Service，上游是真实 Bedrock。两个脚本：

| 文件 | 用途 |
| --- | --- |
| `k6-bedrock.js` | 混合流量（Anthropic messages 流式 + OpenAI chat 流式），按到达速率控节奏：30 秒爬坡、稳态 `HOLD`、最后 1 分钟冲到 `OVER_RPS` |
| `k6-single-model.js` | 单模型并发梯度（固定 VU 数逐级抬高），每个请求打印状态与首字节时间，用来定位某个模型从哪个并发开始变慢 |
| `job.yaml` | 运行 `k6-bedrock.js` 的 Job：私有节点池、与网关反亲和、直接打 Service |

每个请求约 2000 个输入 token（8000 字符英文），前缀带随机串防止 prompt cache 美化数据。

## 运行

```bash
kubectl -n llm-gateway create configmap k6-script --from-file=loadtest/k6-bedrock.js --from-file=loadtest/k6-single-model.js
kubectl apply -f loadtest/job.yaml          # 改 env 里的 TARGET_RPS / HOLD / GPT_SHARE / MAX_OUTPUT_TOKENS
kubectl -n llm-gateway logs -f job/k6-loadtest
```

结果看三处：k6 汇总（`http_req_failed`、`llm_ttfb_ms`、`dropped_iterations`）、网关 `/metrics`
（`llmgw_requests_total`、`llmgw_ttft_seconds`、`llmgw_metering_reports_total`）、`kubectl top pods`。
`dropped_iterations` 非零说明 VU 被慢请求占满，到达速率没打满，通常是上游变慢而不是网关问题。

Bedrock 侧对照用 CloudWatch `AWS/Bedrock` 命名空间、维度 `ModelId=<inference profile>`：`Invocations`、
`InvocationThrottles`、`TimeToFirstToken`、`InputTokenCount`。网关看到的首字时间与 Bedrock 上报的
`TimeToFirstToken` 接近，说明等待发生在 Bedrock 内部；差距大才是网关或网络问题。

## 2026-09-03 PoC 结果

环境：东京 EKS Auto Mode，网关 2 副本（requests 100m CPU / 128Mi），mock 控制面 1 副本，全部私网走 VPC Endpoint。

**Anthropic messages 流式，claude-sonnet-5，22 rps 稳 3 分钟后冲 33 rps 1 分钟**

| 项目 | 结果 |
| --- | --- |
| 请求数 / 失败 | 4814 / 0 |
| 峰值分钟 | 1303 请求，307 万 token（Bedrock CloudWatch：输入 291 万、输出 15 万），无限流 |
| 首字（网关到上游） | p50 1.44 s，p90 1.71 s，p99 2.72 s，最大 5.3 s |
| 端到端 | p50 2.58 s，p99 4.04 s |
| 在途连接峰值 | 94 |
| 网关资源 | 每副本 CPU 峰值 52m / 58m，内存 18Mi；所在节点 CPU 9% |
| 计量 | 队列深度始终 0，无 dropped，控制面收到全部记录 |
| 控制面 key-auth | 33 rps 下 mock 控制面 CPU 18m，`llmgw_keyauth_errors_total` 为 0 |

**OpenAI chat 流式，gpt-5.6-sol，并发梯度 1 → 5 → 20 → 50**

| 并发 | 吞吐 | 首字 p50 / p90 | 说明 |
| --- | --- | --- | --- |
| 1 | 0.13 req/s | 5.7 s / 15.2 s | |
| 5 | 0.42 req/s | 4.8 s / 11.4 s | |
| 20 | 1.0 req/s | 4.1 s / 120 s | 17 个请求撞到客户端 120 秒超时 |
| 50 | 0.69 req/s | 5.5 s / 60 s | 最大 82 s |

Bedrock 侧同一时段 `TimeToFirstToken` 平均 69 s、最大 115 s，`InvocationThrottles` 为 0，每分钟完成数随并发上升从 40 降到 13。
等待发生在 Bedrock 内部，Bedrock 对这个模型排队而不是返回 429，所以网关按 429 / 5xx 触发的故障转移不会发生。

同一模型码走另一账号的 us-west-2（`gpt-5.6-sol-us`，跨账号 AssumeRole + VPC peering + VPCE）20 并发 2 分钟：
387 个请求全部 200，3.2 req/s，首字 p50 2.0 s、p90 5.2 s、p99 12.4 s。GPT 的排队是账号或档位层面的容量差异，不是网关或网络。

混合流量首轮（70% Anthropic + 30% GPT，20 rps）因 GPT 一路把 VU 占满而未打到目标速率，Anthropic 部分 3344 个请求全部 200。
