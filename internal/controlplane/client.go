// Package controlplane talks to the customer's model-gateway management service:
// route snapshot pulling (ETag), key-auth gate and usage reporting.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProviderRoute is one provider candidate for a model.
type ProviderRoute struct {
	ProviderCode      string `json:"providerCode"`
	ProviderModelCode string `json:"providerModelCode"`
	Priority          int    `json:"priority"`
	Weight            int    `json:"weight"`
}

// ModelRoute maps a user-visible model code to its providers.
type ModelRoute struct {
	ModelCode string          `json:"modelCode"`
	Providers []ProviderRoute `json:"providers"`
}

// Routes is the unwrapped body of GET /admin/gateway/model-routes.
type Routes struct {
	Version string       `json:"version"`
	Models  []ModelRoute `json:"models"`
}

// RejectReason mirrors PrecheckRejectReason.
type RejectReason string

const (
	RejectKeyNotFound     RejectReason = "KEY_NOT_FOUND"
	RejectKeyDisabled     RejectReason = "KEY_DISABLED"
	RejectModelNotAllowed RejectReason = "MODEL_NOT_ALLOWED"
	RejectModelNoProvider RejectReason = "MODEL_NO_PROVIDER"
	RejectRPMExceeded     RejectReason = "RPM_EXCEEDED"
	RejectTPMExceeded     RejectReason = "TPM_EXCEEDED"
	RejectQuotaExhausted  RejectReason = "QUOTA_EXHAUSTED"
)

// KeyAuthResult is the data part of the key-auth response.
type KeyAuthResult struct {
	Valid             bool         `json:"valid"`
	RejectReason      RejectReason `json:"rejectReason"`
	Message           string       `json:"message"`
	KeyCode           string       `json:"keyCode"`
	SubjectType       string       `json:"subjectType"`
	SubjectCode       string       `json:"subjectCode"`
	ModelCode         string       `json:"modelCode"`
	RemainingQuotaUSD float64      `json:"remainingQuotaUsd"`
}

// UsageReport is the snake_case body of POST /admin/gateway/usage/report.
type UsageReport struct {
	RequestID         string `json:"request_id"`
	APIKey            string `json:"api_key"`
	ModelCode         string `json:"model_code"`
	ProviderModelCode string `json:"provider_model_code"`
	StartTime         string `json:"start_time"`
	Duration          int64  `json:"duration"`
	TTFT              int64  `json:"ttft"`
	StatusCode        int    `json:"status_code"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	TotalTokens       int64  `json:"total_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
}

// ReportResult is the data part of the usage report response.
type ReportResult struct {
	Accepted  bool   `json:"accepted"`
	Duplicate bool   `json:"duplicate"`
	Message   string `json:"message"`
}

type apiResult struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

const successCode = "00000"

// Client is a thin HTTP client for the control plane.
type Client struct {
	baseURL     string
	token       string
	tokenHeader string
	http        *http.Client
}

func New(baseURL, token, tokenHeader string) *Client {
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		token:       token,
		tokenHeader: tokenHeader,
		http: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ErrNotModified is returned by FetchRoutes when the ETag matched.
var ErrNotModified = errors.New("routes not modified")

// FetchRoutes pulls the full route table. Pass the last ETag to get ErrNotModified on 304.
func (c *Client) FetchRoutes(ctx context.Context, etag string) (*Routes, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/admin/gateway/model-routes", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(c.tokenHeader, c.token)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("model-routes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		io.Copy(io.Discard, resp.Body)
		return nil, etag, ErrNotModified
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", fmt.Errorf("model-routes: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("model-routes: status %d: %s", resp.StatusCode, truncate(body))
	}
	var r Routes
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, "", fmt.Errorf("model-routes: decode: %w", err)
	}
	newETag := resp.Header.Get("ETag")
	if newETag == "" {
		newETag = r.Version
	}
	return &r, newETag, nil
}

// KeyAuth runs the merged gate (key + model whitelist + RPM/TPM + quota).
// A transport or control-plane error is returned as err; a business rejection
// comes back as result.Valid == false with a RejectReason.
func (c *Client) KeyAuth(ctx context.Context, apiKey, modelCode string) (*KeyAuthResult, error) {
	payload, _ := json.Marshal(map[string]string{"apiKey": apiKey, "modelCode": modelCode})
	var out KeyAuthResult
	if err := c.postJSON(ctx, "/admin/gateway/key-auth", payload, &out); err != nil {
		return nil, fmt.Errorf("key-auth: %w", err)
	}
	return &out, nil
}

// ReportUsage posts one metering record. Retryable transport/5xx errors are returned
// as *RetryableError so the caller can back off; business rejections are terminal.
func (c *Client) ReportUsage(ctx context.Context, r UsageReport) (*ReportResult, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var out ReportResult
	if err := c.postJSON(ctx, "/admin/gateway/usage/report", payload, &out); err != nil {
		return nil, fmt.Errorf("usage-report: %w", err)
	}
	return &out, nil
}

// RetryableError marks failures worth retrying (network, timeout, 5xx).
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// IsRetryable reports whether err should be retried.
func IsRetryable(err error) bool {
	var r *RetryableError
	return errors.As(err, &r)
}

func (c *Client) postJSON(ctx context.Context, path string, payload []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(c.tokenHeader, c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return &RetryableError{Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &RetryableError{Err: err}
	}
	if resp.StatusCode >= 500 {
		return &RetryableError{Err: fmt.Errorf("status %d: %s", resp.StatusCode, truncate(body))}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(body))
	}
	var env apiResult
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decode envelope: %w (%s)", err, truncate(body))
	}
	if env.Code != "" && env.Code != successCode {
		return fmt.Errorf("business code %s: %s", env.Code, env.Msg)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("empty data in response: %s", truncate(body))
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode data: %w", err)
	}
	return nil
}

func truncate(b []byte) string {
	const n = 300
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
