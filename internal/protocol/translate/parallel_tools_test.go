package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// Two parallel tool calls from an OpenAI Chat upstream (real shape: index 0/1, id only on the
// first fragment of each, a single terminal finish_reason). Regression for the bug where the
// chat decoder deferred every ToolUseStop to finish_reason, producing overlapping tool blocks in
// the IR that made the Anthropic encoder emit a duplicate content_block_stop and the Responses
// encoder duplicate a function_call in response.completed.output. The fix restores the IR
// non-overlap contract at the decoder (see ir.go StreamEvent).
func TestParallelToolCallsChatDecoderAndEncoders(t *testing.T) {
	chunks := []string{
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","content":null},"finish_reason":null}]}`,
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":\"a\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"p\":\"b\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","model":"gpt","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	}
	var evs []StreamEvent
	dec := newChatStreamIn()
	for _, c := range chunks {
		e, err := dec.ToIR("", []byte(c))
		if err != nil {
			t.Fatalf("decode %q: %v", c, err)
		}
		evs = append(evs, e...)
	}

	// Decoder contract: tool blocks never overlap — each ToolUseStart is closed by its own
	// ToolUseStop before the next ToolUseStart.
	want := []StreamEventKind{
		EventMessageStart,
		EventToolUseStart, EventToolInputDelta, EventToolUseStop,
		EventToolUseStart, EventToolInputDelta, EventToolUseStop,
		EventMessageStop,
	}
	got := kindSeq(evs)
	if len(got) != len(want) {
		t.Fatalf("IR kind sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("IR kind[%d] = %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}

	// Anthropic encoder: every content block gets exactly one content_block_stop.
	stops := map[int]int{}
	for _, f := range parseSSE(t, driveOut(t, newAnthropicStreamOut(), evs)) {
		var d struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		_ = json.Unmarshal([]byte(f.Data), &d)
		if d.Type == "content_block_stop" {
			stops[d.Index]++
		}
	}
	for idx, n := range stops {
		if n != 1 {
			t.Errorf("anthropic: block %d got %d content_block_stop, want 1", idx, n)
		}
	}
	if len(stops) != 2 {
		t.Errorf("anthropic: want 2 tool blocks closed, got %d (%v)", len(stops), stops)
	}

	// Responses encoder: exactly two items done and two entries (no duplicate) in completed.output.
	r := driveOut(t, newResponsesStreamOut(), evs)
	done := 0
	var finalOut []any
	for _, f := range parseSSE(t, r) {
		var d struct {
			Type     string         `json:"type"`
			Response map[string]any `json:"response"`
		}
		_ = json.Unmarshal([]byte(f.Data), &d)
		if d.Type == "response.output_item.done" {
			done++
		}
		if d.Type == "response.completed" {
			finalOut, _ = d.Response["output"].([]any)
		}
	}
	if done != 2 || len(finalOut) != 2 {
		t.Errorf("responses: want 2 items done / 2 in output, got %d / %d\n%s", done, len(finalOut), strings.TrimSpace(string(r)))
	}
}

// An interleaved fragment (index 0 → 1 → 0) violates the non-overlap contract; the decoder must
// fail loudly rather than silently split the tool's arguments across two blocks (design §7).
func TestParallelToolCallsInterleavedIndexIsRejected(t *testing.T) {
	chunks := []string{
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read","arguments":"{\"p\":"}}]}}]}`,
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}}]}`, // late fragment for closed index 0
	}
	dec := newChatStreamIn()
	var lastErr error
	for _, c := range chunks {
		if _, err := dec.ToIR("", []byte(c)); err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		t.Fatal("expected an error on the interleaved (already-closed index 0) fragment")
	}
	if !strings.Contains(lastErr.Error(), "interleaved") {
		t.Fatalf("error should explain the interleave, got %v", lastErr)
	}
}

// Real traffic: GPT-5.6 on Bedrock asked to call exec_command twice in one turn (recorded via the
// official openai SDK, see fixtures/openai-sdk-chat/03-*). The stream confirms the non-overlap
// assumption — all ten index-0 fragments arrive before index 1 opens — and the encoders must
// produce exactly one block/item per call for both Anthropic and Responses clients.
func TestParallelToolCallsRealFixture(t *testing.T) {
	frames := parseSSE(t, fixture(t, "openai-sdk-chat/03-parallel-tool-calls.response.sse"))
	evs := driveIn(t, newChatStreamIn(), frames)

	// IR contract: start,delta...,stop for call 0 fully before start of call 1.
	var seq []string
	for _, e := range evs {
		switch e.Kind {
		case EventToolUseStart:
			seq = append(seq, "start:"+e.ToolCallID)
		case EventToolUseStop:
			seq = append(seq, "stop:"+e.ToolCallID)
		}
	}
	if strings.Join(seq, ",") != "start:call_0,stop:call_0,start:call_1,stop:call_1" {
		t.Fatalf("tool blocks must not overlap in IR: %v", seq)
	}
	if a0, a1 := joinToolInput(evs, 1), joinToolInput(evs, 2); !json.Valid([]byte(a0)) || !json.Valid([]byte(a1)) || a0 == a1 {
		t.Errorf("arguments reassembled per call: %q %q", a0, a1)
	}

	// Anthropic client view: two tool_use blocks, each opened and closed exactly once, ids kept.
	a := parseSSE(t, driveOut(t, newAnthropicStreamOut(), evs))
	starts, stops := map[int]int{}, map[int]int{}
	var ids []string
	for _, f := range a {
		var d struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content_block"`
		}
		_ = json.Unmarshal([]byte(f.Data), &d)
		switch d.Type {
		case "content_block_start":
			starts[d.Index]++
			if d.Block.Type == "tool_use" {
				ids = append(ids, d.Block.ID)
			}
		case "content_block_stop":
			stops[d.Index]++
		}
	}
	if len(ids) != 2 || ids[0] != "call_0" || ids[1] != "call_1" {
		t.Errorf("tool_use ids: %v", ids)
	}
	for idx := range starts {
		if starts[idx] != 1 || stops[idx] != 1 {
			t.Errorf("block %d: %d start / %d stop", idx, starts[idx], stops[idx])
		}
	}

	// Responses client view: two function_call items, each done once, both in completed.output.
	r := parseSSE(t, driveOut(t, newResponsesStreamOut(), evs))
	done := map[string]int{}
	var final []any
	for _, f := range r {
		var d struct {
			Type     string         `json:"type"`
			Item     map[string]any `json:"item"`
			Response map[string]any `json:"response"`
		}
		_ = json.Unmarshal([]byte(f.Data), &d)
		if d.Type == "response.output_item.done" {
			done[d.Item["call_id"].(string)]++
		}
		if d.Type == "response.completed" {
			final, _ = d.Response["output"].([]any)
		}
	}
	if len(done) != 2 || done["call_0"] != 1 || done["call_1"] != 1 || len(final) != 2 {
		t.Errorf("responses: done=%v completed.output=%d", done, len(final))
	}
}

// The follow-up request carries both tool results; every target must keep both pairs aligned.
func TestParallelToolOutputsFollowupAllTargets(t *testing.T) {
	ir, err := openAIChatRequest{}.ToIR(fixture(t, "openai-sdk-chat/04-parallel-tool-outputs-followup.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var calls, results int
	for _, m := range ir.Messages {
		for _, c := range m.Content {
			switch c.Kind {
			case KindToolUse:
				calls++
			case KindToolResult:
				results++
			}
		}
	}
	if calls != 2 || results != 2 {
		t.Fatalf("IR: %d tool_use / %d tool_result", calls, results)
	}
	anth, err := anthropicRequest{}.FromIR(ir, "claude", Options{DefaultMaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(anth), `"type":"tool_use"`) != 2 || strings.Count(string(anth), `"type":"tool_result"`) != 2 ||
		strings.Count(string(anth), `"tool_use_id":"call_`) != 2 {
		t.Errorf("anthropic request must carry both tool_use and both tool_result blocks:\n%s", anth)
	}
	resp, err := openAIResponsesRequest{}.FromIR(ir, "gpt", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(resp), `"type":"function_call"`) != 2 || strings.Count(string(resp), `"type":"function_call_output"`) != 2 {
		t.Errorf("responses request must carry both function_call and both function_call_output items:\n%s", resp)
	}
}
