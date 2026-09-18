package translate

import (
	"encoding/json"
	"fmt"

	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
)

// ======================================================================================
// Anthropic Messages SSE  →  IR events
// ======================================================================================
//
// Real event order (recorded from Bedrock, see testdata/fixtures/claude-code/02-*.sse):
//
//	message_start{message{id,model,usage{input,cache_*,output:init}}}
//	content_block_start{index,content_block{type:thinking|text|tool_use{id,name,input:{}}}}
//	content_block_delta{index,delta{type:thinking_delta|text_delta|input_json_delta|signature_delta}}
//	content_block_stop{index}
//	message_delta{delta{stop_reason},usage{...final counters...}}
//	message_stop
type anthropicStreamIn struct {
	blocks   map[int]*anthBlockState
	stop     StopReason
	usage    protocol.Usage
	usageSet bool
}

type anthBlockState struct {
	kind      ContentKind
	thinking  string
	signature string
	toolID    string
	toolName  string
}

func newAnthropicStreamIn() *anthropicStreamIn {
	return &anthropicStreamIn{blocks: map[int]*anthBlockState{}}
}

type anthEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      *anthropicResp  `json:"message,omitempty"`
	ContentBlock *anthropicBlock `json:"content_block,omitempty"`
	Delta        *anthDelta      `json:"delta,omitempty"`
	Usage        *anthropicUsage `json:"usage,omitempty"`
	Error        *anthError      `json:"error,omitempty"`
}

type anthDelta struct {
	Type        string  `json:"type"`
	Text        string  `json:"text,omitempty"`
	Thinking    string  `json:"thinking,omitempty"`
	PartialJSON string  `json:"partial_json,omitempty"`
	Signature   string  `json:"signature,omitempty"`
	StopReason  *string `json:"stop_reason,omitempty"`
}

type anthError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (s *anthropicStreamIn) ToIR(_ string, data []byte) ([]StreamEvent, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var ev anthEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("anthropic stream: %w", err)
	}
	switch ev.Type {
	case "message_start":
		out := StreamEvent{Kind: EventMessageStart}
		if ev.Message != nil {
			out.ID, out.Model = ev.Message.ID, ev.Message.Model
			// Initial usage: input side is final here; output is a placeholder.
			raw, _ := json.Marshal(map[string]any{"usage": ev.Message.Usage})
			s.usage = protocol.ParseUsage(protocol.Anthropic, raw)
			out.Usage = s.usage
		}
		return []StreamEvent{out}, nil

	case "content_block_start":
		if ev.ContentBlock == nil {
			return nil, nil
		}
		st := &anthBlockState{}
		s.blocks[ev.Index] = st
		switch ev.ContentBlock.Type {
		case "text":
			st.kind = KindText
			if ev.ContentBlock.Text != "" {
				return []StreamEvent{{Kind: EventTextDelta, Index: ev.Index, Text: ev.ContentBlock.Text}}, nil
			}
		case "thinking":
			st.kind = KindThinking
			st.thinking, st.signature = ev.ContentBlock.Thinking, ev.ContentBlock.Signature
		case "redacted_thinking":
			st.kind = KindThinking
			raw, _ := json.Marshal(map[string]string{"type": "redacted_thinking", "data": ev.ContentBlock.Data})
			return []StreamEvent{{Kind: EventThinkingDone, Index: ev.Index, ThinkingRaw: raw}}, nil
		case "tool_use":
			st.kind, st.toolID, st.toolName = KindToolUse, ev.ContentBlock.ID, ev.ContentBlock.Name
			return []StreamEvent{{Kind: EventToolUseStart, Index: ev.Index, ToolCallID: st.toolID, ToolName: st.toolName}}, nil
		default:
			return nil, fmt.Errorf("anthropic stream: unsupported content_block type %q", ev.ContentBlock.Type)
		}
		return nil, nil

	case "content_block_delta":
		if ev.Delta == nil {
			return nil, nil
		}
		st := s.blocks[ev.Index]
		switch ev.Delta.Type {
		case "text_delta":
			return []StreamEvent{{Kind: EventTextDelta, Index: ev.Index, Text: ev.Delta.Text}}, nil
		case "thinking_delta":
			if st != nil {
				st.thinking += ev.Delta.Thinking
			}
			return []StreamEvent{{Kind: EventThinkingDelta, Index: ev.Index, Text: ev.Delta.Thinking}}, nil
		case "signature_delta":
			if st != nil {
				st.signature += ev.Delta.Signature
			}
			return nil, nil
		case "input_json_delta":
			// The first fragment is often "" — forward as-is; consumers concatenate.
			out := StreamEvent{Kind: EventToolInputDelta, Index: ev.Index, ToolInputDelta: ev.Delta.PartialJSON}
			if st != nil {
				out.ToolCallID, out.ToolName = st.toolID, st.toolName
			}
			return []StreamEvent{out}, nil
		}
		return nil, nil

	case "content_block_stop":
		st := s.blocks[ev.Index]
		delete(s.blocks, ev.Index)
		if st == nil {
			return nil, nil
		}
		switch st.kind {
		case KindThinking:
			return []StreamEvent{{Kind: EventThinkingDone, Index: ev.Index, Text: st.thinking, ThinkingSignature: st.signature}}, nil
		case KindToolUse:
			return []StreamEvent{{Kind: EventToolUseStop, Index: ev.Index, ToolCallID: st.toolID, ToolName: st.toolName}}, nil
		}
		return nil, nil

	case "message_delta":
		if ev.Delta != nil {
			s.stop = anthropicStopToIR(ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			// message_delta carries the final counters; keep input-side values from message_start
			// if the delta omits them (older servers only send output_tokens here).
			raw, _ := json.Marshal(map[string]any{"usage": ev.Usage})
			u := protocol.ParseUsage(protocol.Anthropic, raw)
			if ev.Usage.InputTokens == 0 && ev.Usage.CacheReadInputTokens == 0 && ev.Usage.CacheCreationInputTokens == 0 {
				u.Input, u.CacheRead, u.CacheWrite = s.usage.Input, s.usage.CacheRead, s.usage.CacheWrite
			}
			s.usage = u
			s.usageSet = true
		}
		return nil, nil

	case "message_stop":
		stop := s.stop
		if stop == "" {
			stop = StopEndTurn
		}
		return []StreamEvent{{Kind: EventMessageStop, StopReason: stop, Usage: s.usage}}, nil

	case "ping":
		return nil, nil

	case "error":
		msg := "upstream error"
		if ev.Error != nil {
			msg = ev.Error.Type + ": " + ev.Error.Message
		}
		return []StreamEvent{{Kind: EventError, Err: msg}}, nil
	}
	return nil, nil
}

// ======================================================================================
// IR events  →  Anthropic Messages SSE
// ======================================================================================
//
// Synthesizes the block structure Anthropic clients expect: every text/thinking/tool_use run is
// wrapped in content_block_start/stop with a monotonically increasing index; message_delta carries
// stop_reason + final usage; message_stop closes.
type anthropicStreamOut struct {
	started   bool
	nextIndex int
	// open block, if any
	openIdx  int
	openKind ContentKind
	openTool string // tool call id → maps source Index to our block index
	srcToOut map[int]int
	model    string
	id       string
	usage0   protocol.Usage
}

func newAnthropicStreamOut() *anthropicStreamOut {
	return &anthropicStreamOut{openIdx: -1, srcToOut: map[int]int{}}
}

func (s *anthropicStreamOut) FromIR(ev StreamEvent) ([]byte, error) {
	var out []byte
	switch ev.Kind {
	case EventMessageStart:
		s.started = true
		s.id, s.model, s.usage0 = ev.ID, ev.Model, ev.Usage
		if s.id == "" {
			s.id = "msg_" + randomID()
		}
		msg := map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": anthropicUsage{InputTokens: ev.Usage.Input, OutputTokens: 0, CacheReadInputTokens: ev.Usage.CacheRead, CacheCreationInputTokens: ev.Usage.CacheWrite},
		}
		return sse("message_start", map[string]any{"type": "message_start", "message": msg}), nil

	case EventTextDelta:
		out = append(out, s.ensureOpen(KindText, "", "")...)
		return append(out, sse("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.openIdx,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})...), nil

	case EventThinkingDelta:
		out = append(out, s.ensureOpen(KindThinking, "", "")...)
		return append(out, sse("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.openIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
		})...), nil

	case EventThinkingDone:
		// Opaque payloads from other protocols cannot be rendered as Anthropic thinking; only a
		// real signature (or redacted_thinking) is emitted. Unsigned thinking is closed as-is —
		// Anthropic clients tolerate a thinking block without signature in a stream, but will
		// not be able to replay it (FromIR on the request side drops unsigned blocks).
		if len(ev.ThinkingRaw) > 0 {
			var probe struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}
			if json.Unmarshal(ev.ThinkingRaw, &probe) == nil && probe.Type == "redacted_thinking" {
				out = append(out, s.closeOpen()...)
				idx := s.nextIndex
				s.nextIndex++
				out = append(out, sse("content_block_start", map[string]any{"type": "content_block_start", "index": idx,
					"content_block": map[string]any{"type": "redacted_thinking", "data": probe.Data}})...)
				out = append(out, sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})...)
			}
			return out, nil
		}
		if s.openKind == KindThinking && s.openIdx >= 0 {
			if ev.ThinkingSignature != "" {
				out = append(out, sse("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": s.openIdx,
					"delta": map[string]any{"type": "signature_delta", "signature": ev.ThinkingSignature},
				})...)
			}
			out = append(out, s.closeOpen()...)
		}
		return out, nil

	case EventToolUseStart:
		out = append(out, s.closeOpen()...)
		idx := s.nextIndex
		s.nextIndex++
		s.openIdx, s.openKind, s.openTool = idx, KindToolUse, ev.ToolCallID
		s.srcToOut[ev.Index] = idx
		id := ev.ToolCallID
		if id == "" {
			id = "toolu_" + randomID()
		}
		return append(out, sse("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": ev.ToolName, "input": map[string]any{}},
		})...), nil

	case EventToolInputDelta:
		idx, ok := s.srcToOut[ev.Index]
		if !ok {
			// Source emitted a delta without a start (shouldn't happen); open one.
			b, _ := s.FromIR(StreamEvent{Kind: EventToolUseStart, Index: ev.Index, ToolCallID: ev.ToolCallID, ToolName: ev.ToolName})
			out = append(out, b...)
			idx = s.srcToOut[ev.Index]
		}
		return append(out, sse("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ToolInputDelta},
		})...), nil

	case EventToolUseStop:
		if idx, ok := s.srcToOut[ev.Index]; ok {
			delete(s.srcToOut, ev.Index)
			if s.openIdx == idx {
				s.openIdx, s.openKind = -1, ""
			}
			return sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx}), nil
		}
		return nil, nil

	case EventMessageStop:
		out = append(out, s.closeOpen()...)
		u := ev.Usage
		if !u.Found {
			u = s.usage0
		}
		out = append(out, sse("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": anthropicStopFromIR(ev.StopReason), "stop_sequence": nil},
			"usage": anthropicUsage{InputTokens: u.Input, OutputTokens: u.Output, CacheReadInputTokens: u.CacheRead, CacheCreationInputTokens: u.CacheWrite},
		})...)
		return append(out, sse("message_stop", map[string]any{"type": "message_stop"})...), nil

	case EventError:
		out = append(out, s.closeOpen()...)
		return append(out, sse("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": ev.Err}})...), nil
	}
	return nil, nil
}

// ensureOpen opens a block of the given kind if none of that kind is currently open.
func (s *anthropicStreamOut) ensureOpen(kind ContentKind, _ string, _ string) []byte {
	if s.openIdx >= 0 && s.openKind == kind {
		return nil
	}
	out := s.closeOpen()
	idx := s.nextIndex
	s.nextIndex++
	s.openIdx, s.openKind = idx, kind
	var block map[string]any
	switch kind {
	case KindText:
		block = map[string]any{"type": "text", "text": ""}
	case KindThinking:
		block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	}
	return append(out, sse("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": block})...)
}

func (s *anthropicStreamOut) closeOpen() []byte {
	if s.openIdx < 0 {
		return nil
	}
	idx := s.openIdx
	s.openIdx, s.openKind = -1, ""
	return sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

// sse frames one named event.
func sse(event string, v any) []byte {
	b, _ := json.Marshal(v)
	return []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
}
