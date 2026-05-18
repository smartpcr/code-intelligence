package promoter

import (
	"sync/atomic"
)

// Metric name constants -- the binary's /metrics endpoint and
// any downstream collector key off these literals. Centralised
// here so a typo in one call site does not silently un-pair a
// counter from its exposition. Mirrors the pattern used by the
// sibling consolidator package.
const (
	// MetricPromoterRunsTotal counts every Tick invocation
	// (success OR failure) so an alerting rule can detect a
	// stuck poll loop from the time-since-last-increment of
	// this counter.
	MetricPromoterRunsTotal = "promoter_runs_total"

	// MetricPromoterErrorsTotal counts only the Tick
	// invocations that surfaced a non-nil error. Together with
	// MetricPromoterRunsTotal this gives a per-binary failure
	// rate without parsing logs.
	MetricPromoterErrorsTotal = "promoter_errors_total"

	// MetricPromoterLockSkippedTotal counts ticks where the
	// per-replica session-level advisory lock was already held
	// by a sibling replica (`pg_try_advisory_lock` returned
	// false). The tick exits cleanly with `promoter_run.status
	// = 'lock_skipped'`; this counter lets operators detect a
	// stuck holder.
	MetricPromoterLockSkippedTotal = "promoter_lock_skipped_total"

	// MetricPromoterCandidatesEvaluatedTotal counts the
	// candidate Concepts surfaced by the threshold query AND
	// re-confirmed inside the per-Concept transaction (i.e.
	// those for which the version_index recheck passed). A
	// candidate dropped by the recheck (because the
	// Consolidator already bumped a version, or another
	// Promoter replica already promoted) is NOT counted here.
	MetricPromoterCandidatesEvaluatedTotal = "promoter_candidates_evaluated_total"

	// MetricPromoterConceptsPromotedTotal counts publish chains
	// that reached the terminal `published` event during the
	// binary's lifetime. The per-run `promoter_run.concepts_
	// promoted` column is the durable equivalent.
	MetricPromoterConceptsPromotedTotal = "promoter_concepts_promoted_total"

	// MetricPromoterPublishFailuresTotal counts publish chains
	// that recorded a `failed` event. A subsequent retry that
	// succeeds increments ConceptsPromotedTotal but does NOT
	// decrement this counter — the failure history is durable.
	MetricPromoterPublishFailuresTotal = "promoter_publish_failures_total"

	// MetricPromoterRetriesAttemptedTotal counts how many
	// publish chains the retry phase picked up and re-attempted
	// during the binary's lifetime. The expected value on a
	// healthy day is zero (no stalled chains) — a steady-state
	// non-zero value indicates a persistent embedder or Qdrant
	// problem.
	MetricPromoterRetriesAttemptedTotal = "promoter_retries_attempted_total"

	// MetricPromoterOrphansRecoveredTotal counts how many
	// orphaned promoted ConceptVersion rows (tx1 committed
	// but tx2 EmbeddingPublish never landed in a prior tick;
	// invisible to BOTH selectStalled AND selectCandidates
	// without orphan recovery) the orphan-recovery phase
	// converted into a published vector. The expected value
	// on a healthy day is zero — a steady-state non-zero
	// value indicates a persistent tx2 failure mode (DB
	// outage, schema drift) generating fresh orphans
	// tick-after-tick. Evaluator-2 finding #1.
	MetricPromoterOrphansRecoveredTotal = "promoter_orphans_recovered_total"

	// MetricSnapshotPublishedTotal counts concept-side
	// publish chains driven by a §7.4 `mgmt.snapshot`
	// enqueue that reached the terminal `published` event.
	// Mirrors the embedding-side counter the repoindexer
	// exposes (`snapshot_published_total`) -- the name is
	// shared on purpose so a Prometheus scrape across the
	// two binaries reports a single converged value. The
	// classifier (`runAttempt`) looks for ANY queued event
	// on the publish whose `details_json->>'source'` equals
	// `mgmt.snapshot`.
	MetricSnapshotPublishedTotal = "snapshot_published_total"

	// MetricPromoterOrphansPending is a per-tick gauge of
	// orphaned promoted ConceptVersion rows the
	// orphan-recovery scan returned BEFORE the per-orphan
	// driver loop. Captures the orphan backlog depth at the
	// moment of the scan; a steady non-zero value indicates
	// the recovery loop is keeping up but not idle (i.e.
	// fresh orphans are still being generated).
	MetricPromoterOrphansPending = "promoter_orphans_pending"

	// MetricPromoterCandidatesPending is a per-tick gauge of
	// candidate Concepts surfaced by the threshold query
	// BEFORE the per-Concept recheck. Captures the depth of
	// the queue at the moment of the scan; a steady non-zero
	// value indicates the promoter is keeping up but not idle.
	MetricPromoterCandidatesPending = "promoter_candidates_pending"
)

// Metrics is the package's atomic-counter surface. Construct
// via NewMetrics(); read via Snapshot()+CandidatesPending().
// All counters are goroutine-safe via sync/atomic so the
// Run-loop goroutine and the HTTP scrape goroutine can read
// concurrently without locking.
type Metrics struct {
	runs                atomic.Uint64
	errors              atomic.Uint64
	lockSkipped         atomic.Uint64
	candidatesEvaluated atomic.Uint64
	conceptsPromoted    atomic.Uint64
	publishFailures     atomic.Uint64
	retriesAttempted    atomic.Uint64
	orphansRecovered    atomic.Uint64
	snapshotPublished   atomic.Uint64
	candidatesPending   atomic.Int64
	orphansPending      atomic.Int64
}

// NewMetrics returns a zero-initialised Metrics. The gauge
// (`promoter_candidates_pending`) is implicitly zero, which is
// the idle value -- a brand-new binary that has not yet ticked
// reports an empty queue until the first Tick proves otherwise.
func NewMetrics() *Metrics {
	return &Metrics{}
}

// IncRuns increments the per-run counter. Called by
// Service.Tick once per invocation, regardless of outcome.
func (m *Metrics) IncRuns() { m.runs.Add(1) }

// IncErrors increments the per-error counter. Called by
// Service.Tick when the lifecycle returns a non-nil error.
func (m *Metrics) IncErrors() { m.errors.Add(1) }

// IncLockSkipped increments the lock-skipped counter. Called
// by Service.Tick when the advisory lock acquisition returned
// false (a sibling replica is mid-tick).
func (m *Metrics) IncLockSkipped() { m.lockSkipped.Add(1) }

// AddCandidatesEvaluated adds n to the candidates-evaluated
// counter. Called by Service.Tick once per per-Concept
// transaction that PASSED the recheck.
func (m *Metrics) AddCandidatesEvaluated(n uint64) {
	if n == 0 {
		return
	}
	m.candidatesEvaluated.Add(n)
}

// AddConceptsPromoted adds n to the concepts-promoted counter.
// Called by Service.Tick once per publish chain that reached
// the terminal `published` event.
func (m *Metrics) AddConceptsPromoted(n uint64) {
	if n == 0 {
		return
	}
	m.conceptsPromoted.Add(n)
}

// AddPublishFailures adds n to the publish-failures counter.
// Called by Service.Tick once per `failed` event appended.
func (m *Metrics) AddPublishFailures(n uint64) {
	if n == 0 {
		return
	}
	m.publishFailures.Add(n)
}

// AddRetriesAttempted adds n to the retries-attempted counter.
// Called by Service.Tick once per stalled chain the retry
// phase picked up.
func (m *Metrics) AddRetriesAttempted(n uint64) {
	if n == 0 {
		return
	}
	m.retriesAttempted.Add(n)
}

// AddOrphansRecovered adds n to the orphans-recovered counter.
// Called by Service.processOrphans once per orphan that the
// recovery phase drove to the terminal `published` event.
// Evaluator-2 finding #1.
func (m *Metrics) AddOrphansRecovered(n uint64) {
	if n == 0 {
		return
	}
	m.orphansRecovered.Add(n)
}

// AddSnapshotPublished adds n to the snapshot-published
// counter.  Called by Service.runAttempt once per concept
// publish that reached `published` AND was originally
// enqueued by the §7.4 mgmt.snapshot handler (detected via
// the queued-event `source` marker).
func (m *Metrics) AddSnapshotPublished(n uint64) {
	if n == 0 {
		return
	}
	m.snapshotPublished.Add(n)
}

// SetCandidatesPending sets the candidates-pending gauge.
// Called by Service.Tick after the threshold query returns
// (BEFORE the per-Concept recheck loop).
func (m *Metrics) SetCandidatesPending(n int64) {
	m.candidatesPending.Store(n)
}

// SetOrphansPending sets the orphans-pending gauge. Called
// by Service.processOrphans after the orphan-recovery scan
// returns (BEFORE the per-orphan driver loop).
// Evaluator-2 finding #1.
func (m *Metrics) SetOrphansPending(n int64) {
	m.orphansPending.Store(n)
}

// RunsTotal returns the promoter_runs_total value.
func (m *Metrics) RunsTotal() uint64 { return m.runs.Load() }

// ErrorsTotal returns the promoter_errors_total value.
func (m *Metrics) ErrorsTotal() uint64 { return m.errors.Load() }

// LockSkippedTotal returns the promoter_lock_skipped_total value.
func (m *Metrics) LockSkippedTotal() uint64 { return m.lockSkipped.Load() }

// CandidatesEvaluatedTotal returns the
// promoter_candidates_evaluated_total value.
func (m *Metrics) CandidatesEvaluatedTotal() uint64 {
	return m.candidatesEvaluated.Load()
}

// ConceptsPromotedTotal returns the
// promoter_concepts_promoted_total value.
func (m *Metrics) ConceptsPromotedTotal() uint64 {
	return m.conceptsPromoted.Load()
}

// PublishFailuresTotal returns the
// promoter_publish_failures_total value.
func (m *Metrics) PublishFailuresTotal() uint64 {
	return m.publishFailures.Load()
}

// RetriesAttemptedTotal returns the
// promoter_retries_attempted_total value.
func (m *Metrics) RetriesAttemptedTotal() uint64 {
	return m.retriesAttempted.Load()
}

// OrphansRecoveredTotal returns the
// promoter_orphans_recovered_total value. Evaluator-2 finding #1.
func (m *Metrics) OrphansRecoveredTotal() uint64 {
	return m.orphansRecovered.Load()
}

// SnapshotPublishedTotal returns the snapshot_published_total
// value contributed by the concept-side publish path.
func (m *Metrics) SnapshotPublishedTotal() uint64 {
	return m.snapshotPublished.Load()
}

// CandidatesPending returns the current promoter_candidates_pending gauge value.
func (m *Metrics) CandidatesPending() int64 {
	return m.candidatesPending.Load()
}

// OrphansPending returns the current promoter_orphans_pending gauge value.
// Evaluator-2 finding #1.
func (m *Metrics) OrphansPending() int64 {
	return m.orphansPending.Load()
}

// Snapshot returns a stable map of the package's counters
// keyed by the Metric* constants. The map is freshly allocated
// per call so a caller can iterate without holding a lock
// against concurrent IncX writers.
//
// The gauges `promoter_candidates_pending` and
// `promoter_orphans_pending` are NOT included in the Snapshot
// map because the map's value type is uint64; the gauges are
// signed int64. Call CandidatesPending() / OrphansPending()
// separately for those metrics -- the binary's writeMetrics
// handler does exactly that.
func (m *Metrics) Snapshot() map[string]uint64 {
	return map[string]uint64{
		MetricPromoterRunsTotal:                m.runs.Load(),
		MetricPromoterErrorsTotal:              m.errors.Load(),
		MetricPromoterLockSkippedTotal:         m.lockSkipped.Load(),
		MetricPromoterCandidatesEvaluatedTotal: m.candidatesEvaluated.Load(),
		MetricPromoterConceptsPromotedTotal:    m.conceptsPromoted.Load(),
		MetricPromoterPublishFailuresTotal:     m.publishFailures.Load(),
		MetricPromoterRetriesAttemptedTotal:    m.retriesAttempted.Load(),
		MetricPromoterOrphansRecoveredTotal:    m.orphansRecovered.Load(),
		MetricSnapshotPublishedTotal:           m.snapshotPublished.Load(),
	}
}
