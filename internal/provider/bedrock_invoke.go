package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
)

// bedrockAnthropicVersion is the only anthropic_version the InvokeModel API accepts for Claude.
// It goes in the body; the Messages-API anthropic-version header has no meaning here.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// bedrockInvoker calls Amazon Bedrock through the SDK InvokeModel / InvokeModelWithResponseStream
// API instead of the native protocol endpoints (/anthropic/v1, /openai/v1). It exists so the
// gateway can run the same call path as SDK-based gateways: the body is still the target
// protocol's native JSON (Anthropic Messages for Claude, OpenAI Chat Completions for GPT), only
// the envelope differs. Invoke returns an *http.Response shaped like the native endpoint's, so
// failover, relay, translation and metering upstream of it are unchanged.
type bedrockInvoker struct {
	client *bedrockruntime.Client
}

// HTTPDoer is the HTTP client the SDK sends through (the gateway's upstream client, so the
// connect / response-header timeouts and the no-redirect policy apply to this path too).
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func newBedrockInvoker(creds aws.CredentialsProvider, region string, endpoint *url.URL) *bedrockInvoker {
	return &bedrockInvoker{client: bedrockruntime.New(bedrockruntime.Options{
		Region:       region,
		Credentials:  creds,
		BaseEndpoint: aws.String(endpoint.String()),
		// One attempt: 429 / 5xx are the gateway's failover signals, the SDK must not retry them first.
		Retryer: aws.NopRetryer{},
	})}
}

// CanInvoke reports whether requests in protocol can go through InvokeModel on this provider:
// it has a bedrock_invoke endpoint and the protocol is one InvokeModel carries. The OpenAI
// Responses format is not accepted by InvokeModel; route it with providerProtocol=openai_chat.
func (p *Provider) CanInvoke(protocol string) bool {
	return p.invoke != nil && (protocol == config.EndpointAnthropic || protocol == config.EndpointOpenAIChat)
}

// Invoke sends body (already in protocol's native wire format, model already rewritten) through
// InvokeModel, or InvokeModelWithResponseStream when stream is set. hdr supplies the client
// headers that survive into the body (anthropic-beta). The returned response is JSON for
// non-streaming calls and a text/event-stream SSE body for streaming ones; Bedrock API errors
// come back as a response with Bedrock's status code and a protocol-shaped error body. A non-nil
// error means no HTTP response was obtained (transport failure, credentials, context).
func (p *Provider) Invoke(ctx context.Context, hc HTTPDoer, protocol, modelID string, body []byte, hdr http.Header, stream bool) (*http.Response, error) {
	in, err := adaptInvokeBody(protocol, body, hdr, stream)
	if err != nil {
		return nil, err
	}
	withClient := func(o *bedrockruntime.Options) { o.HTTPClient = hc }
	if !stream {
		out, err := p.invoke.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
			ModelId: aws.String(modelID), Body: in,
			ContentType: aws.String("application/json"), Accept: aws.String("application/json"),
		}, withClient)
		if err != nil {
			return invokeErrorResponse(protocol, err)
		}
		h := http.Header{"Content-Type": {"application/json"}}
		if id, ok := awsmiddleware.GetRequestIDMetadata(out.ResultMetadata); ok {
			h.Set("X-Amzn-Requestid", id)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(bytes.NewReader(out.Body)),
			ContentLength: int64(len(out.Body))}, nil
	}

	out, err := p.invoke.client.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId: aws.String(modelID), Body: in, ContentType: aws.String("application/json"),
	}, withClient)
	if err != nil {
		return invokeErrorResponse(protocol, err)
	}
	h := http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}
	if id, ok := awsmiddleware.GetRequestIDMetadata(out.ResultMetadata); ok {
		h.Set("X-Amzn-Requestid", id)
	}
	es := out.GetStream()
	pr, pw := io.Pipe()
	go pumpInvokeStream(protocol, es, pw)
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: &invokeStreamBody{pr: pr, es: es}}, nil
}

// adaptInvokeBody turns a native-endpoint body into an InvokeModel body. InvokeModel picks
// streaming by operation and rejects "stream": true on the non-streaming one; the model travels
// in the URL and a body "model" must otherwise be a valid Bedrock model id, so both are dropped.
// Claude additionally needs anthropic_version in the body, and takes beta flags as anthropic_beta.
func adaptInvokeBody(protocol string, body []byte, hdr http.Header, stream bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("bedrock invoke: request body is not a JSON object: %w", err)
	}
	delete(m, "model")
	delete(m, "stream")
	switch protocol {
	case config.EndpointAnthropic:
		m["anthropic_version"] = json.RawMessage(strconv.Quote(bedrockAnthropicVersion))
		if betas := headerList(hdr, "anthropic-beta"); len(betas) > 0 {
			var existing []string
			if raw, ok := m["anthropic_beta"]; ok {
				_ = json.Unmarshal(raw, &existing)
			}
			b, _ := json.Marshal(mergeUnique(existing, betas))
			m["anthropic_beta"] = b
		}
	case config.EndpointOpenAIChat:
		if !stream {
			delete(m, "stream_options") // only valid alongside streaming
		}
	}
	return json.Marshal(m)
}

// pumpInvokeStream rewrites each InvokeModelWithResponseStream chunk as one SSE event in the
// native endpoint's framing: Anthropic events are named by their "type"; OpenAI Chat chunks are
// bare data lines ended by [DONE]. A stream exception is surfaced to the client as the protocol's
// error event and then as a read error, so the relay records the stream as broken (not billed).
func pumpInvokeStream(protocol string, es *bedrockruntime.InvokeModelWithResponseStreamEventStream, pw *io.PipeWriter) {
	defer es.Close()
	for ev := range es.Events() {
		ch, ok := ev.(*types.ResponseStreamMemberChunk)
		if !ok {
			continue
		}
		if _, err := pw.Write(sseFrame(protocol, ch.Value.Bytes)); err != nil {
			return // reader closed: client gone or relay done
		}
	}
	if err := es.Err(); err != nil {
		_, _ = pw.Write(sseFrame(protocol, errorBody(protocol, streamErrorStatus(err), err.Error())))
		pw.CloseWithError(fmt.Errorf("bedrock invoke stream: %w", err))
		return
	}
	if protocol == config.EndpointOpenAIChat {
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
	}
	pw.Close()
}

func sseFrame(protocol string, data []byte) []byte {
	var b bytes.Buffer
	if protocol == config.EndpointAnthropic {
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &ev) == nil && ev.Type != "" {
			b.WriteString("event: " + ev.Type + "\n")
		}
	}
	b.WriteString("data: ")
	b.Write(bytes.TrimSpace(data))
	b.WriteString("\n\n")
	return b.Bytes()
}

// invokeStreamBody closes the event stream together with the pipe, so a client that goes away
// mid-stream releases the upstream connection instead of leaving the pump blocked.
type invokeStreamBody struct {
	pr *io.PipeReader
	es *bedrockruntime.InvokeModelWithResponseStreamEventStream
}

func (b *invokeStreamBody) Read(p []byte) (int, error) { return b.pr.Read(p) }
func (b *invokeStreamBody) Close() error {
	_ = b.pr.Close()
	return b.es.Close()
}

// invokeErrorResponse maps an SDK error to what the native endpoint would have answered: an HTTP
// response carrying Bedrock's status code, so 429 / 5xx still trigger failover and 4xx reach the
// client. The SDK error text is kept verbatim in the message (it names the Bedrock exception and
// request id, and for a wrong endpoint it is the "deserialization failed" line SDK gateways log).
func invokeErrorResponse(protocol string, err error) (*http.Response, error) {
	// The SDK also wraps "request send failed" (dial / TLS / context) in a ResponseError, with
	// status 0 and no HTTP response behind it: that is a transport failure, not an answer.
	var re *smithyhttp.ResponseError
	if !errors.As(err, &re) || re.Response == nil || re.Response.Response == nil || re.HTTPStatusCode() == 0 {
		return nil, err
	}
	status := re.HTTPStatusCode()
	msg := err.Error()
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorMessage() != "" {
		msg = ae.ErrorCode() + ": " + ae.ErrorMessage()
	}
	body := errorBody(protocol, status, msg)
	h := http.Header{"Content-Type": {"application/json"}}
	if id := re.Response.Header.Get("X-Amzn-Requestid"); id != "" {
		h.Set("X-Amzn-Requestid", id)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, nil
}

// streamErrorStatus gives a mid-stream exception the status the equivalent API error would carry.
func streamErrorStatus(err error) int {
	var (
		thr *types.ThrottlingException
		una *types.ServiceUnavailableException
		val *types.ValidationException
		tmo *types.ModelTimeoutException
	)
	switch {
	case errors.As(err, &thr):
		return http.StatusTooManyRequests
	case errors.As(err, &una):
		return http.StatusServiceUnavailable
	case errors.As(err, &val):
		return http.StatusBadRequest
	case errors.As(err, &tmo):
		return http.StatusRequestTimeout
	}
	return http.StatusInternalServerError
}

// errorBody renders an error in the protocol's own error shape.
func errorBody(protocol string, status int, msg string) []byte {
	typ := errorType(status)
	if protocol == config.EndpointAnthropic {
		b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
		return b
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": typ, "message": msg, "code": strconv.Itoa(status)}})
	return b
}

func errorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	}
	return "invalid_request_error"
}

// headerList splits comma-separated values across all instances of a header.
func headerList(h http.Header, name string) []string {
	var out []string
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// mergeUnique appends b to a, dropping duplicates and keeping first-seen order. No capacity
// hints: a comes from the request body, so its length is not something to size allocations by.
func mergeUnique(a, b []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}
