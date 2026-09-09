// Package observability wires structured logging and Prometheus metrics.
package observability

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewLogger returns a JSON slog logger at the given level.
func NewLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// Metrics holds the gateway's Prometheus collectors.
type Metrics struct {
	Requests        *prometheus.CounterVec
	Duration        *prometheus.HistogramVec
	TTFT            *prometheus.HistogramVec
	Tokens          *prometheus.CounterVec
	KeyAuthRejects  *prometheus.CounterVec
	KeyAuthErrors   prometheus.Counter
	Failovers       *prometheus.CounterVec
	MeteringReports *prometheus.CounterVec
	MeteringQueue   prometheus.Gauge
	RoutesModels    prometheus.Gauge
	RoutesRejected  prometheus.Counter
	registry        *prometheus.Registry
}

func NewMetrics() *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_requests_total", Help: "Requests by protocol, provider and status code.",
		}, []string{"protocol", "provider", "status"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgw_request_duration_seconds", Help: "End-to-end request duration.",
			Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 20, 30, 60, 120, 300},
		}, []string{"protocol", "provider", "model"}),
		TTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgw_ttft_seconds", Help: "Time to first byte from upstream.",
			Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 20, 30, 60},
		}, []string{"protocol", "provider", "model"}),
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_tokens_total", Help: "Tokens by kind (input, output, cache_read, cache_write, reasoning).",
		}, []string{"provider", "model", "kind"}),
		KeyAuthRejects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_keyauth_rejects_total", Help: "Key-auth rejections by reason.",
		}, []string{"reason"}),
		KeyAuthErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "llmgw_keyauth_errors_total", Help: "Key-auth calls that failed (control plane unreachable).",
		}),
		Failovers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_upstream_failovers_total", Help: "Upstream attempts abandoned before first byte.",
		}, []string{"provider", "reason"}),
		MeteringReports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_metering_reports_total", Help: "Usage reports by result (ok, duplicate, dropped, rejected).",
		}, []string{"result"}),
		MeteringQueue: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "llmgw_metering_queue_depth", Help: "Pending usage reports.",
		}),
		RoutesModels: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "llmgw_routes_models", Help: "Models in the current route snapshot.",
		}),
		RoutesRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "llmgw_routes_rejected_total", Help: "Route snapshots refused because they were empty while a non-empty snapshot was loaded.",
		}),
		registry: prometheus.NewRegistry(),
	}
	m.registry.MustRegister(m.Requests, m.Duration, m.TTFT, m.Tokens, m.KeyAuthRejects, m.KeyAuthErrors,
		m.Failovers, m.MeteringReports, m.MeteringQueue, m.RoutesModels, m.RoutesRejected)
	m.registry.MustRegister(prometheus.NewGoCollector())
	return m
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
