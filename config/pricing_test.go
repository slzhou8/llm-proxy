package config

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestLookupMatching(t *testing.T) {
	p := DefaultPricing()
	cases := []struct {
		name   string
		model  string
		wantIn float64
		wantOK bool
		why    string
	}{
		{"exact", "claude-sonnet-4-5", 3, true, "exact key"},
		{"dated snapshot", "claude-sonnet-4-5-20250929", 3, true, "longest-prefix match inherits family price"},
		{"thinking variant", "claude-sonnet-4-5-thinking", 3, true, "-thinking suffix stripped"},
		{"dated thinking", "claude-sonnet-4-5-20250929-thinking", 3, true, "prefix match still wins"},
		{"uppercase", "Claude-Opus-5", 5, true, "case-insensitive"},
		{"vendor prefix", "anthropic/claude-opus-5", 5, true, "vendor prefix dropped"},
		{"routing suffix", "claude-haiku-4-5:free", 1, true, "':' suffix dropped"},
		{"whitespace", "  claude-opus-5  ", 5, true, "trimmed"},
		{"unknown", "qwen3-flash", 0, false, "no rate, no fallback"},
		{"empty", "", 0, false, "empty name is not priced"},
		// Guards the prefix rule against a shorter family key swallowing a
		// longer one: opus-4-1 must not be priced as opus-4 (15/75 both, but
		// the sonnet pair below differ, which is what makes this load-bearing).
		{"longest prefix wins", "claude-sonnet-5", 2, true, "not matched as claude-sonnet-4-*"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mp, ok := p.Lookup(c.model)
			if ok != c.wantOK {
				t.Fatalf("Lookup(%q) ok=%v want %v (%s)", c.model, ok, c.wantOK, c.why)
			}
			if ok && mp.Input != c.wantIn {
				t.Errorf("Lookup(%q) input=%v want %v (%s)", c.model, mp.Input, c.wantIn, c.why)
			}
		})
	}
}

// TestLookupPrefixDoesNotCrossFamilies is the case the longest-prefix rule
// exists to get right: "claude-opus-4-8" must not be priced from the shorter
// "claude-opus-4" key, whose retired rate is 3x higher.
func TestLookupPrefixDoesNotCrossFamilies(t *testing.T) {
	p := DefaultPricing()
	mp, ok := p.Lookup("claude-opus-4-8")
	if !ok {
		t.Fatal("claude-opus-4-8 should be priced")
	}
	if mp.Input != 5 || mp.Output != 25 {
		t.Errorf("got %v/%v, want 5/25 -- likely matched the retired claude-opus-4 key (15/75)", mp.Input, mp.Output)
	}
}

func TestCost(t *testing.T) {
	p := DefaultPricing()
	// Opus 5: $5/1M in, $25/1M out. 1M in + 1M out = $30.
	got, ok := p.Cost("claude-opus-5", 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("expected priced")
	}
	if math.Abs(got-30) > 1e-9 {
		t.Errorf("cost=%v want 30", got)
	}

	// The in/out spread must actually be applied, not blended: the same token
	// total weighted toward output has to cost more.
	inHeavy, _ := p.Cost("claude-opus-5", 900_000, 100_000)
	outHeavy, _ := p.Cost("claude-opus-5", 100_000, 900_000)
	if !(outHeavy > inHeavy) {
		t.Errorf("output-heavy (%v) should cost more than input-heavy (%v)", outHeavy, inHeavy)
	}

	if _, ok := p.Cost("no-such-model", 1000, 1000); ok {
		t.Error("unknown model must report not-priced rather than costing zero")
	}
}

func TestCostFallback(t *testing.T) {
	p := DefaultPricing()
	fb := ModelPrice{Input: 1, Output: 2}
	p.Fallback = &fb
	got, ok := p.Cost("totally-unknown", 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("fallback should price unknown models")
	}
	if math.Abs(got-3) > 1e-9 {
		t.Errorf("cost=%v want 3", got)
	}
}

func TestCostCurrencyRate(t *testing.T) {
	p := DefaultPricing()
	p.Currency, p.Rate = "CNY", 7.2
	got, _ := p.Cost("claude-opus-5", 1_000_000, 0) // $5 * 7.2
	if math.Abs(got-36) > 1e-9 {
		t.Errorf("cost=%v want 36", got)
	}
	if p.DisplayCurrency() != "CNY" {
		t.Errorf("currency=%q want CNY", p.DisplayCurrency())
	}

	// A zero or negative rate must not zero out every amount.
	p.Rate = 0
	got, _ = p.Cost("claude-opus-5", 1_000_000, 0)
	if math.Abs(got-5) > 1e-9 {
		t.Errorf("rate=0 should fall back to 1x, got %v want 5", got)
	}
}

func TestEnsureDefaultsKeepsUserTable(t *testing.T) {
	// A user who deleted every model but one must not get the full default
	// table merged back in.
	p := PricingConfig{
		Enabled: true,
		Models:  map[string]ModelPrice{"only-mine": {Input: 1, Output: 2}},
	}
	p.EnsureDefaults()
	if len(p.Models) != 1 {
		t.Errorf("user table was merged with defaults: %d entries", len(p.Models))
	}
	if p.Currency != "USD" || p.Rate != 1 {
		t.Errorf("missing sub-fields not filled: currency=%q rate=%v", p.Currency, p.Rate)
	}
}

func TestEnsureDefaultsEmptyMapMeansPriceNothing(t *testing.T) {
	// An explicitly emptied table is a real setting ("price nothing"), distinct
	// from an absent one.
	p := PricingConfig{Enabled: true, Models: map[string]ModelPrice{}}
	p.EnsureDefaults()
	if len(p.Models) != 0 {
		t.Errorf("explicitly empty table was repopulated with %d defaults", len(p.Models))
	}
}

// TestLoadAbsentPricingGetsDefaults covers the upgrade path: an existing
// config.json written before pricing existed must come back with the official
// table enabled, not an all-zero block that silently prices everything at zero.
func TestLoadAbsentPricingGetsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"proxy_addr":":18080"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Pricing.Enabled {
		t.Error("pricing should default to enabled when the key is absent")
	}
	if len(cfg.Pricing.Models) == 0 {
		t.Error("pricing table should be populated when the key is absent")
	}
	if cfg.Pricing.Rate != 1 || cfg.Pricing.Currency != "USD" {
		t.Errorf("rate=%v currency=%q, want 1/USD", cfg.Pricing.Rate, cfg.Pricing.Currency)
	}
}

// TestLoadExplicitDisableSurvives is the mirror case: a user who turned cost
// estimation off must stay off across restarts. This is what makes the
// key-presence probe in Load necessary -- an absent block and an explicitly
// disabled one are identical once unmarshalled into the struct.
func TestLoadExplicitDisableSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"pricing":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pricing.Enabled {
		t.Error("explicit enabled:false was overwritten by defaults")
	}
	// Sub-fields still get filled so the block is usable if re-enabled.
	if len(cfg.Pricing.Models) == 0 {
		t.Error("a pricing block with no models should still get the default table")
	}
}

// TestLoadUserRateSurvivesRoundTrip guards the save/load cycle: a customised
// rate must not be reset by the defaults logic on the next start.
func TestLoadUserRateSurvivesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := Default()
	cfg.Pricing = DefaultPricing()
	cfg.Pricing.Currency, cfg.Pricing.Rate = "CNY", 7.1
	cfg.Pricing.Models = map[string]ModelPrice{"claude-opus-5": {Input: 4, Output: 20}}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pricing.Rate != 7.1 || got.Pricing.Currency != "CNY" {
		t.Errorf("rate/currency not preserved: %v %q", got.Pricing.Rate, got.Pricing.Currency)
	}
	if len(got.Pricing.Models) != 1 {
		t.Errorf("model table not preserved: %d entries", len(got.Pricing.Models))
	}
	if mp := got.Pricing.Models["claude-opus-5"]; mp.Input != 4 {
		t.Errorf("custom rate not preserved: %v", mp.Input)
	}
}

// TestDefaultPricingMatchesPublishedRates pins a sample of the official list
// prices so an accidental edit to the table is caught. Rates retrieved from
// platform.claude.com/docs/en/about-claude/pricing on 2026-09-09.
func TestDefaultPricingMatchesPublishedRates(t *testing.T) {
	want := map[string]ModelPrice{
		"claude-fable-5-1":  {Input: 10, Output: 50},
		"claude-opus-5":     {Input: 5, Output: 25},
		"claude-sonnet-5":   {Input: 2, Output: 10},
		"claude-sonnet-4-5": {Input: 3, Output: 15},
		"claude-haiku-4-5":  {Input: 1, Output: 5},
	}
	p := DefaultPricing()
	for name, w := range want {
		got, ok := p.Models[name]
		if !ok {
			t.Errorf("%s missing from default table", name)
			continue
		}
		if got != w {
			t.Errorf("%s = %v/%v, want %v/%v", name, got.Input, got.Output, w.Input, w.Output)
		}
	}
}

// TestPricingJSONRoundTrip checks the wire shape the dashboard PUTs.
func TestPricingJSONRoundTrip(t *testing.T) {
	in := PricingConfig{
		Enabled:  true,
		Currency: "CNY",
		Rate:     7.2,
		Models:   map[string]ModelPrice{"m": {Input: 1.5, Output: 6}},
		Fallback: &ModelPrice{Input: 0.5, Output: 1},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out PricingConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Rate != in.Rate || out.Currency != in.Currency || !out.Enabled {
		t.Errorf("scalars lost: %+v", out)
	}
	if out.Models["m"].Output != 6 {
		t.Errorf("model table lost: %+v", out.Models)
	}
	if out.Fallback == nil || out.Fallback.Input != 0.5 {
		t.Errorf("fallback lost: %+v", out.Fallback)
	}
}
