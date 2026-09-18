// Package translate converts requests, responses and streaming events between the three
// supported wire protocols (OpenAI Chat Completions, OpenAI Responses, Anthropic Messages)
// through a canonical intermediate representation (IR): inbound -> IR -> target.
//
// Rationale (see docs/protocol-translation-design.md): a shared IR means each protocol needs
// only a "to IR" and a "from IR" adapter for requests, responses and streams; every one of the
// six cross-protocol directions then works without writing O(N^2) bespoke converters.
//
// This file defines the IR types. Codec implementations live in per-protocol files and are
// wired in translate.go. STATUS: skeleton — types and interfaces are defined; the per-protocol
// codecs are filled in by later milestones.
//
// Design rules for the IR (phase 1):
//   - Text and tool calling are modelled semantically.
//   - Reasoning/thinking is NOT interpreted, but it MUST round-trip: Codex multi-turn carries
//     opaque `reasoning` items (encrypted_content) and Anthropic multi-turn tool use requires the
//     `thinking` block + `signature` to be echoed back unchanged, otherwise the next turn is
//     rejected. So the IR carries them as opaque payloads (Thinking* fields) that FromIR emits
//     verbatim into the target protocol when the target can carry them.
//   - The IR is stateless. OpenAI Responses server-side state (`previous_response_id`, `store`)
//     cannot be reproduced across providers; a Responses request that relies on it is rejected
//     by the Responses RequestCodec with a clear 4xx (see design doc §7/§8).
package translate

import (
	"encoding/json"

	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
)

// Role is a message author in the IR.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentKind tags a piece of content. Not every protocol supports every kind; conversion that
// cannot represent a kind follows the lossy/unsupported policy in the design doc (§7).
type ContentKind string

const (
	KindText       ContentKind = "text"
	KindImage      ContentKind = "image"       // phase 2 (best-effort carry in phase 1)
	KindThinking   ContentKind = "thinking"    // reasoning; phase 1 = opaque round-trip only
	KindToolUse    ContentKind = "tool_use"    // assistant calls a tool
	KindToolResult ContentKind = "tool_result" // result of a tool call fed back in
)

// Content is one block within a message or response.
type Content struct {
	Kind ContentKind
	Text string // KindText; also the visible summary text of KindThinking when the source exposes it

	// KindThinking — opaque round-trip payload. Exactly one of these is populated depending on
	// the source protocol; FromIR emits it verbatim if the target protocol has a slot for it.
	//   Anthropic: thinking block text + signature (must be echoed back for multi-turn tool use)
	//   OpenAI Responses: the whole `reasoning` item incl. encrypted_content
	ThinkingSignature string          // Anthropic `signature`
	ThinkingRaw       json.RawMessage // OpenAI Responses reasoning item, or redacted_thinking data

	// KindToolUse
	ToolCallID string
	ToolName   string
	ToolInput  json.RawMessage // complete arguments as a JSON object

	// KindToolResult
	ToolResultID string          // the ToolCallID this result answers
	ToolResult   json.RawMessage // result payload (JSON string or object)
	ToolIsError  bool

	// KindImage (phase 2)
	ImageMediaType string
	ImageDataB64   string
	ImageURL       string
}

// Message is one turn of the conversation.
type Message struct {
	Role    Role
	Content []Content
}

// Tool is a function/tool definition offered to the model.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema of the parameters/input
}

// ToolChoiceMode is how the model is allowed/forced to call tools.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"     // default; model decides
	ToolChoiceNone     ToolChoiceMode = "none"     // never call a tool
	ToolChoiceRequired ToolChoiceMode = "required" // must call some tool (OpenAI required / Anthropic any)
	ToolChoiceNamed    ToolChoiceMode = "named"    // must call ToolChoice.Name
)

// ToolChoice constrains tool calling. Zero value = unset (= auto on the wire).
//
// Invariant: ToIR normalises an explicit wire "auto" to the zero value and FromIR omits
// tool_choice for both "" and ToolChoiceAuto. Omitting is the safe encoding everywhere
// (Anthropic rejects tool_choice without tools) and keeps the IR round-trip stable.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string // ToolChoiceNamed
}

// Request is the protocol-neutral form of an inbound request.
type Request struct {
	Model    string
	System   []Content // system / instructions, hoisted out of the message list
	Messages []Message
	Tools    []Tool

	ToolChoice        ToolChoice
	ParallelToolCalls *bool // nil = unset

	// MaxTokens nil = source omitted it. FromIR for a target that REQUIRES it (Anthropic) must
	// then apply Options.DefaultMaxTokens; never write 0 to the wire.
	MaxTokens   *int64
	Temperature *float64
	TopP        *float64
	Stop        []string
	Stream      bool

	// ResponseFormat is the source's structured-output request (e.g. OpenAI response_format /
	// text.format) carried opaquely; phase 1 emits it only when the target has the same concept.
	ResponseFormat json.RawMessage
}

// StopReason is the normalized reason generation ended.
type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopToolUse       StopReason = "tool_use"
	StopStopSequence  StopReason = "stop_sequence"
	StopContentFilter StopReason = "content_filter"
	StopOther         StopReason = "other"
)

// Response is the protocol-neutral form of a complete (non-streaming) response.
type Response struct {
	ID         string // upstream response/message id when present
	Model      string
	Content    []Content // assistant output blocks (text / tool_use / thinking)
	StopReason StopReason
	Usage      protocol.Usage
}

// StreamEventKind enumerates the canonical streaming events. A translator turns the source
// protocol's SSE events into this sequence, then renders it into the target protocol's events.
type StreamEventKind string

const (
	EventMessageStart   StreamEventKind = "message_start"
	EventTextDelta      StreamEventKind = "text_delta"
	EventThinkingDelta  StreamEventKind = "thinking_delta"
	EventThinkingDone   StreamEventKind = "thinking_done"    // carries the opaque signature/raw for round-trip
	EventToolUseStart   StreamEventKind = "tool_use_start"   // a tool call begins (Index, ToolName, ToolCallID known)
	EventToolInputDelta StreamEventKind = "tool_input_delta" // incremental arguments fragment
	EventToolUseStop    StreamEventKind = "tool_use_stop"
	EventMessageStop    StreamEventKind = "message_stop" // carries StopReason + Usage
	EventError          StreamEventKind = "error"
)

// StreamEvent is one canonical streaming event.
//
// Index identifies the output slot a delta belongs to. Both wire formats need it to reassemble
// parallel tool calls: OpenAI tool_calls deltas carry `index` (the call id only appears in the
// first fragment) and Anthropic uses the content block index. Text deltas use Index too so a
// text block interleaved with tool calls maps to the right block on the target side.
type StreamEvent struct {
	Kind  StreamEventKind
	Index int

	ID    string // message_start: upstream message/response id
	Model string // message_start

	Text string // text_delta / thinking_delta

	// tool_use_*
	ToolCallID string
	ToolName   string
	// ToolInputDelta is a raw fragment of the arguments JSON, NOT valid JSON on its own. Both
	// OpenAI (function.arguments fragments) and Anthropic (input_json_delta.partial_json) stream
	// fragments, so the IR forwards them verbatim without accumulating.
	ToolInputDelta string

	// thinking_done — opaque round-trip payload (see Content.Thinking*)
	ThinkingSignature string
	ThinkingRaw       json.RawMessage

	StopReason StopReason     // message_stop
	Usage      protocol.Usage // message_stop
	Err        string         // error
}
