package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
	"github.com/aws-samples/sample-llm-gateway/internal/metering"
	"github.com/aws-samples/sample-llm-gateway/internal/observability"
	"github.com/aws-samples/sample-llm-gateway/internal/provider"
	"github.com/aws-samples/sample-llm-gateway/internal/router"
)

// fakeCP records key-auth and usage calls.
type fakeCP struct {
	mu      sync.Mutex
	reports []controlplane.UsageReport
	reject  string
}

func (f *fakeCP) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/gateway/key-auth", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-HIGRESS-Token") != "tok" {
			w.WriteHeader(401)
			return
		}
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := map[string]any{"valid": true, "subjectCode": "u@x", "modelCode": req["modelCode"]}
		if f.reject != "" {
			data = map[string]any{"valid": false, "rejectReason": f.reject, "message": "nope"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00000", "data": data})
	})
	mux.HandleFunc("/admin/gateway/usage/report", func(w http.ResponseWriter, r *http.Request) {
		var rep controlplane.UsageReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		f.mu.Lock()
		f.reports = append(f.reports, rep)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00000", "data": map[string]any{"accepted": true}})
	})
	return mux
}

func (f *fakeCP) waitReports(n int) []controlplane.UsageReport {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.reports) >= n {
			out := append([]controlplane.UsageReport(nil), f.reports...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// fakeUpstream speaks Anthropic Messages (stream + non-stream) and OpenAI chat (stream).
type fakeUpstream struct {
	mu       sync.Mutex
	lastBody map[string]any
	lastHdr  http.Header
	fail     int // status to return instead of success, 0 = ok
	bigBody  int // when >0, non-stream /messages returns a 200 body of this many bytes
}

func (u *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		u.mu.Lock()
		u.lastBody, u.lastHdr = m, r.Header.Clone()
		fail := u.fail
		big := u.bigBody
		u.mu.Unlock()
		if fail != 0 {
			w.WriteHeader(fail)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		stream, _ := m["stream"].(bool)
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages") && !stream:
			w.Header().Set("Content-Type", "application/json")
			if big > 0 {
				_, _ = w.Write(bytes.Repeat([]byte("x"), big))
				return
			}
			_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, ev := range []string{
				`event: message_start` + "\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1}}}`,
				`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
				`event: message_delta` + "\n" + `data: {"type":"message_delta","usage":{"output_tokens":9}}`,
				`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
			} {
				_, _ = fmt.Fprint(w, ev+"\n\n")
				fl.Flush()
				time.Sleep(5 * time.Millisecond)
			}
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":4,\"prompt_tokens_details\":{\"cached_tokens\":15},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			w.WriteHeader(404)
		}
	})
}

type harness struct {
	gw  *httptest.Server
	cp  *fakeCP
	up  *fakeUpstream
	up2 *fakeUpstream
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cp := &fakeCP{}
	cpSrv := httptest.NewServer(cp.handler())
	t.Cleanup(cpSrv.Close)
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up.handler())
	t.Cleanup(upSrv.Close)
	up2 := &fakeUpstream{}
	up2Srv := httptest.NewServer(up2.handler())
	t.Cleanup(up2Srv.Close)

	cfg := &config.Config{}
	cfg.Server.RequestTimeout = 10 * time.Second
	cfg.Server.UpstreamConnectTimeout = time.Second
	cfg.Server.UpstreamResponseHeaderTimeout = 5 * time.Second
	cfg.Server.MaxRequestBodyBytes = 1 << 20
	cfg.Server.MaxFailoverAttempts = 3
	cfg.ControlPlane.KeyAuthTimeout = time.Second
	cfg.ControlPlane.TokenHeader = "X-HIGRESS-Token"
	cfg.Providers = map[string]config.ProviderConfig{
		"primary": {Auth: config.AuthXAPIKey, APIKey: "up-key", Endpoints: map[string]string{
			config.EndpointAnthropic: upSrv.URL + "/v1", config.EndpointOpenAIChat: upSrv.URL + "/v1"}},
		"backup": {Auth: config.AuthBearer, APIKey: "b-key", Endpoints: map[string]string{
			config.EndpointAnthropic: up2Srv.URL + "/v1"}},
	}
	reg, err := provider.Build(context.Background(), cfg.Providers)
	if err != nil {
		t.Fatal(err)
	}
	cpc := controlplane.New(cpSrv.URL, "tok", "X-HIGRESS-Token")
	rt := router.New()
	rt.Load(&controlplane.Routes{Version: "v1", Models: []controlplane.ModelRoute{
		{ModelCode: "claude-x", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "primary", ProviderModelCode: "global.anthropic.claude-x", Priority: 1, Weight: 100},
			{ProviderCode: "backup", ProviderModelCode: "backup-claude-x", Priority: 2, Weight: 100},
		}},
		{ModelCode: "gpt-x", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "primary", ProviderModelCode: "gpt-x-real", Priority: 1, Weight: 100},
		}},
	}})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := observability.NewMetrics()
	q := metering.New(cpc, 100, 1, 2, time.Second, log, m)
	q.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		q.Shutdown(ctx)
	})
	h := New(cfg, cpc, rt, reg, q, m, log)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", h.ModelsHandler)
	mux.Handle("/", h)
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)
	return &harness{gw: gw, cp: cp, up: up, up2: up2}
}

func post(t *testing.T, url, key, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestAnthropicNonStreamRewriteAndReport(t *testing.T) {
	h := newHarness(t)
	resp, body := post(t, h.gw.URL+"/v1/messages", "sk-client", `{"model":"claude-x","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u1"}}`, map[string]string{"anthropic-beta": "foo-2026"})
	if resp.StatusCode != 200 || !strings.Contains(body, `"text":"hi"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("missing X-Request-Id")
	}
	h.up.mu.Lock()
	if h.up.lastBody["model"] != "global.anthropic.claude-x" {
		t.Errorf("model not rewritten: %v", h.up.lastBody["model"])
	}
	if _, ok := h.up.lastBody["stream_options"]; ok {
		t.Error("stream_options must not be injected for anthropic")
	}
	if h.up.lastBody["metadata"].(map[string]any)["user_id"] != "u1" {
		t.Error("nested field lost")
	}
	if h.up.lastHdr.Get("x-api-key") != "up-key" || h.up.lastHdr.Get("Authorization") != "" {
		t.Errorf("upstream auth wrong: %v", h.up.lastHdr)
	}
	if h.up.lastHdr.Get("anthropic-version") == "" || h.up.lastHdr.Get("anthropic-beta") != "foo-2026" {
		t.Errorf("anthropic headers wrong: %v", h.up.lastHdr)
	}
	h.up.mu.Unlock()
	reps := h.cp.waitReports(1)
	if len(reps) != 1 {
		t.Fatal("no usage report")
	}
	r := reps[0]
	if r.ModelCode != "claude-x" || r.ProviderModelCode != "global.anthropic.claude-x" || r.APIKey != "sk-client" ||
		r.StatusCode != 200 || r.InputTokens != 11 || r.OutputTokens != 7 || r.CacheReadTokens != 3 || r.CacheWriteTokens != 2 {
		t.Errorf("bad report: %+v", r)
	}
	if !strings.HasPrefix(r.RequestID, "req_") || r.StartTime == "" {
		t.Errorf("bad ids: %+v", r)
	}
}

func TestAnthropicStreamPassthroughUsage(t *testing.T) {
	h := newHarness(t)
	resp, body := post(t, h.gw.URL+"/v1/messages", "sk-client", `{"model":"claude-x","stream":true,"max_tokens":5,"messages":[]}`, nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d ct %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if strings.Count(body, "event:") != 4 || !strings.Contains(body, `"text":"hi"`) {
		t.Errorf("stream altered: %q", body)
	}
	reps := h.cp.waitReports(1)
	if len(reps) != 1 || reps[0].InputTokens != 11 || reps[0].OutputTokens != 9 {
		t.Errorf("bad stream report: %+v", reps)
	}
}

func TestOpenAIChatStreamInjectsUsageOption(t *testing.T) {
	h := newHarness(t)
	resp, body := post(t, h.gw.URL+"/v1/chat/completions", "sk-client", `{"model":"gpt-x","stream":true,"messages":[]}`, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	h.up.mu.Lock()
	so, _ := h.up.lastBody["stream_options"].(map[string]any)
	h.up.mu.Unlock()
	if so["include_usage"] != true {
		t.Errorf("include_usage not injected: %v", so)
	}
	reps := h.cp.waitReports(1)
	// prompt 20 with 15 cached -> input 5; completion 4 incl. reasoning 2
	if len(reps) != 1 || reps[0].InputTokens != 5 || reps[0].CacheReadTokens != 15 || reps[0].OutputTokens != 4 || reps[0].ReasoningTokens != 2 {
		t.Errorf("bad openai report: %+v", reps)
	}
}

func TestFailoverOn5xxBeforeFirstByte(t *testing.T) {
	h := newHarness(t)
	h.up.mu.Lock()
	h.up.fail = 503
	h.up.mu.Unlock()
	resp, body := post(t, h.gw.URL+"/v1/messages", "sk-client", `{"model":"claude-x","max_tokens":5,"messages":[]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected failover success, got %d %s", resp.StatusCode, body)
	}
	h.up2.mu.Lock()
	if h.up2.lastBody["model"] != "backup-claude-x" || h.up2.lastHdr.Get("Authorization") != "Bearer b-key" {
		t.Errorf("backup not used correctly: %v %v", h.up2.lastBody, h.up2.lastHdr)
	}
	h.up2.mu.Unlock()
	reps := h.cp.waitReports(1)
	if len(reps) != 1 || reps[0].ProviderModelCode != "backup-claude-x" {
		t.Errorf("report should name the provider that served: %+v", reps)
	}
}

func TestNonStreamOversizedBodyRejected(t *testing.T) {
	old := maxResponseBody
	maxResponseBody = 1024
	defer func() { maxResponseBody = old }()

	h := newHarness(t)
	h.up.mu.Lock()
	h.up.bigBody = 4096 // exceeds the lowered cap
	h.up.mu.Unlock()

	resp, body := post(t, h.gw.URL+"/v1/messages", "sk-client", `{"model":"claude-x","max_tokens":5,"messages":[]}`, nil)
	if resp.StatusCode != 502 {
		t.Fatalf("oversized body should be rejected with 502, got %d %s", resp.StatusCode, body)
	}
	// Anthropic wire format carries only type+message (code is OpenAI-only, and appears in logs).
	if !strings.Contains(body, "exceeds gateway limit") {
		t.Errorf("expected size-limit error message, got %s", body)
	}
	reps := h.cp.waitReports(1)
	if len(reps) != 1 || reps[0].StatusCode != 502 || reps[0].InputTokens != 0 {
		t.Errorf("truncated response should be metered as 502 with 0 tokens: %+v", reps)
	}
}

func TestKeyAuthRejectFormats(t *testing.T) {
	h := newHarness(t)
	h.cp.reject = "RPM_EXCEEDED"
	resp, body := post(t, h.gw.URL+"/v1/messages", "sk-client", `{"model":"claude-x","messages":[]}`, nil)
	if resp.StatusCode != 429 || !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "rate_limit_error") {
		t.Errorf("anthropic reject: %d %s", resp.StatusCode, body)
	}
	h.cp.reject = "KEY_NOT_FOUND"
	resp, body = post(t, h.gw.URL+"/v1/chat/completions", "sk-client", `{"model":"gpt-x","messages":[]}`, nil)
	if resp.StatusCode != 401 || !strings.Contains(body, `"code":"invalid_api_key"`) {
		t.Errorf("openai reject: %d %s", resp.StatusCode, body)
	}
	if reps := h.cp.waitReports(1); reps != nil {
		t.Errorf("rejections must not be metered: %+v", reps)
	}
}

func TestMissingKeyAndModelAndUnknownRoute(t *testing.T) {
	h := newHarness(t)
	resp, _ := post(t, h.gw.URL+"/v1/messages", "", `{"model":"claude-x"}`, nil)
	if resp.StatusCode != 401 {
		t.Errorf("missing key: %d", resp.StatusCode)
	}
	resp, _ = post(t, h.gw.URL+"/v1/messages", "k", `{"messages":[]}`, nil)
	if resp.StatusCode != 400 {
		t.Errorf("missing model: %d", resp.StatusCode)
	}
	resp, _ = post(t, h.gw.URL+"/v1/messages", "k", `{"model":"nope"}`, nil)
	if resp.StatusCode != 404 {
		t.Errorf("unknown route: %d", resp.StatusCode)
	}
	// gpt-x is only routed to a provider without a responses endpoint -> 502 (no attempt possible)
	resp, _ = post(t, h.gw.URL+"/v1/responses", "k", `{"model":"gpt-x","input":"x"}`, nil)
	if resp.StatusCode != 502 {
		t.Errorf("protocol not served: %d", resp.StatusCode)
	}
	r, _ := http.Get(h.gw.URL + "/v1/models")
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), `"id":"claude-x"`) || !strings.Contains(string(b), `"id":"gpt-x"`) {
		t.Errorf("models list: %s", b)
	}
}
