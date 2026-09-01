package proxy

import (
	"testing"
	"time"

	"llmproxy/config"
)

// newTestProxy builds a Proxy with breaker-aware failover enabled and no store,
// which is all the picker/breaker paths need.
func newTestProxy(ups ...config.Upstream) *Proxy {
	cfg := &config.Config{
		Upstreams: ups,
		Failover:  config.FailoverStrategy{Mode: "round_robin", SkipUnhealthy: true, MaxRotations: 1},
	}
	return New(cfg, nil)
}

func up(name string) config.Upstream {
	return config.Upstream{Name: name, Protocol: config.ProtocolOpenAI, Enabled: true, BaseURL: "http://x", APIKey: "k"}
}

// prioUp is up() with an explicit priority tier.
func prioUp(name string, prio int) config.Upstream {
	u := up(name)
	u.Priority = prio
	return u
}

func names(g []config.Upstream) []string {
	out := make([]string, len(g))
	for i, u := range g {
		out[i] = u.Name
	}
	return out
}

func TestBreakerOpensAfterStreak(t *testing.T) {
	p := newTestProxy(up("a"))
	for i := 0; i < breakerFailStreak-1; i++ {
		p.recordHealth("a", false, ErrServer)
		if p.unhealthy("a") {
			t.Fatalf("breaker opened early after %d failures", i+1)
		}
	}
	p.recordHealth("a", false, ErrServer)
	if !p.unhealthy("a") {
		t.Fatalf("breaker did not open after %d failures", breakerFailStreak)
	}
}

func TestBreakerClosesOnSuccess(t *testing.T) {
	p := newTestProxy(up("a"))
	for i := 0; i < breakerFailStreak; i++ {
		p.recordHealth("a", false, ErrServer)
	}
	if !p.unhealthy("a") {
		t.Fatal("expected open breaker")
	}
	p.recordHealth("a", true, ErrServer)
	if p.unhealthy("a") {
		t.Fatal("a success must close the breaker immediately")
	}
}

func TestUnhealthyUpstreamMovedLast(t *testing.T) {
	p := newTestProxy(up("a"), up("b"), up("c"))
	for i := 0; i < breakerFailStreak; i++ {
		p.recordHealth("a", false, ErrServer)
	}
	// Reset the cursor so ordering is deterministic for the assertion.
	p.cursor[tierKey{proto: config.ProtocolOpenAI, prio: 0}] = 0
	got := names(p.snapshotGroup(config.ProtocolOpenAI))
	if len(got) != 3 {
		t.Fatalf("group must keep every upstream, got %v", got)
	}
	if got[len(got)-1] != "a" {
		t.Fatalf("failing upstream should be last, got %v", got)
	}
}

func TestAllUnhealthyStillReturnsGroup(t *testing.T) {
	p := newTestProxy(up("a"), up("b"))
	for _, n := range []string{"a", "b"} {
		for i := 0; i < breakerFailStreak; i++ {
			p.recordHealth(n, false, ErrServer)
		}
	}
	if g := p.snapshotGroup(config.ProtocolOpenAI); len(g) != 2 {
		t.Fatalf("must still attempt every upstream when all are failing, got %v", names(g))
	}
}

func TestSkipUnhealthyDisabledKeepsOrder(t *testing.T) {
	p := newTestProxy(up("a"), up("b"))
	p.cfg.Failover.SkipUnhealthy = false
	for i := 0; i < breakerFailStreak; i++ {
		p.recordHealth("a", false, ErrServer)
	}
	p.cursor[tierKey{proto: config.ProtocolOpenAI, prio: 0}] = 0
	if got := names(p.snapshotGroup(config.ProtocolOpenAI)); got[0] != "a" {
		t.Fatalf("with skip_unhealthy off order must be untouched, got %v", got)
	}
}

func TestBreakerOpenWindowGrows(t *testing.T) {
	p := newTestProxy(up("a"))
	for i := 0; i < breakerFailStreak; i++ {
		p.recordHealth("a", false, ErrServer)
	}
	first := p.BreakerStates()[0].OpenForSec
	p.recordHealth("a", false, ErrServer)
	if second := p.BreakerStates()[0].OpenForSec; second <= first {
		t.Fatalf("open window should grow with further failures: %ds then %ds", first, second)
	}
}

func TestBreakerStatesReportsRemaining(t *testing.T) {
	p := newTestProxy(up("a"))
	p.recordHealth("a", false, ErrServer)
	st := p.BreakerStates()
	if len(st) != 1 || st[0].Upstream != "a" || st[0].FailStreak != 1 {
		t.Fatalf("unexpected snapshot: %+v", st)
	}
	if st[0].Open {
		t.Fatal("one failure must not open the breaker")
	}
	if breakerBaseOpen < time.Second {
		t.Fatal("base open window should be at least a second")
	}
}
