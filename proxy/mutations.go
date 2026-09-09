package proxy

import (
	"fmt"

	"llmproxy/config"
)

// ListUpstreams returns a copy of the upstream list with API keys masked for
// display. Full keys are never sent to the dashboard.
func (p *Proxy) ListUpstreams() []config.Upstream {
	p.mu.RLock()
	out := make([]config.Upstream, len(p.cfg.Upstreams))
	copy(out, p.cfg.Upstreams)
	p.mu.RUnlock()
	for i := range out {
		out[i].APIKey = keyPrefix(out[i].APIKey)
	}
	return out
}

// AddUpstream appends an upstream, returns its position. Name must be unique.
func (p *Proxy) AddUpstream(u config.Upstream) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ex := range p.cfg.Upstreams {
		if ex.Name == u.Name {
			return fmt.Errorf("upstream %q already exists", u.Name)
		}
	}
	p.cfg.Upstreams = append(p.cfg.Upstreams, u)
	return p.saveLocked()
}

// UpstreamPatch is a partial upstream update. Pointer fields distinguish "not
// supplied" from a deliberate zero value, which matters for Priority: the
// dashboard's enable/disable toggle sends only {name, enabled}, and a plain int
// would silently reset the priority to 0.
type UpstreamPatch struct {
	Name              string            `json:"name"`
	Protocol          config.Protocol   `json:"protocol"`
	BaseURL           string            `json:"base_url"`
	APIKey            string            `json:"api_key"`
	ExtraHeaders      map[string]string `json:"extra_headers"`
	UserAgent         string            `json:"user_agent"`
	TranslateToOpenAI *bool             `json:"translate_to_openai"`
	Priority          *int              `json:"priority"`
	Enabled           *bool             `json:"enabled"`
}

// UpdateUpstream merges non-empty fields of u into the upstream with matching
// Name. Empty fields keep their existing value, so a masked key from the
// dashboard (sent as empty) never overwrites the real key, and a toggle-only
// update ({name, enabled}) does not wipe base_url/protocol/key.
func (p *Proxy) UpdateUpstream(u UpstreamPatch) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.cfg.Upstreams {
		if p.cfg.Upstreams[i].Name == u.Name {
			ex := p.cfg.Upstreams[i]
			if u.Protocol != "" {
				ex.Protocol = u.Protocol
			}
			if u.BaseURL != "" {
				ex.BaseURL = u.BaseURL
			}
			if u.APIKey != "" {
				ex.APIKey = u.APIKey // only overwrite key when a new one is provided
			}
			if u.ExtraHeaders != nil {
				ex.ExtraHeaders = u.ExtraHeaders
			}
			// UserAgent is merged verbatim: the dashboard always sends the field,
			// so an empty value intentionally clears any previously set UA.
			ex.UserAgent = u.UserAgent
			if u.TranslateToOpenAI != nil {
				ex.TranslateToOpenAI = *u.TranslateToOpenAI
			}
			if u.Priority != nil {
				ex.Priority = *u.Priority
			}
			if u.Enabled != nil {
				ex.Enabled = *u.Enabled
			}
			p.cfg.Upstreams[i] = ex
			return p.saveLocked()
		}
	}
	return fmt.Errorf("upstream %q not found", u.Name)
}

// DeleteUpstream removes an upstream by name.
func (p *Proxy) DeleteUpstream(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.cfg.Upstreams {
		if p.cfg.Upstreams[i].Name == name {
			p.cfg.Upstreams = append(p.cfg.Upstreams[:i], p.cfg.Upstreams[i+1:]...)
			return p.saveLocked()
		}
	}
	return fmt.Errorf("upstream %q not found", name)
}

// UpdatePolicy sets the retry/failover policy. Pass 0 to leave a field as-is.
func (p *Proxy) UpdatePolicy(retry config.RetryStrategy, failover config.FailoverStrategy) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if retry.MaxAttempts > 0 {
		p.cfg.Retry = retry
	}
	if failover.Mode != "" {
		p.cfg.Failover = failover
	}
	return p.saveLocked()
}

// UpdateAlert sets the webhook alerting configuration. Unlike UpdatePolicy it
// replaces the whole struct, because an empty URL is meaningful (it means
// "disabled") and cannot be distinguished from "field not supplied".
func (p *Proxy) UpdateAlert(alert config.AlertConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.Alert = alert
	return p.saveLocked()
}

// UpdatePricing replaces the cost-estimation pricing table. Like UpdateAlert it
// swaps the whole struct rather than merging: an empty Models map means "price
// nothing" and a false Enabled means "hide the cost panels", neither of which
// can be told apart from "field not supplied" if we merged field by field.
//
// Missing sub-fields (currency, rate) are filled with their defaults so a
// caller that omits them cannot land a table that silently prices everything at
// zero.
func (p *Proxy) UpdatePricing(pricing config.PricingConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pricing.Models == nil {
		// Distinguish "cleared the table" from "omitted the field": an explicit
		// empty map arrives non-nil, so nil here means the caller sent no table
		// and the existing one is kept.
		pricing.Models = p.cfg.Pricing.Models
	}
	if pricing.Currency == "" {
		pricing.Currency = "USD"
	}
	if pricing.Rate <= 0 {
		pricing.Rate = 1
	}
	p.cfg.Pricing = pricing
	return p.saveLocked()
}

// saveLocked writes the in-memory config to disk. Caller must hold p.mu.
func (p *Proxy) saveLocked() error {
	if p.cfgPath == "" {
		return p.cfg.Save(defaultJSONPath())
	}
	return p.cfg.Save(p.cfgPath)
}

func defaultJSONPath() string { return "config.json" }
