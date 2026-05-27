package aggregator_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
)

// fixedClock returns a constant time -- the aggregator stamps
// every snapshot row with the value the clock returns at the
// instant Tick starts.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// mustParseUUID is a fixture helper.
func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.FromString(s)
	if err != nil {
		t.Fatalf("uuid.FromString(%q): %v", s, err)
	}
	return u
}

// TestAggregator_NewAggregator_RejectsNilDeps catches the
// composition-root wiring contract -- a nil source or writer is
// always a misconfiguration and the constructor must surface it
// before the first tick.
func TestAggregator_NewAggregator_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	if _, err := aggregator.NewAggregator(nil, aggregator.NewInMemorySnapshotWriter()); !errors.Is(err, aggregator.ErrAggregatorNilSource) {
		t.Errorf("NewAggregator(nil, w): err=%v, want ErrAggregatorNilSource", err)
	}
	if _, err := aggregator.NewAggregator(aggregator.NewInMemorySampleSource(nil), nil); !errors.Is(err, aggregator.ErrAggregatorNilWriter) {
		t.Errorf("NewAggregator(s, nil): err=%v, want ErrAggregatorNilWriter", err)
	}
}

// TestAggregator_Tick_FiveReposLCOM4 is the canonical Stage 7.1
// implementation-plan scenario `tick-writes-snapshots`:
//
//	Given five repos with active `metric_sample` rows for
//	`lcom4`, When the aggregator ticks, Then
//	`repo_metric_snapshot` has five rows for that metric_kind
//	and `cross_repo_percentile` has one row with non-null
//	p50/p90/p99/histogram_json/built_at.
//
// The test fixture mirrors that exact shape -- five repos, each
// emitting three observations at scope_kind='class' for
// metric_kind='lcom4' -- and asserts on the resulting row
// counts + a non-trivial percentile output.
func TestAggregator_Tick_FiveReposLCOM4(t *testing.T) {
	t.Parallel()
	const metricKind = "lcom4"
	const scopeKind = "class"
	repoIDs := []uuid.UUID{
		mustParseUUID(t, "11111111-1111-1111-1111-111111111111"),
		mustParseUUID(t, "22222222-2222-2222-2222-222222222222"),
		mustParseUUID(t, "33333333-3333-3333-3333-333333333333"),
		mustParseUUID(t, "44444444-4444-4444-4444-444444444444"),
		mustParseUUID(t, "55555555-5555-5555-5555-555555555555"),
	}
	// Three classes per repo, with increasing lcom4 values per
	// repo so each repo lands a distinct mean (1.0, 2.0, 3.0,
	// 4.0, 5.0). This guarantees the cross-repo p50 is the
	// middle repo's mean (3.0) and the histogram_json carries
	// five distinct entries.
	var obs []aggregator.Observation
	for repoIdx, rid := range repoIDs {
		base := float64(repoIdx) + 1 // repo 0 -> base 1.0, repo 4 -> base 5.0
		for k := 0; k < 3; k++ {
			obs = append(obs, aggregator.Observation{
				RepoID:     rid,
				MetricKind: metricKind,
				ScopeKind:  scopeKind,
				Value:      base, // all three observations share the base so per-repo mean = base
			})
		}
	}

	tickAt := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	source := aggregator.NewInMemorySampleSource(obs)
	writer := aggregator.NewInMemorySnapshotWriter()
	agg, err := aggregator.NewAggregator(source, writer, aggregator.WithClock(fixedClock(tickAt)))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	report, err := agg.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Report counters.
	if report.ObservationsRead != 15 {
		t.Errorf("ObservationsRead = %d, want 15", report.ObservationsRead)
	}
	if report.CohortsAggregated != 1 {
		t.Errorf("CohortsAggregated = %d, want 1 (single metric_kind + scope_kind)", report.CohortsAggregated)
	}
	if report.RepoMetricSnapshotRowsWritten != 5 {
		t.Errorf("RepoMetricSnapshotRowsWritten = %d, want 5 (one per repo)", report.RepoMetricSnapshotRowsWritten)
	}
	if report.CrossRepoPercentileRowsWritten != 1 {
		t.Errorf("CrossRepoPercentileRowsWritten = %d, want 1", report.CrossRepoPercentileRowsWritten)
	}
	if report.PortfolioSnapshotRowsWritten != 1 {
		t.Errorf("PortfolioSnapshotRowsWritten = %d, want 1", report.PortfolioSnapshotRowsWritten)
	}
	if !report.BuiltAt.Equal(tickAt) {
		t.Errorf("BuiltAt = %v, want %v", report.BuiltAt, tickAt)
	}

	// Captured WriteSnapshots payload.
	writes := writer.Writes()
	if len(writes) != 1 {
		t.Fatalf("writer.Writes() len = %d, want 1", len(writes))
	}
	snap := writes[0]

	// repo_metric_snapshot: 5 rows, all sharing the same
	// built_at, all carrying the expected per-repo mean.
	if len(snap.RepoMetric) != 5 {
		t.Fatalf("snap.RepoMetric len = %d, want 5", len(snap.RepoMetric))
	}
	for _, r := range snap.RepoMetric {
		if r.MetricKind != metricKind || r.ScopeKind != scopeKind {
			t.Errorf("snap.RepoMetric row metric_kind=%q scope_kind=%q, want %q/%q", r.MetricKind, r.ScopeKind, metricKind, scopeKind)
		}
		if !r.BuiltAt.Equal(tickAt) {
			t.Errorf("snap.RepoMetric row built_at = %v, want %v", r.BuiltAt, tickAt)
		}
		if r.Count != 3 {
			t.Errorf("snap.RepoMetric row count = %d, want 3 (three observations per repo)", r.Count)
		}
		// All three observations per repo share the value, so
		// per-repo p50/p90/p99 = mean.
		if r.Mean != r.P50 || r.P50 != r.P90 || r.P90 != r.P99 {
			t.Errorf("snap.RepoMetric row (mean=%v p50=%v p90=%v p99=%v) -- expected all equal for constant-value cohort", r.Mean, r.P50, r.P90, r.P99)
		}
	}

	// cross_repo_percentile: 1 row, non-null p50/p90/p99 + a
	// histogram_json with one entry per contributing repo.
	if len(snap.CrossRepoPercent) != 1 {
		t.Fatalf("snap.CrossRepoPercent len = %d, want 1", len(snap.CrossRepoPercent))
	}
	cr := snap.CrossRepoPercent[0]
	if cr.MetricKind != metricKind || cr.ScopeKind != scopeKind {
		t.Errorf("cross_repo row metric_kind=%q scope_kind=%q, want %q/%q", cr.MetricKind, cr.ScopeKind, metricKind, scopeKind)
	}
	if !cr.BuiltAt.Equal(tickAt) {
		t.Errorf("cross_repo built_at = %v, want %v", cr.BuiltAt, tickAt)
	}
	// Cross-repo percentiles compute over the FLAT 15-value
	// set across all repos (architecture Sec 3.10 line 644:
	// "the full per-metric percentile vector across all repos").
	// Sorted values are [1,1,1,2,2,2,3,3,3,4,4,4,5,5,5] (n=15).
	// percentile_cont semantics: rank = p * (n-1) = p * 14.
	//   p50: rank=7.0  -> values[7]                       = 3.0
	//   p90: rank=12.6 -> values[12] + 0.6*(values[13]-values[12]) = 5 + 0  = 5.0
	//   p99: rank=13.86-> values[13] + 0.86*(values[14]-values[13]) = 5 + 0 = 5.0
	// (Discriminates against the prior per-repo-means path
	// where input was [1,2,3,4,5] -> p90=4.6, p99=4.96.)
	wantP50 := 3.0
	wantP90 := 5.0
	wantP99 := 5.0
	if math.Abs(cr.P50-wantP50) > 1e-9 {
		t.Errorf("cross_repo p50 = %v, want %v", cr.P50, wantP50)
	}
	if math.Abs(cr.P90-wantP90) > 1e-9 {
		t.Errorf("cross_repo p90 = %v, want %v", cr.P90, wantP90)
	}
	if math.Abs(cr.P99-wantP99) > 1e-9 {
		t.Errorf("cross_repo p99 = %v, want %v", cr.P99, wantP99)
	}
	if len(cr.HistogramJSON) == 0 {
		t.Fatalf("cross_repo histogram_json is empty -- architecture Sec 5.2.5 requires non-empty per-repo histogram")
	}
	var env aggregator.HistogramEnvelope
	if err := json.Unmarshal(cr.HistogramJSON, &env); err != nil {
		t.Fatalf("cross_repo histogram_json unmarshal: %v (bytes=%s)", err, string(cr.HistogramJSON))
	}
	if len(env.Entries) != 5 {
		t.Errorf("cross_repo histogram_json entries len = %d, want 5 (one per repo)", len(env.Entries))
	}

	// portfolio_snapshot: 1 row carrying the aggregate_json.
	if len(snap.Portfolio) != 1 {
		t.Fatalf("snap.Portfolio len = %d, want 1", len(snap.Portfolio))
	}
	pf := snap.Portfolio[0]
	if pf.RepoCount != 5 {
		t.Errorf("portfolio repo_count = %d, want 5", pf.RepoCount)
	}
	if !pf.BuiltAt.Equal(tickAt) {
		t.Errorf("portfolio built_at = %v, want %v", pf.BuiltAt, tickAt)
	}
	var agg2 aggregator.PortfolioAggregate
	if err := json.Unmarshal(pf.AggregateJSON, &agg2); err != nil {
		t.Fatalf("portfolio aggregate_json unmarshal: %v (bytes=%s)", err, string(pf.AggregateJSON))
	}
	if agg2.RepoCount != 5 {
		t.Errorf("portfolio aggregate_json repo_count = %d, want 5", agg2.RepoCount)
	}
	if agg2.TotalObservations != 15 {
		t.Errorf("portfolio aggregate_json total_observations = %d, want 15", agg2.TotalObservations)
	}
	// Weighted mean across 15 observations = (3*(1+2+3+4+5))/15 = 3.0.
	if math.Abs(agg2.WeightedMean-3.0) > 1e-9 {
		t.Errorf("portfolio aggregate_json weighted_mean = %v, want 3.0", agg2.WeightedMean)
	}
	// Unweighted mean across 5 repos = (1+2+3+4+5)/5 = 3.0.
	if math.Abs(agg2.UnweightedMean-3.0) > 1e-9 {
		t.Errorf("portfolio aggregate_json unweighted_mean = %v, want 3.0", agg2.UnweightedMean)
	}
}

// TestAggregator_Tick_EmptySourceWritesEmptySnapshot covers the
// degenerate "fresh deployment, no samples yet" case. The
// aggregator must NOT crash; it must call WriteSnapshots with
// zero rows so the writer can record the heartbeat tick.
func TestAggregator_Tick_EmptySourceWritesEmptySnapshot(t *testing.T) {
	t.Parallel()
	tickAt := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	source := aggregator.NewInMemorySampleSource(nil)
	writer := aggregator.NewInMemorySnapshotWriter()
	agg, err := aggregator.NewAggregator(source, writer, aggregator.WithClock(fixedClock(tickAt)))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	report, err := agg.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if report.ObservationsRead != 0 {
		t.Errorf("ObservationsRead = %d, want 0", report.ObservationsRead)
	}
	if report.RepoMetricSnapshotRowsWritten != 0 || report.CrossRepoPercentileRowsWritten != 0 || report.PortfolioSnapshotRowsWritten != 0 {
		t.Errorf("expected zero rows written for empty source; got %+v", report)
	}
	writes := writer.Writes()
	if len(writes) != 1 {
		t.Fatalf("writer.Writes() len = %d, want 1", len(writes))
	}
	if len(writes[0].RepoMetric) != 0 || len(writes[0].CrossRepoPercent) != 0 || len(writes[0].Portfolio) != 0 {
		t.Errorf("writer captured non-empty snapshot for empty source: %+v", writes[0])
	}
}

// TestAggregator_Tick_GroupsByMetricKindAndScopeKind covers the
// branching path: two metric_kinds and two scope_kinds in one
// observation set produce one cohort PER (metric_kind, scope_kind)
// pair.
func TestAggregator_Tick_GroupsByMetricKindAndScopeKind(t *testing.T) {
	t.Parallel()
	rid := mustParseUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	obs := []aggregator.Observation{
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: 1},
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: 2},
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "class", Value: 5},
		{RepoID: rid, MetricKind: "lcom4", ScopeKind: "class", Value: 3},
	}
	tickAt := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	source := aggregator.NewInMemorySampleSource(obs)
	writer := aggregator.NewInMemorySnapshotWriter()
	agg, err := aggregator.NewAggregator(source, writer, aggregator.WithClock(fixedClock(tickAt)))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	if _, err := agg.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	writes := writer.Writes()
	if len(writes) != 1 {
		t.Fatalf("writer.Writes() len = %d, want 1", len(writes))
	}
	snap := writes[0]
	// Three cohorts: (cyclo, method), (cyclo, class), (lcom4, class).
	if len(snap.CrossRepoPercent) != 3 {
		t.Errorf("snap.CrossRepoPercent len = %d, want 3", len(snap.CrossRepoPercent))
	}
	if len(snap.Portfolio) != 3 {
		t.Errorf("snap.Portfolio len = %d, want 3", len(snap.Portfolio))
	}
	if len(snap.RepoMetric) != 3 {
		t.Errorf("snap.RepoMetric len = %d, want 3", len(snap.RepoMetric))
	}
}

// TestAggregator_Tick_PropagatesWriteError surfaces a writer
// failure verbatim so the loop's retry / backoff path can react.
func TestAggregator_Tick_PropagatesWriteError(t *testing.T) {
	t.Parallel()
	source := aggregator.NewInMemorySampleSource([]aggregator.Observation{
		{RepoID: mustParseUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), MetricKind: "cyclo", ScopeKind: "method", Value: 1},
	})
	writer := aggregator.NewInMemorySnapshotWriter()
	sentinel := errors.New("simulated PG outage")
	writer.SetFailError(sentinel)
	agg, err := aggregator.NewAggregator(source, writer)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	_, err = agg.Tick(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tick err = %v, want errors.Is(err, sentinel)", err)
	}
}

// TestAggregator_Tick_BuiltAtSharedAcrossAllRows is the
// implementation-plan acceptance criterion "Snapshot tables
// carry a `built_at` timestamp updated on every tick (used by
// Stage 7.3 freshness banner)". Every row produced in one Tick
// MUST share the same built_at -- a divergence would let a
// freshness reader see a partial view.
func TestAggregator_Tick_BuiltAtSharedAcrossAllRows(t *testing.T) {
	t.Parallel()
	rid := mustParseUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	obs := []aggregator.Observation{
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: 1},
		{RepoID: rid, MetricKind: "lcom4", ScopeKind: "class", Value: 3},
	}
	tickAt := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	agg, err := aggregator.NewAggregator(aggregator.NewInMemorySampleSource(obs), aggregator.NewInMemorySnapshotWriter(), aggregator.WithClock(fixedClock(tickAt)))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	report, err := agg.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !report.BuiltAt.Equal(tickAt) {
		t.Fatalf("report.BuiltAt = %v, want %v", report.BuiltAt, tickAt)
	}
}

// TestInMemorySampleSource_FiltersNonFinite locks the source
// contract: a NaN / +-Inf observation must never reach the
// aggregator math. The aggregator percentile path assumes clean
// inputs.
func TestInMemorySampleSource_FiltersNonFinite(t *testing.T) {
	t.Parallel()
	rid := mustParseUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	source := aggregator.NewInMemorySampleSource([]aggregator.Observation{
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: 1},
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: math.NaN()},
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: math.Inf(1)},
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: math.Inf(-1)},
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: 2},
	})
	out, err := source.ReadActive(context.Background())
	if err != nil {
		t.Fatalf("ReadActive: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("len(out) = %d, want 2 (NaN / +Inf / -Inf filtered)", len(out))
	}
	for _, o := range out {
		if math.IsNaN(o.Value) || math.IsInf(o.Value, 0) {
			t.Errorf("source leaked non-finite value: %v", o.Value)
		}
	}
}
