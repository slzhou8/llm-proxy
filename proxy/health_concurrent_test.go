package proxy

import (
	"sync"
	"testing"

	"llmproxy/config"
)

// TestBreakerConcurrentAccess hammers the breaker from many goroutines at once.
// Without -race this cannot prove the absence of a data race, but it does catch
// deadlocks and map-concurrency panics, which is what the added locking risks.
func TestBreakerConcurrentAccess(t *testing.T) {
	p := newTestProxy(up("a"), up("b"), up("c"))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := []string{"a", "b", "c"}[i%3]
			for j := 0; j < 200; j++ {
				p.recordHealth(name, j%5 == 0, ErrServer)
				p.unhealthy(name)
				p.snapshotGroup(config.ProtocolOpenAI)
				p.BreakerStates()
			}
		}(i)
	}
	wg.Wait()
}
