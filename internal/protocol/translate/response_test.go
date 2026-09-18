package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
)

// A real Bedrock Anthropic non-stream body (shape from the recorded /v1/messages probe).
const anthropicToolUseBody = `{"model":"claude-sonnet-5","id":"msg_bdrk_abc","type":"message","role":"assistant",
 "content":[
   {"type":"thinking","thinking":"Let me read it.","signature":"SIGBLOB=="},
   {"type":"text","text":"I'll read the file."},
   {"type":"tool_use","id":"toolu_bdrk_01","name":"Read","input":{"file_path":"/tmp/x.txt"}}
 ],
 "stop_reason":"tool_use","stop_sequence":null,
 "usage":{"input_tokens":2,"cache_creation_input_tokens":38813,"cache_read_input_tokens":0,"output_tokens":88,"output_tokens_details":{"thinking_tokens":17}}}`

// A real-shaped OpenAI Chat non-stream body with a tool call and cached prompt tokens.
const chatToolCallBody = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-x",
 "choices":[{"index":0,"message":{"role":"assistant","content":null,
   "tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/x.txt\"}"}}]},
   "finish_reason":"tool_calls"}],
 "usage":{"prompt_tokens":1000,"completion_tokens":30,"total_tokens":1030,
   "prompt_tokens_details":{"cached_tokens":800},"completion_tokens_details":{"reasoning_tokens":12}}}`

// ---- Anthropic response → IR --------------------------------------------------------------

func TestAnthropicResponseToIR(t *testing.T) {
	r, err := anthropicResponse{}.ToIR([]byte(anthropicToolUseBody))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "msg_bdrk_abc" || r.Model != "claude-sonnet-5" || r.StopReason != StopToolUse {
		t.Errorf("id/model/stop: %q %q %q", r.ID, r.Model, r.StopReason)
	}
	if k := kinds(r.Content); len(k) != 3 || k[0] != KindThinking || k[1] != KindText || k[2] != KindToolUse {
		t.Fatalf("content kinds: %v", k)
	}
	if r.Content[0].ThinkingSignature != "SIGBLOB==" || r.Content[2].ToolCallID != "toolu_bdrk_01" {
		t.Errorf("thinking/tool_use fields lost: %+v", r.Content)
	}
	// Usage normalization is delegated to protocol.ParseUsage: input=2, cache_write=38813, output=88.
	want := protocol.Usage{Input: 2, Output: 88, CacheWrite: 38813, Found: true}
	if r.Usage != want {
		t.Errorf("usage = %+v want %+v", r.Usage, want)
	}
}

// ---- OpenAI Chat response → IR → Anthropic response: the Claude Code ← GPT return path ---------

func TestChatResponseToAnthropic(t *testing.T) {
	ir, err := openAIChatResponse{}.ToIR([]byte(chatToolCallBody))
	if err != nil {
		t.Fatal(err)
	}
	if ir.StopReason != StopToolUse {
		t.Errorf("finish_reason tool_calls → StopToolUse, got %q", ir.StopReason)
	}
	// OpenAI prompt_tokens=1000 with cached=800 → IR Input=200 (uncached), CacheRead=800.
	if ir.Usage.Input != 200 || ir.Usage.CacheRead != 800 || ir.Usage.Output != 30 || ir.Usage.Reasoning != 12 {
		t.Errorf("usage: %+v", ir.Usage)
	}
	out, err := anthropicResponse{}.FromIR(ir)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["type"] != "message" || m["role"] != "assistant" || m["id"] != "chatcmpl-1" || m["model"] != "gpt-x" {
		t.Errorf("envelope: %v", m)
	}
	if m["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason: %v", m["stop_reason"])
	}
	content := m["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content should be exactly the tool_use block (content was null): %v", content)
	}
	tu := content[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "Read" {
		t.Errorf("tool_use: %v", tu)
	}
	if inp, _ := tu["input"].(map[string]any); inp == nil || inp["file_path"] != "/tmp/x.txt" {
		t.Errorf("tool_use.input must be a JSON object: %v", tu["input"])
	}
	u := m["usage"].(map[string]any)
	// Anthropic semantics: input_tokens is UNCACHED prompt; cache_read carries the 800.
	if u["input_tokens"] != float64(200) || u["cache_read_input_tokens"] != float64(800) || u["output_tokens"] != float64(30) {
		t.Errorf("anthropic usage: %v", u)
	}
	// Anthropic clients validate the message shape strictly; these must be present.
	for _, k := range []string{"id", "type", "role", "model", "content", "stop_reason", "stop_sequence", "usage"} {
		if _, has := m[k]; !has {
			t.Errorf("anthropic response missing required key %q", k)
		}
	}
}

// ---- Anthropic response → IR → OpenAI Chat response: the reverse direction ---------------------

func TestAnthropicResponseToChat(t *testing.T) {
	ir, err := anthropicResponse{}.ToIR([]byte(anthropicToolUseBody))
	if err != nil {
		t.Fatal(err)
	}
	out, err := openAIChatResponse{}.FromIR(ir)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["object"] != "chat.completion" || m["id"] != "msg_bdrk_abc" || m["model"] != "claude-sonnet-5" {
		t.Errorf("envelope: %v", m)
	}
	ch := m["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason: %v", ch["finish_reason"])
	}
	msg := ch["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] != "I'll read the file." {
		t.Errorf("message: %v", msg)
	}
	// visible thinking text → reasoning_content extension; the signature must NOT leak anywhere.
	if msg["reasoning_content"] != "Let me read it." {
		t.Errorf("reasoning_content: %v", msg["reasoning_content"])
	}
	if strings.Contains(string(out), "SIGBLOB") {
		t.Error("anthropic thinking signature leaked into chat response")
	}
	tcs := msg["tool_calls"].([]any)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if tcs[0].(map[string]any)["id"] != "toolu_bdrk_01" || fn["name"] != "Read" {
		t.Errorf("tool_call: %v", tcs[0])
	}
	if args, _ := fn["arguments"].(string); !json.Valid([]byte(args)) || !strings.Contains(args, "file_path") {
		t.Errorf("arguments must be a JSON-encoded string: %v", fn["arguments"])
	}
	// OpenAI semantics: prompt_tokens includes the cached/written portion.
	u := m["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(2+38813) || u["completion_tokens"] != float64(88) || u["total_tokens"] != float64(2+38813+88) {
		t.Errorf("chat usage: %v", u)
	}
}

// ---- usage must survive a full cross-protocol round trip (metering correctness) ---------------

func TestResponseUsageRoundTrip(t *testing.T) {
	// Chat → IR → Anthropic → IR: normalized counters identical.
	ir1, _ := openAIChatResponse{}.ToIR([]byte(chatToolCallBody))
	a, _ := anthropicResponse{}.FromIR(ir1)
	ir2, err := anthropicResponse{}.ToIR(a)
	if err != nil {
		t.Fatal(err)
	}
	// Reasoning is not representable in Anthropic usage; everything else must match.
	ir1.Usage.Reasoning = 0
	if ir1.Usage != ir2.Usage {
		t.Errorf("usage drift chat→anthropic→ir: %+v vs %+v", ir1.Usage, ir2.Usage)
	}
	// Anthropic → IR → Chat → IR.
	ir3, _ := anthropicResponse{}.ToIR([]byte(anthropicToolUseBody))
	c, _ := openAIChatResponse{}.FromIR(ir3)
	ir4, err := openAIChatResponse{}.ToIR(c)
	if err != nil {
		t.Fatal(err)
	}
	// Chat has no cache_write slot: it folds into prompt_tokens, which the parser reads back as Input.
	if ir4.Usage.Input != ir3.Usage.Input+ir3.Usage.CacheWrite || ir4.Usage.Output != ir3.Usage.Output {
		t.Errorf("usage drift anthropic→chat→ir: %+v vs %+v", ir3.Usage, ir4.Usage)
	}
}

func TestChatResponseInvalidToolArgumentsPreserved(t *testing.T) {
	body := `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,
	  "tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	ir, err := openAIChatResponse{}.ToIR([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(ir.Content[0].ToolInput) || !strings.Contains(string(ir.Content[0].ToolInput), "_raw") {
		t.Errorf("truncated arguments should be wrapped, not dropped or left invalid: %s", ir.Content[0].ToolInput)
	}
	if ir.StopReason != StopMaxTokens {
		t.Errorf("finish_reason length → StopMaxTokens, got %q", ir.StopReason)
	}
}
