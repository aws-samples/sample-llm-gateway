package metering

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
	"github.com/aws-samples/sample-llm-gateway/internal/observability"
)

// fakeControlPlane accepts usage reports with the ApiResult envelope the client expects.
func fakeControlPlane() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":"00000","msg":"ok","data":{"accepted":true,"duplicate":false,"message":"ok"}}`)
	}))
}

func testQueue(cp *controlplane.Client) *Queue {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cp, 64, 3, 2, time.Second, log, observability.NewMetrics())
}

// TestShutdownWithConcurrentEnqueueDoesNotPanic reproduces ISSUE-1: request goroutines call
// Enqueue while Shutdown runs (and keep calling after it returns). If the queue closed its
// data channel, a late Enqueue would send on a closed channel and panic, crashing the test.
// q.ch is never closed, so this must complete cleanly.
func TestShutdownWithConcurrentEnqueueDoesNotPanic(t *testing.T) {
	srv := fakeControlPlane()
	defer srv.Close()
	q := testQueue(controlplane.New(srv.URL, "t", "X-HIGRESS-Token"))
	q.Start()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					q.Enqueue(controlplane.UsageReport{RequestID: "req_x"})
				}
			}
		}()
	}

	time.Sleep(20 * time.Millisecond) // let producers fill and workers run
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	q.Shutdown(ctx)
	cancel()
	time.Sleep(20 * time.Millisecond) // producers keep enqueuing AFTER shutdown: must not panic
	close(stop)
	wg.Wait()
}

// TestEnqueueAfterShutdownReturnsFalse confirms a post-shutdown Enqueue is rejected cleanly
// (no panic, returns false) rather than being counted as a capacity drop.
func TestEnqueueAfterShutdownReturnsFalse(t *testing.T) {
	srv := fakeControlPlane()
	defer srv.Close()
	q := testQueue(controlplane.New(srv.URL, "t", "X-HIGRESS-Token"))
	q.Start()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	q.Shutdown(ctx)
	cancel()

	if q.Enqueue(controlplane.UsageReport{RequestID: "after"}) {
		t.Fatal("Enqueue after Shutdown should return false")
	}
}
