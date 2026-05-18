package rerankertrainer

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Metric name constants. Centralised here so a typo in one call
// site does not silently un-pair a counter from its exposition.
// Mirrors the pattern used by the sibling consolidator and
// tracelogpruner packages.
const (
	// MetricRerankerRunsTotal counts every Tick invocation
	// (success OR failure) so an alerting rule can detect a
	// stuck nightly run from the time-since-last-increment of
	// this counter.
	MetricRerankerRunsTotal = "reranker_runs_total"

	// MetricRerankerErrorsTotal counts only Tick invocations
	// that surfaced a non-nil error. Together with
	// MetricRerankerRunsTotal this gives a per-binary failure
	// rate without parsing logs.
	MetricRerankerErrorsTotal = "reranker_errors_total"

	// MetricRerankerLockSkippedTotal counts ticks that returned
	// without training because `pg_try_advisory_lock` returned
	// false (another replica holds the lock). Distinct from
	// MetricRerankerErrorsTotal -- a lock-skipped tick is the
	// correct cross-replica behaviour, not an error.
	MetricRerankerLockSkippedTotal = "reranker_lock_skipped_total"

	// MetricRerankerModelsPublishedTotal counts the
	// `reranker_model` rows INSERTed. Bumped exactly once per
	// successful Tick that publishes a fresh model row -- a
	// duplicate fingerprint (idempotent retry) is intercepted
	// by the UNIQUE index on `version` and does NOT bump the
	// counter.
	MetricRerankerModelsPublishedTotal = "reranker_models_published_total"

	// MetricRerankerPositivePairsTotal counts the positive
	// LabelledPair entries the most recent run *consumed*
	// (post-cap, post-window filter). Bumped per pair so the
	// total over a binary lifetime is a useful "labelled
	// supervision throughput" signal.
	MetricRerankerPositivePairsTotal = "reranker_positive_pairs_total"

	// MetricRerankerNegativePairsTotal mirrors
	// MetricRerankerPositivePairsTotal for negatives (failure
	// / degraded / pre-correction `human_corrected`).
	MetricRerankerNegativePairsTotal = "reranker_negative_pairs_total"

	// MetricRerankerEpisodesBelowMinTotal counts ticks that
	// SKIPPED publication because the post-cap labelled-pair
	// count fell below `Config.MinEpisodes`. Operators consult
	// this counter to confirm "no new model published" is
	// caused by sparse supervision rather than a busted
	// trainer.
	MetricRerankerEpisodesBelowMinTotal = "reranker_episodes_below_min_total"

	// MetricRerankerCappedActorTotal is the per-actor metric
	// from the §6.4 acceptance scenario: "Given operator O
	// submits 100 mgmt.feedback(human_corrected) calls in an
	// hour, ... emits `trainer_capped_actor_total{actor=O}`".
	// The metric NAME the §6.4 scenario uses is
	// `trainer_capped_actor_total`; the prefix `reranker_` is
	// the package's standard prefix. Both spellings are
	// exposed in /metrics so the scenario assertion and the
	// dashboard alike resolve to the same series; see
	// AltMetricCappedActorTotal below for the alias.
	MetricRerankerCappedActorTotal = "reranker_capped_actor_total"

	// AltMetricCappedActorTotal is the spelling the §6.4
	// scenario text uses verbatim. The exposition emits BOTH
	// names with identical samples so neither downstream
	// consumer (alerting / scenario test) has to know about
	// the other.
	AltMetricCappedActorTotal = "trainer_capped_actor_total"

	// MetricRerankerLastTrainedAtSeconds is the unix-seconds
	// timestamp of the most recent successful publish. A
	// gauge so an alerting rule can compute
	// `time() - reranker_last_trained_at_seconds > 604800` to
	// fire the same condition the recall path's §9.10 stale
	// flag uses. Zero (the unset value) is treated by the
	// alert rule as "no row exists" -- the same handling as
	// `RerankerFreshnessSource.LatestRerankerTrainedAt`
	// returning `(_, false, nil)`.
	MetricRerankerLastTrainedAtSeconds = "reranker_last_trained_at_seconds"
)

// Metrics is the package's atomic-counter surface. All counters
// are goroutine-safe via sync/atomic so the Run-loop goroutine
// and the HTTP scrape goroutine can read concurrently without
// locking. The per-actor `cappedActors` map is guarded by a
// dedicated RWMutex because the actor label is not known at
// compile time.
type Metrics struct {
	runs              atomic.Uint64
	errors            atomic.Uint64
	lockSkipped       atomic.Uint64
	modelsPublished   atomic.Uint64
	positivePairs     atomic.Uint64
	negativePairs     atomic.Uint64
	episodesBelowMin  atomic.Uint64
	lastTrainedAtUnix atomic.Int64
	cappedActorsMu    sync.RWMutex
	cappedActors      map[string]uint64
}

// NewMetrics returns a zero-initialised Metrics. The
// `reranker_last_trained_at_seconds` gauge is implicitly zero,
// which the §9.10 alert and the recall-path freshness check
// both interpret as "no model has been trained yet" (same as
// `RerankerFreshnessSource` returning `(_, false, nil)`).
func NewMetrics() *Metrics {
	return &Metrics{cappedActors: make(map[string]uint64)}
}

// IncRuns bumps reranker_runs_total. Called by Service.Tick
// once per invocation regardless of outcome.
func (m *Metrics) IncRuns() { m.runs.Add(1) }

// IncErrors bumps reranker_errors_total. Called by Service.Tick
// when the lifecycle returns a non-nil error.
func (m *Metrics) IncErrors() { m.errors.Add(1) }

// IncLockSkipped bumps reranker_lock_skipped_total. Called on
// the lock-miss path so an operator can distinguish "no
// publish because we lost the lock" from "no publish because
// data was sparse".
func (m *Metrics) IncLockSkipped() { m.lockSkipped.Add(1) }

// IncModelsPublished bumps reranker_models_published_total.
// Called per successful `reranker_model` INSERT only; an
// idempotent retry that hits the UNIQUE-version conflict does
// not increment.
func (m *Metrics) IncModelsPublished() { m.modelsPublished.Add(1) }

// AddPositivePairs adds n to reranker_positive_pairs_total.
func (m *Metrics) AddPositivePairs(n uint64) {
	if n == 0 {
		return
	}
	m.positivePairs.Add(n)
}

// AddNegativePairs adds n to reranker_negative_pairs_total.
func (m *Metrics) AddNegativePairs(n uint64) {
	if n == 0 {
		return
	}
	m.negativePairs.Add(n)
}

// IncEpisodesBelowMin bumps reranker_episodes_below_min_total.
// Called on a Tick where the post-cap labelled-pair count fell
// below the configured minimum so no publish happened.
func (m *Metrics) IncEpisodesBelowMin() { m.episodesBelowMin.Add(1) }

// AddCappedActor increments the per-actor capped counter. The
// label is the `episode_update.actor` enum value the cap was
// applied to (in v1 single-tenant this is "operator"; the
// label remains a runtime value so a future per-human
// identifier slots in without a code change).
func (m *Metrics) AddCappedActor(actor string, n uint64) {
	if n == 0 || actor == "" {
		return
	}
	m.cappedActorsMu.Lock()
	defer m.cappedActorsMu.Unlock()
	m.cappedActors[actor] += n
}

// SetLastTrainedAt updates the gauge to the wall-clock unix
// time of the most recent successful publish.
func (m *Metrics) SetLastTrainedAt(t time.Time) {
	m.lastTrainedAtUnix.Store(t.Unix())
}

// RunsTotal returns the reranker_runs_total counter value.
func (m *Metrics) RunsTotal() uint64 { return m.runs.Load() }

// ErrorsTotal returns the reranker_errors_total counter value.
func (m *Metrics) ErrorsTotal() uint64 { return m.errors.Load() }

// LockSkippedTotal returns the reranker_lock_skipped_total
// counter value.
func (m *Metrics) LockSkippedTotal() uint64 { return m.lockSkipped.Load() }

// ModelsPublishedTotal returns the
// reranker_models_published_total counter value.
func (m *Metrics) ModelsPublishedTotal() uint64 { return m.modelsPublished.Load() }

// PositivePairsTotal returns the reranker_positive_pairs_total
// counter value.
func (m *Metrics) PositivePairsTotal() uint64 { return m.positivePairs.Load() }

// NegativePairsTotal returns the reranker_negative_pairs_total
// counter value.
func (m *Metrics) NegativePairsTotal() uint64 { return m.negativePairs.Load() }

// EpisodesBelowMinTotal returns the
// reranker_episodes_below_min_total counter value.
func (m *Metrics) EpisodesBelowMinTotal() uint64 { return m.episodesBelowMin.Load() }

// CappedActorTotal returns the per-actor counter for `actor`,
// or 0 if no entry exists.
func (m *Metrics) CappedActorTotal(actor string) uint64 {
	m.cappedActorsMu.RLock()
	defer m.cappedActorsMu.RUnlock()
	return m.cappedActors[actor]
}

// CappedActorSnapshot returns a sorted-by-actor copy of the
// per-actor map so the metrics exposition produces
// deterministic output.
func (m *Metrics) CappedActorSnapshot() []CappedActorSample {
	m.cappedActorsMu.RLock()
	defer m.cappedActorsMu.RUnlock()
	out := make([]CappedActorSample, 0, len(m.cappedActors))
	for actor, count := range m.cappedActors {
		out = append(out, CappedActorSample{Actor: actor, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Actor < out[j].Actor })
	return out
}

// LastTrainedAt returns the gauge as a time.Time. A zero
// `m.lastTrainedAtUnix` returns the zero time -- callers
// interpret that as "no successful publish yet" identically
// to `RerankerFreshnessSource` returning `(_, false, nil)`.
func (m *Metrics) LastTrainedAt() time.Time {
	v := m.lastTrainedAtUnix.Load()
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

// CappedActorSample is one entry in CappedActorSnapshot's
// output. Exported so the binary's /metrics handler can range
// over the slice without reaching into package internals.
type CappedActorSample struct {
	Actor string
	Count uint64
}

// Snapshot returns a stable map of the package's COUNTER
// values keyed by the Metric* constants. Per-label counters
// (CappedActor) are excluded -- callers iterate
// CappedActorSnapshot() to emit those, matching the sibling
// consolidator.Metrics.Snapshot shape.
//
// The gauge `reranker_last_trained_at_seconds` is excluded for
// the same reason consolidator.Metrics excludes
// `consolidator_episode_lag`: the map value type is uint64
// and the gauge is more naturally read via LastTrainedAt().
func (m *Metrics) Snapshot() map[string]uint64 {
	return map[string]uint64{
		MetricRerankerRunsTotal:             m.runs.Load(),
		MetricRerankerErrorsTotal:           m.errors.Load(),
		MetricRerankerLockSkippedTotal:      m.lockSkipped.Load(),
		MetricRerankerModelsPublishedTotal:  m.modelsPublished.Load(),
		MetricRerankerPositivePairsTotal:    m.positivePairs.Load(),
		MetricRerankerNegativePairsTotal:    m.negativePairs.Load(),
		MetricRerankerEpisodesBelowMinTotal: m.episodesBelowMin.Load(),
	}
}
