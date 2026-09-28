package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
)

// fakeBedrock emulates the InvokeModel / InvokeModelWithResponseStream wire format.
type fakeBedrock struct {
	t        *testing.T
	gotPath  string
	gotBody  map[string]any
	status   int      // non-zero: answer every call with this status and errBody
	errBody  string   // raw error body (JSON or HTML)
	errType  string   // X-Amzn-Errortype for JSON errors
	chunks   []string // payloads for the streaming operation
	excAfter bool     // send a throttlingException frame after the chunks
}

func (f *fakeBedrock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.gotPath = r.URL.EscapedPath()
	raw, _ := io.ReadAll(r.Body)
	f.gotBody = nil
	_ = json.Unmarshal(raw, &f.gotBody)
	if f.status != 0 {
		if f.errType != "" {
			w.Header().Set("X-Amzn-Errortype", f.errType)
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/html")
		}
		w.Header().Set("X-Amzn-Requestid", "req-fake-err")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.errBody)
		return
	}
	w.Header().Set("X-Amzn-Requestid", "req-fake-ok")
	if !strings.HasSuffix(r.URL.Path, "/invoke-with-response-stream") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":1}}`)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	enc := eventstream.NewEncoder()
	for _, c := range f.chunks {
		p, _ := json.Marshal(map[string][]byte{"bytes": []byte(c)})
		f.encode(enc, w, eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
			{Name: ":event-type", Value: eventstream.StringValue("chunk")},
			{Name: ":content-type", Value: eventstream.StringValue("application/json")},
		}, p)
	}
	if f.excAfter {
		f.encode(enc, w, eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("exception")},
			{Name: ":exception-type", Value: eventstream.StringValue("throttlingException")},
			{Name: ":content-type", Value: eventstream.StringValue("application/json")},
		}, []byte(`{"message":"Too many tokens, please wait"}`))
	}
}

func (f *fakeBedrock) encode(enc *eventstream.Encoder, w http.ResponseWriter, h eventstream.Headers, payload []byte) {
	if err := enc.Encode(w, eventstream.Message{Headers: h, Payload: payload}); err != nil {
		f.t.Fatalf("encode frame: %v", err)
	}
	w.(http.Flusher).Flush()
}

func newTestInvokeProvider(t *testing.T, f *fakeBedrock) (*Provider, *httptest.Server) {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	creds := credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "")
	return &Provider{Code: "bedrock-invoke", invoke: newBedrockInvoker(creds, "us-west-2", u), bedrock: true}, srv
}

func TestAdaptInvokeBody(t *testing.T) {
	hdr := http.Header{}
	hdr.Add("anthropic-beta", "interleaved-thinking-2025-05-14, context-management-2025-06-27")
	out, err := adaptInvokeBody(config.EndpointAnthropic,
		[]byte(`{"model":"global.anthropic.claude-opus-5-5","stream":true,"max_tokens":5,"anthropic_beta":["context-management-2025-06-27"],"messages":[]}`), hdr, true)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if _, ok := m["model"]; ok {
		t.Error("model must be dropped (it travels in the URL)")
	}
	if _, ok := m["stream"]; ok {
		t.Error("stream must be dropped (the operation selects streaming)")
	}
	if m["anthropic_version"] != bedrockAnthropicVersion {
		t.Errorf("anthropic_version = %v", m["anthropic_version"])
	}
	betas, _ := m["anthropic_beta"].([]any)
	if len(betas) != 2 || betas[0] != "context-management-2025-06-27" || betas[1] != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic_beta not merged uniquely: %v", betas)
	}

	out, _ = adaptInvokeBody(config.EndpointOpenAIChat,
		[]byte(`{"model":"global.openai.gpt-5.6-sol","stream":false,"stream_options":{"include_usage":true},"messages":[]}`), nil, false)
	m = nil
	_ = json.Unmarshal(out, &m)
	for _, k := range []string{"model", "stream", "stream_options", "anthropic_version"} {
		if _, ok := m[k]; ok {
			t.Errorf("chat non-stream body still has %q: %s", k, out)
		}
	}
	out, _ = adaptInvokeBody(config.EndpointOpenAIChat, []byte(`{"stream":true,"stream_options":{"include_usage":true},"messages":[]}`), nil, true)
	if !bytes.Contains(out, []byte(`"stream_options"`)) {
		t.Errorf("chat stream body must keep stream_options: %s", out)
	}
}

func TestCanInvoke(t *testing.T) {
	p := &Provider{invoke: &bedrockInvoker{}}
	if !p.CanInvoke(config.EndpointAnthropic) || !p.CanInvoke(config.EndpointOpenAIChat) {
		t.Error("anthropic and openai_chat bodies go through InvokeModel")
	}
	if p.CanInvoke(config.EndpointOpenAIResponses) {
		t.Error("InvokeModel does not accept the Responses format")
	}
	if (&Provider{}).CanInvoke(config.EndpointAnthropic) {
		t.Error("no bedrock_invoke endpoint → no invoke")
	}
}

func TestInvokeNonStreaming(t *testing.T) {
	f := &fakeBedrock{}
	p, _ := newTestInvokeProvider(t, f)
	resp, err := p.Invoke(context.Background(), http.DefaultClient, config.EndpointAnthropic, "global.anthropic.claude-opus-5-5",
		[]byte(`{"model":"global.anthropic.claude-opus-5-5","max_tokens":5,"messages":[]}`), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || !bytes.Contains(body, []byte(`"input_tokens":3`)) {
		t.Fatalf("status %d type %q body %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp.Header.Get("X-Amzn-Requestid") != "req-fake-ok" {
		t.Errorf("request id not carried: %v", resp.Header)
	}
	if f.gotPath != "/model/global.anthropic.claude-opus-5-5/invoke" {
		t.Errorf("path = %s", f.gotPath)
	}
	if f.gotBody["anthropic_version"] != bedrockAnthropicVersion || f.gotBody["model"] != nil {
		t.Errorf("upstream body not adapted: %v", f.gotBody)
	}
}

func TestInvokeStreamingAnthropicSSE(t *testing.T) {
	f := &fakeBedrock{chunks: []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"message_stop"}`,
	}}
	p, _ := newTestInvokeProvider(t, f)
	resp, err := p.Invoke(context.Background(), http.DefaultClient, config.EndpointAnthropic, "global.anthropic.claude-opus-5-5",
		[]byte(`{"stream":true,"max_tokens":5,"messages":[]}`), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("clean stream must end with EOF, got %v", err)
	}
	want := "event: message_start\ndata: " + f.chunks[0] + "\n\n" +
		"event: content_block_delta\ndata: " + f.chunks[1] + "\n\n" +
		"event: message_stop\ndata: " + f.chunks[2] + "\n\n"
	if string(got) != want {
		t.Fatalf("SSE mismatch:\n got %q\nwant %q", got, want)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	if f.gotPath != "/model/global.anthropic.claude-opus-5-5/invoke-with-response-stream" {
		t.Errorf("path = %s", f.gotPath)
	}
}

func TestInvokeStreamingChatSSEEndsWithDone(t *testing.T) {
	f := &fakeBedrock{chunks: []string{
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
	}}
	p, _ := newTestInvokeProvider(t, f)
	resp, err := p.Invoke(context.Background(), http.DefaultClient, config.EndpointOpenAIChat, "global.openai.gpt-5.6-sol",
		[]byte(`{"stream":true,"messages":[]}`), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	want := "data: " + f.chunks[0] + "\n\n" + "data: " + f.chunks[1] + "\n\n" + "data: [DONE]\n\n"
	if string(got) != want {
		t.Fatalf("SSE mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestInvokeStreamExceptionIsErrorEventThenReadError(t *testing.T) {
	f := &fakeBedrock{chunks: []string{`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`}, excAfter: true}
	p, _ := newTestInvokeProvider(t, f)
	resp, err := p.Invoke(context.Background(), http.DefaultClient, config.EndpointAnthropic, "m", []byte(`{"messages":[]}`), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("a stream exception must surface as a read error so the relay marks the stream broken")
	}
	if !bytes.Contains(got, []byte("event: error\n")) || !bytes.Contains(got, []byte(`"type":"rate_limit_error"`)) ||
		!bytes.Contains(got, []byte("Too many tokens")) {
		t.Fatalf("client should get an Anthropic error event before the break, got %q", got)
	}
}

func TestInvokeAPIErrorKeepsStatus(t *testing.T) {
	f := &fakeBedrock{status: 429, errType: "ThrottlingException", errBody: `{"message":"Too many requests, please wait before trying again."}`}
	p, _ := newTestInvokeProvider(t, f)
	resp, err := p.Invoke(context.Background(), http.DefaultClient, config.EndpointOpenAIChat, "m", []byte(`{"messages":[]}`), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 {
		t.Fatalf("429 must stay 429 (failover signal), got %d %s", resp.StatusCode, body)
	}
	var e struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error.Type != "rate_limit_error" || !strings.Contains(e.Error.Message, "ThrottlingException") {
		t.Errorf("OpenAI-shaped error expected, got %s", body)
	}
	if resp.Header.Get("X-Amzn-Requestid") != "req-fake-err" {
		t.Errorf("request id not carried: %v", resp.Header)
	}
}

// A bedrock_invoke endpoint pointed at the wrong host answers 404 with an HTML/XML body. The
// client must get the 404 and the SDK's own error line, the one SDK-based gateways log.
func TestInvokeWrongHostHTML404(t *testing.T) {
	f := &fakeBedrock{status: 404, errBody: "<html><body>Not Found</body></html>"}
	p, _ := newTestInvokeProvider(t, f)
	resp, err := p.Invoke(context.Background(), http.DefaultClient, config.EndpointAnthropic, "m", []byte(`{"messages":[]}`), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 || !bytes.Contains(body, []byte("deserialization failed")) || !bytes.Contains(body, []byte(`"type":"not_found_error"`)) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestInvokeTransportErrorIsError(t *testing.T) {
	f := &fakeBedrock{}
	p, srv := newTestInvokeProvider(t, f)
	srv.Close()
	if _, err := p.Invoke(context.Background(), http.DefaultClient, config.EndpointAnthropic, "m", []byte(`{"messages":[]}`), nil, false); err == nil {
		t.Fatal("connection refused must be an error (transport failover), not a synthesized response")
	}
}
