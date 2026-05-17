package consolidator

import (
	"sync/atomic"
	"time"
)

// Metric name constants -- the binary's /metrics endpoint and
// any downstream collector key off these literals. Centralised
// here so a typo in one call site does not silently un-pair a
// counter from its exposition. Mirrors the pattern used by the
// sibling tracelogpruner package.
const (
	// MetricConsolidatorRunsTotal counts every Tick invocation
	// (success OR failure) so an alerting rule can detect a
	// stuck poll loop from the time-since-last-increment of
	// this counter. Distinct from the per-concept counters
	// below: those can be zero on a healthy day when no group
	// crossed the threshold yet.
	MetricConsolidatorRunsTotal = "consolidator_runs_total"

	// MetricConsolidatorErrorsTotal counts only the Tick
	// invocations that surfaced a non-nil error. Together with
	// MetricConsolidatorRunsTotal this gives a per-binary
	// failure rate without parsing logs.
	MetricConsolidatorErrorsTotal = "consolidator_errors_total"

	// MetricConsolidatorEpisodesScannedTotal counts the Episode
	// rows the Consolidator has read (and decided whether to
	// crystallise into a Concept) over the binary's lifetime.
	// Operators correlate this against the
	// `episode_id`-cardinality of the partitioned Episode table
	// to verify the cursor is making forward progress.
	MetricConsolidatorEpisodesScannedTotal = "consolidator_episodes_scanned_total"

	// MetricConsolidatorConceptsCreatedTotal counts the
	// `concept` rows inserted (first-time crystallisations).
	// Subsequent ConceptVersion appends to an existing Concept
	// do NOT bump this counter -- they bump the versions
	// counter below.
	MetricConsolidatorConceptsCreatedTotal = "consolidator_concepts_created_total"

	// MetricConsolidatorVersionsAppendedTotal counts the
	// `concept_version` rows the Consolidator has appended
	// across the binary's lifetime. Bumped once per signature
	// group that contributes new support, regardless of
	// whether the Concept was created on this tick or already
	// existed.
	MetricConsolidatorVersionsAppendedTotal = "consolidator_versions_appended_total"

	// MetricConsolidatorSupportsAppendedTotal counts the
	// `concept_support` rows inserted. One row per (Concept,
	// Episode, repo_id) tuple contributing to a crystallised
	// version, so the sum across binary lifetime grows roughly
	// linearly with consumed positive-or-negative Episodes.
	MetricConsolidatorSupportsAppendedTotal = "consolidator_supports_appended_total"

	// MetricConsolidatorEpisodeLag is the implementation-plan
	// §6.1 metric requirement: `max(Episode.created_at) − the
	// run's new episode_high_water_mark.created_at`. Exposed
	// as a gauge in SECONDS (the gauge value is a float64
	// seconds magnitude); the metric NAME deliberately omits
	// the `_seconds` suffix per the literal §6.1 wording line
	// 903 ("Emit metric `consolidator_episode_lag` = ...").
	//
	// Stored internally as int64 nanoseconds so the atomic
	// load is lock-free; `LagSeconds()` converts to float64
	// seconds for the /metrics exposition. Zero is the
	// healthy value (the cursor has caught up to the latest
	// Episode); a positive value indicates the Consolidator
	// has fallen behind the writers.
	MetricConsolidatorEpisodeLag = "consolidator_episode_lag"
)

// Metrics is the package's atomic-counter surface. Construct
// via NewMetrics(); read via Snapshot()+LagSeconds(). All
// counters are goroutine-safe via sync/atomic so the Run-loop
// goroutine and the HTTP scrape goroutine can read concurrently
// without locking.
type Metrics struct {
	runs             atomic.Uint64
	errors           atomic.Uint64
	episodesScanned  atomic.Uint64
	conceptsCreated  atomic.Uint64
	versionsAppended atomic.Uint64
	supportsAppended atomic.Uint64
	// episodeLagNanos is a SIGNED int64 so a paranoid
	// "max(created_at) precedes cursor" case (shouldn't
	// happen, but partial-failure ticks could leave the
	// cursor briefly ahead) does not feed a uint64 underflow
	// into the gauge. We clamp negative reads to zero in
	// LagSeconds for the operator-facing exposition.
	episodeLagNanos atomic.Int64
}

// NewMetrics returns a zero-initialised Metrics. The gauge
// (`episode_lag_seconds`) is implicitly zero, which is the
// healthy value -- a brand-new binary that has not yet ticked
// reports a "caught-up" cursor until the first Tick proves
// otherwise.
func NewMetrics() *Metrics {
	return &Metrics{}
}

// IncRuns increments the per-run counter. Called by
// Service.Tick once per invocation, regardless of outcome.
func (m *Metrics) IncRuns() { m.runs.Add(1) }

// IncErrors increments the per-error counter. Called by
// Service.Tick when the lifecycle returns a non-nil error.
func (m *Metrics) IncErrors() { m.errors.Add(1) }

// AddEpisodesScanned adds n to the episodes-scanned counter.
// Called by Service.Tick after the Episode scan returns.
func (m *Metrics) AddEpisodesScanned(n uint64) {
	if n == 0 {
		return
	}
	m.episodesScanned.Add(n)
}

// AddConceptsCreated adds n to the concepts-created counter.
// Called by Service.Tick after a successful Concept INSERT
// (i.e. ON CONFLICT did NOT fire -- the row is brand new).
func (m *Metrics) AddConceptsCreated(n uint64) {
	if n == 0 {
		return
	}
	m.conceptsCreated.Add(n)
}

// AddVersionsAppended adds n to the concept-versions-appended
// counter. Called by Service.Tick after a successful
// concept_version INSERT.
func (m *Metrics) AddVersionsAppended(n uint64) {
	if n == 0 {
		return
	}
	m.versionsAppended.Add(n)
}

// AddSupportsAppended adds n to the concept-supports-appended
// counter. Called by Service.Tick after a batch of
// concept_support INSERTs commits.
func (m *Metrics) AddSupportsAppended(n uint64) {
	if n == 0 {
		return
	}
	m.supportsAppended.Add(n)
}

// SetEpisodeLag sets the lag gauge. Called by Service.Tick at
// the end of a successful run; the value is the wall-clock
// delta between `max(Episode.created_at)` and the new
// `episode_high_water_mark`'s `created_at`.
//
// Stored as a signed nanosecond int64 (rather than seconds-as-
// float64) so the atomic load on the scrape path is lock-free.
// LagSeconds() converts to a float64 seconds value for the
// Prometheus text-format exposition.
func (m *Metrics) SetEpisodeLag(d time.Duration) {
	m.episodeLagNanos.Store(int64(d))
}

// RunsTotal returns the consolidator_runs_total value.
func (m *Metrics) RunsTotal() uint64 { return m.runs.Load() }

// ErrorsTotal returns the consolidator_errors_total value.
func (m *Metrics) ErrorsTotal() uint64 { return m.errors.Load() }

// EpisodesScannedTotal returns the
// consolidator_episodes_scanned_total value.
func (m *Metrics) EpisodesScannedTotal() uint64 {
	return m.episodesScanned.Load()
}

// ConceptsCreatedTotal returns the
// consolidator_concepts_created_total value.
func (m *Metrics) ConceptsCreatedTotal() uint64 {
	return m.conceptsCreated.Load()
}

// VersionsAppendedTotal returns the
// consolidator_versions_appended_total value.
func (m *Metrics) VersionsAppendedTotal() uint64 {
	return m.versionsAppended.Load()
}

// SupportsAppendedTotal returns the
// consolidator_supports_appended_total value.
func (m *Metrics) SupportsAppendedTotal() uint64 {
	return m.supportsAppended.Load()
}

// EpisodeLag returns the current lag as a time.Duration. May
// be negative under the rare partial-failure case described in
// the SetEpisodeLag doc; LagSeconds clamps to zero for the
// operator-facing exposition.
func (m *Metrics) EpisodeLag() time.Duration {
	return time.Duration(m.episodeLagNanos.Load())
}

// LagSeconds returns the lag in seconds as a float64, clamped
// to [0, +Inf). Used by the /metrics gauge exposition.
func (m *Metrics) LagSeconds() float64 {
	n := m.episodeLagNanos.Load()
	if n < 0 {
		return 0
	}
	return float64(n) / float64(time.Second)
}

// Snapshot returns a stable map of the package's counters
// keyed by the Metric* constants. The map is freshly allocated
// per call so a caller can iterate without holding a lock
// against concurrent IncX writers.
//
// The gauge `consolidator_episode_lag` is NOT included
// in the Snapshot map because the map's value type is uint64
// (matching tracelogpruner.Snapshot's shape) and the gauge is
// a floating-point seconds value. Call LagSeconds() separately
// for that one metric -- the binary's writeMetrics handler
// does exactly that.
func (m *Metrics) Snapshot() map[string]uint64 {
	return map[string]uint64{
		MetricConsolidatorRunsTotal:             m.runs.Load(),
		MetricConsolidatorErrorsTotal:           m.errors.Load(),
		MetricConsolidatorEpisodesScannedTotal:  m.episodesScanned.Load(),
		MetricConsolidatorConceptsCreatedTotal:  m.conceptsCreated.Load(),
		MetricConsolidatorVersionsAppendedTotal: m.versionsAppended.Load(),
		MetricConsolidatorSupportsAppendedTotal: m.supportsAppended.Load(),
	}
}
