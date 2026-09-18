package translate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fixtures", rel))
	if err != nil {
		t.Fatalf("fixture %s: %v", rel, err)
	}
	return b
}

func mustJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, string(b[:min(len(b), 400)]))
	}
	return m
}

// ---- Anthropic → IR: real Claude Code traffic ---------------------------------------------

func TestAnthropicToIR_ClaudeCode_ToolResultFollowup(t *testing.T) {
	ir, err := anthropicRequest{}.ToIR(fixture(t, "claude-code/03-tool-result-followup.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ir.Model != "claude-sonnet-5" || !ir.Stream {
		t.Errorf("model/stream: %q %v", ir.Model, ir.Stream)
	}
	if ir.MaxTokens == nil || *ir.MaxTokens != 64000 {
		t.Errorf("max_tokens should be 64000, got %v", ir.MaxTokens)
	}
	// system: 3 text blocks hoisted
	if len(ir.System) != 3 || ir.System[0].Kind != KindText || !strings.HasPrefix(ir.System[0].Text, "x-anthropic-billing-header") {
		t.Errorf("system blocks: %+v", ir.System)
	}
	if len(ir.Tools) != 22 {
		t.Errorf("want 22 tools, got %d", len(ir.Tools))
	}
	var read *Tool
	for i := range ir.Tools {
		if ir.Tools[i].Name == "Read" {
			read = &ir.Tools[i]
		}
	}
	if read == nil || !json.Valid(read.Schema) || !strings.Contains(string(read.Schema), `"file_path"`) {
		t.Errorf("Read tool schema not carried: %+v", read)
	}
	// messages: user(text,text) / system(str) / assistant(thinking,tool_use) / user(tool_result) / system(block)
	wantRoles := []Role{RoleUser, RoleSystem, RoleAssistant, RoleUser, RoleSystem}
	if len(ir.Messages) != len(wantRoles) {
		t.Fatalf("want %d messages, got %d", len(wantRoles), len(ir.Messages))
	}
	for i, r := range wantRoles {
		if ir.Messages[i].Role != r {
			t.Errorf("messages[%d] role = %q want %q", i, ir.Messages[i].Role, r)
		}
	}
	// mid-conversation system message with string content
	if ms := ir.Messages[1]; len(ms.Content) != 1 || ms.Content[0].Kind != KindText || !strings.HasPrefix(ms.Content[0].Text, "<system-reminder>") {
		t.Errorf("mid-conversation system content: %+v", ms.Content)
	}
	// assistant: thinking (with signature, empty text) + tool_use
	as := ir.Messages[2].Content
	if len(as) != 2 || as[0].Kind != KindThinking || as[0].ThinkingSignature == "" || as[1].Kind != KindToolUse {
		t.Fatalf("assistant content: kinds=%v sig=%d", kinds(as), len(as[0].ThinkingSignature))
	}
	if as[1].ToolCallID != "toolu_bdrk_01DvBxF543NYes3fUgqodtEL" || as[1].ToolName != "Read" || !strings.Contains(string(as[1].ToolInput), "sample.txt") {
		t.Errorf("tool_use: %+v", as[1])
	}
	// user: tool_result with string content
	tr := ir.Messages[3].Content[0]
	if tr.Kind != KindToolResult || tr.ToolResultID != as[1].ToolCallID {
		t.Errorf("tool_result should reference the tool_use id: %+v", tr)
	}
	var s string
	if json.Unmarshal(tr.ToolResult, &s) != nil || !strings.Contains(s, "hello world") {
		t.Errorf("tool_result content: %s", tr.ToolResult)
	}
}

func TestAnthropicToIR_ClaudeCode_ToolUseCall(t *testing.T) {
	ir, err := anthropicRequest{}.ToIR(fixture(t, "claude-code/02-tool-use-call.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Messages) != 2 || ir.Messages[0].Role != RoleUser || ir.Messages[1].Role != RoleSystem {
		t.Errorf("roles: %v", roles(ir.Messages))
	}
	if ir.ToolChoice.Mode != "" { // Claude Code sends no tool_choice
		t.Errorf("tool_choice should be unset, got %+v", ir.ToolChoice)
	}
}

// ---- Anthropic → IR → OpenAI Chat: the Claude Code → GPT direction --------------------------

func TestAnthropicToChat_ClaudeCode_ToolResultFollowup(t *testing.T) {
	ir, err := anthropicRequest{}.ToIR(fixture(t, "claude-code/03-tool-result-followup.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := openAIChatRequest{}.FromIR(ir, "gpt-x", Options{DefaultMaxTokens: DefaultMaxTokens})
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["model"] != "gpt-x" {
		t.Errorf("model rewrite: %v", m["model"])
	}
	// Anthropic max_tokens → max_completion_tokens (not max_tokens)
	if m["max_completion_tokens"] != float64(64000) || m["max_tokens"] != nil {
		t.Errorf("max tokens mapping: mct=%v mt=%v", m["max_completion_tokens"], m["max_tokens"])
	}
	// streaming must force include_usage so the gateway can meter
	if so, _ := m["stream_options"].(map[string]any); m["stream"] != true || so == nil || so["include_usage"] != true {
		t.Errorf("stream_options.include_usage must be forced on: stream=%v so=%v", m["stream"], m["stream_options"])
	}
	msgs := m["messages"].([]any)
	// Expected sequence:
	//  0 system (hoisted 3 blocks joined)
	//  1 user (two text blocks joined)
	//  2 system (mid-conversation, string)
	//  3 assistant with tool_calls (thinking dropped)
	//  4 tool (tool_result → role:tool)
	//  5 system (mid-conversation, block)
	wantRoles := []string{"system", "user", "system", "assistant", "tool", "system"}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("want %d chat messages, got %d: %v", len(wantRoles), len(msgs), chatRoles(msgs))
	}
	for i, r := range wantRoles {
		if msgs[i].(map[string]any)["role"] != r {
			t.Errorf("messages[%d].role = %v want %s (all: %v)", i, msgs[i].(map[string]any)["role"], r, chatRoles(msgs))
		}
	}
	sys0 := msgs[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(sys0, "x-anthropic-billing-header") || !strings.Contains(sys0, "You are a Claude agent") {
		t.Errorf("hoisted system should join all 3 blocks: %.120s", sys0)
	}
	asst := msgs[3].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant should carry exactly 1 tool_call, got %v", asst)
	}
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if tc["id"] != "toolu_bdrk_01DvBxF543NYes3fUgqodtEL" || tc["type"] != "function" || fn["name"] != "Read" {
		t.Errorf("tool_call: %v", tc)
	}
	args, ok := fn["arguments"].(string)
	if !ok || !json.Valid([]byte(args)) || !strings.Contains(args, "sample.txt") {
		t.Errorf("arguments must be a JSON-encoded string: %v", fn["arguments"])
	}
	if _, has := asst["content"]; has && asst["content"] != nil && asst["content"] != "" {
		// thinking must not leak into content
		if s, _ := asst["content"].(string); strings.Contains(s, "thinking") {
			t.Errorf("thinking leaked into assistant content: %v", asst["content"])
		}
	}
	tool := msgs[4].(map[string]any)
	if tool["tool_call_id"] != "toolu_bdrk_01DvBxF543NYes3fUgqodtEL" || !strings.Contains(tool["content"].(string), "hello world") {
		t.Errorf("tool message: %v", tool)
	}
	tools := m["tools"].([]any)
	if len(tools) != 22 {
		t.Errorf("want 22 tools, got %d", len(tools))
	}
	t0 := tools[0].(map[string]any)
	if t0["type"] != "function" || t0["function"].(map[string]any)["parameters"] == nil {
		t.Errorf("tool def shape: %v", t0)
	}
	// Anthropic-only fields must not leak onto the OpenAI wire.
	for _, k := range []string{"system", "thinking", "metadata", "output_config", "context_management", "stop_sequences", "anthropic_version"} {
		if _, has := m[k]; has {
			t.Errorf("anthropic field %q leaked into chat request", k)
		}
	}
}

// ---- Anthropic → IR → Anthropic must be semantically lossless (round-trip) -------------------

func TestAnthropicRoundTrip_ClaudeCode(t *testing.T) {
	for _, f := range []string{"01-text-only", "02-tool-use-call", "03-tool-result-followup"} {
		t.Run(f, func(t *testing.T) {
			in := fixture(t, "claude-code/"+f+".request.json")
			ir, err := anthropicRequest{}.ToIR(in)
			if err != nil {
				t.Fatal(err)
			}
			out, err := anthropicRequest{}.FromIR(ir, ir.Model, Options{})
			if err != nil {
				t.Fatal(err)
			}
			ir2, err := anthropicRequest{}.ToIR(out)
			if err != nil {
				t.Fatalf("re-parse: %v", err)
			}
			// Structural equality of the IR after a full round trip.
			a, _ := json.Marshal(ir)
			b, _ := json.Marshal(ir2)
			if string(a) != string(b) {
				t.Errorf("IR changed across round trip\n first: %.300s\nsecond: %.300s", a, b)
			}
			// The thinking signature must survive verbatim (Anthropic rejects replays without it).
			m := mustJSON(t, out)
			if f == "03-tool-result-followup" {
				msgs := m["messages"].([]any)
				asst := msgs[2].(map[string]any)["content"].([]any)
				th := asst[0].(map[string]any)
				if th["type"] != "thinking" || th["signature"] == "" {
					t.Errorf("thinking block/signature not preserved: %v", th)
				}
				if asst[1].(map[string]any)["type"] != "tool_use" {
					t.Errorf("tool_use not preserved: %v", asst[1])
				}
			}
		})
	}
}

// ---- OpenAI Chat → IR → Anthropic: the reverse direction (direction 3) -----------------------

func TestChatToAnthropic_ToolCallingShape(t *testing.T) {
	in := []byte(`{
	  "model":"gpt-x","stream":true,"max_tokens":300,"temperature":0.2,"stop":["END"],
	  "messages":[
	    {"role":"system","content":"You are terse."},
	    {"role":"user","content":"What is in notes.txt?"},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"notes.txt\"}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"alpha\nbeta"},
	    {"role":"user","content":[{"type":"text","text":"thanks"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}
	  ],
	  "tools":[{"type":"function","function":{"name":"read_file","description":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},
	           {"type":"web_search"}],
	  "tool_choice":{"type":"function","function":{"name":"read_file"}}
	}`)
	ir, err := openAIChatRequest{}.ToIR(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.System) != 1 || ir.System[0].Text != "You are terse." {
		t.Errorf("system hoist: %+v", ir.System)
	}
	if len(ir.Tools) != 1 { // web_search (hosted) dropped
		t.Errorf("want 1 function tool, got %d", len(ir.Tools))
	}
	if ir.ToolChoice.Mode != ToolChoiceNamed || ir.ToolChoice.Name != "read_file" {
		t.Errorf("tool_choice: %+v", ir.ToolChoice)
	}
	if ir.MaxTokens == nil || *ir.MaxTokens != 300 || len(ir.Stop) != 1 || ir.Stop[0] != "END" {
		t.Errorf("max_tokens/stop: %v %v", ir.MaxTokens, ir.Stop)
	}
	out, err := anthropicRequest{}.FromIR(ir, "claude-real", Options{DefaultMaxTokens: DefaultMaxTokens})
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["model"] != "claude-real" || m["max_tokens"] != float64(300) || m["stream"] != true {
		t.Errorf("top-level: model=%v max_tokens=%v stream=%v", m["model"], m["max_tokens"], m["stream"])
	}
	if ss, _ := m["stop_sequences"].([]any); len(ss) != 1 || ss[0] != "END" {
		t.Errorf("stop → stop_sequences: %v", m["stop_sequences"])
	}
	sys := m["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != "You are terse." {
		t.Errorf("system: %v", m["system"])
	}
	msgs := m["messages"].([]any)
	// user / assistant(tool_use) / user(tool_result + text + image merged) — role:tool becomes a
	// user turn and merges with the following user turn (Anthropic forbids consecutive same roles).
	wantRoles := []string{"user", "assistant", "user"}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("want %v, got %v", wantRoles, chatRoles(msgs))
	}
	for i, r := range wantRoles {
		if msgs[i].(map[string]any)["role"] != r {
			t.Errorf("messages[%d].role=%v want %s", i, msgs[i].(map[string]any)["role"], r)
		}
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	tu := asst[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "read_file" {
		t.Errorf("tool_use: %v", tu)
	}
	if inp, _ := tu["input"].(map[string]any); inp == nil || inp["path"] != "notes.txt" {
		t.Errorf("tool_use.input must be a JSON object (not a string): %v", tu["input"])
	}
	last := msgs[2].(map[string]any)["content"].([]any)
	if len(last) != 3 {
		t.Fatalf("merged user turn should have tool_result+text+image, got %d: %v", len(last), last)
	}
	tr := last[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" || tr["content"] != "alpha\nbeta" {
		t.Errorf("tool_result: %v", tr)
	}
	if last[1].(map[string]any)["type"] != "text" || last[2].(map[string]any)["type"] != "image" {
		t.Errorf("text/image order: %v", last)
	}
	img := last[2].(map[string]any)["source"].(map[string]any)
	if img["type"] != "base64" || img["media_type"] != "image/png" || img["data"] != "AAAA" {
		t.Errorf("image source: %v", img)
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "read_file" {
		t.Errorf("tool_choice: %v", tc)
	}
	// OpenAI-only fields must not leak.
	for _, k := range []string{"stop", "max_completion_tokens", "stream_options", "response_format", "parallel_tool_calls"} {
		if _, has := m[k]; has {
			t.Errorf("openai field %q leaked into anthropic request", k)
		}
	}
}

func TestChatToAnthropic_DefaultMaxTokensApplied(t *testing.T) {
	in := []byte(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`) // no max tokens
	ir, err := openAIChatRequest{}.ToIR(in)
	if err != nil {
		t.Fatal(err)
	}
	if ir.MaxTokens != nil {
		t.Fatalf("MaxTokens should be nil when source omits it, got %v", *ir.MaxTokens)
	}
	out, err := anthropicRequest{}.FromIR(ir, "c", Options{DefaultMaxTokens: DefaultMaxTokens})
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("default max_tokens not applied: %v", m["max_tokens"])
	}
	// With no default configured, Anthropic's required field cannot be satisfied → error, not 0.
	var a anthropicRequest
	if _, err := a.FromIR(ir, "c", Options{}); err == nil {
		t.Error("expected error when max_tokens missing and no default configured")
	}
}

// ---- helpers ------------------------------------------------------------------------------

func kinds(cs []Content) []ContentKind {
	out := make([]ContentKind, len(cs))
	for i, c := range cs {
		out[i] = c.Kind
	}
	return out
}

func roles(ms []Message) []Role {
	out := make([]Role, len(ms))
	for i, m := range ms {
		out[i] = m.Role
	}
	return out
}

func chatRoles(msgs []any) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i], _ = m.(map[string]any)["role"].(string)
	}
	return out
}
