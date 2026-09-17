# 协议转换设计（草案 / DRAFT — 待与 Odin 对齐后再实现）

> 状态：**设计已定稿（§8 决策依主流实践 + 仓库惯例作出，无需逐条等 Odin），准备进入实现**。分支 `gh/feat-protocol-translation`（基于第二批修复 PR #1）。
> 唯一需知会 Odin 的是"推翻'不做协议转换'的产品定位"，放在 PR 描述里说明，不阻塞实现。

## 1. 目标与范围

让网关在三种主流 LLM 协议之间互转，使一种协议的客户端能访问另一种协议的模型：

- OpenAI **Chat Completions**：`POST /v1/chat/completions`
- OpenAI **Responses**：`POST /v1/responses`
- Anthropic **Messages**：`POST /v1/messages`

**验收场景（Odin 指定）**：
1. **Claude Code → GPT**：Claude Code 说 Anthropic Messages，目标是 GPT（OpenAI Chat）。方向 = 请求 `Anthropic → OpenAI Chat`、响应 `OpenAI Chat → Anthropic`（含流式）。
2. **Codex → Claude**：Codex 说 OpenAI Responses，目标是 Claude（Anthropic）。方向 = 请求 `OpenAI Responses → Anthropic`、响应 `Anthropic → OpenAI Responses`（含流式）。

> 关键判断：**这两个场景决定了优先要做的是哪两条方向**，不必一上来把 6 个方向都做全（见 §4）。

## 2. 现状架构与转换的落点

当前请求流（`internal/proxy/handler.go`）：
```
Detect(入站 path) → proto        // 入站协议
key-auth → route → 选中 candidate(providerCode, providerModelCode)
prov.Endpoint(proto)             // ← 用"入站协议"去找 provider 的端点；找不到就跳过该候选
preq.Rewrite(proto, model)       // ← 只换 model，不改格式
upURL = base + proto.UpstreamPath()
转发 → relay(按 proto 解析 usage、透传字节)
```
**核心事实**：目前"上游协议 == 入站协议"，provider 只有声明了该协议的端点才会被选中，否则 502 `provider lacks endpoint for protocol`。

**转换落点**：引入"目标协议"（provider 实际会说的协议）与"入站协议"解耦，中间过一个**规范中间表示(IR)**（对齐 LiteLLM：入站→IR→目标，避免 O(N²) 个 bespoke 转换器）。当两者不同：
```
入站 proto=A ─▶ [A→IR]─▶[IR→B] ─▶ 以 B 转发 ─▶ 上游返回 B
                                                 │
客户端收到 A ◀─ [IR→A]◀─[B→IR] ◀─────────────────┘   （流式则逐事件经 IR 转换）
usage 仍按上游 B 解析（usage 语义已在 Usage 里归一，不受协议影响）
```
新增子包 `internal/protocol/translate`：每个协议实现 `请求 to/from IR`、`响应 to/from IR`、`流式事件 to/from IR`。
`providerProtocol == 入站` 时完全走今天的透传路径，不进 IR（零回归风险、零额外开销）。

**目标协议从哪来（已决策，见 §8.1）**：路由候选新增可选字段 `providerProtocol`（控制面下发）。缺省 = 目标==入站 = 今天的纯透传；
填了则要求 provider 声明了对应 endpoint。与"控制面下发路由、provider 声明 endpoints"的现有模型一致，同一 model 可路由到不同协议的 provider。

## 3. 三协议要点对照（转换难点全在这张表）

| 维度 | OpenAI Chat | OpenAI Responses | Anthropic Messages |
|---|---|---|---|
| 系统提示 | `messages[role=system]` | 顶层 `instructions` | **顶层 `system`**（不是 message） |
| 历史消息 | `messages[]`（role+content） | `input[]`（typed items）或纯字符串 | `messages[]`（role 仅 user/assistant） |
| content 形态 | string 或 parts[] | items[]（`input_text`/`input_image`/…） | string 或 blocks[]（`text`/`image`/`tool_use`/`tool_result`） |
| max tokens | `max_tokens` / `max_completion_tokens`（可选） | `max_output_tokens`（可选） | **`max_tokens`（必填）** |
| 工具定义 | `tools[{type:function, function:{...}}]` | `tools[{type:function,...}]` | `tools[{name, input_schema}]` |
| 工具调用（响应） | `message.tool_calls[]` | output item `function_call` | content block `tool_use` |
| 工具结果（请求） | `messages[role=tool]` | input item `function_call_output` | content block `tool_result`（在 user 消息里） |
| 停止原因 | `finish_reason`（stop/length/tool_calls/…) | `status`+`incomplete_details` | `stop_reason`（end_turn/max_tokens/tool_use/…) |
| 推理 | `reasoning`(o 系)/`reasoning_content` | reasoning item | `thinking` block |
| 非流式 usage | `usage.prompt_tokens/completion_tokens` | `usage.input_tokens/output_tokens` | `usage.input_tokens/output_tokens` |

## 4. 六个方向 × 无损性评级（先做打勾的两条）

| # | 方向 | 优先 | 无损性 | 主要难点 |
|---|---|---|---|---|
| 1 | Anthropic → OpenAI Chat（请求）/ 反向（响应） | ✅ 场景1 | 中 | system 提上/下、max_tokens 必填、tool_use↔tool_calls、thinking↔reasoning、流式事件重组 |
| 2 | OpenAI Responses → Anthropic（请求）/ 反向（响应） | ✅ 场景2 | 中低 | Responses 的 `input` items ↔ Anthropic messages/blocks 差异最大、function_call(_output) 映射、流式 typed events↔block events |
| 3 | OpenAI Chat → Anthropic（请求）/ 反向（响应） | ✅ 本期 | 中 | 同 1（对称方向） |
| 4 | OpenAI Chat ↔ OpenAI Responses（两向） | ✅ 本期 | 中 | 同厂但 messages↔input 结构不同 |
| 5 | Responses → OpenAI Chat / 反向 | ✅ 本期 | 中 | 同 4 |
| 6 | Anthropic → Responses（请求）/ 反向（响应） | ✅ 本期 | 中低 | 同 2（对称方向） |

**范围（更正）**：Odin 要的是"三种协议互转"=**全 6 个跨协议方向都实现**；`claude code→gpt`、`codex→claude` 是**必过的端到端验收门槛**（覆盖最易错的路径），不是实现上限。
IR 架构让全矩阵成本可控：给每个协议写 `to IR` / `from IR`（请求、响应、流式各一对），共 3×2×3=18 个适配函数，写完任意"入站→目标"组合自动可用。
"全 6 向无损不现实"指的是**内容深度**（reasoning/hosted tools/server 端状态无法无损），按 §7 处理；**方向本身 6 向全做**。文本 + 工具调用一期全 6 向覆盖；图片/reasoning 二期。

## 5. 请求转换（以优先方向为例）

### 5.1 Anthropic 请求 → OpenAI Chat
- `system`（顶层，可能是 string 或 blocks）→ 追加为 `messages` 首个 `{role:system}`。
- `messages[]`：role user/assistant 直接映；content blocks：`text`→文本；`image`→ OpenAI `image_url`（base64 data URL）；`tool_use`→ 归入 assistant `tool_calls`；`tool_result`→ 独立 `{role:tool, tool_call_id}`。
- `max_tokens` → `max_tokens`（Anthropic 必填，天然有值）。
- `tools[{name,input_schema}]` → `tools[{type:function,function:{name,parameters}}]`。
- `stop_sequences`→`stop`；`temperature`/`top_p` 直传。

### 5.2 OpenAI Responses 请求 → Anthropic
- `instructions` → 顶层 `system`。
- `input`：字符串 → 单条 user 文本；items[] → 逐项映射（`input_text`→text block、`input_image`→image block、`function_call`→assistant `tool_use`、`function_call_output`→user `tool_result`）。
- **`max_output_tokens` 可能缺失 → Anthropic 必填**：缺失时补配置默认 `server.default_max_tokens`（默认 **4096**，LiteLLM 同款），记 `debug` 日志 + 指标 `llmgw_translation_defaults_total{field="max_tokens"}`（已决策，见 §8.3）。
- `tools` 映射同上。

## 6. 响应转换

### 6.1 非流式
按上表把 `choices[0].message` / `output[]` / `content[]` 三种结构互相组装；`finish_reason`↔`stop_reason`↔`status` 用固定枚举表映射（未知值兜底到最接近的）。usage **不用转**——网关内部 `Usage` 已归一，只是最后按目标协议的字段名重新序列化给客户端。

### 6.2 流式（**最难、最容易出 bug**）
三家 SSE 事件模型完全不同，转换 = **把源事件流实时重组成目标事件流**：

| 源 → 目标 | 要合成的目标事件序列 |
|---|---|
| OpenAI Chat delta → Anthropic | `message_start` → `content_block_start` → 多个 `content_block_delta(text_delta)` → `content_block_stop` → `message_delta(stop_reason,usage)` → `message_stop` |
| Anthropic events → OpenAI Chat | 首 `role` chunk → 多个 `{delta:{content}}` chunk → 末 `{finish_reason}` + `usage` chunk → `[DONE]` |
| Anthropic events → Responses | `response.created`→`response.output_text.delta`*→`response.completed` |

难点具体：
- **状态机**：转换器要维护"当前在第几个 content block、是文本还是 tool_use、是否已发 message_start"等状态，逐事件推进。
- **工具调用流式**：OpenAI `tool_calls` 的 arguments 是增量 JSON 片段，Anthropic 是 `input_json_delta`——增量边界不一样，要缓冲重切。
- **首字节时机 / usage 落点**：usage 在不同协议落在不同事件；转换后要保证仍能被 `StreamParser` 抽到（计量不能因转换丢失）。
- **错误中途发生**：源流中途报错，目标流要合成一个合理的错误事件或干净结束。

实现上建议：新增 `StreamTranslator` 接口（输入源事件、输出目标事件字节），在 `handler.go` 的 relay 里当 `proto(入站) != 目标` 时接管转发；**保持第二批 ISSUE-3 的"数据面/计量面解耦"精神**——但注意这里数据面本身要改写，不能再纯 `io.Copy`。

## 7. 有损 / 不支持的处理原则（需 Odin 拍板，见 §8）

| 情况 | 建议处理 |
|---|---|
| 目标协议不支持的字段（如某些 sampling 参数） | 静默丢弃 + 记 `debug` 日志（不报错，保可用） |
| 必填字段缺失需补默认（OpenAI→Anthropic 的 max_tokens） | 补配置默认值 + 记指标 |
| 语义无法表达（罕见的多模态/工具组合） | **报 4xx 明确错误**，不要静默产出错误结果 |
| 未知枚举（finish_reason/stop_reason） | 兜底到最接近值 + 记日志 |

原则：**能用就别报错、但绝不静默产出"看起来对其实错"的结果**（呼应第二批 ISSUE-2 的思路）。

## 8. 决策（依主流实践 + 仓库惯例，不再逐条问 Odin）

> 依据：Codex 已强制 Responses（openai/codex 讨论 #7782，约 2026-02 移除 chat/completions）；Claude Code 固定说 Anthropic Messages；
> 主流转换网关（LiteLLM 等）用"规范中间表示双向转"，缺 max_tokens 用 `DEFAULT_MAX_TOKENS`(4096) 兜底；无损转换不可假设。均为交付客户可用的成熟做法。

1. **目标协议怎么声明** → **路由候选新增可选字段 `providerProtocol`**（控制面下发），取值 `openai_chat`/`openai_responses`/`anthropic`。
   - 缺省（不填）= 目标协议 == 入站协议 = **今天的纯透传行为**（100% 向后兼容，老路由不受影响）。
   - 填了就要求该 provider 声明了对应 `endpoints.<providerProtocol>`，否则该候选被跳过（复用现有 `provider lacks endpoint` 逻辑）。
   - 选它的理由：与仓库"控制面下发路由、provider 声明 endpoints"的现有模型完全一致；同一 model 可路由到不同协议的 provider；不引入请求头/环境变量这类隐式开关。

2. **最终范围 = 全 6 向；实现顺序 = 先打穿 2 条验收方向，再铺其余 4 向（同一 PR 交付）**。
   架构走"规范中间表示(IR)"（对齐 LiteLLM：入站→IR→目标，请求/响应/流式各一套 to/from IR）。
   Odin 原话"支持几个协议互转"即全矩阵互转；两个客户端场景是**验收测试**不是范围上限。
   IR 让全矩阵 = 每协议写 to/from IR（3×2×3=18 个适配函数），写完 6 个跨协议方向自动可用；不写 O(N²) 个 bespoke 转换器。
   **顺序（评审建议，采纳）**：只有 Claude Code→GPT、Codex→Claude 两条能用真实客户端 E2E 验证，而 bug 恰恰藏在流式细节里；
   先把这两条端到端打穿（含真实流量 fixture），再用同一套 IR 横向补齐其余 4 向 + golden 测试。这不缩范围，只是不让 4 条"仅 golden 测试"的方向稀释对关键路径的打磨。
   一期 6 向都做**文本 + 工具调用 + reasoning round-trip**；图片留二期。

3. **缺 max_tokens 补默认值** → **可接受，默认 8192**（评审修正：LiteLLM 的 4096 对 agentic coding 太小，Claude Code/Codex 常吐大 diff，截断表现为正常的 `stop_reason: max_tokens`，用户难以察觉），做成配置 `server.default_max_tokens`，触发时记 `debug` 日志 + 指标 `llmgw_translation_defaults_total{field="max_tokens"}`。
   Anthropic `max_tokens` 必填，OpenAI/Responses 可缺省，转过去必须补。IR 用 `MaxTokens *int64`，nil 才补默认，**绝不把 0 写上线**。
   转到 OpenAI Chat 时发 **`max_completion_tokens`**（o 系/GPT-5 拒收 `max_tokens`）——这是转换器生成新 body，不是改写透传体，不违背仓库"透传不改字段名"的纪律。

4. **复杂内容范围** → **一期：文本 + 工具调用 + reasoning 的 opaque round-trip**。
   工具调用**不是可选项**——Claude Code、Codex 都是 agentic 客户端，重度依赖工具调用，没有它这个 feature 对验收场景没意义。
   **reasoning/thinking 不能推到二期**（评审修正）：Codex 多轮携带 opaque `reasoning` item（`encrypted_content`），Anthropic 多轮工具调用要求把 `thinking` 块连同 `signature` **原样回传**，丢了下一轮直接 400。
   所以一期做的是"**不理解、只保证不丢**"：IR 用 `ThinkingSignature` / `ThinkingRaw` 承载，FromIR 原样吐回目标协议有对应槽位的地方。真正的 reasoning 语义映射（如 budget 参数）二期。
   **二期：多模态（图片）**；一期"能带就带、带不过去按 §7 处理"。

5. **报错 vs 尽力而为** → **保持 §7 表**（能用就别报错，但绝不静默产出"看起来对其实错"的结果）。业界共识即"无损不可假设"，与此一致。

6. **README/文档同步** → **确认要改**。去掉"不做协议转换是产品决定"的表述（README、`internal/protocol` 包注释、CLAUDE.md 若在 GitHub 版存在），
   改成"支持协议互转"的支持矩阵 + 有损边界说明。这是本 feature 的一部分，随实现一起交付。

7. **OpenAI Responses 的服务端状态（`previous_response_id` / `store`）** → **IR 无状态，不支持跨 provider 复现状态**。
   Responses 请求带 `previous_response_id` 时，Responses RequestCodec 直接返回 4xx 明确报错（§7 "不静默出错"）；`store` 忽略。
   这是 LiteLLM 等成熟网关的同款处理。**风险**：Codex 若依赖 `previous_response_id` 做多轮，场景 2 会从根上卡住。
   **必做证伪步骤**：写 codec 之前先用透传模式录一次真实 Codex 流量，确认它是否自带全量历史（`store:false`）。若依赖状态，方案变为要求客户端关状态或重新评估该方向。

8. **跨协议 usage 的实现约束**（评审提醒，最易漏）：
   - 上游返回的是**目标协议 B** 的 usage 字段，`StreamParser`/`ParseUsage` 必须用 **targetProto** 构造，不能沿用入站 proto；
   - FromIR 生成 OpenAI Chat 流式请求时要**注入 `stream_options.include_usage: true`**（今天 `Rewrite` 只在入站是 chat 时注入，转换路径要自己做）；
   - IR.Usage 已归一，渲染回入站协议 A 只是换字段名；计量按 IR.Usage 上报。用真实端点回归计量，防"看起来通了账算错"。

9. **IR 一期必备字段**（评审补齐）：`ToolChoice`（auto/none/required/named，agentic 客户端会用）；流式事件 `Index`（OpenAI tool_calls delta 的 id 只在首片出现、Anthropic 用 block index，**并行工具调用重组必需**）；
   `ToolInputDelta string`（原样片段、不累积——两家都是片段流）；`ParallelToolCalls`、`ResponseFormat` 作 opaque 携带。`MaxTokens *int64` 与 `Temperature/TopP` 同用指针表示"未设置"。

> 第 6 点"改产品定位"在 PR 描述里**知会 Odin**。评审另建议**动手前用一条消息向 Odin 确认"6 向 + reasoning 一期"这个范围**（若他实际只要 2 向，4 向白做）——是否发由负责人决定，不阻塞 M2/M3 的骨架与 fixture 录制。

## 9. 测试方案

- **golden fixture 必须来自真实抓包，不手写**（评审强调，最高杠杆）：手写 JSON 会漏掉真实客户端的怪癖，而那正是出 bug 的地方。
  做法：先用**透传模式**（`providerProtocol` 留空）让 Claude Code 打真 Claude、Codex 打真 GPT，在网关侧录制请求体 / 非流式响应 / 完整 SSE 事件序列（含多轮工具调用、reasoning 块），落成 `testdata/fixtures/{claude-code,codex}/*.json|.sse`。
  这一步同时完成 §8.7 的 Codex 有状态性证伪。
- **单元测试**：每个方向的请求/响应/流式各若干 golden 用例（fixture → 期望目标 JSON）；重点覆盖 system 位置、max_tokens 默认、tool_choice、工具调用（含并行 index）、reasoning round-trip、finish/stop 枚举。
- **流式状态机测试**：喂真实源事件序列，断言输出目标事件序列**逐字节**符合预期（参照第二批 ISSUE-3 的做法）；**工具调用流式单独拉出来测**（增量 JSON 片段、并行 index、中途错误）。
- **计量回归**：每个方向断言 IR.Usage 正确（用目标协议 B 的字段解析），转换不能吃掉 usage。
- **端到端（真实端点，验收场景）**：
  - 场景1：本机跑 Claude Code，`ANTHROPIC_BASE_URL` 指向网关，路由 `Anthropic → 某 GPT`；验证多轮对话、工具调用、流式。
  - 场景2：本机跑 Codex，base 指向网关，路由 `Responses → 某 Claude`；同上。
  - 真实端点用上次验证过能通的 Bedrock 模型（Claude 用 `us.anthropic.claude-sonnet-5` 这类干净别名，别用带日期版本号的退役 ID）。
- 计量核对：转换后 usage 仍要正确上报（转换不能吃掉 usage）。

## 10. 里程碑

1. ✅ 设计定稿（本文档，§8 已决策；评审后修正 reasoning/状态/字段/默认值/顺序）。
2. ✅ 骨架 + 分叉点 + 零回归：`providerProtocol` 路由字段透传到 `Candidate`；`translate` 子包 IR 类型 + codec 接口 + `Supported()`；handler 按 `Supported()` 决定跳过候选；4 个测试守护（透传不变、跨协议未支持时拒且不调上游、非法值拒）。
3. **录制真实 fixture**（透传模式跑 Claude Code→Claude、Codex→GPT），同时证伪 Codex 有状态性（§8.7）。落 `testdata/fixtures/`。
4. **方向 1：Anthropic ↔ OpenAI Chat**（Claude Code→GPT）：Anthropic 请求 to IR、Chat 请求 from IR；Chat 响应 to IR、Anthropic 响应 from IR；流式状态机；golden 单测（用 fixture）。
5. **方向 2：Responses ↔ Anthropic**（Codex→Claude）：同上。
6. handler relay 接线：`Supported(proto,target)` 为真时走 IR 转换（parser 按 **targetProto** 建、FromIR 注入 include_usage），否则透传；打开 `Supported()` 对应方向。
7. **E2E 验收（必过）**：Claude Code→GPT、Codex→Claude，真实 Bedrock 端点（Claude 用干净别名），含多轮工具调用 + reasoning round-trip + 计量核对。
8. **横向铺齐其余 4 向**（Chat→Anthropic、Anthropic→Responses、Chat↔Responses 两向）：补各协议缺的 to/from IR，6 向 golden 单测全绿，`Supported()` 全开。
9. README/`protocol` 包注释/文档更新（去掉"不做协议转换"，写清 6 向支持矩阵与有损边界）+ CHANGELOG 草稿放 PR 描述。
