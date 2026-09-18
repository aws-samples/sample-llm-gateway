package translate

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
)

func TestSupportedMatrix(t *testing.T) {
	all := []protocol.Protocol{protocol.Anthropic, protocol.OpenAIChat, protocol.OpenAIResponses}
	for _, from := range all {
		for _, to := range all {
			got := Supported(from, to)
			want := from != to && (from == protocol.Anthropic || to == protocol.Anthropic) // Chat↔Responses lands in M8
			if got != want {
				t.Errorf("Supported(%s,%s)=%v want %v", from, to, got, want)
			}
		}
	}
	if Supported("grpc", protocol.Anthropic) || Supported(protocol.Anthropic, "") {
		t.Error("invalid protocols must not be supported")
	}
	if _, err := New(protocol.OpenAIChat, protocol.OpenAIResponses, Options{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("New for unsupported direction: %v", err)
	}
}

func TestTranslatorRequestReportsDefaultedMaxTokens(t *testing.T) {
	tr, err := New(protocol.OpenAIResponses, protocol.Anthropic, Options{DefaultMaxTokens: 1234})
	if err != nil {
		t.Fatal(err)
	}
	out, defaulted, err := tr.Request([]byte(`{"model":"m","input":"hi"}`), "claude")
	if err != nil || !defaulted || !strings.Contains(string(out), `"max_tokens":1234`) {
		t.Errorf("defaulted=%v err=%v out=%s", defaulted, err, out)
	}
	out, defaulted, err = tr.Request([]byte(`{"model":"m","input":"hi","max_output_tokens":50}`), "claude")
	if err != nil || defaulted || !strings.Contains(string(out), `"max_tokens":50`) {
		t.Errorf("explicit max_output_tokens: defaulted=%v err=%v out=%s", defaulted, err, out)
	}
	// Target that does not require max_tokens never reports a default.
	tr2, _ := New(protocol.Anthropic, protocol.OpenAIChat, Options{DefaultMaxTokens: 1234})
	if _, defaulted, err := tr2.Request([]byte(`{"model":"m","max_tokens":9,"messages":[{"role":"user","content":"hi"}]}`), "gpt"); err != nil || defaulted {
		t.Errorf("chat target: defaulted=%v err=%v", defaulted, err)
	}
}

func TestTranslatorErrorShapes(t *testing.T) {
	tr, _ := New(protocol.Anthropic, protocol.OpenAIChat, Options{})
	cases := []struct {
		body              string
		wantType, wantMsg string
		wantCode          string
	}{
		// OpenAI shape
		{`{"error":{"message":"bad tool","type":"invalid_request_error","param":"tools","code":"invalid_value"}}`, "invalid_request_error", "bad tool", "invalid_value"},
		// Anthropic shape
		{`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, "overloaded_error", "Overloaded", "upstream_error"},
		// Bedrock bare message
		{`{"message":"The provided model identifier is invalid."}`, "api_error", "The provided model identifier is invalid.", "upstream_error"},
		// fake / string error
		{`{"error":"boom"}`, "api_error", "boom", "upstream_error"},
		// not JSON at all → trimmed text
		{`  <html>502 Bad Gateway</html>  `, "api_error", "<html>502 Bad Gateway</html>", "upstream_error"},
		// empty → status text
		{``, "api_error", "upstream returned status 503", "upstream_error"},
	}
	for _, c := range cases {
		ge := tr.Error(503, []byte(c.body))
		if ge.Status != 503 || ge.Type != c.wantType || ge.Message != c.wantMsg || ge.Code != c.wantCode {
			t.Errorf("Error(%q) = %+v", c.body, ge)
		}
	}
}

func TestStreamFailClosesHalfOpenMessage(t *testing.T) {
	tr, _ := New(protocol.Anthropic, protocol.OpenAIChat, Options{})
	st := tr.NewStream()
	out, err := st.Feed("", []byte(`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","content":"par"},"finish_reason":null}]}`))
	if err != nil || !strings.Contains(string(out), "event: message_start") || !strings.Contains(string(out), `"text":"par"`) {
		t.Fatalf("feed: %v %s", err, out)
	}
	if st.Ended() {
		t.Fatal("must not be ended before message_stop")
	}
	b := st.Fail("upstream died")
	if !strings.Contains(string(b), "event: content_block_stop") || !strings.Contains(string(b), "event: error") || !strings.Contains(string(b), "upstream died") {
		t.Errorf("Fail must close the open block and emit an error event, got %s", b)
	}
	if !st.Ended() || st.Fail("again") != nil {
		t.Error("Fail is terminal and idempotent")
	}
	if out, err := st.Feed("", []byte(`{"choices":[{"index":0,"delta":{"content":"late"}}]}`)); err != nil || out != nil {
		t.Errorf("events after the end must be swallowed: %v %s", err, out)
	}
}

func TestStreamEndedAfterMessageStop(t *testing.T) {
	tr, _ := New(protocol.OpenAIResponses, protocol.Anthropic, Options{})
	st := tr.NewStream()
	events := []struct{ ev, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"m1","model":"claude","usage":{"input_tokens":3,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
	var all []byte
	for _, e := range events {
		out, err := st.Feed(e.ev, []byte(e.data))
		if err != nil {
			t.Fatalf("%s: %v", e.ev, err)
		}
		all = append(all, out...)
	}
	if !st.Ended() || st.Usage().Output != 2 || st.Usage().Input != 3 {
		t.Errorf("ended=%v usage=%+v", st.Ended(), st.Usage())
	}
	if !strings.Contains(string(all), `"type":"response.completed"`) || st.Fail("x") != nil {
		t.Errorf("terminal event missing or Fail not idempotent:\n%s", all)
	}
}

// Regression (found on a real Claude Code → GPT run): the usage the client sees must be the
// same split metering bills. Bedrock's Chat usage carries the non-standard
// prompt_tokens_details.cache_write_tokens; the decoder used to drop it, so the client saw
// input_tokens=14331/cache_write=0 while the account said input=2/cache_write=14329.
func TestChatStreamUsageMatchesMeteringSplit(t *testing.T) {
	tr, _ := New(protocol.Anthropic, protocol.OpenAIChat, Options{})
	st := tr.NewStream()
	chunks := []string{
		`{"choices":[{"delta":{"role":"assistant","content":"5"},"finish_reason":null,"index":0}],"id":"chatcmpl-1","model":"us.openai.gpt-5.6-sol","object":"chat.completion.chunk","usage":null}`,
		`{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"id":"chatcmpl-1","model":"us.openai.gpt-5.6-sol","object":"chat.completion.chunk","usage":null}`,
		`{"choices":[],"id":"chatcmpl-1","model":"us.openai.gpt-5.6-sol","object":"chat.completion.chunk","usage":{"completion_tokens":29,"completion_tokens_details":{"accepted_prediction_tokens":0,"audio_tokens":0,"reasoning_tokens":0,"rejected_prediction_tokens":0},"prompt_tokens":14331,"prompt_tokens_details":{"audio_tokens":0,"cache_write_tokens":14329,"cached_tokens":0},"total_tokens":14360}}`,
		`[DONE]`,
	}
	var all []byte
	for _, c := range chunks {
		out, err := st.Feed("", []byte(c))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, out...)
	}
	metering := protocol.ParseUsage(protocol.OpenAIChat, []byte(`{"usage":{"prompt_tokens":14331,"completion_tokens":29,"prompt_tokens_details":{"cache_write_tokens":14329,"cached_tokens":0}}}`))
	if got := st.Usage(); got.Input != metering.Input || got.CacheWrite != metering.CacheWrite || got.Output != metering.Output {
		t.Errorf("client usage %+v != metering usage %+v", got, metering)
	}
	if !strings.Contains(string(all), `"input_tokens":2,"output_tokens":29,"cache_read_input_tokens":0,"cache_creation_input_tokens":14329`) {
		t.Errorf("message_delta usage must carry the metering split:\n%s", all)
	}
}
