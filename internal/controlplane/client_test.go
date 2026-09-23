package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const testTokenHeader = "X-HIGRESS-Token"

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return c
}

func okEnvelope(data string) string {
	return `{"code":"00000","msg":"ok","data":` + data + `}`
}

// N2: a 429 (or 408) from the control plane on usage/report must be classified as
// retryable so the metering worker backs off instead of dropping the record after one try.
func TestReportUsageTransientStatusesAreRetryable(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(okEnvelope(`{"accepted":true,"duplicate":false,"message":"ok"}`)))
			}))
			defer srv.Close()
			c := New(srv.URL, "tok", testTokenHeader)

			_, err := c.ReportUsage(ctx(t), UsageReport{RequestID: "r1"})
			if err == nil {
				t.Fatalf("first call should fail with status %d", status)
			}
			if !IsRetryable(err) {
				t.Fatalf("status %d must be retryable, got terminal error: %v", status, err)
			}
			// Second attempt (what the metering worker does after backoff) succeeds.
			res, err := c.ReportUsage(ctx(t), UsageReport{RequestID: "r1"})
			if err != nil || !res.Accepted {
				t.Fatalf("retry should succeed, got res=%+v err=%v", res, err)
			}
		})
	}

	// Sanity: a plain 4xx (e.g. 401 token rotated) stays terminal so it is dropped once, not retried.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	_, err := New(srv.URL, "tok", testTokenHeader).ReportUsage(ctx(t), UsageReport{RequestID: "r2"})
	if err == nil || IsRetryable(err) {
		t.Fatalf("401 must be a terminal error, got %v", err)
	}
}

// N3: the control-plane client must not follow redirects. Go's default client forwards custom
// headers (our privileged token) to the redirect target and replays the POST body on 307/308.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var leakedToken atomic.Value // string; token header value seen by the "attacker" host
	leakedToken.Store("")
	var bodySeen atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakedToken.Store(r.Header.Get(testTokenHeader))
		if r.ContentLength > 0 {
			bodySeen.Store(true)
		}
		_, _ = w.Write([]byte(okEnvelope(`{"valid":true}`)))
	}))
	defer evil.Close()

	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			leakedToken.Store("")
			bodySeen.Store(false)
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, evil.URL+"/admin/gateway/key-auth", code)
			}))
			defer cp.Close()
			c := New(cp.URL, "super-secret-token", testTokenHeader)

			res, err := c.KeyAuth(ctx(t), "sk-customer", "m")
			if err == nil {
				t.Fatalf("redirect must surface as an error (fail-closed), got result %+v", res)
			}
			if got := leakedToken.Load().(string); got != "" {
				t.Fatalf("token leaked to redirect target: %q", got)
			}
			if bodySeen.Load() {
				t.Fatal("request body (customer apiKey) replayed to redirect target")
			}
		})
	}

	// Same guarantee for the GET path.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/admin/gateway/model-routes", http.StatusFound)
	}))
	defer cp.Close()
	leakedToken.Store("")
	if _, _, err := New(cp.URL, "super-secret-token", testTokenHeader).FetchRoutes(ctx(t), ""); err == nil {
		t.Fatal("FetchRoutes must not follow redirects")
	}
	if got := leakedToken.Load().(string); got != "" {
		t.Fatalf("token leaked on FetchRoutes redirect: %q", got)
	}
}

// N4: model-routes must contain a "models" key. An ApiResult-wrapped body (the shape the other
// two endpoints use) or any unrelated JSON object previously decoded as an empty table, and at
// startup the empty-table guard does not apply — the gateway would come up Ready with 0 models.
func TestFetchRoutesRequiresModelsField(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
		wantLen int
	}{
		{"wrapped in ApiResult → error", okEnvelope(`{"version":"v1","models":[{"modelCode":"m","providers":[]}]}`), true, 0},
		{"unrelated object → error", `{"status":"ok"}`, true, 0},
		{"empty object → error", `{}`, true, 0},
		{"explicit empty models → ok (legit empty table)", `{"version":"v1","models":[]}`, false, 0},
		{"explicit null models → ok (key present, empty table)", `{"version":"v1","models":null}`, false, 0},
		{"models of wrong type → error", `{"version":"v1","models":"oops"}`, true, 0},
		{"normal → ok", `{"version":"v2","models":[{"modelCode":"m","providers":[{"providerCode":"p","providerModelCode":"pm","priority":10,"weight":100}]}]}`, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			routes, _, err := New(srv.URL, "tok", testTokenHeader).FetchRoutes(ctx(t), "")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for body %s, got routes %+v", tc.body, routes)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(routes.Models) != tc.wantLen {
				t.Fatalf("want %d models, got %d", tc.wantLen, len(routes.Models))
			}
		})
	}
}

// N6: fields the gateway does not use are not decoded, so a control plane that serialises
// them in an unexpected type (e.g. remainingQuotaUsd as a decimal string) cannot break
// key-auth for every request.
func TestKeyAuthIgnoresUnusedFieldsOfAnyType(t *testing.T) {
	data := map[string]any{
		"valid":             true,
		"subjectCode":       "user@example.com",
		"modelCode":         "m",
		"keyCode":           12345,            // number instead of string
		"subjectType":       []string{"USER"}, // array instead of string
		"remainingQuotaUsd": "12.50",          // string instead of number (Java BigDecimal style)
		"someFutureField":   map[string]any{"x": 1},
	}
	raw, _ := json.Marshal(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(okEnvelope(string(raw))))
	}))
	defer srv.Close()

	res, err := New(srv.URL, "tok", testTokenHeader).KeyAuth(ctx(t), "sk-x", "m")
	if err != nil {
		t.Fatalf("key-auth must tolerate unused fields of any type, got %v", err)
	}
	if !res.Valid || res.SubjectCode != "user@example.com" {
		t.Fatalf("used fields decoded wrong: %+v", res)
	}
}
