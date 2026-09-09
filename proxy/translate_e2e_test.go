package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"llmproxy/config"
	"llmproxy/stats"
)

// fakeAnthropic mirrors the parts of the Anthropic API the bridge touches.
func fakeAnthropic(t *testing.T, stream bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "up-secret" {
			t.Errorf("upstream did not receive its own key; got %q", r.Header.Get("x-api-key"))
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("bridge must rewrite path to /v1/messages, got %q", r.URL.Path)
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write([]byte("event: message_start\n"))
			w.Write([]byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":1}}}` + "\n\n"))
			w.Write([]byte("event: content_block_start\n"))
			w.Write([]byte(`data: {"index":0,"content_block":{"type":"text"}}` + "\n\n"))
			w.Write([]byte("event: content_block_delta\n"))
			w.Write([]byte(`data: {"index":0,"delta":{"type":"text_delta","text":"hello "}}` + "\n\n"))
			w.Write([]byte("event: content_block_delta\n"))
			w.Write([]byte(`data: {"index":0,"delta":{"type":"text_delta","text":"anthropic"}}` + "\n\n"))
			w.Write([]byte("event: message_delta\n"))
			w.Write([]byte(`data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n"))
			w.Write([]byte("event: message_stop\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"hello from anthropic"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`))
	}))
}

func bridgeProxy(t *testing.T, baseURL string) *Proxy {
	store, err := stats.NewStore(filepath.Join(t.TempDir(), "stats.json"), 10)
	if err != nil {
		t.Fatalf("stats store: %v", err)
	}
	p := New(&config.Config{
		Upstreams: []config.Upstream{{
			Name: "bridge", Protocol: config.ProtocolAnthropic, BaseURL: baseURL,
			APIKey: "up-secret", Enabled: true, TranslateToOpenAI: true,
		}},
		Failover: config.FailoverStrategy{Mode: "round_robin", MaxRotations: 1},
	}, store)
	return p
}

func TestBridgeEndToEndNonStream(t *testing.T) {
	anth := fakeAnthropic(t, false)
	defer anth.Close()
	p := bridgeProxy(t, anth.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion"`) {
		t.Errorf("response is not OpenAI-shaped: %s", body)
	}
	if !strings.Contains(body, "hello from anthropic") {
		t.Errorf("text not carried through: %s", body)
	}
}

func TestBridgeEndToEndStream(t *testing.T) {
	anth := fakeAnthropic(t, true)
	defer anth.Close()
	p := bridgeProxy(t, anth.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE]:\n%s", body)
	}
	if !strings.Contains(body, "hello ") || !strings.Contains(body, "anthropic") {
		t.Errorf("streamed text not carried through:\n%s", body)
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("not OpenAI SSE:\n%s", body)
	}
}

// TestBridgeErrorPassthrough checks that an Anthropic upstream error keeps its
// status code and reaches an OpenAI client as an OpenAI-shaped error body,
// instead of being mangled into an empty success completion.
func TestBridgeErrorPassthrough(t *testing.T) {
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"model not found: no-such-model"}}`))
	}))
	defer anth.Close()
	p := bridgeProxy(t, anth.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"model not found: no-such-model"`) {
		t.Errorf("upstream error message lost:\n%s", body)
	}
	if strings.Contains(body, `"object":"chat.completion"`) {
		t.Errorf("error must not be shaped as a success completion:\n%s", body)
	}
}

// TestBridgeSkipsNonChatEndpoints checks that the OpenAI->Anthropic bridge only
// engages for /v1/chat/completions. A GET /v1/models has no body to translate
// and no Anthropic counterpart to rewrite the path to; translating it anyway
// made the proxy fail the request with "openai request is not valid JSON:
// unexpected end of JSON input" before it ever reached the upstream.
func TestBridgeSkipsNonChatEndpoints(t *testing.T) {
	var gotPath, gotKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"object":"list","data":[{"id":"claude-x"}]}`))
	}))
	defer up.Close()
	p := bridgeProxy(t, up.URL)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/models" {
		t.Errorf("path must be forwarded untouched, got %q", gotPath)
	}
	if gotKey != "up-secret" {
		t.Errorf("upstream did not receive its own key, got %q", gotKey)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"claude-x"`) {
		t.Errorf("upstream body not passed through: %s", body)
	}
}
