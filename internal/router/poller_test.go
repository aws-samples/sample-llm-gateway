package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
)

type fakeCounter struct{ n atomic.Int64 }

func (c *fakeCounter) Inc() { c.n.Add(1) }

// 控制面因自身故障返回 200 + 空模型列表时，网关必须保留上一份非空快照，并且不推进 ETag（下次重新拉全表）。
// 启动时（还没有快照）空表照常加载，否则网关永远起不来。
func TestPollerRejectsEmptyTable(t *testing.T) {
	var body atomic.Pointer[controlplane.Routes]
	full := &controlplane.Routes{Version: "v1", Models: []controlplane.ModelRoute{{
		ModelCode: "m", Providers: []controlplane.ProviderRoute{{ProviderCode: "a", ProviderModelCode: "a.m", Priority: 1, Weight: 1}}}}}
	empty := &controlplane.Routes{Version: "v2"}
	var etags []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etags = append(etags, r.Header.Get("If-None-Match"))
		rt := body.Load()
		w.Header().Set("ETag", rt.Version)
		_ = json.NewEncoder(w).Encode(rt)
	}))
	defer srv.Close()
	cp := controlplane.New(srv.URL, "tok", "X-HIGRESS-Token")
	r := New()
	p := NewPoller(cp, r, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rej := &fakeCounter{}
	p.Rejected = rej
	ctx := context.Background()

	body.Store(empty) // 启动时空表：允许
	if err := p.Sync(ctx); err != nil || !r.Ready() || len(r.Models()) != 0 {
		t.Fatalf("initial empty table should load: err=%v ready=%v", err, r.Ready())
	}
	body.Store(full)
	if err := p.Sync(ctx); err != nil || len(r.Models()) != 1 {
		t.Fatalf("full table: err=%v models=%v", err, r.Models())
	}
	body.Store(empty) // 已有非空快照后再来空表：拒绝
	err := p.Sync(ctx)
	if err != ErrEmptyRoutes {
		t.Fatalf("want ErrEmptyRoutes, got %v", err)
	}
	if len(r.Models()) != 1 || r.Version() != "v1" {
		t.Errorf("snapshot must be kept: models=%v version=%s", r.Models(), r.Version())
	}
	if rej.n.Load() != 1 {
		t.Errorf("rejected counter = %d, want 1", rej.n.Load())
	}
	body.Store(full)
	if err := p.Sync(ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	// 第 4 次请求带的 If-None-Match 仍是 v1：被拒的空表没有推进 ETag
	if len(etags) != 4 || etags[3] != "v1" {
		t.Errorf("etag sequence %v, want the 4th poll to still carry v1", etags)
	}
}
