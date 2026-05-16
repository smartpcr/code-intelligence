package spaningestor

// Cross-batch parent-span index.
//
// Evaluator iter-1 finding #2: "only resolves parent_span_id
// when the parent span is in the same batch — normal collector
// batching can silently lose observed_calls edges." The collector
// flushes by size/time, so parent and child spans of one trace
// commonly arrive in distinct exports. Without an index, every
// such child fell through to the destination-Method solo
// aggregate and the edge was never observed.
//
// Design choice: bounded in-process LRU + TTL cache keyed by
// (trace_id, span_id) -> ObservationTarget {NodeID, NodeKind}.
// In-process is sufficient because:
//
//   * The Span Ingestor is single-instance per repo by §4.2
//     deployment model (cmd/span-ingestor composition root in
//     the binary is a singleton).
//   * Cross-process scale-out would need a persistent table
//     (e.g. `recent_span_resolution`); deferred to v2 with a
//     follow-up workstream because it requires a new
//     partitioned table + writer extension + retention job.
//
// TTL is wall-clock based (default 10 minutes) so a long-lived
// trace whose parent arrived far in the past does not lock
// memory indefinitely.

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

// ObservationTarget identifies a Node a span resolved to. The
// `Kind` field is used so the cross-batch parent lookup can
// preserve "child observed against Block X" → its parent should
// also link to Block X if the resolver chose Block.
type ObservationTarget struct {
	NodeID string
	// Kind is the resolver's outcome ("method" or "block"); used
	// only for diagnostics today, retained on the cache entry
	// so a v2 dashboard can answer "what fraction of cross-batch
	// hits resolved to a Block".
	Kind string
}

// parentIndexEntry is one cache record.
type parentIndexEntry struct {
	key      string // trace_id + "\x00" + span_id (stable separator)
	target   ObservationTarget
	expiresAt time.Time
}

// ParentIndex is the bounded LRU+TTL cache. Safe for
// concurrent use. Construct via newParentIndex.
type ParentIndex struct {
	mu       sync.Mutex
	keyCap   int
	ttl      time.Duration
	lru      *list.List // most-recently-touched first
	byKey    map[string]*list.Element
	now      func() time.Time

	hits           atomic.Int64
	misses         atomic.Int64
	keyEvictions   atomic.Int64
	ttlEvictions   atomic.Int64
}

// newParentIndex constructs a parent index. A keyCap of 0 / TTL
// of 0 disables the cache (every Lookup is a miss). This is
// the right behaviour for tests that don't want the index in the
// way.
func newParentIndex(keyCap int, ttl time.Duration, now func() time.Time) *ParentIndex {
	if now == nil {
		now = time.Now
	}
	return &ParentIndex{
		keyCap: keyCap,
		ttl:    ttl,
		lru:    list.New(),
		byKey:  make(map[string]*list.Element),
		now:    now,
	}
}

func indexKey(traceID, spanID string) string {
	return traceID + "\x00" + spanID
}

// Remember inserts or refreshes the (trace,span) → target
// mapping. A zero TTL or zero keyCap makes Remember a no-op.
func (p *ParentIndex) Remember(traceID, spanID string, target ObservationTarget) {
	if p == nil || p.keyCap == 0 || p.ttl == 0 || traceID == "" || spanID == "" || target.NodeID == "" {
		return
	}
	now := p.now()
	key := indexKey(traceID, spanID)

	p.mu.Lock()
	defer p.mu.Unlock()
	if elem, ok := p.byKey[key]; ok {
		// Update in place.
		entry := elem.Value.(*parentIndexEntry)
		entry.target = target
		entry.expiresAt = now.Add(p.ttl)
		p.lru.MoveToFront(elem)
		return
	}
	for len(p.byKey) >= p.keyCap {
		// Evict the LRU tail.
		tail := p.lru.Back()
		if tail == nil {
			break
		}
		p.lru.Remove(tail)
		delete(p.byKey, tail.Value.(*parentIndexEntry).key)
		p.keyEvictions.Add(1)
	}
	entry := &parentIndexEntry{
		key:       key,
		target:    target,
		expiresAt: now.Add(p.ttl),
	}
	elem := p.lru.PushFront(entry)
	p.byKey[key] = elem
}

// Lookup returns (target, true) on a fresh hit; (zero, false)
// on miss or expired. Expired entries are evicted lazily on
// lookup so a long-quiet trace does not waste memory.
func (p *ParentIndex) Lookup(traceID, spanID string) (ObservationTarget, bool) {
	if p == nil || p.keyCap == 0 || p.ttl == 0 || traceID == "" || spanID == "" {
		return ObservationTarget{}, false
	}
	key := indexKey(traceID, spanID)
	now := p.now()

	p.mu.Lock()
	defer p.mu.Unlock()
	elem, ok := p.byKey[key]
	if !ok {
		p.misses.Add(1)
		return ObservationTarget{}, false
	}
	entry := elem.Value.(*parentIndexEntry)
	if now.After(entry.expiresAt) {
		p.lru.Remove(elem)
		delete(p.byKey, key)
		p.ttlEvictions.Add(1)
		p.misses.Add(1)
		return ObservationTarget{}, false
	}
	p.lru.MoveToFront(elem)
	p.hits.Add(1)
	return entry.target, true
}

// Snapshot returns operator-facing counters: (hits, misses,
// keyEvictions, ttlEvictions). Exposed for the /metrics handler
// and for tests.
func (p *ParentIndex) Snapshot() (hits, misses, keyEvictions, ttlEvictions int64) {
	if p == nil {
		return 0, 0, 0, 0
	}
	return p.hits.Load(), p.misses.Load(), p.keyEvictions.Load(), p.ttlEvictions.Load()
}

// Size returns the current entry count (for tests).
func (p *ParentIndex) Size() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byKey)
}
