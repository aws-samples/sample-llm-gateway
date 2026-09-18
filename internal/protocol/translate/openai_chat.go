package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// openAIChatRequest is the RequestCodec for OpenAI Chat Completions.
//
// FromIR (the Claude Code → GPT direction) renders:
//   - IR System → a leading {role:"system"} message
//   - IR RoleSystem inside Messages → {role:"system"} in place (OpenAI allows it)
//   - KindToolUse blocks in an assistant message → message.tool_calls[]
//   - KindToolResult blocks → one {role:"tool", tool_call_id, content} message each
//   - KindThinking → dropped (Chat Completions has no slot to replay reasoning; the model
//     regenerates it). KindImage → image_url content part.
//   - MaxTokens → max_completion_tokens (o-series / GPT-5 reject max_tokens)
//   - Stream → stream + stream_options.include_usage=true so the final chunk carries usage
type openAIChatRequest struct{}

// ---- wire types -------------------------------------------------------------------------

type chatReq struct {
	Model               string          `json:"model"`
	Messages            []chatMsg       `json:"messages"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int64          `json:"max_tokens,omitempty"` // legacy; read on ToIR only
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"` // string | []string
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *chatStreamOpts `json:"stream_options,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
}

type chatStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"` // string | []part | null
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type chatPart struct {
	Type     string        `json:"type"` // text | image_url
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // function
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string
}

type chatTool struct {
	Type     string      `json:"type"` // function
	Function chatToolDef `json:"function"`
}

type chatToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ---- FromIR -----------------------------------------------------------------------------

func (openAIChatRequest) FromIR(r *Request, providerModel string, _ Options) ([]byte, error) {
	w := chatReq{
		Model:             providerModel,
		Temperature:       r.Temperature,
		TopP:              r.TopP,
		Stream:            r.Stream,
		ParallelToolCalls: r.ParallelToolCalls,
		ResponseFormat:    r.ResponseFormat,
	}
	if r.MaxTokens != nil && *r.MaxTokens > 0 {
		w.MaxCompletionTokens = r.MaxTokens
	}
	if len(r.Stop) > 0 {
		w.Stop, _ = json.Marshal(r.Stop)
	}
	if r.Stream {
		w.StreamOptions = &chatStreamOpts{IncludeUsage: true}
	}
	if len(r.System) > 0 {
		txt := joinText(r.System)
		c, _ := json.Marshal(txt)
		w.Messages = append(w.Messages, chatMsg{Role: "system", Content: c})
	}
	for i, m := range r.Messages {
		msgs, err := irMessageToChat(m)
		if err != nil {
			return nil, fmt.Errorf("openai chat request: messages[%d]: %w", i, err)
		}
		w.Messages = append(w.Messages, msgs...)
	}
	for _, t := range r.Tools {
		params := t.Schema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		w.Tools = append(w.Tools, chatTool{Type: "function", Function: chatToolDef{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	switch r.ToolChoice.Mode {
	case "", ToolChoiceAuto:
	case ToolChoiceRequired:
		w.ToolChoice = json.RawMessage(`"required"`)
	case ToolChoiceNone:
		w.ToolChoice = json.RawMessage(`"none"`)
	case ToolChoiceNamed:
		w.ToolChoice, _ = json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": r.ToolChoice.Name}})
	}
	return json.Marshal(w)
}

// irMessageToChat renders one IR message as one or more Chat messages. Tool results become
// separate role:tool messages; tool_use blocks attach to the assistant message as tool_calls.
func irMessageToChat(m Message) ([]chatMsg, error) {
	switch m.Role {
	case RoleSystem:
		c, _ := json.Marshal(joinText(m.Content))
		return []chatMsg{{Role: "system", Content: c}}, nil
	case RoleTool:
		return toolResultsToChat(m.Content)
	case RoleUser:
		// Users can carry tool_result blocks (Anthropic style) mixed with text. Split them out.
		var parts []chatPart
		var results []Content
		for _, c := range m.Content {
			switch c.Kind {
			case KindText:
				parts = append(parts, chatPart{Type: "text", Text: c.Text})
			case KindImage:
				parts = append(parts, chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: imageDataURL(c)}})
			case KindToolResult:
				results = append(results, c)
			case KindThinking:
				// not meaningful on a user turn
			default:
				return nil, fmt.Errorf("unsupported content kind %q in user message", c.Kind)
			}
		}
		var out []chatMsg
		// OpenAI requires tool messages to directly follow the assistant tool_calls message, so
		// emit results first, then any accompanying user text.
		if len(results) > 0 {
			tm, err := toolResultsToChat(results)
			if err != nil {
				return nil, err
			}
			out = append(out, tm...)
		}
		if len(parts) > 0 {
			out = append(out, chatMsg{Role: "user", Content: chatContent(parts)})
		}
		return out, nil
	case RoleAssistant:
		msg := chatMsg{Role: "assistant"}
		var parts []chatPart
		for _, c := range m.Content {
			switch c.Kind {
			case KindText:
				parts = append(parts, chatPart{Type: "text", Text: c.Text})
			case KindToolUse:
				args := string(c.ToolInput)
				if args == "" {
					args = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, chatToolCall{ID: c.ToolCallID, Type: "function", Function: chatFunction{Name: c.ToolName, Arguments: args}})
			case KindThinking:
				// Chat Completions cannot replay reasoning; drop.
			case KindImage:
				// assistant-side images are not a Chat Completions concept; drop.
			default:
				return nil, fmt.Errorf("unsupported content kind %q in assistant message", c.Kind)
			}
		}
		if len(parts) > 0 {
			msg.Content = chatContent(parts)
		} else if len(msg.ToolCalls) == 0 {
			msg.Content = json.RawMessage(`""`)
		}
		return []chatMsg{msg}, nil
	}
	return nil, fmt.Errorf("unsupported role %q", m.Role)
}

func toolResultsToChat(cs []Content) ([]chatMsg, error) {
	var out []chatMsg
	for _, c := range cs {
		if c.Kind != KindToolResult {
			return nil, fmt.Errorf("tool message may only carry tool_result, got %q", c.Kind)
		}
		// OpenAI tool content is a string.
		var s string
		if len(c.ToolResult) > 0 && c.ToolResult[0] == '"' {
			_ = json.Unmarshal(c.ToolResult, &s)
		} else {
			s = string(c.ToolResult)
		}
		if c.ToolIsError && !strings.HasPrefix(s, "Error") {
			s = "Error: " + s
		}
		content, _ := json.Marshal(s)
		out = append(out, chatMsg{Role: "tool", ToolCallID: c.ToolResultID, Content: content})
	}
	return out, nil
}

// chatContent renders parts as a plain string when they are all text (the most compatible
// shape), otherwise as a parts array.
func chatContent(parts []chatPart) json.RawMessage {
	allText := true
	for _, p := range parts {
		if p.Type != "text" {
			allText = false
			break
		}
	}
	if allText {
		var sb strings.Builder
		for i, p := range parts {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
		b, _ := json.Marshal(sb.String())
		return b
	}
	b, _ := json.Marshal(parts)
	return b
}

func imageDataURL(c Content) string {
	if c.ImageURL != "" {
		return c.ImageURL
	}
	mt := c.ImageMediaType
	if mt == "" {
		mt = "image/png"
	}
	return "data:" + mt + ";base64," + c.ImageDataB64
}

func joinText(cs []Content) string {
	var sb strings.Builder
	for _, c := range cs {
		if c.Kind == KindText {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// ---- ToIR -------------------------------------------------------------------------------

func (openAIChatRequest) ToIR(body []byte) (*Request, error) {
	var w chatReq
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("openai chat request: %w", err)
	}
	if w.Model == "" {
		return nil, errors.New("openai chat request: model is required")
	}
	r := &Request{
		Model:             w.Model,
		Temperature:       w.Temperature,
		TopP:              w.TopP,
		Stream:            w.Stream,
		ParallelToolCalls: w.ParallelToolCalls,
		ResponseFormat:    w.ResponseFormat,
	}
	switch {
	case w.MaxCompletionTokens != nil:
		r.MaxTokens = w.MaxCompletionTokens
	case w.MaxTokens != nil:
		r.MaxTokens = w.MaxTokens
	}
	if len(w.Stop) > 0 && string(w.Stop) != "null" {
		if w.Stop[0] == '"' {
			var s string
			_ = json.Unmarshal(w.Stop, &s)
			r.Stop = []string{s}
		} else {
			_ = json.Unmarshal(w.Stop, &r.Stop)
		}
	}
	// Leading system/developer messages hoist into IR System; later ones stay in place.
	leading := true
	for i, m := range w.Messages {
		switch m.Role {
		case "system", "developer":
			blocks, err := chatContentToIR(m.Content)
			if err != nil {
				return nil, fmt.Errorf("openai chat request: messages[%d]: %w", i, err)
			}
			if leading {
				r.System = append(r.System, blocks...)
			} else {
				r.Messages = append(r.Messages, Message{Role: RoleSystem, Content: blocks})
			}
			continue
		}
		leading = false
		switch m.Role {
		case "user":
			blocks, err := chatContentToIR(m.Content)
			if err != nil {
				return nil, fmt.Errorf("openai chat request: messages[%d]: %w", i, err)
			}
			r.Messages = append(r.Messages, Message{Role: RoleUser, Content: blocks})
		case "assistant":
			blocks, err := chatContentToIR(m.Content)
			if err != nil {
				return nil, fmt.Errorf("openai chat request: messages[%d]: %w", i, err)
			}
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Function.Arguments)
				if len(args) == 0 || !json.Valid(args) {
					args = json.RawMessage("{}")
				}
				blocks = append(blocks, Content{Kind: KindToolUse, ToolCallID: tc.ID, ToolName: tc.Function.Name, ToolInput: args})
			}
			r.Messages = append(r.Messages, Message{Role: RoleAssistant, Content: blocks})
		case "tool":
			var s string
			if len(m.Content) > 0 && m.Content[0] == '"' {
				_ = json.Unmarshal(m.Content, &s)
			} else {
				// parts array → concatenate text
				blocks, _ := chatContentToIR(m.Content)
				s = joinText(blocks)
			}
			res, _ := json.Marshal(s)
			r.Messages = append(r.Messages, Message{Role: RoleTool, Content: []Content{{Kind: KindToolResult, ToolResultID: m.ToolCallID, ToolResult: res}}})
		default:
			return nil, fmt.Errorf("openai chat request: messages[%d]: unknown role %q", i, m.Role)
		}
	}
	for _, t := range w.Tools {
		if t.Type != "function" && t.Type != "" {
			continue // hosted tool types have no cross-protocol meaning
		}
		r.Tools = append(r.Tools, Tool{Name: t.Function.Name, Description: t.Function.Description, Schema: t.Function.Parameters})
	}
	if len(w.ToolChoice) > 0 && string(w.ToolChoice) != "null" {
		if w.ToolChoice[0] == '"' {
			var s string
			_ = json.Unmarshal(w.ToolChoice, &s)
			switch s {
			case "auto":
				r.ToolChoice = ToolChoice{Mode: ToolChoiceAuto}
			case "none":
				r.ToolChoice = ToolChoice{Mode: ToolChoiceNone}
			case "required":
				r.ToolChoice = ToolChoice{Mode: ToolChoiceRequired}
			}
		} else {
			var obj struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(w.ToolChoice, &obj) == nil && obj.Function.Name != "" {
				r.ToolChoice = ToolChoice{Mode: ToolChoiceNamed, Name: obj.Function.Name}
			}
		}
	}
	return r, nil
}

func chatContentToIR(raw json.RawMessage) ([]Content, error) {
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
	var parts []chatPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	out := make([]Content, 0, len(parts))
	for i, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, Content{Kind: KindText, Text: p.Text})
		case "image_url":
			c := Content{Kind: KindImage}
			if p.ImageURL != nil {
				if mt, data, ok := parseDataURL(p.ImageURL.URL); ok {
					c.ImageMediaType, c.ImageDataB64 = mt, data
				} else {
					c.ImageURL = p.ImageURL.URL
				}
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("content[%d]: unsupported part type %q", i, p.Type)
		}
	}
	return out, nil
}

// parseDataURL splits "data:<mediatype>;base64,<data>".
func parseDataURL(u string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := u[len("data:"):]
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return "", "", false
	}
	return rest[:semi], rest[semi+len(";base64,"):], true
}
