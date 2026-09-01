package proxy

import (
	"testing"

	"llmproxy/config"
	"llmproxy/stats"
)

func patchProxy(t *testing.T) *Proxy {
	t.Helper()
	st, _ := stats.NewStore(t.TempDir()+"/s.json", 200)
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "a", Protocol: config.ProtocolOpenAI, BaseURL: "http://x",
		APIKey: "secret", Priority: 7, Enabled: true,
	}}
	p := New(cfg, st)
	p.SetConfigPath(t.TempDir() + "/c.json")
	return p
}

// The dashboard's enable/disable toggle sends only {name, enabled}. Everything
// else -- especially Priority, whose zero value is meaningful -- must survive.
func TestTogglePreservesPriorityAndKey(t *testing.T) {
	p := patchProxy(t)
	off := false
	if err := p.UpdateUpstream(UpstreamPatch{Name: "a", Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	got := p.CurrentConfig().Upstreams[0]
	if got.Priority != 7 {
		t.Errorf("priority reset by toggle: got %d, want 7", got.Priority)
	}
	if got.APIKey != "secret" {
		t.Errorf("api key lost by toggle: %q", got.APIKey)
	}
	if got.Enabled {
		t.Error("upstream should be disabled")
	}
}

func TestPatchCanSetPriorityToZero(t *testing.T) {
	p := patchProxy(t)
	zero, on := 0, true
	if err := p.UpdateUpstream(UpstreamPatch{Name: "a", Priority: &zero, Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if got := p.CurrentConfig().Upstreams[0].Priority; got != 0 {
		t.Errorf("explicit zero priority not applied: got %d", got)
	}
}

func TestPatchEmptyKeyKeepsExisting(t *testing.T) {
	p := patchProxy(t)
	on := true
	if err := p.UpdateUpstream(UpstreamPatch{Name: "a", APIKey: "", Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if got := p.CurrentConfig().Upstreams[0].APIKey; got != "secret" {
		t.Errorf("blank key should not overwrite: got %q", got)
	}
}

func TestPatchUnknownUpstream(t *testing.T) {
	p := patchProxy(t)
	if err := p.UpdateUpstream(UpstreamPatch{Name: "nope"}); err == nil {
		t.Fatal("expected an error for an unknown upstream")
	}
}
