// Package proxy implements the request pipeline:
// detect protocol -> extract key/model -> key-auth -> route -> rewrite -> forward
// -> relay (tee usage) -> async metering.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
	"github.com/aws-samples/sample-llm-gateway/internal/metering"
	"github.com/aws-samples/sample-llm-gateway/internal/observability"
	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
	"github.com/aws-samples/sample-llm-gateway/internal/provider"
	"github.com/aws-samples/sample-llm-gateway/internal/router"
)

const (
	userAgent            = "sample-llm-gateway/0.1"
	defaultAnthropicVers = "2023-06-01"
)

// maxResponseBody caps a buffered (non-streaming) upstream response. A body larger than this
// is rejected with 502 rather than silently truncated (a truncated body relayed with the
// upstream 2xx would look successful but be corrupt). A var, not const, so tests can lower it.
var maxResponseBody int64 = 64 << 20

// Headers copied from the client request to the upstream request (lower-case).
var forwardRequestHeaders = map[string]bool{
	"accept":            true,
	"anthropic-version": true,
	"anthropic-beta":    true,
	"openai-beta":       true,
}

// Headers copied from the upstream response to the client response (canonical form).
var forwardResponseHeaders = []string{
	"Content-Type", "Cache-Control",
}

type Handler struct {
	cfg       *config.Config
	cp        *controlplane.Client
	router    *router.Router
	providers provider.Registry
	meter     *metering.Queue
	metrics   *observability.Metrics
	client    *http.Client
	log       *slog.Logger
}

func New(cfg *config.Config, cp *controlplane.Client, r *router.Router, reg provider.Registry,
	meter *metering.Queue, m *observability.Metrics, log *slog.Logger) *Handler {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.Server.UpstreamConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           512,
		MaxIdleConnsPerHost:    256,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  cfg.Server.UpstreamResponseHeaderTimeout,
		ExpectContinueTimeout:  1 * time.Second,
		MaxResponseHeaderBytes: 1 << 20,
	}
	return &Handler{
		cfg: cfg, cp: cp, router: r, providers: reg, meter: meter, metrics: m,
		client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		log: log,
	}
}

// ServeHTTP handles one inference request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proto, ok := protocol.Detect(r.URL.Path)
	if !ok {
		protocol.ErrNotFound(fmt.Sprintf("unknown path %s", r.URL.Path)).Write(w, protocol.OpenAIChat)
		return
	}
	if r.Method != http.MethodPost {
		(&protocol.GatewayError{Status: http.StatusMethodNotAllowed, Type: "invalid_request_error", Message: "method not allowed"}).Write(w, proto)
		return
	}
	reqID := newRequestID()
	w.Header().Set("X-Request-Id", reqID)
	start := time.Now()
	log := h.log.With("request_id", reqID, "protocol", string(proto))

	apiKey := extractAPIKey(r)
	if apiKey == "" {
		protocol.ErrUnauthorized("missing API key: use Authorization: Bearer <key> or x-api-key").Write(w, proto)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.cfg.Server.MaxRequestBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			(&protocol.GatewayError{Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error", Message: "request body too large"}).Write(w, proto)
			return
		}
		protocol.ErrBadRequest("failed to read request body").Write(w, proto)
		return
	}
	preq, err := protocol.ParseRequest(body)
	if err != nil {
		protocol.ErrBadRequest(err.Error()).Write(w, proto)
		return
	}
	log = log.With("model", preq.Model, "stream", preq.Stream)

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Server.RequestTimeout)
	defer cancel()

	// --- key-auth gate (fail-closed) ---
	authCtx, authCancel := context.WithTimeout(ctx, h.cfg.ControlPlane.KeyAuthTimeout)
	auth, err := h.cp.KeyAuth(authCtx, apiKey, preq.Model)
	authCancel()
	if err != nil {
		h.metrics.KeyAuthErrors.Inc()
		log.Error("key-auth unavailable", "err", err)
		protocol.ErrUnavailable("authorization service unavailable").Write(w, proto)
		return
	}
	if !auth.Valid {
		h.metrics.KeyAuthRejects.WithLabelValues(string(auth.RejectReason)).Inc()
		log.Info("key-auth rejected", "reason", auth.RejectReason, "subject", auth.SubjectCode)
		protocol.FromReject(auth.RejectReason, auth.Message).Write(w, proto)
		return
	}
	log = log.With("subject", auth.SubjectCode)

	// --- routing ---
	attempts := h.router.Attempts(preq.Model)
	if len(attempts) == 0 {
		log.Warn("no route for model")
		protocol.ErrNotFound(fmt.Sprintf("model %q has no active route", preq.Model)).Write(w, proto)
		return
	}
	if len(attempts) > h.cfg.Server.MaxFailoverAttempts {
		attempts = attempts[:h.cfg.Server.MaxFailoverAttempts]
	}

	var (
		lastCand   *router.Candidate
		lastErr    error
		lastStatus int
		outcome    *relayResult
	)
	for i := range attempts {
		cand := attempts[i]
		prov, ok := h.providers[cand.ProviderCode]
		if !ok {
			log.Error("route references unconfigured provider", "provider", cand.ProviderCode)
			lastErr = fmt.Errorf("provider %q not configured", cand.ProviderCode)
			continue
		}
		base, ok := prov.Endpoint(string(proto))
		if !ok {
			log.Warn("provider lacks endpoint for protocol", "provider", cand.ProviderCode)
			lastErr = fmt.Errorf("provider %q does not serve %s", cand.ProviderCode, proto)
			continue
		}
		lastCand = &cand

		upBody, err := preq.Rewrite(proto, cand.ProviderModelCode)
		if err != nil {
			protocol.ErrBadRequest("failed to rewrite request body").Write(w, proto)
			return
		}
		upURL := *base
		upURL.Path = strings.TrimRight(base.Path, "/") + proto.UpstreamPath()
		// upURL 只由配置里的 provider base URL 加协议固定路径拼成，不含任何客户端输入（客户端只能影响请求体）。
		upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL.String(), bytes.NewReader(upBody)) // nosemgrep: gosec.G107-1
		if err != nil {
			protocol.ErrBadGateway("failed to build upstream request").Write(w, proto)
			return
		}
		upReq.ContentLength = int64(len(upBody))
		copyRequestHeaders(r.Header, upReq.Header)
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("User-Agent", userAgent)
		if proto == protocol.Anthropic && upReq.Header.Get("anthropic-version") == "" {
			upReq.Header.Set("anthropic-version", defaultAnthropicVers)
		}
		if err := prov.Authenticate(ctx, upReq, upBody); err != nil {
			log.Error("upstream auth failed", "provider", cand.ProviderCode, "err", err)
			lastErr = err
			h.metrics.Failovers.WithLabelValues(cand.ProviderCode, "auth").Inc()
			continue
		}

		attemptStart := time.Now()
		resp, err := h.client.Do(upReq)
		if err != nil {
			if ctx.Err() != nil {
				lastErr = ctx.Err()
				break
			}
			lastErr = err
			h.metrics.Failovers.WithLabelValues(cand.ProviderCode, "transport").Inc()
			log.Warn("upstream transport error, failing over", "provider", cand.ProviderCode, "attempt", i+1, "err", err)
			continue
		}
		lastStatus = resp.StatusCode
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && i < len(attempts)-1 {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			h.metrics.Failovers.WithLabelValues(cand.ProviderCode, strconv.Itoa(resp.StatusCode)).Inc()
			log.Warn("upstream error status, failing over", "provider", cand.ProviderCode, "status", resp.StatusCode,
				"attempt", i+1, "body", string(snippet))
			lastErr = fmt.Errorf("upstream status %d", resp.StatusCode)
			continue
		}

		// Commit to this response.
		outcome = h.relay(w, resp, proto, attemptStart)
		outcome.provider = cand
		classifyTruncation(outcome, ctx, r.Context())
		break
	}

	dur := time.Since(start)
	if outcome == nil {
		// Every attempt failed before a response could be committed.
		status := http.StatusBadGateway
		msg := "all upstream attempts failed"
		if lastErr != nil {
			msg = msg + ": " + lastErr.Error()
		}
		if errors.Is(lastErr, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		(&protocol.GatewayError{Status: status, Type: "api_error", Code: "upstream_error", Message: msg}).Write(w, proto)
		provCode, provModel := "", ""
		if lastCand != nil {
			provCode, provModel = lastCand.ProviderCode, lastCand.ProviderModelCode
		}
		h.metrics.Requests.WithLabelValues(string(proto), provCode, strconv.Itoa(status)).Inc()
		log.Error("request failed", "status", status, "upstream_status", lastStatus, "duration_ms", dur.Milliseconds(), "err", lastErr)
		if lastCand != nil {
			h.report(reqID, apiKey, preq.Model, provModel, start, dur, 0, status, protocol.Usage{})
		}
		return
	}

	h.metrics.Requests.WithLabelValues(string(proto), outcome.provider.ProviderCode, strconv.Itoa(outcome.status)).Inc()
	h.metrics.Duration.WithLabelValues(string(proto), outcome.provider.ProviderCode, outcome.provider.ProviderModelCode).Observe(dur.Seconds())
	if outcome.ttft > 0 {
		h.metrics.TTFT.WithLabelValues(string(proto), outcome.provider.ProviderCode, outcome.provider.ProviderModelCode).Observe(outcome.ttft.Seconds())
	}
	u := outcome.usage
	if u.Found {
		lbl := h.metrics.Tokens.MustCurryWith(map[string]string{"provider": outcome.provider.ProviderCode, "model": preq.Model})
		lbl.WithLabelValues("input").Add(float64(u.Input))
		lbl.WithLabelValues("output").Add(float64(u.Output))
		lbl.WithLabelValues("cache_read").Add(float64(u.CacheRead))
		lbl.WithLabelValues("cache_write").Add(float64(u.CacheWrite))
		lbl.WithLabelValues("reasoning").Add(float64(u.Reasoning))
	}
	log.Info("request completed",
		"provider", outcome.provider.ProviderCode, "provider_model", outcome.provider.ProviderModelCode,
		"status", outcome.status, "duration_ms", dur.Milliseconds(), "ttft_ms", outcome.ttft.Milliseconds(),
		"input_tokens", u.Input, "output_tokens", u.Output, "cache_read", u.CacheRead, "cache_write", u.CacheWrite,
		"reasoning", u.Reasoning, "usage_found", u.Found, "client_error", outcome.clientErr != nil, "truncated", outcome.truncated)
	h.report(reqID, apiKey, preq.Model, outcome.provider.ProviderModelCode, start, dur, outcome.ttft, outcome.status, u)
}

func (h *Handler) report(reqID, apiKey, model, providerModel string, start time.Time, dur, ttft time.Duration, status int, u protocol.Usage) {
	h.meter.Enqueue(controlplane.UsageReport{
		RequestID:         reqID,
		APIKey:            apiKey,
		ModelCode:         model,
		ProviderModelCode: providerModel,
		StartTime:         start.UTC().Format("2006-01-02T15:04:05.000Z"),
		Duration:          dur.Milliseconds(),
		TTFT:              ttft.Milliseconds(),
		StatusCode:        status,
		InputTokens:       u.Input,
		OutputTokens:      u.Output,
		TotalTokens:       u.Total(),
		CacheReadTokens:   u.CacheRead,
		CacheWriteTokens:  u.CacheWrite,
		ReasoningTokens:   u.Reasoning,
	})
}

type relayResult struct {
	status      int
	ttft        time.Duration
	usage       protocol.Usage
	provider    router.Candidate
	clientErr   error  // writing to the client failed (client went away)
	upstreamErr error  // the upstream stream ended with an error instead of a clean EOF
	truncated   string // set by classifyTruncation: request_timeout / client_disconnect / upstream_stream_error
}

// StatusClientClosedRequest is nginx's convention for "client went away before the response
// finished". It is never sent on the wire (headers are already out), only reported.
const StatusClientClosedRequest = 499

// classifyTruncation marks streams that did not run to completion. Such records must not be
// billed (customer decision 2026-09-03): the reported status becomes 504 / 499 / 502 and the
// token counts are zeroed, matching the contract's shape for failed requests. A stream counts
// as complete only when the upstream body ended with a clean EOF and the client took all of it.
func classifyTruncation(res *relayResult, reqCtx, clientCtx context.Context) {
	if res.upstreamErr == nil && res.clientErr == nil {
		return
	}
	switch {
	case errors.Is(reqCtx.Err(), context.DeadlineExceeded):
		res.truncated, res.status = "request_timeout", http.StatusGatewayTimeout
	case res.clientErr != nil || clientCtx.Err() != nil:
		res.truncated, res.status = "client_disconnect", StatusClientClosedRequest
	default:
		res.truncated, res.status = "upstream_stream_error", http.StatusBadGateway
	}
	res.usage = protocol.Usage{}
}

// relay streams the upstream response to the client while extracting usage.
func (h *Handler) relay(w http.ResponseWriter, resp *http.Response, proto protocol.Protocol, attemptStart time.Time) *relayResult {
	defer resp.Body.Close()
	res := &relayResult{status: resp.StatusCode}
	for _, k := range forwardResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	if v := firstHeader(resp.Header, "x-request-id", "request-id", "x-amzn-requestid"); v != "" {
		w.Header().Set("X-Upstream-Request-Id", v)
	}
	isSSE := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

	if !isSSE {
		// Read one byte past the cap so an oversized body can be told from an exact fit.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
		res.ttft = time.Since(attemptStart)
		if err != nil {
			h.log.Warn("upstream body read error", "err", err)
			(&protocol.GatewayError{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_error", Message: "upstream body read error"}).Write(w, proto)
			res.status = http.StatusBadGateway
			return res
		}
		if int64(len(body)) > maxResponseBody {
			// Relaying the truncated bytes with the upstream 2xx would hand the client corrupt
			// JSON that looks successful. Fail loudly instead, and let metering see a 502.
			h.log.Warn("upstream response body too large, rejecting", "limit_bytes", maxResponseBody, "upstream_status", resp.StatusCode)
			(&protocol.GatewayError{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_body_too_large", Message: "upstream response body exceeds gateway limit"}).Write(w, proto)
			res.status = http.StatusBadGateway
			return res
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			res.usage = protocol.ParseUsage(proto, body)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(resp.StatusCode)
		_, res.clientErr = w.Write(body) // nosemgrep: no-direct-write-to-responsewriter, go.lang.security.audit.xss.no-direct-write-to-responsewriter -- 原样转发上游 JSON 正文，Content-Type 随上游头透传，不是渲染 HTML
		return res
	}

	// Streaming: write through immediately, parse SSE on the side.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	fw := &flushWriter{w: w}
	fbr := &firstByteReader{r: resp.Body}
	parser := protocol.NewStreamParser(proto)
	tee := io.TeeReader(fbr, fw)
	sc := bufio.NewScanner(tee)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var event string
	var data bytes.Buffer
	dispatch := func() {
		if data.Len() > 0 || event != "" {
			parser.Feed(event, bytes.TrimSuffix(data.Bytes(), []byte("\n")))
		}
		event = ""
		data.Reset()
	}
	for sc.Scan() {
		line := sc.Bytes()
		line = bytes.TrimSuffix(line, []byte("\r"))
		switch {
		case len(line) == 0:
			dispatch()
		case bytes.HasPrefix(line, []byte("event:")):
			event = string(bytes.TrimSpace(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			data.Write(bytes.TrimPrefix(line[5:], []byte(" ")))
			data.WriteByte('\n')
		}
	}
	dispatch()
	res.clientErr = fw.err
	if err := sc.Err(); err != nil {
		res.upstreamErr = err
		if fw.err == nil {
			h.log.Warn("upstream stream ended with error", "err", err)
		}
	}
	res.ttft = fbr.first.Sub(attemptStart)
	if fbr.first.IsZero() {
		res.ttft = 0
	}
	res.usage = parser.Usage()
	return res
}

type flushWriter struct {
	w   http.ResponseWriter
	err error
}

func (f *flushWriter) Write(p []byte) (int, error) {
	if f.err != nil {
		return len(p), nil // keep draining upstream so usage is still parsed
	}
	n, err := f.w.Write(p)
	if err != nil {
		f.err = err
		return len(p), nil
	}
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, nil
}

type firstByteReader struct {
	r     io.Reader
	first time.Time
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && f.first.IsZero() {
		f.first = time.Now()
	}
	return n, err
}

func copyRequestHeaders(src, dst http.Header) {
	for k, vs := range src {
		if forwardRequestHeaders[strings.ToLower(k)] {
			for _, v := range vs {
				dst.Add(k, v)
			}
		}
	}
}

func extractAPIKey(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if len(v) > 7 && strings.EqualFold(v[:7], "Bearer ") {
			return strings.TrimSpace(v[7:])
		}
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func firstHeader(h http.Header, keys ...string) string {
	for _, k := range keys {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// ModelsHandler serves GET /v1/models from the route snapshot (OpenAI list format).
func (h *Handler) ModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	models := h.router.Models()
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: make([]model, 0, len(models))}
	for _, m := range models {
		out.Data = append(out.Data, model{ID: m, Object: "model", OwnedBy: "gateway"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
