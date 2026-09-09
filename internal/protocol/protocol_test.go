package protocol

import (
	"encoding/json"
	"testing"
)

func TestDetect(t *testing.T) {
	cases := map[string]Protocol{
		"/v1/chat/completions":        OpenAIChat,
		"/openai/v1/chat/completions": OpenAIChat,
		"/v1/responses":               OpenAIResponses,
		"/v1/messages":                Anthropic,
		"/anthropic/v1/messages":      Anthropic,
	}
	for path, want := range cases {
		got, ok := Detect(path)
		if !ok || got != want {
			t.Errorf("Detect(%q) = %q,%v want %q", path, got, ok, want)
		}
	}
	if _, ok := Detect("/v1/embeddings"); ok {
		t.Error("embeddings should not be detected")
	}
}

func TestRewriteKeepsNestedAndInjectsUsage(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"metadata":{"a":1}}`)
	r, err := ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Rewrite(OpenAIChat, "global.anthropic.claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "global.anthropic.claude-sonnet-5" {
		t.Errorf("model not rewritten: %v", m["model"])
	}
	so, _ := m["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage not injected: %v", m["stream_options"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "hi" {
		t.Errorf("nested content altered: %v", m["messages"])
	}
	// Anthropic protocol must not get stream_options.
	out2, _ := r.Rewrite(Anthropic, "x")
	var m2 map[string]any
	_ = json.Unmarshal(out2, &m2)
	if _, ok := m2["stream_options"]; ok {
		t.Error("stream_options must not be injected for anthropic")
	}
}

func TestRewritePreservesExistingStreamOptions(t *testing.T) {
	r, _ := ParseRequest([]byte(`{"model":"m","stream":true,"stream_options":{"include_obfuscation":false}}`))
	out, _ := r.Rewrite(OpenAIChat, "pm")
	var m struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	_ = json.Unmarshal(out, &m)
	if m.StreamOptions["include_obfuscation"] != false || m.StreamOptions["include_usage"] != true {
		t.Errorf("stream_options merge wrong: %v", m.StreamOptions)
	}
}

func TestParseRequestMissingModel(t *testing.T) {
	if _, err := ParseRequest([]byte(`{"messages":[]}`)); err != ErrMissingModel {
		t.Errorf("want ErrMissingModel, got %v", err)
	}
	if _, err := ParseRequest([]byte(`not json`)); err == nil {
		t.Error("want error on invalid json")
	}
}

func TestOpenAIChatUsageNormalization(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":300,"prompt_tokens_details":{"cached_tokens":800},"completion_tokens_details":{"reasoning_tokens":120}}}`)
	u := ParseUsage(OpenAIChat, body)
	want := Usage{Input: 200, Output: 300, CacheRead: 800, Reasoning: 120, Found: true}
	if u != want {
		t.Errorf("got %+v want %+v", u, want)
	}
}

func TestOpenAIChatStream(t *testing.T) {
	s := NewStreamParser(OpenAIChat)
	s.Feed("", []byte(`{"id":"x","choices":[{"delta":{"content":"hi"}}],"usage":null}`))
	s.Feed("", []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	s.Feed("", []byte(`[DONE]`))
	u := s.Usage()
	if !u.Found || u.Input != 10 || u.Output != 5 {
		t.Errorf("got %+v", u)
	}
}

func TestResponsesStream(t *testing.T) {
	s := NewStreamParser(OpenAIResponses)
	s.Feed("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"x"}`))
	s.Feed("response.completed", []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":50,"output_tokens":20,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":8}}}}`))
	u := s.Usage()
	want := Usage{Input: 20, Output: 20, CacheRead: 30, Reasoning: 8, Found: true}
	if u != want {
		t.Errorf("got %+v want %+v", u, want)
	}
}

func TestAnthropicStream(t *testing.T) {
	s := NewStreamParser(Anthropic)
	s.Feed("message_start", []byte(`{"type":"message_start","message":{"usage":{"input_tokens":13,"cache_creation_input_tokens":100,"cache_read_input_tokens":7,"output_tokens":2}}}`))
	s.Feed("content_block_delta", []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`))
	s.Feed("message_delta", []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`))
	u := s.Usage()
	want := Usage{Input: 13, Output: 42, CacheRead: 7, CacheWrite: 100, Found: true}
	if u != want {
		t.Errorf("got %+v want %+v", u, want)
	}
}

func TestAnthropicNonStream(t *testing.T) {
	u := ParseUsage(Anthropic, []byte(`{"usage":{"input_tokens":13,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}`))
	if u != (Usage{Input: 13, Output: 5, Found: true}) {
		t.Errorf("got %+v", u)
	}
}
