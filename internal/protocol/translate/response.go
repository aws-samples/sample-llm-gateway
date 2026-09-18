package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
)

// ======================================================================================
// Anthropic Messages — non-streaming response
// ======================================================================================

type anthropicResponse struct{}

type anthropicResp struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"` // "message"
	Role         string           `json:"role"` // "assistant"
	Model        string           `json:"model"`
	Content      []anthropicBlock `json:"content"`
	StopReason   *string          `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anthropicUsage   `json:"usage"`
}

// anthropicUsage is what we emit. Upstream extras (cache_creation breakdown, service_tier,
// thinking_tokens) are not reproduced — the IR only carries the normalized counters.
type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (anthropicResponse) ToIR(body []byte) (*Response, error) {
	var w anthropicResp
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("anthropic response: %w", err)
	}
	if w.Type != "" && w.Type != "message" {
		return nil, fmt.Errorf("anthropic response: unexpected type %q", w.Type)
	}
	raw, _ := json.Marshal(w.Content)
	content, err := anthropicContentToIR(raw)
	if err != nil {
		return nil, fmt.Errorf("anthropic response: %w", err)
	}
	r := &Response{
		ID:      w.ID,
		Model:   w.Model,
		Content: content,
		Usage:   protocol.ParseUsage(protocol.Anthropic, body),
	}
	r.StopReason = anthropicStopToIR(w.StopReason)
	return r, nil
}

func anthropicStopToIR(s *string) StopReason {
	if s == nil {
		return StopOther
	}
	switch *s {
	case "end_turn":
		return StopEndTurn
	case "max_tokens":
		return StopMaxTokens
	case "tool_use":
		return StopToolUse
	case "stop_sequence":
		return StopStopSequence
	case "refusal":
		return StopContentFilter
	case "pause_turn":
		return StopEndTurn
	}
	return StopOther
}

func (anthropicResponse) FromIR(r *Response) ([]byte, error) {
	blocks, err := irContentToAnthropic(r.Content, false)
	if err != nil {
		return nil, fmt.Errorf("anthropic response: %w", err)
	}
	if blocks == nil {
		blocks = []anthropicBlock{}
	}
	id := r.ID
	if id == "" {
		id = "msg_" + randomID()
	}
	stop := anthropicStopFromIR(r.StopReason)
	w := anthropicResp{
		ID: id, Type: "message", Role: "assistant", Model: r.Model, Content: blocks,
		StopReason: &stop, StopSequence: nil,
		Usage: anthropicUsage{
			InputTokens: r.Usage.Input, OutputTokens: r.Usage.Output,
			CacheReadInputTokens: r.Usage.CacheRead, CacheCreationInputTokens: r.Usage.CacheWrite,
		},
	}
	return json.Marshal(w)
}

func anthropicStopFromIR(s StopReason) string {
	switch s {
	case StopEndTurn:
		return "end_turn"
	case StopMaxTokens:
		return "max_tokens"
	case StopToolUse:
		return "tool_use"
	case StopStopSequence:
		return "stop_sequence"
	case StopContentFilter:
		return "refusal"
	}
	return "end_turn"
}

// ======================================================================================
// OpenAI Chat Completions — non-streaming response
// ======================================================================================

type openAIChatResponse struct{}

type chatResp struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"` // chat.completion
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int              `json:"index"`
	Message      chatRespMsg      `json:"message"`
	FinishReason *string          `json:"finish_reason"`
	Logprobs     *json.RawMessage `json:"logprobs,omitempty"`
}

type chatRespMsg struct {
	Role             string          `json:"role"`
	Content          *string         `json:"content"`
	ToolCalls        []chatToolCall  `json:"tool_calls,omitempty"`
	Refusal          *string         `json:"refusal,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"` // DeepSeek/vLLM-style
	Reasoning        json.RawMessage `json:"reasoning,omitempty"`         // some gateways
}

type chatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

func (openAIChatResponse) ToIR(body []byte) (*Response, error) {
	var w chatResp
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("openai chat response: %w", err)
	}
	if len(w.Choices) == 0 {
		return nil, errors.New("openai chat response: no choices")
	}
	ch := w.Choices[0]
	r := &Response{ID: w.ID, Model: w.Model, Usage: protocol.ParseUsage(protocol.OpenAIChat, body)}
	// Reasoning text (non-standard but common) → opaque thinking without a signature. Anthropic
	// FromIR drops unsigned thinking, so it does not break replay; other targets may show it.
	if ch.Message.ReasoningContent != nil && *ch.Message.ReasoningContent != "" {
		r.Content = append(r.Content, Content{Kind: KindThinking, Text: *ch.Message.ReasoningContent})
	}
	if ch.Message.Content != nil && *ch.Message.Content != "" {
		r.Content = append(r.Content, Content{Kind: KindText, Text: *ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 || !json.Valid(args) {
			// Models occasionally emit truncated/invalid JSON; keep it as a string so nothing is lost.
			args, _ = json.Marshal(map[string]string{"_raw": tc.Function.Arguments})
		}
		r.Content = append(r.Content, Content{Kind: KindToolUse, ToolCallID: tc.ID, ToolName: tc.Function.Name, ToolInput: args})
	}
	r.StopReason = chatFinishToIR(ch.FinishReason, len(ch.Message.ToolCalls) > 0)
	return r, nil
}

func chatFinishToIR(s *string, hasToolCalls bool) StopReason {
	if s == nil {
		if hasToolCalls {
			return StopToolUse
		}
		return StopOther
	}
	switch *s {
	case "stop":
		if hasToolCalls {
			return StopToolUse
		}
		return StopEndTurn
	case "length":
		return StopMaxTokens
	case "tool_calls", "function_call":
		return StopToolUse
	case "content_filter":
		return StopContentFilter
	}
	return StopOther
}

func (openAIChatResponse) FromIR(r *Response) ([]byte, error) {
	msg := chatRespMsg{Role: "assistant"}
	var text string
	hasText := false
	for _, c := range r.Content {
		switch c.Kind {
		case KindText:
			if hasText {
				text += "\n"
			}
			text += c.Text
			hasText = true
		case KindToolUse:
			args := string(c.ToolInput)
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, chatToolCall{ID: c.ToolCallID, Type: "function", Function: chatFunction{Name: c.ToolName, Arguments: args}})
		case KindThinking:
			// Chat Completions has no standard slot; expose visible reasoning text via the
			// widely-used reasoning_content extension when present. Opaque payloads are dropped.
			if c.Text != "" {
				t := c.Text
				msg.ReasoningContent = &t
			}
		case KindImage:
			// no assistant-side image concept in Chat Completions
		}
	}
	if hasText {
		msg.Content = &text
	} else if len(msg.ToolCalls) == 0 {
		empty := ""
		msg.Content = &empty
	}
	finish := chatFinishFromIR(r.StopReason, len(msg.ToolCalls) > 0)
	id := r.ID
	if id == "" {
		id = "chatcmpl-" + randomID()
	}
	w := chatResp{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: r.Model,
		Choices: []chatChoice{{Index: 0, Message: msg, FinishReason: &finish}},
		Usage:   chatUsageFromIR(r.Usage),
	}
	return json.Marshal(w)
}

func chatFinishFromIR(s StopReason, hasToolCalls bool) string {
	switch s {
	case StopToolUse:
		return "tool_calls"
	case StopMaxTokens:
		return "length"
	case StopContentFilter:
		return "content_filter"
	case StopEndTurn, StopStopSequence:
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}

// chatUsageFromIR renders IR usage in OpenAI shape. IR.Input is the UNCACHED prompt portion
// (protocol.Usage semantics), so prompt_tokens = input + cache_read + cache_write.
func chatUsageFromIR(u protocol.Usage) *chatUsage {
	if !u.Found {
		return nil
	}
	cu := &chatUsage{
		PromptTokens:     u.Input + u.CacheRead + u.CacheWrite,
		CompletionTokens: u.Output,
	}
	cu.TotalTokens = cu.PromptTokens + cu.CompletionTokens
	if u.CacheRead > 0 {
		cu.PromptTokensDetails = &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{CachedTokens: u.CacheRead}
	}
	if u.Reasoning > 0 {
		cu.CompletionTokensDetails = &struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		}{ReasoningTokens: u.Reasoning}
	}
	return cu
}
