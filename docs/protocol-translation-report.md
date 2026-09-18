# 协议转换：实施与验证报告

实施日期 2026-09-17 至 09-18，被测版本为分支 `gh/feat-protocol-translation`（基线 GitHub `main` `e2351e6` + PR #1，末次 commit `c5b0f53`）。
本文回答"做了什么、怎么做的、怎么测的、踩了哪些坑、最后结果如何"。设计本身（IR 结构、字段映射表、决策依据）在
[protocol-translation-design.md](protocol-translation-design.md)，使用方式在 README「Protocol translation」与
[control-plane.md](control-plane.md) / [operations.md](operations.md)，这里不重复。

## 0. 结论

- OpenAI Chat Completions / OpenAI Responses / Anthropic Messages **六个方向全部实现**，由路由候选的可选字段 `providerProtocol` 触发；不带该字段的路由走原来的透传代码路径，零回归。
- 验收场景 **Claude Code → GPT** 与 **Codex → Claude** 在 Amazon Bedrock（us-east-1）上用官方客户端多轮工具调用跑通；另外四个方向也全部用真实客户端跑通（其中 Chat 侧客户端为官方 `openai` SDK，原因见 §4.3）。
- 计量口径与透传一致：始终用上游协议的解析器读原始上游字节，不依赖转换代码。
- 真机测试暴露了 3 个单元测试栈发现不了的问题（mock 控制面丢字段、客户端可见 usage 与账单拆分不一致、客户端先断连导致 499 误判），全部修复并加回归测试。
- 单元测试：`translate` 包 45 个、`proxy` 包新增 8 个（六方向 × 流式/非流式 + 错误路径），全部基于真实录制流量；`go test -race ./...` 全绿。

## 1. 需求与范围

Odin 的原话（2026-09-17）："LLM Proxy 这块有个新的 feature：做一个协议转换，适配现在三种主流 llm 协议 chat/completion、v1/messages 和 response，支持几个协议互转。这块挺容易出 bug 的，做完了测试一下通过这个网关让 claude code 访问 gpt，然后 codex 访问 claude。"

范围决策过程：

| 问题 | 备选 | 最终 | 依据 |
| --- | --- | --- | --- |
| 2 向还是 6 向 | 只做两条验收方向 / 6 向并行 / 6 向但先 2 向 E2E | **6 向，先把 2 条验收方向端到端打通再铺剩余 4 向，同一 PR 交付** | 两位外部评审都建议"先 2 后 4"；Odin 的措辞是"支持几个协议互转"，重点测两条而非只做两条 |
| 目标协议怎么声明 | 请求头 / 环境变量 / provider 单端点推断 / 路由候选字段 | **路由候选 `providerProtocol`，缺省 = 透传** | 与仓库"控制面下发路由、provider 声明 endpoints"模型一致，不引入隐式开关 |
| 架构 | 每对协议写转换器（O(N²)）/ 规范中间表示 | **IR 中枢**：入站 → IR → 目标，18 个适配函数覆盖 6 向 | LiteLLM 等成熟网关的做法 |
| reasoning | 一期不做 / 完整转换 / 不透明 round-trip | **一期做不透明 round-trip** | 录制证明 Claude Code 每轮都回传带签名的 thinking 块，不带就 400；跨家族转换没有正确定义 |
| 缺 `max_tokens` 补多少 | 4096（LiteLLM 默认）/ 8192 / 按模型 | **8192，可配** | 仓库原先纯透传没有这个概念；agentic 客户端大 diff 在 4096 处会以正常的 `stop_reason: max_tokens` 静默截断 |
| 是否逐条问 Odin | 问 / 按主流实践定 | **按主流实践与仓库惯例定，只在 PR 里知会"推翻了不做协议转换的书面边界"** | 用户决定 |

吸收的评审意见（两位 AI 评审，M2 阶段）：handler 的开关必须收敛到 `translate.Supported()` 一处；`tool_choice`、流式事件 `Index`、`parallel_tool_calls`、`response_format` 一期就要进 IR；`max_tokens` 默认调高；golden fixture 必须真实抓包不手写；跨协议 usage 抽取用真实端点回归；先确认 Codex 是否依赖 `previous_response_id`。全部采纳。

## 2. 执行过程

九个里程碑，每个一个 commit，每步 `go build / go vet / go test -race ./...` 全绿后才进下一步。

| # | 内容 | 验证方式 | commit |
| --- | --- | --- | --- |
| M1+M2 | 设计文档；`translate` 子包 IR 类型 + codec 接口 + `Supported()`（恒 false）；路由候选 `providerProtocol` 透传到 `Candidate`；handler 分叉点 | 4 个 handler 测试：透传不变、跨协议未支持时拒且不调上游、非法值拒 | `9796f4c` |
| M3 | `tools/recordproxy` 录制工具；透传模式录 Claude Code → Claude（3 轮）、Codex → GPT（2 轮）真实流量作 fixture | 逐项剥离请求体字段实测 Bedrock 接受/拒绝清单（§4.1） | `c8c5023` |
| M4a | Anthropic ↔ Chat 请求与非流式响应 codec | 11 个 golden 测试（fixture 驱动，含 round-trip） | `610ab29` |
| M4b | Anthropic ↔ Chat 流式状态机 | 6 个测试，真实 SSE 逐事件断言 | `5d799a2` |
| M5 | Responses ↔ Anthropic 请求/响应/流式 codec | 9 个测试；两处 round-trip 失败修正（§4.2 P7、P8） | `6282541` |
| M6 | `Translator` 门面；handler 接线（请求转换、响应/流重渲染、错误重渲染、截断分类）；`server.default_max_tokens`；两项指标；`Supported()` 打开 Anthropic 相关 4 向 | proxy 层 7 个测试 + translate 门面 5 个测试 | `8383443` |
| M7 | 真实客户端 E2E：Claude Code → GPT、Codex → Claude | 真 Bedrock；发现并修 3 个问题（§4.2 P9–P11） | `bbe0bdf` |
| M8 | 录制官方 openai SDK 的 Chat 流量；Chat ↔ Responses 8 个 golden 测试；`Supported()` 全开；剩余 4 向 handler 测试与真实客户端 E2E | 全部通过 | `0191395` |
| M9 | README / control-plane / operations / configuration / 设计文档 / 包注释；撤销"不做协议转换"声明 | 无代码行为变化 | `c5b0f53` |

代码量：`translate` 包 10 个源文件约 3300 行；`proxy/handler.go` +199/−35；测试约 2600 行；fixture 20 个文件。

## 3. 测试方法

三层，由内向外。

### 3.1 codec 层 golden 测试（`internal/protocol/translate/*_test.go`，45 个）

全部输入来自真实录制流量，**没有手写的请求体或 SSE**（评审强调的最高杠杆项）：

| fixture 目录 | 来源 | 内容 |
| --- | --- | --- |
| `claude-code/` | 官方 Claude Code 2.1.274 → 透传 → Bedrock Claude Sonnet 5 | 01 纯文本；02 thinking → tool_use(Read)；03 assistant 回传 thinking+tool_use，user 回传 tool_result。请求头、请求体、完整 SSE |
| `codex/` | Codex CLI 0.154.0 → 透传 → Bedrock GPT-5.6 Sol（Responses） | 01 function_call（38 个参数增量片段）；02 function_call_output 回传 |
| `openai-sdk-chat/` | 官方 openai Node SDK 7.18 最小 agentic 循环 → 透传 → Bedrock GPT-5.6 Sol（Chat） | 01 tool_calls 流；02 tool 结果回传后的文本流 |
| `bedrock-gpt-chat-tool-call.response.sse` | Bedrock Chat 端点 | 带工具调用的 Chat SSE |

覆盖：每个方向的请求 ToIR / FromIR、非流式响应双向、流式解码 → 编码；IR round-trip 结构不变（Anthropic、Responses、Chat→Responses、Responses→Chat）；usage 跨协议 round-trip 数值不变；tool_choice / system 位置 / 默认 max_tokens / 无效工具参数保留 / 签名不泄漏 / 帧格式不泄漏。

### 3.2 handler 层（`internal/proxy/translation_test.go`，8 个用例含 8 个子用例）

fake 上游同时扮演三种协议（流式与非流式），经完整 handler 链路：六方向 × 流式/非流式各一项，断言客户端信封、上游体（证明目标 codec 跑了）、请求头过滤、计量报告的 token 拆分；另有上游错误体重渲染、不可转换请求 400、配置覆盖默认 max_tokens。

### 3.3 真实客户端 E2E（真 Bedrock）

本地栈：mock 控制面 `:9090` → 网关 `:8080`（`aws_iam` 直连 `bedrock-runtime.us-east-1.amazonaws.com`）→ 前面挂 `recordproxy :8081` 录制客户端侧流量。路由用 mock 控制面的 `PUT /debug/routes` 下发。客户端：

- Claude Code：`/opt/homebrew/bin/claude -p ... --allowedTools Read`，`ANTHROPIC_BASE_URL=http://127.0.0.1:8081`、`ANTHROPIC_MODEL=<modelCode>`，独立 `CLAUDE_CONFIG_DIR`。
- Codex：`codex exec --skip-git-repo-check ...`，`CODEX_HOME` 指向一份 `model_provider` 为网关、`wire_api = "responses"` 的 config.toml。
- openai SDK：`/tmp/llmgw/chat-agent/agent.mjs`（不进仓库）——流式 + 一个 `exec_command` 工具 + 结果回传直到模型给出答案。

任务统一为"用工具读一个文件并说出行数"，能一次覆盖：首问 → 模型发工具调用 → 客户端回传结果 → 模型作答，即一个完整 agentic 循环。

## 4. 踩坑与处理

### 4.1 录制阶段（M3，透传模式）

| # | 现象 | 根因 | 处理 |
| --- | --- | --- | --- |
| P1 | Claude Code 透传打 Bedrock Claude **100% 400** | Bedrock 原生 Anthropic 端点拒绝：`metadata.user_id` 含 `{ " :` 字符；`output_config.format`（结构化输出 beta）；`anthropic-beta: prompt-caching-scope-2026-01-05`。逐项剥离实测，其余 7 个 beta 头、`thinking: adaptive`、`max_tokens: 64000` 都接受 | 定性为"Bedrock 供应商适配"而非转换问题。只在 `recordproxy -bedrock-compat` 里清洗以便录制，**未进网关**（透传路径不改，见 §5） |
| P2 | Codex 透传打 Bedrock GPT 400 `web search is not supported`；openai SDK 带工具打 Chat 端点 400 `Function tools with reasoning_effort are not supported` | Bedrock Responses 端点拒 `web_search` 工具类型；Chat 端点上 GPT-5.x 带工具必须 `reasoning_effort: "none"` | 转换路径由 codec 处理（hosted 工具丢弃；Chat FromIR 有工具时写 `reasoning_effort: none`）。透传路径仍需客户端满足 |
| P3 | 评审担心 Codex 依赖 `previous_response_id`，无状态 IR 会卡死场景 2 | 录制证明 Codex 发 `store: false`、`previous_response_id: null`、每轮全量 `input` | 证伪；IR 保持无状态，`previous_response_id` 返 400 |
| P4 | 内部 toolbox 版 Claude Code 完全忽略 `ANTHROPIC_BASE_URL`，流量直接出去了 | 该发行版固定走内部网关 | 改用 `/opt/homebrew/bin/claude`（官方版），并用独立 `CLAUDE_CONFIG_DIR` 隔离配置 |
| P5 | 录不到 Responses 的 `reasoning` item | Bedrock Responses 端点（gpt-5.6-sol）即使 `reasoning.effort=medium` 也不返回 reasoning item / `encrypted_content` | Responses 侧 reasoning 解析按 OpenAI 官方规范实现；round-trip 用 Claude Code 的 thinking fixture 验证 |
| P6 | recordproxy 录不到响应 | agentic 客户端收到终止事件立即断连，`httputil.ReverseProxy` 以 `panic(http.ErrAbortHandler)` 中止 handler | 落盘放 `defer` |
| P16 | Anthropic `messages[]` 里出现 `role: system` | Claude Code 用 `mid-conversation-system` beta | 设计文档 §3 对照表修正；IR `RoleSystem` 允许出现在 Messages 中，目标协议不支持时降级 |

### 4.2 实现阶段（M5–M8）

| # | 现象 | 根因 | 定位方式 | 处理 |
| --- | --- | --- | --- | --- |
| P7 | Responses round-trip 测试失败：System 块数变了 | 把 `instructions` 和 `input` 里的 `developer` item 都塞进 IR System 并 `joinText` 合并 | 对比 round-trip 前后 IR JSON | `instructions` → System；`developer/system` item 留 Messages（RoleSystem）逐 block 渲染；Anthropic FromIR 只把开头连续的 RoleSystem 提升为顶层 `system` |
| P8 | Responses round-trip 失败：`ToolChoice.Mode` 从 `auto` 变 `""` | Codex 显式发 `tool_choice: "auto"`，FromIR 因是默认值而省略 | 临时测试打印首个差异字节 | 三协议 ToIR 把显式 `auto` 归一为零值，FromIR 统一省略。三家省略 == auto，且 Anthropic 无 tools 带 tool_choice 会报错 |
| P9 | M6 接完线后 curl 打转换路由，真 Bedrock 回 404 "model doesn't exist or doesn't support this API"；透传同一模型 ID 正常；转换后的请求体单测里完全正确 | 起一个只回显的假上游 + 第二个网关实例，看到**路径仍是入站协议的**、体只改了 model —— 说明 `providerProtocol` 到 handler 时是空的。追到 mock 控制面的 `providerRoute` 结构体没有该字段，反序列化即丢 | 回显假上游 | mock 控制面补字段。真实控制面按契约下发不受影响，但本地/集群 mock 必须有 |
| P10 | Claude Code → GPT 一轮里，客户端收到 `input_tokens: 14331 / cache_creation: 0`，而计量记 `input 2 / cache_write 14329`；总数一样、拆分不同 | Chat 流解码器把 usage 解进只有 `cached_tokens`/`reasoning_tokens` 的类型化结构再序列化，丢了 Bedrock 非标 `prompt_tokens_details.cache_write_tokens`；计量走的是原始字节 | 对照网关日志与 recordproxy 录到的客户端侧 `message_delta` | usage 保持 `json.RawMessage` 原样交给 `protocol.ParseUsage`，客户端与账单走同一解析路径。加回归测试 |
| P11 | Codex → Claude 两轮全部记成 **499 `client_disconnect`、token 0**，但 Codex 拿到了完整答案 | Codex 收到 `response.completed` 立即断连；此时网关还在等 Bedrock Anthropic 流的 EOF（`message_stop` 后连接收得慢），`r.Context()` 取消 → 上游读到 `context canceled` → 判为客户端断开 | 网关日志 `truncated=client_disconnect` 与 Codex 成功输出矛盾 | 转换流一旦写出终止事件（`Stream.Ended()`）立即停止读上游、按完成处理；每个 codec 都保证 usage 事件先于 MessageStop。**透传路径**用 Codex → GPT（经 P1 清洗层）对照实测三轮均 200 无此现象，**未改** |
| P12 | 要录 Chat 协议的真实 agentic 流量，Codex 配 `wire_api = "chat"` 报错 | Codex 0.154 已移除 Chat 支持，Chat Completions 现无主流 CLI agent | — | 用官方 `openai` Node SDK 写最小 agentic 循环（流式 + 工具 + 回传），既做录制源也做 E2E 客户端 |
| P13 | handler 测试断言 Responses 流里有 `event: response.xxx` 行，失败 | 我按 OpenAI 文档写断言；实际 Bedrock/OpenAI 发的是 **data-only 帧**、类型在 `data.type`，Codex 就是这么消费的，编码器忠实于录制流量 | 对照 `codex/01-tool-call.response.sse` | 改断言，编码器不动 |
| P17 | 给 fake `primary` provider 加 `openai_responses` 端点后，老用例 `TestMissingKeyAndModelAndUnknownRoute` 失败 | 该用例依赖 `primary` 没有 responses 端点来验证 502 | — | 端点挪到 `backup` provider，老用例前提不变 |

### 4.3 未进代码、只记录的

- Chat Completions 方向的 E2E 客户端是官方 SDK 而非 CLI agent（P12），这是市场现状不是取舍。
- Claude Code 对陌生模型名打 `unrecognized_model` 警告、Codex 打 `Model metadata not found` 警告，均不影响运行；生产上 `modelCode` 用真实模型名即可消除。
- 开发环境两个小坑：zsh 对 `rm -rf dir/*` 弹交互确认会卡住非交互命令（用 `rm -rf dir` 整目录删）；Go 里 `if x, err := T{}.Method(); ...` 复合字面量在 if 头里解析失败（先赋变量）。

## 5. 真实客户端 E2E 结果（Bedrock us-east-1，2026-09-18）

| 客户端 → 入站协议 | `providerProtocol` → 模型 | 轮次 | 结果 |
| --- | --- | --- | --- |
| Claude Code → Anthropic | `openai_chat` → `us.openai.gpt-5.6-sol` | 3（首问 → Read → 结果 → 答） | 全 200，"The file has 3 lines." 正确；usage 含 cache_write/cache_read |
| Claude Code → Anthropic | `openai_responses` → `us.openai.gpt-5.6-sol` | 3 | 同上 |
| Codex → Responses | `anthropic` → `us.anthropic.claude-sonnet-5` | 2（exec_command → 结果 → 答） | 全 200，"4 lines" 正确 |
| Codex → Responses | `openai_chat` → `us.openai.gpt-5.6-sol` | 2 | 同上；无需 `-bedrock-compat` |
| openai SDK → Chat | `anthropic` → `us.anthropic.claude-sonnet-5` | 2 | 全 200，客户端 usage 与账单一致 |
| openai SDK → Chat | `openai_responses` → `us.openai.gpt-5.6-sol` | 2 | 全 200 |

所有请求 `usage_found: true`、`truncated: ""`。转换路径**不需要**录制阶段的 Bedrock 清洗层：P1/P2 里 Bedrock 会拒的字段要么不进 IR、要么被 codec 处理、要么被请求头过滤挡掉。

对照组（透传）：Codex → GPT 经 `-bedrock-compat` 清洗层三轮全 200，无 499——说明 P11 只出现在转换路径，透传不改是有依据的。

## 6. 遗留与边界

| 项 | 状态 | 说明 |
| --- | --- | --- |
| 透传路径的 Bedrock 兼容层（P1、P2） | **未做** | Claude Code / Codex 直连本网关透传打 Bedrock 今天仍会 400。属于供应商适配，与协议转换独立；设计文档 §8b 给了 provider 级 `compat` 规则的方向，需单独立项 |
| 透传路径"客户端先断连"的 499 判定 | 未改 | 实测未复现（P11 对照组）；若某供应商在终止事件后迟迟不关连接，透传也会中招，届时再用同一思路（识别终止事件）处理 |
| reasoning 跨家族转换 | 不做（设计决策） | 只做同家族不透明 round-trip；Codex → Claude 路上 Claude 的 thinking 不会以 Codex 可识别的形式出现 |
| `n > 1` | 只保留第一个 choice | 设计决策 |
| Chat 方向的 CLI agent E2E | 用官方 SDK 代替 | 市场现状（P12） |
| PR #1 未合入 | 等待 | 本 feature 分支基于 PR #1，开 PR 后 diff 暂含其一个 commit，#1 合入后自动消失 |

## 7. 复现步骤（本地，需 Bedrock 权限）

```bash
# 1. 构建
go build -o /tmp/llmgw/gateway ./cmd/gateway
go build -o /tmp/llmgw/mock-cp ./cmd/mock-controlplane
go build -o /tmp/llmgw/recordproxy ./tools/recordproxy

# 2. 起栈（三个终端；AWS 凭证走默认链）
/tmp/llmgw/mock-cp -listen :9090 -token mock-token -keys sk-demo-key
AWS_REGION=us-east-1 /tmp/llmgw/gateway -config configs/gateway.local.yaml   # provider bedrock: aws_iam，三个 endpoints 都指 bedrock-runtime.us-east-1
/tmp/llmgw/recordproxy -listen :8081 -target http://127.0.0.1:8080 -out /tmp/llmgw/rec   # 可选，只为录流量

# 3. 下发转换路由（缺省 providerProtocol 的候选为透传）
curl -X PUT http://127.0.0.1:9090/debug/routes -H 'Content-Type: application/json' -d '{"version":"e2e","models":[
  {"modelCode":"claude-via-gpt","providers":[{"providerCode":"bedrock","providerModelCode":"us.openai.gpt-5.6-sol","providerProtocol":"openai_chat","priority":1,"weight":100}]},
  {"modelCode":"gpt-5","providers":[{"providerCode":"bedrock","providerModelCode":"us.anthropic.claude-sonnet-5","providerProtocol":"anthropic","priority":1,"weight":100}]}]}'

# 4a. Claude Code → GPT
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 ANTHROPIC_API_KEY=sk-demo-key ANTHROPIC_MODEL=claude-via-gpt \
  claude -p "Use your Read tool to read sample.txt, then answer in one sentence: how many lines?" --allowedTools Read --output-format text

# 4b. Codex → Claude（config.toml: model="gpt-5", model_providers.llmgw.base_url="http://127.0.0.1:8080/v1", wire_api="responses", env_key="LLMGW_API_KEY"）
CODEX_HOME=/path/to/codex-home LLMGW_API_KEY=sk-demo-key codex exec --skip-git-repo-check "Use exec_command to run: wc -l notes.txt. State the line count in one sentence."

# 5. 核对
# 网关日志每条 request completed：status 200、usage_found true、truncated 为空、provider_model 是目标模型
# curl :8080/metrics | grep llmgw_translation
```
