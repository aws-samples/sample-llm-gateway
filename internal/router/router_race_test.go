package router

import (
	"runtime"
	"sync"
	"testing"

	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
)

// 生产里每个请求 goroutine 都会调 Attempts；同一优先级多候选时会走加权随机。
// 用 -race 跑这条能看出随机数生成器是否被并发共享。
func TestAttemptsConcurrentWithReload(t *testing.T) {
	r := New()
	routes := func(v string) *controlplane.Routes {
		return &controlplane.Routes{Version: v, Models: []controlplane.ModelRoute{{
			ModelCode: "m", Providers: []controlplane.ProviderRoute{
				{ProviderCode: "a", ProviderModelCode: "a.m", Priority: 1, Weight: 10},
				{ProviderCode: "b", ProviderModelCode: "b.m", Priority: 1, Weight: 20},
				{ProviderCode: "c", ProviderModelCode: "c.m", Priority: 1, Weight: 30},
				{ProviderCode: "d", ProviderModelCode: "d.m", Priority: 2, Weight: 1},
			}}}}
	}
	r.Load(routes("v1"))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	swapperDone := make(chan struct{})
	go func() { // 模拟 poller 热更新
		defer close(swapperDone)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				r.Load(routes("v" + string(rune('0'+i%10))))
				runtime.Gosched()
			}
		}
	}()
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				out := r.Attempts("m")
				if len(out) != 4 || out[3].ProviderCode != "d" {
					t.Errorf("bad attempts: %+v", out)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-swapperDone
}
