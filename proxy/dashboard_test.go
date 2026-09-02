package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"llmproxy/config"
	"llmproxy/stats"
)

// fakeUpstream records the headers of the request it last received and answers
// with a minimal OpenAI-shaped body.
type fakeUpstream struct {
	srv     *httptest.Server
	got     http.Header
	gotPath string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got = r.Header.Clone()
		f.gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// newDashProxy builds a proxy with one upstream and one client key configured.
// The configured key matters: it is what makes the client-key gate active, which
// is the condition under which the dashboard's upstream test used to fail.
func newDashProxy(t *testing.T, proto config.Protocol, baseURL string) *Proxy {
	t.Helper()
	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name: "free", Protocol: proto, BaseURL: baseURL,
			APIKey: "upstream-secret", Enabled: true,
		}},
		ClientKeys: []config.ClientKey{{Name: "app", Key: "ck-good", Enabled: true}},
		Failover:   config.FailoverStrategy{Mode: "round_robin", MaxRotations: 1},
	}
	store, err := stats.NewStore(filepath.Join(t.TempDir(), "stats.json"), 10)
	if err != nil {
		t.Fatalf("stats store: %v", err)
	}
	p := New(cfg, store)
	// Pin the config path inside the temp dir: token accounting persists the
	// config, and the default would drop a config.json beside the test files.
	p.SetConfigPath(filepath.Join(t.TempDir(), "config.json"))
	return p
}

// dashboardRequest mimics the panel's own fetch: it posts to /api/test, not to
// an inference path. Tests that build the inference path directly would miss the
// upstream-path rewrite entirely.
func dashboardRequest(proto, upstream, auth string) *http.Request {
	r := dashTestRequest(proto, upstream, auth)
	r.URL.Path = "/api/test"
	return r
}

func dashTestRequest(proto, upstream, auth string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-LLMPROXY-Protocol", proto)
	r.Header.Set("X-LLMPROXY-Upstream", upstream)
	r.Header.Set("Authorization", "Bearer "+auth)
	return r
}

// The dashboard authenticates its caller with a panel JWT, not a client key.
// Running the client-key gate on that request rejected the JWT as an invalid
// client key, so every "test" button press returned 401 once any client key
// was configured.
func TestDashboardTestSkipsClientKeyGate(t *testing.T) {
	up := newFakeUpstream(t)
	p := newDashProxy(t, config.ProtocolOpenAI, up.srv.URL)

	w := httptest.NewRecorder()
	p.TestThrough(w, dashTestRequest("openai", "free", "panel.jwt.value"))

	if w.Code != http.StatusOK {
		t.Fatalf("dashboard test must reach the upstream, got %d: %s", w.Code, w.Body.String())
	}
	if up.got == nil {
		t.Fatal("upstream was never called")
	}
}

// The gate itself must stay closed for ordinary callers.
func TestProxyStillRejectsBadClientKey(t *testing.T) {
	up := newFakeUpstream(t)
	p := newDashProxy(t, config.ProtocolOpenAI, up.srv.URL)

	w := httptest.NewRecorder()
	p.ServeHTTP(w, dashTestRequest("openai", "free", "not-a-real-key"))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown client key must be rejected, got %d", w.Code)
	}
	if up.got != nil {
		t.Fatal("rejected call must never reach the upstream")
	}
}

func TestValidClientKeyStillForwards(t *testing.T) {
	up := newFakeUpstream(t)
	p := newDashProxy(t, config.ProtocolOpenAI, up.srv.URL)

	w := httptest.NewRecorder()
	p.ServeHTTP(w, dashTestRequest("openai", "free", "ck-good"))

	if w.Code != http.StatusOK {
		t.Fatalf("valid client key must forward, got %d: %s", w.Code, w.Body.String())
	}
}

// Whatever the caller authenticated with is a credential for this proxy, not for
// the upstream, and must not be forwarded to a third party.
func TestInboundCredentialNotLeakedUpstream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proto   config.Protocol
		hdr     string
		wantVal string
	}{
		{"openai sends bearer upstream key", config.ProtocolOpenAI, "Authorization", "Bearer upstream-secret"},
		{"anthropic sends x-api-key", config.ProtocolAnthropic, "X-Api-Key", "upstream-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t)
			p := newDashProxy(t, tc.proto, up.srv.URL)

			p.TestThrough(httptest.NewRecorder(),
				dashTestRequest(string(tc.proto), "free", "panel.jwt.value"))

			if up.got == nil {
				t.Fatal("upstream was never called")
			}
			if got := up.got.Get(tc.hdr); got != tc.wantVal {
				t.Errorf("%s = %q, want %q", tc.hdr, got, tc.wantVal)
			}
			for _, h := range up.got.Values("Authorization") {
				if strings.Contains(h, "panel.jwt.value") {
					t.Errorf("inbound panel JWT leaked upstream in Authorization: %q", h)
				}
			}
			if tc.proto == config.ProtocolAnthropic && up.got.Get("Authorization") != "" {
				t.Errorf("anthropic upstream must get no Authorization, got %q", up.got.Get("Authorization"))
			}
		})
	}
}

// The pin hint is addressed to this proxy and is meaningless downstream.
func TestRoutingHintsStrippedUpstream(t *testing.T) {
	up := newFakeUpstream(t)
	p := newDashProxy(t, config.ProtocolOpenAI, up.srv.URL)

	p.TestThrough(httptest.NewRecorder(), dashTestRequest("openai", "free", "panel.jwt.value"))

	if up.got == nil {
		t.Fatal("upstream was never called")
	}
	for _, h := range []string{"X-LLMPROXY-Upstream", "X-LLMPROXY-Protocol"} {
		if v := up.got.Get(h); v != "" {
			t.Errorf("%s must be stripped before forwarding, got %q", h, v)
		}
	}
}

// The dashboard posts to /api/test; forwarding that path verbatim made the
// upstream answer 404 ("Cannot POST /v1/api/test") once the auth gate was fixed.
func TestDashboardTestRewritesUpstreamPath(t *testing.T) {
	for _, tc := range []struct {
		proto    config.Protocol
		wantPath string
	}{
		{config.ProtocolOpenAI, "/v1/chat/completions"},
		{config.ProtocolAnthropic, "/v1/messages"},
	} {
		t.Run(string(tc.proto), func(t *testing.T) {
			up := newFakeUpstream(t)
			p := newDashProxy(t, tc.proto, up.srv.URL)

			w := httptest.NewRecorder()
			p.TestThrough(w, dashboardRequest(string(tc.proto), "free", "panel.jwt.value"))

			if up.gotPath != tc.wantPath {
				t.Errorf("upstream path = %q, want %q", up.gotPath, tc.wantPath)
			}
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

// The panel's real request shape must survive the whole path, gate included.
func TestDashboardRequestReachesUpstream(t *testing.T) {
	up := newFakeUpstream(t)
	p := newDashProxy(t, config.ProtocolOpenAI, up.srv.URL)

	w := httptest.NewRecorder()
	p.TestThrough(w, dashboardRequest("openai", "free", "panel.jwt.value"))

	if w.Code != http.StatusOK {
		t.Fatalf("panel test must succeed, got %d: %s", w.Code, w.Body.String())
	}
	if up.got.Get("Authorization") != "Bearer upstream-secret" {
		t.Errorf("upstream got %q, want the upstream key", up.got.Get("Authorization"))
	}
}
