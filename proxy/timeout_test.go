package proxy

import (
	"net/http"
	"testing"
	"time"
)

func TestBudgetExhausted(t *testing.T) {
	if budgetExhausted(time.Time{}) {
		t.Error("a zero deadline means unlimited and must never be exhausted")
	}
	if budgetExhausted(time.Now().Add(time.Minute)) {
		t.Error("a future deadline is not exhausted")
	}
	if !budgetExhausted(time.Now().Add(-time.Second)) {
		t.Error("a past deadline must be exhausted")
	}
}

func TestWaitFits(t *testing.T) {
	if !waitFits(time.Time{}, time.Hour) {
		t.Error("with no deadline any wait fits")
	}
	if !waitFits(time.Now().Add(10*time.Second), time.Second) {
		t.Error("a short wait inside the budget should fit")
	}
	// Sleeping past the deadline only delays an inevitable failure.
	if waitFits(time.Now().Add(time.Second), 30*time.Second) {
		t.Error("a wait that overruns the deadline must not fit")
	}
}

func TestRetryDeadlineRespectsConfig(t *testing.T) {
	p := newTestProxy(up("a"))
	start := time.Now()

	p.cfg.Retry.TotalTimeoutSec = 0
	if d := p.retryDeadline(start); !d.IsZero() {
		t.Error("0 should disable the budget")
	}

	p.cfg.Retry.TotalTimeoutSec = 30
	if got := p.retryDeadline(start); got.Sub(start) != 30*time.Second {
		t.Errorf("deadline should be start+30s, got %v", got.Sub(start))
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "7")
	if got := retryAfter(resp); got != 7*time.Second {
		t.Errorf("got %v, want 7s", got)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", time.Now().Add(20*time.Second).UTC().Format(http.TimeFormat))
	got := retryAfter(resp)
	// Date parsing has second granularity, so allow a small window.
	if got < 18*time.Second || got > 21*time.Second {
		t.Errorf("got %v, want ~20s", got)
	}
}

func TestRetryAfterCapped(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "86400") // a day
	if got := retryAfter(resp); got != 5*time.Minute {
		t.Errorf("an absurd Retry-After must be capped at 5m, got %v", got)
	}
}

func TestRetryAfterAbsentOrGarbage(t *testing.T) {
	if got := retryAfter(&http.Response{Header: http.Header{}}); got != 0 {
		t.Errorf("absent header should yield 0, got %v", got)
	}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "soon-ish")
	if got := retryAfter(resp); got != 0 {
		t.Errorf("unparseable header should yield 0, got %v", got)
	}
	if got := retryAfter(nil); got != 0 {
		t.Errorf("nil response should yield 0, got %v", got)
	}
}
