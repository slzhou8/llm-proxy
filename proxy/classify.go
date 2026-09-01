package proxy

import (
	"errors"
	"net"
	"net/http"
	"strings"
)

// ErrClass is the retryable-category of an upstream failure.
type ErrClass string

const (
	ErrNone      ErrClass = ""             // success
	ErrRateLimit ErrClass = "rate_limit"   // 429
	ErrServer    ErrClass = "server_error" // 5xx
	ErrTimeout   ErrClass = "timeout"
	ErrNetwork   ErrClass = "network"  // connection refused / reset / DNS
	ErrAuth      ErrClass = "auth"     // 401/403: this upstream's key is rejected
	ErrBusiness  ErrClass = "business" // 400/404 etc: caller's fault -> pass through
)

// classifyResponse maps an upstream HTTP status to an ErrClass.
func classifyResponse(status int) ErrClass {
	switch {
	case status == http.StatusTooManyRequests: // 429
		return ErrRateLimit
	case status >= 500:
		return ErrServer
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// The proxy always sends the upstream's own configured key, so a 401/403
		// means that key is bad -- another upstream may still work.
		return ErrAuth
	case status >= 400:
		return ErrBusiness
	default:
		return ErrNone
	}
}

// classifyTransport inspects a transport-level error (not an HTTP status).
func classifyTransport(err error) ErrClass {
	if err == nil {
		return ErrNone
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return ErrTimeout
	case errors.Is(err, net.ErrClosed) ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "EOF"):
		return ErrNetwork
	default:
		return ErrNetwork
	}
}
