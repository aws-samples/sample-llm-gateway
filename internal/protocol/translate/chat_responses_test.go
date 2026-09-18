package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// M8: OpenAI Chat Completions ↔ OpenAI Responses — the last two directions. Fixtures:
//   - openai-sdk-chat/*: real Chat traffic recorded from the official openai Node SDK running an
//     agentic loop (tool call → tool result) against GPT on Bedrock (Codex dropped wire_api=chat,
//     so the SDK is the mainstream Chat-speaking client).
//   - codex/*: real Responses traffic recorded from Codex CLI.

// ---- Chat request → IR → Responses request ---------------------------------------------------

func TestChatToResponses_SDK_ToolOutputFollowup(t *testing.T) {
	ir, err := openAIChatRequest{}.ToIR(fixture(t, "openai-sdk-chat/02-tool-output-followup.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Leading system message → IR System; then user, assistant(tool_use), tool(tool_result).
	if len(ir.System) != 1 || !strings.Contains(ir.System[0].Text, "terse coding agent") {
		t.Errorf("system → System: %+v", ir.System)
	}
	if got := roles(ir.Messages); len(got) != 3 || got[0] != RoleUser || got[1] != RoleAssistant || got[2] != RoleTool {
		t.Errorf("roles: %v", got)
	}
	if ir.MaxTokens != nil || !ir.Stream {
		t.Errorf("MaxTokens should be nil (SDK omitted it), stream true: %v %v", ir.MaxTokens, ir.Stream)
	}

	out, err := openAIResponsesRequest{}.FromIR(ir, "gpt-real", Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["model"] != "gpt-real" || m["stream"] != true || m["store"] != false {
		t.Errorf("envelope: model=%v stream=%v store=%v", m["model"], m["stream"], m["store"])
	}
	if !strings.Contains(m["instructions"].(string), "terse coding agent") {
		t.Errorf("System → instructions: %v", m["instructions"])
	}
	items := m["input"].([]any)
	types := make([]string, len(items))
	for i, it := range items {
		types[i] = it.(map[string]any)["type"].(string)
	}
	if strings.Join(types, ",") != "message,function_call,function_call_output" {
		t.Fatalf("input item types: %v", types)
	}
	fc := items[1].(map[string]any)
	if fc["call_id"] != "call_0" || fc["name"] != "exec_command" || fc["arguments"] != `{"cmd":"wc -l notes.txt"}` {
		t.Errorf("function_call: %v", fc)
	}
	fo := items[2].(map[string]any)
	if fo["call_id"] != "call_0" || !strings.Contains(fo["output"].(string), "4 notes.txt") {
		t.Errorf("function_call_output: %v", fo)
	}
	tools := m["tools"].([]any)
	t0 := tools[0].(map[string]any)
	if len(tools) != 1 || t0["type"] != "function" || t0["name"] != "exec_command" || t0["parameters"] == nil || t0["function"] != nil {
		t.Errorf("Responses tools must be flat {type,name,parameters}: %v", t0)
	}
	// Chat-only fields must not leak.
	for _, k := range []string{"messages", "stream_options", "max_completion_tokens", "max_tokens", "reasoning_effort"} {
		if _, has := m[k]; has {
			t.Errorf("chat field %q leaked into Responses request", k)
		}
	}
}

// ---- Responses request → IR → Chat request ---------------------------------------------------

func TestResponsesToChat_Codex_ToolOutputFollowup(t *testing.T) {
	ir, err := openAIResponsesRequest{}.ToIR(fixture(t, "codex/02-tool-output-followup.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := openAIChatRequest{}.FromIR(ir, "gpt-real", Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["model"] != "gpt-real" || m["stream"] != true {
		t.Errorf("envelope: %v %v", m["model"], m["stream"])
	}
	if so, _ := m["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("include_usage must be forced for a Chat upstream: %v", m["stream_options"])
	}
	if m["reasoning_effort"] != "none" {
		t.Errorf("tools present → reasoning_effort none (Bedrock gpt-5.x on /chat/completions): %v", m["reasoning_effort"])
	}
	if _, has := m["max_completion_tokens"]; has {
		t.Errorf("Codex omits max_output_tokens; Chat does not require it, so nothing must be defaulted: %v", m["max_completion_tokens"])
	}
	msgs := m["messages"].([]any)
	var rolesSeen []string
	var toolMsgs, assistantWithCalls int
	for _, x := range msgs {
		mm := x.(map[string]any)
		rolesSeen = append(rolesSeen, mm["role"].(string))
		switch mm["role"] {
		case "tool":
			toolMsgs++
			if mm["tool_call_id"] == "" || mm["content"] == nil {
				t.Errorf("tool message: %v", mm)
			}
		case "assistant":
			if tcs, _ := mm["tool_calls"].([]any); len(tcs) > 0 {
				assistantWithCalls++
				tc := tcs[0].(map[string]any)
				if tc["type"] != "function" || tc["id"] == "" || !json.Valid([]byte(tc["function"].(map[string]any)["arguments"].(string))) {
					t.Errorf("tool_call: %v", tc)
				}
			}
		}
	}
	// instructions → leading system; the developer item → a second system message; then the turns.
	if len(rolesSeen) < 4 || rolesSeen[0] != "system" || rolesSeen[1] != "system" {
		t.Errorf("roles: %v", rolesSeen)
	}
	if toolMsgs != 1 || assistantWithCalls != 1 {
		t.Errorf("tool round-trip: tool msgs=%d assistant w/ calls=%d roles=%v", toolMsgs, assistantWithCalls, rolesSeen)
	}
	tools := m["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("tools lost")
	}
	for _, x := range tools {
		tt := x.(map[string]any)
		fn, _ := tt["function"].(map[string]any)
		if tt["type"] != "function" || fn["name"] == nil || fn["parameters"] == nil {
			t.Errorf("Chat tools must be {type:function,function:{name,parameters}}: %v", tt)
		}
		if fn["name"] == "web_search" {
			t.Error("hosted web_search tool must not be turned into a function")
		}
	}
	for _, k := range []string{"input", "instructions", "store", "include", "reasoning", "previous_response_id", "prompt_cache_key", "client_metadata"} {
		if _, has := m[k]; has {
			t.Errorf("responses field %q leaked into Chat request", k)
		}
	}
}

// ---- Chat ↔ Responses request round trips must be structurally stable -------------------------

func TestChatResponsesRequestRoundTrips(t *testing.T) {
	// Chat → IR → Responses → IR
	in := fixture(t, "openai-sdk-chat/02-tool-output-followup.request.json")
	ir1, err := openAIChatRequest{}.ToIR(in)
	if err != nil {
		t.Fatal(err)
	}
	asResp, err := openAIResponsesRequest{}.FromIR(ir1, ir1.Model, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ir2, err := openAIResponsesRequest{}.ToIR(asResp)
	if err != nil {
		t.Fatalf("re-parse as Responses: %v", err)
	}
	a, _ := json.Marshal(ir1)
	b, _ := json.Marshal(ir2)
	if string(a) != string(b) {
		t.Errorf("Chat→Responses→IR drifted\n first: %.400s\nsecond: %.400s", a, b)
	}
	// Responses (Codex) → IR → Chat → IR: the developer item is hoisted into System by the Chat
	// ToIR (leading system messages), so compare with that normalisation applied.
	in2 := fixture(t, "codex/02-tool-output-followup.request.json")
	ir3, err := openAIResponsesRequest{}.ToIR(in2)
	if err != nil {
		t.Fatal(err)
	}
	asChat, err := openAIChatRequest{}.FromIR(ir3, ir3.Model, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ir4, err := openAIChatRequest{}.ToIR(asChat)
	if err != nil {
		t.Fatalf("re-parse as Chat: %v", err)
	}
	if len(ir4.System) != 2 || len(ir4.Messages) != len(ir3.Messages)-1 {
		t.Errorf("developer item should hoist into System on the Chat side: System=%d Messages=%d (was %d)", len(ir4.System), len(ir4.Messages), len(ir3.Messages))
	}
	if len(ir4.Tools) != len(ir3.Tools) || ir4.Stream != ir3.Stream {
		t.Errorf("tools/stream drifted: %d vs %d, %v vs %v", len(ir4.Tools), len(ir3.Tools), ir4.Stream, ir3.Stream)
	}
}

// ---- non-streaming responses -----------------------------------------------------------------

func TestChatResponseToResponses(t *testing.T) {
	ir, err := openAIChatResponse{}.ToIR([]byte(chatToolCallBody))
	if err != nil {
		t.Fatal(err)
	}
	out, err := openAIResponsesResponse{}.FromIR(ir)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["object"] != "response" || m["status"] != "completed" {
		t.Errorf("envelope: %v", m)
	}
	items := m["output"].([]any)
	var sawMsg, sawFC bool
	for _, it := range items {
		im := it.(map[string]any)
		switch im["type"] {
		case "message":
			sawMsg = true
		case "function_call":
			sawFC = true
			if im["call_id"] == "" || im["name"] == nil || !json.Valid([]byte(im["arguments"].(string))) || im["status"] != "completed" {
				t.Errorf("function_call item: %v", im)
			}
		}
	}
	if !sawFC {
		t.Errorf("tool_calls → function_call item missing: %v", items)
	}
	_ = sawMsg
	// usage: Chat prompt_tokens (incl. cached) ↔ Responses input_tokens (incl. cached).
	var chat struct {
		Usage struct {
			PromptTokens     float64 `json:"prompt_tokens"`
			CompletionTokens float64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal([]byte(chatToolCallBody), &chat)
	u := m["usage"].(map[string]any)
	if u["input_tokens"] != chat.Usage.PromptTokens || u["output_tokens"] != chat.Usage.CompletionTokens {
		t.Errorf("usage: %v vs chat %+v", u, chat.Usage)
	}
}

func TestResponsesResponseToChat(t *testing.T) {
	body := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-x",
	  "output":[
	    {"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Running it.","annotations":[]}]},
	    {"type":"function_call","id":"fc_1","status":"completed","call_id":"call_9","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}
	  ],
	  "usage":{"input_tokens":120,"output_tokens":40,"input_tokens_details":{"cached_tokens":100},"output_tokens_details":{"reasoning_tokens":25},"total_tokens":160}}`
	ir, err := openAIResponsesResponse{}.ToIR([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := openAIChatResponse{}.FromIR(ir)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["object"] != "chat.completion" || m["id"] != "resp_1" {
		t.Errorf("envelope: %v", m)
	}
	ch := m["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if ch["finish_reason"] != "tool_calls" || msg["content"] != "Running it." {
		t.Errorf("choice: %v", ch)
	}
	tcs := msg["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if len(tcs) != 1 || tc["id"] != "call_9" || tc["function"].(map[string]any)["name"] != "exec_command" || tc["function"].(map[string]any)["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("tool_calls: %v", tcs)
	}
	u := m["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(120) || u["completion_tokens"] != float64(40) ||
		u["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(100) ||
		u["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(25) {
		t.Errorf("usage: %v", u)
	}
}

// ---- streaming: real Chat SSE (SDK ← GPT) → IR → Responses SSE ---------------------------------

func TestChatStreamToResponsesSSE_RealToolCall(t *testing.T) {
	frames := parseSSE(t, fixture(t, "openai-sdk-chat/01-tool-call.response.sse"))
	evs := driveIn(t, newChatStreamIn(), frames)
	if evs[len(evs)-1].Kind != EventMessageStop {
		t.Fatalf("decoder must end with message_stop: %v", kindSeq(evs))
	}
	out := driveOut(t, newResponsesStreamOut(), evs)
	got := parseSSE(t, out)

	var types []string
	var args strings.Builder
	var added, final map[string]any
	var usage map[string]any
	for _, f := range got {
		var d struct {
			Type     string         `json:"type"`
			Seq      *int           `json:"sequence_number"`
			Item     map[string]any `json:"item"`
			Delta    string         `json:"delta"`
			Response map[string]any `json:"response"`
		}
		if err := json.Unmarshal([]byte(f.Data), &d); err != nil || d.Seq == nil {
			t.Fatalf("bad frame (err=%v seq=%v): %s", err, d.Seq, f.Data)
		}
		types = append(types, d.Type)
		switch d.Type {
		case "response.output_item.added":
			if d.Item["type"] == "function_call" {
				added = d.Item
			}
		case "response.function_call_arguments.delta":
			args.WriteString(d.Delta)
		case "response.completed":
			final = d.Response
			usage, _ = d.Response["usage"].(map[string]any)
		}
	}
	if types[0] != "response.created" || types[len(types)-1] != "response.completed" {
		t.Fatalf("envelope: %v ... %v", types[0], types[len(types)-1])
	}
	if added == nil || added["call_id"] != "call_0" || added["name"] != "exec_command" {
		t.Errorf("function_call item added: %v", added)
	}
	if args.String() != `{"cmd":"wc -l notes.txt"}` {
		t.Errorf("arguments reassembled from Chat fragments: %q", args.String())
	}
	fcDone := false
	for _, it := range final["output"].([]any) {
		im := it.(map[string]any)
		if im["type"] == "function_call" && im["status"] == "completed" && im["arguments"] == `{"cmd":"wc -l notes.txt"}` {
			fcDone = true
		}
	}
	if !fcDone {
		t.Errorf("response.completed.output must carry the completed function_call: %v", final["output"])
	}
	// Chat usage prompt 93 / completion 22 → Responses input_tokens 93 / output_tokens 22.
	if usage == nil || usage["input_tokens"] != float64(93) || usage["output_tokens"] != float64(22) {
		t.Errorf("usage: %v", usage)
	}
	if strings.Contains(string(out), "chat.completion.chunk") || strings.Contains(string(out), "[DONE]") {
		t.Error("Chat framing leaked into Responses stream")
	}
}

// ---- streaming: real Responses SSE (Codex ← GPT) → IR → Chat SSE -------------------------------

func TestResponsesStreamToChatSSE_RealToolCall(t *testing.T) {
	frames := parseSSE(t, fixture(t, "codex/01-tool-call.response.sse"))
	evs := driveIn(t, newResponsesStreamIn(), frames)
	if evs[len(evs)-1].Kind != EventMessageStop {
		evs = append(evs, StreamEvent{Kind: EventMessageStop, StopReason: StopToolUse})
	}
	out := driveOut(t, newChatStreamOut(), evs)
	got := parseSSE(t, out)
	if got[len(got)-1].Data != "[DONE]" {
		t.Fatalf("Chat stream must end with [DONE], got %q", got[len(got)-1].Data)
	}
	var (
		id, name  string
		args      strings.Builder
		finish    string
		sawRole   bool
		usage     map[string]any
		toolIndex = -1
	)
	for _, f := range got[:len(got)-1] {
		var c struct {
			Object  string `json:"object"`
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
				Delta        struct {
					Role      string `json:"role"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(f.Data), &c); err != nil {
			t.Fatalf("bad chunk: %v\n%s", err, f.Data)
		}
		if c.Object != "chat.completion.chunk" {
			t.Errorf("object: %q", c.Object)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		for _, ch := range c.Choices {
			if ch.Delta.Role == "assistant" {
				sawRole = true
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				if toolIndex == -1 {
					toolIndex = tc.Index
				} else if tc.Index != toolIndex {
					t.Errorf("tool_calls index must be stable for one call: %d vs %d", tc.Index, toolIndex)
				}
				if tc.ID != "" {
					id = tc.ID
				}
				if tc.Function.Name != "" {
					name = tc.Function.Name
				}
				args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if !sawRole {
		t.Error("first chunk must carry delta.role=assistant (SDKs key on it)")
	}
	if id == "" || name == "" || !json.Valid([]byte(args.String())) {
		t.Errorf("tool call: id=%q name=%q args=%q", id, name, args.String())
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason: %q", finish)
	}
	if usage == nil || usage["prompt_tokens"] == nil || usage["completion_tokens"] == nil {
		t.Errorf("final usage chunk missing (include_usage shape): %v", usage)
	}
	if strings.Contains(string(out), "response.output_item") || strings.Contains(string(out), "sequence_number") {
		t.Error("Responses framing leaked into Chat stream")
	}
}

// ---- real Chat text stream (tool result follow-up) → Responses -------------------------------------

func TestChatStreamToResponsesSSE_RealTextOnly(t *testing.T) {
	frames := parseSSE(t, fixture(t, "openai-sdk-chat/02-tool-output-followup.response.sse"))
	evs := driveIn(t, newChatStreamIn(), frames)
	out := driveOut(t, newResponsesStreamOut(), evs)
	var text strings.Builder
	var status string
	for _, f := range parseSSE(t, out) {
		var d struct {
			Type     string         `json:"type"`
			Delta    string         `json:"delta"`
			Response map[string]any `json:"response"`
		}
		_ = json.Unmarshal([]byte(f.Data), &d)
		if d.Type == "response.output_text.delta" {
			text.WriteString(d.Delta)
		}
		if d.Type == "response.completed" {
			status, _ = d.Response["status"].(string)
		}
	}
	if !strings.Contains(text.String(), "4 lines") || status != "completed" {
		t.Errorf("text=%q status=%q", text.String(), status)
	}
}
