package partitionmaintainer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Unit tests driven through go-sqlmock. These cover the
// validation surface (New rejects blank / unqualified entries),
// the default-application logic, the maintenance-call wire
// shape (cluster-wide vs scoped), the scrape-call wire shape,
// the gauge / counter increments around success and failure,
// and the empty-scope path.
//
// Integration tests in service_integration_test.go cover the
// pg_partman wire path against a real cluster (skipping when
// AGENT_MEMORY_PG_URL is unset).

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNew_rejectsBlankParentTableEntry(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		_ = mock.ExpectationsWereMet()
		_ = db.Close()
	}()

	_, err = New(db, Config{
		ParentTables: []string{"public.episode", "   "},
	}, discardLogger())
	if err == nil {
		t.Fatal("New with blank ParentTables entry: expected error, got nil")
	}
}

func TestNew_rejectsUnqualifiedParentTableEntry(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		_ = mock.ExpectationsWereMet()
		_ = db.Close()
	}()

	_, err = New(db, Config{
		ParentTables: []string{"episode"},
	}, discardLogger())
	if err == nil {
		t.Fatal("New with unqualified ParentTables entry: expected error, got nil")
	}
}

func TestNew_appliesDefaults(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		_ = mock.ExpectationsWereMet()
		_ = db.Close()
	}()

	svc, err := New(db, Config{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := svc.Config()
	if got.MaintenanceInterval != DefaultMaintenanceInterval {
		t.Errorf("MaintenanceInterval = %v, want %v", got.MaintenanceInterval, DefaultMaintenanceInterval)
	}
	if got.MaintenanceTimeout != DefaultMaintenanceTimeout {
		t.Errorf("MaintenanceTimeout = %v, want %v", got.MaintenanceTimeout, DefaultMaintenanceTimeout)
	}
	if got.LagScrapeInterval != DefaultLagScrapeInterval {
		t.Errorf("LagScrapeInterval = %v, want %v", got.LagScrapeInterval, DefaultLagScrapeInterval)
	}
	if got.LagScrapeTimeout != DefaultLagScrapeTimeout {
		t.Errorf("LagScrapeTimeout = %v, want %v", got.LagScrapeTimeout, DefaultLagScrapeTimeout)
	}
	if len(got.ParentTables) != 0 {
		t.Errorf("ParentTables = %v, want empty (cluster-wide default)", got.ParentTables)
	}
}

// TestConfigReturnsIndependentParentTablesSlice guards against
// the slice-aliasing trap: a caller mutating Config().ParentTables
// MUST NOT reach into the Service's runtime state.
func TestConfigReturnsIndependentParentTablesSlice(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		_ = mock.ExpectationsWereMet()
		_ = db.Close()
	}()

	svc, err := New(db, Config{
		ParentTables: []string{"public.episode"},
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg1 := svc.Config()
	cfg1.ParentTables[0] = "evil.tenant"
	cfg2 := svc.Config()
	if cfg2.ParentTables[0] != "public.episode" {
		t.Errorf("after mutating returned ParentTables, Service still reports %v; want unchanged",
			cfg2.ParentTables)
	}
}

// TestRunMaintenance_clusterWidePathIssuesUnscopedCall asserts
// the cluster-wide shape: a Service with no ParentTables AND no
// SchemaFilter MUST issue a single `run_maintenance(p_analyze :=
// false)` call with no parent argument so partman walks every
// part_config row.
func TestRunMaintenance_clusterWidePathIssuesUnscopedCall(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
		_ = db.Close()
	}()

	svc, err := New(db, Config{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectExec(`partman\.run_maintenance\(p_analyze := false\)`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	res, err := svc.RunMaintenance(context.Background())
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	// Cluster-wide path does not iterate; ParentsMaintained
	// stays at 0 by contract.
	if res.ParentsMaintained != 0 {
		t.Errorf("ParentsMaintained = %d, want 0 (cluster-wide path does not iterate)",
			res.ParentsMaintained)
	}
	snap := svc.Metrics().Snapshot()
	if snap[MetricPartitionMaintenanceRunsTotal] != 1 {
		t.Errorf("%s = %d, want 1",
			MetricPartitionMaintenanceRunsTotal,
			snap[MetricPartitionMaintenanceRunsTotal])
	}
	if snap[MetricPartitionMaintenanceErrorsTotal] != 0 {
		t.Errorf("%s = %d, want 0",
			MetricPartitionMaintenanceErrorsTotal,
			snap[MetricPartitionMaintenanceErrorsTotal])
	}
}

// TestRunMaintenance_scopedPathIssuesPerParentCalls asserts the
// per-parent shape: a Service with explicit ParentTables MUST
// issue one scoped run_maintenance call per parent.
func TestRunMaintenance_scopedPathIssuesPerParentCalls(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
		_ = db.Close()
	}()

	svc, err := New(db, Config{
		ParentTables: []string{"amtest.episode", "amtest.observation"},
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectExec(`partman\.run_maintenance`).
		WithArgs("amtest.episode").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`partman\.run_maintenance`).
		WithArgs("amtest.observation").
		WillReturnResult(sqlmock.NewResult(0, 0))

	res, err := svc.RunMaintenance(context.Background())
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	if res.ParentsMaintained != 2 {
		t.Errorf("ParentsMaintained = %d, want 2", res.ParentsMaintained)
	}
}

// TestRunMaintenance_errorIncrementsErrorCounter validates the
// failure path: a SQL error MUST bump the error counter and
// surface as a wrapped error to the caller. Run() relies on the
// caller's errors.Is(err, context.Canceled) check so the wrap
// MUST preserve context.Canceled when the underlying driver
// surfaces it.
func TestRunMaintenance_errorIncrementsErrorCounter(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		_ = mock.ExpectationsWereMet()
		_ = db.Close()
	}()

	svc, err := New(db, Config{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sentinel := errors.New("simulated partman failure")
	mock.ExpectExec(`partman\.run_maintenance`).
		WillReturnError(sentinel)

	_, err = svc.RunMaintenance(context.Background())
	if err == nil {
		t.Fatal("RunMaintenance: expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("RunMaintenance error = %v, want wrap of %v", err, sentinel)
	}
	snap := svc.Metrics().Snapshot()
	if snap[MetricPartitionMaintenanceErrorsTotal] != 1 {
		t.Errorf("%s = %d, want 1",
			MetricPartitionMaintenanceErrorsTotal,
			snap[MetricPartitionMaintenanceErrorsTotal])
	}
}

// TestScrapeLag_explicitParentsHappyPathSetsGauge exercises the
// happy-path scrape for two parents. The per-parent lag SQL is
// stubbed; we assert the MAX is taken correctly and the gauge
// is updated.
func TestScrapeLag_explicitParentsHappyPathSetsGauge(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
		_ = db.Close()
	}()

	svc, err := New(db, Config{
		ParentTables: []string{"amtest.episode", "amtest.observation"},
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// episode lag = 0 (healthy).
	mock.ExpectQuery(`pg_inherits`).
		WithArgs("amtest.episode").
		WillReturnRows(sqlmock.NewRows([]string{"lag"}).AddRow(int64(0)))
	// observation lag = 90 000 s (>1 day, alert-firing).
	mock.ExpectQuery(`pg_inherits`).
		WithArgs("amtest.observation").
		WillReturnRows(sqlmock.NewRows([]string{"lag"}).AddRow(int64(90000)))

	res, err := svc.ScrapeLag(context.Background())
	if err != nil {
		t.Fatalf("ScrapeLag: %v", err)
	}
	if res.MaxLagSeconds != 90000 {
		t.Errorf("MaxLagSeconds = %d, want 90000", res.MaxLagSeconds)
	}
	if len(res.ParentLags) != 2 {
		t.Fatalf("ParentLags = %d entries, want 2", len(res.ParentLags))
	}
	snap := svc.Metrics().Snapshot()
	if snap[MetricPartitionProvisionLagSeconds] != 90000 {
		t.Errorf("%s gauge = %d, want 90000",
			MetricPartitionProvisionLagSeconds,
			snap[MetricPartitionProvisionLagSeconds])
	}
	if snap[MetricPartitionParentsObservedGauge] != 2 {
		t.Errorf("%s gauge = %d, want 2",
			MetricPartitionParentsObservedGauge,
			snap[MetricPartitionParentsObservedGauge])
	}
	if snap[MetricPartitionLagScrapesTotal] != 1 {
		t.Errorf("%s = %d, want 1",
			MetricPartitionLagScrapesTotal,
			snap[MetricPartitionLagScrapesTotal])
	}
	if snap[MetricPartitionLagScrapeErrorsTotal] != 0 {
		t.Errorf("%s = %d, want 0",
			MetricPartitionLagScrapeErrorsTotal,
			snap[MetricPartitionLagScrapeErrorsTotal])
	}
}

// TestScrapeLag_emptyExplicitScopeResetsGauge ensures an
// operator narrowing the scope to an empty set zeroes the gauge
// rather than leaving a stale value lingering on the /metrics
// endpoint.
//
// Construction tricks past the New-time blank-parent rejection
// by setting the gauge through a successful scrape first, then
// stubbing the cluster-wide path with zero parents.
func TestScrapeLag_emptyScopeResetsGauge(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
		_ = db.Close()
	}()

	svc, err := New(db, Config{
		SchemaFilter: "no_such_schema",
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Pre-seed the gauge so we can prove the reset.
	svc.Metrics().SetProvisionLagSeconds(42)

	mock.ExpectQuery(`partman\.part_config`).
		WithArgs("no_such_schema").
		WillReturnRows(sqlmock.NewRows([]string{"parent_table"}))

	res, err := svc.ScrapeLag(context.Background())
	if err != nil {
		t.Fatalf("ScrapeLag: %v", err)
	}
	if res.MaxLagSeconds != 0 {
		t.Errorf("MaxLagSeconds = %d, want 0", res.MaxLagSeconds)
	}
	if got := svc.Metrics().ProvisionLagSeconds(); got != 0 {
		t.Errorf("gauge = %d, want 0 (reset on empty scope)", got)
	}
}

// TestScrapeLag_perParentErrorIncrementsErrorCounter validates
// the failure path of the per-parent lag query.
func TestScrapeLag_perParentErrorIncrementsErrorCounter(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		_ = mock.ExpectationsWereMet()
		_ = db.Close()
	}()

	svc, err := New(db, Config{
		ParentTables: []string{"amtest.episode"},
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sentinel := errors.New("simulated lag SQL failure")
	mock.ExpectQuery(`pg_inherits`).
		WithArgs("amtest.episode").
		WillReturnError(sentinel)

	_, err = svc.ScrapeLag(context.Background())
	if err == nil {
		t.Fatal("ScrapeLag: expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("ScrapeLag error = %v, want wrap of %v", err, sentinel)
	}
	if got := svc.Metrics().LagScrapeErrorsTotal(); got != 1 {
		t.Errorf("%s = %d, want 1",
			MetricPartitionLagScrapeErrorsTotal, got)
	}
}

// TestRun_initialPassesAndShutdownOnCtxCancel validates the
// Run() loop: one maintenance + one scrape land immediately on
// invocation, then Run blocks on the two tickers until ctx is
// cancelled. We pick generous intervals so neither ticker fires
// during the test; the initial passes are the only SQL calls
// expected.
func TestRun_initialPassesAndShutdownOnCtxCancel(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
		_ = db.Close()
	}()

	svc, err := New(db, Config{
		ParentTables:        []string{"amtest.episode"},
		MaintenanceInterval: time.Hour, // never fires
		LagScrapeInterval:   time.Hour, // never fires
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectExec(`partman\.run_maintenance`).
		WithArgs("amtest.episode").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`pg_inherits`).
		WithArgs("amtest.episode").
		WillReturnRows(sqlmock.NewRows([]string{"lag"}).AddRow(int64(0)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	// Wait for both initial passes to count.
	deadline := time.After(2 * time.Second)
	for {
		if svc.Metrics().MaintenanceRunsTotal() >= 1 &&
			svc.Metrics().LagScrapesTotal() >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("initial passes did not complete within 2s; maintenance=%d scrapes=%d",
				svc.Metrics().MaintenanceRunsTotal(),
				svc.Metrics().LagScrapesTotal())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case runErr := <-done:
		if !errors.Is(runErr, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancel")
	}
}

// TestScrapeLag_clampsNegativeLagToZero exercises the
// defence-in-depth path where the SQL returns a negative value
// (which the SQL should never do thanks to GREATEST(0, ...),
// but a future regression there must not pollute the gauge
// with a wrap-around uint64).
func TestScrapeLag_clampsNegativeLagToZero(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		_ = mock.ExpectationsWereMet()
		_ = db.Close()
	}()

	svc, err := New(db, Config{
		ParentTables: []string{"amtest.episode"},
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mock.ExpectQuery(`pg_inherits`).
		WithArgs("amtest.episode").
		WillReturnRows(sqlmock.NewRows([]string{"lag"}).AddRow(int64(-99)))

	res, err := svc.ScrapeLag(context.Background())
	if err != nil {
		t.Fatalf("ScrapeLag: %v", err)
	}
	if res.MaxLagSeconds != 0 {
		t.Errorf("MaxLagSeconds = %d, want 0 (negative input must clamp)", res.MaxLagSeconds)
	}
	if got := svc.Metrics().ProvisionLagSeconds(); got != 0 {
		t.Errorf("gauge = %d, want 0 (negative input must clamp)", got)
	}
}
