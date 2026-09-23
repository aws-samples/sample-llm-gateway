// recordproxy is a development tool: a transparent reverse proxy that sits between a real
// client (Claude Code, Codex, an SDK) and the gateway and records every exchange to disk —
// request headers+body, response headers, and the full response body / SSE byte stream.
//
// The recordings become golden fixtures for internal/protocol/translate: real client traffic
// captures quirks that hand-written JSON never does (see docs/protocol-translation-design.md §9).
//
// Usage:
//
//	go run ./tools/recordproxy -listen :8081 -target http://127.0.0.1:8080 -out testdata/fixtures/claude-code
//
// Each request produces <out>/<seq>-<method>-<path>.req.json / .res.headers / .res.body (or .res.sse
// when the upstream answered with text/event-stream). Bodies are stored byte-for-byte.
// Not part of the shipped gateway.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	listen := flag.String("listen", ":8081", "address to listen on")
	target := flag.String("target", "http://127.0.0.1:8080", "gateway base URL to forward to")
	out := flag.String("out", "recordings", "directory to write recordings into")
	compat := flag.Bool("bedrock-compat", false, "strip fields Bedrock's Anthropic-native endpoint rejects (metadata.user_id charset, output_config) from /v1/messages before forwarding; the ORIGINAL body is still what gets recorded")
	flag.Parse()

	tu, err := url.Parse(*target)
	if err != nil {
		log.Fatalf("bad -target: %v", err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}

	var seq atomic.Int64
	rp := httputil.NewSingleHostReverseProxy(tu)
	rp.FlushInterval = -1 // stream SSE through immediately
	director := rp.Director
	rp.Director = func(r *http.Request) {
		director(r)
		r.Host = tu.Host
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := seq.Add(1)
		base := filepath.Join(*out, fmt.Sprintf("%03d-%s-%s", n, r.Method, sanitize(r.URL.Path)))

		// Capture the request body (recorded verbatim), then hand a replay copy to the proxy.
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		writeFile(base+".req.headers", headersText(r.Method+" "+r.URL.RequestURI(), r.Header))
		writeFile(base+".req.json", body)
		fwd := body
		if *compat && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/responses") {
			// Bedrock's Responses endpoint rejects OpenAI-hosted tool types (web_search); Codex
			// sends one by default. Drop it so the recorded session can complete.
			if b, changed := stripHostedTools(body); changed {
				fwd = b
				writeFile(base+".req.forwarded.json", fwd)
			}
		}
		if *compat && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages") {
			if b, changed := bedrockCompat(body); changed {
				fwd = b
				writeFile(base+".req.forwarded.json", fwd)
			}
			// 3. anthropic-beta: Bedrock rejects the whole request if any listed value is unknown.
			if hv := r.Header.Get("Anthropic-Beta"); hv != "" {
				if filtered := stripUnsupportedBeta(hv); filtered != hv {
					if filtered == "" {
						r.Header.Del("Anthropic-Beta")
					} else {
						r.Header.Set("Anthropic-Beta", filtered)
					}
				}
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(fwd))
		r.ContentLength = int64(len(fwd))
		r.Header.Set("Content-Length", fmt.Sprint(len(fwd)))

		rec := &recorder{ResponseWriter: w, base: base}
		start := time.Now()
		// httputil.ReverseProxy aborts the handler with panic(http.ErrAbortHandler) when the
		// client hangs up before the upstream body is fully copied — agentic clients (Codex) close
		// the connection as soon as they see the terminal event. Persist whatever we captured in a
		// defer so the recording survives that abort, then let the panic propagate (net/http
		// swallows ErrAbortHandler silently).
		defer func() {
			p := recover()
			rec.finish()
			note := ""
			if p != nil {
				note = " (client aborted before upstream EOF)"
			}
			log.Printf("#%03d %s %s -> %d (%s, %d bytes) saved %s%s", n, r.Method, r.URL.Path, rec.status,
				time.Since(start).Round(time.Millisecond), rec.buf.Len(), filepath.Base(base), note)
			if p != nil {
				panic(p)
			}
		}()
		rp.ServeHTTP(rec, r)
	})

	log.Printf("recordproxy listening on %s -> %s, writing to %s", *listen, *target, *out)
	log.Fatal(http.ListenAndServe(*listen, nil)) // nosemgrep: go.lang.security.audit.net.use-tls.use-tls -- local dev tool, loopback only
}

// recorder tees the response to a buffer while streaming it through to the client.
type recorder struct {
	http.ResponseWriter
	base   string
	status int
	buf    bytes.Buffer
	isSSE  bool
	hdrDne bool
}

func (rc *recorder) WriteHeader(code int) {
	rc.status = code
	if !rc.hdrDne {
		rc.hdrDne = true
		ct := rc.Header().Get("Content-Type")
		rc.isSSE = strings.HasPrefix(strings.ToLower(ct), "text/event-stream")
		writeFile(rc.base+".res.headers", headersText(fmt.Sprintf("HTTP %d", code), rc.Header()))
	}
	rc.ResponseWriter.WriteHeader(code)
}

func (rc *recorder) Write(p []byte) (int, error) {
	if !rc.hdrDne {
		rc.WriteHeader(http.StatusOK)
	}
	rc.buf.Write(p)
	n, err := rc.ResponseWriter.Write(p)
	if f, ok := rc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func (rc *recorder) Flush() {
	if f, ok := rc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rc *recorder) finish() {
	ext := ".res.body"
	if rc.isSSE {
		ext = ".res.sse"
	}
	writeFile(rc.base+ext, rc.buf.Bytes())
}

func headersText(first string, h http.Header) []byte {
	var b strings.Builder
	b.WriteString(first + "\n")
	for k, vs := range h {
		for _, v := range vs {
			// Never persist credentials.
			lk := strings.ToLower(k)
			if lk == "authorization" || lk == "x-api-key" || strings.Contains(lk, "token") {
				v = "<redacted>"
			}
			b.WriteString(k + ": " + v + "\n")
		}
	}
	return []byte(b.String())
}

func writeFile(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("write %s: %v", path, err)
	}
}

// bedrockCompat applies the two workarounds discovered in M3 (design doc §8b) so real Claude
// Code sessions can complete against Bedrock while we record them. Recording-only; the gateway's
// own provider-compat layer is designed separately.
func bedrockCompat(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	changed := false
	// 1. metadata.user_id must match Bedrock's charset; Claude Code sends a JSON string.
	if raw, ok := m["metadata"]; ok {
		var md map[string]json.RawMessage
		if json.Unmarshal(raw, &md) == nil {
			if uid, ok := md["user_id"]; ok {
				var s string
				if json.Unmarshal(uid, &s) == nil && !validUserID(s) {
					sum := sha256.Sum256([]byte(s))
					b, _ := json.Marshal(hex.EncodeToString(sum[:]))
					md["user_id"] = b
					m["metadata"], _ = json.Marshal(md)
					changed = true
				}
			}
		}
	}
	// 2. output_config (structured-output beta) is not accepted by Bedrock yet.
	if _, ok := m["output_config"]; ok {
		delete(m, "output_config")
		changed = true
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// bedrockUnsupportedBeta lists anthropic-beta values Bedrock's native endpoint rejected in M3
// probing (each of Claude Code's 8 values tested alone; only this one fails).
var bedrockUnsupportedBeta = map[string]bool{
	"prompt-caching-scope-2026-01-05": true,
}

func stripUnsupportedBeta(hv string) string {
	var keep []string
	for _, v := range strings.Split(hv, ",") {
		v = strings.TrimSpace(v)
		if v != "" && !bedrockUnsupportedBeta[v] {
			keep = append(keep, v)
		}
	}
	return strings.Join(keep, ",")
}

// stripHostedTools removes Responses `tools` entries whose type Bedrock does not serve
// (OpenAI-hosted tools). Probed in M3: only web_search fails; function and namespace pass.
func stripHostedTools(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	raw, ok := m["tools"]
	if !ok {
		return body, false
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return body, false
	}
	keep := tools[:0]
	changed := false
	for _, t := range tools {
		var typ string
		_ = json.Unmarshal(t["type"], &typ)
		if typ == "web_search" || typ == "web_search_preview" {
			changed = true
			continue
		}
		keep = append(keep, t)
	}
	if !changed {
		return body, false
	}
	m["tools"], _ = json.Marshal(keep)
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

func validUserID(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.', c == '@':
		default:
			return false
		}
	}
	return true
}

func sanitize(p string) string {
	p = strings.Trim(p, "/")
	p = strings.ReplaceAll(p, "/", "_")
	if p == "" {
		p = "root"
	}
	return p
}
