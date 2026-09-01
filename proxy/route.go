package proxy

import (
	"net/http"
	"strings"

	"llmproxy/config"
)

// detectProtocol decides which upstream group a request belongs to.
// Two complementary signals, in priority order:
//  1. Explicit routing header X-LLMPROXY-Protocol (openai|anthropic) — lets a
//     client force a specific group even if the path is ambiguous.
//  2. Path signature: Anthropic SDK calls /v1/messages and the Anthropic
//     endpoint /v1/complete; everything else is treated as OpenAI.
//
// The request is always forwarded verbatim; this only chooses the upstream
// group, it never rewrites the request.
func detectProtocol(r *http.Request) config.Protocol {
	if v := r.Header.Get("X-LLMPROXY-Protocol"); v != "" {
		if strings.EqualFold(v, string(config.ProtocolAnthropic)) {
			return config.ProtocolAnthropic
		}
		if strings.EqualFold(v, string(config.ProtocolOpenAI)) {
			return config.ProtocolOpenAI
		}
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/v1/messages") || strings.HasPrefix(p, "/v1/complete") {
		return config.ProtocolAnthropic
	}
	return config.ProtocolOpenAI
}

// routePath is the canonical route used for stats/dashboard display.
func routePath(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/v1/chat/completions"):
		return "/v1/chat/completions"
	case strings.HasPrefix(p, "/v1/embeddings"):
		return "/v1/embeddings"
	case strings.HasPrefix(p, "/v1/models"):
		return "/v1/models"
	case strings.HasPrefix(p, "/v1/completions"):
		return "/v1/completions"
	case strings.HasPrefix(p, "/v1/messages"):
		return "/v1/messages"
	default:
		return p
	}
}
