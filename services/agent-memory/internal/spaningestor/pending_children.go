package spaningestor

// pending_children.go — bounded LRU+TTL index of resolved
// child spans whose parent has NOT yet been seen.
//
// Evaluator iter-2 finding #2: with the in-batch parent map
// and the cross-batch ParentIndex, the ingestor only routes a
// child to an observed-call edge when its parent was already
// resolved (or appeared in the same batch). The opposite case —
// child exported BEFORE parent — is just as common in OTel
// pipelines because the collector flushes by size/time, and
// short child spans complete and export first.
//
// Without a pending index those children were written as solo
// observations on the destination Method and never reconciled
// once the parent arrived. PendingChildIndex parks the resolved
// child observation (its dst target + obs timing) under
// (trace_id, parent_span_id). When a span that matches that
// key later resolves to a Method/Block, the Ingestor drains
// the parked children and emits one observed-call edge per
// child with srcNodeID = the newly-resolved parent target.
//
// Two flush triggers:
//
//   1. Parent-arrived flush: processBatch calls Drain on each
//      Remember call in the per-span resolution pass; any
//      children waiting for that key are immediately written
//      as observed-call edges via the supplied flushHit
//      callback.
//
//   2. TTL flush: the supervisor goroutine (the same one that
//      runs the backpressure tick) calls FlushExpired on each
//      tick. Children whose deadline has passed are written as
//      solo observations via flushTimeout. A counter on the
//      Ingestor surfaces the wait-then-give-up rate so the
//      operator can tune the TTL against trace timing.
//
// Bound: keyCap caps the number of distinct
// (trace_id, parent_span_id) keys; per-key the list of pending
// children is unbounded but in practice short (typically <10).
// At a default cap of 100000 the memory ceiling is ~30 MB.

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// PendingChild is one parked child observation. We retain just
// enough state to emit the eventual write — the resolver does
// not need to be re-run because resolution already happened in
// the original processBatch pass.
type PendingChild struct {
	RepoID       string
	TraceID      string
	SpanID       string
	ParentSpanID string
	DstTarget    ObservationTarget
	Obs          graphwriter.ObservationInput
	AddedAt      time.Time
}

// pendingChildEntry is one cache slot: a queue of children
// waiting on the same (trace_id, parent_span_id) key.
type pendingChildEntry struct {
	key       string
	children  []PendingChild
	expiresAt time.Time
}

// PendingChildIndex is the cross-batch parent-arrival
// reconciliation cache. Safe for concurrent use.
type PendingChildIndex struct {
	mu     sync.Mutex
	keyCap int
	ttl    time.Duration
	lru    *list.List
	byKey  map[string]*list.Element
	now    func() time.Time

	hits          atomic.Int64 // Drain returned >0 children
	adds          atomic.Int64 // Add inserted a new entry or appended
	keyEvictions  atomic.Int64
	ttlEvictions  atomic.Int64
	currentEntries atomic.Int64
}

// newPendingChildIndex constructs a pending-child index. A
// zero keyCap or zero TTL disables the cache (Add is a no-op,
// Drain always misses) — useful for tests that want to keep
// the legacy "write solo immediately" behaviour.
func newPendingChildIndex(keyCap int, ttl time.Duration, now func() time.Time) *PendingChildIndex {
	if now == nil {
		now = time.Now
	}
	return &PendingChildIndex{
		keyCap: keyCap,
		ttl:    ttl,
		lru:    list.New(),
		byKey:  make(map[string]*list.Element),
		now:    now,
	}
}

// Add parks a resolved child under (trace_id, parent_span_id).
// The TTL counts from the moment the FIRST child for this key
// was added; subsequent adds extend the deadline so a busy key
// stays alive while traffic flows.
//
// Returns the slice of children evicted by the LRU cap (if
// any) so the caller can flush them as solo observations
// before they vanish.
func (p *PendingChildIndex) Add(child PendingChild) (evictedTimeout []PendingChild) {
	if p == nil || p.keyCap == 0 || p.ttl == 0 {
		return nil
	}
	if child.TraceID == "" || child.ParentSpanID == "" {
		return nil
	}
	now := p.now()
	key := indexKey(child.TraceID, child.ParentSpanID)

	p.mu.Lock()
	defer p.mu.Unlock()
	if elem, ok := p.byKey[key]; ok {
		entry := elem.Value.(*pendingChildEntry)
		entry.children = append(entry.children, child)
		entry.expiresAt = now.Add(p.ttl)
		p.lru.MoveToFront(elem)
		p.adds.Add(1)
		return nil
	}
	for len(p.byKey) >= p.keyCap {
		tail := p.lru.Back()
		if tail == nil {
			break
		}
		entry := tail.Value.(*pendingChildEntry)
		evictedTimeout = append(evictedTimeout, entry.children...)
		p.lru.Remove(tail)
		delete(p.byKey, entry.key)
		p.keyEvictions.Add(1)
		p.currentEntries.Add(-1)
	}
	entry := &pendingChildEntry{
		key:       key,
		children:  []PendingChild{child},
		expiresAt: now.Add(p.ttl),
	}
	elem := p.lru.PushFront(entry)
	p.byKey[key] = elem
	p.adds.Add(1)
	p.currentEntries.Add(1)
	return evictedTimeout
}

// Drain returns and removes ALL children waiting on the given
// (trace_id, parent_span_id) key. The parent arrived — its
// resolved target should be the src node on every emitted
// observed-call edge.
//
// Returns nil if no children were parked under the key (the
// common case — most spans are not parents of pending
// children).
func (p *PendingChildIndex) Drain(traceID, parentSpanID string) []PendingChild {
	if p == nil || p.keyCap == 0 || p.ttl == 0 {
		return nil
	}
	if traceID == "" || parentSpanID == "" {
		return nil
	}
	key := indexKey(traceID, parentSpanID)

	p.mu.Lock()
	defer p.mu.Unlock()
	elem, ok := p.byKey[key]
	if !ok {
		return nil
	}
	entry := elem.Value.(*pendingChildEntry)
	p.lru.Remove(elem)
	delete(p.byKey, key)
	p.currentEntries.Add(-1)
	p.hits.Add(1)
	return entry.children
}

// FlushExpired walks the LRU tail and removes every entry
// whose deadline has passed. The caller-supplied `onTimeout`
// callback receives each evicted child so it can write the
// solo observation.
//
// Called from the backpressure supervisor on every tick (the
// supervisor goroutine is the only periodic loop the Ingestor
// runs, so we reuse it rather than starting a dedicated one).
func (p *PendingChildIndex) FlushExpired(onTimeout func(PendingChild)) int {
	if p == nil || p.keyCap == 0 || p.ttl == 0 {
		return 0
	}
	now := p.now()
	var batched []PendingChild

	p.mu.Lock()
	for {
		tail := p.lru.Back()
		if tail == nil {
			break
		}
		entry := tail.Value.(*pendingChildEntry)
		if now.Before(entry.expiresAt) {
			// LRU is ordered MRU-front / LRU-back AND we
			// only push-front on add, so the tail is also
			// the oldest deadline. As soon as the tail is
			// fresh, all newer entries are fresher.
			break
		}
		batched = append(batched, entry.children...)
		p.lru.Remove(tail)
		delete(p.byKey, entry.key)
		p.ttlEvictions.Add(1)
		p.currentEntries.Add(-1)
	}
	p.mu.Unlock()

	if onTimeout != nil {
		for _, c := range batched {
			onTimeout(c)
		}
	}
	return len(batched)
}

// Snapshot returns operator-facing counters: (hits, adds,
// keyEvictions, ttlEvictions, currentEntries).
func (p *PendingChildIndex) Snapshot() (hits, adds, keyEvictions, ttlEvictions, current int64) {
	if p == nil {
		return 0, 0, 0, 0, 0
	}
	return p.hits.Load(),
		p.adds.Load(),
		p.keyEvictions.Load(),
		p.ttlEvictions.Load(),
		p.currentEntries.Load()
}

// Size returns the number of distinct keys currently parked.
func (p *PendingChildIndex) Size() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byKey)
}
