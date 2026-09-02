package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"llmproxy/config"
)

// defaultAnthropicVersion is sent when listing models from an Anthropic
// upstream, which rejects requests that carry no version header.
const defaultAnthropicVersion = "2023-06-01"

// modelsBodyLimit caps how much of a model listing we read. A gateway fronting
// hundreds of models answers with ~100KB; the limit only guards against an
// upstream that streams without end.
const modelsBodyLimit = 4 << 20 // 4 MiB

// ModelInfo is one entry of an upstream's model listing, flattened to the few
// fields the dashboard needs.
type ModelInfo struct {
	ID string `json:"id"`
	// Available reflects a non-standard field some gateways return to say whether
	// they hold a usable provider key for this model. Upstreams that omit it are
	// reported as available, since a plain OpenAI endpoint lists only what it serves.
	Available bool `json:"available"`
	// Reason carries the gateway's explanation when Available is false (e.g. "no_key").
	Reason string `json:"reason,omitempty"`
}

// ErrUpstreamNotFound is returned when no enabled upstream matches the name.
var ErrUpstreamNotFound = errors.New("upstream not found or disabled")

// findUpstream returns a copy of the named enabled upstream. Copying under the
// lock keeps the caller from reading fields that concurrent config edits write.
func (p *Proxy) findUpstream(name string) (config.Upstream, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.cfg.Upstreams {
		if p.cfg.Upstreams[i].Enabled && p.cfg.Upstreams[i].Name == name {
			return p.cfg.Upstreams[i], true
		}
	}
	return config.Upstream{}, false
}

// FetchModels asks one upstream which models it serves, so the dashboard can
// offer a real choice instead of a hardcoded guess. Which models exist is a
// property of the upstream, not of the protocol.
func (p *Proxy) FetchModels(ctx context.Context, name string) ([]ModelInfo, error) {
	up, ok := p.findUpstream(name)
	if !ok {
		return nil, ErrUpstreamNotFound
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(up.BaseURL, "/v1/models"), nil)
	if err != nil {
		return nil, err
	}
	// Present the upstream's own credential, the way sendOne does per protocol.
	switch up.Protocol {
	case config.ProtocolAnthropic:
		req.Header.Set("x-api-key", up.APIKey)
		req.Header.Set("anthropic-version", defaultAnthropicVersion)
	default:
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
	for k, v := range up.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsBodyLimit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, snippet(body))
	}

	// Both protocols wrap the listing in {"data":[...]}. Availability fields are
	// optional: pointers distinguish "absent" from "present and false".
	var parsed struct {
		Data []struct {
			ID                string  `json:"id"`
			Available         *bool   `json:"available"`
			UnavailableReason *string `json:"unavailable_reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("cannot parse model list: %w", err)
	}

	out := make([]ModelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		mi := ModelInfo{ID: m.ID, Available: true}
		if m.Available != nil {
			mi.Available = *m.Available
		}
		if !mi.Available && m.UnavailableReason != nil {
			mi.Reason = *m.UnavailableReason
		}
		out = append(out, mi)
	}

	// Usable models first so the picker opens on something that will work;
	// the sort is stable so each group keeps the upstream's own ordering.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Available && !out[j].Available })
	return out, nil
}

// snippet trims an upstream error body to something loggable.
func snippet(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
