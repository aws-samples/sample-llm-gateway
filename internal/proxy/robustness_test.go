package proxy

// 健壮性测试：把网关放进一个可注入故障的环境里（控制面慢 / 挂 / 拒绝，上游慢 / 拒连 / 5xx / 永不结束，
// 客户端断线 / 慢发 body），验证高压生产环境下的行为：不阻塞、不泄漏、故障转移正确、计量不丢或按预期丢。
//
// 跑法：
//   go test -race ./internal/proxy/ -run Robust                 # 全部故障场景（约 20 秒）
//   ROBUST_LOAD=1 go test ./internal/proxy/ -run LoadCeiling -v # 本机吞吐上限（上游与控制面都是进程内 fake）
//
// 标注 "记录现状" 的断言描述的是当前实现的行为而不是理想行为，背景与取舍见 docs/robustness-report.md。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
	"github.com/aws-samples/sample-llm-gateway/internal/metering"
	"github.com/aws-samples/sample-llm-gateway/internal/observability"
	"github.com/aws-samples/sample-llm-gateway/internal/provider"
	"github.com/aws-samples/sample-llm-gateway/internal/router"
)

// ---------- 可注入故障的控制面 ----------

type chaosCP struct {
	keyAuthDelay    atomic.Int64 // ns，key-auth 响应前等待
	keyAuthStatus   atomic.Int32 // 非 0：直接回这个 HTTP 状态
	reportDelay     atomic.Int64 // ns，usage-report 响应前等待
	reportStatus    atomic.Int32 // 非 0：直接回这个 HTTP 状态
	reportFailFirst atomic.Int32 // 每个 request_id 前 N 次上报回 503

	mu       sync.Mutex
	attempts map[string]int // request_id -> 上报次数
	accepted []controlplane.UsageReport
}

func newChaosCP() *chaosCP { return &chaosCP{attempts: map[string]int{}} }

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *chaosCP) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/gateway/key-auth", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req) // 先读完 body，服务端才会察觉网关挂断（与真实控制面一致）
		if !sleepCtx(r.Context(), time.Duration(c.keyAuthDelay.Load())) {
			return
		}
		if st := c.keyAuthStatus.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00000", "data": map[string]any{
			"valid": true, "subjectCode": "tenant-a", "modelCode": req["modelCode"]}})
	})
	mux.HandleFunc("/admin/gateway/usage/report", func(w http.ResponseWriter, r *http.Request) {
		var rep controlplane.UsageReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		c.mu.Lock()
		c.attempts[rep.RequestID]++
		n := c.attempts[rep.RequestID]
		c.mu.Unlock()
		if !sleepCtx(r.Context(), time.Duration(c.reportDelay.Load())) {
			return
		}
		if st := c.reportStatus.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		if int32(n) <= c.reportFailFirst.Load() {
			w.WriteHeader(503)
			return
		}
		c.mu.Lock()
		c.accepted = append(c.accepted, rep)
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00000", "data": map[string]any{"accepted": true}})
	})
	return mux
}

func (c *chaosCP) acceptedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.accepted)
}

func (c *chaosCP) acceptedCopy() []controlplane.UsageReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]controlplane.UsageReport(nil), c.accepted...)
}

func (c *chaosCP) waitAccepted(n int, d time.Duration) []controlplane.UsageReport {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.acceptedCount() >= n {
			return c.acceptedCopy()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.acceptedCopy()
}

// ---------- 可注入故障的上游（Anthropic messages 协议） ----------

type chaosUpstream struct {
	name        string
	status      atomic.Int32 // 非 0：立即回这个状态 + 短 JSON
	headerDelay atomic.Int64 // ns，写响应头前等待（模拟首字慢）
	chunks      atomic.Int32 // 流式：content_block_delta 条数
	chunkDelay  atomic.Int64 // ns，每条 delta 之间等待
	hang        atomic.Bool  // 流式：发完 delta 后永不结束，直到请求上下文取消
	abort       atomic.Bool  // 流式：发完 delta 后直接关 TCP 连接（模拟上游网络层断掉）
	raw         atomic.Pointer[[]byte]
	rawCT       atomic.Pointer[string]

	calls     atomic.Int64
	cancelled atomic.Int64 // 处理途中发现请求上下文被取消的次数（网关取消了对上游的请求）
	inflight  atomic.Int64
}

const finalOutputTokens = 9

func (u *chaosUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		u.inflight.Add(1)
		defer u.inflight.Add(-1)
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if st := u.status.Load(); st != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(st))
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"upstream_%d","message":"%s boom"}}`, st, u.name)
			return
		}
		if !sleepCtx(r.Context(), time.Duration(u.headerDelay.Load())) {
			u.cancelled.Add(1)
			return
		}
		if raw := u.raw.Load(); raw != nil {
			w.Header().Set("Content-Type", *u.rawCT.Load())
			_, _ = w.Write(*raw)
			return
		}
		stream, _ := m["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":%q,"content":[{"type":"text","text":"hi from %s"}],"usage":{"input_tokens":11,"output_tokens":%d}}`,
				m["model"], u.name, finalOutputTokens)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		emit := func(ev, data string) bool {
			if r.Context().Err() != nil {
				u.cancelled.Add(1)
				return false
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, data)
			fl.Flush()
			return true
		}
		if !emit("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1}}}`) {
			return
		}
		for i := int32(0); i < u.chunks.Load(); i++ {
			if !sleepCtx(r.Context(), time.Duration(u.chunkDelay.Load())) {
				u.cancelled.Add(1)
				return
			}
			if !emit("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"chunk-%d "}}`, i)) {
				return
			}
		}
		if u.hang.Load() {
			<-r.Context().Done()
			u.cancelled.Add(1)
			return
		}
		if u.abort.Load() {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		if !emit("message_delta", fmt.Sprintf(`{"type":"message_delta","usage":{"output_tokens":%d}}`, finalOutputTokens)) {
			return
		}
		emit("message_stop", `{"type":"message_stop"}`)
	})
}

// ---------- 组装 ----------

type chaosOpts struct {
	readTimeout    time.Duration // http.Server.ReadTimeout（main.go 用 server.read_timeout）
	requestTimeout time.Duration
	headerTimeout  time.Duration
	connectTimeout time.Duration
	keyAuthTimeout time.Duration
	reportTimeout  time.Duration
	queueSize      int
	workers        int
	maxRetries     int
	maxBody        int64
	primaryURL     string // 覆盖 primary 的 base URL（例如指向一个已关闭的端口）
	tier           int    // >0：把 N 个同优先级、权重递增的 provider 挂到 model "tiered"
}

func (o *chaosOpts) defaults() {
	if o.readTimeout == 0 {
		o.readTimeout = 60 * time.Second
	}
	if o.requestTimeout == 0 {
		o.requestTimeout = 10 * time.Second
	}
	if o.headerTimeout == 0 {
		o.headerTimeout = 5 * time.Second
	}
	if o.connectTimeout == 0 {
		o.connectTimeout = time.Second
	}
	if o.keyAuthTimeout == 0 {
		o.keyAuthTimeout = time.Second
	}
	if o.reportTimeout == 0 {
		o.reportTimeout = time.Second
	}
	if o.queueSize == 0 {
		o.queueSize = 1000
	}
	if o.workers == 0 {
		o.workers = 2
	}
	if o.maxRetries == 0 {
		o.maxRetries = 2
	}
	if o.maxBody == 0 {
		o.maxBody = 1 << 20
	}
}

type chaosHarness struct {
	t       *testing.T
	gw      *httptest.Server
	gwSrv   *http.Server
	cp      *chaosCP
	cpSrv   *httptest.Server
	up, up2 *chaosUpstream
	tiers   []*chaosUpstream
	rt      *router.Router
	q       *metering.Queue
	m       *observability.Metrics
	client  *http.Client
	opts    chaosOpts
}

func newChaos(t *testing.T, opts chaosOpts) *chaosHarness {
	t.Helper()
	opts.defaults()
	cp := newChaosCP()
	cpSrv := httptest.NewServer(cp.handler())
	t.Cleanup(cpSrv.Close)
	up := &chaosUpstream{name: "primary"}
	up.chunks.Store(3)
	upSrv := httptest.NewServer(up.handler())
	t.Cleanup(upSrv.Close)
	up2 := &chaosUpstream{name: "backup"}
	up2.chunks.Store(3)
	up2Srv := httptest.NewServer(up2.handler())
	t.Cleanup(up2Srv.Close)

	primary := upSrv.URL
	if opts.primaryURL != "" {
		primary = opts.primaryURL
	}
	cfg := &config.Config{}
	cfg.Server.RequestTimeout = opts.requestTimeout
	cfg.Server.UpstreamConnectTimeout = opts.connectTimeout
	cfg.Server.UpstreamResponseHeaderTimeout = opts.headerTimeout
	cfg.Server.MaxRequestBodyBytes = opts.maxBody
	cfg.Server.MaxFailoverAttempts = 3
	cfg.ControlPlane.KeyAuthTimeout = opts.keyAuthTimeout
	cfg.ControlPlane.TokenHeader = "X-HIGRESS-Token"
	cfg.Providers = map[string]config.ProviderConfig{
		"primary": {Auth: config.AuthXAPIKey, APIKey: "p-key", Endpoints: map[string]string{config.EndpointAnthropic: primary + "/v1"}},
		"backup":  {Auth: config.AuthBearer, APIKey: "b-key", Endpoints: map[string]string{config.EndpointAnthropic: up2Srv.URL + "/v1"}},
	}
	routes := &controlplane.Routes{Version: "v1", Models: []controlplane.ModelRoute{
		{ModelCode: "claude-x", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "primary", ProviderModelCode: "primary.claude-x", Priority: 1, Weight: 100},
			{ProviderCode: "backup", ProviderModelCode: "backup.claude-x", Priority: 2, Weight: 100},
		}},
		{ModelCode: "primary-only", Providers: []controlplane.ProviderRoute{
			{ProviderCode: "primary", ProviderModelCode: "primary.only", Priority: 1, Weight: 100},
		}},
	}}
	h := &chaosHarness{t: t, cp: cp, cpSrv: cpSrv, up: up, up2: up2, opts: opts}
	if opts.tier > 0 {
		var prs []controlplane.ProviderRoute
		for i := 0; i < opts.tier; i++ {
			u := &chaosUpstream{name: fmt.Sprintf("tier-%d", i)}
			u.chunks.Store(1)
			s := httptest.NewServer(u.handler())
			t.Cleanup(s.Close)
			code := fmt.Sprintf("tier-%d", i)
			cfg.Providers[code] = config.ProviderConfig{Auth: config.AuthNone, Endpoints: map[string]string{config.EndpointAnthropic: s.URL + "/v1"}}
			prs = append(prs, controlplane.ProviderRoute{ProviderCode: code, ProviderModelCode: code + ".model", Priority: 1, Weight: (i + 1) * 10})
			h.tiers = append(h.tiers, u)
		}
		routes.Models = append(routes.Models, controlplane.ModelRoute{ModelCode: "tiered", Providers: prs})
	}
	reg, err := provider.Build(context.Background(), cfg.Providers)
	if err != nil {
		t.Fatal(err)
	}
	cpc := controlplane.New(cpSrv.URL, "tok", "X-HIGRESS-Token")
	rt := router.New()
	rt.Load(routes)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := observability.NewMetrics()
	q := metering.New(cpc, opts.queueSize, opts.workers, opts.maxRetries, opts.reportTimeout, log, m)
	q.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		q.Shutdown(ctx)
	})
	hd := New(cfg, cpc, rt, reg, q, m, log)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", hd.ModelsHandler)
	mux.Handle("/", hd)
	gw := httptest.NewUnstartedServer(mux)
	gw.Config.ReadHeaderTimeout = 10 * time.Second
	gw.Config.ReadTimeout = opts.readTimeout
	gw.Start()
	t.Cleanup(gw.Close)
	h.gw, h.gwSrv, h.rt, h.q, h.m = gw, gw.Config, rt, q, m
	h.client = &http.Client{Transport: &http.Transport{MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024}}
	return h
}

func (h *chaosHarness) do(ctx context.Context, model string, stream bool) (*http.Response, error) {
	body := fmt.Sprintf(`{"model":%q,"stream":%v,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`, model, stream)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.gw.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	return h.client.Do(req)
}

// call 发一个请求并读完 body，返回状态码、body、耗时。
func (h *chaosHarness) call(model string, stream bool) (int, string, time.Duration) {
	start := time.Now()
	resp, err := h.do(context.Background(), model, stream)
	if err != nil {
		return 0, err.Error(), time.Since(start)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b), time.Since(start)
}

// metric 从 /metrics 文本里取一个样本值：name 是指标名，labels 形如 `provider="primary",reason="transport"`（顺序按字母序）。
func (h *chaosHarness) metric(name, labels string) float64 {
	rec := httptest.NewRecorder()
	h.m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	prefix := name
	if labels != "" {
		prefix = name + "{" + labels + "}"
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, prefix+" ") {
			v, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
			return v
		}
	}
	return 0
}

func (h *chaosHarness) failovers(provider, reason string) float64 {
	return h.metric("llmgw_upstream_failovers_total", fmt.Sprintf(`provider=%q,reason=%q`, provider, reason))
}

func (h *chaosHarness) reports(result string) float64 {
	return h.metric("llmgw_metering_reports_total", fmt.Sprintf(`result=%q`, result))
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// goroutineBaseline 在一次预热请求后取样，后续用 assertNoLeak 比较。
func (h *chaosHarness) goroutineBaseline() int {
	h.call("claude-x", false)
	h.client.CloseIdleConnections()
	time.Sleep(100 * time.Millisecond)
	return runtime.NumGoroutine()
}

// lastReport 等到至少 n 条上报后返回最后一条（预热请求也会产生一条，用它跳过）。
func (h *chaosHarness) lastReport(n int) controlplane.UsageReport {
	h.t.Helper()
	reps := h.cp.waitAccepted(n, 2*time.Second)
	if len(reps) < n {
		h.t.Fatalf("only %d reports, want %d", len(reps), n)
	}
	return reps[len(reps)-1]
}

// assertNoLeak 先关掉测试客户端自己的空闲 keep-alive 连接（每条连接在客户端和服务端各占 goroutine，不算网关的），
// 再等 goroutine 数回落到基线附近。
func (h *chaosHarness) assertNoLeak(base int, slack int) {
	h.t.Helper()
	var n int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.client.CloseIdleConnections()
		n = runtime.NumGoroutine()
		if n <= base+slack {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Errorf("goroutine leak: baseline %d, now %d (slack %d)", base, n, slack)
}

// ---------- 场景 ----------

// 同一优先级 3 个 provider 加权随机选路，300 个并发请求：全部成功、分布按权重、每条都计量。
// 这是 -race 的主要探针：Router 的随机数生成器被所有请求 goroutine 共享。
func TestRobust_ConcurrentWeightedRouting(t *testing.T) {
	h := newChaos(t, chaosOpts{tier: 3})
	const n = 300
	var wg sync.WaitGroup
	var bad atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, body, _ := h.call("tiered", i%2 == 0)
			if st != 200 {
				bad.Add(1)
				t.Logf("status %d: %s", st, body)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d requests failed", bad.Load())
	}
	var dist []string
	total := int64(0)
	for _, u := range h.tiers {
		dist = append(dist, fmt.Sprintf("%s=%d", u.name, u.calls.Load()))
		total += u.calls.Load()
	}
	t.Logf("distribution (weights 10/20/30): %s", strings.Join(dist, " "))
	if total != n {
		t.Errorf("upstream calls %d != %d", total, n)
	}
	// 权重 10/20/30 → 期望占比 1/6, 2/6, 3/6；给 ±60% 的容忍。
	if c := h.tiers[2].calls.Load(); c < n/6 || c > n*5/6 {
		t.Errorf("weight-30 provider got %d of %d, weighting looks broken", c, n)
	}
	if got := h.cp.waitAccepted(n, 5*time.Second); len(got) != n {
		t.Errorf("metering: accepted %d of %d", len(got), n)
	}
}

// 控制面完全不可达（连接拒绝）：fail-closed，503 要快，不能拖到 key_auth_timeout，也不能泄漏 goroutine。
func TestRobust_ControlPlaneDown(t *testing.T) {
	h := newChaos(t, chaosOpts{keyAuthTimeout: 2 * time.Second})
	base := h.goroutineBaseline()
	h.cpSrv.Close() // 端口关闭 → connection refused
	var wg sync.WaitGroup
	var maxDur atomic.Int64
	var codes sync.Map
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, _, d := h.call("claude-x", false)
			codes.Store(st, true)
			for {
				old := maxDur.Load()
				if int64(d) <= old || maxDur.CompareAndSwap(old, int64(d)) {
					break
				}
			}
		}()
	}
	wg.Wait()
	codes.Range(func(k, _ any) bool {
		if k.(int) != 503 {
			t.Errorf("expected 503 for every request, saw %v", k)
		}
		return true
	})
	if d := time.Duration(maxDur.Load()); d > 500*time.Millisecond {
		t.Errorf("connection refused should fail fast, slowest request took %v", d)
	}
	if v := h.metric("llmgw_keyauth_errors_total", ""); v != 100 {
		t.Errorf("keyauth_errors = %v, want 100", v)
	}
	if h.up.calls.Load() != 1 { // 只有预热那一次
		t.Errorf("upstream must not be called when key-auth fails, calls=%d", h.up.calls.Load())
	}
	h.assertNoLeak(base, 8)
}

// 控制面挂住不回（比拒连更常见的生产故障）：每个请求在 key_auth_timeout 处放弃，上游零调用。
func TestRobust_ControlPlaneHang(t *testing.T) {
	h := newChaos(t, chaosOpts{keyAuthTimeout: 300 * time.Millisecond})
	base := h.goroutineBaseline()
	h.cp.keyAuthDelay.Store(int64(5 * time.Second))
	var wg sync.WaitGroup
	var durs [100]time.Duration
	var codes [100]int
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, durs[i] = h.call("claude-x", false)
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 503 {
			t.Fatalf("request %d: status %d", i, codes[i])
		}
		if durs[i] < 280*time.Millisecond || durs[i] > 900*time.Millisecond {
			t.Errorf("request %d took %v, want ≈ key_auth_timeout (300ms)", i, durs[i])
		}
	}
	if h.up.calls.Load() != 1 {
		t.Errorf("upstream calls=%d, want 1 (warm-up only)", h.up.calls.Load())
	}
	h.cp.keyAuthDelay.Store(0)
	h.assertNoLeak(base, 8)
}

// 上游首字超过 upstream_response_header_timeout：算传输失败，切到备份，整体耗时 ≈ 超时值而不是叠加。
func TestRobust_UpstreamHeaderTimeoutFailover(t *testing.T) {
	h := newChaos(t, chaosOpts{headerTimeout: 300 * time.Millisecond})
	h.up.headerDelay.Store(int64(3 * time.Second))
	st, body, d := h.call("claude-x", false)
	if st != 200 || !strings.Contains(body, "hi from backup") {
		t.Fatalf("expected backup to serve, got %d %s", st, body)
	}
	if d < 280*time.Millisecond || d > time.Second {
		t.Errorf("failover took %v, want ≈ header timeout", d)
	}
	if h.failovers("primary", "transport") != 1 {
		t.Errorf("transport failover not counted")
	}
	waitFor(t, 2*time.Second, "primary to observe cancel", func() bool { return h.up.cancelled.Load() == 1 })
	reps := h.cp.waitAccepted(1, 2*time.Second)
	if len(reps) != 1 || reps[0].ProviderModelCode != "backup.claude-x" || reps[0].OutputTokens != finalOutputTokens {
		t.Errorf("report should name backup with full usage: %+v", reps)
	}
}

// 上游端口不通（节点 / VPCE 故障）：连接拒绝立即切备份。
func TestRobust_UpstreamConnectionRefusedFailover(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close()
	h := newChaos(t, chaosOpts{primaryURL: dead})
	st, body, d := h.call("claude-x", true)
	if st != 200 || !strings.Contains(body, "message_stop") {
		t.Fatalf("expected backup stream, got %d %s", st, body)
	}
	if d > 500*time.Millisecond {
		t.Errorf("refused connection should fail over instantly, took %v", d)
	}
	if h.failovers("primary", "transport") != 1 {
		t.Errorf("transport failover not counted")
	}
}

// 所有候选都 5xx：最后一个候选的状态与 body 原样透传（不是网关自己的 502），并且计量记录带这个状态。
func TestRobust_AllProvidersFail_LastStatusPassthrough(t *testing.T) {
	h := newChaos(t, chaosOpts{})
	h.up.status.Store(503)
	h.up2.status.Store(529)
	st, body, _ := h.call("claude-x", false)
	if st != 529 || !strings.Contains(body, "backup boom") {
		t.Fatalf("want upstream 529 passthrough, got %d %s", st, body)
	}
	if h.failovers("primary", "503") != 1 {
		t.Errorf("503 failover not counted")
	}
	reps := h.cp.waitAccepted(1, 2*time.Second)
	if len(reps) != 1 || reps[0].StatusCode != 529 || reps[0].ProviderModelCode != "backup.claude-x" {
		t.Errorf("report: %+v", reps)
	}

	// 只有一个候选时 5xx 也直接透传，不做无意义的重试。
	h.up.calls.Store(0)
	st, _, _ = h.call("primary-only", false)
	if st != 503 || h.up.calls.Load() != 1 {
		t.Errorf("single candidate: status %d calls %d", st, h.up.calls.Load())
	}
}

// 流式响应超过 request_timeout：网关切断流、取消上游，计量按「不计费」上报：status 504、token 全 0。
// 记录现状：客户端收到的是一个"正常结束"的 chunked 响应，没有 message_stop，也没有错误事件。
func TestRobust_RequestTimeoutMidStream(t *testing.T) {
	h := newChaos(t, chaosOpts{requestTimeout: 700 * time.Millisecond})
	base := h.goroutineBaseline()
	h.up.chunks.Store(1000)
	h.up.chunkDelay.Store(int64(50 * time.Millisecond))
	h.up.hang.Store(true)
	st, body, d := h.call("claude-x", true)
	if st != 200 {
		t.Fatalf("status %d", st)
	}
	if d < 650*time.Millisecond || d > 1500*time.Millisecond {
		t.Errorf("stream should be cut at request_timeout, took %v", d)
	}
	n := strings.Count(body, "event: content_block_delta")
	if n < 5 || n > 20 || strings.Contains(body, "message_stop") {
		t.Errorf("expected a truncated stream (~13 deltas, no message_stop), got %d deltas", n)
	}
	waitFor(t, 2*time.Second, "upstream cancel", func() bool { return h.up.cancelled.Load() >= 1 })
	rep := h.lastReport(2)
	if rep.StatusCode != 504 || rep.InputTokens != 0 || rep.OutputTokens != 0 || rep.TotalTokens != 0 {
		t.Errorf("truncated stream must be reported as 504 with zero tokens: %+v", rep)
	}
	if v := h.metric("llmgw_requests_total", `protocol="anthropic",provider="primary",status="504"`); v != 1 {
		t.Errorf("requests_total status=504 = %v, want 1", v)
	}
	h.up.hang.Store(false)
	h.assertNoLeak(base, 8)
}

// 客户端中途断开：网关把取消传给上游（不白烧 token），计量按「不计费」上报：status 499、token 全 0。
func TestRobust_ClientDisconnectMidStream(t *testing.T) {
	h := newChaos(t, chaosOpts{})
	base := h.goroutineBaseline()
	h.up.chunks.Store(1000)
	h.up.chunkDelay.Store(int64(30 * time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := h.do(ctx, "claude-x", true)
	if err != nil {
		t.Fatal(err)
	}
	// 读到第 3 个 delta 就挂断
	sc := bufio.NewScanner(resp.Body)
	seen := 0
	for sc.Scan() && seen < 3 {
		if strings.HasPrefix(sc.Text(), "event: content_block_delta") {
			seen++
		}
	}
	cancel()
	resp.Body.Close()
	waitFor(t, 2*time.Second, "upstream to see the cancel", func() bool { return h.up.cancelled.Load() >= 1 })
	rep := h.lastReport(2)
	if rep.StatusCode != 499 || rep.InputTokens != 0 || rep.OutputTokens != 0 {
		t.Errorf("client disconnect must be reported as 499 with zero tokens: %+v", rep)
	}
	h.assertNoLeak(base, 8)
}

// 上游流中途断连（网络层，不是 4xx/5xx）：客户端已经拿到 200 和一部分内容，无法重试；计量按不完整处理，status 502、token 全 0。
func TestRobust_UpstreamStreamAbortedMidway(t *testing.T) {
	h := newChaos(t, chaosOpts{})
	h.up.chunks.Store(3)
	h.up.abort.Store(true)
	st, body, _ := h.call("claude-x", true)
	if st != 200 || strings.Count(body, "event: content_block_delta") != 3 || strings.Contains(body, "message_stop") {
		t.Fatalf("expected a 200 stream cut after 3 deltas, got %d: %q", st, body)
	}
	rep := h.lastReport(1)
	if rep.StatusCode != 502 || rep.OutputTokens != 0 || rep.InputTokens != 0 {
		t.Errorf("aborted upstream stream must be reported as 502 with zero tokens: %+v", rep)
	}
	// 正常跑完的流不受影响
	h.up.abort.Store(false)
	if st, _, _ := h.call("claude-x", true); st != 200 {
		t.Fatal(st)
	}
	if rep := h.lastReport(2); rep.StatusCode != 200 || rep.OutputTokens != finalOutputTokens {
		t.Errorf("complete stream should still bill: %+v", rep)
	}
}

// 计量队列打满：请求路径不能被拖慢（Enqueue 非阻塞），超出的按 dropped 计数，队列里的最终送达。
func TestRobust_MeteringQueueFullDoesNotBlockRequests(t *testing.T) {
	h := newChaos(t, chaosOpts{queueSize: 4, workers: 1, reportTimeout: 2 * time.Second})
	h.cp.reportDelay.Store(int64(200 * time.Millisecond))
	const n = 40
	var slowest time.Duration
	for i := 0; i < n; i++ {
		st, _, d := h.call("claude-x", false)
		if st != 200 {
			t.Fatalf("status %d", st)
		}
		if d > slowest {
			slowest = d
		}
	}
	if slowest > 150*time.Millisecond {
		t.Errorf("request path slowed down by metering backlog: slowest %v", slowest)
	}
	waitFor(t, 5*time.Second, "queue to drain", func() bool {
		return h.reports("ok")+h.reports("dropped") >= n
	})
	ok, dropped := h.reports("ok"), h.reports("dropped")
	t.Logf("queue_size=4 workers=1 cp_latency=200ms burst=%d → ok=%v dropped=%v", n, ok, dropped)
	if dropped == 0 || ok < 5 {
		t.Errorf("expected both delivered and dropped reports, got ok=%v dropped=%v", ok, dropped)
	}
	if v := h.metric("llmgw_metering_queue_depth", ""); v != 0 {
		t.Errorf("queue depth gauge should return to 0, got %v", v)
	}
}

// 控制面上报接口回非重试类错误（401，如 token 被轮换）：每条只试一次就丢，不产生重试风暴。
func TestRobust_MeteringNonRetryableDropsOnce(t *testing.T) {
	h := newChaos(t, chaosOpts{maxRetries: 5})
	h.cp.reportStatus.Store(401)
	for i := 0; i < 5; i++ {
		if st, _, _ := h.call("claude-x", false); st != 200 {
			t.Fatalf("status %d", st)
		}
	}
	waitFor(t, 3*time.Second, "5 drops", func() bool { return h.reports("dropped") == 5 })
	h.cp.mu.Lock()
	defer h.cp.mu.Unlock()
	for id, n := range h.cp.attempts {
		if n != 1 {
			t.Errorf("%s attempted %d times, want exactly 1 for a 401", id, n)
		}
	}
}

// 控制面上报接口抖动（每条前 2 次 503）：指数退避重试后全部送达，request_id 不变（控制面可幂等去重）。
func TestRobust_MeteringRetriesThenRecovers(t *testing.T) {
	h := newChaos(t, chaosOpts{maxRetries: 3, workers: 4})
	h.cp.reportFailFirst.Store(2)
	const n = 8
	for i := 0; i < n; i++ {
		if st, _, _ := h.call("claude-x", i%2 == 0); st != 200 {
			t.Fatalf("status %d", st)
		}
	}
	reps := h.cp.waitAccepted(n, 8*time.Second)
	if len(reps) != n {
		t.Fatalf("accepted %d of %d after retries", len(reps), n)
	}
	h.cp.mu.Lock()
	for _, r := range reps {
		if h.cp.attempts[r.RequestID] != 3 {
			t.Errorf("%s: %d attempts, want 3 (2 failures + 1 success)", r.RequestID, h.cp.attempts[r.RequestID])
		}
	}
	h.cp.mu.Unlock()
	if h.reports("ok") != n || h.reports("dropped") != 0 {
		t.Errorf("ok=%v dropped=%v", h.reports("ok"), h.reports("dropped"))
	}
}

// 超大 / 畸形请求体：413 / 400 都要在鉴权前就拦住，上游零调用。
func TestRobust_BodyLimitsAndMalformedInput(t *testing.T) {
	h := newChaos(t, chaosOpts{maxBody: 64 << 10})
	send := func(body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	pad := func(n int) string { // 合法 JSON，总长恰好 n 字节
		head := `{"model":"claude-x","messages":[],"pad":"`
		tail := `"}`
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"exactly at limit", pad(64 << 10), 200},
		{"one byte over", pad((64 << 10) + 1), 413},
		{"way over (4 MiB)", pad(4 << 20), 413},
		{"empty body", "", 400},
		{"truncated json", `{"model":"claude-x","messages":[`, 400},
		{"json array", `[{"model":"claude-x"}]`, 400},
		{"model not a string", `{"model":123,"messages":[]}`, 400},
		{"model null", `{"model":null}`, 400},
		{"stream not a bool is tolerated", `{"model":"claude-x","stream":"yes","messages":[]}`, 200},
	}
	for _, c := range cases {
		before := h.up.calls.Load()
		st, body := send(c.body)
		if st != c.want {
			t.Errorf("%s: status %d want %d (%s)", c.name, st, c.want, strings.TrimSpace(body))
		}
		if c.want != 200 && h.up.calls.Load() != before {
			t.Errorf("%s: upstream was called for a rejected request", c.name)
		}
	}
}

// 慢发 body 的客户端（slowloris 变种）：request_timeout 在 body 读完之后才开始计时，所以读 body 这一段由
// http.Server 的 ReadTimeout（配置 server.read_timeout，默认 60s）兜底。这里用 500ms 的 ReadTimeout、拖 1.2s 的 body，
// 期望请求被拒（400 或连接被服务端关闭），而不是像修复前那样成功返回 200。
func TestRobust_SlowBodyClientCutByReadTimeout(t *testing.T) {
	h := newChaos(t, chaosOpts{readTimeout: 500 * time.Millisecond, requestTimeout: 10 * time.Second})
	body := `{"model":"claude-x","messages":[{"role":"user","content":"slow"}]}`
	conn, err := net.Dial("tcp", strings.TrimPrefix(h.gw.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /v1/messages HTTP/1.1\r\nHost: gw\r\nAuthorization: Bearer k\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(body))
	half := len(body) / 2
	_, _ = conn.Write([]byte(body[:half]))
	time.Sleep(1200 * time.Millisecond)
	_, _ = conn.Write([]byte(body[half:]))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Logf("connection closed by server before a response: %v (acceptable)", err)
	} else {
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == 200 {
			t.Fatalf("slow body upload must not succeed past ReadTimeout, got 200")
		}
		t.Logf("slow body rejected with status %d", resp.StatusCode)
	}
	if h.up.calls.Load() != 0 {
		t.Errorf("upstream must not be called, calls=%d", h.up.calls.Load())
	}
}

// 路由表热更新与请求并发：200 个在途请求期间快速切换快照 100 次，不能出现任何非 200。
// 控制面下发空路由表的守卫在 Poller.Sync，见 internal/router 的 TestPollerRejectsEmptyTable。
func TestRobust_RouteHotSwapUnderLoad(t *testing.T) {
	h := newChaos(t, chaosOpts{})
	mk := func(provider string) *controlplane.Routes {
		return &controlplane.Routes{Version: provider, Models: []controlplane.ModelRoute{
			{ModelCode: "claude-x", Providers: []controlplane.ProviderRoute{
				{ProviderCode: provider, ProviderModelCode: provider + ".claude-x", Priority: 1, Weight: 1}}},
		}}
	}
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				h.rt.Load(mk("primary"))
			} else {
				h.rt.Load(mk("backup"))
			}
			time.Sleep(time.Millisecond)
		}
	}()
	var wg sync.WaitGroup
	var bad atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if st, _, _ := h.call("claude-x", i%2 == 0); st != 200 {
					bad.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	if bad.Load() != 0 {
		t.Errorf("%d requests failed during hot swap", bad.Load())
	}
	a, b := h.up.calls.Load(), h.up2.calls.Load()
	if a == 0 || b == 0 {
		t.Errorf("both providers should have served during the swap: primary=%d backup=%d", a, b)
	}

}

// SSE 边角：CRLF 行尾、注释行、多行 data、1 MiB 单行、结尾没有空行。客户端收到的字节要与上游完全一致，usage 也要解析到。
func TestRobust_SSEEdgeCasesBytewisePassthrough(t *testing.T) {
	h := newChaos(t, chaosOpts{})
	big := strings.Repeat("A", 1<<20)
	raw := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":1}}}\r\n\r\n" +
		": keep-alive comment\r\n\r\n" +
		"data:{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"no-space-after-colon\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + big + "\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\ndata: \"usage\":{\"output_tokens\":42}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}" // 没有结尾空行，直接 EOF
	b := []byte(raw)
	ct := "text/event-stream; charset=utf-8"
	h.up.raw.Store(&b)
	h.up.rawCT.Store(&ct)
	st, body, _ := h.call("claude-x", true)
	if st != 200 {
		t.Fatalf("status %d", st)
	}
	if body != raw {
		t.Errorf("stream altered in transit: len %d vs %d", len(body), len(raw))
	}
	reps := h.cp.waitAccepted(1, 2*time.Second)
	if len(reps) != 1 || reps[0].InputTokens != 11 || reps[0].OutputTokens != 42 {
		t.Errorf("usage from edge-case stream: %+v", reps)
	}
}

// 优雅退出：在途流式请求期间 Shutdown，请求要完整跑完（收到 message_stop），Shutdown 后新连接被拒，
// 队列里的计量在 metering.Shutdown 内送达。
func TestRobust_GracefulShutdownDrainsInflight(t *testing.T) {
	h := newChaos(t, chaosOpts{})
	h.up.chunks.Store(8)
	h.up.chunkDelay.Store(int64(60 * time.Millisecond)) // ≈ 0.5s 的流
	h.cp.reportDelay.Store(int64(150 * time.Millisecond))
	type result struct {
		st   int
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := h.do(context.Background(), "claude-x", true)
		if err != nil {
			res <- result{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		res <- result{resp.StatusCode, string(b), err}
	}()
	time.Sleep(120 * time.Millisecond) // 确保流已经开始
	shStart := time.Now()
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.gwSrv.Shutdown(shCtx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	shDur := time.Since(shStart)
	r := <-res
	if r.err != nil || r.st != 200 || !strings.Contains(r.body, "message_stop") || strings.Count(r.body, "event: content_block_delta") != 8 {
		t.Errorf("in-flight stream was cut by shutdown: err=%v status=%d deltas=%d", r.err, r.st, strings.Count(r.body, "event: content_block_delta"))
	}
	if shDur < 300*time.Millisecond {
		t.Errorf("Shutdown returned after %v, before the in-flight stream finished", shDur)
	}
	if _, err := h.do(context.Background(), "claude-x", false); err == nil {
		t.Errorf("new connection accepted after Shutdown")
	}
	mCtx, mCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer mCancel()
	h.q.Shutdown(mCtx)
	if got := h.cp.acceptedCount(); got != 1 {
		t.Errorf("metering not flushed during shutdown: accepted %d", got)
	}
}

// 响应头：每个响应都带 X-Request-Id，上游 Content-Type 原样透传。
func TestRobust_UpstreamHeadersPassthrough(t *testing.T) {
	h := newChaos(t, chaosOpts{})
	st, _, _ := h.call("claude-x", false)
	if st != 200 {
		t.Fatal(st)
	}
	resp, err := h.do(context.Background(), "claude-x", false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Request-Id") == "" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("headers: %v", resp.Header)
	}
	io.Copy(io.Discard, resp.Body)
}

// ---------- 吞吐上限（本机，进程内 fake 上游与控制面） ----------

// ROBUST_LOAD=1 时运行：非流式与流式各打两轮，输出 rps / 分位延迟 / 堆内存。
// 结果是网关 + fake 共享一台机器 CPU 的下限值，不是绝对上限；意义在于看单副本在纯转发路径上的开销量级与是否有泄漏。
// 泄漏判定用"平台法"：同样的负载跑第二轮之后 goroutine 数不应再增长（第一轮会把上游 / 控制面连接池填满，那不是泄漏）。
func TestRobust_LoadCeiling(t *testing.T) {
	if os.Getenv("ROBUST_LOAD") == "" {
		t.Skip("set ROBUST_LOAD=1 to run the local throughput probe")
	}
	h := newChaos(t, chaosOpts{queueSize: 100000, workers: 8})
	h.up.chunks.Store(4)
	run := func(name string, stream bool, total, conc int) {
		lat := make([]time.Duration, total)
		var idx atomic.Int64
		var fail atomic.Int64
		var wg sync.WaitGroup
		var ms0, ms1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms0)
		start := time.Now()
		for c := 0; c < conc; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(idx.Add(1)) - 1
					if i >= total {
						return
					}
					s := time.Now()
					st, _, _ := h.call("claude-x", stream)
					lat[i] = time.Since(s)
					if st != 200 {
						fail.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		wall := time.Since(start)
		runtime.ReadMemStats(&ms1)
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		p := func(q float64) time.Duration { return lat[int(float64(total-1)*q)] }
		t.Logf("%s: total=%d conc=%d wall=%v rps=%.0f fail=%d p50=%v p95=%v p99=%v max=%v heap_inuse=%dMiB→%dMiB goroutines=%d",
			name, total, conc, wall.Round(time.Millisecond), float64(total)/wall.Seconds(), fail.Load(),
			p(0.5).Round(time.Microsecond), p(0.95).Round(time.Microsecond), p(0.99).Round(time.Microsecond), lat[total-1].Round(time.Microsecond),
			ms0.HeapInuse>>20, ms1.HeapInuse>>20, runtime.NumGoroutine())
		if fail.Load() != 0 {
			t.Errorf("%s: %d failures", name, fail.Load())
		}
	}
	settle := func() int {
		time.Sleep(300 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	run("non-stream round 1", false, 20000, 256)
	g1 := settle()
	run("non-stream round 2", false, 20000, 256)
	g2 := settle()
	run("stream(4 deltas) round 1", true, 10000, 256)
	g3 := settle()
	run("stream(4 deltas) round 2", true, 10000, 256)
	g4 := settle()
	waitFor(t, 30*time.Second, "metering to drain", func() bool { return h.cp.acceptedCount() >= 60000 })
	t.Logf("metering: ok=%v dropped=%v; goroutines after rounds: %d → %d (non-stream), %d → %d (stream)", h.reports("ok"), h.reports("dropped"), g1, g2, g3, g4)
	if g2 > g1+16 || g4 > g3+16 {
		t.Errorf("goroutines keep growing between identical rounds: %d→%d, %d→%d", g1, g2, g3, g4)
	}
}
