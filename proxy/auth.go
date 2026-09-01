package proxy

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"llmproxy/config"
)

// ctxKey is the private type for context values set by the proxy.
type ctxKey string

// ctxKeyClient carries the authenticated client key name across the call.
const ctxKeyClient ctxKey = "clientKey"

// clientKeyFrom reads the client key name from a request context ("" if none).
func clientKeyFrom(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKeyClient).(string); ok {
		return v
	}
	return ""
}

// writeAuthError renders an auth failure as an OpenAI-style JSON error with 401,
// so callers see a consistent, well-formed body (transparency is preserved for
// upstream responses; this is the proxy's own gate).
func writeAuthError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":{"message":"` + jsonEscape(err.Error()) + `","type":"invalid_request_error","code":"invalid_api_key"}}`))
}

// Auth error reasons, surfaced to the caller as an OpenAI-style error body.
var (
	errNoKey       = errors.New("missing API key")
	errBadKey      = errors.New("invalid API key")
	errKeyExpired  = errors.New("API key expired")
	errKeyQuota    = errors.New("API key token quota exceeded")
	errKeyDisabled = errors.New("API key disabled")
)

// authenticateClient validates the caller's Authorization against the configured
// client keys. Returns the matching key's name, or an error describing why the
// call is rejected. When no client keys are configured at all, authentication is
// skipped (open mode) so the proxy still works before any key is set up.
func (p *Proxy) authenticateClient(authHeader string) (string, error) {
	presented := extractBearer(authHeader)

	// Copy the matched key by value while holding the lock: UsedTokens is
	// written by addUsedTokens on every proxied call, so a pointer into the live
	// slice would be read unlocked below.
	p.mu.RLock()
	hasAny := len(p.cfg.ClientKeys) > 0
	var match *config.ClientKey
	if presented != "" {
		for i := range p.cfg.ClientKeys {
			if p.cfg.ClientKeys[i].Key == presented {
				k := p.cfg.ClientKeys[i]
				match = &k
				break
			}
		}
	}
	p.mu.RUnlock()

	if !hasAny {
		return "", nil // open mode: no keys configured yet
	}
	if presented == "" {
		return "", errNoKey
	}
	if match == nil {
		return "", errBadKey
	}
	if !match.Enabled {
		return "", errKeyDisabled
	}
	if match.ExpiresAt != 0 && time.Now().Unix() >= match.ExpiresAt {
		return "", errKeyExpired
	}
	if match.TokenQuota != 0 && match.UsedTokens >= match.TokenQuota {
		return "", errKeyQuota
	}
	return match.Name, nil
}

// addUsedTokens adds n tokens to the named client key's usage counter and
// persists the config. No-op when name is empty (open mode) or n <= 0.
func (p *Proxy) addUsedTokens(name string, n int64) {
	if name == "" || n <= 0 {
		return
	}
	p.mu.Lock()
	for i := range p.cfg.ClientKeys {
		if p.cfg.ClientKeys[i].Name == name {
			p.cfg.ClientKeys[i].UsedTokens += n
			break
		}
	}
	_ = p.saveLocked()
	p.mu.Unlock()
}

// extractBearer pulls the token out of an "Authorization: Bearer <t>" header,
// tolerating a raw token without the Bearer prefix.
func extractBearer(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}
