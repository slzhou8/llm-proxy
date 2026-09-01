package proxy

import (
	"time"

	"llmproxy/config"
)

// upstreamHealth tracks the recent outcome of one upstream so the picker can
// deprioritise it while it is failing.
//
// This is deliberately separate from alertState: alerting is about notifying a
// human ("this has been broken for a while"), while breaking is about routing
// ("do not waste this request on it right now"). They need different thresholds
// and different reset behaviour, so they do not share state.
type upstreamHealth struct {
	streak   int       // consecutive failures
	openTill time.Time // breaker is open (upstream deprioritised) until this time
}

// breaker tuning. These are intentionally not user-configurable yet: the values
// only need to be good enough to stop a dead upstream from being tried first,
// and every extra knob is another thing to get wrong in the dashboard.
const (
	breakerFailStreak = 3                // consecutive failures before opening
	breakerBaseOpen   = 15 * time.Second // first open duration
	breakerMaxOpen    = 5 * time.Minute  // cap after repeated failures

	// A 429 means "slow down", not "broken": back off briefly so the upstream
	// gets a chance to recover, and return to it much sooner than a dead one.
	breakerRateLimitBase = 5 * time.Second
	breakerRateLimitMax  = 60 * time.Second
)

// recordHealth updates the breaker for one upstream after a call.
//
// A success closes the breaker immediately: the upstream just proved it works,
// so there is no reason to keep avoiding it. Failures past the streak threshold
// open it for a window that doubles with each further failure, so a persistently
// dead upstream is skipped for longer while a blip recovers fast.
func (p *Proxy) recordHealth(upstream string, ok bool, cls ErrClass) {
	if upstream == "" {
		return
	}
	// A malformed or unauthorised *caller* request (400/404) says nothing about
	// the upstream's health, so it must never demote a working key.
	if !ok && cls == ErrBusiness {
		return
	}
	p.healthMu.Lock()
	defer p.healthMu.Unlock()

	h := p.health[upstream]
	if h == nil {
		h = &upstreamHealth{}
		p.health[upstream] = h
	}
	if ok {
		h.streak = 0
		h.openTill = time.Time{}
		return
	}
	h.streak++
	if h.streak < breakerFailStreak {
		return
	}
	// Exponential open window, doubling with each further failure.
	base, maxOpen := breakerBaseOpen, breakerMaxOpen
	if cls == ErrRateLimit {
		base, maxOpen = breakerRateLimitBase, breakerRateLimitMax
	}
	open := base << (h.streak - breakerFailStreak)
	if open > maxOpen || open <= 0 {
		open = maxOpen
	}
	h.openTill = time.Now().Add(open)
}

// unhealthy reports whether an upstream's breaker is currently open.
func (p *Proxy) unhealthy(name string) bool {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	h := p.health[name]
	return h != nil && time.Now().Before(h.openTill)
}

// HealthSnapshot is the breaker state for one upstream, for the dashboard.
type HealthSnapshot struct {
	Upstream   string `json:"upstream"`
	FailStreak int    `json:"fail_streak"`
	Open       bool   `json:"open"`         // currently deprioritised
	OpenForSec int    `json:"open_for_sec"` // remaining seconds, 0 when closed
}

// BreakerStates exposes breaker state so the dashboard can show which upstreams
// are being skipped and why.
func (p *Proxy) BreakerStates() []HealthSnapshot {
	now := time.Now()
	p.healthMu.Lock()
	out := make([]HealthSnapshot, 0, len(p.health))
	for name, h := range p.health {
		s := HealthSnapshot{Upstream: name, FailStreak: h.streak}
		if now.Before(h.openTill) {
			s.Open = true
			s.OpenForSec = int(h.openTill.Sub(now).Seconds()) + 1
		}
		out = append(out, s)
	}
	p.healthMu.Unlock()
	return out
}

// skipUnhealthy reports whether breaker-aware ordering is enabled in config.
func (p *Proxy) skipUnhealthy() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg.Failover.SkipUnhealthy
}

// deprioritiseUnhealthy reorders group so upstreams with an open breaker come
// last, preserving relative order within each partition.
//
// Unhealthy upstreams are moved rather than removed: if every upstream is
// currently failing, the request must still be attempted somewhere rather than
// failing with "no upstream available".
func (p *Proxy) deprioritiseUnhealthy(group []config.Upstream) []config.Upstream {
	if len(group) < 2 || !p.skipUnhealthy() {
		return group
	}
	healthy := make([]config.Upstream, 0, len(group))
	broken := make([]config.Upstream, 0, len(group))
	for _, u := range group {
		if p.unhealthy(u.Name) {
			broken = append(broken, u)
		} else {
			healthy = append(healthy, u)
		}
	}
	if len(broken) == 0 {
		return group
	}
	return append(healthy, broken...)
}
