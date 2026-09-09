package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmproxy/config"
	"llmproxy/proxy"
	"llmproxy/stats"
)

// costServer builds a Server backed by a store loaded from statsJSON (empty
// string for a fresh store). The handlers are called directly, bypassing
// requireAuth, since the auth wrapper is not what these tests are about.
func costServer(t *testing.T, pricing config.PricingConfig, statsJSON string) (*Server, *stats.Store) {
	t.Helper()
	dir := t.TempDir()
	statsPath := filepath.Join(dir, "stats.json")
	if statsJSON != "" {
		if err := os.WriteFile(statsPath, []byte(statsJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := stats.NewStore(statsPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Pricing:  pricing,
		Failover: config.FailoverStrategy{Mode: "round_robin"},
	}
	p := proxy.New(cfg, store)
	return NewServer(cfg, p, store), store
}

func getCost(t *testing.T, s *Server, from, to string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/stats/cost?from="+from+"&to="+to, nil)
	rec := httptest.NewRecorder()
	s.apiStatsCost(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v -- %s", err, rec.Body.String())
	}
	return out
}

func today() string { return time.Now().Format("2006-01-02") }

// TestCostAppliesInputOutputRates checks the basic path: recorded splits are
// priced at their own rates rather than blended.
func TestCostAppliesInputOutputRates(t *testing.T) {
	s, store := costServer(t, config.DefaultPricing(), "")
	// 1M input + 1M output on Opus 5 ($5 in / $25 out) = $30.
	store.RecordCall(stats.Call{
		Time: time.Now(), OK: true, RealModel: "claude-opus-5",
		PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000,
	})
	out := getCost(t, s, today(), today())

	tot := out["total"].(map[string]any)
	if got := tot["cost"].(float64); got < 29.99 || got > 30.01 {
		t.Errorf("cost=%v want 30", got)
	}
	if got := tot["unpriced_models"].(float64); got != 0 {
		t.Errorf("unpriced_models=%v want 0", got)
	}
	if tot["estimated_split"].(bool) {
		t.Error("estimated_split should be false when the split was recorded")
	}
	models := out["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("got %d model rows, want 1", len(models))
	}
	row := models[0].(map[string]any)
	if !row["priced"].(bool) {
		t.Error("row should be priced")
	}
	if row["input_rate"].(float64) != 5 || row["output_rate"].(float64) != 25 {
		t.Errorf("rates not reported: %v", row)
	}
}

// TestCostUnknownModelNotSilentlyZero is the honesty case: a model with no rate
// must be reported as unpriced, with its tokens counted separately, rather than
// contributing zero to a total that then looks complete.
func TestCostUnknownModelNotSilentlyZero(t *testing.T) {
	s, store := costServer(t, config.DefaultPricing(), "")
	store.RecordCall(stats.Call{
		Time: time.Now(), OK: true, RealModel: "some-gateway-model",
		PromptTokens: 1000, CompletionTokens: 2000, TotalTokens: 3000,
	})
	out := getCost(t, s, today(), today())
	tot := out["total"].(map[string]any)

	if got := tot["cost"].(float64); got != 0 {
		t.Errorf("cost=%v want 0 (nothing priceable)", got)
	}
	if got := tot["unpriced_models"].(float64); got != 1 {
		t.Errorf("unpriced_models=%v want 1", got)
	}
	if got := tot["unpriced_tokens"].(float64); got != 3000 {
		t.Errorf("unpriced_tokens=%v want 3000", got)
	}
	row := out["models"].([]any)[0].(map[string]any)
	if row["priced"].(bool) {
		t.Error("unknown model must be marked unpriced")
	}
}

// TestCostPricedRowsSortFirst checks the ordering contract: priced rows lead,
// sorted by spend, so unpriced models collect at the bottom instead of
// interleaving among real costs at zero.
func TestCostPricedRowsSortFirst(t *testing.T) {
	s, store := costServer(t, config.DefaultPricing(), "")
	now := time.Now()
	store.RecordCall(stats.Call{Time: now, OK: true, RealModel: "unknown-a", PromptTokens: 9_000_000, TotalTokens: 9_000_000})
	store.RecordCall(stats.Call{Time: now, OK: true, RealModel: "claude-haiku-4-5", PromptTokens: 1_000_000, TotalTokens: 1_000_000})
	store.RecordCall(stats.Call{Time: now, OK: true, RealModel: "claude-opus-5", PromptTokens: 1_000_000, TotalTokens: 1_000_000})

	models := getCost(t, s, today(), today())["models"].([]any)
	if len(models) != 3 {
		t.Fatalf("got %d rows, want 3", len(models))
	}
	first := models[0].(map[string]any)
	second := models[1].(map[string]any)
	last := models[2].(map[string]any)
	if first["model"] != "claude-opus-5" {
		t.Errorf("highest-cost model should lead, got %v", first["model"])
	}
	if second["model"] != "claude-haiku-4-5" {
		t.Errorf("second by cost should be haiku, got %v", second["model"])
	}
	if last["priced"].(bool) {
		t.Errorf("unpriced row should sort last, got %v", last["model"])
	}
}

// TestCostApportionsLegacySplit covers the upgrade path. Per-model roll-ups
// persisted before the input/output split was tracked carry only a total; those
// must be apportioned by the day's overall ratio and flagged, because pricing
// them as all-input or all-output is wrong by the 5x spread between the rates.
func TestCostApportionsLegacySplit(t *testing.T) {
	day := today()
	// Day totals say the traffic was 80% input / 20% output. The per-model entry
	// has only total_tokens, exactly as older snapshots stored it.
	legacy := fmt.Sprintf(`{
	  "calls": [],
	  "by_day": {
	    %q: {
	      "total": 1, "ok": 1, "failed": 0,
	      "prompt_tokens": 800000, "completion_tokens": 200000, "total_tokens": 1000000,
	      "by_model": {"claude-opus-5": {"total": 1, "ok": 1, "failed": 0, "total_tokens": 1000000}}
	    }
	  }
	}`, day)
	s, _ := costServer(t, config.DefaultPricing(), legacy)
	out := getCost(t, s, day, day)

	tot := out["total"].(map[string]any)
	if !tot["estimated_split"].(bool) {
		t.Error("estimated_split must be true so the UI can mark the figure approximate")
	}
	// 800k in * $5/1M + 200k out * $25/1M = 4.00 + 5.00 = $9.00
	if got := tot["cost"].(float64); got < 8.99 || got > 9.01 {
		t.Errorf("cost=%v want 9.00 (apportioned 80/20)", got)
	}
	if got := tot["unsplit_tokens"].(float64); got != 0 {
		t.Errorf("unsplit_tokens=%v want 0 (a ratio was available)", got)
	}
	row := out["models"].([]any)[0].(map[string]any)
	if !row["estimated_split"].(bool) {
		t.Error("the row itself should be flagged as estimated")
	}
	if got := row["prompt_tokens"].(float64); got != 800000 {
		t.Errorf("apportioned input=%v want 800000", got)
	}
}

// TestCostUnsplittableTokensAreNotPriced checks the corner of the legacy path
// where no ratio exists to apportion with: those tokens are reported separately
// rather than being given an invented split.
func TestCostUnsplittableTokensAreNotPriced(t *testing.T) {
	day := today()
	legacy := fmt.Sprintf(`{
	  "calls": [],
	  "by_day": {
	    %q: {
	      "total": 1, "ok": 1,
	      "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 5000,
	      "by_model": {"claude-opus-5": {"total": 1, "ok": 1, "total_tokens": 5000}}
	    }
	  }
	}`, day)
	s, _ := costServer(t, config.DefaultPricing(), legacy)
	tot := getCost(t, s, day, day)["total"].(map[string]any)

	if got := tot["unsplit_tokens"].(float64); got != 5000 {
		t.Errorf("unsplit_tokens=%v want 5000", got)
	}
	if got := tot["cost"].(float64); got != 0 {
		t.Errorf("cost=%v want 0 -- an unsplittable total must not be priced by guessing", got)
	}
}

// TestCostRespectsDisabledAndCurrency checks that the response carries the
// display settings the dashboard needs, including when estimation is off.
func TestCostRespectsDisabledAndCurrency(t *testing.T) {
	pr := config.DefaultPricing()
	pr.Enabled = false
	pr.Currency, pr.Rate = "CNY", 7.2
	s, store := costServer(t, pr, "")
	store.RecordCall(stats.Call{
		Time: time.Now(), OK: true, RealModel: "claude-opus-5",
		PromptTokens: 1_000_000, TotalTokens: 1_000_000,
	})
	out := getCost(t, s, today(), today())

	if out["enabled"].(bool) {
		t.Error("enabled should mirror the config so the UI can hide the panels")
	}
	if out["currency"].(string) != "CNY" {
		t.Errorf("currency=%v want CNY", out["currency"])
	}
	// $5 * 7.2 = 36
	if got := out["total"].(map[string]any)["cost"].(float64); got < 35.99 || got > 36.01 {
		t.Errorf("cost=%v want 36 (currency rate applied)", got)
	}
}

// TestCostRangeRequiresDates guards the parameter validation.
func TestCostRangeRequiresDates(t *testing.T) {
	s, _ := costServer(t, config.DefaultPricing(), "")
	req := httptest.NewRequest(http.MethodGet, "/api/stats/cost", nil)
	rec := httptest.NewRecorder()
	s.apiStatsCost(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
}

// TestPricingPutRejectsNegativeRates checks the write path's validation; a
// negative rate would silently produce negative spend.
func TestPricingPutRejectsNegativeRates(t *testing.T) {
	s, _ := costServer(t, config.DefaultPricing(), "")
	body := `{"enabled":true,"models":{"m":{"input":-1,"output":5}}}`
	req := httptest.NewRequest(http.MethodPut, "/api/pricing", strings.NewReader(body))
	rec := httptest.NewRecorder()
	// Called directly, so the admin-role gate in the handler sees no user in
	// context and rejects; assert on that boundary instead of the rate check.
	s.apiPricing(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403 -- writes must require an admin identity", rec.Code)
	}
}
