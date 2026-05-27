package aggregator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gofrs/uuid"
)

// Aggregator is the cadence-driven worker that materialises the
// three Measurement sub-store derived views (architecture Sec
// 3.10 / Sec 5.2.4 -- Sec 5.2.6).
//
// Construction is via [NewAggregator]; the cadence loop wraps the
// aggregator in [Loop] which calls [Aggregator.Tick] every
// [config.DefaultAggregatorCadence] (15 min).
//
// # Single source of truth (G6)
//
// Tick reads `source.ReadActive`, computes the derived rows in
// process, and writes via `writer.WriteSnapshots`. The aggregator
// holds NO state between ticks -- every snapshot is recomputable
// from `metric_sample` + `metric_sample_active` + `metric_retraction`
// at any time. Restarting the aggregator loses zero correctness.
//
// # Concurrency
//
// [Tick] is safe for concurrent invocation -- it allocates fresh
// working buffers and never touches Aggregator-level state. In
// production exactly one [Loop] drives one [Aggregator]; the
// concurrent-safety property exists so tests can drive multiple
// ticks in parallel against the same Aggregator without
// surprising shared-state behaviour.
type Aggregator struct {
	source SampleSource
	writer SnapshotWriter
	now    func() time.Time
}

// AggregatorOption configures an [Aggregator].
type AggregatorOption func(*Aggregator)

// WithClock overrides the wall-clock function used to stamp
// `built_at`. Defaults to [time.Now] in production; tests inject
// a deterministic clock so the captured snapshot rows have a
// known timestamp.
func WithClock(now func() time.Time) AggregatorOption {
	return func(a *Aggregator) { a.now = now }
}

// ErrAggregatorNilSource surfaces a nil [SampleSource] at
// composition-root wiring time.
var ErrAggregatorNilSource = errors.New("aggregator: NewAggregator: source is nil")

// ErrAggregatorNilWriter surfaces a nil [SnapshotWriter] at
// composition-root wiring time.
var ErrAggregatorNilWriter = errors.New("aggregator: NewAggregator: writer is nil")

// NewAggregator constructs an aggregator. Returns an error when
// either dependency is nil so the wiring bug surfaces at startup
// rather than at first tick.
func NewAggregator(source SampleSource, writer SnapshotWriter, opts ...AggregatorOption) (*Aggregator, error) {
	if source == nil {
		return nil, ErrAggregatorNilSource
	}
	if writer == nil {
		return nil, ErrAggregatorNilWriter
	}
	a := &Aggregator{
		source: source,
		writer: writer,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

// repoCohortKey groups observations by `(repo_id, metric_kind,
// scope_kind)` for the per-repo snapshot.
type repoCohortKey struct {
	repoID     uuid.UUID
	metricKind string
	scopeKind  string
}

// cohortKey groups observations by `(metric_kind, scope_kind)`
// for the cross-repo and portfolio rows.
type cohortKey struct {
	metricKind string
	scopeKind  string
}

// Tick executes one aggregation pass:
//
//  1. Captures `built_at` from the injected clock (single value
//     shared by every row written this tick).
//  2. Reads the active observation set from the source.
//  3. Groups by `(repo_id, metric_kind, scope_kind)` and computes
//     per-repo summaries (count, mean, p50, p90, p99).
//  4. Groups by `(metric_kind, scope_kind)` and computes
//     cross-repo percentile rows over the FLAT observation-value
//     set across ALL contributing repos (architecture Sec 3.10
//     line 644: "the full per-metric percentile vector across all
//     repos"). Iter-3 evaluator finding #2: prior iterations
//     computed percentiles over per-repo means, which silently
//     drops within-repo variance and was a contract mismatch
//     against the architecture doc.
//  5. For each cohort, also produces the portfolio aggregate
//     (weighted mean across all observations + per-repo entries).
//  6. Calls WriteSnapshots once with all three slices so the
//     PG-backed writer can persist them atomically.
//
// Returns a [Report] summarising the tick. On read or write
// failure, returns the underlying error; the Report value is
// populated with whatever counters were captured up to the
// failure point.
func (a *Aggregator) Tick(ctx context.Context) (Report, error) {
	report := Report{BuiltAt: a.now().UTC()}

	obs, err := a.source.ReadActive(ctx)
	if err != nil {
		return report, fmt.Errorf("aggregator: read active samples: %w", err)
	}
	report.ObservationsRead = len(obs)

	if len(obs) == 0 {
		// No active samples at all -- nothing to snapshot. Emit
		// an empty WriteSnapshots so the writer can record the
		// "fresh built_at, zero rows" tick (a degenerate but
		// legitimate G6 state for a brand-new deployment).
		if err := a.writer.WriteSnapshots(ctx, Snapshots{}); err != nil {
			return report, fmt.Errorf("aggregator: write snapshots (empty tick): %w", err)
		}
		return report, nil
	}

	// Step 1: bucket observations by (repo_id, metric_kind, scope_kind).
	repoCohorts := make(map[repoCohortKey][]float64)
	for _, o := range obs {
		k := repoCohortKey{repoID: o.RepoID, metricKind: o.MetricKind, scopeKind: o.ScopeKind}
		repoCohorts[k] = append(repoCohorts[k], o.Value)
	}

	// Step 2: per-repo summaries -> RepoMetricSnapshotRow set
	// and (metric_kind, scope_kind)-indexed per-repo summary
	// table for the cross-repo / portfolio step. Also collect
	// the FLAT observation-value set per cohort so cross-repo
	// percentiles compute over every contributing sample (not
	// just per-repo means) -- architecture Sec 3.10 line 644.
	type perRepoSummary struct {
		repoID  uuid.UUID
		summary summary
	}
	perCohort := make(map[cohortKey][]perRepoSummary)
	cohortValues := make(map[cohortKey][]float64)
	repoRows := make([]RepoMetricSnapshotRow, 0, len(repoCohorts))
	for k, values := range repoCohorts {
		ck := cohortKey{metricKind: k.metricKind, scopeKind: k.scopeKind}
		// Capture the pristine values BEFORE `summarise` sorts
		// them in-place; `append(dst, src...)` copies each
		// element so the cohort slice is independent of any
		// later mutation to `values`.
		cohortValues[ck] = append(cohortValues[ck], values...)
		s := summarise(values)
		repoRows = append(repoRows, RepoMetricSnapshotRow{
			RepoID:     k.repoID,
			MetricKind: k.metricKind,
			ScopeKind:  k.scopeKind,
			Count:      s.count,
			Mean:       s.mean,
			P50:        s.p50,
			P90:        s.p90,
			P99:        s.p99,
			BuiltAt:    report.BuiltAt,
		})
		perCohort[ck] = append(perCohort[ck], perRepoSummary{repoID: k.repoID, summary: s})
	}

	// Deterministic ordering of repo rows for tests + readability.
	sort.Slice(repoRows, func(i, j int) bool {
		if repoRows[i].MetricKind != repoRows[j].MetricKind {
			return repoRows[i].MetricKind < repoRows[j].MetricKind
		}
		if repoRows[i].ScopeKind != repoRows[j].ScopeKind {
			return repoRows[i].ScopeKind < repoRows[j].ScopeKind
		}
		return repoRows[i].RepoID.String() < repoRows[j].RepoID.String()
	})

	// Step 3: per-cohort cross-repo percentile + portfolio rows.
	crossRows := make([]CrossRepoPercentileRow, 0, len(perCohort))
	portfolioRows := make([]PortfolioSnapshotRow, 0, len(perCohort))
	for ck, entries := range perCohort {
		// Sort entries by repo_id for stable histogram bytes
		// (G6 determinism: identical inputs -> identical
		// histogram_json bytes).
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].repoID.String() < entries[j].repoID.String()
		})

		// Per-repo entries feed the histogram_json + portfolio
		// per-repo block; cross-repo p50/p90/p99 are computed
		// over the FULL flat observation set (cohortValues[ck])
		// per architecture Sec 3.10 line 644.
		var totalObs int64
		var totalValueSum float64
		var unweightedMeanAcc float64
		histEntries := make([]HistogramEntry, len(entries))
		portfolioEntries := make([]PortfolioPerRepoEntry, len(entries))
		for i, e := range entries {
			totalObs += e.summary.count
			totalValueSum += e.summary.mean * float64(e.summary.count)
			unweightedMeanAcc += e.summary.mean
			histEntries[i] = HistogramEntry{
				RepoID: e.repoID.String(),
				Count:  e.summary.count,
				Mean:   e.summary.mean,
				P50:    e.summary.p50,
				P90:    e.summary.p90,
				P99:    e.summary.p99,
			}
			portfolioEntries[i] = PortfolioPerRepoEntry{
				RepoID: e.repoID.String(),
				Count:  e.summary.count,
				Mean:   e.summary.mean,
			}
		}

		crossSummary := summarise(cohortValues[ck])
		var weighted float64
		if totalObs > 0 {
			weighted = totalValueSum / float64(totalObs)
		}
		var unweighted float64
		if len(entries) > 0 {
			unweighted = unweightedMeanAcc / float64(len(entries))
		}

		// Serialise histogram_json with the envelope shape.
		histBytes, err := json.Marshal(HistogramEnvelope{Entries: histEntries})
		if err != nil {
			return report, fmt.Errorf("aggregator: marshal histogram_json (metric_kind=%s, scope_kind=%s): %w", ck.metricKind, ck.scopeKind, err)
		}
		crossRows = append(crossRows, CrossRepoPercentileRow{
			MetricKind:    ck.metricKind,
			ScopeKind:     ck.scopeKind,
			P50:           crossSummary.p50,
			P90:           crossSummary.p90,
			P99:           crossSummary.p99,
			HistogramJSON: histBytes,
			BuiltAt:       report.BuiltAt,
		})

		aggregateBytes, err := json.Marshal(PortfolioAggregate{
			TotalObservations: totalObs,
			RepoCount:         len(entries),
			WeightedMean:      weighted,
			UnweightedMean:    unweighted,
			P50:               crossSummary.p50,
			P90:               crossSummary.p90,
			P99:               crossSummary.p99,
			PerRepo:           portfolioEntries,
		})
		if err != nil {
			return report, fmt.Errorf("aggregator: marshal aggregate_json (metric_kind=%s, scope_kind=%s): %w", ck.metricKind, ck.scopeKind, err)
		}
		portfolioRows = append(portfolioRows, PortfolioSnapshotRow{
			MetricKind:    ck.metricKind,
			ScopeKind:     ck.scopeKind,
			RepoCount:     len(entries),
			AggregateJSON: aggregateBytes,
			BuiltAt:       report.BuiltAt,
		})
	}

	// Deterministic ordering of cross-repo + portfolio rows.
	sort.Slice(crossRows, func(i, j int) bool {
		if crossRows[i].MetricKind != crossRows[j].MetricKind {
			return crossRows[i].MetricKind < crossRows[j].MetricKind
		}
		return crossRows[i].ScopeKind < crossRows[j].ScopeKind
	})
	sort.Slice(portfolioRows, func(i, j int) bool {
		if portfolioRows[i].MetricKind != portfolioRows[j].MetricKind {
			return portfolioRows[i].MetricKind < portfolioRows[j].MetricKind
		}
		return portfolioRows[i].ScopeKind < portfolioRows[j].ScopeKind
	})

	report.CohortsAggregated = len(perCohort)
	report.RepoMetricSnapshotRowsWritten = len(repoRows)
	report.CrossRepoPercentileRowsWritten = len(crossRows)
	report.PortfolioSnapshotRowsWritten = len(portfolioRows)

	snap := Snapshots{
		RepoMetric:       repoRows,
		CrossRepoPercent: crossRows,
		Portfolio:        portfolioRows,
	}
	if err := a.writer.WriteSnapshots(ctx, snap); err != nil {
		return report, fmt.Errorf("aggregator: write snapshots: %w", err)
	}
	return report, nil
}
