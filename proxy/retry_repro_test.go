package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"llmproxy/config"
	"llmproxy/stats"
)

// newRetryProxy spins up a single-upstream proxy whose upstream is served by h,
// with retries enabled and a short backoff so the test stays fast.
func newRetryProxy(t *testing.T, h http.HandlerFunc) (*Proxy, *stats.Store) {
	t.Helper()
	srv := httptest.NewServer(h)
	st, err := stats.NewStore(filepath.Join(t.TempDir(), "stats.json"), 50)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	go st.Run()
	t.Cleanup(func() { st.Stop(); srv.Close() })

	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name: "a", Protocol: config.ProtocolOpenAI, Enabled: true, BaseURL: srv.URL, APIKey: "k",
		}},
		Retry: config.RetryStrategy{
			MaxAttempts: 3, BaseDelayMS: 1, MaxDelayMS: 2,
			RetryOn429: true, RetryOn5xx: true, RetryTimeout: true, TotalTimeoutSec: 30,
		},
		Failover: config.FailoverStrategy{Mode: "round_robin"},
	}
	return New(cfg, st), st
}

// forceRST closes the connection with a TCP reset instead of a graceful FIN, so
// the proxy sees a real transport error rather than a clean io.EOF.
func forceRST(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = conn.Close()
}

func postJSON(path, payload string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// tally counts recorded calls across all days.
func tally(st *stats.Store) (ok, failed, total int64) {
	for _, d := range st.Snapshot() {
		ok += d.OK
		failed += d.Failed
		total += d.Total
	}
	return
}

// First attempt 429 -> retry 200. The client must still get the body.
func TestRetrySuccessNonStream(t *testing.T) {
	var hits int32
	p, _ := newRetryProxy(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hello"}}]}`)
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, postJSON("/v1/chat/completions", `{"model":"gpt","messages":[]}`))

	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	t.Logf("status=%d hits=%d body=%q", res.StatusCode, hits, string(body))
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "hello") {
		t.Errorf("want 200 with body, got status=%d body=%q", res.StatusCode, string(body))
	}
}

// Same, but the successful retry is an SSE stream.
func TestRetrySuccessSSE(t *testing.T) {
	var hits int32
	p, _ := newRetryProxy(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, postJSON("/v1/chat/completions", `{"model":"gpt","stream":true,"messages":[]}`))

	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	t.Logf("status=%d hits=%d body=%q", res.StatusCode, hits, string(body))
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "[DONE]") {
		t.Errorf("want 200 with complete stream, got status=%d body=%q", res.StatusCode, string(body))
	}
}

// Bytes already went out, so the answer cannot be retried -- but it must be
// recorded as a failure, not silently counted as a success.
func TestStreamResetAfterBytesIsRecordedAsFailure(t *testing.T) {
	var hits int32
	p, st := newRetryProxy(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream server does not support hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"))
		_, _ = conn.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		forceRST(conn)
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, postJSON("/v1/chat/completions", `{"model":"gpt","stream":true,"messages":[]}`))

	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	ok, failed, total := tally(st)
	t.Logf("status=%d hits=%d ok=%d failed=%d total=%d body=%q", res.StatusCode, hits, ok, failed, total, string(body))

	// Whether the transport error lands before or after the first chunk reaches
	// the proxy is a timing race, so the retry count is not asserted here. What
	// must hold is that this request is recorded exactly once and counted as a
	// failure, never as a success.
	if total != 1 {
		t.Errorf("expected exactly one recorded call, got %d", total)
	}
	if ok != 0 || failed != 1 {
		t.Errorf("a truncated stream must be recorded as failed: ok=%d failed=%d", ok, failed)
	}
}

// The upstream answered 200 but reset before a single body byte arrived. Nothing
// was committed yet, so the proxy can retry -- and the caller gets a real answer
// instead of hanging on an empty response.
func TestStreamResetBeforeBytesRetriesAndSucceeds(t *testing.T) {
	var hits int32
	p, st := newRetryProxy(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("upstream server does not support hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			// Status line only, then reset before emitting any body.
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"))
			forceRST(conn)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\ndata: [DONE]\n\n")
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, postJSON("/v1/chat/completions", `{"model":"gpt","stream":true,"messages":[]}`))

	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	ok, failed, total := tally(st)
	t.Logf("status=%d hits=%d ok=%d failed=%d total=%d body=%q", res.StatusCode, hits, ok, failed, total, string(body))

	if res.StatusCode != http.StatusOK {
		t.Errorf("want 200 after successful retry, got %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "recovered") || !strings.Contains(string(body), "[DONE]") {
		t.Errorf("caller must receive the complete retry answer, got %q", string(body))
	}
	if hits != 2 {
		t.Errorf("expected a retry after the uncommitted failure, hits=%d", hits)
	}
	if total != 1 || ok != 1 || failed != 0 {
		t.Errorf("the retry must be the only recorded call: total=%d ok=%d failed=%d", total, ok, failed)
	}
}
