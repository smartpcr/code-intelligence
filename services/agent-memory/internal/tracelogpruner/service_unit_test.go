package tracelogpruner

// Unit tests driven through go-sqlmock. These cover the
// validation surface (New rejects unqualified parent table,
// rejects blank parent), the metric increments around a
// successful Prune, the single-int return-value scanning, and
// the error-path metric increments without requiring a live
// PostgreSQL.
//
// pg_partman v5's `drop_partition_time` is declared
// `RETURNS int` (count of partitions detached/dropped), so
// every mock here returns exactly ONE row with ONE integer
// column. A multi-row mock would not match the production
// wire contract and would mask the metric-increment bug
// caught by the iter-1 evaluator review.
//
// Integration tests in service_integration_test.go cover the
// pg_partman wire path against a real cluster (skipping when
// AGENT_MEMORY_PG_URL is unset).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNew_rejectsBlankParentTable(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		_ = mock.ExpectationsWereMet()
		_ = db.Close()
	}()

	_, err = New(db, Config{ParentTable: "   "}, discardLogger())
	if err == nil {
		t.Fatal("New with blank ParentTable: expected error, got nil")
	}
}

func TestNew_rejectsUnqualifiedParentTable(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		_ = mock.ExpectationsWereMet()
		_ = db.Close()
	}()

	_, err = New(db, Config{ParentTable: "trace_observation_log"}, discardLogger())
	if err == nil {
		t.Fatal("New with unqualified ParentTable: expected error, got nil")
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

	svc, err := New(db, Config{ParentTable: "public.trace_observation_log"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := svc.Config()
	if got.Retention != DefaultRetention {
		t.Errorf("Retention = %v, want %v", got.Retention, DefaultRetention)
	}
	if got.RunInterval != DefaultRunInterval {
		t.Errorf("RunInterval = %v, want %v", got.RunInterval, DefaultRunInterval)
	}
	if got.PruneTimeout != DefaultPruneTimeout {
		t.Errorf("PruneTimeout = %v, want %v", got.PruneTimeout, DefaultPruneTimeout)
	}
	// KeepTable default must be true (detach only — the §8.1
	// safe default). An omitted KeepTable (nil *bool) MUST
	// resolve to true so a `Config{ParentTable: ...}` caller
	// does not silently DROP partitions.
	if got.KeepTable == nil {
		t.Fatal("Config().KeepTable is nil; want *bool with default true")
	}
	if !*got.KeepTable {
		t.Errorf("Config().KeepTable = false, want true (default for omitted)")
	}
}

// TestNew_keepTableExplicitFalsePreserved ensures an operator
// who explicitly opts INTO the destructive DROP-after-detach
// path gets that exact behavior — the *bool indirection must
// not silently coerce false→true on its way through New.
func TestNew_keepTableExplicitFalsePreserved(t *testing.T) {
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
		ParentTable: "public.trace_observation_log",
		KeepTable:   boolPtr(false),
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := svc.Config()
	if got.KeepTable == nil || *got.KeepTable {
		t.Errorf("Config().KeepTable = %v, want explicit false", got.KeepTable)
	}
}

// TestConfigReturnsIsolatedKeepTablePointer guards against the
// pointer-aliasing trap: a caller mutating *Config().KeepTable
// MUST NOT reach into the Service's runtime state.
func TestConfigReturnsIsolatedKeepTablePointer(t *testing.T) {
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
		ParentTable: "public.trace_observation_log",
		KeepTable:   boolPtr(true),
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg1 := svc.Config()
	cfg2 := svc.Config()
	if cfg1.KeepTable == cfg2.KeepTable {
		t.Errorf("Config() returned the same *bool pointer twice; want a fresh allocation per call to prevent aliasing")
	}
	*cfg1.KeepTable = false
	cfg3 := svc.Config()
	if cfg3.KeepTable == nil || !*cfg3.KeepTable {
		t.Errorf("after mutating returned *KeepTable, Service still reports KeepTable = %v; want true (mutation must not bleed in)", cfg3.KeepTable)
	}
}

// TestPrune_returnedCountSetsResultAndIncrementsCounter is the
// happy-path scenario: drop_partition_time returns the integer
// 2 (two partitions detached). PartitionsDropped == 2 and the
// trace_log_partitions_dropped_total counter increases by 2.
func TestPrune_returnedCountSetsResultAndIncrementsCounter(t *testing.T) {
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
		ParentTable:  "amtest.trace_observation_log",
		Retention:    30 * 24 * time.Hour,
		KeepTable:    boolPtr(true),
		RunInterval:  24 * time.Hour,
		PruneTimeout: 30 * time.Second,
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectQuery(`partman\.drop_partition_time`).
		WithArgs("amtest.trace_observation_log", "2592000.000000 seconds", true).
		WillReturnRows(sqlmock.NewRows([]string{"drop_partition_time"}).AddRow(2))

	res, err := svc.Prune(context.Background())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.PartitionsDropped != 2 {
		t.Errorf("PartitionsDropped = %d, want 2", res.PartitionsDropped)
	}
	snap := svc.Metrics().Snapshot()
	if snap[MetricTraceLogPartitionsDroppedTotal] != 2 {
		t.Errorf("%s = %d, want 2",
			MetricTraceLogPartitionsDroppedTotal,
			snap[MetricTraceLogPartitionsDroppedTotal])
	}
	if snap[MetricTraceLogPruneRunsTotal] != 1 {
		t.Errorf("%s = %d, want 1",
			MetricTraceLogPruneRunsTotal,
			snap[MetricTraceLogPruneRunsTotal])
	}
	if snap[MetricTraceLogPruneErrorsTotal] != 0 {
		t.Errorf("%s = %d, want 0",
			MetricTraceLogPruneErrorsTotal,
			snap[MetricTraceLogPruneErrorsTotal])
	}
}

// TestPrune_zeroReturnedCountDoesNotIncrementCounter is the
// no-op scenario: pg_partman returns 0 (no partition has yet
// fallen out of the retention window). The
// trace_log_partitions_dropped_total counter MUST NOT increment
// (so the metric stays a faithful "total partitions ever
// detached by this binary") while the runs counter increments
// to 1 (a Prune attempt is still a Prune attempt).
//
// This was the iter-1 evaluator-flagged bug: the old
// implementation counted ROWS returned (always 1 for an int
// return) so a no-op day showed a +1 on the dropped counter
// and a multi-partition day also showed +1.
func TestPrune_zeroReturnedCountDoesNotIncrementCounter(t *testing.T) {
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
		ParentTable: "public.trace_observation_log",
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectQuery(`partman\.drop_partition_time`).
		WillReturnRows(sqlmock.NewRows([]string{"drop_partition_time"}).AddRow(0))

	res, err := svc.Prune(context.Background())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.PartitionsDropped != 0 {
		t.Errorf("PartitionsDropped = %d, want 0", res.PartitionsDropped)
	}
	snap := svc.Metrics().Snapshot()
	if snap[MetricTraceLogPartitionsDroppedTotal] != 0 {
		t.Errorf("%s = %d, want 0 (no-op must NOT increment)",
			MetricTraceLogPartitionsDroppedTotal,
			snap[MetricTraceLogPartitionsDroppedTotal])
	}
	if snap[MetricTraceLogPruneRunsTotal] != 1 {
		t.Errorf("%s = %d, want 1",
			MetricTraceLogPruneRunsTotal,
			snap[MetricTraceLogPruneRunsTotal])
	}
}

// TestPrune_multiPartitionCountIncrementsByN exercises the
// catch-up case (binary restarted after a long outage, three
// partitions are now outside the retention window). The
// counter must increment by exactly N (3), not by 1 (the row
// count of the single-row int return).
func TestPrune_multiPartitionCountIncrementsByN(t *testing.T) {
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
		ParentTable: "public.trace_observation_log",
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectQuery(`partman\.drop_partition_time`).
		WillReturnRows(sqlmock.NewRows([]string{"drop_partition_time"}).AddRow(3))

	res, err := svc.Prune(context.Background())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.PartitionsDropped != 3 {
		t.Errorf("PartitionsDropped = %d, want 3", res.PartitionsDropped)
	}
	if got := svc.Metrics().PartitionsDroppedTotal(); got != 3 {
		t.Errorf("%s counter = %d, want 3 (multi-partition catch-up must increment by N, not 1)",
			MetricTraceLogPartitionsDroppedTotal, got)
	}
}

// TestPrune_queryErrorIncrementsErrorCounter ensures the
// failure path bumps the error counter and surfaces the wrapped
// error.
func TestPrune_queryErrorIncrementsErrorCounter(t *testing.T) {
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
		ParentTable: "public.trace_observation_log",
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sentinel := errors.New("simulated partman failure")
	mock.ExpectQuery(`partman\.drop_partition_time`).
		WillReturnError(sentinel)

	_, err = svc.Prune(context.Background())
	if err == nil {
		t.Fatal("Prune: expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Prune error = %v, want wrap of %v", err, sentinel)
	}
	snap := svc.Metrics().Snapshot()
	if snap[MetricTraceLogPruneErrorsTotal] != 1 {
		t.Errorf("%s = %d, want 1",
			MetricTraceLogPruneErrorsTotal,
			snap[MetricTraceLogPruneErrorsTotal])
	}
	if snap[MetricTraceLogPartitionsDroppedTotal] != 0 {
		t.Errorf("%s = %d, want 0 (no partitions on failure)",
			MetricTraceLogPartitionsDroppedTotal,
			snap[MetricTraceLogPartitionsDroppedTotal])
	}
}

// TestRun_initialPruneAndShutdownOnCtxCancel validates the
// daily-cron loop: one Prune call lands immediately on Run
// invocation, then Run blocks on the ticker until ctx is
// cancelled. We pick a generous RunInterval so the ticker
// never fires during the test; the initial sweep is the only
// SQL call expected.
func TestRun_initialPruneAndShutdownOnCtxCancel(t *testing.T) {
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
		ParentTable: "public.trace_observation_log",
		RunInterval: time.Hour, // never fires during this test
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectQuery(`partman\.drop_partition_time`).
		WillReturnRows(sqlmock.NewRows([]string{"drop_partition_time"}).AddRow(1))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	// Give the initial sweep a moment, then cancel and wait.
	deadline := time.After(2 * time.Second)
	for {
		if svc.Metrics().RunsTotal() >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("initial Prune did not complete within 2s")
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

	snap := svc.Metrics().Snapshot()
	if snap[MetricTraceLogPruneRunsTotal] != 1 {
		t.Errorf("%s = %d, want 1 (initial sweep only)",
			MetricTraceLogPruneRunsTotal,
			snap[MetricTraceLogPruneRunsTotal])
	}
	if snap[MetricTraceLogPartitionsDroppedTotal] != 1 {
		t.Errorf("%s = %d, want 1",
			MetricTraceLogPartitionsDroppedTotal,
			snap[MetricTraceLogPartitionsDroppedTotal])
	}
}

// TestPrune_keepTableFalseFlowsThroughBinding asserts the
// boolean parameter binding picks up Config.KeepTable=false so
// an explicit operator config that DROPs the partition (instead
// of detaching) is faithful at the wire layer.
func TestPrune_keepTableFalseFlowsThroughBinding(t *testing.T) {
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
		ParentTable: "public.trace_observation_log",
		KeepTable:   boolPtr(false),
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectQuery(`partman\.drop_partition_time`).
		WithArgs("public.trace_observation_log", "2592000.000000 seconds", false).
		WillReturnRows(sqlmock.NewRows([]string{"drop_partition_time"}).AddRow(0))

	if _, err := svc.Prune(context.Background()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
}

// TestPrune_omittedKeepTableBindsTrue asserts the safe default
// path: a `Config{ParentTable: ...}` caller (with no KeepTable
// set) MUST result in `p_keep_table := true` at the wire — NOT
// the Go zero value `false` that would silently DROP partitions.
// This is the regression-guard for evaluator finding #5.
func TestPrune_omittedKeepTableBindsTrue(t *testing.T) {
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

	// Note: NO KeepTable in this Config literal — exercises
	// the default-application code path inside New.
	svc, err := New(db, Config{
		ParentTable: "public.trace_observation_log",
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectQuery(`partman\.drop_partition_time`).
		WithArgs("public.trace_observation_log", "2592000.000000 seconds", true).
		WillReturnRows(sqlmock.NewRows([]string{"drop_partition_time"}).AddRow(0))

	if _, err := svc.Prune(context.Background()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
}
