package management

// Stage 6.3 -- reader_test.go covers the eight `mgmt.read.*`
// verbs through a pure-Go fake [MetricsBackend]. Both READ
// MODES land here:
//
//   * SHA-pinned suite: covers the active-row contract
//     (impl-plan Stage 6.3 scenario
//     `sha-pinned-returns-active-row`), the per-row
//     post-condition invariant guards, and the regressions
//     `delta='newly_failing'` filter.
//
//   * Latest-dashboard suite: covers verbatim snapshot
//     return (`latest-dashboard-returns-snapshot`) plus the
//     Stage 7.3 freshness banner (`stale-percentile-banner-on-insights`,
//     `fresh-percentile-no-banner`).
//
// The fake backend lives in this file only; the production
// PG-backed implementation is [PGMetricsBackend] in
// `pg_metrics_backend.go` (same package), with sqlmock-driven
// SQL-trace tests in `pg_metrics_backend_test.go`.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/management/insights"
)

// -----------------------------------------------------------
// fakeBackend -- in-memory [MetricsBackend] for tests.
// -----------------------------------------------------------

// fakeBackend implements [MetricsBackend] with public maps the
// tests populate directly. Each map's key is the request's
// natural key so a fixture set is trivial to seed.
type fakeBackend struct {
	repos          map[uuid.UUID]*RepoRow
	metricSamples  map[metricSampleKey]*MetricSampleRow
	samplesByRepo  map[repoShaKey][]MetricSampleRow
	findings       map[repoShaKey][]FindingRow
	regressions    map[repoShaKey][]FindingRow
	plans          map[repoShaKey]*RefactorPlanRow
	crossRepo      map[crossRepoKey]*CrossRepoRow
	portfolios     map[string][]PortfolioRow
	failNext       error
}

type metricSampleKey struct {
	RepoID     uuid.UUID
	SHA        string
	ScopeID    uuid.UUID
	MetricKind string
}

type repoShaKey struct {
	RepoID uuid.UUID
	SHA    string
}

type crossRepoKey struct {
	MetricKind string
	ScopeKind  string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		repos:         map[uuid.UUID]*RepoRow{},
		metricSamples: map[metricSampleKey]*MetricSampleRow{},
		samplesByRepo: map[repoShaKey][]MetricSampleRow{},
		findings:      map[repoShaKey][]FindingRow{},
		regressions:   map[repoShaKey][]FindingRow{},
		plans:         map[repoShaKey]*RefactorPlanRow{},
		crossRepo:     map[crossRepoKey]*CrossRepoRow{},
		portfolios:    map[string][]PortfolioRow{},
	}
}

func (f *fakeBackend) ReadRepo(_ context.Context, id uuid.UUID) (*RepoRow, error) {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	row, ok := f.repos[id]
	if !ok {
		return nil, ErrNotFound
	}
	return row, nil
}

func (f *fakeBackend) ReadMetricSample(_ context.Context, repoID uuid.UUID, sha string, scopeID uuid.UUID, metricKind string) (*MetricSampleRow, error) {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	row, ok := f.metricSamples[metricSampleKey{repoID, sha, scopeID, metricKind}]
	if !ok {
		return nil, ErrNotFound
	}
	return row, nil
}

func (f *fakeBackend) ReadMetricSamples(_ context.Context, repoID uuid.UUID, sha string, filter MetricSamplesFilter) ([]MetricSampleRow, error) {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	rows := f.samplesByRepo[repoShaKey{repoID, sha}]
	if isEmptyFilter(filter) {
		return rows, nil
	}
	out := make([]MetricSampleRow, 0, len(rows))
	for _, r := range rows {
		if filter.MetricKind != "" && r.MetricKind != filter.MetricKind {
			continue
		}
		if !filter.ScopeID.IsNil() && r.ScopeID != filter.ScopeID {
			continue
		}
		if filter.Pack != "" && r.Pack != filter.Pack {
			continue
		}
		if filter.Source != "" && r.Source != filter.Source {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func isEmptyFilter(f MetricSamplesFilter) bool {
	return f.MetricKind == "" && f.ScopeID.IsNil() && f.Pack == "" && f.Source == ""
}

func (f *fakeBackend) ReadFindings(_ context.Context, repoID uuid.UUID, sha string) ([]FindingRow, error) {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	return f.findings[repoShaKey{repoID, sha}], nil
}

func (f *fakeBackend) ReadRegressions(_ context.Context, repoID uuid.UUID, sha string) ([]FindingRow, error) {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	return f.regressions[repoShaKey{repoID, sha}], nil
}

func (f *fakeBackend) ReadRefactorPlan(_ context.Context, repoID uuid.UUID, sha string) (*RefactorPlanRow, error) {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	row, ok := f.plans[repoShaKey{repoID, sha}]
	if !ok {
		return nil, ErrNotFound
	}
	return row, nil
}

func (f *fakeBackend) ReadCrossRepo(_ context.Context, metricKind, scopeKind string) (*CrossRepoRow, error) {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	row, ok := f.crossRepo[crossRepoKey{metricKind, scopeKind}]
	if !ok {
		return nil, ErrNotFound
	}
	return row, nil
}

func (f *fakeBackend) ReadPortfolio(_ context.Context, metricKind string) ([]PortfolioRow, error) {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	return f.portfolios[metricKind], nil
}

// Compile-time guard: the fake satisfies the interface.
var _ MetricsBackend = (*fakeBackend)(nil)

// fixedClock returns a constant time; used to make freshness
// staleness deterministic.
type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid.NewV4: %v", err)
	}
	return id
}

// -----------------------------------------------------------
// Backend-unavailable tests.
// -----------------------------------------------------------

func TestReader_ReadRepo_BackendUnavailable(t *testing.T) {
	t.Parallel()
	r := NewReader(nil)
	_, err := r.ReadRepo(context.Background(), mustUUID(t))
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("err=%v; want ErrBackendUnavailable", err)
	}
}

func TestReader_ReadMetricSample_BackendUnavailable(t *testing.T) {
	t.Parallel()
	r := NewReader(nil)
	_, err := r.ReadMetricSample(context.Background(), mustUUID(t), "abc", mustUUID(t), "cyclo")
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("err=%v; want ErrBackendUnavailable", err)
	}
}

func TestReader_ReadCrossRepo_BackendUnavailable(t *testing.T) {
	t.Parallel()
	r := NewReader(nil)
	_, err := r.ReadCrossRepo(context.Background(), "cyclo", "method")
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("err=%v; want ErrBackendUnavailable", err)
	}
}

// -----------------------------------------------------------
// SHA-pinned: ReadRepo.
// -----------------------------------------------------------

func TestReader_ReadRepo_ReturnsRowWithSHAPinnedMode(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	repoID := mustUUID(t)
	fb.repos[repoID] = &RepoRow{
		RepoID:        repoID,
		DisplayName:   "demo",
		DefaultBranch: "main",
		Mode:          "embedded",
		RepoURL:       "https://example/demo.git",
		CreatedAt:     time.Unix(1_700_000_000, 0).UTC(),
	}
	r := NewReader(nil, WithMetricsBackend(fb))

	resp, err := r.ReadRepo(context.Background(), repoID)
	if err != nil {
		t.Fatalf("ReadRepo: %v", err)
	}
	if resp.Mode != ReadModeSHAPinned {
		t.Errorf("Mode=%q, want %q", resp.Mode, ReadModeSHAPinned)
	}
	if resp.Repo.DisplayName != "demo" {
		t.Errorf("Repo.DisplayName=%q, want demo", resp.Repo.DisplayName)
	}
}

func TestReader_ReadRepo_NotFound(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	r := NewReader(nil, WithMetricsBackend(fb))
	_, err := r.ReadRepo(context.Background(), mustUUID(t))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v; want ErrNotFound", err)
	}
}

func TestReader_ReadRepo_MismatchedRepoIDIsInvariantViolation(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	requested := mustUUID(t)
	wrong := mustUUID(t)
	// Backend bug: the fake returns a row for the requested
	// key but the row carries a different repo_id.
	fb.repos[requested] = &RepoRow{RepoID: wrong, DisplayName: "wrong"}
	r := NewReader(nil, WithMetricsBackend(fb))
	_, err := r.ReadRepo(context.Background(), requested)
	if !errors.Is(err, ErrBackendInvariantViolation) {
		t.Fatalf("err=%v; want ErrBackendInvariantViolation", err)
	}
}

// -----------------------------------------------------------
// SHA-pinned: ReadMetricSample (active-row contract).
// -----------------------------------------------------------

// TestReader_ReadMetricSample_ReturnsActiveRow pins impl-plan
// Stage 6.3 scenario `sha-pinned-returns-active-row`. The
// fake's `metricSamples` map only ever stores the ACTIVE row
// (the older retracted row is filtered out at the backend
// boundary -- the fake mirrors the contract by simply not
// holding the retracted sample in the map).
func TestReader_ReadMetricSample_ReturnsActiveRow(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	repoID := mustUUID(t)
	scopeID := mustUUID(t)
	activeSampleID := mustUUID(t)
	val := 7.5
	activeRow := &MetricSampleRow{
		SampleID:      activeSampleID,
		RepoID:        repoID,
		SHA:           "abc123",
		ScopeID:       scopeID,
		MetricKind:    "cyclo",
		MetricVersion: 1,
		Value:         &val,
		Pack:          "foundation",
		Source:        "computed",
		CreatedAt:     time.Unix(1_700_000_000, 0).UTC(),
	}
	fb.metricSamples[metricSampleKey{repoID, "abc123", scopeID, "cyclo"}] = activeRow
	r := NewReader(nil, WithMetricsBackend(fb))

	resp, err := r.ReadMetricSample(context.Background(), repoID, "abc123", scopeID, "cyclo")
	if err != nil {
		t.Fatalf("ReadMetricSample: %v", err)
	}
	if resp.Mode != ReadModeSHAPinned {
		t.Errorf("Mode=%q, want %q", resp.Mode, ReadModeSHAPinned)
	}
	if resp.Sample.SampleID != activeSampleID {
		t.Errorf("Sample.SampleID=%s, want active=%s", resp.Sample.SampleID, activeSampleID)
	}
	if resp.Sample.Value == nil || *resp.Sample.Value != 7.5 {
		t.Errorf("Sample.Value=%v, want 7.5", resp.Sample.Value)
	}
}

func TestReader_ReadMetricSample_NotFound(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	r := NewReader(nil, WithMetricsBackend(fb))
	_, err := r.ReadMetricSample(context.Background(), mustUUID(t), "abc", mustUUID(t), "cyclo")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v; want ErrNotFound", err)
	}
}

// TestReader_ReadMetricSample_MismatchedSHAIsInvariantViolation
// asserts the Reader's defensive post-condition guard: a
// backend that returns a row for a different SHA (a writer
// bug or a fake misconfigured by a test author) trips
// [ErrBackendInvariantViolation] at the boundary -- the
// architecture Sec 5.2.1 lines 971-977 "NEVER silently
// substitute" contract is enforced here, not just in SQL.
func TestReader_ReadMetricSample_MismatchedSHAIsInvariantViolation(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	repoID := mustUUID(t)
	scopeID := mustUUID(t)
	// Backend "bug": the fake stores a row whose SHA does
	// NOT match the lookup key.
	fb.metricSamples[metricSampleKey{repoID, "requested-sha", scopeID, "cyclo"}] = &MetricSampleRow{
		SampleID:      mustUUID(t),
		RepoID:        repoID,
		SHA:           "different-sha",
		ScopeID:       scopeID,
		MetricKind:    "cyclo",
		MetricVersion: 1,
	}
	r := NewReader(nil, WithMetricsBackend(fb))
	_, err := r.ReadMetricSample(context.Background(), repoID, "requested-sha", scopeID, "cyclo")
	if !errors.Is(err, ErrBackendInvariantViolation) {
		t.Fatalf("err=%v; want ErrBackendInvariantViolation", err)
	}
}

// -----------------------------------------------------------
// SHA-pinned: ReadMetricSamples.
// -----------------------------------------------------------

func TestReader_ReadMetricSamples_FiltersAndModeTag(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	repoID := mustUUID(t)
	sha := "abc"
	scopeA := mustUUID(t)
	scopeB := mustUUID(t)
	v1, v2 := 1.0, 2.0
	fb.samplesByRepo[repoShaKey{repoID, sha}] = []MetricSampleRow{
		{SampleID: mustUUID(t), RepoID: repoID, SHA: sha, ScopeID: scopeA, MetricKind: "cyclo", MetricVersion: 1, Value: &v1, Pack: "foundation", Source: "computed"},
		{SampleID: mustUUID(t), RepoID: repoID, SHA: sha, ScopeID: scopeB, MetricKind: "lcom4", MetricVersion: 1, Value: &v2, Pack: "foundation", Source: "computed"},
	}
	r := NewReader(nil, WithMetricsBackend(fb))

	// Empty filter -> both samples.
	all, err := r.ReadMetricSamples(context.Background(), repoID, sha, MetricSamplesFilter{})
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if all.Mode != ReadModeSHAPinned {
		t.Errorf("Mode=%q, want %q", all.Mode, ReadModeSHAPinned)
	}
	if len(all.Samples) != 2 {
		t.Errorf("len(Samples)=%d, want 2", len(all.Samples))
	}

	// Filter on metric_kind=cyclo.
	cyclo, err := r.ReadMetricSamples(context.Background(), repoID, sha, MetricSamplesFilter{MetricKind: "cyclo"})
	if err != nil {
		t.Fatalf("cyclo: %v", err)
	}
	if len(cyclo.Samples) != 1 || cyclo.Samples[0].MetricKind != "cyclo" {
		t.Errorf("cyclo filter returned %+v", cyclo.Samples)
	}

	// Filter on scope_id=scopeB.
	bOnly, err := r.ReadMetricSamples(context.Background(), repoID, sha, MetricSamplesFilter{ScopeID: scopeB})
	if err != nil {
		t.Fatalf("bOnly: %v", err)
	}
	if len(bOnly.Samples) != 1 || bOnly.Samples[0].ScopeID != scopeB {
		t.Errorf("scope filter returned %+v", bOnly.Samples)
	}
}

func TestReader_ReadMetricSamples_EmptyResultStillReturnsSliceNotNil(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	r := NewReader(nil, WithMetricsBackend(fb))
	resp, err := r.ReadMetricSamples(context.Background(), mustUUID(t), "abc", MetricSamplesFilter{})
	if err != nil {
		t.Fatalf("ReadMetricSamples: %v", err)
	}
	if resp.Samples == nil {
		t.Error("Samples is nil; want non-nil empty slice for stable JSON shape")
	}
	if len(resp.Samples) != 0 {
		t.Errorf("len(Samples)=%d, want 0", len(resp.Samples))
	}
}

// -----------------------------------------------------------
// SHA-pinned: ReadFindings.
// -----------------------------------------------------------

func TestReader_ReadFindings_ReturnsAllFindingsAtSHA(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	repoID := mustUUID(t)
	sha := "deadbeef"
	fb.findings[repoShaKey{repoID, sha}] = []FindingRow{
		{FindingID: mustUUID(t), RepoID: repoID, SHA: sha, RuleID: "solid.srp", Severity: "block", Delta: "newly_failing"},
		{FindingID: mustUUID(t), RepoID: repoID, SHA: sha, RuleID: "solid.dip", Severity: "warn", Delta: "unchanged"},
	}
	r := NewReader(nil, WithMetricsBackend(fb))

	resp, err := r.ReadFindings(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("ReadFindings: %v", err)
	}
	if resp.Mode != ReadModeSHAPinned {
		t.Errorf("Mode=%q, want %q", resp.Mode, ReadModeSHAPinned)
	}
	if len(resp.Findings) != 2 {
		t.Errorf("len(Findings)=%d, want 2", len(resp.Findings))
	}
}

// -----------------------------------------------------------
// SHA-pinned: ReadRegressions (delta='newly_failing' filter).
// -----------------------------------------------------------

func TestReader_ReadRegressions_ReturnsOnlyNewlyFailing(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	repoID := mustUUID(t)
	sha := "deadbeef"
	// The backend already filters; we seed only newly_failing
	// rows.
	fb.regressions[repoShaKey{repoID, sha}] = []FindingRow{
		{FindingID: mustUUID(t), RepoID: repoID, SHA: sha, RuleID: "solid.srp", Severity: "block", Delta: "newly_failing"},
	}
	r := NewReader(nil, WithMetricsBackend(fb))

	resp, err := r.ReadRegressions(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("ReadRegressions: %v", err)
	}
	if resp.Mode != ReadModeSHAPinned {
		t.Errorf("Mode=%q, want %q", resp.Mode, ReadModeSHAPinned)
	}
	if len(resp.Findings) != 1 || resp.Findings[0].Delta != "newly_failing" {
		t.Errorf("regressions=%+v, want exactly one delta=newly_failing", resp.Findings)
	}
}

// TestReader_ReadRegressions_NonRegressionRowIsInvariantViolation
// asserts the Reader's per-row guard: a backend that returns
// a `delta='resolved'` row from ReadRegressions trips the
// invariant guard so the canon stays uniform across the wire
// (architecture Sec 5.4.1 lines 1188-1190 -- regressions is
// the `newly_failing` bucket only).
func TestReader_ReadRegressions_NonRegressionRowIsInvariantViolation(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	repoID := mustUUID(t)
	sha := "deadbeef"
	fb.regressions[repoShaKey{repoID, sha}] = []FindingRow{
		{FindingID: mustUUID(t), RepoID: repoID, SHA: sha, Delta: "resolved"},
	}
	r := NewReader(nil, WithMetricsBackend(fb))
	_, err := r.ReadRegressions(context.Background(), repoID, sha)
	if !errors.Is(err, ErrBackendInvariantViolation) {
		t.Fatalf("err=%v; want ErrBackendInvariantViolation", err)
	}
}

// -----------------------------------------------------------
// SHA-pinned: ReadRefactorPlan.
// -----------------------------------------------------------

func TestReader_ReadRefactorPlan_ReturnsPlanWithEmbeddedTasks(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	repoID := mustUUID(t)
	sha := "v1"
	planID := mustUUID(t)
	taskID := mustUUID(t)
	fb.plans[repoShaKey{repoID, sha}] = &RefactorPlanRow{
		PlanID:     planID,
		RepoID:     repoID,
		SHA:        sha,
		HotspotIDs: []uuid.UUID{mustUUID(t)},
		SummaryMD:  "Top 3 hotspots",
		CreatedAt:  time.Unix(1_700_000_000, 0).UTC(),
		Tasks: []RefactorTaskRow{
			{TaskID: taskID, PlanID: planID, ScopeID: mustUUID(t), Kind: "split_class", EffortHours: 4, RuleID: "solid.srp", DescriptionMD: "Extract X"},
		},
	}
	r := NewReader(nil, WithMetricsBackend(fb))

	resp, err := r.ReadRefactorPlan(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("ReadRefactorPlan: %v", err)
	}
	if resp.Mode != ReadModeSHAPinned {
		t.Errorf("Mode=%q, want %q", resp.Mode, ReadModeSHAPinned)
	}
	if resp.Plan.PlanID != planID {
		t.Errorf("Plan.PlanID=%s, want %s", resp.Plan.PlanID, planID)
	}
	if len(resp.Plan.Tasks) != 1 || resp.Plan.Tasks[0].Kind != "split_class" {
		t.Errorf("tasks=%+v, want one split_class task", resp.Plan.Tasks)
	}
}

func TestReader_ReadRefactorPlan_NotFound(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	r := NewReader(nil, WithMetricsBackend(fb))
	_, err := r.ReadRefactorPlan(context.Background(), mustUUID(t), "abc")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v; want ErrNotFound", err)
	}
}

// -----------------------------------------------------------
// Latest-dashboard: ReadCrossRepo.
// -----------------------------------------------------------

// TestReader_ReadCrossRepo_ReturnsSnapshotVerbatim pins
// impl-plan Stage 6.3 scenario `latest-dashboard-returns-snapshot`
// + fresh-percentile-no-banner (Stage 7.3). The freshness
// projection runs with a fixed clock so the snapshot's
// `built_at` is WITHIN the freshness window -- the response
// envelope MUST carry `Degraded=false` and an empty
// DegradedReason.
func TestReader_ReadCrossRepo_ReturnsSnapshotVerbatim(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	builtAt := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	hist := json.RawMessage(`{"buckets":[1,2,3]}`)
	fb.crossRepo[crossRepoKey{"cyclo", "method"}] = &CrossRepoRow{
		PercentileID:  mustUUID(t),
		MetricKind:    "cyclo",
		ScopeKind:     "method",
		P50:           3.0,
		P90:           7.0,
		P99:           12.0,
		HistogramJSON: hist,
		BuiltAt:       builtAt,
	}
	// Now is 10 minutes after built_at -- well within
	// FreshnessWindowSeconds (3600s).
	now := builtAt.Add(10 * time.Minute)
	fresh := &insights.Freshness{
		Window: time.Duration(insights.FreshnessWindowSeconds) * time.Second,
		Clock:  fixedClock{now},
	}
	r := NewReader(nil, WithMetricsBackend(fb), WithInsightsFreshness(fresh))

	resp, err := r.ReadCrossRepo(context.Background(), "cyclo", "method")
	if err != nil {
		t.Fatalf("ReadCrossRepo: %v", err)
	}
	if resp.Mode != ReadModeLatestDashboard {
		t.Errorf("Mode=%q, want %q", resp.Mode, ReadModeLatestDashboard)
	}
	if resp.Row.P50 != 3.0 || resp.Row.P90 != 7.0 || resp.Row.P99 != 12.0 {
		t.Errorf("Row=%+v, want p50=3 p90=7 p99=12 (verbatim)", resp.Row)
	}
	if string(resp.Row.HistogramJSON) != string(hist) {
		t.Errorf("HistogramJSON=%s, want %s (verbatim)", resp.Row.HistogramJSON, hist)
	}
	if resp.Degraded {
		t.Errorf("Degraded=true; want false for fresh snapshot")
	}
	if resp.DegradedReason != "" {
		t.Errorf("DegradedReason=%q, want empty", resp.DegradedReason)
	}
}

// TestReader_ReadCrossRepo_StaleSnapshotEmitsPercentileStale
// pins Stage 7.3 scenario `stale-percentile-banner-on-insights`:
// a `built_at` older than the freshness window stamps
// `degraded=true, degraded_reason='percentile_stale'`.
func TestReader_ReadCrossRepo_StaleSnapshotEmitsPercentileStale(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	builtAt := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	fb.crossRepo[crossRepoKey{"cyclo", "method"}] = &CrossRepoRow{
		MetricKind: "cyclo",
		ScopeKind:  "method",
		P50:        1.0, P90: 2.0, P99: 3.0,
		BuiltAt: builtAt,
	}
	// Now is 2h after built_at -- past the 1h
	// FreshnessWindowSeconds threshold.
	now := builtAt.Add(2 * time.Hour)
	fresh := &insights.Freshness{
		Window: time.Duration(insights.FreshnessWindowSeconds) * time.Second,
		Clock:  fixedClock{now},
	}
	r := NewReader(nil, WithMetricsBackend(fb), WithInsightsFreshness(fresh))

	resp, err := r.ReadCrossRepo(context.Background(), "cyclo", "method")
	if err != nil {
		t.Fatalf("ReadCrossRepo: %v", err)
	}
	if !resp.Degraded {
		t.Error("Degraded=false; want true for stale snapshot")
	}
	if resp.DegradedReason != insights.DegradedReasonPercentileStale {
		t.Errorf("DegradedReason=%q, want %q", resp.DegradedReason, insights.DegradedReasonPercentileStale)
	}
}

func TestReader_ReadCrossRepo_NotFound(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	r := NewReader(nil, WithMetricsBackend(fb))
	_, err := r.ReadCrossRepo(context.Background(), "cyclo", "method")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v; want ErrNotFound", err)
	}
}

// TestReader_ReadCrossRepo_WithoutFreshnessExplicitlyDisabled
// asserts the documented contract: when the composition root
// EXPLICITLY opts out via [WithoutFreshness] (e.g. a one-off
// operator tool that wants the raw snapshot regardless of
// staleness), the latest-dashboard verbs still return the
// snapshot row but the freshness banner is suppressed
// (Degraded=false, no reason).
//
// Note (iter 2 evaluator item 5): the SILENT-skip path -- a
// composition root that simply forgot to inject freshness --
// is no longer permitted; [NewReader] auto-defaults to
// [insights.NewPercentileFreshness] in that case. This test
// exercises the EXPLICIT opt-out branch via
// [WithoutFreshness].
func TestReader_ReadCrossRepo_WithoutFreshnessExplicitlyDisabled(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	// built_at is one year ago -- would be stale if freshness
	// were wired.
	fb.crossRepo[crossRepoKey{"cyclo", "method"}] = &CrossRepoRow{
		MetricKind: "cyclo", ScopeKind: "method",
		BuiltAt: time.Now().Add(-365 * 24 * time.Hour),
	}
	r := NewReader(nil, WithMetricsBackend(fb), WithoutFreshness())

	resp, err := r.ReadCrossRepo(context.Background(), "cyclo", "method")
	if err != nil {
		t.Fatalf("ReadCrossRepo: %v", err)
	}
	if resp.Degraded {
		t.Error("Degraded=true with WithoutFreshness; want suppressed banner")
	}
	if resp.DegradedReason != "" {
		t.Errorf("DegradedReason=%q with WithoutFreshness, want empty", resp.DegradedReason)
	}
}

// TestReader_NewReader_AutoDefaultsFreshness pins the iter-2
// evaluator item 5 fix: a composition root that forgot to
// wire [WithInsightsFreshness] previously got
// `Degraded=false` on stale snapshots silently. After the
// fix, [NewReader] auto-defaults to
// [insights.NewPercentileFreshness], so a snapshot whose
// `built_at` is older than [insights.FreshnessWindowSeconds]
// is reported as degraded WITHOUT any explicit composition-
// root wiring.
func TestReader_NewReader_AutoDefaultsFreshness(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	// One year old -- comfortably past the 3600s window.
	fb.crossRepo[crossRepoKey{"cyclo", "method"}] = &CrossRepoRow{
		MetricKind: "cyclo", ScopeKind: "method",
		BuiltAt: time.Now().Add(-365 * 24 * time.Hour),
	}
	// NO WithInsightsFreshness / WithoutFreshness opts -- the
	// default must kick in.
	r := NewReader(nil, WithMetricsBackend(fb))

	resp, err := r.ReadCrossRepo(context.Background(), "cyclo", "method")
	if err != nil {
		t.Fatalf("ReadCrossRepo: %v", err)
	}
	if !resp.Degraded {
		t.Fatal("Degraded=false with no freshness opt; want auto-defaulted PercentileFreshness to flag stale")
	}
	if resp.DegradedReason != insights.DegradedReasonPercentileStale {
		t.Errorf("DegradedReason=%q, want %q", resp.DegradedReason, insights.DegradedReasonPercentileStale)
	}
}

// -----------------------------------------------------------
// Latest-dashboard: ReadPortfolio.
// -----------------------------------------------------------

func TestReader_ReadPortfolio_ReturnsEveryScopeKindForMetric(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	fb.portfolios["cyclo"] = []PortfolioRow{
		{MetricKind: "cyclo", ScopeKind: "method", RepoCount: 5, BuiltAt: now.Add(-5 * time.Minute)},
		{MetricKind: "cyclo", ScopeKind: "file", RepoCount: 5, BuiltAt: now.Add(-3 * time.Minute)},
	}
	fresh := &insights.Freshness{
		Window: time.Duration(insights.FreshnessWindowSeconds) * time.Second,
		Clock:  fixedClock{now},
	}
	r := NewReader(nil, WithMetricsBackend(fb), WithInsightsFreshness(fresh))

	resp, err := r.ReadPortfolio(context.Background(), "cyclo")
	if err != nil {
		t.Fatalf("ReadPortfolio: %v", err)
	}
	if resp.Mode != ReadModeLatestDashboard {
		t.Errorf("Mode=%q, want %q", resp.Mode, ReadModeLatestDashboard)
	}
	if len(resp.Rows) != 2 {
		t.Errorf("len(Rows)=%d, want 2", len(resp.Rows))
	}
	if resp.Degraded {
		t.Error("Degraded=true, want false for fresh portfolio")
	}
	expectedOldest := now.Add(-5 * time.Minute)
	if !resp.OldestBuiltAt.Equal(expectedOldest) {
		t.Errorf("OldestBuiltAt=%v, want %v", resp.OldestBuiltAt, expectedOldest)
	}
}

// TestReader_ReadPortfolio_StaleAnyRowStampsBanner asserts the
// portfolio's worst-case freshness verdict: a single stale row
// flips Degraded to true on the envelope even when other rows
// are fresh.
func TestReader_ReadPortfolio_StaleAnyRowStampsBanner(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	fb.portfolios["cyclo"] = []PortfolioRow{
		{MetricKind: "cyclo", ScopeKind: "method", BuiltAt: now.Add(-2 * time.Hour)}, // stale
		{MetricKind: "cyclo", ScopeKind: "file", BuiltAt: now.Add(-5 * time.Minute)}, // fresh
	}
	fresh := &insights.Freshness{
		Window: time.Duration(insights.FreshnessWindowSeconds) * time.Second,
		Clock:  fixedClock{now},
	}
	r := NewReader(nil, WithMetricsBackend(fb), WithInsightsFreshness(fresh))

	resp, err := r.ReadPortfolio(context.Background(), "cyclo")
	if err != nil {
		t.Fatalf("ReadPortfolio: %v", err)
	}
	if !resp.Degraded {
		t.Error("Degraded=false; want true (one row stale)")
	}
	if resp.DegradedReason != insights.DegradedReasonPercentileStale {
		t.Errorf("DegradedReason=%q, want %q", resp.DegradedReason, insights.DegradedReasonPercentileStale)
	}
}

func TestReader_ReadPortfolio_EmptyResultStillReturnsLatestDashboardMode(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	r := NewReader(nil, WithMetricsBackend(fb))
	resp, err := r.ReadPortfolio(context.Background(), "cyclo")
	if err != nil {
		t.Fatalf("ReadPortfolio: %v", err)
	}
	if resp.Mode != ReadModeLatestDashboard {
		t.Errorf("Mode=%q, want %q", resp.Mode, ReadModeLatestDashboard)
	}
	if resp.Rows == nil {
		t.Error("Rows is nil; want non-nil empty slice")
	}
	if len(resp.Rows) != 0 {
		t.Errorf("len(Rows)=%d, want 0", len(resp.Rows))
	}
}

// TestReader_ReadCrossRepo_RejectsMismatchedKey pins iter 2
// evaluator item 4: a backend that returned a row whose
// `metric_kind` / `scope_kind` does not match the request
// MUST surface as [ErrBackendInvariantViolation] at the Reader
// boundary, NOT be silently echoed onto the wire.
func TestReader_ReadCrossRepo_RejectsMismatchedKey(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	// Seed at (cyclo, method) but plant a row that lies about
	// its own identity (says it is cyclo/file).
	fb.crossRepo[crossRepoKey{"cyclo", "method"}] = &CrossRepoRow{
		MetricKind: "cyclo",
		ScopeKind:  "file", // mismatch -- request will be scope_kind=method
		BuiltAt:    time.Now(),
	}
	r := NewReader(nil, WithMetricsBackend(fb), WithoutFreshness())

	_, err := r.ReadCrossRepo(context.Background(), "cyclo", "method")
	if !errors.Is(err, ErrBackendInvariantViolation) {
		t.Fatalf("err=%v; want ErrBackendInvariantViolation", err)
	}
}

// TestReader_ReadCrossRepo_RejectsMismatchedMetricKind pins
// the same invariant on the metric_kind axis.
func TestReader_ReadCrossRepo_RejectsMismatchedMetricKind(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	fb.crossRepo[crossRepoKey{"cyclo", "method"}] = &CrossRepoRow{
		MetricKind: "fan_in", // mismatch
		ScopeKind:  "method",
		BuiltAt:    time.Now(),
	}
	r := NewReader(nil, WithMetricsBackend(fb), WithoutFreshness())

	_, err := r.ReadCrossRepo(context.Background(), "cyclo", "method")
	if !errors.Is(err, ErrBackendInvariantViolation) {
		t.Fatalf("err=%v; want ErrBackendInvariantViolation", err)
	}
}

// TestReader_ReadPortfolio_RejectsMismatchedMetricKindRow pins
// the iter 2 item 4 invariant on the portfolio surface: a
// backend that returns a row for a different metric_kind is
// rejected at the Reader boundary.
func TestReader_ReadPortfolio_RejectsMismatchedMetricKindRow(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	fb.portfolios["cyclo"] = []PortfolioRow{
		{MetricKind: "cyclo", ScopeKind: "method", BuiltAt: time.Now()},
		{MetricKind: "fan_in", ScopeKind: "method", BuiltAt: time.Now()}, // mismatch
	}
	r := NewReader(nil, WithMetricsBackend(fb), WithoutFreshness())

	_, err := r.ReadPortfolio(context.Background(), "cyclo")
	if !errors.Is(err, ErrBackendInvariantViolation) {
		t.Fatalf("err=%v; want ErrBackendInvariantViolation", err)
	}
}

// -----------------------------------------------------------
// Backend errors propagate verbatim.
// -----------------------------------------------------------

// TestReader_BackendErrorsPropagate asserts that a non-sentinel
// backend error (e.g. infrastructure failure) propagates
// unwrapped so the HTTP layer can map it to 500/503 without
// being shadowed by the Reader's invariant guards.
func TestReader_BackendErrorsPropagate(t *testing.T) {
	t.Parallel()
	fb := newFakeBackend()
	boom := errors.New("kaboom")
	fb.failNext = boom
	r := NewReader(nil, WithMetricsBackend(fb))

	_, err := r.ReadRepo(context.Background(), mustUUID(t))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v; want kaboom", err)
	}
}

// TestReader_ReadModeValuesAreCanonical pins the wire-side
// constants -- these strings are echoed onto the
// `mgmt.read.*` response envelopes and the HTTP gateway
// (Stage 6.4) renders them verbatim. A typo here would drift
// the wire shape, so we lock the literals at the type level.
func TestReader_ReadModeValuesAreCanonical(t *testing.T) {
	t.Parallel()
	if string(ReadModeSHAPinned) != "sha_pinned" {
		t.Errorf("ReadModeSHAPinned=%q, want \"sha_pinned\"", ReadModeSHAPinned)
	}
	if string(ReadModeLatestDashboard) != "latest_dashboard" {
		t.Errorf("ReadModeLatestDashboard=%q, want \"latest_dashboard\"", ReadModeLatestDashboard)
	}
}
