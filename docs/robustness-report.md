# 健壮性测试报告

测试日期 2026-09-03，被测版本 v0.4.0（commit c667e3c）。目标是回答一个问题：这套网关放进高压生产环境，在控制面、上游、客户端三方各自出故障时，行为是否可预期，会不会阻塞、泄漏、丢计量。

## 0. 处理结果（同日，v0.5.0）

| 问题 | 决定 | 落地 |
| --- | --- | --- |
| A 选路随机数并发写 | 修 | `router.go` 改用包级 `rand.IntN`，单候选不抽签；CI 的 unit-test 加 `-race` |
| B 断线 / 截断时计量少报 | 客户确认不计费 | v0.5.1：没跑完的流上报 499（客户端断开）/ 504（`request_timeout`）/ 502（上游中断），token 全 0，日志加 `truncated` 字段 |
| C 读 body 无时限 | 修 | 新增 `server.read_timeout`，默认 60 s |
| D 空路由表照单全收 | 修 | `Poller.Sync` 拒收「已有非空快照时的空表」，不推进 ETag，计 `llmgw_routes_rejected_total` |
| E 退出预算 | 修 | Pod 宽限期 300 s；SIGTERM 后 `/readyz` 先置 503 等 5 s，再等在途请求 `server.shutdown_timeout`（默认 280 s），计量刷写单独 10 s |
| F 控制面长故障下计量丢弃 | 先放着 | 保持无状态，靠 `dropped` 告警与 Bedrock 侧对账 |
| G 32 MiB body 与内存 | 保持上限 | 有图片需求，内存 limit 提到 1Gi，换算关系写进 `docs/configuration.md` |
| H key-auth 熔断 | 不加 | 控制面挂起属于需要人介入的事故，不为它增加状态 |
| I 截断流像正常结束 | 文档 | README 已知限制写明靠 `message_stop` / `[DONE]` / `response.completed` 判断完整性 |

修完后 `go test -race ./... -count=1` 全部通过，慢发 body 的场景改为验证 `read_timeout` 生效，空路由表守卫在 `internal/router/poller_test.go` 单独覆盖。

v0.5.0 已发到东京 PoC 集群：`scripts/smoke.sh` 17 项全过、13 条计量 token 无缺失；删 Pod 实测 `/readyz` 在 1 秒内变 503，第 5 秒关监听，日志 `shutting down`（`drain_delay=5s`、`shutdown_timeout=4m40s`）到 `bye` 间隔 5 秒；`/metrics` 里已有 `llmgw_routes_rejected_total`。

v0.5.1（不计费口径）同日上线：冒烟 17 项全过；用 `curl --max-time 2` 打一个 1500 token 的流式请求模拟客户端中途断线，客户端收到 1 条 delta 后断开，mock 控制面收到的记录为 `status_code 499`、token 全 0，网关日志 `truncated=client_disconnect`。

## 1. 测试方法

新增两个测试文件，全部可离线重复运行，不依赖 AWS 账号。

| 文件 | 内容 |
| --- | --- |
| `internal/proxy/robustness_test.go` | 一个可注入故障的环境：控制面可设延迟、挂起、返回任意状态、前 N 次上报失败；上游可设首字延迟、5xx、拒连、永不结束、原始字节回放；客户端可中途断线、慢发 body。16 个场景 + 1 个吞吐探针 |
| `internal/router/router_race_test.go` | 路由热更新与并发选路同时进行 |

运行方式：

```bash
go test -race ./... -count=1                                     # 全部故障场景，约 25 秒
ROBUST_LOAD=1 go test ./internal/proxy/ -run LoadCeiling -v      # 本机吞吐探针，约 7 秒
```

`-race` 不能省。本轮最重要的一个问题只有竞态检测器能发现，CI 现在的 `go test ./... -count=1` 不带这个开关，全部通过。

## 2. 结论

最值得注意的两个结果都不在预设的故障场景里，而是测试跑起来之后才冒出来的。一个是选路代码里有一处并发写，每个请求都会触发，只有竞态检测器能看见，现有 CI 一直是绿的；另一个是客户端断线时计量少报，这不是 bug，是流式协议把 usage 放在流末尾的固有限制，但它直接影响客户的计费口径，需要客户拍板。

预设的故障场景本身没有意外：控制面不可达时秒级 fail-closed，上游慢或坏时按信号切换，计量队列打满不拖慢请求，路由热更新期间零错误，优雅退出能等在途流式请求跑完。本机吞吐探针里单进程非流式 1.5 万 rps 以上、流式 7 千 rps 以上，6 万条计量零丢，goroutine 无增长。

需要处理的问题共 9 个，除上面两个之外，其余是超时边界、退出预算、容量上限这类生产加固项。详见第 4 节。

## 3. 场景与结果

| # | 场景 | 注入的故障 | 期望 | 结果 |
| --- | --- | --- | --- | --- |
| 1 | 加权选路并发 | 同优先级 3 个 provider 权重 10/20/30，300 并发 | 全 200，分布按权重，每条计量 | 分布 61/90/149，计量 300/300；**竞态检测器报警**（问题 A） |
| 2 | 控制面拒连 | 关闭控制面端口 | 每请求 503，最慢不超 500 ms，上游零调用，无泄漏 | 通过，100 请求全部 503，无泄漏 |
| 3 | 控制面挂起 | key-auth 睡 5 s，`key_auth_timeout` 300 ms | 每请求 ≈ 300 ms 后 503 | 通过，全部落在 280 ~ 900 ms |
| 4 | 上游首字超时 | 主 provider 首字 3 s，`upstream_response_header_timeout` 300 ms | 切备份，总耗时 ≈ 超时值，主 provider 收到取消 | 通过，310 ms |
| 5 | 上游拒连 | 主 provider 指向已关闭端口 | 立即切备份 | 通过，10 ms |
| 6 | 全部候选失败 | 主 503、备份 529 | 最后一个候选的状态与 body 原样透传，计量带 529；单候选时 5xx 不重试 | 通过 |
| 7 | 流式超过 request_timeout | 上游每 50 ms 一条 delta 且永不结束，`request_timeout` 700 ms | 在 700 ms 处切断，上游收到取消，仍上报 | 通过；上报 output_tokens=1（问题 B），客户端看到的是正常结束的响应（问题 I） |
| 8 | 客户端中途断线 | 读到第 3 条 delta 后挂断 | 取消传到上游，仍上报 | 通过；上报 output_tokens=1（问题 B） |
| 9 | 计量队列打满 | 队列 4、worker 1、上报延迟 200 ms、40 个请求 | 请求路径不变慢，超出部分计 dropped，队列水位归零 | 通过，最慢请求 < 150 ms，ok=5 dropped=35 |
| 10 | 上报被 401 拒绝 | 控制面上报接口回 401 | 每条只试一次，不重试 | 通过 |
| 11 | 上报接口抖动 | 每条前 2 次 503 | 退避重试后全部送达，request_id 不变 | 通过，8/8，每条 3 次 |
| 12 | 请求体边界 | 恰好达上限、超 1 字节、超 4 MiB、空、截断 JSON、数组、model 非字符串 | 413 / 400，上游零调用 | 通过 |
| 13 | 慢发 body | body 拖 1.2 s 发完，`request_timeout` 400 ms | 记录现状 | 请求成功返回 200，说明读 body 阶段没有时间上限（问题 C，已修：`read_timeout` 生效后同样的请求被拒） |
| 14 | 路由热更新 | 200 个请求在途期间切换快照 100 次；随后下发空路由表 | 零错误；记录空表行为 | 零错误；空表被接受，所有模型立刻 404（问题 D，已修：守卫拒收）；**竞态检测器报警**（问题 A，已修） |
| 15 | SSE 边角 | CRLF 行尾、注释行、`data:` 后无空格、多行 data、1 MiB 单行、结尾无空行 | 字节级一致透传，usage 解析到 | 通过，input 11 / output 42 |
| 16 | 优雅退出 | 在途流式请求期间调用 Shutdown | 请求跑完收到 message_stop，Shutdown 之后拒新连接，队列里的计量送达 | 通过 |
| 17 | 吞吐探针 | 256 并发，非流式 2 万 × 2 轮，流式 1 万 × 2 轮 | 零失败，两轮之间 goroutine 不增长 | 见第 5 节 |

## 4. 发现的问题

### A. 选路随机数生成器被所有请求并发写（必修）

位置 `internal/router/router.go:121`。`Router` 持有一个 `*rand.Rand`，`math/rand/v2` 的 `*rand.Rand` 不保证并发安全，而 `Attempts` 在每个请求 goroutine 里调用，`weightedOrder` 即使只有一个候选也会调一次 `IntN`。所以生产里每一个请求都在竞争写这个生成器，不只多候选场景。

影响：不会 panic，`IntN` 的结果永远在范围内。但 Go 内存模型下这是未定义行为，实际表现是权重分布可能失真，而且任何带 `-race` 的测试都会失败。CI 目前不带 `-race`，所以一直没暴露。

修法：删掉 `rng` 字段，改用包级 `rand.IntN`（`math/rand/v2` 的包级函数是并发安全的）；单候选时直接返回不必抽签。CI 的 unit-test job 加 `-race`。

### B. 客户端断线或超时截断时，计量少报输出 token（要与客户对齐口径）

场景 7、8 里，网关把取消传给上游（这是对的，否则上游会一直生成到 max_tokens），但此时 Anthropic 流的 `message_delta` 还没到，上报的 `output_tokens` 是 `message_start` 里的初值 1。Bedrock 按实际已生成的 token 计费，控制面收到的却是 1，并且 `status_code` 是 200，与正常完成的请求无法区分。OpenAI 流也一样，usage 在最后一个 chunk 里。

这不是实现错误，是流式协议的固有限制：usage 只在流末尾出现，断在中间就拿不到。有一个看起来能拿到准确值的做法：客户端断了之后网关不取消上游，继续把流读完。它被排除的原因是上游并不知道客户端已经走了，会一直生成到 max_tokens，为了一个准确的计数多花几倍的 token，得不偿失。剩下能做的是把这类记录标出来，让控制面知道它是不完整的。客户的决定是这类请求不计费，于是按契约里失败记录的样子上报：客户端断线 `status_code` 499（nginx 对「客户端关闭请求」的约定），request_timeout 截断 504，上游连接中途断掉 502，token 全 0。判定信号是上游流是否以正常 EOF 结束、客户端是否收下了全部内容，不靠猜 usage 有没有到。供应商按实际生成量收费的差额由运营方承担。

### C. 读请求体不受 request_timeout 约束

`handler.go` 里 `context.WithTimeout` 在 `io.ReadAll` 之后才创建，`http.Server` 只设了 `ReadHeaderTimeout: 10s`，没有 `ReadTimeout`。一个发完请求头后慢慢滴 body 的连接可以无限期占住一个 goroutine 和一份 body 缓冲。场景 13 用 400 ms 的 request_timeout、拖 1.2 s 的 body 验证了这一点。

修法：`http.Server` 加 `ReadTimeout`（例如 60 s）。它只约束读请求这一段，不影响后面几分钟的流式响应写出；或者在读 body 前用 `http.NewResponseController(w).SetReadDeadline` 设一个截止时间。

### D. 空路由表照单全收

控制面若因自身故障返回 `{"models":[]}` 且状态 200，网关会把空快照换上去，所有模型立刻 404，持续到下一次轮询拿到正确数据。场景 14 验证了这个行为。

建议在 `Poller.Sync` 加守卫：当前快照非空而新快照为空时拒绝加载，记 error 日志并计数，保留旧快照。真要清空全部路由的运维操作极少见，可以通过重启网关完成。

### E. 优雅退出的时间预算与 request_timeout 不匹配

`main.go` 里 `srv.Shutdown` 与 `meter.Shutdown` 共用同一个 30 s 的 context；Pod 的 `terminationGracePeriodSeconds` 用默认 30 s（`deploy/k8s/05-network.yaml` 里的 5m 是 NodePool 的节点排空时间，不是 Pod 的）；`request_timeout` 默认 10 分钟。三者叠在一起的后果：

- 最要紧的一条：滚动更新时，跑了超过 30 s 的流式请求会被 SIGKILL 切断。LLM 长输出场景 30 s 很常见，每次发版都会切掉一批用户正在读的回答。
- 其次，如果 HTTP 排空用满 30 s，计量刷写拿到的是一个已经过期的 context，队列里的记录直接丢。
- 影响最小但最容易修的：没有 preStop 钩子，Shutdown 关监听与 Endpoint 摘除之间有几秒窗口，会出现连接拒绝。

建议：`terminationGracePeriodSeconds` 提到 120 ~ 300 s，与业务上能接受的最长单次请求对齐；收到 SIGTERM 后先把 `/readyz` 置 503、等 5 s 再 `Shutdown`；计量刷写单独给 10 s 预算，不与 HTTP 排空共用。

### F. 控制面长时间故障下计量的容量上限

现有参数：队列 10000，4 个 worker，每条最多重试 5 次，单次超时 5 s，退避 0.2 → 3.2 s。控制面挂起（超时而非快速失败）时，一条记录最坏占用一个 worker 36 s，4 个 worker 合计每秒只能处理 0.11 条。队列在 30 rps 下约 5.5 分钟打满，300 rps 下约 33 s，之后每条新记录直接丢，直到控制面恢复。丢弃有 `llmgw_metering_reports_total{result="dropped"}` 计数和 error 日志，但没有落盘或死信队列，被丢弃的记录无法找回。

这是设计取舍（网关无状态、不引入本地存储），需要客户知情。可做的加固：控制面故障时缩短重试（例如首次超时后直接放弃、由队列保住吞吐）；或者加一个小容量的本地文件溢出区。对 `dropped` 指标配告警是最低要求。

### G. 请求体上限与内存限制的关系

`max_request_body_bytes` 默认 32 MiB，Pod 内存 limit 512 Mi。body 在 key-auth 之前就完整读入内存（因为 key-auth 需要 body 里的 model 字段），所以持有任意一个 bearer 字符串、能连到 Service 的客户端，用十几个并发的最大体积请求就能把 Pod 打到 OOM。Service 是私网 ClusterIP，暴露面有限，但内网里的误用或压测脚本同样会触发。

建议：没有多模态大附件需求时把上限降到 4 ~ 8 MiB；有需求则相应抬高内存 limit，并在文档里写明两者的换算关系。

### H. 控制面挂起时没有熔断

key-auth 每个请求都实时调控制面，没有缓存也没有熔断。控制面挂起时每个请求都要等满 `key_auth_timeout`（默认 2 s）才 503，1000 rps 下会有 2000 个 goroutine 同时在等，客户端感受到的是 2 s 延迟后失败，而不是立刻失败。场景 3 验证了单个请求的行为是对的，但没有验证高并发下的堆积。

fail-closed 本身是需求，不建议改。可以加一个简单的熔断：连续 N 次超时后，在接下来 T 秒内直接 503，不再发请求。另外，因为每个请求都要实时调一次 key-auth，控制面这个接口的容量必须与网关吞吐同量级，容量规划时要明确写出。

### I. 超时截断的流对客户端像正常结束

场景 7 里，网关在 request_timeout 处停止转发，Go 的 chunked 编码会正常收尾，客户端收到的是一次正常的连接结束，没有错误事件。Anthropic 客户端可以靠缺少 `message_stop` 判断不完整，OpenAI 客户端靠缺少 `[DONE]`，但都要客户端自己实现。

第一阶段不做协议转换是既定决定，网关不应向流中插入自行生成的事件。这里只是记录，建议写进给接入方的说明。

## 5. 本机吞吐探针

环境：Apple Silicon 10 核，上游与控制面都是同进程内的 fake（立即响应），256 并发。数字表示网关自身转发路径的开销量级，不是绝对上限，因为 fake 与网关在抢同一台机器的 CPU。

| 轮次 | 请求数 | 吞吐 | p50 | p95 | p99 | 最大 | 堆内存 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 非流式 第 1 轮 | 20000 | 15773 rps | 14.6 ms | 23.6 ms | 67.0 ms | 85 ms | 16 → 62 MiB |
| 非流式 第 2 轮 | 20000 | 18031 rps | 13.4 ms | 20.8 ms | 26.7 ms | 31 ms | 47 → 64 MiB |
| 流式 4 delta 第 1 轮 | 10000 | 7097 rps | 34.6 ms | 47.2 ms | 58.5 ms | 65 ms | 55 → 91 MiB |
| 流式 4 delta 第 2 轮 | 10000 | 6744 rps | 36.6 ms | 51.1 ms | 58.8 ms | 81 ms | 54 → 93 MiB |

6 万条计量全部送达，dropped 为 0。两轮之间 goroutine 数 1576 → 1574、1568 → 1568，没有增长；这个水位是网关到上游的连接池（`MaxIdleConnsPerHost` 256）加 fake 服务端的连接，90 s 空闲后回收。

对照 2026-09-03 的集群压测（`loadtest/README.md`）：33 rps 下每副本 CPU 峰值 58m、内存 18 Mi。两组数据放在一起看，瓶颈在上游模型容量与控制面 key-auth 容量，不在网关本身。

## 6. 建议的处理顺序

1. 修问题 A，CI 加 `-race`。改动小，风险低。
2. 与客户确认问题 B 的口径（499 / 504），以及问题 F 的丢弃策略是否可接受。
3. 问题 C、D、E 一起做一版加固：`ReadTimeout`、空路由守卫、退出预算与 preStop。
4. 问题 G、H 按客户的流量规划决定参数，写进 `docs/configuration.md` 的容量说明。
