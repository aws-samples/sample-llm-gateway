package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
)

// ======================================================================================
// OpenAI Responses API — request
// ======================================================================================
//
// Shapes handled are the ones real Codex traffic uses (see testdata/fixtures/codex):
//   - instructions (string) → IR System
//   - input: string | []item where item.type ∈ message{role: developer|system|user|assistant,
//     content: string | [{type:input_text|output_text|input_image|...}]}, function_call{call_id,name,arguments},
//     function_call_output{call_id,output}, reasoning{encrypted_content,summary}
//   - tools[]: {type:function,name,description,parameters} | {type:namespace,tools:[...]} (flattened)
//     hosted types (web_search, ...) are dropped — they have no cross-provider meaning
//   - tool_choice: "auto"|"none"|"required"|{type:function,name}
//   - max_output_tokens (optional → IR MaxTokens nil), temperature, top_p, stream
//   - store / previous_response_id: the IR is stateless. previous_response_id is REJECTED with a
//     clear error (design §8.7); store is ignored. Codex sends store:false + full history.
type openAIResponsesRequest struct{}

type responsesReq struct {
	Model              string            `json:"model"`
	Instructions       string            `json:"instructions,omitempty"`
	Input              json.RawMessage   `json:"input"`
	Tools              []json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens    *int64            `json:"max_output_tokens,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Text               json.RawMessage   `json:"text,omitempty"`
	Reasoning          json.RawMessage   `json:"reasoning,omitempty"`
	Include            []string          `json:"include,omitempty"`
}

type responsesItem struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"` // string | []responsesPart
	// function_call / function_call_output
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"` // string | []part
	// reasoning
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	Summary          json.RawMessage `json:"summary,omitempty"`
	Status           string          `json:"status,omitempty"`
	// raw holds the item's original bytes so an opaque item (reasoning) can be replayed verbatim
	// instead of being re-marshaled from the typed fields above, which would silently drop any
	// field this struct does not model (e.g. a future reasoning field). Excluded from marshaling.
	raw json.RawMessage
}

// UnmarshalJSON keeps the original bytes in raw while decoding the modeled fields.
func (it *responsesItem) UnmarshalJSON(b []byte) error {
	type alias responsesItem
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*it = responsesItem(a)
	it.raw = append(json.RawMessage(nil), b...)
	return nil
}

// rawItem returns the original bytes if captured, else a best-effort re-marshal of the typed fields.
func (it *responsesItem) rawItem() json.RawMessage {
	if len(it.raw) > 0 {
		return it.raw
	}
	b, _ := json.Marshal(it)
	return b
}

type responsesPart struct {
	Type     string `json:"type"` // input_text | output_text | input_image | refusal
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
}

type responsesToolDef struct {
	Type        string             `json:"type"`
	Name        string             `json:"name,omitempty"`
	Description string             `json:"description,omitempty"`
	Parameters  json.RawMessage    `json:"parameters,omitempty"`
	Strict      *bool              `json:"strict,omitempty"`
	Tools       []responsesToolDef `json:"tools,omitempty"` // namespace
}

// ErrStatefulResponses is returned when a Responses request relies on server-side state.
var ErrStatefulResponses = errors.New("openai responses request: previous_response_id is not supported by the gateway (stateless translation); send the full conversation in input and set store:false")

func (openAIResponsesRequest) ToIR(body []byte) (*Request, error) {
	var w responsesReq
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("openai responses request: %w", err)
	}
	if w.Model == "" {
		return nil, errors.New("openai responses request: model is required")
	}
	if w.PreviousResponseID != "" {
		return nil, ErrStatefulResponses
	}
	r := &Request{
		Model:             w.Model,
		MaxTokens:         w.MaxOutputTokens,
		Temperature:       w.Temperature,
		TopP:              w.TopP,
		Stream:            w.Stream,
		ParallelToolCalls: w.ParallelToolCalls,
	}
	if w.Instructions != "" {
		r.System = append(r.System, Content{Kind: KindText, Text: w.Instructions})
	}
	// text.format → IR ResponseFormat (opaque)
	if len(w.Text) > 0 && string(w.Text) != "null" {
		var tx struct {
			Format json.RawMessage `json:"format"`
		}
		if json.Unmarshal(w.Text, &tx) == nil && len(tx.Format) > 0 && string(tx.Format) != "null" {
			r.ResponseFormat = tx.Format
		}
	}
	// input: string or []item
	if len(w.Input) > 0 && string(w.Input) != "null" {
		if w.Input[0] == '"' {
			var s string
			_ = json.Unmarshal(w.Input, &s)
			r.Messages = append(r.Messages, Message{Role: RoleUser, Content: []Content{{Kind: KindText, Text: s}}})
		} else {
			var items []responsesItem
			if err := json.Unmarshal(w.Input, &items); err != nil {
				return nil, fmt.Errorf("openai responses request: input: %w", err)
			}
			for i, it := range items {
				switch it.Type {
				case "message", "":
					blocks, err := responsesContentToIR(it.Content)
					if err != nil {
						return nil, fmt.Errorf("openai responses request: input[%d]: %w", i, err)
					}
					switch it.Role {
					case "developer", "system":
						// Responses distinguishes request-level `instructions` (→ IR System) from
						// developer/system items inside `input` (Codex sends both). Keep the latter
						// in Messages as RoleSystem so the distinction survives a round trip.
						r.Messages = append(r.Messages, Message{Role: RoleSystem, Content: blocks})
					case "user":
						r.Messages = append(r.Messages, Message{Role: RoleUser, Content: blocks})
					case "assistant":
						r.Messages = appendAssistant(r.Messages, blocks...)
					default:
						return nil, fmt.Errorf("openai responses request: input[%d]: unknown role %q", i, it.Role)
					}
				case "function_call":
					args := json.RawMessage(it.Arguments)
					if len(args) == 0 || !json.Valid(args) {
						args = json.RawMessage("{}")
					}
					r.Messages = appendAssistant(r.Messages, Content{Kind: KindToolUse, ToolCallID: it.CallID, ToolName: it.Name, ToolInput: args})
				case "function_call_output":
					r.Messages = append(r.Messages, Message{Role: RoleTool, Content: []Content{{Kind: KindToolResult, ToolResultID: it.CallID, ToolResult: responsesOutputToIR(it.Output)}}})
				case "reasoning":
					// Opaque round-trip payload (encrypted_content + any unmodeled fields); attach
					// to the assistant turn verbatim so it replays byte-for-byte.
					r.Messages = appendAssistant(r.Messages, Content{Kind: KindThinking, ThinkingRaw: it.rawItem(), Text: reasoningSummaryText(it.Summary)})
				default:
					return nil, fmt.Errorf("openai responses request: input[%d]: unsupported item type %q", i, it.Type)
				}
			}
		}
	}
	for _, raw := range w.Tools {
		var td responsesToolDef
		if json.Unmarshal(raw, &td) != nil {
			continue
		}
		r.Tools = append(r.Tools, flattenResponsesTools(td)...)
	}
	if len(w.ToolChoice) > 0 && string(w.ToolChoice) != "null" {
		if w.ToolChoice[0] == '"' {
			var s string
			_ = json.Unmarshal(w.ToolChoice, &s)
			switch s {
			case "auto":
				// wire default → IR zero value (see ToolChoice doc)
			case "none":
				r.ToolChoice = ToolChoice{Mode: ToolChoiceNone}
			case "required":
				r.ToolChoice = ToolChoice{Mode: ToolChoiceRequired}
			}
		} else {
			var obj struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(w.ToolChoice, &obj) == nil && obj.Type == "function" && obj.Name != "" {
				r.ToolChoice = ToolChoice{Mode: ToolChoiceNamed, Name: obj.Name}
			}
		}
	}
	return r, nil
}

// appendAssistant merges consecutive assistant-side items (message, function_call, reasoning)
// into one IR assistant message — Responses splits them into separate top-level items.
func appendAssistant(msgs []Message, cs ...Content) []Message {
	if n := len(msgs); n > 0 && msgs[n-1].Role == RoleAssistant {
		msgs[n-1].Content = append(msgs[n-1].Content, cs...)
		return msgs
	}
	return append(msgs, Message{Role: RoleAssistant, Content: cs})
}

func flattenResponsesTools(td responsesToolDef) []Tool {
	switch td.Type {
	case "function":
		return []Tool{{Name: td.Name, Description: td.Description, Schema: td.Parameters}}
	case "namespace":
		var out []Tool
		for _, t := range td.Tools {
			out = append(out, flattenResponsesTools(t)...)
		}
		return out
	}
	return nil // hosted tools (web_search, file_search, ...) have no cross-provider meaning
}

func responsesContentToIR(raw json.RawMessage) ([]Content, error) {
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
	var parts []responsesPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	out := make([]Content, 0, len(parts))
	for i, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, Content{Kind: KindText, Text: p.Text})
		case "refusal":
			out = append(out, Content{Kind: KindText, Text: p.Refusal})
		case "input_image":
			c := Content{Kind: KindImage}
			if mt, data, ok := parseDataURL(p.ImageURL); ok {
				c.ImageMediaType, c.ImageDataB64 = mt, data
			} else {
				c.ImageURL = p.ImageURL
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("content[%d]: unsupported part type %q", i, p.Type)
		}
	}
	return out, nil
}

func responsesOutputToIR(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`""`)
	}
	if raw[0] == '"' {
		return raw
	}
	// []part → concatenated text
	var parts []responsesPart
	if json.Unmarshal(raw, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		b, _ := json.Marshal(sb.String())
		return b
	}
	return raw
}

func reasoningSummaryText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	return ""
}

// ---- FromIR (IR → Responses request) ------------------------------------------------------

func (openAIResponsesRequest) FromIR(r *Request, providerModel string, _ Options) ([]byte, error) {
	out := map[string]any{"model": providerModel, "store": false}
	if r.Stream {
		out["stream"] = true
	}
	if r.MaxTokens != nil && *r.MaxTokens > 0 {
		out["max_output_tokens"] = *r.MaxTokens
	}
	if r.Temperature != nil {
		out["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		out["top_p"] = *r.TopP
	}
	if r.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *r.ParallelToolCalls
	}
	if len(r.System) > 0 {
		out["instructions"] = joinText(r.System)
	}
	if len(r.ResponseFormat) > 0 {
		out["text"] = map[string]any{"format": r.ResponseFormat}
	}
	var items []any
	for i, m := range r.Messages {
		switch m.Role {
		case RoleSystem:
			var parts []map[string]any
			for _, c := range m.Content {
				if c.Kind == KindText {
					parts = append(parts, map[string]any{"type": "input_text", "text": c.Text})
				}
			}
			if len(parts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "developer", "content": parts})
			}
		case RoleUser:
			var parts []map[string]any
			for _, c := range m.Content {
				switch c.Kind {
				case KindText:
					parts = append(parts, map[string]any{"type": "input_text", "text": c.Text})
				case KindImage:
					parts = append(parts, map[string]any{"type": "input_image", "image_url": imageDataURL(c)})
				case KindToolResult:
					items = append(items, map[string]any{"type": "function_call_output", "call_id": c.ToolResultID, "output": toolResultString(c)})
				}
			}
			if len(parts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "user", "content": parts})
			}
		case RoleTool:
			for _, c := range m.Content {
				if c.Kind == KindToolResult {
					items = append(items, map[string]any{"type": "function_call_output", "call_id": c.ToolResultID, "output": toolResultString(c)})
				}
			}
		case RoleAssistant:
			var parts []map[string]any
			flushParts := func() {
				if len(parts) > 0 {
					items = append(items, map[string]any{"type": "message", "role": "assistant", "content": parts})
					parts = nil
				}
			}
			for _, c := range m.Content {
				switch c.Kind {
				case KindText:
					parts = append(parts, map[string]any{"type": "output_text", "text": c.Text})
				case KindToolUse:
					flushParts()
					args := string(c.ToolInput)
					if args == "" {
						args = "{}"
					}
					items = append(items, map[string]any{"type": "function_call", "call_id": c.ToolCallID, "name": c.ToolName, "arguments": args})
				case KindThinking:
					flushParts()
					// Only a genuine Responses reasoning item (encrypted_content) can be replayed.
					var probe struct {
						Type string `json:"type"`
					}
					if len(c.ThinkingRaw) > 0 && json.Unmarshal(c.ThinkingRaw, &probe) == nil && probe.Type == "reasoning" {
						// Embed the captured bytes verbatim: going through map[string]any would
						// re-order keys and turn integers into float64 (1e+06-style output).
						items = append(items, json.RawMessage(c.ThinkingRaw))
					}
					// Anthropic thinking (signature) cannot be replayed into Responses; dropped.
				}
			}
			flushParts()
		default:
			return nil, fmt.Errorf("openai responses request: messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	if items == nil {
		items = []any{}
	}
	out["input"] = items
	var tools []map[string]any
	for _, t := range r.Tools {
		params := t.Schema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": params})
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	switch r.ToolChoice.Mode {
	case "", ToolChoiceAuto:
	case ToolChoiceRequired:
		out["tool_choice"] = "required"
	case ToolChoiceNone:
		out["tool_choice"] = "none"
	case ToolChoiceNamed:
		out["tool_choice"] = map[string]any{"type": "function", "name": r.ToolChoice.Name}
	}
	return json.Marshal(out)
}

func toolResultString(c Content) string {
	if len(c.ToolResult) > 0 && c.ToolResult[0] == '"' {
		var s string
		_ = json.Unmarshal(c.ToolResult, &s)
		if c.ToolIsError && !strings.HasPrefix(s, "Error") {
			s = "Error: " + s
		}
		return s
	}
	return string(c.ToolResult)
}

// ======================================================================================
// OpenAI Responses API — non-streaming response
// ======================================================================================

type openAIResponsesResponse struct{}

type responsesResp struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	Model             string          `json:"model"`
	Status            string          `json:"status"`
	Output            []responsesItem `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (openAIResponsesResponse) ToIR(body []byte) (*Response, error) {
	var w responsesResp
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("openai responses response: %w", err)
	}
	r := &Response{ID: w.ID, Model: w.Model, Usage: protocol.ParseUsage(protocol.OpenAIResponses, body)}
	hasTool := false
	for _, it := range w.Output {
		switch it.Type {
		case "message":
			blocks, err := responsesContentToIR(it.Content)
			if err != nil {
				return nil, fmt.Errorf("openai responses response: %w", err)
			}
			r.Content = append(r.Content, blocks...)
		case "function_call":
			hasTool = true
			args := json.RawMessage(it.Arguments)
			if len(args) == 0 || !json.Valid(args) {
				args, _ = json.Marshal(map[string]string{"_raw": it.Arguments})
			}
			r.Content = append(r.Content, Content{Kind: KindToolUse, ToolCallID: it.CallID, ToolName: it.Name, ToolInput: args})
		case "reasoning":
			// Verbatim opaque payload (see rawItem): keeps fields this struct does not model.
			r.Content = append(r.Content, Content{Kind: KindThinking, ThinkingRaw: it.rawItem(), Text: reasoningSummaryText(it.Summary)})
		}
	}
	r.StopReason = responsesStatusToIR(w.Status, w.IncompleteDetails, hasTool)
	return r, nil
}

func responsesStatusToIR(status string, inc *struct {
	Reason string `json:"reason"`
}, hasTool bool) StopReason {
	switch status {
	case "completed", "":
		if hasTool {
			return StopToolUse
		}
		return StopEndTurn
	case "incomplete":
		if inc != nil {
			switch inc.Reason {
			case "max_output_tokens":
				return StopMaxTokens
			case "content_filter":
				return StopContentFilter
			}
		}
		return StopOther
	case "failed", "cancelled":
		return StopOther
	}
	return StopOther
}

func (openAIResponsesResponse) FromIR(r *Response) ([]byte, error) {
	id := r.ID
	if id == "" {
		id = "resp_" + randomID()
	}
	var output []any
	var parts []map[string]any
	flush := func() {
		if len(parts) > 0 {
			output = append(output, map[string]any{"type": "message", "id": "msg_" + randomID(), "status": "completed", "role": "assistant", "content": parts})
			parts = nil
		}
	}
	for _, c := range r.Content {
		switch c.Kind {
		case KindText:
			parts = append(parts, map[string]any{"type": "output_text", "text": c.Text, "annotations": []any{}})
		case KindToolUse:
			flush()
			args := string(c.ToolInput)
			if args == "" {
				args = "{}"
			}
			output = append(output, map[string]any{"type": "function_call", "id": "fc_" + randomID(), "status": "completed", "call_id": c.ToolCallID, "name": c.ToolName, "arguments": args})
		case KindThinking:
			flush()
			var probe struct {
				Type string `json:"type"`
			}
			if len(c.ThinkingRaw) > 0 && json.Unmarshal(c.ThinkingRaw, &probe) == nil && probe.Type == "reasoning" {
				output = append(output, json.RawMessage(c.ThinkingRaw)) // verbatim, see request FromIR
			} else if c.Text != "" {
				// Visible reasoning from another protocol → summary-only reasoning item (not replayable).
				output = append(output, map[string]any{"type": "reasoning", "id": "rs_" + randomID(), "summary": []map[string]any{{"type": "summary_text", "text": c.Text}}})
			}
		}
	}
	flush()
	if output == nil {
		output = []any{}
	}
	status, incomplete := responsesStatusFromIR(r.StopReason)
	obj := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status, "model": r.Model,
		"output": output, "error": nil, "incomplete_details": incomplete,
		"usage": responsesUsageFromIR(r.Usage),
	}
	return json.Marshal(obj)
}

func responsesStatusFromIR(s StopReason) (string, any) {
	switch s {
	case StopMaxTokens:
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case StopContentFilter:
		return "incomplete", map[string]any{"reason": "content_filter"}
	}
	return "completed", nil
}

// responsesUsageFromIR renders IR usage in Responses shape (input_tokens includes cached).
func responsesUsageFromIR(u protocol.Usage) any {
	if !u.Found {
		return nil
	}
	in := u.Input + u.CacheRead + u.CacheWrite
	return map[string]any{
		"input_tokens":          in,
		"output_tokens":         u.Output,
		"total_tokens":          in + u.Output,
		"input_tokens_details":  map[string]any{"cached_tokens": u.CacheRead},
		"output_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning},
	}
}
