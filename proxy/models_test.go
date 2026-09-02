package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmproxy/config"
)

// modelsProxy builds a proxy whose single upstream points at a stub that serves
// the given /v1/models payload with the given status.
func modelsProxy(t *testing.T, proto config.Protocol, status int, payload string) (*Proxy, *http.Header) {
	t.Helper()
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Upstreams: []config.Upstream{{
		Name: "free", Protocol: proto, BaseURL: srv.URL,
		APIKey: "upstream-secret", Enabled: true,
	}}}
	return New(cfg, nil), &seen
}

func TestFetchModelsParsesAvailability(t *testing.T) {
	p, _ := modelsProxy(t, config.ProtocolOpenAI, http.StatusOK, `{"data":[
		{"id":"gpt-4o-mini","available":false,"unavailable_reason":"no_key"},
		{"id":"auto","available":true},
		{"id":"legacy"}
	]}`)

	got, err := p.FetchModels(context.Background(), "free")
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 models, got %d: %+v", len(got), got)
	}
	// Usable models sort ahead of unusable ones so the picker opens on one that works.
	if !got[0].Available || !got[1].Available {
		t.Errorf("available models must come first, got %+v", got)
	}
	if last := got[2]; last.ID != "gpt-4o-mini" || last.Available || last.Reason != "no_key" {
		t.Errorf("unavailable model mis-parsed: %+v", last)
	}
	// An upstream that omits the field lists only what it serves, so absent means usable.
	for _, m := range got {
		if m.ID == "legacy" && !m.Available {
			t.Error(`missing "available" must default to true`)
		}
	}
}

func TestFetchModelsSendsUpstreamCredential(t *testing.T) {
	for _, tc := range []struct {
		proto    config.Protocol
		hdr, val string
	}{
		{config.ProtocolOpenAI, "Authorization", "Bearer upstream-secret"},
		{config.ProtocolAnthropic, "X-Api-Key", "upstream-secret"},
	} {
		t.Run(string(tc.proto), func(t *testing.T) {
			p, seen := modelsProxy(t, tc.proto, http.StatusOK, `{"data":[]}`)
			if _, err := p.FetchModels(context.Background(), "free"); err != nil {
				t.Fatalf("FetchModels: %v", err)
			}
			if got := seen.Get(tc.hdr); got != tc.val {
				t.Errorf("%s = %q, want %q", tc.hdr, got, tc.val)
			}
			// Anthropic rejects a request that carries no version header.
			if tc.proto == config.ProtocolAnthropic && seen.Get("anthropic-version") == "" {
				t.Error("anthropic model listing must send anthropic-version")
			}
		})
	}
}

func TestFetchModelsUnknownUpstream(t *testing.T) {
	p, _ := modelsProxy(t, config.ProtocolOpenAI, http.StatusOK, `{"data":[]}`)
	if _, err := p.FetchModels(context.Background(), "nope"); !errors.Is(err, ErrUpstreamNotFound) {
		t.Fatalf("want ErrUpstreamNotFound, got %v", err)
	}
}

// A disabled upstream is not a test target: the dashboard lists it, but the
// proxy would never route to it.
func TestFetchModelsSkipsDisabledUpstream(t *testing.T) {
	cfg := &config.Config{Upstreams: []config.Upstream{{
		Name: "off", Protocol: config.ProtocolOpenAI, BaseURL: "http://127.0.0.1:1",
		APIKey: "k", Enabled: false,
	}}}
	p := New(cfg, nil)
	if _, err := p.FetchModels(context.Background(), "off"); !errors.Is(err, ErrUpstreamNotFound) {
		t.Fatalf("want ErrUpstreamNotFound for disabled upstream, got %v", err)
	}
}

// A refused listing must surface as an error rather than an empty list, so the
// dialog can say why instead of silently showing no models.
func TestFetchModelsUpstreamError(t *testing.T) {
	p, _ := modelsProxy(t, config.ProtocolOpenAI, http.StatusUnauthorized,
		`{"error":{"message":"bad key"}}`)
	_, err := p.FetchModels(context.Background(), "free")
	if err == nil {
		t.Fatal("a 401 listing must be an error")
	}
	if got := err.Error(); got == "" {
		t.Error("error must carry the upstream status/body")
	}
}

func TestFetchModelsRejectsGarbage(t *testing.T) {
	p, _ := modelsProxy(t, config.ProtocolOpenAI, http.StatusOK, `<html>not json</html>`)
	if _, err := p.FetchModels(context.Background(), "free"); err == nil {
		t.Fatal("non-JSON listing must be an error")
	}
}
