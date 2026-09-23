package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
)

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
	// TargetIsBedrock enables Amazon Bedrock-only request quirks in the target codec. Today: an
	// OpenAI Chat target carrying function tools gets reasoning_effort="none" (Bedrock's
	// /chat/completions rejects tools otherwise). Left false for direct OpenAI and third-party
	// OpenAI-compatible endpoints, which may reject or misinterpret that field.
	TargetIsBedrock bool
}

// DefaultMaxTokens is the shipped default for Options.DefaultMaxTokens (see the field doc).
const DefaultMaxTokens int64 = 8192

// Supported reports whether translation from one protocol to another is implemented.
// Same-protocol pairs are pass-through and never go through this package. The proxy handler
// treats a false result as "not implemented" and skips the candidate (failover continues).
//
// All six cross-protocol directions between Anthropic Messages, OpenAI Chat Completions and
// OpenAI Responses are implemented. The function stays as the single switch so a direction can
// be pulled if a codec regresses.
func Supported(from, to protocol.Protocol) bool {
	if from == to || !from.Valid() || !to.Valid() {
		return false
	}
	return requestCodec(from) != nil && requestCodec(to) != nil
}

// ErrUnsupported is returned by New for a direction Supported() rejects.
var ErrUnsupported = errors.New("protocol translation not supported")

// ---- codec lookup -------------------------------------------------------------------------

func requestCodec(p protocol.Protocol) RequestCodec {
	switch p {
	case protocol.Anthropic:
		return anthropicRequest{}
	case protocol.OpenAIChat:
		return openAIChatRequest{}
	case protocol.OpenAIResponses:
		return openAIResponsesRequest{}
	}
	return nil
}

func responseCodec(p protocol.Protocol) ResponseCodec {
	switch p {
	case protocol.Anthropic:
		return anthropicResponse{}
	case protocol.OpenAIChat:
		return openAIChatResponse{}
	case protocol.OpenAIResponses:
		return openAIResponsesResponse{}
	}
	return nil
}

func newStreamDecoder(p protocol.Protocol) StreamDecoder {
	switch p {
	case protocol.Anthropic:
		return newAnthropicStreamIn()
	case protocol.OpenAIChat:
		return newChatStreamIn()
	case protocol.OpenAIResponses:
		return newResponsesStreamIn()
	}
	return nil
}

func newStreamEncoder(p protocol.Protocol) StreamEncoder {
	switch p {
	case protocol.Anthropic:
		return newAnthropicStreamOut()
	case protocol.OpenAIChat:
		return newChatStreamOut()
	case protocol.OpenAIResponses:
		return newResponsesStreamOut()
	}
	return nil
}

// ---- Translator: the handler-facing façade ------------------------------------------------

// Translator converts one exchange between an inbound protocol (what the client speaks) and a
// target protocol (what the provider speaks). It is stateless and safe to share; per-response
// stream state lives in the Stream returned by NewStream.
type Translator struct {
	From, To protocol.Protocol
	opts     Options
}

// New returns a Translator for the direction, or ErrUnsupported.
func New(from, to protocol.Protocol, opts Options) (*Translator, error) {
	if !Supported(from, to) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrUnsupported, from, to)
	}
	return &Translator{From: from, To: to, opts: opts}, nil
}

// Request converts an inbound request body into the target protocol with providerModel on the
// wire. defaulted reports that Options.DefaultMaxTokens was written because the source omitted
// max_tokens and the target requires it; callers surface this in metrics so a truncated answer
// (stop_reason max_tokens) can be traced back to the gateway default.
func (t *Translator) Request(body []byte, providerModel string) (out []byte, defaulted bool, err error) {
	ir, err := requestCodec(t.From).ToIR(body)
	if err != nil {
		return nil, false, err
	}
	defaulted = t.To == protocol.Anthropic && (ir.MaxTokens == nil || *ir.MaxTokens <= 0) && t.opts.DefaultMaxTokens > 0
	out, err = requestCodec(t.To).FromIR(ir, providerModel, t.opts)
	if err != nil {
		return nil, false, err
	}
	return out, defaulted, nil
}

// Response converts a successful (2xx) non-streaming target-protocol body into the inbound
// protocol. Usage is carried through the IR so the client sees the provider's real counts.
func (t *Translator) Response(body []byte) ([]byte, error) {
	ir, err := responseCodec(t.To).ToIR(body)
	if err != nil {
		return nil, err
	}
	return responseCodec(t.From).FromIR(ir)
}

// Error maps a non-2xx target-protocol body to a GatewayError so the handler can render it in
// the inbound wire format (the raw body would be the wrong shape for the client). Both OpenAI
// ({"error":{message,type,code}}) and Anthropic ({"type":"error","error":{type,message}}) shapes
// are understood, plus Bedrock's bare {"message"}; anything else becomes an api_error whose
// message is the (trimmed) upstream text.
func (t *Translator) Error(status int, body []byte) *protocol.GatewayError {
	ge := &protocol.GatewayError{Status: status, Type: "api_error", Code: "upstream_error"}
	var w struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &w) == nil {
		if len(w.Error) > 0 && w.Error[0] == '{' {
			var e struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(w.Error, &e) == nil {
				if e.Type != "" {
					ge.Type = e.Type
				}
				if e.Code != "" {
					ge.Code = e.Code
				}
				ge.Message = e.Message
			}
		} else if len(w.Error) > 0 && w.Error[0] == '"' {
			_ = json.Unmarshal(w.Error, &ge.Message)
		}
		if ge.Message == "" {
			ge.Message = w.Message
		}
	}
	if ge.Message == "" {
		s := strings.TrimSpace(string(body))
		if len(s) > 512 {
			s = s[:512] + "…"
		}
		if s == "" {
			s = fmt.Sprintf("upstream returned status %d", status)
		}
		ge.Message = s
	}
	return ge
}

// NewStream returns a fresh per-response stream translator.
func (t *Translator) NewStream() *Stream {
	return &Stream{dec: newStreamDecoder(t.To), enc: newStreamEncoder(t.From)}
}

// Stream translates one SSE response: target-protocol events in, inbound-protocol bytes out.
type Stream struct {
	dec   StreamDecoder
	enc   StreamEncoder
	ended bool // a MessageStop (or Error) has been emitted to the client
	usage protocol.Usage
}

// Feed takes one upstream SSE event and returns the framed bytes to write to the client (may be
// empty). After an error the caller should stop feeding and call Fail.
func (s *Stream) Feed(event string, data []byte) ([]byte, error) {
	if s.ended {
		return nil, nil
	}
	evs, err := s.dec.ToIR(event, data)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, ev := range evs {
		b, err := s.enc.FromIR(ev)
		if err != nil {
			return out, err
		}
		out = append(out, b...)
		switch ev.Kind {
		case EventMessageStop:
			s.ended, s.usage = true, ev.Usage
		case EventError:
			s.ended = true
		}
	}
	return out, nil
}

// Ended reports whether the client-side message has been closed (message_stop or error seen).
func (s *Stream) Ended() bool { return s.ended }

// Usage is the usage carried by the MessageStop event that was rendered to the client (zero
// until then). Metering should NOT rely on this — it parses the raw upstream stream with the
// target protocol's StreamParser, exactly like pass-through — this exists for tests/logging.
func (s *Stream) Usage() protocol.Usage { return s.usage }

// Fail renders an error event in the inbound protocol so the client does not see a half-open
// message when translation (or the upstream) breaks mid-stream. Returns nil once ended.
func (s *Stream) Fail(msg string) []byte {
	if s.ended {
		return nil
	}
	s.ended = true
	b, err := s.enc.FromIR(StreamEvent{Kind: EventError, Err: msg})
	if err != nil {
		return nil
	}
	return b
}
