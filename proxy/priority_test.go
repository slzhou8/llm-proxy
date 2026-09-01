package proxy

import (
	"testing"

	"llmproxy/config"
)

func TestHigherPriorityFirst(t *testing.T) {
	// Names are chosen so alphabetical order would disagree with priority: this
	// fails if the picker still sorts by name.
	p := newTestProxy(prioUp("zzz-main", 10), prioUp("aaa-backup", 1))
	got := names(p.snapshotGroup(config.ProtocolOpenAI))
	if got[0] != "zzz-main" {
		t.Fatalf("highest priority must come first, got %v", got)
	}
}

func TestEqualPriorityRoundRobins(t *testing.T) {
	p := newTestProxy(prioUp("a", 5), prioUp("b", 5))
	first := names(p.snapshotGroup(config.ProtocolOpenAI))[0]
	second := names(p.snapshotGroup(config.ProtocolOpenAI))[0]
	if first == second {
		t.Fatalf("same-priority upstreams should alternate, both started with %s", first)
	}
}

func TestTiersStayOrderedWhileRotating(t *testing.T) {
	// Two main keys share the top tier, one backup sits below and must never be
	// promoted above them by rotation.
	p := newTestProxy(prioUp("main-1", 10), prioUp("main-2", 10), prioUp("backup", 1))
	for i := 0; i < 6; i++ {
		got := names(p.snapshotGroup(config.ProtocolOpenAI))
		if got[2] != "backup" {
			t.Fatalf("round %d: lower tier must stay last, got %v", i, got)
		}
	}
}

func TestUnhealthyTopPriorityFallsBehind(t *testing.T) {
	p := newTestProxy(prioUp("main", 10), prioUp("backup", 1))
	for i := 0; i < breakerFailStreak; i++ {
		p.recordHealth("main", false, ErrServer)
	}
	got := names(p.snapshotGroup(config.ProtocolOpenAI))
	if got[0] != "backup" {
		t.Fatalf("a broken top-priority upstream should yield to backup, got %v", got)
	}
	// ...and reclaim its place as soon as it works again.
	p.recordHealth("main", true, ErrNone)
	if got := names(p.snapshotGroup(config.ProtocolOpenAI)); got[0] != "main" {
		t.Fatalf("recovered upstream must regain priority, got %v", got)
	}
}

func TestMaxRotationsCapsCandidates(t *testing.T) {
	p := newTestProxy(prioUp("a", 3), prioUp("b", 2), prioUp("c", 1))
	p.cfg.Failover.MaxRotations = 1 // one switch => at most two upstreams
	if got := p.limitRotations(p.snapshotGroup(config.ProtocolOpenAI)); len(got) != 2 {
		t.Fatalf("expected 2 candidates with MaxRotations=1, got %v", names(got))
	}
}

func TestMaxRotationsZeroMeansUnlimited(t *testing.T) {
	p := newTestProxy(prioUp("a", 3), prioUp("b", 2), prioUp("c", 1))
	p.cfg.Failover.MaxRotations = 0
	if got := p.limitRotations(p.snapshotGroup(config.ProtocolOpenAI)); len(got) != 3 {
		t.Fatalf("0 should not limit candidates, got %v", names(got))
	}
}
