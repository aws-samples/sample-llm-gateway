package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
)

// Cross-protocol relay tests (M6). The two acceptance directions are exercised end-to-end
// through the handler against the fake upstream: Anthropic client → OpenAI Chat provider
// (Claude Code → GPT) and OpenAI Responses client → Anthropic provider (Codex → Claude).
// Metering must reflect the provider's real usage, in the provider's protocol.

func loadTranslationRoutes(h *harness) {
	h.rt.Load(&controlplane.Routes{Version: "v", Models: []controlplane.ModelRoute{
		{ModelCode: "claude-to-gpt", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "primary", ProviderModelCode: "gpt-x-real", ProviderProtocol: "openai_chat", Priority: 1, Weight: 100},
		}},
		{ModelCode: "codex-to-claude", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "primary", ProviderModelCode: "global.anthropic.claude-x", ProviderProtocol: "anthropic", Priority: 1, Weight: 100},
		}},
	}})
}

func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, s)
	}
	return m
}

func (u *fakeUpstream) snapshot() (map[string]any, map[string][]string, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastBody, u.lastHdr, u.lastPath
}

// ---- Anthropic client → OpenAI Chat provider -------------------------------------------------

func TestTranslateAnthropicToChatNonStream(t *testing.T) {
	h := newHarness(t)
	loadTranslationRoutes(h)
	req := `{"model":"claude-to-gpt","max_tokens":64,"system":"be terse",
	  "messages":[{"role":"user","content":"read a.txt"}],
	  "tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]}`
	resp, body := post(t, h.gw.URL+"/v1/messages", "sk-client", req, map[string]string{
		"anthropic-version": "2023-06-01", "anthropic-beta": "foo-2026",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type %q", ct)
	}

	// Client sees an Anthropic message with a tool_use block.
	m := decodeJSON(t, body)
	if m["type"] != "message" || m["role"] != "assistant" || m["stop_reason"] != "tool_use" {
		t.Errorf("anthropic envelope wrong: %v", m)
	}
	content, _ := m["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want text + tool_use blocks, got %v", content)
	}
	if b := content[0].(map[string]any); b["type"] != "text" || b["text"] != "Running it." {
		t.Errorf("text block: %v", b)
	}
	if b := content[1].(map[string]any); b["type"] != "tool_use" || b["name"] != "Read" || b["id"] != "call_9" ||
		b["input"].(map[string]any)["file_path"] != "a.txt" {
		t.Errorf("tool_use block: %v", b)
	}
	// Usage is rendered in Anthropic shape from the provider's real counts (prompt 20 = 5 + 15 cached).
	usage := m["usage"].(map[string]any)
	if usage["input_tokens"] != float64(5) || usage["cache_read_input_tokens"] != float64(15) || usage["output_tokens"] != float64(4) {
		t.Errorf("usage: %v", usage)
	}

	// Upstream got an OpenAI Chat body at the chat path, with Anthropic-only headers stripped.
	up, hdr, path := h.up.snapshot()
	if !strings.HasSuffix(path, "/chat/completions") {
		t.Errorf("upstream path %q", path)
	}
	if up["model"] != "gpt-x-real" || up["max_completion_tokens"] != float64(64) || up["max_tokens"] != nil || up["system"] != nil {
		t.Errorf("upstream body: %v", up)
	}
	msgs := up["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "user" {
		t.Errorf("messages: %v", msgs)
	}
	tools := up["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" ||
		tools[0].(map[string]any)["function"].(map[string]any)["name"] != "Read" {
		t.Errorf("tools: %v", tools)
	}
	if up["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort should be none when tools are present (Bedrock gpt-5.x), got %v", up["reasoning_effort"])
	}
	if len(hdr["Anthropic-Version"]) != 0 || len(hdr["Anthropic-Beta"]) != 0 {
		t.Errorf("anthropic headers must not reach an OpenAI endpoint: %v", hdr)
	}
	if hdr["X-Api-Key"] == nil {
		t.Errorf("provider auth missing: %v", hdr)
	}

	// Metering: usage parsed from the provider's (OpenAI) body, model codes from the route.
	reps := h.cp.waitReports(1)
	if len(reps) != 1 {
		t.Fatal("no usage report")
	}
	r := reps[0]
	if r.ModelCode != "claude-to-gpt" || r.ProviderModelCode != "gpt-x-real" || r.StatusCode != 200 ||
		r.InputTokens != 5 || r.CacheReadTokens != 15 || r.OutputTokens != 4 || r.ReasoningTokens != 2 {
		t.Errorf("bad report: %+v", r)
	}
}

func TestTranslateAnthropicToChatStream(t *testing.T) {
	h := newHarness(t)
	loadTranslationRoutes(h)
	resp, body := post(t, h.gw.URL+"/v1/messages", "sk-client",
		`{"model":"claude-to-gpt","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d ct %q body %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	// The client must see a well-formed Anthropic event sequence, not OpenAI chunks.
	for _, want := range []string{
		"event: message_start", `"type":"message_start"`,
		"event: content_block_start", `"type":"text"`,
		"event: content_block_delta", `"text":"hi"`,
		"event: content_block_stop",
		"event: message_delta", `"stop_reason":"end_turn"`, `"output_tokens":4`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") || strings.Contains(body, "chat.completion") {
		t.Errorf("OpenAI framing leaked to the Anthropic client:\n%s", body)
	}
	up, _, _ := h.up.snapshot()
	if so, _ := up["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("include_usage must be forced on for the OpenAI upstream: %v", up["stream_options"])
	}
	reps := h.cp.waitReports(1)
	if len(reps) != 1 || reps[0].StatusCode != 200 || reps[0].InputTokens != 5 || reps[0].CacheReadTokens != 15 ||
		reps[0].OutputTokens != 4 || reps[0].ReasoningTokens != 2 {
		t.Errorf("bad report: %+v", reps)
	}
}

// ---- OpenAI Responses client → Anthropic provider --------------------------------------------

func TestTranslateResponsesToAnthropicNonStream(t *testing.T) {
	h := newHarness(t)
	loadTranslationRoutes(h)
	req := `{"model":"codex-to-claude","instructions":"be terse","store":false,
	  "input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	resp, body := post(t, h.gw.URL+"/v1/responses", "sk-client", req, map[string]string{"openai-beta": "responses=v1"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	m := decodeJSON(t, body)
	if m["object"] != "response" || m["status"] != "completed" {
		t.Errorf("responses envelope wrong: %v", m)
	}
	out := m["output"].([]any)
	if len(out) != 1 || out[0].(map[string]any)["type"] != "message" {
		t.Fatalf("output: %v", out)
	}
	part := out[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "hi" {
		t.Errorf("output_text: %v", part)
	}
	// Responses input_tokens includes cached tokens (11 uncached + 3 cache read + 2 cache write).
	usage := m["usage"].(map[string]any)
	if usage["input_tokens"] != float64(16) || usage["output_tokens"] != float64(7) ||
		usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(3) {
		t.Errorf("usage: %v", usage)
	}

	up, hdr, path := h.up.snapshot()
	if !strings.HasSuffix(path, "/messages") {
		t.Errorf("upstream path %q", path)
	}
	// Codex omits max_output_tokens; Anthropic requires max_tokens → gateway default (8192).
	if up["model"] != "global.anthropic.claude-x" || up["max_tokens"] != float64(8192) {
		t.Errorf("upstream body: model=%v max_tokens=%v", up["model"], up["max_tokens"])
	}
	if !strings.Contains(string(mustMarshal(t, up["system"])), "be terse") {
		t.Errorf("instructions → system: %v", up["system"])
	}
	for _, k := range []string{"input", "instructions", "store", "max_output_tokens"} {
		if _, has := up[k]; has {
			t.Errorf("Responses field %q leaked to Anthropic upstream", k)
		}
	}
	if len(hdr["Anthropic-Version"]) == 0 {
		t.Errorf("anthropic-version must be set for an Anthropic upstream: %v", hdr)
	}
	if len(hdr["Openai-Beta"]) != 0 {
		t.Errorf("openai-beta must not reach an Anthropic endpoint: %v", hdr)
	}

	reps := h.cp.waitReports(1)
	if len(reps) != 1 {
		t.Fatal("no usage report")
	}
	r := reps[0]
	if r.ModelCode != "codex-to-claude" || r.ProviderModelCode != "global.anthropic.claude-x" || r.StatusCode != 200 ||
		r.InputTokens != 11 || r.OutputTokens != 7 || r.CacheReadTokens != 3 || r.CacheWriteTokens != 2 {
		t.Errorf("bad report: %+v", r)
	}
}

func TestTranslateResponsesToAnthropicStream(t *testing.T) {
	h := newHarness(t)
	loadTranslationRoutes(h)
	resp, body := post(t, h.gw.URL+"/v1/responses", "sk-client",
		`{"model":"codex-to-claude","stream":true,"store":false,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d ct %q body %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	// Frames are data-only with data.type, matching what Bedrock/OpenAI emit and Codex consumes
	// (see translate/testdata/fixtures/codex/*.response.sse).
	for _, want := range []string{
		`"type":"response.created"`,
		`"type":"response.output_item.added"`,
		`"type":"response.output_text.delta"`, `"delta":"hi"`,
		`"type":"response.output_item.done"`,
		`"type":"response.completed"`, `"status":"completed"`, `"output_tokens":9`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "event: message_start") || strings.Contains(body, "content_block_delta") {
		t.Errorf("Anthropic framing leaked to the Responses client:\n%s", body)
	}
	up, _, _ := h.up.snapshot()
	if up["stream"] != true || up["max_tokens"] != float64(8192) {
		t.Errorf("upstream body: %v", up)
	}
	// message_start input 11, message_delta output 9.
	reps := h.cp.waitReports(1)
	if len(reps) != 1 || reps[0].StatusCode != 200 || reps[0].InputTokens != 11 || reps[0].OutputTokens != 9 {
		t.Errorf("bad report: %+v", reps)
	}
}

// ---- error paths -----------------------------------------------------------------------------

// An upstream error body arrives in the provider's format; the client must get it in its own.
func TestTranslateUpstreamErrorRenderedInClientFormat(t *testing.T) {
	h := newHarness(t)
	loadTranslationRoutes(h)
	h.up.mu.Lock()
	h.up.fail = 400 // fake returns {"error":"boom"}; 4xx is not failed over
	h.up.mu.Unlock()
	resp, body := post(t, h.gw.URL+"/v1/messages", "sk-client",
		`{"model":"claude-to-gpt","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("upstream status must be preserved, got %d %s", resp.StatusCode, body)
	}
	m := decodeJSON(t, body)
	if m["type"] != "error" || m["error"].(map[string]any)["message"] != "boom" {
		t.Errorf("error must be in Anthropic shape: %s", body)
	}
	reps := h.cp.waitReports(1)
	if len(reps) != 1 || reps[0].StatusCode != 400 || reps[0].InputTokens != 0 {
		t.Errorf("bad report: %+v", reps)
	}
}

// A request the codec cannot express (stateful Responses) is a 400 in the client's own format,
// and the upstream is never called.
func TestTranslateUntranslatableRequestIs400(t *testing.T) {
	h := newHarness(t)
	loadTranslationRoutes(h)
	resp, body := post(t, h.gw.URL+"/v1/responses", "sk-client",
		`{"model":"codex-to-claude","previous_response_id":"resp_123","input":"hi"}`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d %s", resp.StatusCode, body)
	}
	m := decodeJSON(t, body)
	e, _ := m["error"].(map[string]any)
	if e["type"] != "invalid_request_error" || !strings.Contains(e["message"].(string), "previous_response_id") {
		t.Errorf("error must be OpenAI-shaped and name the field: %s", body)
	}
	if up, _, _ := h.up.snapshot(); up != nil {
		t.Error("upstream must not be called")
	}
}

// server.default_max_tokens overrides the built-in default.
func TestTranslateDefaultMaxTokensFromConfig(t *testing.T) {
	h := newHarnessCfg(t, func(c *config.Config) { c.Server.DefaultMaxTokens = 4096 })
	loadTranslationRoutes(h)
	resp, body := post(t, h.gw.URL+"/v1/responses", "sk-client",
		`{"model":"codex-to-claude","store":false,"input":"hi"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if up, _, _ := h.up.snapshot(); up["max_tokens"] != float64(4096) {
		t.Errorf("configured default not applied: %v", up["max_tokens"])
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- the remaining four directions (M8) ------------------------------------------------------
//
// One table entry per direction, non-stream and stream, through the handler. Each checks the
// client-side envelope, one upstream-body property proving the target codec ran, and the metering
// report parsed from the upstream protocol.

func loadRemainingDirectionRoutes(h *harness) {
	h.rt.Load(&controlplane.Routes{Version: "v", Models: []controlplane.ModelRoute{
		// backup serves openai_responses (fake up2); primary serves anthropic + openai_chat (fake up).
		{ModelCode: "chat-to-responses", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "backup", ProviderModelCode: "gpt-x-real", ProviderProtocol: "openai_responses", Priority: 1, Weight: 100}}},
		{ModelCode: "chat-to-claude", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "primary", ProviderModelCode: "global.anthropic.claude-x", ProviderProtocol: "anthropic", Priority: 1, Weight: 100}}},
		{ModelCode: "claude-to-responses", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "backup", ProviderModelCode: "gpt-x-real", ProviderProtocol: "openai_responses", Priority: 1, Weight: 100}}},
		{ModelCode: "responses-to-chat", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "primary", ProviderModelCode: "gpt-x-real", ProviderProtocol: "openai_chat", Priority: 1, Weight: 100}}},
	}})
}

func TestTranslateRemainingDirections(t *testing.T) {
	type rep struct{ in, out, cr, cw, rs int64 }
	cases := []struct {
		name, path, body string
		upstream         func(h *harness) *fakeUpstream
		wantClient       []string // substrings the client body must contain
		wantUpstream     func(t *testing.T, up map[string]any)
		wantReport       rep
	}{
		{
			name: "chat→responses non-stream", path: "/v1/chat/completions",
			body:       `{"model":"chat-to-responses","messages":[{"role":"system","content":"terse"},{"role":"user","content":"hi"}]}`,
			upstream:   func(h *harness) *fakeUpstream { return h.up2 },
			wantClient: []string{`"object":"chat.completion"`, `"content":"hi"`, `"finish_reason":"stop"`, `"prompt_tokens":30`, `"cached_tokens":10`},
			wantUpstream: func(t *testing.T, up map[string]any) {
				if up["model"] != "gpt-x-real" || up["store"] != false || up["instructions"] != "terse" || up["input"] == nil || up["messages"] != nil {
					t.Errorf("responses upstream body: %v", up)
				}
			},
			wantReport: rep{in: 20, out: 6, cr: 10, rs: 2},
		},
		{
			name: "chat→responses stream", path: "/v1/chat/completions",
			body:       `{"model":"chat-to-responses","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			upstream:   func(h *harness) *fakeUpstream { return h.up2 },
			wantClient: []string{`"object":"chat.completion.chunk"`, `"role":"assistant"`, `"content":"hi"`, `"finish_reason":"stop"`, `"prompt_tokens":30`, "data: [DONE]"},
			wantUpstream: func(t *testing.T, up map[string]any) {
				if up["stream"] != true || up["store"] != false {
					t.Errorf("responses upstream body: %v", up)
				}
			},
			wantReport: rep{in: 20, out: 6, cr: 10, rs: 2},
		},
		{
			name: "chat→anthropic non-stream", path: "/v1/chat/completions",
			body:       `{"model":"chat-to-claude","messages":[{"role":"user","content":"hi"}]}`,
			upstream:   func(h *harness) *fakeUpstream { return h.up },
			wantClient: []string{`"object":"chat.completion"`, `"content":"hi"`, `"prompt_tokens":16`, `"cached_tokens":3`, `"completion_tokens":7`},
			wantUpstream: func(t *testing.T, up map[string]any) {
				// SDK omitted max_tokens → gateway default; Chat-only fields stay out.
				if up["max_tokens"] != float64(8192) || up["model"] != "global.anthropic.claude-x" || up["max_completion_tokens"] != nil {
					t.Errorf("anthropic upstream body: %v", up)
				}
			},
			wantReport: rep{in: 11, out: 7, cr: 3, cw: 2},
		},
		{
			name: "chat→anthropic stream", path: "/v1/chat/completions",
			body:       `{"model":"chat-to-claude","stream":true,"max_tokens":40,"messages":[{"role":"user","content":"hi"}]}`,
			upstream:   func(h *harness) *fakeUpstream { return h.up },
			wantClient: []string{`"object":"chat.completion.chunk"`, `"content":"hi"`, `"finish_reason":"stop"`, `"completion_tokens":9`, "data: [DONE]"},
			wantUpstream: func(t *testing.T, up map[string]any) {
				if up["max_tokens"] != float64(40) || up["stream"] != true || up["stream_options"] != nil {
					t.Errorf("anthropic upstream body: %v", up)
				}
			},
			wantReport: rep{in: 11, out: 9},
		},
		{
			name: "anthropic→responses non-stream", path: "/v1/messages",
			body:       `{"model":"claude-to-responses","max_tokens":64,"system":"terse","messages":[{"role":"user","content":"hi"}]}`,
			upstream:   func(h *harness) *fakeUpstream { return h.up2 },
			wantClient: []string{`"type":"message"`, `"role":"assistant"`, `"text":"hi"`, `"stop_reason":"end_turn"`, `"input_tokens":20`, `"cache_read_input_tokens":10`},
			wantUpstream: func(t *testing.T, up map[string]any) {
				if up["max_output_tokens"] != float64(64) || up["instructions"] != "terse" || up["store"] != false || up["system"] != nil || up["max_tokens"] != nil {
					t.Errorf("responses upstream body: %v", up)
				}
			},
			wantReport: rep{in: 20, out: 6, cr: 10, rs: 2},
		},
		{
			name: "anthropic→responses stream", path: "/v1/messages",
			body:       `{"model":"claude-to-responses","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			upstream:   func(h *harness) *fakeUpstream { return h.up2 },
			wantClient: []string{"event: message_start", "event: content_block_delta", `"text":"hi"`, "event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop"},
			wantUpstream: func(t *testing.T, up map[string]any) {
				if up["stream"] != true {
					t.Errorf("responses upstream body: %v", up)
				}
			},
			wantReport: rep{in: 20, out: 6, cr: 10, rs: 2},
		},
		{
			name: "responses→chat non-stream", path: "/v1/responses",
			body:       `{"model":"responses-to-chat","store":false,"instructions":"terse","input":"hi"}`,
			upstream:   func(h *harness) *fakeUpstream { return h.up },
			wantClient: []string{`"object":"response"`, `"status":"completed"`, `"type":"function_call"`, `"call_id":"call_9"`, `"output_text"`, `"input_tokens":20`, `"cached_tokens":15`},
			wantUpstream: func(t *testing.T, up map[string]any) {
				msgs, _ := up["messages"].([]any)
				if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || up["input"] != nil || up["store"] != nil {
					t.Errorf("chat upstream body: %v", up)
				}
			},
			wantReport: rep{in: 5, out: 4, cr: 15, rs: 2},
		},
		{
			name: "responses→chat stream", path: "/v1/responses",
			body:       `{"model":"responses-to-chat","store":false,"stream":true,"input":"hi"}`,
			upstream:   func(h *harness) *fakeUpstream { return h.up },
			wantClient: []string{`"type":"response.created"`, `"type":"response.output_text.delta"`, `"delta":"hi"`, `"type":"response.completed"`, `"status":"completed"`, `"output_tokens":4`},
			wantUpstream: func(t *testing.T, up map[string]any) {
				if so, _ := up["stream_options"].(map[string]any); so["include_usage"] != true {
					t.Errorf("include_usage must be forced for a Chat upstream: %v", up)
				}
			},
			wantReport: rep{in: 5, out: 4, cr: 15, rs: 2},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			loadRemainingDirectionRoutes(h)
			resp, body := post(t, h.gw.URL+c.path, "sk-client", c.body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d body %s", resp.StatusCode, body)
			}
			for _, want := range c.wantClient {
				if !strings.Contains(body, want) {
					t.Errorf("client body missing %q:\n%s", want, body)
				}
			}
			up, _, _ := c.upstream(h).snapshot()
			if up == nil {
				t.Fatal("upstream not called")
			}
			c.wantUpstream(t, up)
			reps := h.cp.waitReports(1)
			if len(reps) != 1 {
				t.Fatal("no usage report")
			}
			r := reps[0]
			if r.StatusCode != 200 || r.InputTokens != c.wantReport.in || r.OutputTokens != c.wantReport.out ||
				r.CacheReadTokens != c.wantReport.cr || r.CacheWriteTokens != c.wantReport.cw || r.ReasoningTokens != c.wantReport.rs {
				t.Errorf("report %+v, want %+v", r, c.wantReport)
			}
		})
	}
}
