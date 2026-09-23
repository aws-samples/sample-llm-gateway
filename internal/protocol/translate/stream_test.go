package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// sseFrame is one parsed SSE block.
type sseFrame struct {
	Event string
	Data  string
}

// parseSSE splits an SSE byte stream into frames, joining multi-line data: fields.
func parseSSE(t *testing.T, raw []byte) []sseFrame {
	t.Helper()
	var frames []sseFrame
	var cur sseFrame
	var data []string
	flush := func() {
		if len(data) > 0 || cur.Event != "" {
			cur.Data = strings.Join(data, "\n")
			frames = append(frames, cur)
		}
		cur, data = sseFrame{}, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			cur.Event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
	flush()
	return frames
}

// driveIn feeds frames through a StreamDecoder and collects the IR events.
func driveIn(t *testing.T, codec StreamDecoder, frames []sseFrame) []StreamEvent {
	t.Helper()
	var out []StreamEvent
	for _, f := range frames {
		evs, err := codec.ToIR(f.Event, []byte(f.Data))
		if err != nil {
			t.Fatalf("ToIR(%s): %v\n%s", f.Event, err, f.Data)
		}
		out = append(out, evs...)
	}
	return out
}

// driveOut renders IR events through a StreamEncoder and concatenates the bytes.
func driveOut(t *testing.T, codec StreamEncoder, evs []StreamEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, ev := range evs {
		b, err := codec.FromIR(ev)
		if err != nil {
			t.Fatalf("FromIR(%s): %v", ev.Kind, err)
		}
		buf.Write(b)
	}
	return buf.Bytes()
}

func kindSeq(evs []StreamEvent) []StreamEventKind {
	out := make([]StreamEventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func joinToolInput(evs []StreamEvent, idx int) string {
	var sb strings.Builder
	for _, e := range evs {
		if e.Kind == EventToolInputDelta && e.Index == idx {
			sb.WriteString(e.ToolInputDelta)
		}
	}
	return sb.String()
}

// ---- Anthropic SSE → IR (real Claude Code / Bedrock stream) ----------------------------------

func TestAnthropicStreamToIR_RealToolUse(t *testing.T) {
	frames := parseSSE(t, fixture(t, "claude-code/02-tool-use-call.response.sse"))
	evs := driveIn(t, newAnthropicStreamIn(), frames)

	ks := kindSeq(evs)
	// message_start, thinking_delta (1, empty), thinking_done(sig), tool_use_start, 10× tool_input_delta, tool_use_stop, message_stop
	if ks[0] != EventMessageStart || ks[len(ks)-1] != EventMessageStop {
		t.Fatalf("must start with message_start and end with message_stop: %v", ks)
	}
	var tuStart, tuStop, thDone int
	for _, e := range evs {
		switch e.Kind {
		case EventToolUseStart:
			tuStart++
			if e.ToolCallID != "toolu_bdrk_01DvBxF543NYes3fUgqodtEL" || e.ToolName != "Read" || e.Index != 1 {
				t.Errorf("tool_use_start: %+v", e)
			}
		case EventToolUseStop:
			tuStop++
		case EventThinkingDone:
			thDone++
			if e.ThinkingSignature == "" || e.Index != 0 {
				t.Errorf("thinking_done must carry the signature: sig=%d idx=%d", len(e.ThinkingSignature), e.Index)
			}
		}
	}
	if tuStart != 1 || tuStop != 1 || thDone != 1 {
		t.Errorf("counts start=%d stop=%d thinking_done=%d", tuStart, tuStop, thDone)
	}
	// Fragments concatenate to the exact arguments JSON.
	args := joinToolInput(evs, 1)
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || m["file_path"] != "/private/tmp/llmgw/cc-work/sample.txt" {
		t.Errorf("reassembled tool input: %q", args)
	}
	last := evs[len(evs)-1]
	if last.StopReason != StopToolUse {
		t.Errorf("stop_reason: %q", last.StopReason)
	}
	// Final usage comes from message_delta: input=2, cache_write=38813, output=88.
	if !last.Usage.Found || last.Usage.Input != 2 || last.Usage.CacheWrite != 38813 || last.Usage.Output != 88 {
		t.Errorf("final usage: %+v", last.Usage)
	}
}

func TestAnthropicStreamToIR_RealTextOnly(t *testing.T) {
	frames := parseSSE(t, fixture(t, "claude-code/01-text-only.response.sse"))
	evs := driveIn(t, newAnthropicStreamIn(), frames)
	var text strings.Builder
	for _, e := range evs {
		if e.Kind == EventTextDelta {
			text.WriteString(e.Text)
		}
	}
	if text.Len() == 0 {
		t.Fatal("no text deltas")
	}
	last := evs[len(evs)-1]
	if last.Kind != EventMessageStop || last.StopReason != StopEndTurn || last.Usage.Output != 16 || last.Usage.Input != 1013 {
		t.Errorf("stop/usage: %+v", last)
	}
}

// ---- OpenAI Chat SSE → IR (real Bedrock GPT stream with a tool call) ---------------------------

func TestChatStreamToIR_RealToolCall(t *testing.T) {
	frames := parseSSE(t, fixture(t, "bedrock-gpt-chat-tool-call.response.sse"))
	evs := driveIn(t, newChatStreamIn(), frames)
	ks := kindSeq(evs)
	if ks[0] != EventMessageStart || ks[len(ks)-1] != EventMessageStop {
		t.Fatalf("shape: %v", ks)
	}
	var starts []StreamEvent
	for _, e := range evs {
		if e.Kind == EventToolUseStart {
			starts = append(starts, e)
		}
	}
	if len(starts) != 1 || starts[0].ToolCallID != "call_0" || starts[0].ToolName != "read_file" {
		t.Fatalf("tool_use_start: %+v", starts)
	}
	// Fragments carried id/name only in the first chunk; the codec must still attribute them.
	args := joinToolInput(evs, starts[0].Index)
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || m["path"] != "/tmp/x.txt" {
		t.Errorf("reassembled arguments: %q", args)
	}
	for _, e := range evs {
		if e.Kind == EventToolInputDelta && (e.ToolCallID != "call_0" || e.ToolName != "read_file") {
			t.Errorf("fragment lost tool identity: %+v", e)
		}
	}
	last := evs[len(evs)-1]
	if last.StopReason != StopToolUse {
		t.Errorf("finish_reason tool_calls → StopToolUse, got %q", last.StopReason)
	}
	// finish_reason arrived BEFORE the usage chunk; MessageStop must still carry usage.
	if !last.Usage.Found || last.Usage.Input != 51 || last.Usage.Output != 21 {
		t.Errorf("usage must be attached to message_stop even though it arrives after finish_reason: %+v", last.Usage)
	}
	// Exactly one MessageStop even though [DONE] follows.
	stops := 0
	for _, k := range ks {
		if k == EventMessageStop {
			stops++
		}
	}
	if stops != 1 {
		t.Errorf("want exactly 1 message_stop, got %d", stops)
	}
}

// ---- Scenario 1 return path: real GPT chat stream → IR → Anthropic SSE for Claude Code -----------

func TestChatStreamToAnthropicSSE(t *testing.T) {
	frames := parseSSE(t, fixture(t, "bedrock-gpt-chat-tool-call.response.sse"))
	evs := driveIn(t, newChatStreamIn(), frames)
	out := driveOut(t, newAnthropicStreamOut(), evs)

	got := parseSSE(t, out)
	names := make([]string, len(got))
	for i, f := range got {
		names[i] = f.Event
	}
	// Required Anthropic skeleton for a single tool_use response.
	want := []string{"message_start", "content_block_start"}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("frame %d = %s want %s (all: %v)", i, names[i], w, names)
		}
	}
	tail := names[len(names)-3:]
	if tail[0] != "content_block_stop" || tail[1] != "message_delta" || tail[2] != "message_stop" {
		t.Fatalf("tail must be content_block_stop, message_delta, message_stop: %v", names)
	}
	// Every frame's event name must match its data.type (Anthropic SDKs dispatch on both).
	for _, f := range got {
		var d struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(f.Data), &d) != nil || d.Type != f.Event {
			t.Errorf("event %q vs data.type %q", f.Event, d.Type)
		}
	}
	// content_block_start must be a tool_use with id/name and empty input object.
	var cbs struct {
		Index int `json:"index"`
		Block struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content_block"`
	}
	_ = json.Unmarshal([]byte(got[1].Data), &cbs)
	if cbs.Block.Type != "tool_use" || cbs.Block.ID != "call_0" || cbs.Block.Name != "read_file" || cbs.Block.Input == nil || cbs.Index != 0 {
		t.Errorf("content_block_start: %s", got[1].Data)
	}
	// input_json_delta fragments concatenate to the original arguments.
	var sb strings.Builder
	for _, f := range got {
		if f.Event != "content_block_delta" {
			continue
		}
		var d struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(f.Data), &d)
		if d.Delta.Type != "input_json_delta" || d.Index != 0 {
			t.Errorf("delta: %s", f.Data)
		}
		sb.WriteString(d.Delta.PartialJSON)
	}
	if sb.String() != `{"path":"/tmp/x.txt"}` {
		t.Errorf("reassembled partial_json: %q", sb.String())
	}
	// message_delta carries stop_reason tool_use and OpenAI usage re-expressed in Anthropic terms.
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage anthropicUsage `json:"usage"`
	}
	_ = json.Unmarshal([]byte(got[len(got)-2].Data), &md)
	if md.Delta.StopReason != "tool_use" || md.Usage.InputTokens != 51 || md.Usage.OutputTokens != 21 {
		t.Errorf("message_delta: %s", got[len(got)-2].Data)
	}
	if bytes.Contains(out, []byte("[DONE]")) || bytes.Contains(out, []byte("chat.completion")) {
		t.Error("OpenAI framing leaked into Anthropic stream")
	}
}

// ---- Reverse: real Anthropic stream → IR → OpenAI Chat SSE -------------------------------------

func TestAnthropicStreamToChatSSE(t *testing.T) {
	frames := parseSSE(t, fixture(t, "claude-code/02-tool-use-call.response.sse"))
	evs := driveIn(t, newAnthropicStreamIn(), frames)
	out := driveOut(t, newChatStreamOut(), evs)

	if !bytes.HasSuffix(bytes.TrimSpace(out), []byte("data: [DONE]")) {
		t.Fatalf("chat stream must end with [DONE]:\n%s", out[max(0, len(out)-200):])
	}
	got := parseSSE(t, out)
	var (
		roleSeen  bool
		toolFirst map[string]any
		args      strings.Builder
		finish    string
		usage     *chatUsage
	)
	for _, f := range got {
		if f.Data == "[DONE]" {
			continue
		}
		var c struct {
			Object  string `json:"object"`
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
				Delta        struct {
					Role      string           `json:"role"`
					Content   *string          `json:"content"`
					Reasoning *string          `json:"reasoning_content"`
					ToolCalls []map[string]any `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *chatUsage `json:"usage"`
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
				roleSeen = true
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				fn, _ := tc["function"].(map[string]any)
				if id, ok := tc["id"]; ok && id != "" {
					toolFirst = tc
				}
				if fn != nil {
					if a, _ := fn["arguments"].(string); a != "" {
						args.WriteString(a)
					}
				}
			}
		}
	}
	if !roleSeen {
		t.Error("first chunk must carry role=assistant")
	}
	if toolFirst == nil || toolFirst["id"] != "toolu_bdrk_01DvBxF543NYes3fUgqodtEL" || toolFirst["type"] != "function" || toolFirst["index"] != float64(0) {
		t.Errorf("first tool_calls fragment must carry id/type/index: %v", toolFirst)
	}
	var m map[string]any
	if json.Unmarshal([]byte(args.String()), &m) != nil || m["file_path"] == nil {
		t.Errorf("arguments reassembled from fragments: %q", args.String())
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason: %q", finish)
	}
	// Anthropic input=2 + cache_write=38813 → OpenAI prompt_tokens=38815.
	if usage == nil || usage.PromptTokens != 38815 || usage.CompletionTokens != 88 {
		t.Errorf("usage chunk: %+v", usage)
	}
	// Anthropic signature must never reach an OpenAI client.
	if bytes.Contains(out, []byte("EqUCCpEBCBEQ")) || bytes.Contains(out, []byte("signature")) {
		t.Error("thinking signature leaked into chat stream")
	}
}

// ---- Round trip: Anthropic → IR → Anthropic must reproduce the same event skeleton ---------------

func TestAnthropicStreamRoundTrip(t *testing.T) {
	src := parseSSE(t, fixture(t, "claude-code/02-tool-use-call.response.sse"))
	evs := driveIn(t, newAnthropicStreamIn(), src)
	out := driveOut(t, newAnthropicStreamOut(), evs)
	dst := parseSSE(t, out)

	seq := func(fs []sseFrame) []string {
		var s []string
		for _, f := range fs {
			if f.Event == "ping" {
				continue
			}
			s = append(s, f.Event)
		}
		return s
	}
	a, b := seq(src), seq(dst)
	// Source has one thinking_delta and one signature_delta; ours re-emits the same count of
	// deltas (thinking text delta + signature delta), so the skeleton must match exactly.
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("event skeleton differs\nsrc: %v\ndst: %v", a, b)
	}
	// Re-parse our output and confirm the IR is identical to the first pass.
	evs2 := driveIn(t, newAnthropicStreamIn(), dst)
	if strings.Join(toStrings(kindSeq(evs)), ",") != strings.Join(toStrings(kindSeq(evs2)), ",") {
		t.Errorf("IR kinds differ after round trip\n1: %v\n2: %v", kindSeq(evs), kindSeq(evs2))
	}
	if joinToolInput(evs, 1) != joinToolInput(evs2, 1) {
		t.Error("tool input differs after round trip")
	}
	sig1, sig2 := "", ""
	for _, e := range evs {
		if e.Kind == EventThinkingDone {
			sig1 = e.ThinkingSignature
		}
	}
	for _, e := range evs2 {
		if e.Kind == EventThinkingDone {
			sig2 = e.ThinkingSignature
		}
	}
	if sig1 == "" || sig1 != sig2 {
		t.Errorf("thinking signature must survive round trip: %d vs %d chars", len(sig1), len(sig2))
	}
}

func toStrings[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}
