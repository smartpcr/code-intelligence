package aggregator_test

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
)

const pgSourceTestSchema = "clean_code_aggregator_test"

func newSQLMockSampleSource(t *testing.T) (*aggregator.PGSampleSource, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	s, err := aggregator.NewPGSampleSourceWithSchema(db, pgSourceTestSchema)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPGSampleSourceWithSchema: %v", err)
	}
	return s, mock, func() { _ = db.Close() }
}

// TestNewPGSampleSource_RejectsNilDB pins the wiring guard.
func TestNewPGSampleSource_RejectsNilDB(t *testing.T) {
	t.Parallel()
	if _, err := aggregator.NewPGSampleSource(nil); !errors.Is(err, aggregator.ErrPGSampleSourceNilDB) {
		t.Errorf("NewPGSampleSource(nil) err = %v, want ErrPGSampleSourceNilDB", err)
	}
}

// TestNewPGSampleSourceWithSchema_RejectsEmptySchema pins the
// guard against blank schema names.
func TestNewPGSampleSourceWithSchema_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	db, _, _ := sqlmock.New()
	defer db.Close()
	if _, err := aggregator.NewPGSampleSourceWithSchema(db, ""); !errors.Is(err, aggregator.ErrPGSampleSourceEmptySchema) {
		t.Errorf("NewPGSampleSourceWithSchema(db, \"\") err = %v, want ErrPGSampleSourceEmptySchema", err)
	}
	if _, err := aggregator.NewPGSampleSourceWithSchema(db, "   "); !errors.Is(err, aggregator.ErrPGSampleSourceEmptySchema) {
		t.Errorf("NewPGSampleSourceWithSchema(db, \"   \") err = %v, want ErrPGSampleSourceEmptySchema", err)
	}
}

// TestPGSampleSource_ReadActive_FiresExpectedJoin is the SQL
// trace pin. The aggregator MUST read through:
//
//	metric_sample_active msa
//	  JOIN metric_sample ms ON ms.sample_id = msa.sample_id
//	  JOIN scope_binding  sb ON sb.scope_id = ms.scope_id
//	  LEFT JOIN metric_retraction mr ON mr.sample_id = msa.sample_id
//	 WHERE mr.sample_id IS NULL
//	   AND ms.value IS NOT NULL
//
// because:
//   - msa is the canonical active-sample side-relation (tech-spec
//     Sec 7.1.b active-table pattern)
//   - scope_binding is the only carrier of `scope_kind` (metric_sample
//     stores only `scope_id`)
//   - the LEFT JOIN + IS NULL anti-pattern filters retracted samples
//   - the IS NOT NULL guard rejects degraded NULL values
//
// Any rewrite that drops one of those JOINs / predicates would
// silently change the active-row read shape and produce
// incorrect snapshots.
func TestPGSampleSource_ReadActive_FiresExpectedJoin(t *testing.T) {
	t.Parallel()
	s, mock, closeFn := newSQLMockSampleSource(t)
	defer closeFn()

	// Pin every required identifier + predicate. The regex is
	// permissive on whitespace so a future formatter doesn't break
	// the test, but it requires the JOINs / WHERE in order.
	queryPattern := `SELECT\s+ms\.repo_id,\s*ms\.metric_kind,\s*sb\.scope_kind::text,\s*ms\.value\s+` +
		`FROM\s+"` + pgSourceTestSchema + `"\."metric_sample_active"\s+msa\s+` +
		`JOIN\s+"` + pgSourceTestSchema + `"\."metric_sample"\s+ms\s+ON\s+ms\.sample_id\s*=\s*msa\.sample_id\s+` +
		`JOIN\s+"` + pgSourceTestSchema + `"\."scope_binding"\s+sb\s+ON\s+sb\.scope_id\s*=\s*ms\.scope_id\s+` +
		`LEFT\s+JOIN\s+"` + pgSourceTestSchema + `"\."metric_retraction"\s+mr\s+ON\s+mr\.sample_id\s*=\s*msa\.sample_id\s+` +
		`WHERE\s+mr\.sample_id\s+IS\s+NULL\s+` +
		`AND\s+ms\.value\s+IS\s+NOT\s+NULL`

	mock.ExpectQuery(queryPattern).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "metric_kind", "scope_kind", "value"}).
			AddRow("11111111-1111-1111-1111-111111111111", "cyclo", "method", 3.0).
			AddRow("22222222-2222-2222-2222-222222222222", "cyclo", "method", 5.0))

	obs, err := s.ReadActive(context.Background())
	if err != nil {
		t.Fatalf("ReadActive: %v", err)
	}
	if got := len(obs); got != 2 {
		t.Fatalf("len(obs) = %d, want 2", got)
	}
	if obs[0].MetricKind != "cyclo" || obs[0].ScopeKind != "method" || obs[0].Value != 3.0 {
		t.Errorf("obs[0] = %+v, want {metric_kind=cyclo, scope_kind=method, value=3.0}", obs[0])
	}
	if obs[1].Value != 5.0 {
		t.Errorf("obs[1].Value = %v, want 5.0", obs[1].Value)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestPGSampleSource_ReadActive_QualifiesSchema covers the
// schema-qualification path. A test schema like
// `clean_code_aggregator_test` must show up in the rendered SQL
// without the writer ever falling back to the default
// `clean_code` schema.
func TestPGSampleSource_ReadActive_QualifiesSchema(t *testing.T) {
	t.Parallel()
	s, mock, closeFn := newSQLMockSampleSource(t)
	defer closeFn()
	mock.ExpectQuery(regexp.QuoteMeta(`"` + pgSourceTestSchema + `"."metric_sample_active"`)).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "metric_kind", "scope_kind", "value"}))
	if _, err := s.ReadActive(context.Background()); err != nil {
		t.Fatalf("ReadActive: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestPGSampleSource_ReadActive_PropagatesQueryError surfaces
// the driver failure so the aggregator's loop sees it.
func TestPGSampleSource_ReadActive_PropagatesQueryError(t *testing.T) {
	t.Parallel()
	s, mock, closeFn := newSQLMockSampleSource(t)
	defer closeFn()
	sentinel := errors.New("simulated PG error")
	mock.ExpectQuery(`SELECT`).WillReturnError(sentinel)
	if _, err := s.ReadActive(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("ReadActive err = %v, want errors.Is(err, sentinel)", err)
	}
}

// TestPGSampleSource_ReadActive_HonoursContextCancel ensures a
// pre-cancelled ctx aborts before the SQL fires -- the loop's
// SIGTERM path relies on this.
func TestPGSampleSource_ReadActive_HonoursContextCancel(t *testing.T) {
	t.Parallel()
	s, _, closeFn := newSQLMockSampleSource(t)
	defer closeFn()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ReadActive(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadActive(cancelled ctx) err = %v, want context.Canceled", err)
	}
}
