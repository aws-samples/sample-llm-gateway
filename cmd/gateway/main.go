// Command gateway is a lightweight pass-through LLM proxy that integrates with the
// customer's model-gateway control plane for key-auth, routing and metering.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
	"github.com/aws-samples/sample-llm-gateway/internal/metering"
	"github.com/aws-samples/sample-llm-gateway/internal/observability"
	"github.com/aws-samples/sample-llm-gateway/internal/provider"
	"github.com/aws-samples/sample-llm-gateway/internal/proxy"
	"github.com/aws-samples/sample-llm-gateway/internal/router"
	"github.com/aws-samples/sample-llm-gateway/internal/secrets"
)

func main() {
	cfgPath := flag.String("config", "configs/gateway.yaml", "path to YAML config")
	healthcheck := flag.Bool("healthcheck", false, "probe the running gateway's /healthz on the listen port from -config and exit 0/1 (for Docker HEALTHCHECK; the image has no curl)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	if *healthcheck {
		os.Exit(healthCheck(cfg.Server.Listen))
	}
	log := observability.NewLogger(cfg.Server.LogLevel)
	slog.SetDefault(log) // packages without an injected logger (provider startup) log through the same JSON handler
	if strings.HasPrefix(cfg.ControlPlane.BaseURL, "http://") {
		log.Warn("control_plane.base_url uses plaintext http (allow_insecure); control-plane token and customer api keys are sent in cleartext", "base_url", cfg.ControlPlane.BaseURL)
	}
	metrics := observability.NewMetrics()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Secret fields written as secretsmanager:// references are fetched once here, before
	// anything that uses them is built. Plaintext configs never touch Secrets Manager.
	if secrets.HasRefs(cfg) {
		res, err := secrets.New(ctx, cfg.Secrets, log)
		if err != nil {
			log.Error("secrets manager setup failed", "err", err)
			os.Exit(2)
		}
		if err := res.Resolve(ctx, cfg); err != nil {
			log.Error("secret resolution failed", "err", err)
			os.Exit(2)
		}
	}

	reg, err := provider.Build(ctx, cfg.Providers)
	if err != nil {
		log.Error("provider setup failed", "err", err)
		os.Exit(2)
	}
	cp := controlplane.New(cfg.ControlPlane.BaseURL, cfg.ControlPlane.Token, cfg.ControlPlane.TokenHeader)
	rt := router.New()
	poller := router.NewPoller(cp, rt, cfg.ControlPlane.RoutesPollInterval, log)
	poller.Rejected = metrics.RoutesRejected

	// Initial route load: retry for up to 60s, then give up so the orchestrator restarts us.
	deadline := time.Now().Add(60 * time.Second)
	for {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := poller.Sync(sctx)
		cancel()
		if err == nil && rt.Ready() {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			log.Error("could not load initial routes", "err", err)
			os.Exit(1)
		}
		log.Warn("initial routes load failed, retrying", "err", err)
		time.Sleep(3 * time.Second)
	}
	metrics.RoutesModels.Set(float64(len(rt.Models())))
	go func() {
		poller.Run(ctx)
	}()
	go func() {
		t := time.NewTicker(cfg.ControlPlane.RoutesPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				metrics.RoutesModels.Set(float64(len(rt.Models())))
			}
		}
	}()

	meter := metering.New(cp, cfg.Metering.QueueSize, cfg.Metering.Workers, cfg.Metering.MaxRetries,
		cfg.ControlPlane.ReportTimeout, log, metrics)
	meter.Start()

	h := proxy.New(cfg, cp, rt, reg, meter, metrics, log)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// draining flips on SIGTERM so the readiness probe fails and the Service stops sending
	// new connections here before the listener closes.
	var draining atomic.Bool
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		if !rt.Ready() {
			http.Error(w, "routes not loaded", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/v1/models", h.ModelsHandler)
	mux.Handle("/", h)

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout, // bounds a slow request-body upload; does not apply to the response
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: streaming responses are bounded by server.request_timeout via context.
	}
	go func() {
		log.Info("gateway listening", "addr", cfg.Server.Listen, "providers", len(reg), "models", len(rt.Models()), "routes_version", rt.Version())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	// Shutdown sequence, sized to fit terminationGracePeriodSeconds:
	//   1. readiness -> 503 and wait drainDelay so endpoints controllers stop routing to us
	//   2. close the listener and wait up to shutdown_timeout for in-flight requests
	//   3. flush the metering queue with its own budget, so a long HTTP drain cannot starve it
	log.Info("shutting down", "drain_delay", drainDelay.String(), "shutdown_timeout", cfg.Server.ShutdownTimeout.String())
	draining.Store(true)
	time.Sleep(drainDelay)
	shCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	if err := srv.Shutdown(shCtx); err != nil {
		log.Warn("shutdown deadline reached, in-flight requests cut", "err", err)
	}
	cancel()
	mCtx, mCancel := context.WithTimeout(context.Background(), meteringFlushTimeout)
	meter.Shutdown(mCtx)
	mCancel()
	log.Info("bye")
}

// healthCheck GETs /healthz on the local listen port and returns a process exit code.
func healthCheck(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: bad listen address:", err)
		return 1
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

const (
	drainDelay           = 5 * time.Second
	meteringFlushTimeout = 10 * time.Second
)
