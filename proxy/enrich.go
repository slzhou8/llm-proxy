package proxy

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// enrichment holds the fields we best-effort parse out of an upstream response.
type enrichment struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	RealModel        string
	Provider         string
}

// usageShape matches the common OpenAI/Anthropic-style response fields we care
// about. Everything is optional; missing fields stay zero/empty.
type usageShape struct {
	Model string `json:"model"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		// Anthropic naming
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	RoutedVia struct {
		Platform string `json:"platform"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"_routed_via"`
}

// parseBody extracts enrichment from a complete (non-streaming) JSON body.
func parseBody(body []byte) enrichment {
	var s usageShape
	if err := json.Unmarshal(body, &s); err != nil {
		return enrichment{}
	}
	return fromShape(s)
}

// parseSSE extracts enrichment from an SSE stream's accumulated data lines.
// It scans every `data:` payload and keeps the last usage/model it sees, since
// usage typically arrives in the final chunk. `[DONE]` and non-JSON lines are
// skipped.
func parseSSE(dataLines [][]byte) enrichment {
	var out enrichment
	for _, line := range dataLines {
		payload := line
		if i := indexData(line); i >= 0 {
			payload = line[i:]
		}
		payload = trimSpace(payload)
		if len(payload) == 0 || string(payload) == "[DONE]" || payload[0] != '{' {
			continue
		}
		var s usageShape
		if json.Unmarshal(payload, &s) != nil {
			continue
		}
		e := fromShape(s)
		// Merge: a later chunk with usage/model overrides earlier empties.
		if e.RealModel != "" {
			out.RealModel = e.RealModel
		}
		if e.Provider != "" {
			out.Provider = e.Provider
		}
		if e.TotalTokens > 0 || e.PromptTokens > 0 || e.CompletionTokens > 0 {
			out.PromptTokens = e.PromptTokens
			out.CompletionTokens = e.CompletionTokens
			out.TotalTokens = e.TotalTokens
		}
	}
	return out
}

func fromShape(s usageShape) enrichment {
	e := enrichment{
		PromptTokens:     s.Usage.PromptTokens,
		CompletionTokens: s.Usage.CompletionTokens,
		TotalTokens:      s.Usage.TotalTokens,
		RealModel:        s.Model,
	}
	// Anthropic-style token names.
	if e.PromptTokens == 0 && s.Usage.InputTokens > 0 {
		e.PromptTokens = s.Usage.InputTokens
	}
	if e.CompletionTokens == 0 && s.Usage.OutputTokens > 0 {
		e.CompletionTokens = s.Usage.OutputTokens
	}
	if e.TotalTokens == 0 && (e.PromptTokens > 0 || e.CompletionTokens > 0) {
		e.TotalTokens = e.PromptTokens + e.CompletionTokens
	}
	// Provider / real model from _routed_via, if present.
	if s.RoutedVia.Platform != "" {
		e.Provider = s.RoutedVia.Platform
	} else if s.RoutedVia.Provider != "" {
		e.Provider = s.RoutedVia.Provider
	}
	// Use the top-level model for RealModel (it is the canonical value). The
	// _routed_via.model may carry routing suffixes (e.g. ":free") that we do not
	// want to show, so it only fills RealModel when the top-level model is
	// absent.
	if e.RealModel == "" && s.RoutedVia.Model != "" {
		e.RealModel = s.RoutedVia.Model
	}
	// Fallback: when the upstream omits _routed_via (e.g. streaming responses on
	// this upstream), derive the provider from a "vendor/model" style model name.
	// The vendor prefix matches what _routed_via.platform reports on the same
	// upstream, so this is a reliable signal rather than a guess.
	if e.Provider == "" && e.RealModel != "" {
		if i := strings.IndexByte(e.RealModel, '/'); i > 0 {
			e.Provider = e.RealModel[:i]
		}
	}
	return e
}

// clientIP resolves the caller IP, preferring forwarding headers over the raw
// TCP RemoteAddr (proxy may sit behind a gateway/nginx).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// first entry is the original client
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return normalizeIP(strings.TrimSpace(xff[:i]))
		}
		return normalizeIP(strings.TrimSpace(xff))
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return normalizeIP(strings.TrimSpace(xr))
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return normalizeIP(host)
}

// normalizeIP maps the IPv6 loopback to the more recognizable IPv4 loopback so
// local calls show as 127.0.0.1 instead of ::1.
func normalizeIP(host string) string {
	if host == "::1" {
		return "127.0.0.1"
	}
	return host
}

// small byte helpers to avoid importing bytes just for two calls
func indexData(b []byte) int {
	// returns index just after "data:" prefix (with optional space), else -1
	const p = "data:"
	if len(b) >= len(p) && string(b[:len(p)]) == p {
		i := len(p)
		if i < len(b) && b[i] == ' ' {
			i++
		}
		return i
	}
	return -1
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}
