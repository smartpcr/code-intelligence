package degraded

import (
	"sync"
	"sync/atomic"
)

// Metric is the per-verb degraded counter surface the
// implementation-plan §8.1 step 4 wires.  Production
// composition roots can implement it on top of Prometheus /
// OTel; tests use the in-process [Counter].
//
// The interface is deliberately narrow — `IncDegraded(verb,
// reason)` — so the wiring at the verb boundary is a
// one-liner.  Verb names are lower_snake_case and aligned
// with the proto method names (`agent.recall`,
// `agent.observe`, `agent.expand`, `agent.summarize`,
// `mgmt.read.repos`, …).  Reasons are the closed-set
// strings from `reason.go`.
//
// Calls MUST be safe from multiple goroutines (the Observe
// burst test exercises this).
type Metric interface {
	IncDegraded(verb, reason string)
}

// MetricFunc adapts a plain function into a [Metric].
// Useful when the composition root has a Prometheus
// CounterVec it just wants to point at.
type MetricFunc func(verb, reason string)

// IncDegraded implements [Metric].
func (f MetricFunc) IncDegraded(verb, reason string) { f(verb, reason) }

// NopMetric is a [Metric] that discards every call. Useful
// as a default when a service is constructed without a
// real metric wired so call sites never need a nil check.
var NopMetric Metric = MetricFunc(func(string, string) {})

// Counter is the in-process [Metric] implementation. The
// table is sparse (only verb/reason pairs that actually
// fired): pre-registering all 6 reasons per verb would
// require knowing the verb set up front; the test surface
// uses Count / Snapshot to inspect specific pairs without
// caring about absent rows.
//
// Concurrency model: a single sync.RWMutex guards the outer
// map (verb-keyed); each leaf is a `*atomic.Int64` so the
// hot path on a known pair avoids the global lock once the
// pair has been registered. The first call for a new pair
// takes a brief write lock.
type Counter struct {
	mu     sync.RWMutex
	counts map[string]map[string]*atomic.Int64
}

// NewCounter returns a fresh [Counter] with no recorded
// pairs.
func NewCounter() *Counter {
	return &Counter{counts: make(map[string]map[string]*atomic.Int64)}
}

// IncDegraded implements [Metric]. Safe for concurrent use.
//
// The function intentionally does NOT validate `reason`
// against the closed set: callers run [Enforce] before
// invoking IncDegraded so an enforce-rejected response
// never reaches the counter (matching the §13 contract
// that a 500 response is NOT counted as a degraded
// success).
func (c *Counter) IncDegraded(verb, reason string) {
	if c == nil {
		return
	}
	c.mu.RLock()
	if inner, ok := c.counts[verb]; ok {
		if cnt, ok := inner[reason]; ok {
			c.mu.RUnlock()
			cnt.Add(1)
			return
		}
	}
	c.mu.RUnlock()
	c.mu.Lock()
	inner, ok := c.counts[verb]
	if !ok {
		inner = make(map[string]*atomic.Int64, len(AllReasons()))
		c.counts[verb] = inner
	}
	cnt, ok := inner[reason]
	if !ok {
		cnt = new(atomic.Int64)
		inner[reason] = cnt
	}
	c.mu.Unlock()
	cnt.Add(1)
}

// Count returns the recorded count for the (verb, reason)
// pair, or 0 if the pair has never been incremented.
func (c *Counter) Count(verb, reason string) int64 {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	inner, ok := c.counts[verb]
	if !ok {
		return 0
	}
	cnt, ok := inner[reason]
	if !ok {
		return 0
	}
	return cnt.Load()
}

// Snapshot returns a deep copy of the entire counter table.
// Useful for dashboard endpoints; tests prefer Count for
// pinpoint assertions.
func (c *Counter) Snapshot() map[string]map[string]int64 {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]map[string]int64, len(c.counts))
	for v, m := range c.counts {
		inner := make(map[string]int64, len(m))
		for r, cnt := range m {
			inner[r] = cnt.Load()
		}
		out[v] = inner
	}
	return out
}

// Reset clears every recorded count. Tests call this between
// scenarios; production code should never call it.
func (c *Counter) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = make(map[string]map[string]*atomic.Int64)
}
