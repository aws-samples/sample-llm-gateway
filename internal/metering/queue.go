// Package metering buffers usage reports and delivers them asynchronously with retries.
package metering

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
	"github.com/aws-samples/sample-llm-gateway/internal/observability"
)

type Queue struct {
	ch         chan controlplane.UsageReport
	cp         *controlplane.Client
	workers    int
	maxRetries int
	timeout    time.Duration
	log        *slog.Logger
	metrics    *observability.Metrics
	wg         sync.WaitGroup
	// done signals shutdown. The data channel q.ch is NEVER closed: Enqueue is called
	// concurrently by request goroutines while Shutdown runs on another goroutine, and
	// sending on a closed channel panics (a send on a closed channel counts as a "ready"
	// select case whose only outcome is panic, so a select can't guard against it). Using
	// a separate close-only signal removes the "send on closed channel" case entirely.
	done      chan struct{}
	closeOnce sync.Once
}

func New(cp *controlplane.Client, size, workers, maxRetries int, timeout time.Duration, log *slog.Logger, m *observability.Metrics) *Queue {
	return &Queue{
		ch: make(chan controlplane.UsageReport, size), cp: cp, workers: workers,
		maxRetries: maxRetries, timeout: timeout, log: log, metrics: m,
		done: make(chan struct{}),
	}
}

// Enqueue adds a report without blocking. Returns false when the queue is full (counts a
// drop) or when the queue is shutting down (not a drop). q.ch is never closed, so none of
// the select cases can panic even if this is called concurrently with Shutdown.
func (q *Queue) Enqueue(r controlplane.UsageReport) bool {
	// Fast path: once shutting down, reject deterministically. Without this, a select over
	// {send, <-done} with a non-full buffer would pick the send branch ~half the time, so
	// records would keep landing in a queue workers may already have stopped draining.
	select {
	case <-q.done:
		return false
	default:
	}
	select {
	case q.ch <- r:
		q.metrics.MeteringQueue.Set(float64(len(q.ch)))
		return true
	case <-q.done:
		// Shutting down (raced the fast path): stop accepting. Not a capacity drop.
		return false
	default:
		q.metrics.MeteringReports.WithLabelValues("dropped").Inc()
		q.log.Error("metering queue full, report dropped", "request_id", r.RequestID)
		return false
	}
}

// Start launches the workers.
func (q *Queue) Start() {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
}

// Shutdown signals workers to stop and waits for in-flight reports up to the deadline.
// It only closes the done signal (never q.ch), so a late Enqueue cannot panic.
func (q *Queue) Shutdown(ctx context.Context) {
	q.closeOnce.Do(func() { close(q.done) })
	finished := make(chan struct{})
	go func() { q.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		// Ran out of the flush budget; remaining buffered records are effectively dropped.
		q.log.Warn("metering shutdown deadline reached", "pending", len(q.ch))
	}
}

// worker delivers reports until shutdown. On shutdown it drains whatever is already
// buffered (bounded by the Shutdown deadline via ctx on the outer wait) and then exits.
func (q *Queue) worker() {
	defer q.wg.Done()
	for {
		select {
		case r := <-q.ch:
			q.metrics.MeteringQueue.Set(float64(len(q.ch)))
			q.deliver(r)
		case <-q.done:
			for {
				select {
				case r := <-q.ch:
					q.metrics.MeteringQueue.Set(float64(len(q.ch)))
					q.deliver(r)
				default:
					return
				}
			}
		}
	}
}

func (q *Queue) deliver(r controlplane.UsageReport) {
	backoff := 200 * time.Millisecond
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), q.timeout)
		res, err := q.cp.ReportUsage(ctx, r)
		cancel()
		if err == nil {
			switch {
			case res.Duplicate:
				q.metrics.MeteringReports.WithLabelValues("duplicate").Inc()
			case res.Accepted:
				q.metrics.MeteringReports.WithLabelValues("ok").Inc()
			default:
				q.metrics.MeteringReports.WithLabelValues("rejected").Inc()
				q.log.Warn("usage report rejected by control plane", "request_id", r.RequestID, "message", res.Message)
			}
			return
		}
		if !controlplane.IsRetryable(err) || attempt >= q.maxRetries {
			q.metrics.MeteringReports.WithLabelValues("dropped").Inc()
			q.log.Error("usage report failed permanently", "request_id", r.RequestID, "attempts", attempt+1, "err", err)
			return
		}
		q.log.Warn("usage report retry", "request_id", r.RequestID, "attempt", attempt+1, "err", err)
		time.Sleep(backoff)
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}
