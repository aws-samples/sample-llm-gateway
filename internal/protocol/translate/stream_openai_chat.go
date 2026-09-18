package translate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
)

// ======================================================================================
// OpenAI Chat Completions SSE  →  IR events
// ======================================================================================
//
// Real chunk order (recorded from Bedrock, see testdata/fixtures/bedrock-gpt-chat-tool-call.response.sse):
//
//	delta{role:"assistant",content:""}
//	delta{tool_calls:[{index:0,id,type,function{name,arguments:""}}]}     ← id/name only here
//	delta{tool_calls:[{index:0,function{arguments:"frag"}}]} ...           ← fragments by index
//	{finish_reason:"tool_calls"}
//	{choices:[],usage{...}}                                                ← separate chunk (include_usage)
//	[DONE]
//
// finish_reason arrives BEFORE usage, so MessageStop is deferred until usage (or [DONE]).
type chatStreamIn struct {
	started    bool
	id, model  string
	finish     *string
	hasTools   bool
	textOpen   bool
	toolsOpen  map[int]bool // tool_calls index → we have emitted ToolUseStart
	toolIDs    map[int]string
	toolNames  map[int]string
	usage      protocol.Usage
	stopSent   bool
	reasonOpen bool
}

func newChatStreamIn() *chatStreamIn {
	return &chatStreamIn{toolsOpen: map[int]bool{}, toolIDs: map[int]string{}, toolNames: map[int]string{}}
}

type chatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int     `json:"index"`
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Role             string  `json:"role,omitempty"`
			Content          *string `json:"content"`
			ReasoningContent *string `json:"reasoning_content,omitempty"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
	} `json:"choices"`
	// Usage stays raw and is handed to protocol.ParseUsage untouched, so the counters the client
	// sees are exactly the ones metering bills. A typed struct here silently dropped the
	// provider-specific cache-write fields (Bedrock's cache_creation_input_tokens) and the two
	// disagreed on a real Claude Code run.
	Usage json.RawMessage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// hasUsage reports whether the chunk carries a usage object (include_usage final chunk).
func (c *chatChunk) hasUsage() bool { return len(c.Usage) > 0 && string(c.Usage) != "null" }

func (s *chatStreamIn) ToIR(_ string, data []byte) ([]StreamEvent, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if string(data) == "[DONE]" {
		return s.finishIfNeeded(), nil
	}
	var c chatChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("openai chat stream: %w", err)
	}
	if c.Error != nil {
		return []StreamEvent{{Kind: EventError, Err: c.Error.Type + ": " + c.Error.Message}}, nil
	}
	var out []StreamEvent
	if !s.started {
		s.started = true
		s.id, s.model = c.ID, c.Model
		out = append(out, StreamEvent{Kind: EventMessageStart, ID: c.ID, Model: c.Model})
	}
	if c.hasUsage() {
		s.usage = protocol.ParseUsage(protocol.OpenAIChat, append(append([]byte(`{"usage":`), c.Usage...), '}'))
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue // n>1 is not translatable; keep the first choice
		}
		d := ch.Delta
		if d.ReasoningContent != nil && *d.ReasoningContent != "" {
			s.reasonOpen = true
			out = append(out, StreamEvent{Kind: EventThinkingDelta, Index: -1, Text: *d.ReasoningContent})
		}
		if d.Content != nil && *d.Content != "" {
			if s.reasonOpen {
				s.reasonOpen = false
				out = append(out, StreamEvent{Kind: EventThinkingDone, Index: -1})
			}
			s.textOpen = true
			out = append(out, StreamEvent{Kind: EventTextDelta, Index: 0, Text: *d.Content})
		}
		for _, tc := range d.ToolCalls {
			s.hasTools = true
			// Tool indexes are offset by 1 so they never collide with the single text slot (0).
			idx := tc.Index + 1
			if !s.toolsOpen[tc.Index] {
				s.toolsOpen[tc.Index] = true
				s.toolIDs[tc.Index] = tc.ID
				s.toolNames[tc.Index] = tc.Function.Name
				if s.reasonOpen {
					s.reasonOpen = false
					out = append(out, StreamEvent{Kind: EventThinkingDone, Index: -1})
				}
				out = append(out, StreamEvent{Kind: EventToolUseStart, Index: idx, ToolCallID: tc.ID, ToolName: tc.Function.Name})
			} else {
				// Later fragments may repeat id/name; fill in if the first fragment lacked them.
				if s.toolIDs[tc.Index] == "" && tc.ID != "" {
					s.toolIDs[tc.Index] = tc.ID
				}
				if s.toolNames[tc.Index] == "" && tc.Function.Name != "" {
					s.toolNames[tc.Index] = tc.Function.Name
				}
			}
			if tc.Function.Arguments != "" {
				out = append(out, StreamEvent{Kind: EventToolInputDelta, Index: idx, ToolCallID: s.toolIDs[tc.Index], ToolName: s.toolNames[tc.Index], ToolInputDelta: tc.Function.Arguments})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			f := *ch.FinishReason
			s.finish = &f
			// Close all open tool calls now; the usage chunk follows separately.
			for i := range s.toolsOpen {
				if s.toolsOpen[i] {
					out = append(out, StreamEvent{Kind: EventToolUseStop, Index: i + 1, ToolCallID: s.toolIDs[i], ToolName: s.toolNames[i]})
					s.toolsOpen[i] = false
				}
			}
		}
	}
	// With include_usage the usage chunk comes after finish_reason; emit MessageStop once we
	// have both (or at [DONE] as a fallback).
	if s.finish != nil && c.hasUsage() {
		out = append(out, s.finishIfNeeded()...)
	}
	return out, nil
}

func (s *chatStreamIn) finishIfNeeded() []StreamEvent {
	if s.stopSent || !s.started {
		return nil
	}
	s.stopSent = true
	var out []StreamEvent
	for i := range s.toolsOpen { // safety: close anything still open
		if s.toolsOpen[i] {
			out = append(out, StreamEvent{Kind: EventToolUseStop, Index: i + 1, ToolCallID: s.toolIDs[i], ToolName: s.toolNames[i]})
			s.toolsOpen[i] = false
		}
	}
	if s.reasonOpen {
		s.reasonOpen = false
		out = append(out, StreamEvent{Kind: EventThinkingDone, Index: -1})
	}
	return append(out, StreamEvent{Kind: EventMessageStop, StopReason: chatFinishToIR(s.finish, s.hasTools), Usage: s.usage})
}

// ======================================================================================
// IR events  →  OpenAI Chat Completions SSE
// ======================================================================================
type chatStreamOut struct {
	id, model string
	created   int64
	started   bool
	toolIdx   map[int]int // source Index → OpenAI tool_calls index (0-based, dense)
	nextTool  int
	hasTools  bool
}

func newChatStreamOut() *chatStreamOut { return &chatStreamOut{toolIdx: map[int]int{}} }

func (s *chatStreamOut) chunk(delta map[string]any, finish *string, usage *chatUsage) []byte {
	ch := []map[string]any{}
	if delta != nil || finish != nil {
		c := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
		if delta == nil {
			c["delta"] = map[string]any{}
		}
		if finish != nil {
			c["finish_reason"] = *finish
		}
		ch = append(ch, c)
	}
	obj := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": ch,
	}
	if usage != nil {
		obj["usage"] = usage
	}
	b, _ := json.Marshal(obj)
	return []byte("data: " + string(b) + "\n\n")
}

func (s *chatStreamOut) FromIR(ev StreamEvent) ([]byte, error) {
	switch ev.Kind {
	case EventMessageStart:
		s.started = true
		s.id, s.model, s.created = ev.ID, ev.Model, time.Now().Unix()
		if s.id == "" {
			s.id = "chatcmpl-" + randomID()
		}
		return s.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil), nil
	case EventTextDelta:
		return s.chunk(map[string]any{"content": ev.Text}, nil, nil), nil
	case EventThinkingDelta:
		// Widely-used extension for visible reasoning; harmless to clients that ignore it.
		if ev.Text == "" {
			return nil, nil
		}
		return s.chunk(map[string]any{"reasoning_content": ev.Text}, nil, nil), nil
	case EventThinkingDone:
		return nil, nil
	case EventToolUseStart:
		s.hasTools = true
		idx := s.nextTool
		s.nextTool++
		s.toolIdx[ev.Index] = idx
		id := ev.ToolCallID
		if id == "" {
			id = "call_" + randomID()
		}
		return s.chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": idx, "id": id, "type": "function",
			"function": map[string]any{"name": ev.ToolName, "arguments": ""},
		}}}, nil, nil), nil
	case EventToolInputDelta:
		idx, ok := s.toolIdx[ev.Index]
		if !ok {
			b, _ := s.FromIR(StreamEvent{Kind: EventToolUseStart, Index: ev.Index, ToolCallID: ev.ToolCallID, ToolName: ev.ToolName})
			idx = s.toolIdx[ev.Index]
			return append(b, s.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx, "function": map[string]any{"arguments": ev.ToolInputDelta},
			}}}, nil, nil)...), nil
		}
		if ev.ToolInputDelta == "" {
			return nil, nil
		}
		return s.chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": idx, "function": map[string]any{"arguments": ev.ToolInputDelta},
		}}}, nil, nil), nil
	case EventToolUseStop:
		return nil, nil
	case EventMessageStop:
		finish := chatFinishFromIR(ev.StopReason, s.hasTools)
		out := s.chunk(map[string]any{}, &finish, nil)
		if u := chatUsageFromIR(ev.Usage); u != nil {
			out = append(out, s.chunk(nil, nil, u)...) // choices: [] + usage (include_usage shape)
		}
		return append(out, []byte("data: [DONE]\n\n")...), nil
	case EventError:
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": ev.Err, "type": "api_error"}})
		return []byte("data: " + string(b) + "\n\n"), nil
	}
	return nil, nil
}
