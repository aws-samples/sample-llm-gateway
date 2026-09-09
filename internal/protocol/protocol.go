// Package protocol contains the minimal body handling for each supported wire
// protocol: extracting/rewriting the model, making sure usage is emitted, and
// parsing usage out of streaming and non-streaming responses. No format conversion.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
)

// Protocol identifies the wire format of a request.
type Protocol string

const (
	OpenAIChat      Protocol = config.EndpointOpenAIChat
	OpenAIResponses Protocol = config.EndpointOpenAIResponses
	Anthropic       Protocol = config.EndpointAnthropic
)

// UpstreamPath is the path appended to the provider's base URL for each protocol.
func (p Protocol) UpstreamPath() string {
	switch p {
	case OpenAIChat:
		return "/chat/completions"
	case OpenAIResponses:
		return "/responses"
	case Anthropic:
		return "/messages"
	}
	return ""
}

// Detect maps an inbound path to a protocol. Accepts the canonical /v1/... paths
// and tolerates an extra leading segment (e.g. /openai/v1/chat/completions).
func Detect(path string) (Protocol, bool) {
	p := strings.TrimSuffix(path, "/")
	switch {
	case strings.HasSuffix(p, "/v1/chat/completions"):
		return OpenAIChat, true
	case strings.HasSuffix(p, "/v1/responses"):
		return OpenAIResponses, true
	case strings.HasSuffix(p, "/v1/messages"):
		return Anthropic, true
	}
	return "", false
}

// Usage is the normalized token accounting reported to the control plane.
//
// Semantics (Anthropic-style, matching the control plane's price cards):
//   - Input: uncached prompt tokens (OpenAI prompt_tokens minus cached tokens)
//   - CacheRead / CacheWrite: cache hits / cache writes
//   - Output: all generated tokens including reasoning
//   - Reasoning: subset of Output when the provider breaks it out, else 0
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Found      bool
}

// Total returns input + cache + output.
func (u Usage) Total() int64 { return u.Input + u.CacheRead + u.CacheWrite + u.Output }

// Request is a parsed inbound body with only the fields the gateway needs.
type Request struct {
	Model  string
	Stream bool
	fields map[string]json.RawMessage
}

var ErrMissingModel = errors.New("request body must include a non-empty \"model\" field")

// ParseRequest decodes the top-level object without touching nested values.
func ParseRequest(body []byte) (*Request, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	r := &Request{fields: fields}
	if raw, ok := fields["model"]; ok {
		_ = json.Unmarshal(raw, &r.Model)
	}
	if r.Model == "" {
		return nil, ErrMissingModel
	}
	if raw, ok := fields["stream"]; ok {
		_ = json.Unmarshal(raw, &r.Stream)
	}
	return r, nil
}

// Rewrite returns the body to send upstream: model replaced with providerModel and,
// for streaming OpenAI chat, stream_options.include_usage forced on so the final
// chunk carries usage.
func (r *Request) Rewrite(p Protocol, providerModel string) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(r.fields)+1)
	for k, v := range r.fields {
		out[k] = v
	}
	m, _ := json.Marshal(providerModel)
	out["model"] = m
	if p == OpenAIChat && r.Stream {
		var opts map[string]json.RawMessage
		if raw, ok := out["stream_options"]; ok && len(raw) > 0 && string(raw) != "null" {
			_ = json.Unmarshal(raw, &opts)
		}
		if opts == nil {
			opts = map[string]json.RawMessage{}
		}
		opts["include_usage"] = json.RawMessage("true")
		so, _ := json.Marshal(opts)
		out["stream_options"] = so
	}
	return json.Marshal(out)
}

// ParseUsage extracts usage from a complete (non-streaming) response body.
func ParseUsage(p Protocol, body []byte) Usage {
	switch p {
	case OpenAIChat:
		return parseOpenAIChatUsage(body)
	case OpenAIResponses:
		return parseResponsesBodyUsage(body)
	case Anthropic:
		return parseAnthropicMessageUsage(body)
	}
	return Usage{}
}

// StreamParser accumulates usage from SSE events.
type StreamParser interface {
	// Feed receives one SSE event (event name may be empty) and its data payload.
	Feed(event string, data []byte)
	Usage() Usage
}

// NewStreamParser returns the parser for a protocol.
func NewStreamParser(p Protocol) StreamParser {
	switch p {
	case OpenAIChat:
		return &openAIChatStream{}
	case OpenAIResponses:
		return &responsesStream{}
	case Anthropic:
		return &anthropicStream{}
	}
	return &nopStream{}
}

type nopStream struct{}

func (*nopStream) Feed(string, []byte) {}
func (*nopStream) Usage() Usage        { return Usage{} }

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
