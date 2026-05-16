package spaningestor

// Per-edge / per-method latency aggregator with a bounded
// rolling window. This is the in-process compute for the p50 /
// p95 numbers the Span Ingestor (Stage 4.2) writes into the
// `trace_observation` and `method_solo_observation` aggregate
// rows on every observation.
//
// Why in-process (not SQL window functions)
// -----------------------------------------
// Tech-spec §8.7.4 marks `trace_observation` as the
// UPDATE-grantable counter row: the writer takes a row lock on
// the affected edge, runs the UPSERT, and commits. Recomputing
// the median over the full history on every write would (a)
// scan the unbounded `trace_observation_log` partition set —
// `O(N)` per span — and (b) take an exclusive lock long enough
// to serialise the whole pipeline. The in-process window keeps
// the SQL hot path to a single-row UPSERT.
//
// Why a bounded window
// --------------------
// A naive unbounded slice grows without bound for hot edges
// and OOMs the worker before backpressure can clear it. We cap
// the per-key window at `windowCap` recent durations (default
// 256) and we cap the *number of keys* the aggregator tracks
// at `keyCap` via LRU eviction. Both caps are surfaced as
// counters (`window_capacity_evictions_total`,
// `key_capacity_evictions_total`) so the operator dashboard can
// alert if either is non-zero — it means the in-memory window
// is undersized for the production traffic shape and the
// reported quantiles are biased toward recent samples.
//
// Concurrency
// -----------
// Concurrent calls on DIFFERENT keys hit disjoint state and
// can proceed in parallel (per-key sync.Mutex). Concurrent
// calls on the SAME key serialise; ingestion fan-out is per
// repo and the writer pipeline is mostly per-edge, so
// contention on a single key is rare in steady state.
//
// LRU + window updates take a single brief global lock to
// move the key to the front of the recency list. The hot path
// for an already-known key is: global lock (move-to-front) +
// per-key lock (window append + quantile compute). All locks
// are released before the SQL UPSERT runs.

import (
	"container/list"
	"math"
	"sort"
	"sync"
	"sync/atomic"
)

// LatencyAggregator is the per-process latency window
// computer. Construct via NewLatencyAggregator; zero value is
// not usable.
type LatencyAggregator struct {
	mu        sync.Mutex // protects ring + lru
	ring      map[string]*list.Element
	lru       *list.List // most-recent-first
	keyCap    int
	windowCap int

	windowEvictions atomic.Int64
	keyEvictions    atomic.Int64
}

// aggEntry is one ring buffer for a single key. The ring stores
// the most-recent `windowCap` duration samples in millisecond
// precision (float64 so we can preserve sub-millisecond detail
// for fast spans).
type aggEntry struct {
	key    string
	mu     sync.Mutex
	window []float64 // capped at windowCap
	cursor int       // next write index when window is full
	count  int       // number of samples since reset (clipped to windowCap once full)
}

// NewLatencyAggregator constructs an aggregator with the given
// caps. A `keyCap <= 0` or `windowCap <= 0` panics — the caller
// MUST commit to bounded memory.
func NewLatencyAggregator(keyCap, windowCap int) *LatencyAggregator {
	if keyCap <= 0 {
		panic("spaningestor: NewLatencyAggregator: keyCap must be > 0")
	}
	if windowCap <= 0 {
		panic("spaningestor: NewLatencyAggregator: windowCap must be > 0")
	}
	return &LatencyAggregator{
		ring:      make(map[string]*list.Element, keyCap),
		lru:       list.New(),
		keyCap:    keyCap,
		windowCap: windowCap,
	}
}

// Observe records one duration (milliseconds) for the given key
// and returns the post-write p50 / p95 over the rolling window.
// A negative duration is treated as 0 (the caller is expected to
// validate upstream; this is defence-in-depth so a clock jump
// in an emitter doesn't poison the median).
func (a *LatencyAggregator) Observe(key string, durationMs float64) (p50, p95 float64) {
	if durationMs < 0 {
		durationMs = 0
	}
	entry := a.getOrCreate(key)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if len(entry.window) < a.windowCap {
		entry.window = append(entry.window, durationMs)
	} else {
		// Once the window is full, every write evicts the oldest
		// sample. This is the per-key window-capacity-evicted
		// case; we count it for the operator dashboard.
		entry.window[entry.cursor] = durationMs
		entry.cursor = (entry.cursor + 1) % a.windowCap
		a.windowEvictions.Add(1)
	}
	entry.count++
	return quantiles(entry.window)
}

// Snapshot returns the (windowEvictions, keyEvictions) counter
// pair. Used by the binary's metrics endpoint.
func (a *LatencyAggregator) Snapshot() (windowEvictions, keyEvictions int64) {
	return a.windowEvictions.Load(), a.keyEvictions.Load()
}

// Size returns the current number of tracked keys. For tests.
func (a *LatencyAggregator) Size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.ring)
}

// getOrCreate fetches the entry for `key`, allocating it (and
// evicting the LRU entry when at capacity) on miss. Holds the
// global mutex only long enough to mutate the LRU list.
func (a *LatencyAggregator) getOrCreate(key string) *aggEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if elem, ok := a.ring[key]; ok {
		a.lru.MoveToFront(elem)
		return elem.Value.(*aggEntry)
	}
	if len(a.ring) >= a.keyCap {
		// Evict the oldest. The aggEntry's per-key mutex may be
		// held by a concurrent Observer on the same key, but
		// since we are about to drop our reference and no one
		// else can reach this entry through the ring anymore,
		// the in-flight Observer finishes harmlessly against
		// the abandoned object.
		oldest := a.lru.Back()
		if oldest != nil {
			a.lru.Remove(oldest)
			oldKey := oldest.Value.(*aggEntry).key
			delete(a.ring, oldKey)
			a.keyEvictions.Add(1)
		}
	}
	entry := &aggEntry{
		key:    key,
		window: make([]float64, 0, a.windowCap),
	}
	elem := a.lru.PushFront(entry)
	a.ring[key] = elem
	return entry
}

// quantiles returns (p50, p95) over the supplied sample list.
// An empty list returns (0, 0). Computes by sorting a copy so
// the caller's window stays in insertion order (the ring
// buffer's cursor depends on it).
//
// We pick the "nearest-rank" definition: ceil(p * N) - 1 (with
// guards for N == 0 / p == 0). This matches the convention the
// Prometheus quantile() function uses and is what the operator
// dashboard naturally interprets.
func quantiles(samples []float64) (p50, p95 float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	dup := make([]float64, len(samples))
	copy(dup, samples)
	sort.Float64s(dup)
	return nearestRank(dup, 0.50), nearestRank(dup, 0.95)
}

func nearestRank(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	// ceil(p * N) - 1, clamped to [0, N-1]
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
