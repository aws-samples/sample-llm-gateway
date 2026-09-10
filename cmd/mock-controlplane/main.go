// Command mock-controlplane emulates the three gateway-facing endpoints of the
// customer's control plane for local development and cluster smoke tests.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type providerRoute struct {
	ProviderCode      string `json:"providerCode"`
	ProviderModelCode string `json:"providerModelCode"`
	Priority          int    `json:"priority"`
	Weight            int    `json:"weight"`
}
type modelRoute struct {
	ModelCode string          `json:"modelCode"`
	Providers []providerRoute `json:"providers"`
}
type routes struct {
	Version string       `json:"version"`
	Models  []modelRoute `json:"models"`
}

type server struct {
	mu          sync.Mutex
	token       string
	tokenHeader string
	keys        map[string]bool
	routes      routes
	seen        map[string]bool
	usages      []map[string]any
}

func apiResult(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": "00000", "msg": "ok", "data": data})
}

func (s *server) authorized(r *http.Request) bool {
	return r.Header.Get(s.tokenHeader) == s.token
}

func (s *server) setRoutes(rt routes) {
	b, _ := json.Marshal(rt.Models)
	sum := sha256.Sum256(b)
	rt.Version = hex.EncodeToString(sum[:8])
	s.routes = rt
}

func (s *server) modelKnown(code string) bool {
	for _, m := range s.routes.Models {
		if m.ModelCode == code {
			return true
		}
	}
	return false
}

func (s *server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("If-None-Match") == s.routes.Version && s.routes.Version != "" {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", s.routes.Version)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.routes)
}

func (s *server) handleKeyAuth(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		APIKey    string `json:"apiKey"`
		ModelCode string `json:"modelCode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := map[string]any{"valid": false, "modelCode": req.ModelCode}
	switch {
	case !s.keys[req.APIKey]:
		resp["rejectReason"], resp["message"] = "KEY_NOT_FOUND", "api key not found"
	case strings.HasSuffix(req.APIKey, "-disabled"):
		resp["rejectReason"], resp["message"] = "KEY_DISABLED", "api key disabled"
	case strings.HasSuffix(req.APIKey, "-quota0"):
		resp["rejectReason"], resp["message"] = "QUOTA_EXHAUSTED", "monthly quota exhausted"
	case !s.modelKnown(req.ModelCode):
		resp["rejectReason"], resp["message"] = "MODEL_NO_PROVIDER", "model has no active provider"
	default:
		resp["valid"] = true
		resp["keyCode"] = "key-" + maskKey(req.APIKey)
		resp["subjectType"] = "USER"
		resp["subjectCode"] = "demo.user@example.com"
		resp["remainingQuotaUsd"] = 100.0
	}
	log.Printf("key-auth key=...%s model=%s valid=%v reason=%v", maskKey(req.APIKey), req.ModelCode, resp["valid"], resp["rejectReason"])
	apiResult(w, resp)
}

func (s *server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var rec map[string]any
	if err := json.Unmarshal(body, &rec); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	reqID, _ := rec["request_id"].(string)
	apiKey, _ := rec["api_key"].(string)
	s.mu.Lock()
	defer s.mu.Unlock()
	if reqID == "" {
		apiResult(w, map[string]any{"accepted": false, "duplicate": false, "message": "request_id required"})
		return
	}
	if !s.keys[apiKey] {
		apiResult(w, map[string]any{"accepted": false, "duplicate": false, "message": "unknown api key"})
		return
	}
	if s.seen[reqID] {
		apiResult(w, map[string]any{"accepted": true, "duplicate": true, "message": "duplicate"})
		return
	}
	s.seen[reqID] = true
	rec["api_key"] = "..." + maskKey(apiKey)
	s.usages = append(s.usages, rec)
	log.Printf("usage %s", string(mustJSON(rec)))
	apiResult(w, map[string]any{"accepted": true, "duplicate": false, "message": "ok"})
}

func (s *server) handleDebugUsages(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodDelete {
		s.usages = nil
		s.seen = map[string]bool{}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.usages)
}

func (s *server) handleDebugRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT a routes JSON document", http.StatusMethodNotAllowed)
		return
	}
	var rt routes
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.setRoutes(rt)
	v := s.routes.Version
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "routes updated, version=%s\n", v) // nosemgrep: no-fprintf-to-responsewriter, go.lang.security.audit.xss.no-fprintf-to-responsewriter -- 调试接口，明文 text/plain 回显版本号，不是 HTML
}

func main() {
	listen := flag.String("listen", ":9090", "listen address")
	token := flag.String("token", os.Getenv("MOCK_CP_TOKEN"), "value expected in the token header")
	tokenHeader := flag.String("token-header", "X-HIGRESS-Token", "token header name")
	keys := flag.String("keys", os.Getenv("MOCK_CP_KEYS"), "comma-separated valid api keys")
	tokenFile := flag.String("token-file", "", "read the expected token from this file (e.g. a mounted Secret); overrides -token")
	keysFile := flag.String("keys-file", "", "read the comma-separated valid api keys from this file; overrides -keys")
	routesFile := flag.String("routes", "", "routes JSON file (defaults to a built-in Bedrock sample)")
	flag.Parse()
	// File-based secrets let Kubernetes mount them as volumes instead of environment variables.
	if *tokenFile != "" {
		*token = readTrimmed(*tokenFile)
	}
	if *keysFile != "" {
		*keys = readTrimmed(*keysFile)
	}
	if *token == "" {
		*token = "mock-token"
	}
	s := &server{token: *token, tokenHeader: *tokenHeader, keys: map[string]bool{}, seen: map[string]bool{}}
	for _, k := range strings.Split(*keys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			s.keys[k] = true
		}
	}
	if len(s.keys) == 0 {
		s.keys["sk-demo-key"] = true
	}
	rt := defaultRoutes()
	if *routesFile != "" {
		b, err := os.ReadFile(*routesFile)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(b, &rt); err != nil {
			log.Fatal(err)
		}
	}
	s.setRoutes(rt)

	mux := http.NewServeMux()
	mux.HandleFunc("/admin/healthCheck", func(w http.ResponseWriter, _ *http.Request) { apiResult(w, "ok") })
	mux.HandleFunc("/admin/gateway/model-routes", s.handleRoutes)
	mux.HandleFunc("/admin/gateway/key-auth", s.handleKeyAuth)
	mux.HandleFunc("/admin/gateway/usage/report", s.handleUsage)
	mux.HandleFunc("/debug/usages", s.handleDebugUsages)
	mux.HandleFunc("/debug/routes", s.handleDebugRoutes)
	log.Printf("mock control plane listening on %s (models=%d keys=%d version=%s)", *listen, len(s.routes.Models), len(s.keys), s.routes.Version)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// readTrimmed returns the file content without surrounding whitespace; a missing file is fatal.
func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func defaultRoutes() routes {
	one := func(model, provider, pm string) modelRoute {
		return modelRoute{ModelCode: model, Providers: []providerRoute{{ProviderCode: provider, ProviderModelCode: pm, Priority: 10, Weight: 100}}}
	}
	return routes{Models: []modelRoute{
		one("claude-sonnet-5", "bedrock", "global.anthropic.claude-sonnet-5"),
		one("claude-opus-5", "bedrock", "global.anthropic.claude-opus-5"),
		one("gpt-5.6-sol", "bedrock", "global.openai.gpt-5.6-sol"),
		one("gpt-5.6-luna", "bedrock", "global.openai.gpt-5.6-luna"),
	}}
}

// maskKey redacts an api key down to its last 4 characters, the only part that ever reaches a
// log line, the keyCode echoed to the gateway, or the stored usage record.
func maskKey(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
