package budget

import (
	"sync"
	"testing"
)

func TestConcurrencyLimiterAcquireRelease(t *testing.T) {
	limiter := NewConcurrencyLimiter(nil, 3) // default 3 for this test

	for i := 0; i < 3; i++ {
		if !limiter.Acquire("agent-a") {
			t.Fatalf("acquire %d should succeed under limit", i)
		}
	}
	if limiter.Acquire("agent-a") {
		t.Fatal("acquire at limit should fail")
	}
	limiter.Release("agent-a")
	if !limiter.Acquire("agent-a") {
		t.Fatal("acquire after release should succeed")
	}
}

func TestConcurrencyLimiterPerAgentIsolation(t *testing.T) {
	limiter := NewConcurrencyLimiter(nil, 1)
	if !limiter.Acquire("agent-a") {
		t.Fatal("agent-a first acquire should succeed")
	}
	if !limiter.Acquire("agent-b") {
		t.Fatal("agent-b is independent and should succeed")
	}
}

func TestConcurrencyLimiterPerAgentLimit(t *testing.T) {
	limiter := NewConcurrencyLimiter(map[string]int64{"vip": 2}, 50)
	if !limiter.Acquire("vip") {
		t.Fatal("first vip acquire should succeed")
	}
	if !limiter.Acquire("vip") {
		t.Fatal("second vip acquire should succeed")
	}
	if limiter.Acquire("vip") {
		t.Fatal("third vip acquire should fail (configured limit 2)")
	}
}

func TestConcurrencyLimiterRaceSafe(t *testing.T) {
	limiter := NewConcurrencyLimiter(nil, 100)
	var waitGroup sync.WaitGroup
	for i := 0; i < 500; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if limiter.Acquire("agent-a") {
				limiter.Release("agent-a")
			}
		}()
	}
	waitGroup.Wait()
	// After all paired acquire/release settle, count must be 0.
	if got := limiter.current("agent-a"); got != 0 {
		t.Fatalf("counter leaked: got %d, want 0", got)
	}
}
