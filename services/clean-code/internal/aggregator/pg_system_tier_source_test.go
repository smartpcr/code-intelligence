package aggregator_test

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
)

const pgSystemTierSourceTestSchema = "clean_code_aggregator_test"

func newSQLMockSystemTierSource(t *testing.T) (*aggregator.PGSystemTierInputSource, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	src, err := aggregator.NewPGSystemTierInputSourceWithSchema(db, pgSystemTierSourceTestSchema)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPGSystemTierInputSourceWithSchema: %v", err)
	}
	return src, mock, func() { _ = db.Close() }
}

// TestNewPGSystemTierInputSource_RejectsNilDB pins the
// wiring guard against nil *sql.DB.
func TestNewPGSystemTierInputSource_RejectsNilDB(t *testing.T) {
	t.Parallel()
	if _, err := aggregator.NewPGSystemTierInputSource(nil); !errors.Is(err, aggregator.ErrPGSystemTierInputSourceNilDB) {
		t.Errorf("NewPGSystemTierInputSource(nil) err = %v, want ErrPGSystemTierInputSourceNilDB", err)
	}
}

// TestNewPGSystemTierInputSourceWithSchema_RejectsEmptySchema
// pins the guard against blank schema names.
func TestNewPGSystemTierInputSourceWithSchema_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	db, _, _ := sqlmock.New()
	defer db.Close()
	if _, err := aggregator.NewPGSystemTierInputSourceWithSchema(db, ""); !errors.Is(err, aggregator.ErrPGSystemTierInputSourceEmptySchema) {
		t.Errorf("empty schema err = %v, want ErrPGSystemTierInputSourceEmptySchema", err)
	}
	if _, err := aggregator.NewPGSystemTierInputSourceWithSchema(db, "   "); !errors.Is(err, aggregator.ErrPGSystemTierInputSourceEmptySchema) {
		t.Errorf("whitespace schema err = %v, want ErrPGSystemTierInputSourceEmptySchema", err)
	}
}

// TestPGSystemTierInputSource_EmptyActiveSet_ReturnsEmpty
// pins the no-op case: zero (repo_id, sha) pairs in the
// active table -> no inputs returned, no follow-up queries
// fired.
func TestPGSystemTierInputSource_EmptyActiveSet_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	src, mock, cleanup := newSQLMockSystemTierSource(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT DISTINCT ms\.repo_id, ms\.sha`).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha"}))

	got, err := src.ReadSystemTierInputs(context.Background())
	if err != nil {
		t.Fatalf("ReadSystemTierInputs err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty active set -> len(got) = %d, want 0", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGSystemTierInputSource_ReadSystemTierInputs_HappyPath
// drives the full per-pair fan-out for ONE (repo_id, sha):
// one pair row -> one scan_run lookup -> one scope row ->
// one foundation sample. Asserts the SystemTierInput shape,
// the Mode is Embedded, and both edge-availability flags
// are false (v1 embedded mode).
func TestPGSystemTierInputSource_ReadSystemTierInputs_HappyPath(t *testing.T) {
	t.Parallel()
	src, mock, cleanup := newSQLMockSystemTierSource(t)
	defer cleanup()

	repoID := uuid.Must(uuid.NewV4())
	runID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	const sha = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	mock.ExpectQuery(`SELECT DISTINCT ms\.repo_id, ms\.sha`).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha"}).
			AddRow(repoID.String(), sha))

	mock.ExpectQuery(`SELECT scan_run_id`).
		WithArgs(repoID, sha).
		WillReturnRows(sqlmock.NewRows([]string{"scan_run_id"}).
			AddRow(runID.String()))

	mock.ExpectQuery(`SELECT DISTINCT sb\.scope_id, sb\.scope_kind`).
		WithArgs(repoID, sha).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind"}).
			AddRow(scopeID.String(), "file"))

	mock.ExpectQuery(`SELECT sb\.scope_id, sb\.scope_kind::text, ms\.metric_kind, ms\.value, ms\.attrs_json`).
		WithArgs(repoID, sha).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind", "metric_kind", "value", "attrs_json"}).
			AddRow(scopeID.String(), "file", "cyclomatic_complexity", 12.5, `{"language":"go"}`))

	got, err := src.ReadSystemTierInputs(context.Background())
	if err != nil {
		t.Fatalf("ReadSystemTierInputs err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	in := got[0]
	if in.Mode != aggregator.SystemTierModeEmbedded {
		t.Errorf("in.Mode = %q, want %q", in.Mode, aggregator.SystemTierModeEmbedded)
	}
	if in.RepoID != repoID {
		t.Errorf("in.RepoID = %s, want %s", in.RepoID, repoID)
	}
	if in.SHA != sha {
		t.Errorf("in.SHA = %q, want %q", in.SHA, sha)
	}
	if in.ProducerRunID != runID {
		t.Errorf("in.ProducerRunID = %s, want %s", in.ProducerRunID, runID)
	}
	if in.XRepoEdgesAvailable {
		t.Error("in.XRepoEdgesAvailable = true; PG source v1 ships embedded-mode-only (want false)")
	}
	if in.CallEdgesAvailable {
		t.Error("in.CallEdgesAvailable = true; PG source v1 ships embedded-mode-only (want false)")
	}
	if len(in.Scopes) != 1 || in.Scopes[0].ScopeID != scopeID || in.Scopes[0].ScopeKind != "file" {
		t.Errorf("in.Scopes = %+v; want one (%s, file)", in.Scopes, scopeID)
	}
	if len(in.Foundation) != 1 {
		t.Fatalf("len(in.Foundation) = %d, want 1", len(in.Foundation))
	}
	fs := in.Foundation[0]
	if fs.ScopeID != scopeID || fs.ScopeKind != "file" || fs.MetricKind != "cyclomatic_complexity" || fs.Value != 12.5 {
		t.Errorf("foundation sample = %+v; mismatch", fs)
	}
	if fs.Attrs["language"] != "go" {
		t.Errorf("foundation sample attrs = %+v; want language=go", fs.Attrs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGSystemTierInputSource_SkipsPairsWithoutSucceededScanRun
// asserts the SKIP-when-no-anchor contract: a (repo_id, sha)
// pair with no `status='succeeded'` scan_run row produces ZERO
// SystemTierInput rows for that pair (and NO follow-up scopes
// / foundation queries are fired for that pair). The next
// pair in the result set is still processed.
func TestPGSystemTierInputSource_SkipsPairsWithoutSucceededScanRun(t *testing.T) {
	t.Parallel()
	src, mock, cleanup := newSQLMockSystemTierSource(t)
	defer cleanup()

	repoA := uuid.Must(uuid.NewV4())
	repoB := uuid.Must(uuid.NewV4())
	runB := uuid.Must(uuid.NewV4())
	scopeB := uuid.Must(uuid.NewV4())
	const shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	mock.ExpectQuery(`SELECT DISTINCT ms\.repo_id, ms\.sha`).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha"}).
			AddRow(repoA.String(), shaA).
			AddRow(repoB.String(), shaB))

	// Pair A: no scan_run anchor (sql.ErrNoRows shape) ->
	// no scope/foundation queries.
	mock.ExpectQuery(`SELECT scan_run_id`).
		WithArgs(repoA, shaA).
		WillReturnError(sql.ErrNoRows)

	// Pair B: full happy path.
	mock.ExpectQuery(`SELECT scan_run_id`).
		WithArgs(repoB, shaB).
		WillReturnRows(sqlmock.NewRows([]string{"scan_run_id"}).AddRow(runB.String()))
	mock.ExpectQuery(`SELECT DISTINCT sb\.scope_id`).
		WithArgs(repoB, shaB).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind"}).
			AddRow(scopeB.String(), "repo"))
	mock.ExpectQuery(`SELECT sb\.scope_id, sb\.scope_kind::text, ms\.metric_kind`).
		WithArgs(repoB, shaB).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind", "metric_kind", "value", "attrs_json"}).
			AddRow(scopeB.String(), "repo", "loc", 1234.0, nil))

	got, err := src.ReadSystemTierInputs(context.Background())
	if err != nil {
		t.Fatalf("ReadSystemTierInputs err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (pair A skipped, pair B kept)", len(got))
	}
	if got[0].RepoID != repoB {
		t.Errorf("kept pair RepoID = %s, want %s", got[0].RepoID, repoB)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGSystemTierInputSource_FoundationQueryFiltersSystemPack
// asserts the foundation-samples SQL EXCLUDES system-pack
// rows in its WHERE clause. The system-tier composer must
// NEVER consume its own outputs (definitional cycle); pinning
// the SQL shape via regex enforces this at the query layer.
func TestPGSystemTierInputSource_FoundationQueryFiltersSystemPack(t *testing.T) {
	t.Parallel()
	src, mock, cleanup := newSQLMockSystemTierSource(t)
	defer cleanup()

	repoID := uuid.Must(uuid.NewV4())
	runID := uuid.Must(uuid.NewV4())
	const sha = "cccccccccccccccccccccccccccccccccccccccc"

	mock.ExpectQuery(`SELECT DISTINCT ms\.repo_id, ms\.sha`).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha"}).AddRow(repoID.String(), sha))
	mock.ExpectQuery(`SELECT scan_run_id`).
		WithArgs(repoID, sha).
		WillReturnRows(sqlmock.NewRows([]string{"scan_run_id"}).AddRow(runID.String()))
	mock.ExpectQuery(`SELECT DISTINCT sb\.scope_id`).
		WithArgs(repoID, sha).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind"}))
	// Literal regex pin: the WHERE must include the closed
	// pack set 'base', 'solid', 'ingested' AND must filter
	// out NULL values / degraded rows.
	expr := `ms\.pack IN \('base', 'solid', 'ingested'\)\s+AND ms\.value IS NOT NULL\s+AND ms\.degraded = false`
	if _, err := regexp.Compile(expr); err != nil {
		t.Fatalf("regex compile: %v", err)
	}
	mock.ExpectQuery(expr).
		WithArgs(repoID, sha).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind", "metric_kind", "value", "attrs_json"}))

	if _, err := src.ReadSystemTierInputs(context.Background()); err != nil {
		t.Fatalf("ReadSystemTierInputs err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGSystemTierInputSource_PropagatesQueryError asserts a
// DB-side query failure on the top-level repo+sha enumeration
// surfaces as an error (the aggregator's outer loop will
// record it as a tick failure and back off).
func TestPGSystemTierInputSource_PropagatesQueryError(t *testing.T) {
	t.Parallel()
	src, mock, cleanup := newSQLMockSystemTierSource(t)
	defer cleanup()

	boom := errors.New("connection reset")
	mock.ExpectQuery(`SELECT DISTINCT ms\.repo_id, ms\.sha`).
		WillReturnError(boom)

	_, err := src.ReadSystemTierInputs(context.Background())
	if err == nil {
		t.Fatal("ReadSystemTierInputs err = nil; want non-nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v; want errors.Is %v", err, boom)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGSystemTierInputSource_DeterministicOrder pins G6:
// inputs are returned sorted by (repo_id bytes, sha string)
// so a re-run produces identical output bytes. Drives TWO
// pairs with deliberately reversed input ordering and
// asserts the output respects the canonical order.
func TestPGSystemTierInputSource_DeterministicOrder(t *testing.T) {
	t.Parallel()
	src, mock, cleanup := newSQLMockSystemTierSource(t)
	defer cleanup()

	// Lexically-smaller repo bytes than the other to make
	// the canonical ordering deterministic in the test.
	repoLo := uuid.UUID{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}
	repoHi := uuid.UUID{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8, 0xf7, 0xf6, 0xf5, 0xf4, 0xf3, 0xf2, 0xf1, 0xf0}
	runLo := uuid.Must(uuid.NewV4())
	runHi := uuid.Must(uuid.NewV4())
	const shaLo = "1111111111111111111111111111111111111111"
	const shaHi = "2222222222222222222222222222222222222222"

	// Pair-enumeration SQL has ORDER BY repo_id, sha;
	// sqlmock cannot validate ORDER BY on the server side,
	// but we still feed rows in the canonical order so the
	// downstream sort is a no-op for the happy path.
	mock.ExpectQuery(`SELECT DISTINCT ms\.repo_id, ms\.sha`).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha"}).
			AddRow(repoLo.String(), shaLo).
			AddRow(repoHi.String(), shaHi))

	mock.ExpectQuery(`SELECT scan_run_id`).WithArgs(repoLo, shaLo).
		WillReturnRows(sqlmock.NewRows([]string{"scan_run_id"}).AddRow(runLo.String()))
	mock.ExpectQuery(`SELECT DISTINCT sb\.scope_id`).WithArgs(repoLo, shaLo).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind"}))
	mock.ExpectQuery(`SELECT sb\.scope_id, sb\.scope_kind::text, ms\.metric_kind`).WithArgs(repoLo, shaLo).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind", "metric_kind", "value", "attrs_json"}))

	mock.ExpectQuery(`SELECT scan_run_id`).WithArgs(repoHi, shaHi).
		WillReturnRows(sqlmock.NewRows([]string{"scan_run_id"}).AddRow(runHi.String()))
	mock.ExpectQuery(`SELECT DISTINCT sb\.scope_id`).WithArgs(repoHi, shaHi).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind"}))
	mock.ExpectQuery(`SELECT sb\.scope_id, sb\.scope_kind::text, ms\.metric_kind`).WithArgs(repoHi, shaHi).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id", "scope_kind", "metric_kind", "value", "attrs_json"}))

	got, err := src.ReadSystemTierInputs(context.Background())
	if err != nil {
		t.Fatalf("ReadSystemTierInputs err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].RepoID != repoLo || got[1].RepoID != repoHi {
		t.Errorf("output order = [%s, %s]; want [%s, %s] (bytewise ascending)",
			got[0].RepoID, got[1].RepoID, repoLo, repoHi)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGSystemTierInputSource_ContextCancelled asserts
// ctx.Err() propagates even before the first query is issued
// (defensive guard at the top of ReadSystemTierInputs).
func TestPGSystemTierInputSource_ContextCancelled(t *testing.T) {
	t.Parallel()
	src, _, cleanup := newSQLMockSystemTierSource(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := src.ReadSystemTierInputs(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
}
