package translate

import (
	"encoding/json"
	"errors"
	"fmt"
)

// anthropicRequest is the RequestCodec for the Anthropic Messages API.
//
// Shapes handled are the ones real Claude Code traffic uses (see testdata/fixtures/claude-code):
//   - system: string or []text-block (with optional cache_control)
//   - messages[].role: user / assistant, and (mid-conversation-system beta) system
//   - messages[].content: string or []block where block.type ∈ text, thinking, redacted_thinking,
//     tool_use, tool_result, image
//   - tools[]: {name, description, input_schema}
//   - tool_choice: {type: auto|any|none|tool, name?}
//   - max_tokens (required by Anthropic), temperature, top_p, stop_sequences, stream
//
// Fields with no IR slot (thinking config, context_management, metadata, output_config,
// cache_control, ...) are dropped on ToIR; the design doc §7 policy covers what to do about
// them at the provider-compat layer.
type anthropicRequest struct{}

// ---- wire types -------------------------------------------------------------------------

type anthropicReq struct {
	Model         string           `json:"model"`
	System        json.RawMessage  `json:"system,omitempty"`
	Messages      []anthropicMsg   `json:"messages"`
	Tools         []anthropicTool  `json:"tools,omitempty"`
	ToolChoice    *anthropicChoice `json:"tool_choice,omitempty"`
	MaxTokens     *int64           `json:"max_tokens,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	Thinking      json.RawMessage  `json:"thinking,omitempty"`
	Metadata      json.RawMessage  `json:"metadata,omitempty"`
	Extra         map[string]any   `json:"-"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []anthropicBlock
}

type anthropicBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// thinking / redacted_thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"` // redacted_thinking
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []block
	IsError   bool            `json:"is_error,omitempty"`
	// image
	Source *anthropicImageSource `json:"source,omitempty"`
	// passthrough
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicChoice struct {
	Type string `json:"type"` // auto | any | none | tool
	Name string `json:"name,omitempty"`
}

// ---- ToIR -------------------------------------------------------------------------------

func (anthropicRequest) ToIR(body []byte) (*Request, error) {
	var w anthropicReq
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	if w.Model == "" {
		return nil, errors.New("anthropic request: model is required")
	}
	r := &Request{
		Model:       w.Model,
		MaxTokens:   w.MaxTokens,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		Stop:        w.StopSequences,
		Stream:      w.Stream,
	}
	// system: string | []block
	if len(w.System) > 0 && string(w.System) != "null" {
		blocks, err := anthropicContentToIR(w.System)
		if err != nil {
			return nil, fmt.Errorf("anthropic request: system: %w", err)
		}
		r.System = blocks
	}
	for i, m := range w.Messages {
		role, err := anthropicRoleToIR(m.Role)
		if err != nil {
			return nil, fmt.Errorf("anthropic request: messages[%d]: %w", i, err)
		}
		blocks, err := anthropicContentToIR(m.Content)
		if err != nil {
			return nil, fmt.Errorf("anthropic request: messages[%d]: %w", i, err)
		}
		r.Messages = append(r.Messages, Message{Role: role, Content: blocks})
	}
	for _, t := range w.Tools {
		r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	if w.ToolChoice != nil {
		switch w.ToolChoice.Type {
		case "", "auto":
			r.ToolChoice = ToolChoice{Mode: ToolChoiceAuto}
		case "any":
			r.ToolChoice = ToolChoice{Mode: ToolChoiceRequired}
		case "none":
			r.ToolChoice = ToolChoice{Mode: ToolChoiceNone}
		case "tool":
			r.ToolChoice = ToolChoice{Mode: ToolChoiceNamed, Name: w.ToolChoice.Name}
		default:
			return nil, fmt.Errorf("anthropic request: unknown tool_choice.type %q", w.ToolChoice.Type)
		}
	}
	return r, nil
}

func anthropicRoleToIR(role string) (Role, error) {
	switch role {
	case "user":
		return RoleUser, nil
	case "assistant":
		return RoleAssistant, nil
	case "system": // mid-conversation-system beta; Claude Code uses it
		return RoleSystem, nil
	}
	return "", fmt.Errorf("unknown role %q", role)
}

// anthropicContentToIR accepts a JSON string or an array of blocks.
func anthropicContentToIR(raw json.RawMessage) ([]Content, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []Content{{Kind: KindText, Text: s}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	out := make([]Content, 0, len(blocks))
	for i, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, Content{Kind: KindText, Text: b.Text})
		case "thinking":
			out = append(out, Content{Kind: KindThinking, Text: b.Thinking, ThinkingSignature: b.Signature})
		case "redacted_thinking":
			raw, _ := json.Marshal(map[string]string{"type": "redacted_thinking", "data": b.Data})
			out = append(out, Content{Kind: KindThinking, ThinkingRaw: raw})
		case "tool_use":
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage("{}")
			}
			out = append(out, Content{Kind: KindToolUse, ToolCallID: b.ID, ToolName: b.Name, ToolInput: in})
		case "tool_result":
			out = append(out, Content{Kind: KindToolResult, ToolResultID: b.ToolUseID, ToolResult: toolResultToIR(b.Content), ToolIsError: b.IsError})
		case "image":
			c := Content{Kind: KindImage}
			if b.Source != nil {
				c.ImageMediaType = b.Source.MediaType
				c.ImageDataB64 = b.Source.Data
				c.ImageURL = b.Source.URL
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("content[%d]: unsupported block type %q", i, b.Type)
		}
	}
	return out, nil
}

// toolResultToIR normalises Anthropic tool_result.content (string | []block) to a JSON string
// payload. Nested blocks are flattened to their text; anything else is kept as raw JSON.
func toolResultToIR(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`""`)
	}
	if raw[0] == '"' {
		return raw
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var s string
		allText := true
		for _, b := range blocks {
			if b.Type != "text" {
				allText = false
				break
			}
			s += b.Text
		}
		if allText {
			out, _ := json.Marshal(s)
			return out
		}
	}
	return raw
}

// ---- FromIR -----------------------------------------------------------------------------

func (anthropicRequest) FromIR(r *Request, providerModel string, opts Options) ([]byte, error) {
	w := anthropicReq{
		Model:         providerModel,
		Temperature:   r.Temperature,
		TopP:          r.TopP,
		StopSequences: r.Stop,
		Stream:        r.Stream,
	}
	// Anthropic requires max_tokens.
	switch {
	case r.MaxTokens != nil && *r.MaxTokens > 0:
		w.MaxTokens = r.MaxTokens
	case opts.DefaultMaxTokens > 0:
		v := opts.DefaultMaxTokens
		w.MaxTokens = &v
	default:
		return nil, errors.New("anthropic request: max_tokens is required and no default is configured")
	}
	if len(r.System) > 0 {
		blocks, err := irContentToAnthropic(r.System, true)
		if err != nil {
			return nil, err
		}
		w.System, _ = json.Marshal(blocks)
	}
	for i, m := range r.Messages {
		var role string
		switch m.Role {
		case RoleUser:
			role = "user"
		case RoleAssistant:
			role = "assistant"
		case RoleSystem:
			role = "system" // mid-conversation system (beta); kept as-is
		case RoleTool:
			// IR tool role = one or more tool results; Anthropic carries them as user blocks.
			role = "user"
		default:
			return nil, fmt.Errorf("anthropic request: messages[%d]: unsupported role %q", i, m.Role)
		}
		blocks, err := irContentToAnthropic(m.Content, false)
		if err != nil {
			return nil, fmt.Errorf("anthropic request: messages[%d]: %w", i, err)
		}
		raw, _ := json.Marshal(blocks)
		w.Messages = append(w.Messages, anthropicMsg{Role: role, Content: raw})
	}
	for _, t := range r.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		w.Tools = append(w.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	switch r.ToolChoice.Mode {
	case "", ToolChoiceAuto:
		// omit → Anthropic default auto
	case ToolChoiceRequired:
		w.ToolChoice = &anthropicChoice{Type: "any"}
	case ToolChoiceNone:
		w.ToolChoice = &anthropicChoice{Type: "none"}
	case ToolChoiceNamed:
		w.ToolChoice = &anthropicChoice{Type: "tool", Name: r.ToolChoice.Name}
	}
	// Merge adjacent same-role messages: Anthropic rejects consecutive messages with the same
	// role (common after converting OpenAI's separate role:tool messages into user blocks).
	w.Messages = mergeAdjacentAnthropic(w.Messages)
	return json.Marshal(w)
}

// irContentToAnthropic renders IR blocks as Anthropic content blocks. systemCtx=true means we
// are rendering the top-level `system`, where only text is legal.
func irContentToAnthropic(cs []Content, systemCtx bool) ([]anthropicBlock, error) {
	out := make([]anthropicBlock, 0, len(cs))
	for _, c := range cs {
		switch c.Kind {
		case KindText:
			out = append(out, anthropicBlock{Type: "text", Text: c.Text})
		case KindThinking:
			if systemCtx {
				continue
			}
			if len(c.ThinkingRaw) > 0 {
				// redacted_thinking or an opaque reasoning payload from another protocol. Only
				// re-emit what Anthropic can parse (redacted_thinking); other opaque payloads are
				// dropped — they belong to a different provider and cannot be validated here.
				var probe struct {
					Type string `json:"type"`
					Data string `json:"data"`
				}
				if json.Unmarshal(c.ThinkingRaw, &probe) == nil && probe.Type == "redacted_thinking" {
					out = append(out, anthropicBlock{Type: "redacted_thinking", Data: probe.Data})
				}
				continue
			}
			if c.ThinkingSignature == "" {
				// A thinking block without a signature is rejected by Anthropic on replay; drop it.
				continue
			}
			out = append(out, anthropicBlock{Type: "thinking", Thinking: c.Text, Signature: c.ThinkingSignature})
		case KindToolUse:
			if systemCtx {
				return nil, errors.New("tool_use not allowed in system")
			}
			in := c.ToolInput
			if len(in) == 0 {
				in = json.RawMessage("{}")
			}
			out = append(out, anthropicBlock{Type: "tool_use", ID: c.ToolCallID, Name: c.ToolName, Input: in})
		case KindToolResult:
			if systemCtx {
				return nil, errors.New("tool_result not allowed in system")
			}
			out = append(out, anthropicBlock{Type: "tool_result", ToolUseID: c.ToolResultID, Content: toolResultFromIR(c.ToolResult), IsError: c.ToolIsError})
		case KindImage:
			if systemCtx {
				continue
			}
			src := &anthropicImageSource{}
			if c.ImageURL != "" {
				src.Type, src.URL = "url", c.ImageURL
			} else {
				src.Type, src.MediaType, src.Data = "base64", c.ImageMediaType, c.ImageDataB64
			}
			out = append(out, anthropicBlock{Type: "image", Source: src})
		default:
			return nil, fmt.Errorf("unsupported content kind %q", c.Kind)
		}
	}
	return out, nil
}

// toolResultFromIR renders the IR tool result (a JSON string or arbitrary JSON) as
// Anthropic tool_result.content. Anthropic accepts a plain string; non-string JSON is
// stringified so the model still sees it.
func toolResultFromIR(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`""`)
	}
	if raw[0] == '"' {
		return raw
	}
	s, _ := json.Marshal(string(raw))
	return s
}

func mergeAdjacentAnthropic(msgs []anthropicMsg) []anthropicMsg {
	if len(msgs) < 2 {
		return msgs
	}
	out := make([]anthropicMsg, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1].Role == m.Role && m.Role != "system" {
			var a, b []json.RawMessage
			_ = json.Unmarshal(out[n-1].Content, &a)
			_ = json.Unmarshal(m.Content, &b)
			merged, _ := json.Marshal(append(a, b...))
			out[n-1].Content = merged
			continue
		}
		out = append(out, m)
	}
	return out
}
