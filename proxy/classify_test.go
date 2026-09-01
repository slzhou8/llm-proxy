package proxy

import (
	"net/http"
	"testing"

	"llmproxy/config"
)

func TestClassifyResponse(t *testing.T) {
	cases := []struct {
		status int
		want   ErrClass
	}{
		{http.StatusOK, ErrNone},
		{http.StatusTooManyRequests, ErrRateLimit},
		{http.StatusInternalServerError, ErrServer},
		{http.StatusBadGateway, ErrServer},
		{http.StatusUnauthorized, ErrAuth},   // upstream key rejected
		{http.StatusForbidden, ErrAuth},      // upstream key rejected
		{http.StatusBadRequest, ErrBusiness}, // caller's fault
		{http.StatusNotFound, ErrBusiness},
	}
	for _, c := range cases {
		if got := classifyResponse(c.status); got != c.want {
			t.Errorf("status %d => %q, want %q", c.status, got, c.want)
		}
	}
}

func TestFailoverable(t *testing.T) {
	// Auth failures must fail over: a different upstream may hold a valid key.
	for _, cls := range []ErrClass{ErrRateLimit, ErrServer, ErrTimeout, ErrNetwork, ErrAuth} {
		if !failoverable(cls) {
			t.Errorf("%q should try another upstream", cls)
		}
	}
	// A bad caller request gets the same answer everywhere, so do not fan it out.
	for _, cls := range []ErrClass{ErrNone, ErrBusiness} {
		if failoverable(cls) {
			t.Errorf("%q must not fail over", cls)
		}
	}
}

func TestAuthNotRetriedOnSameUpstream(t *testing.T) {
	p := newTestProxy(up("a"))
	// A rejected credential will stay rejected, so it must not be retried.
	if p.retryable(ErrAuth) {
		t.Fatal("ErrAuth must not be retried against the same upstream")
	}
}

func TestBusinessErrorDoesNotOpenBreaker(t *testing.T) {
	p := newTestProxy(up("a"))
	// A caller sending malformed requests must never demote a healthy upstream.
	for i := 0; i < breakerFailStreak+3; i++ {
		p.recordHealth("a", false, ErrBusiness)
	}
	if p.unhealthy("a") {
		t.Fatal("400/404 from the caller must not open the breaker")
	}
	if st := p.BreakerStates(); len(st) != 0 {
		t.Fatalf("business errors should not create breaker state, got %+v", st)
	}
}

func TestRateLimitRecoversSoonerThanOutage(t *testing.T) {
	rl := newTestProxy(up("a"))
	for i := 0; i < breakerFailStreak; i++ {
		rl.recordHealth("a", false, ErrRateLimit)
	}
	down := newTestProxy(up("a"))
	for i := 0; i < breakerFailStreak; i++ {
		down.recordHealth("a", false, ErrNetwork)
	}
	rlOpen, downOpen := rl.BreakerStates()[0].OpenForSec, down.BreakerStates()[0].OpenForSec
	if rlOpen >= downOpen {
		t.Fatalf("throttling should clear faster than an outage: 429=%ds network=%ds", rlOpen, downOpen)
	}
}

func TestAuthErrorOpensBreaker(t *testing.T) {
	p := newTestProxy(prioUp("bad-key", 10), prioUp("good-key", 1))
	// An expired upstream key should stop being tried first.
	for i := 0; i < breakerFailStreak; i++ {
		p.recordHealth("bad-key", false, ErrAuth)
	}
	if got := names(p.snapshotGroup(config.ProtocolOpenAI)); got[0] != "good-key" {
		t.Fatalf("upstream with a rejected key should be deprioritised, got %v", got)
	}
}
