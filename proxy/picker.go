package proxy

import (
	"math/rand"
	"sort"

	"llmproxy/config"
)

// tierKey identifies one priority tier inside a protocol group. Each tier keeps
// its own round-robin cursor so tiers rotate independently of each other.
type tierKey struct {
	proto config.Protocol
	prio  int
}

// snapshotGroup returns the ordered list of upstreams to try for a protocol:
//
//  1. by priority, highest first (upstreams sharing a priority form one tier)
//  2. round-robin within each tier, so equal-priority keys share the load
//  3. upstreams with an open circuit breaker pushed to the back
//
// The returned slice is a defensive copy; the caller owns it.
func (p *Proxy) snapshotGroup(proto config.Protocol) []config.Upstream {
	p.mu.RLock()
	enabled := make([]config.Upstream, 0, len(p.cfg.Upstreams))
	for _, u := range p.cfg.Upstreams {
		if u.Enabled && u.Protocol == proto {
			enabled = append(enabled, u)
		}
	}
	p.mu.RUnlock()

	if len(enabled) == 0 {
		return nil
	}

	// Highest priority first; name breaks ties so tier membership is stable.
	sort.Slice(enabled, func(i, j int) bool {
		if enabled[i].Priority != enabled[j].Priority {
			return enabled[i].Priority > enabled[j].Priority
		}
		return enabled[i].Name < enabled[j].Name
	})

	// Rotate each tier independently, then concatenate tiers in priority order.
	ordered := make([]config.Upstream, 0, len(enabled))
	for start := 0; start < len(enabled); {
		end := start
		for end < len(enabled) && enabled[end].Priority == enabled[start].Priority {
			end++
		}
		ordered = append(ordered, p.rotateTier(proto, enabled[start:end])...)
		start = end
	}

	return p.deprioritiseUnhealthy(ordered)
}

// rotateTier advances the tier's cursor and returns the tier rotated by it, so
// consecutive requests start at a different member of the same priority.
func (p *Proxy) rotateTier(proto config.Protocol, tier []config.Upstream) []config.Upstream {
	if len(tier) < 2 {
		return tier
	}
	k := tierKey{proto: proto, prio: tier[0].Priority}

	p.rrMu.Lock()
	cur := p.cursor[k]
	p.cursor[k] = (cur + 1) % len(tier)
	p.rrMu.Unlock()

	cur %= len(tier)
	if cur == 0 {
		return tier
	}
	rot := make([]config.Upstream, 0, len(tier))
	rot = append(rot, tier[cur:]...)
	rot = append(rot, tier[:cur]...)
	return rot
}

// limitRotations truncates the candidate list to the configured failover budget.
// MaxRotations counts switches, so at most MaxRotations+1 upstreams are tried;
// 0 or negative means unlimited. This bounds the worst-case latency and spend of
// a single request when several upstreams are unhealthy at once.
func (p *Proxy) limitRotations(group []config.Upstream) []config.Upstream {
	p.mu.RLock()
	max := p.cfg.Failover.MaxRotations
	p.mu.RUnlock()
	if max <= 0 || len(group) <= max+1 {
		return group
	}
	return group[:max+1]
}

// randFloat returns a float in [0,1). Wrapped so backoff jitter is easy to read.
func randFloat() float64 { return rand.Float64() }
