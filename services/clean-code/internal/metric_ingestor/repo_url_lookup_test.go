package metric_ingestor_test

// repo_url_lookup_test.go covers the iter-5 evaluator item
// 2 read-side seam: [metric_ingestor.RepoURLLookup] and its
// three implementations (StaticRepoURLLookup,
// SyntheticRepoURLLookup, PGRepoURLLookup).
//
// The PG case is exercised with go-sqlmock so the tests
// exercise the cache + SQL-error wrapping without standing
// up a Postgres fixture.

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
)

var repoURLLookupTestRepoID = uuid.Must(uuid.FromString("aaaaaaaa-bbbb-cccc-dddd-eeeeffff0042"))

// TestStaticRepoURLLookup_Hits pins the happy path: a
// repoID in the map returns the recorded URL.
func TestStaticRepoURLLookup_Hits(t *testing.T) {
	t.Parallel()
	want := "https://example.com/org/repo"
	l := metric_ingestor.StaticRepoURLLookup{
		URLs: map[uuid.UUID]string{repoURLLookupTestRepoID: want},
	}
	got, err := l.LookupRepoURL(context.Background(), repoURLLookupTestRepoID)
	if err != nil {
		t.Fatalf("LookupRepoURL err=%v", err)
	}
	if got != want {
		t.Errorf("got=%q, want=%q", got, want)
	}
}

// TestStaticRepoURLLookup_MissesReturnsErrRepoURLLookupNotFound
// pins the not-found surface for the static lookup.
func TestStaticRepoURLLookup_MissesReturnsErrRepoURLLookupNotFound(t *testing.T) {
	t.Parallel()
	l := metric_ingestor.StaticRepoURLLookup{URLs: nil}
	_, err := l.LookupRepoURL(context.Background(), repoURLLookupTestRepoID)
	if !errors.Is(err, metric_ingestor.ErrRepoURLLookupNotFound) {
		t.Errorf("err=%v, want wrap of ErrRepoURLLookupNotFound", err)
	}
}

// TestStaticRepoURLLookup_EmptyValueReturnsErrRepoURLLookupEmpty
// pins the empty-value surface: a row with an empty
// repo_url MUST return ErrRepoURLLookupEmpty so the
// dispatcher can fail-fast.
func TestStaticRepoURLLookup_EmptyValueReturnsErrRepoURLLookupEmpty(t *testing.T) {
	t.Parallel()
	l := metric_ingestor.StaticRepoURLLookup{
		URLs: map[uuid.UUID]string{repoURLLookupTestRepoID: "   "},
	}
	_, err := l.LookupRepoURL(context.Background(), repoURLLookupTestRepoID)
	if !errors.Is(err, metric_ingestor.ErrRepoURLLookupEmpty) {
		t.Errorf("err=%v, want wrap of ErrRepoURLLookupEmpty", err)
	}
}

// TestSyntheticRepoURLLookup_ReturnsSyntheticStamp pins the
// fall-back surface used by scaffold-mode wiring: any
// non-zero repoID gets the `clean-code-repo:<uuid>` stamp.
func TestSyntheticRepoURLLookup_ReturnsSyntheticStamp(t *testing.T) {
	t.Parallel()
	got, err := metric_ingestor.SyntheticRepoURLLookup{}.LookupRepoURL(context.Background(), repoURLLookupTestRepoID)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := metric_ingestor.SyntheticRepoURL(repoURLLookupTestRepoID)
	if got != want {
		t.Errorf("got=%q, want=%q", got, want)
	}
}

// TestSyntheticRepoURLLookup_RejectsZeroRepoID pins the
// wiring-bug guard: a zero UUID is a programmer error.
func TestSyntheticRepoURLLookup_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	_, err := metric_ingestor.SyntheticRepoURLLookup{}.LookupRepoURL(context.Background(), uuid.Nil)
	if err == nil {
		t.Error("err=nil, want non-nil for zero repoID")
	}
}

// TestPGRepoURLLookup_HitsAndCaches pins the cached path:
// a successful SELECT must populate the per-process cache,
// and a SECOND lookup for the same repoID must NOT issue
// a SELECT (the mock's ExpectationsWereMet would fail if
// it did).
func TestPGRepoURLLookup_HitsAndCaches(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	l, err := metric_ingestor.NewPGRepoURLLookupWithSchema(db, "clean_code")
	if err != nil {
		t.Fatalf("NewPGRepoURLLookupWithSchema: %v", err)
	}

	want := "https://example.com/org/repo"
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT repo_url FROM "clean_code"."repo" WHERE repo_id =`)).
		WithArgs(repoURLLookupTestRepoID).
		WillReturnRows(sqlmock.NewRows([]string{"repo_url"}).AddRow(want))

	got, err := l.LookupRepoURL(context.Background(), repoURLLookupTestRepoID)
	if err != nil {
		t.Fatalf("first lookup err=%v", err)
	}
	if got != want {
		t.Errorf("first lookup got=%q, want=%q", got, want)
	}
	// Second call MUST be served from cache -- no further
	// ExpectQuery declared.
	got2, err := l.LookupRepoURL(context.Background(), repoURLLookupTestRepoID)
	if err != nil {
		t.Fatalf("cached lookup err=%v", err)
	}
	if got2 != want {
		t.Errorf("cached lookup got=%q, want=%q", got2, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: unmet expectations: %v", err)
	}
}

// TestPGRepoURLLookup_NoRowsReturnsErrRepoURLLookupNotFound
// pins the missing-row surface: a sql.ErrNoRows result
// MUST be translated into a wrap of ErrRepoURLLookupNotFound
// so callers can use errors.Is.
func TestPGRepoURLLookup_NoRowsReturnsErrRepoURLLookupNotFound(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	l, err := metric_ingestor.NewPGRepoURLLookupWithSchema(db, "clean_code")
	if err != nil {
		t.Fatalf("NewPGRepoURLLookupWithSchema: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT repo_url FROM "clean_code"."repo" WHERE repo_id =`)).
		WithArgs(repoURLLookupTestRepoID).
		WillReturnRows(sqlmock.NewRows([]string{"repo_url"}))

	_, err = l.LookupRepoURL(context.Background(), repoURLLookupTestRepoID)
	if !errors.Is(err, metric_ingestor.ErrRepoURLLookupNotFound) {
		t.Errorf("err=%v, want wrap of ErrRepoURLLookupNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: unmet expectations: %v", err)
	}
}

// TestPGRepoURLLookup_NullRepoURLReturnsErrRepoURLLookupNotFound
// pins the back-compat surface for rows inserted BEFORE
// migration 0006_repo_url.up.sql added the column -- those
// rows have NULL `repo_url` and the lookup must fail-fast
// rather than silently emit empty canonical-signature stamps.
func TestPGRepoURLLookup_NullRepoURLReturnsErrRepoURLLookupNotFound(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	l, err := metric_ingestor.NewPGRepoURLLookupWithSchema(db, "clean_code")
	if err != nil {
		t.Fatalf("NewPGRepoURLLookupWithSchema: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT repo_url FROM "clean_code"."repo" WHERE repo_id =`)).
		WithArgs(repoURLLookupTestRepoID).
		WillReturnRows(sqlmock.NewRows([]string{"repo_url"}).AddRow(nil))

	_, err = l.LookupRepoURL(context.Background(), repoURLLookupTestRepoID)
	if !errors.Is(err, metric_ingestor.ErrRepoURLLookupNotFound) {
		t.Errorf("err=%v, want wrap of ErrRepoURLLookupNotFound (NULL repo_url)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: unmet expectations: %v", err)
	}
}

// TestPGRepoURLLookup_EmptyRepoURLReturnsErrRepoURLLookupEmpty
// pins the "row present but empty value" surface.
func TestPGRepoURLLookup_EmptyRepoURLReturnsErrRepoURLLookupEmpty(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	l, err := metric_ingestor.NewPGRepoURLLookupWithSchema(db, "clean_code")
	if err != nil {
		t.Fatalf("NewPGRepoURLLookupWithSchema: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT repo_url FROM "clean_code"."repo" WHERE repo_id =`)).
		WithArgs(repoURLLookupTestRepoID).
		WillReturnRows(sqlmock.NewRows([]string{"repo_url"}).AddRow("   "))

	_, err = l.LookupRepoURL(context.Background(), repoURLLookupTestRepoID)
	if !errors.Is(err, metric_ingestor.ErrRepoURLLookupEmpty) {
		t.Errorf("err=%v, want wrap of ErrRepoURLLookupEmpty", err)
	}
}

// TestNewPGRepoURLLookup_RejectsNilDB pins the
// composition-root error surface.
func TestNewPGRepoURLLookup_RejectsNilDB(t *testing.T) {
	t.Parallel()
	if _, err := metric_ingestor.NewPGRepoURLLookup(nil); err == nil {
		t.Error("NewPGRepoURLLookup(nil) returned nil err, want non-nil")
	}
}
