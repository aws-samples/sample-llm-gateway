package translate

import "github.com/aws-samples/sample-llm-gateway/internal/protocol"

// RequestCodec converts a protocol's request body to and from the IR.
type RequestCodec interface {
	// ToIR parses a request body in this protocol into the IR.
	ToIR(body []byte) (*Request, error)
	// FromIR renders an IR request into this protocol's body. providerModel is the model id to
	// put on the wire; opts carries conversion knobs (e.g. default max_tokens).
	FromIR(r *Request, providerModel string, opts Options) ([]byte, error)
}

// ResponseCodec converts a protocol's non-streaming response body to and from the IR.
type ResponseCodec interface {
	ToIR(body []byte) (*Response, error)
	FromIR(*Response) ([]byte, error)
}

// StreamDecoder turns one source protocol's SSE events into canonical events. It is stateful
// (tracks open blocks / tool calls across events), so a fresh instance is created per response.
// ToIR is fed one SSE event at a time (event name may be empty) and returns zero or more
// canonical events.
type StreamDecoder interface {
	ToIR(event string, data []byte) ([]StreamEvent, error)
}

// StreamEncoder renders canonical events into the target protocol's SSE bytes (already framed:
// "event: ...\ndata: ...\n\n" for named-event protocols, "data: ...\n\n" for OpenAI). Stateful;
// one instance per response.
type StreamEncoder interface {
	FromIR(StreamEvent) ([]byte, error)
}

// Options carries conversion knobs decided by config/policy.
type Options struct {
	// DefaultMaxTokens is used when the target protocol requires max_tokens (Anthropic) but the
	// source omitted it (Request.MaxTokens == nil). 0 means "no default" (leave unset and let the
	// upstream reject). Agentic coding clients emit large diffs, so the shipped default is 8192,
	// not the 4096 many proxies copy from LiteLLM — that value silently truncates real work with
	// a normal-looking stop_reason: max_tokens.
	DefaultMaxTokens int64
}

// DefaultMaxTokens is the shipped default for Options.DefaultMaxTokens (see the field doc).
const DefaultMaxTokens int64 = 8192

// Supported reports whether translation from one protocol to another is implemented.
// Same-protocol pairs are pass-through and never go through this package.
//
// STATUS: skeleton — no codecs are wired yet, so this returns false for every cross-protocol
// pair. The proxy handler treats a false result as "not implemented" and rejects the candidate.
// Later milestones register codecs and flip this on per direction.
func Supported(from, to protocol.Protocol) bool {
	if from == to || !from.Valid() || !to.Valid() {
		return false
	}
	return false
}
