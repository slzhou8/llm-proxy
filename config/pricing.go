package config

import "strings"

// ModelPrice is the per-million-token list price for one model, in USD.
// Input and Output are priced separately because the spread between them is
// large (5x on most Claude models), so a single blended rate would be wrong for
// any traffic shape other than the one it was calibrated on.
type ModelPrice struct {
	Input  float64 `json:"input"`  // USD per 1M prompt/input tokens
	Output float64 `json:"output"` // USD per 1M completion/output tokens
}

// PricingConfig turns recorded token counts into an estimated spend for the
// dashboard. It is a local estimate only: the proxy prices what the upstream
// reported in `usage`, which is not the same as what the upstream actually
// bills. See Cost for the specific caveats.
type PricingConfig struct {
	Enabled bool `json:"enabled"` // master switch for the dashboard's cost panels

	// Currency is a display label and Rate the multiplier applied to the USD
	// figures below, so a user billed in another currency can read the panel in
	// it. Rate <= 0 is treated as 1 (no conversion).
	Currency string  `json:"currency"`
	Rate     float64 `json:"rate"`

	// Models maps a model name to its price. A recorded model matches a key
	// exactly, or by longest key prefix, so one entry ("claude-sonnet-4-5")
	// covers the dated and -thinking variants the upstream may report
	// ("claude-sonnet-4-5-20250929", "claude-sonnet-4-5-thinking").
	Models map[string]ModelPrice `json:"models"`

	// Fallback prices models that match no key. Nil (the default) leaves them
	// unpriced and counted separately, which keeps an unknown model visible as
	// a gap rather than silently folding a guessed rate into the total.
	Fallback *ModelPrice `json:"fallback,omitempty"`
}

// DefaultPricing returns Anthropic's published list prices (USD per million
// tokens, https://platform.claude.com/docs/en/about-claude/pricing, retrieved
// 2026-09-09). Retired models are included because an upstream gateway may
// still serve them.
//
// These are base input and output rates. Batch (0.5x), cache writes (1.25x/2x),
// cache reads (0.1x, 0.025x on Fable/Mythos 5.1) and the us-only inference
// multiplier (1.1x) are not modelled -- see Cost.
func DefaultPricing() PricingConfig {
	return PricingConfig{
		Enabled:  true,
		Currency: "USD",
		Rate:     1,
		Models: map[string]ModelPrice{
			// Fable / Mythos tier
			"claude-fable-5-1":  {Input: 10, Output: 50},
			"claude-mythos-5-1": {Input: 10, Output: 50},
			"claude-fable-5":    {Input: 10, Output: 50},
			"claude-mythos-5":   {Input: 10, Output: 50},
			// Opus tier
			"claude-opus-5":   {Input: 5, Output: 25},
			"claude-opus-4-8": {Input: 5, Output: 25},
			"claude-opus-4-7": {Input: 5, Output: 25},
			"claude-opus-4-6": {Input: 5, Output: 25},
			"claude-opus-4-5": {Input: 5, Output: 25},
			"claude-opus-4-1": {Input: 15, Output: 75}, // retired
			"claude-opus-4":   {Input: 15, Output: 75}, // retired
			// Sonnet tier
			"claude-sonnet-5":   {Input: 2, Output: 10},
			"claude-sonnet-4-6": {Input: 3, Output: 15},
			"claude-sonnet-4-5": {Input: 3, Output: 15},
			"claude-sonnet-4":   {Input: 3, Output: 15}, // retired
			// Haiku tier
			"claude-haiku-4-5": {Input: 1, Output: 5},
			"claude-haiku-3-5": {Input: 0.80, Output: 4}, // retired
		},
	}
}

// EnsureDefaults fills in sub-fields a partial pricing block left out. It is
// for a config that HAS a pricing block; whether one was present at all is
// decided by the caller (Load), because an absent block and an explicitly
// all-zero one are indistinguishable after unmarshalling.
//
// A non-nil Models map is left exactly as given, never merged with the default
// table: json.Unmarshal adds keys to an existing map rather than replacing it,
// so seeding defaults ahead of the overlay would resurrect a model the user
// deleted in the dashboard on the next restart.
func (p *PricingConfig) EnsureDefaults() {
	if p.Models == nil {
		p.Models = DefaultPricing().Models
	}
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.Rate <= 0 {
		p.Rate = 1
	}
}

// normalizeModel reduces an upstream-reported model name to the form the
// Models keys use: lowercase, without a vendor prefix ("anthropic/claude-x")
// and without a routing suffix ("qwen3-flash:free").
func normalizeModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.IndexByte(m, ':'); i >= 0 {
		m = m[:i]
	}
	return m
}

// Lookup resolves the price for a recorded model name. Match order: exact key,
// exact key after dropping a "-thinking" suffix, then the longest key that is a
// prefix of the name (so a dated snapshot inherits its family's price).
// Reports false when nothing matched and no Fallback is configured.
func (p PricingConfig) Lookup(model string) (ModelPrice, bool) {
	name := normalizeModel(model)
	if name == "" {
		return p.fallback()
	}
	if mp, ok := p.Models[name]; ok {
		return mp, true
	}
	if base := strings.TrimSuffix(name, "-thinking"); base != name {
		if mp, ok := p.Models[base]; ok {
			return mp, true
		}
	}
	best := ""
	for k := range p.Models {
		if len(k) > len(best) && strings.HasPrefix(name, k) {
			best = k
		}
	}
	if best != "" {
		return p.Models[best], true
	}
	return p.fallback()
}

func (p PricingConfig) fallback() (ModelPrice, bool) {
	if p.Fallback != nil {
		return *p.Fallback, true
	}
	return ModelPrice{}, false
}

// Cost estimates the spend for in/out tokens on one model, in the configured
// display currency. Reports false when the model has no price, so the caller
// can account for unpriced traffic instead of adding a silent zero.
//
// The result is an approximation, and biased low, for three reasons:
//   - Cache reads and writes are billed at their own rates (0.1x / 1.25x-2x of
//     input) and the Anthropic `usage` block reports them in fields the proxy
//     does not record, so cached input is missing from these token counts.
//   - Batch (0.5x) and us-only inference (1.1x) multipliers are not applied.
//   - A third-party gateway may charge its own margin over list price.
func (p PricingConfig) Cost(model string, inTokens, outTokens int64) (float64, bool) {
	mp, ok := p.Lookup(model)
	if !ok {
		return 0, false
	}
	usd := (float64(inTokens)*mp.Input + float64(outTokens)*mp.Output) / 1e6
	return usd * p.rate(), true
}

// rate is the USD->display-currency multiplier, defaulting to 1 so a config
// written without one still produces USD figures rather than zeroes.
func (p PricingConfig) rate() float64 {
	if p.Rate <= 0 {
		return 1
	}
	return p.Rate
}

// DisplayCurrency is the label to render amounts with, defaulting to USD.
func (p PricingConfig) DisplayCurrency() string {
	if p.Currency == "" {
		return "USD"
	}
	return p.Currency
}
