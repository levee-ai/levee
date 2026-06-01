package budget

import (
	"sync"
	"sync/atomic"
)

// ConcurrencyLimiter bounds the number of simultaneous in-flight requests per
// agent. The counter is incremented inside the store's Reserve and decremented
// in Reconcile and Forfeit. It is a separate structure from budget windows: a
// stream slot is not a budget resource, it is infrastructure protection.
type ConcurrencyLimiter struct {
	limits       map[string]int64
	defaultLimit int64

	mutex    sync.RWMutex
	counters map[string]*atomic.Int64
}

// NewConcurrencyLimiter builds a limiter. perAgentLimits may be nil. Any agent
// without an entry uses defaultLimit (the project default is 50).
func NewConcurrencyLimiter(perAgentLimits map[string]int64, defaultLimit int64) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		limits:       perAgentLimits,
		defaultLimit: defaultLimit,
		counters:     make(map[string]*atomic.Int64),
	}
}

// limitFor returns the configured limit for an agent or the default.
func (limiter *ConcurrencyLimiter) limitFor(agentName string) int64 {
	if value, ok := limiter.limits[agentName]; ok {
		return value
	}
	return limiter.defaultLimit
}

// counterFor returns the agent's counter, creating it on first use.
func (limiter *ConcurrencyLimiter) counterFor(agentName string) *atomic.Int64 {
	limiter.mutex.RLock()
	counter, ok := limiter.counters[agentName]
	limiter.mutex.RUnlock()
	if ok {
		return counter
	}
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	if counter, ok = limiter.counters[agentName]; ok {
		return counter
	}
	counter = &atomic.Int64{}
	limiter.counters[agentName] = counter
	return counter
}

// Acquire reserves one stream slot. It returns false if the agent is already
// at its limit (the increment is rolled back in that case). Safe under
// concurrent callers via atomic add-then-check.
func (limiter *ConcurrencyLimiter) Acquire(agentName string) bool {
	counter := limiter.counterFor(agentName)
	if counter.Add(1) > limiter.limitFor(agentName) {
		counter.Add(-1)
		return false
	}
	return true
}

// Release returns one stream slot.
func (limiter *ConcurrencyLimiter) Release(agentName string) {
	counter := limiter.counterFor(agentName)
	counter.Add(-1)
}

// current reports the live count for an agent (test helper).
func (limiter *ConcurrencyLimiter) current(agentName string) int64 {
	return limiter.counterFor(agentName).Load()
}
