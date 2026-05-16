package spaningestor

// Ingestor is the §4.2 Span Ingestor worker that the Span
// Ingestor binary (`cmd/span-ingestor`) hosts. It:
//
//  1. Accepts batches of normalized OTel spans via `Enqueue`
//     (the OTLP/HTTP receiver in `otlphttp.go` is one caller).
//  2. Drains the per-instance bounded queue from `Run` and, for
//     each batch, resolves every span via the Stage 4.1 Resolver.
//  3. Within a batch, resolves the caller side via
//     `parent_span_id` (tech-spec §8.6 row 3): a span whose
//     parent is in the same batch becomes an `observed_calls`
//     Edge from parent's Method → this span's Method; a span
//     with no parent (root) becomes a `method_solo_observation`
//     on the destination Method.
//  4. Computes p50 / p95 over a per-edge / per-method rolling
//     window (LatencyAggregator) and writes via the §4.2
//     GraphWriter extensions (`AppendObservedCallTrace`,
//     `AppendSoloMethodObservation`).
//  5. Watches the queue depth against a configurable threshold
//     and, when the depth exceeds the threshold for `sustain`
//     wall-clock, UPSERTs `repo_health` to `degraded=true,
//     reason=span_ingestor_backpressure` (per tech-spec §C22 +
//     §8.3 SLO envelope).
//
// What the Ingestor does NOT do
// -----------------------------
//   * Cross-process parent resolution. Cross-BATCH parent
//     resolution within a single Ingestor process IS supported
//     via the in-process `ParentIndex` LRU+TTL cache (see
//     parent_index.go). A cross-PROCESS variant for multi-
//     instance scale-out would need a durable parent-span
//     index (e.g. a partitioned `recent_span_resolution`
//     table); that is a separate workstream because it pulls
//     a new hot-path table + writer extension + retention
//     job.
//
//   * Cross-process SHA recovery. The per-repo SHA cache
//     (`SHACache`) is in-process. If the binary crashes mid-
//     batch the next process pays one extra Postgres query
//     per repo to repopulate; no correctness concern.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// DegradedReasonBackpressure is the closed-set ENUM literal the
// agent_memory schema accepts on `repo_health.degraded_reason`
// when the Span Ingestor's queue depth exceeds the §8.3
// sustained envelope. Centralized here so a typo in the
// supervisor doesn't silently fail the UPSERT.
const DegradedReasonBackpressure = "span_ingestor_backpressure"

// ingestorSource identifies this worker class on
// `repo_health.source`. Free-form text by schema design; we
// reserve this exact literal for this binary so operators can
// filter the health rows by author.
const ingestorSource = "span-ingestor"

// SpanBatch is the unit `Enqueue` accepts. The OTLP receiver
// (`otlphttp.go`) builds these from incoming export requests;
// integration tests in this package may construct them
// directly.
//
// All spans in a batch MUST share `RepoID` so the within-batch
// parent map and the per-batch backpressure account stay
// coherent. The receiver validates this before calling
// `Enqueue`; the Ingestor re-validates as defence-in-depth.
type SpanBatch struct {
	RepoID string
	Spans  []ObservationSpan
}

// ObservationSpan is the per-span unit the Ingestor receives:
// the Stage-4.1 Span (consumed by the Resolver) PLUS the two
// timing fields the Resolver does not care about but the
// observation writer needs.
//
// We keep `Span` as the embedded value so resolver.Resolve(s.Span)
// reads naturally; StartedAt and DurationMs are bound to the
// outer struct because the Resolver's pure-function contract
// must not need a "duration" field to do its work.
type ObservationSpan struct {
	Span
	StartedAt  time.Time
	DurationMs float64
}

// IngestorMetrics holds the per-repo counters the Ingestor
// publishes. Mirrors `Resolver.Metrics` in shape and lock
// discipline. Counters are written to under no SQL lock; reads
// via Snapshot acquire the rw-mutex briefly for a consistent
// view.
//
// Why these specific counters
// ---------------------------
//   * span_ingested_total: dashboard top-line for offered load.
//   * span_dropped_total:  shed-load count (queue full at
//     Enqueue time). Tripping non-zero is the operator's signal
//     that the queue is undersized OR that downstream writers
//     are pinned on contention.
//   * solo_aggregates_total: root spans observed. Healthy for
//     entry-point traffic, but a sudden ratio change implies an
//     emitter regression (e.g. missing parent_span_id).
//   * parent_span_missing_total: spans whose parent was NOT in
//     the same batch. Cross-batch parent resolution is v2;
//     watching this counter tells us when it becomes worth
//     building.
type IngestorMetrics struct {
	mu sync.RWMutex

	spansIngested  map[string]*atomic.Int64
	spansDropped   map[string]*atomic.Int64
	soloAggregates map[string]*atomic.Int64
	parentMissing  map[string]*atomic.Int64
	// shaLookupErrors counts spans whose batch was dropped at
	// the writer boundary because the per-repo current_head_sha
	// lookup returned an error (DB outage, permission denied,
	// transient). Evaluator iter-2 #4: silently falling back to
	// the "observed" sentinel on error would pollute G2
	// fingerprints with an incorrect SHA; instead we drop the
	// edge and surface the gap on a dedicated counter so the
	// operator can correlate with the DB error.
	shaLookupErrors map[string]*atomic.Int64
	// parentArrivedLate counts pending children that found
	// their parent via the PendingChildIndex flush path
	// (evaluator iter-2 #2). A non-zero value indicates the
	// out-of-order reconciliation is doing useful work.
	parentArrivedLate map[string]*atomic.Int64
	// parentNeverArrived counts pending children that were
	// evicted from PendingChildIndex by the TTL flush before
	// any parent arrived; they were written as solo aggregates
	// with reason="pending_child_ttl_expired". Useful for
	// tuning the TTL against trace timing distributions.
	parentNeverArrived map[string]*atomic.Int64
	// inflight is the per-repo count of spans currently
	// either queued or being processed. Evaluator iter-1 #6:
	// the backpressure supervisor used to attribute degraded
	// to every repo that ever ingested (lifetime metric),
	// which falsely degraded stale repos. inflight gives an
	// "is this repo actually contributing to the backlog
	// right now" signal — the supervisor only marks repos
	// with inflight > 0.
	inflight map[string]*atomic.Int64
}

// NewIngestorMetrics constructs ready-to-use counters.
func NewIngestorMetrics() *IngestorMetrics {
	return &IngestorMetrics{
		spansIngested:      make(map[string]*atomic.Int64),
		spansDropped:       make(map[string]*atomic.Int64),
		soloAggregates:     make(map[string]*atomic.Int64),
		parentMissing:      make(map[string]*atomic.Int64),
		shaLookupErrors:    make(map[string]*atomic.Int64),
		parentArrivedLate:  make(map[string]*atomic.Int64),
		parentNeverArrived: make(map[string]*atomic.Int64),
		inflight:           make(map[string]*atomic.Int64),
	}
}

func (m *IngestorMetrics) inc(bucket map[string]*atomic.Int64, repoID string) {
	if m == nil {
		return
	}
	m.mu.RLock()
	c, ok := bucket[repoID]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		c, ok = bucket[repoID]
		if !ok {
			c = new(atomic.Int64)
			bucket[repoID] = c
		}
		m.mu.Unlock()
	}
	c.Add(1)
}

func (m *IngestorMetrics) add(bucket map[string]*atomic.Int64, repoID string, delta int64) {
	if m == nil {
		return
	}
	m.mu.RLock()
	c, ok := bucket[repoID]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		c, ok = bucket[repoID]
		if !ok {
			c = new(atomic.Int64)
			bucket[repoID] = c
		}
		m.mu.Unlock()
	}
	c.Add(delta)
}

// IncIngested is the per-span counter incremented exactly once
// per successfully-enqueued span (NOT per-batch).
func (m *IngestorMetrics) IncIngested(repoID string) { m.inc(m.spansIngested, repoID) }

// AddIngested adjusts the per-repo ingested counter by delta.
// Normal accumulation should prefer IncIngested so the per-span
// semantics stay obvious at the call site; AddIngested exists
// so EnqueueAtomic's defensive partial-overflow rollback can
// symmetrically undo the IncIngested calls for batches that the
// rollback retroactively promotes into the dropped ledger.
// Without this decrement, ingested + dropped would double-count
// the same spans and corrupt the operator drop-rate ratio
// (span_dropped_total / span_ingested_total).
func (m *IngestorMetrics) AddIngested(repoID string, delta int64) {
	m.add(m.spansIngested, repoID, delta)
}

// IncDropped is the per-span counter incremented when a batch
// is rejected at `Enqueue` due to queue-full backpressure. The
// caller MUST add one per dropped span (not one per dropped
// batch) so the dashboard ratio against ingested stays
// interpretable.
func (m *IngestorMetrics) IncDropped(repoID string) { m.inc(m.spansDropped, repoID) }

// IncSoloAggregate counts root spans (or cross-batch-parent
// spans) routed to the destination-Method solo aggregate.
func (m *IngestorMetrics) IncSoloAggregate(repoID string) { m.inc(m.soloAggregates, repoID) }

// IncParentMissing counts spans whose parent_span_id was set
// but did not appear in the same batch (cross-batch parent).
func (m *IngestorMetrics) IncParentMissing(repoID string) { m.inc(m.parentMissing, repoID) }

// IncShaLookupError counts spans whose enclosing batch could
// not write an observed-call edge because the per-repo
// current_head_sha lookup raised an error. Evaluator iter-2 #4:
// a non-zero value indicates a DB-level problem; the spans are
// NOT written under the "observed" sentinel because that would
// silently corrupt G2 fingerprints.
func (m *IngestorMetrics) IncShaLookupError(repoID string) {
	m.inc(m.shaLookupErrors, repoID)
}

// IncParentArrivedLate counts pending children that found
// their parent via the PendingChildIndex flush path.
func (m *IngestorMetrics) IncParentArrivedLate(repoID string) {
	m.inc(m.parentArrivedLate, repoID)
}

// IncParentNeverArrived counts pending children evicted from
// PendingChildIndex by the TTL flush before a parent was seen.
func (m *IngestorMetrics) IncParentNeverArrived(repoID string) {
	m.inc(m.parentNeverArrived, repoID)
}

// AddInflight adjusts the per-repo in-flight counter by delta.
// Called with +1 per span at Enqueue time and -1 per span when
// processBatch completes for the span. The backpressure
// supervisor reads InflightSnapshot to attribute the degraded
// flag only to repos contributing to the current backlog.
func (m *IngestorMetrics) AddInflight(repoID string, delta int64) {
	m.add(m.inflight, repoID, delta)
}

// InflightSnapshot returns a {repoID -> current in-flight count}
// view. Used by the backpressure supervisor and exposed on
// /metrics. Repos with a zero value MUST be filtered by the
// caller so the supervisor doesn't degrade a quiesced repo.
func (m *IngestorMetrics) InflightSnapshot() map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return snapshot(m.inflight, &m.mu)
}

// SnapshotCounters returns a flat counter map for the operator
// dashboard / tests.
func (m *IngestorMetrics) SnapshotCounters() map[string]map[string]int64 {
	if m == nil {
		return map[string]map[string]int64{}
	}
	out := map[string]map[string]int64{
		"span_ingested_total":         snapshot(m.spansIngested, &m.mu),
		"span_dropped_total":          snapshot(m.spansDropped, &m.mu),
		"solo_aggregates_total":       snapshot(m.soloAggregates, &m.mu),
		"parent_span_missing_total":   snapshot(m.parentMissing, &m.mu),
		"sha_lookup_error_total":      snapshot(m.shaLookupErrors, &m.mu),
		"parent_arrived_late_total":   snapshot(m.parentArrivedLate, &m.mu),
		"parent_never_arrived_total":  snapshot(m.parentNeverArrived, &m.mu),
		"span_inflight":               snapshot(m.inflight, &m.mu),
	}
	return out
}

func snapshot(bucket map[string]*atomic.Int64, mu *sync.RWMutex) map[string]int64 {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]int64, len(bucket))
	for k, v := range bucket {
		out[k] = v.Load()
	}
	return out
}

// HealthWriter is the narrow GraphWriter surface the Ingestor
// needs for the backpressure side. The full `*graphwriter.Writer`
// satisfies it; tests pass a fake.
type HealthWriter interface {
	UpsertRepoHealth(ctx context.Context, in graphwriter.HealthInput) (graphwriter.HealthRecord, error)
}

// TraceWriter is the narrow GraphWriter surface the Ingestor
// needs for the observation hot path. The full
// `*graphwriter.Writer` satisfies it; tests pass a fake.
type TraceWriter interface {
	AppendObservedCallTrace(
		ctx context.Context,
		edgeIn graphwriter.EdgeInput,
		obs graphwriter.ObservationInput,
	) (graphwriter.ObservedCallTraceRecord, error)
	AppendSoloMethodObservation(
		ctx context.Context, methodNodeID string, obs graphwriter.ObservationInput,
	) (graphwriter.SoloObservationRecord, error)
}

// MethodResolver is the narrow Resolver surface the Ingestor
// needs. The full `*Resolver` satisfies it; tests pass a fake.
type MethodResolver interface {
	Resolve(ctx context.Context, span Span) (Resolution, error)
}

// Config holds the Ingestor's tunables. Zero values are valid
// (defaults applied in New).
type Config struct {
	// QueueDepth is the bounded channel capacity. Spans
	// arriving when the channel is full are dropped (and
	// counted under `span_dropped_total`).
	QueueDepth int
	// BackpressureThreshold is the depth-watermark above
	// which the supervisor begins counting toward the
	// degraded-state transition. Per §8.3 the sustained
	// envelope for agent.observe is 50 RPS — Scenario 3 of
	// the workstream says "2x of envelope for 30s" trips
	// backpressure, so the default is QueueDepth >= 2*50 = 100.
	BackpressureThreshold int
	// BackpressureSustain is the wall-clock window the depth
	// must stay AT OR ABOVE BackpressureThreshold before
	// `repo_health.degraded` is set to true. Default 30s
	// per Scenario 3.
	BackpressureSustain time.Duration
	// BackpressureClearance is the symmetric cooldown window
	// the depth must stay BELOW BackpressureThreshold before
	// the flag is cleared. Default same as Sustain so the
	// flap pattern stays symmetric.
	BackpressureClearance time.Duration
	// WindowKeyCap caps the LRU footprint of the
	// LatencyAggregator (number of distinct edges/methods).
	// Default 10000.
	WindowKeyCap int
	// WindowSizeCap caps the per-key sample buffer. Default 256.
	WindowSizeCap int
	// HealthSupervisorInterval is how often the supervisor
	// checks the depth. Default 1s.
	HealthSupervisorInterval time.Duration
	// ParentCacheKeyCap caps the cross-batch parent-span LRU.
	// Contract:
	//   * negative (e.g. `-1`) — cache DISABLED. Cross-batch
	//     parents are not looked up and every cross-batch parent
	//     becomes a solo aggregate, matching v1 behaviour pre-fix.
	//     Useful for unit tests that want deterministic behaviour
	//     without the LRU.
	//   * zero — use the default capacity (100000, ≈ 10 MB).
	//   * positive — explicit capacity.
	ParentCacheKeyCap int
	// ParentCacheTTL bounds how long a parent-span resolution
	// is retained for cross-batch lookup. Default 10 minutes.
	// Most OTel traces complete within a few seconds; 10 min is
	// generous headroom for long-running batch jobs.
	ParentCacheTTL time.Duration
	// SHACacheTTL bounds how long the per-repo current_head_sha
	// stays cached before a re-query. Default 30s.
	SHACacheTTL time.Duration
	// PendingChildKeyCap caps the number of distinct
	// (trace_id, parent_span_id) keys parked in the cross-batch
	// parent-arrived-late index. Contract:
	//   * negative (e.g. `-1`) — out-of-order reconciliation
	//     DISABLED. Children whose parent has not arrived become
	//     solo observations immediately, matching v1 behaviour
	//     pre-fix. Useful for unit tests.
	//   * zero — use the default capacity (100000, ≈ 30 MB).
	//   * positive — explicit capacity.
	PendingChildKeyCap int
	// PendingChildTTL bounds how long a parked child waits
	// for its parent before being flushed as a solo
	// observation. Default 10 minutes — same as
	// ParentCacheTTL so the two cross-batch caches expire
	// on the same wall-clock budget.
	PendingChildTTL time.Duration
}

func (c *Config) applyDefaults() {
	if c.QueueDepth <= 0 {
		c.QueueDepth = 1024
	}
	if c.BackpressureThreshold <= 0 {
		c.BackpressureThreshold = 100
	}
	if c.BackpressureSustain <= 0 {
		c.BackpressureSustain = 30 * time.Second
	}
	if c.BackpressureClearance <= 0 {
		c.BackpressureClearance = c.BackpressureSustain
	}
	if c.WindowKeyCap <= 0 {
		c.WindowKeyCap = 10000
	}
	if c.WindowSizeCap <= 0 {
		c.WindowSizeCap = 256
	}
	if c.HealthSupervisorInterval <= 0 {
		c.HealthSupervisorInterval = time.Second
	}
	if c.ParentCacheKeyCap < 0 {
		c.ParentCacheKeyCap = 0
	}
	if c.ParentCacheKeyCap == 0 {
		c.ParentCacheKeyCap = 100000
	}
	if c.ParentCacheTTL <= 0 {
		c.ParentCacheTTL = 10 * time.Minute
	}
	if c.SHACacheTTL <= 0 {
		c.SHACacheTTL = 30 * time.Second
	}
	if c.PendingChildKeyCap < 0 {
		c.PendingChildKeyCap = 0
	}
	if c.PendingChildKeyCap == 0 {
		c.PendingChildKeyCap = 100000
	}
	if c.PendingChildTTL <= 0 {
		c.PendingChildTTL = 10 * time.Minute
	}
}

// Ingestor is the worker. Construct via NewIngestor; never
// share an Ingestor between two Run loops.
type Ingestor struct {
	cfg     Config
	queue   chan SpanBatch
	resolver MethodResolver
	writer   TraceWriter
	health   HealthWriter
	agg      *LatencyAggregator
	metrics  *IngestorMetrics
	logger   *slog.Logger
	now      func() time.Time
	idLookup MethodIDResolver

	// parentIndex is the cross-batch parent-span resolution
	// cache (LRU + TTL). Evaluator iter-1 #2: with the
	// in-batch-only parent map, normal collector batching
	// silently lost observed_calls edges. parentIndex covers
	// the cross-batch case.
	parentIndex *ParentIndex

	// shaCache is the per-repo current_head_sha TTL cache.
	// Evaluator iter-1 #7: replaces the hardcoded "observed"
	// sentinel on EdgeInput.FromSHA with the live SHA so the
	// G2 fingerprint preimage matches the snapshot in effect
	// when the span was observed.
	shaCache *SHACache

	// pendingChildren is the cross-batch parent-arrived-late
	// reconciliation cache (evaluator iter-2 #2). When a child
	// span resolves but its parent is in neither the in-batch
	// parentMap nor the cross-batch ParentIndex, the resolved
	// child is parked here under (trace_id, parent_span_id).
	// Subsequent batches whose Remember() touches that key
	// drain the parked children and emit observed-call edges
	// with the now-known parent target as the src. TTL-expired
	// keys are flushed as solo observations by the supervisor
	// tick. nil when the cache is disabled.
	pendingChildren *PendingChildIndex

	// enqueueMu serializes ALL producer paths (Enqueue +
	// EnqueueAtomic). Evaluator iter-1 #5 / iter-2 #1: the
	// previous design left single-batch Enqueue mutex-free,
	// which allowed a concurrent single-batch send to consume
	// the capacity EnqueueAtomic had just checked under the
	// mutex, causing the multi-batch send to block on a full
	// channel instead of returning ErrQueueFull. Holding the
	// mutex across the precheck + send in BOTH paths makes the
	// "check capacity, never block, return ErrQueueFull on
	// overflow" contract atomic against other producers (the
	// drainer Run loop only consumes from the channel, never
	// produces, so it does NOT take this mutex).
	enqueueMu sync.Mutex

	// degradedState tracks per-repo backpressure book-keeping
	// the supervisor reads on each tick. Map mutations happen
	// only inside the supervisor goroutine, so no mutex is
	// needed for the map itself; the per-repo struct lives on
	// the heap and is read by the supervisor only.
	degradedState map[string]*backpressureState
}

// MethodIDResolver maps a resolver-side MethodCandidate to the
// `node.node_id` (uuid) the GraphWriter takes on its EdgeInput.
// The resolver already carries `NodeID` on MethodCandidate, but
// it is a string — this interface exists so a test can override
// the lookup (e.g. to inject deliberately-wrong IDs for
// negative cases) without redefining the resolver.
type MethodIDResolver interface {
	// MethodNodeID returns the node_id for the given Method
	// candidate. The default implementation just returns
	// `cand.NodeID`.
	MethodNodeID(cand *MethodCandidate) string
}

// defaultMethodIDResolver is the production wiring — the
// resolver's MethodCandidate already carries the node_id.
type defaultMethodIDResolver struct{}

func (defaultMethodIDResolver) MethodNodeID(cand *MethodCandidate) string {
	if cand == nil {
		return ""
	}
	return cand.NodeID
}

// NewIngestor constructs a ready-to-run Ingestor. Wire it via
// `Run(ctx)` and feed it via `Enqueue(batch)`.
//
// Nil checks:
//   * `resolver` MUST be non-nil; nil panics.
//   * `writer` MUST be non-nil; nil panics.
//   * `health` MAY be nil — the Ingestor will not raise the
//     backpressure flag and the supervisor goroutine is skipped.
//     Useful for unit tests that don't want a Postgres dep.
//   * `logger` defaults to slog.Default().
//
// Optional wiring after construction:
//   * `SetSHAReader` — see evaluator iter-1 #7 fix; without it,
//     observed-call edges carry the documented "observed"
//     sentinel as a fallback.
//   * `EnableParentIndex` is called automatically with the
//     defaults from `cfg.ParentCacheKeyCap` /
//     `cfg.ParentCacheTTL`; tests that want to disable it can
//     set `cfg.ParentCacheKeyCap = -1`.
func NewIngestor(
	resolver MethodResolver,
	writer TraceWriter,
	health HealthWriter,
	cfg Config,
	logger *slog.Logger,
) *Ingestor {
	if resolver == nil {
		panic("spaningestor: NewIngestor: nil resolver")
	}
	if writer == nil {
		panic("spaningestor: NewIngestor: nil writer")
	}
	disableParentIndex := cfg.ParentCacheKeyCap < 0
	disablePending := cfg.PendingChildKeyCap < 0
	cfg.applyDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	now := time.Now
	i := &Ingestor{
		cfg:           cfg,
		queue:         make(chan SpanBatch, cfg.QueueDepth),
		resolver:      resolver,
		writer:        writer,
		health:        health,
		agg:           NewLatencyAggregator(cfg.WindowKeyCap, cfg.WindowSizeCap),
		metrics:       NewIngestorMetrics(),
		logger:        logger,
		now:           now,
		idLookup:      defaultMethodIDResolver{},
		degradedState: make(map[string]*backpressureState),
	}
	if !disableParentIndex {
		i.parentIndex = newParentIndex(cfg.ParentCacheKeyCap, cfg.ParentCacheTTL, now)
	}
	if !disablePending {
		i.pendingChildren = newPendingChildIndex(cfg.PendingChildKeyCap, cfg.PendingChildTTL, now)
	}
	// shaCache starts wired to a nil reader so Get returns
	// ("", nil) and the ingestor falls back to the documented
	// sentinel. SetSHAReader installs the real reader.
	i.shaCache = newSHACache(nil, cfg.SHACacheTTL, now)
	return i
}

// SetSHAReader installs the per-repo current_head_sha reader
// the ingestor uses to populate `EdgeInput.FromSHA` on
// observed-call edges (evaluator iter-1 #7). Calling with nil
// reverts to the "observed" sentinel fallback. Safe to call
// only before `Run` starts; not goroutine-safe with a running
// loop.
func (i *Ingestor) SetSHAReader(r SHAReader) {
	i.shaCache = newSHACache(r, i.cfg.SHACacheTTL, i.now)
}

// ParentIndex returns the cross-batch parent-span cache for
// the /metrics endpoint and for tests. nil when the cache is
// disabled.
func (i *Ingestor) ParentIndex() *ParentIndex { return i.parentIndex }

// PendingChildren returns the cross-batch
// parent-arrived-late reconciliation cache (evaluator iter-2
// #2). nil when the cache is disabled. Used by /metrics and
// tests; production code drives it implicitly through
// processBatch + the supervisor TTL flush.
func (i *Ingestor) PendingChildren() *PendingChildIndex { return i.pendingChildren }

// SHACache returns the per-repo SHA cache for the /metrics
// endpoint and for tests.
func (i *Ingestor) SHACache() *SHACache { return i.shaCache }

// Metrics returns the live IngestorMetrics for the operator
// dashboard / tests.
func (i *Ingestor) Metrics() *IngestorMetrics { return i.metrics }

// Aggregator returns the live LatencyAggregator (for tests /
// dashboard).
func (i *Ingestor) Aggregator() *LatencyAggregator { return i.agg }

// QueueDepth returns the current in-channel depth (for tests
// and the metrics endpoint).
func (i *Ingestor) QueueDepth() int { return len(i.queue) }

// ErrQueueFull is the typed error `Enqueue` returns when the
// bounded channel is full. The receiver translates this to an
// HTTP 503 + Retry-After header.
var ErrQueueFull = errors.New("spaningestor: queue full")

// Enqueue submits a batch for processing. Returns ErrQueueFull
// immediately (no blocking) when the bounded channel cannot
// accept the batch; the receiver MUST translate that to a 503
// so the OTel collector retries with backoff rather than piling
// up in the worker's process memory.
//
// On enqueue success, `IncIngested(repoID)` is called once per
// span. On `ErrQueueFull`, `IncDropped(repoID)` is called once
// per span in the rejected batch.
//
// Evaluator iter-2 #1: takes `enqueueMu` so the capacity check
// (the select default branch is the capacity check at the
// channel boundary) and EnqueueAtomic's pre-checked sends do
// not race. The drainer (Run) only consumes from the channel,
// never sends, so it never contends on the mutex.
func (i *Ingestor) Enqueue(batch SpanBatch) error {
	if batch.RepoID == "" {
		return errors.New("spaningestor: Enqueue: empty repo_id")
	}
	if len(batch.Spans) == 0 {
		return nil
	}
	for _, s := range batch.Spans {
		if s.RepoID != "" && s.RepoID != batch.RepoID {
			return fmt.Errorf(
				"spaningestor: Enqueue: span repo_id %q does not match batch %q",
				s.RepoID, batch.RepoID)
		}
	}
	i.enqueueMu.Lock()
	defer i.enqueueMu.Unlock()
	select {
	case i.queue <- batch:
		for range batch.Spans {
			i.metrics.IncIngested(batch.RepoID)
		}
		// Evaluator iter-1 #6: track per-repo in-flight count
		// so the backpressure supervisor can attribute the
		// degraded flag to currently-contributing repos, not
		// the lifetime "ever-ingested" set.
		i.metrics.AddInflight(batch.RepoID, int64(len(batch.Spans)))
		return nil
	default:
		for range batch.Spans {
			i.metrics.IncDropped(batch.RepoID)
		}
		return ErrQueueFull
	}
}

// EnqueueAtomic submits N batches all-or-nothing. Evaluator
// iter-1 #5: the OTLP receiver used to enqueue one repo's
// batch successfully then 503 on the next when capacity ran
// out, causing the Collector to retry the WHOLE POST and
// duplicate the already-enqueued spans. EnqueueAtomic checks
// capacity once under enqueueMu and either accepts every batch
// or rejects them all (incrementing IncDropped for every span
// in every batch).
//
// On all-or-nothing success, IncIngested + AddInflight are
// called per span exactly as for the single-batch path.
//
// Evaluator iter-2 #1: both this AND Enqueue hold enqueueMu,
// and BOTH use non-blocking sends. With the mutex held no
// other producer can interleave; the drainer only consumes;
// so a passed precheck guarantees every send below succeeds
// without blocking (the `default` arm is defensive and rolls
// back). This makes EnqueueAtomic provably non-blocking.
func (i *Ingestor) EnqueueAtomic(batches []SpanBatch) error {
	if len(batches) == 0 {
		return nil
	}
	for _, b := range batches {
		if b.RepoID == "" {
			return errors.New("spaningestor: EnqueueAtomic: empty repo_id in batch")
		}
		for _, s := range b.Spans {
			if s.RepoID != "" && s.RepoID != b.RepoID {
				return fmt.Errorf(
					"spaningestor: EnqueueAtomic: span repo_id %q does not match batch %q",
					s.RepoID, b.RepoID)
			}
		}
	}

	i.enqueueMu.Lock()
	defer i.enqueueMu.Unlock()

	// Only batches with spans actually consume a queue slot;
	// count those for the precheck so a zero-span batch passes
	// through without being charged capacity.
	nonEmpty := 0
	for _, b := range batches {
		if len(b.Spans) > 0 {
			nonEmpty++
		}
	}
	if cap(i.queue)-len(i.queue) < nonEmpty {
		for _, b := range batches {
			for range b.Spans {
				i.metrics.IncDropped(b.RepoID)
			}
		}
		return ErrQueueFull
	}
	// Track partially-accepted state so a defensive rollback
	// can decrement metrics if a send somehow falls into the
	// default arm (it should not under the mutex, but the
	// rollback keeps the metrics honest if the channel is
	// closed or replaced during the lock window).
	accepted := batches[:0]
	for _, b := range batches {
		if len(b.Spans) == 0 {
			continue
		}
		select {
		case i.queue <- b:
			accepted = append(accepted, b)
			for range b.Spans {
				i.metrics.IncIngested(b.RepoID)
			}
			i.metrics.AddInflight(b.RepoID, int64(len(b.Spans)))
		default:
			// Defensive: should be unreachable while we hold
			// the mutex (no other producer, drainer only
			// consumes). Roll back any accepted sends'
			// ingested + in-flight counts so the spans the
			// rollback promotes back into the dropped ledger
			// are not double-counted as both ingested and
			// dropped — without the AddIngested(-N) decrement
			// the operator drop-rate ratio
			// (span_dropped_total / span_ingested_total)
			// would be silently corrupted, and the supervisor
			// would also pin degraded for never-processed
			// spans. Then count every input span as dropped
			// to match the precheck rejection path's
			// semantics, and report the partial-overflow as
			// ErrQueueFull.
			for _, ab := range accepted {
				n := int64(len(ab.Spans))
				i.metrics.AddInflight(ab.RepoID, -n)
				i.metrics.AddIngested(ab.RepoID, -n)
			}
			for _, rb := range batches {
				for range rb.Spans {
					i.metrics.IncDropped(rb.RepoID)
				}
			}
			return ErrQueueFull
		}
	}
	return nil
}

// Run drains the queue until `ctx` is Done. A background
// supervisor goroutine runs concurrently when EITHER `health`
// is non-nil OR `pendingChildren` is enabled — the supervisor
// tick is responsible for the backpressure state machine
// (health-conditional) AND the TTL flush of parked
// out-of-order children (pending-conditional). Both can run
// in the same goroutine since the tick is cheap.
//
// Run returns ctx.Err() on a normal shutdown; it never returns
// a worker-loop error (per-batch errors are logged and swallowed
// so a single bad batch does not stop the pipeline).
func (i *Ingestor) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	if i.health != nil || i.pendingChildren != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i.runSupervisor(ctx)
		}()
	}

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case batch := <-i.queue:
			i.processBatch(ctx, batch)
		}
	}
}

// processBatch resolves every span in the batch, builds the
// within-batch (trace_id, span_id) → target map, and writes
// one of:
//   * AppendObservedCallTrace, when parent is resolved either
//     in-batch or via the cross-batch ParentIndex cache, OR
//     when this span IS the parent that a previously-parked
//     pending child was waiting for.
//   * AppendSoloMethodObservation, when there is no parent
//     (root) or when self-call.
//   * PendingChildIndex.Add, when the parent is unresolvable
//     from either source — the child is parked under
//     (trace_id, parent_span_id) so a future batch carrying
//     the parent can reconcile it into an edge. The
//     supervisor's TTL flush eventually writes solo for any
//     parked child whose parent never arrives.
//
// Resolution errors are logged but never propagate — a single
// unresolvable span MUST NOT poison the rest of the batch.
//
// After processing every span, the per-repo in-flight counter
// is decremented by the batch's span count regardless of
// per-span outcome — the "in flight" budget is consumed by
// every accepted span whether or not the writer call succeeded.
func (i *Ingestor) processBatch(ctx context.Context, batch SpanBatch) {
	defer i.metrics.AddInflight(batch.RepoID, -int64(len(batch.Spans)))

	// (traceID, spanID) → ObservationTarget (NodeID + kind). We
	// populate this for spans that resolved to a Method (Status
	// >= StatusMethod); it acts as the source for the
	// within-batch parent lookup, AND we mirror every entry into
	// the cross-batch parentIndex so a child arriving in a LATER
	// batch can still find its parent.
	//
	// Evaluator iter-2 #3: keyed by the composite (trace_id,
	// span_id), not span_id alone — OTel span IDs are scoped per
	// trace, so a multi-trace batch can have colliding span_ids
	// that would otherwise resolve to the wrong parent.
	parentMap := make(map[string]ObservationTarget, len(batch.Spans))
	type resolved struct {
		span ObservationSpan
		res  Resolution
	}
	resolvedSpans := make([]resolved, 0, len(batch.Spans))

	// Per-batch SHA resolver: caches the first call for the
	// batch's RepoID so a 1000-span batch is at most one
	// SHACache Get (which itself memoizes for TTL). Returns
	// (sha, err); see resolveSHA contract on perRepoSHA.
	resolveBatchSHA := i.perRepoSHA(ctx, batch.RepoID)

	// Pass 1: resolve, populate parentMap, drain any pending
	// children waiting on each newly-resolved parent.
	for _, sp := range batch.Spans {
		// Spans inherit the batch's RepoID if their own is
		// empty; the receiver SHOULD already set it, but be
		// tolerant.
		if sp.RepoID == "" {
			sp.RepoID = batch.RepoID
		}
		res, err := i.resolver.Resolve(ctx, sp.Span)
		if err != nil {
			// Resolver lookup-backend failure — log + skip the
			// span. Per resolver.go contract, lookup failures
			// MUST NOT trip span_unresolved_total.
			i.logger.Warn("spaningestor.resolve_failed",
				slog.String("repo_id", sp.RepoID),
				slog.String("trace_id", sp.TraceID),
				slog.String("span_id", sp.SpanID),
				slog.String("err", err.Error()))
			continue
		}
		if res.Status != StatusUnresolved && res.Method != nil {
			target := i.pickObservationTarget(res)
			if target.NodeID != "" {
				parentMap[indexKey(sp.TraceID, sp.SpanID)] = target
				// Mirror into cross-batch parentIndex per
				// evaluator iter-1 #2. Safe no-op when the
				// index is disabled.
				i.parentIndex.Remember(sp.TraceID, sp.SpanID, target)
				// Drain any pending children that were
				// parked waiting for THIS span as their
				// parent. Evaluator iter-2 #2: this is the
				// out-of-order reconciliation hot path.
				if i.pendingChildren != nil {
					for _, child := range i.pendingChildren.Drain(sp.TraceID, sp.SpanID) {
						i.metrics.IncParentArrivedLate(child.RepoID)
						i.emitObservedEdge(ctx, child.RepoID, target, child.DstTarget,
							child.Obs, i.perRepoSHA(ctx, child.RepoID))
					}
				}
			}
		}
		resolvedSpans = append(resolvedSpans, resolved{span: sp, res: res})
	}

	// Pass 2: write the current batch's spans now that
	// parentMap is fully populated.
	for _, r := range resolvedSpans {
		if r.res.Status == StatusUnresolved {
			// Resolver already incremented span_unresolved_total;
			// nothing for the writer to do.
			continue
		}
		dstTarget := i.pickObservationTarget(r.res)
		if dstTarget.NodeID == "" {
			// A defensive guard — a resolved Method must have
			// a node_id. Treat as resolve failure for metrics.
			i.logger.Warn("spaningestor.resolved_method_missing_node_id",
				slog.String("repo_id", r.span.RepoID),
				slog.String("span_id", r.span.SpanID))
			continue
		}
		obs := graphwriter.ObservationInput{
			TraceID:    r.span.TraceID,
			SpanID:     r.span.SpanID,
			StartedAt:  i.startedAt(r.span),
			DurationMs: r.span.DurationMs,
		}

		if r.span.ParentSpanID == "" {
			// True root span — record on the destination
			// Method's solo aggregate per tech-spec §8.6 row 3.
			i.metrics.IncSoloAggregate(r.span.RepoID)
			i.emitSoloObservation(ctx, r.span.RepoID, dstTarget, obs, "root_span")
			continue
		}

		// 1) In-batch parent lookup (cheapest). Composite key
		//    per evaluator iter-2 #3.
		srcTarget, ok := parentMap[indexKey(r.span.TraceID, r.span.ParentSpanID)]
		// 2) Cross-batch parent lookup via parentIndex (LRU+TTL).
		if !ok {
			if t, hit := i.parentIndex.Lookup(r.span.TraceID, r.span.ParentSpanID); hit {
				srcTarget = t
				ok = true
			}
		}
		if !ok {
			// Parent unresolvable from either source —
			// evaluator iter-2 #2: park the resolved child in
			// pendingChildren so a future batch that brings
			// the parent reconciles it. If the cache is
			// disabled fall back to the legacy
			// "write-solo-immediately" behaviour so the
			// observation isn't lost.
			i.metrics.IncParentMissing(r.span.RepoID)
			if i.pendingChildren != nil {
				evicted := i.pendingChildren.Add(PendingChild{
					RepoID:       r.span.RepoID,
					TraceID:      r.span.TraceID,
					SpanID:       r.span.SpanID,
					ParentSpanID: r.span.ParentSpanID,
					DstTarget:    dstTarget,
					Obs:          obs,
					AddedAt:      i.now(),
				})
				// LRU eviction of OTHER (older, never-reconciled)
				// pending children — write them as solo
				// observations so their timing data is not
				// dropped.
				for _, c := range evicted {
					i.metrics.IncParentNeverArrived(c.RepoID)
					i.emitSoloObservation(ctx, c.RepoID, c.DstTarget, c.Obs, "lru_evicted")
				}
			} else {
				i.emitSoloObservation(ctx, r.span.RepoID, dstTarget, obs,
					"cross_batch_parent_missing")
			}
			continue
		}
		// Parent resolved — emit edge.
		i.emitObservedEdge(ctx, r.span.RepoID, srcTarget, dstTarget, obs, resolveBatchSHA)
	}
}

// emitSoloObservation aggregates latency on the destination
// Method's solo window and calls AppendSoloMethodObservation.
// Used by: root spans, self-call collapse, cross-batch parent
// missing (when pending cache is disabled), pending-child LRU
// eviction, and pending-child TTL expiry.
func (i *Ingestor) emitSoloObservation(
	ctx context.Context,
	repoID string,
	dstTarget ObservationTarget,
	obs graphwriter.ObservationInput,
	reason string,
) {
	if dstTarget.NodeID == "" {
		return
	}
	key := "solo:" + dstTarget.NodeID
	obs.P50LatencyMs, obs.P95LatencyMs = i.agg.Observe(key, obs.DurationMs)
	if _, err := i.writer.AppendSoloMethodObservation(ctx, dstTarget.NodeID, obs); err != nil {
		i.logger.Error("spaningestor.solo_append_failed",
			slog.String("repo_id", repoID),
			slog.String("span_id", obs.SpanID),
			slog.String("reason", reason),
			slog.String("err", err.Error()))
	}
}

// emitObservedEdge writes one observed-call edge + log + edge
// aggregate. Used by both the current batch's in-batch /
// cross-batch parent paths AND the pending-child flush (when a
// previously-parked child's parent finally arrives in a later
// batch).
//
// `resolveSHA` is the closure for the source repo's SHA;
// passing a closure lets callers share one SHA resolution
// across many calls (the common case is one closure per batch).
//
// Evaluator iter-2 #4: a SHA lookup error drops the edge
// (counter bumped) rather than silently writing under the
// "observed" sentinel.
func (i *Ingestor) emitObservedEdge(
	ctx context.Context,
	repoID string,
	srcTarget, dstTarget ObservationTarget,
	obs graphwriter.ObservationInput,
	resolveSHA func() (string, error),
) {
	if srcTarget.NodeID == "" || dstTarget.NodeID == "" {
		return
	}
	if srcTarget.NodeID == dstTarget.NodeID {
		// Self-call: would synthesize a self-loop edge which
		// the architecture doesn't want; route to the solo
		// aggregate instead. Rare in practice (a Method
		// calling itself via a language feature like Java
		// reflection) but the alternative is polluting the
		// call graph.
		i.metrics.IncSoloAggregate(repoID)
		i.emitSoloObservation(ctx, repoID, dstTarget, obs, "self_call")
		return
	}
	repoUUID, err := fingerprint.ParseRepoID(repoID)
	if err != nil {
		i.logger.Warn("spaningestor.invalid_repo_uuid",
			slog.String("repo_id", repoID),
			slog.String("err", err.Error()))
		return
	}
	fromSHA, shaErr := resolveSHA()
	if shaErr != nil {
		i.metrics.IncShaLookupError(repoID)
		return
	}
	if fromSHA == "" {
		fromSHA = "observed"
	}
	edgeKey := srcTarget.NodeID + "->" + dstTarget.NodeID
	obs.P50LatencyMs, obs.P95LatencyMs = i.agg.Observe(edgeKey, obs.DurationMs)
	edgeIn := graphwriter.EdgeInput{
		RepoID:    repoUUID,
		Kind:      "", // pinned by AppendObservedCallTrace
		SrcNodeID: srcTarget.NodeID,
		DstNodeID: dstTarget.NodeID,
		FromSHA:   fromSHA,
	}
	if _, err := i.writer.AppendObservedCallTrace(ctx, edgeIn, obs); err != nil {
		i.logger.Error("spaningestor.observed_call_append_failed",
			slog.String("repo_id", repoID),
			slog.String("span_id", obs.SpanID),
			slog.String("err", err.Error()))
	}
}

// perRepoSHA returns a closure that memoizes the SHA for
// `repoID` for the lifetime of the closure. Combined with the
// SHACache's per-TTL memoization, a 1000-span batch is at most
// one Postgres roundtrip for the SHA lookup.
//
// Contract mirrors SHACache.Get:
//   * ("",   nil) — no reader wired OR repo has no row.
//                   Caller falls back to "observed" sentinel.
//   * (sha,  nil) — happy path, real SHA.
//   * ("",   err) — lookup failure (DB outage, permission).
//                   Caller MUST drop the edge and bump
//                   IncShaLookupError.
func (i *Ingestor) perRepoSHA(ctx context.Context, repoID string) func() (string, error) {
	var (
		sha    string
		err    error
		called bool
	)
	return func() (string, error) {
		if called {
			return sha, err
		}
		called = true
		sha, err = i.shaCache.Get(ctx, repoID)
		if err != nil {
			i.logger.Warn("spaningestor.sha_lookup_failed",
				slog.String("repo_id", repoID),
				slog.String("err", err.Error()))
		}
		return sha, err
	}
}

// flushExpiredPendingChildren is the supervisor-tick callback
// for the PendingChildIndex TTL eviction. Each expired child
// is written as a solo observation with reason
// "pending_child_ttl_expired" and the
// parent_never_arrived_total counter is bumped.
func (i *Ingestor) flushExpiredPendingChildren(ctx context.Context) {
	if i.pendingChildren == nil {
		return
	}
	i.pendingChildren.FlushExpired(func(c PendingChild) {
		i.metrics.IncParentNeverArrived(c.RepoID)
		i.emitSoloObservation(ctx, c.RepoID, c.DstTarget, c.Obs,
			"pending_child_ttl_expired")
	})
}

// pickObservationTarget chooses the Node the span's observation
// should anchor to. Evaluator iter-1 #3: architecture §3.7 says
// call edges and observation rows can target Blocks; the
// resolver already emits StatusBlock with a Block candidate.
// When both Method and Block are present we prefer Block because
// the Block-level signal is strictly more specific.
func (i *Ingestor) pickObservationTarget(res Resolution) ObservationTarget {
	if res.Status == StatusBlock && res.Block != nil && res.Block.NodeID != "" {
		return ObservationTarget{NodeID: res.Block.NodeID, Kind: "block"}
	}
	if res.Method != nil {
		return ObservationTarget{NodeID: i.idLookup.MethodNodeID(res.Method), Kind: "method"}
	}
	return ObservationTarget{}
}

// startedAt returns the span's StartedAt when present, falling
// back to the Ingestor's injectable clock (`i.now`) so the
// zero-StartedAt path is testable with a deterministic clock,
// matching how the supervisor, caches, and processBatch source
// wall-clock time.
func (i *Ingestor) startedAt(s ObservationSpan) time.Time {
	if !s.StartedAt.IsZero() {
		return s.StartedAt
	}
	return i.now().UTC()
}
