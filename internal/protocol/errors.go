package protocol

import (
	"encoding/json"
	"net/http"

	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
)

// GatewayError is an error the gateway itself produces (not an upstream body).
type GatewayError struct {
	Status  int
	Type    string // protocol-neutral error type slug
	Code    string // optional machine code (OpenAI "code" field)
	Message string
}

func (e *GatewayError) Error() string { return e.Message }

// Common constructors.
func ErrUnauthorized(msg string) *GatewayError {
	return &GatewayError{Status: http.StatusUnauthorized, Type: "authentication_error", Code: "invalid_api_key", Message: msg}
}
func ErrBadRequest(msg string) *GatewayError {
	return &GatewayError{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: msg}
}
func ErrNotFound(msg string) *GatewayError {
	return &GatewayError{Status: http.StatusNotFound, Type: "not_found_error", Code: "model_not_found", Message: msg}
}
func ErrUnavailable(msg string) *GatewayError {
	return &GatewayError{Status: http.StatusServiceUnavailable, Type: "api_error", Code: "service_unavailable", Message: msg}
}
func ErrBadGateway(msg string) *GatewayError {
	return &GatewayError{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_error", Message: msg}
}

// FromReject maps a control-plane rejection to an HTTP error.
func FromReject(reason controlplane.RejectReason, msg string) *GatewayError {
	if msg == "" {
		msg = string(reason)
	}
	switch reason {
	case controlplane.RejectKeyNotFound, controlplane.RejectKeyDisabled:
		return &GatewayError{Status: http.StatusUnauthorized, Type: "authentication_error", Code: "invalid_api_key", Message: msg}
	case controlplane.RejectModelNotAllowed:
		return &GatewayError{Status: http.StatusForbidden, Type: "permission_error", Code: "model_not_allowed", Message: msg}
	case controlplane.RejectModelNoProvider:
		return &GatewayError{Status: http.StatusNotFound, Type: "not_found_error", Code: "model_not_found", Message: msg}
	case controlplane.RejectRPMExceeded, controlplane.RejectTPMExceeded:
		return &GatewayError{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: msg}
	case controlplane.RejectQuotaExhausted:
		return &GatewayError{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "insufficient_quota", Message: msg}
	}
	return &GatewayError{Status: http.StatusForbidden, Type: "permission_error", Code: string(reason), Message: msg}
}

// Write renders the error in the wire format the client expects.
func (e *GatewayError) Write(w http.ResponseWriter, p Protocol) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	var body any
	if p == Anthropic {
		body = map[string]any{
			"type":  "error",
			"error": map[string]any{"type": e.Type, "message": e.Message},
		}
	} else {
		body = map[string]any{
			"error": map[string]any{"message": e.Message, "type": e.Type, "param": nil, "code": e.Code},
		}
	}
	_ = json.NewEncoder(w).Encode(body)
}
