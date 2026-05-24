package dsl

import (
	"sync"

	"github.com/gofrs/uuid"
)

// Cache memoises compiled [Predicate] instances per
// `(policy_version_id, source string)` pair. Stage 5.4
// brief: "Cache parsed predicates per policy_version_id so
// re-evaluation is hot-path cheap."
//
// The Rule Engine (Stage 5.7) calls [Cache.GetOrCompile] on
// every `eval.gate` invocation; the active policy will
// usually be the same one for thousands of consecutive
// calls, so the cache hit path MUST be a single RWMutex
// RLock + two map lookups followed by a `<-entry.ready`
// channel receive on an already-closed channel (a few
// nanoseconds).
//
// Concurrent misses on different `(policy, source)` keys
// proceed in parallel: the cache mutex is held ONLY long
// enough to insert a placeholder entry; the actual
// [Compile] call (which may call into the
// [ThresholdResolver] and could be slow) runs WITHOUT the
// cache mutex held. Multiple callers racing for the same
// `(policy, source)` key are de-duplicated by waiting on
// the placeholder's `ready` channel, so [Compile] runs at
// most once per key -- the singleflight property.
//
// Cache entries are immutable once written -- [PolicyVersion]
// rows are themselves immutable (architecture G5), so a hit
// always returns a [Predicate] still consistent with the
// policy. To free memory after a policy retirement, call
// [Cache.Invalidate] with the retired policy_version_id.
type Cache struct {
	mu      sync.RWMutex
	entries map[uuid.UUID]map[string]*cacheEntry
}

// cacheEntry is the per-`(policy, source)` placeholder. Its
// `ready` channel is closed once [Compile] returns; readers
// `<-entry.ready` before reading `pred` / `err` (the
// channel-close happens-before guarantee makes the read of
// `pred` / `err` race-free after the receive). The
// `panicVal` field carries a panic value if [Compile]
// panicked; readers re-raise it so a buggy DSL doesn't
// silently produce `(nil, nil)`.
type cacheEntry struct {
	ready    chan struct{}
	pred     *Predicate
	err      error
	panicVal any
}

// NewCache constructs an empty [Cache].
func NewCache() *Cache {
	return &Cache{entries: make(map[uuid.UUID]map[string]*cacheEntry)}
}

// GetOrCompile returns the compiled [Predicate] for
// `(policyVersionID, source)`, compiling it on a miss. The
// resolver is consulted ONLY on the miss path; cached
// entries do not re-resolve thresholds, which preserves the
// Stage 5.4 purity contract over the hot path.
//
// Concurrent callers requesting the same `(policy_version,
// source)` are de-duplicated by the singleflight pattern:
// [Compile] runs at most once and every caller returns the
// same `*Predicate` (or the same error). Concurrent callers
// requesting DIFFERENT keys never block on each other -- a
// slow compile for one key does NOT stall hits or
// compilations for another.
func (c *Cache) GetOrCompile(policyVersionID uuid.UUID, source string, resolver ThresholdResolver) (*Predicate, error) {
	// Hot path: RLock + 2 map lookups + closed-channel
	// receive. The receive on an already-closed channel
	// is a few nanoseconds and provides the
	// happens-before for reading `pred` / `err`.
	c.mu.RLock()
	if perPolicy, ok := c.entries[policyVersionID]; ok {
		if entry, ok := perPolicy[source]; ok {
			c.mu.RUnlock()
			return waitEntry(entry)
		}
	}
	c.mu.RUnlock()

	// Miss: promote to a write Lock, double-check (a
	// concurrent caller may have raced us to install an
	// entry), and either return the racing caller's
	// entry or install our own placeholder and release
	// the mutex BEFORE running [Compile].
	c.mu.Lock()
	perPolicy, ok := c.entries[policyVersionID]
	if !ok {
		perPolicy = make(map[string]*cacheEntry)
		c.entries[policyVersionID] = perPolicy
	}
	if entry, ok := perPolicy[source]; ok {
		c.mu.Unlock()
		return waitEntry(entry)
	}
	entry := &cacheEntry{ready: make(chan struct{})}
	perPolicy[source] = entry
	c.mu.Unlock()

	// Compile WITHOUT holding the cache mutex. Other
	// policy versions and other source strings can
	// compile in parallel; concurrent callers for THIS
	// key wait on entry.ready (see waitEntry).
	//
	// The defer captures any panic from [Compile] so
	// waiters re-raise it rather than observing a silent
	// (nil, nil) result.
	defer func() {
		if r := recover(); r != nil {
			entry.panicVal = r
			close(entry.ready)
			panic(r)
		}
		close(entry.ready)
	}()
	pred, err := Compile(source, resolver)
	entry.pred = pred
	entry.err = err
	return pred, err
}

// waitEntry blocks until entry.ready closes, then re-raises
// any compile-time panic or returns the cached result.
func waitEntry(entry *cacheEntry) (*Predicate, error) {
	<-entry.ready
	if entry.panicVal != nil {
		panic(entry.panicVal)
	}
	return entry.pred, entry.err
}

// Invalidate drops all cached entries for policyVersionID.
// Called when a policy version is retired -- the entries are
// immutable, but the memory they hold can be reclaimed.
//
// Calling Invalidate on an unknown policy_version_id is a
// no-op.
//
// Invalidate is a MEMORY-RECLAMATION HINT, not a hard
// correctness boundary: a [GetOrCompile] call that races
// with Invalidate may re-install an entry for the
// invalidated policy after Invalidate returns. This is
// acceptable for Stage 5.4 because [PolicyVersion] rows are
// immutable (architecture G5), so re-compiling a "retired"
// policy version is semantically equivalent. Callers that
// require post-Invalidate guarantees must externally
// quiesce concurrent GetOrCompile calls for the retired
// policy.
func (c *Cache) Invalidate(policyVersionID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, policyVersionID)
}

// Len returns the count of cached entries. Used by tests
// and an eventual Prometheus gauge.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, perPolicy := range c.entries {
		n += len(perPolicy)
	}
	return n
}

// LenForPolicy returns the count of cached entries for a
// specific policy version. Used by tests asserting the
// per-policy isolation of the cache.
func (c *Cache) LenForPolicy(policyVersionID uuid.UUID) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries[policyVersionID])
}
