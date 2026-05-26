package coverage_test

// pg_resolver_test.go covers the iter-3 evaluator item 3
// production resolver. The PG SELECT is exercised with
// go-sqlmock so the round-trip is verified without
// standing up a Postgres fixture.

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/coverage"
)

const pgResolverTestSchema = "clean_code"

var pgResolverTestRepoID = uuid.Must(uuid.FromString("aaaaaaaa-1111-2222-3333-444444444444"))

func staticURL(url string) coverage.RepoURLLookupFunc {
	return func(_ context.Context, _ uuid.UUID) (string, error) {
		return url, nil
	}
}

// TestNewPGScopeResolver_RejectsNilDB pins the
// construction-time fail-fast for a nil `*sql.DB`.
func TestNewPGScopeResolver_RejectsNilDB(t *testing.T) {
	t.Parallel()
	_, err := coverage.NewPGScopeResolver(nil, staticURL("x"))
	if !errors.Is(err, coverage.ErrPGScopeResolverNilDB) {
		t.Fatalf("err = %v; want ErrPGScopeResolverNilDB", err)
	}
}

// TestNewPGScopeResolver_RejectsNilURLLookup pins the
// construction-time fail-fast for a nil [RepoURLLookupFunc].
func TestNewPGScopeResolver_RejectsNilURLLookup(t *testing.T) {
	t.Parallel()
	_, err := coverage.NewPGScopeResolver(&sql.DB{}, nil)
	if !errors.Is(err, coverage.ErrPGScopeResolverNilURLLookup) {
		t.Fatalf("err = %v; want ErrPGScopeResolverNilURLLookup", err)
	}
}

// TestNewPGScopeResolverWithSchema_RejectsEmptySchema pins
// the construction-time fail-fast for an empty schema.
func TestNewPGScopeResolverWithSchema_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	_, err := coverage.NewPGScopeResolverWithSchema(&sql.DB{}, "", staticURL("x"))
	if !errors.Is(err, coverage.ErrPGScopeResolverEmptySchema) {
		t.Fatalf("err = %v; want ErrPGScopeResolverEmptySchema", err)
	}
}

// TestPGScopeResolver_ResolvesScopeID pins the happy path:
// a (repo_id, file_path) pair whose scope_binding row
// exists returns (scope_id, true, nil) and the SQL
// statement uses the canonical natural-key shape.
func TestPGScopeResolver_ResolvesScopeID(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	wantScopeID := uuid.Must(uuid.FromString("bbbbbbbb-1111-2222-3333-444444444444"))
	repoURL := "https://example.com/org/coverage-repo"
	relPath := "internal/svc/handler.go"
	wantSig, err := scope.BuildFile(repoURL, relPath)
	if err != nil {
		t.Fatalf("scope.BuildFile: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT scope_id`)).
		WithArgs(pgResolverTestRepoID.String(), string(scope.KindFile), wantSig).
		WillReturnRows(sqlmock.NewRows([]string{"scope_id"}).AddRow(wantScopeID.String()))

	r, err := coverage.NewPGScopeResolverWithSchema(db, pgResolverTestSchema, staticURL(repoURL))
	if err != nil {
		t.Fatalf("NewPGScopeResolverWithSchema: %v", err)
	}

	gotID, ref, found, err := r.ResolveFileScope(context.Background(), pgResolverTestRepoID, "deadbeef", relPath)
	if err != nil {
		t.Fatalf("ResolveFileScope: %v", err)
	}
	if !found {
		t.Errorf("found = false; want true")
	}
	if gotID != wantScopeID {
		t.Errorf("scope_id = %s; want %s", gotID, wantScopeID)
	}
	if ref.Kind != scope.KindFile {
		t.Errorf("ref.Kind = %s; want file", ref.Kind)
	}
	if ref.Path != relPath || ref.QualifiedName != relPath {
		t.Errorf("ref.Path / QualifiedName = (%q, %q); want both %q", ref.Path, ref.QualifiedName, relPath)
	}
	if ref.LocalID != wantScopeID.String() {
		t.Errorf("ref.LocalID = %q; want %q", ref.LocalID, wantScopeID.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

// TestPGScopeResolver_MissingBindingReturnsFoundFalseNoError
// pins the skip-and-count contract: a row that does not
// exist in scope_binding returns (uuid.Nil, _, false, nil)
// so the hydrator's
// `coverage_skipped_unbound_scope`-incrementing path runs.
// This is THE invariant that distinguishes coverage from
// the churn AutoMapScopeResolver -- coverage MUST NOT
// invent a scope_id.
func TestPGScopeResolver_MissingBindingReturnsFoundFalseNoError(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT scope_id`)).
		WillReturnError(sql.ErrNoRows)

	r, err := coverage.NewPGScopeResolverWithSchema(db, pgResolverTestSchema, staticURL("https://example.com/r"))
	if err != nil {
		t.Fatalf("NewPGScopeResolverWithSchema: %v", err)
	}

	gotID, _, found, err := r.ResolveFileScope(context.Background(), pgResolverTestRepoID, "deadbeef", "internal/foo.go")
	if err != nil {
		t.Fatalf("ResolveFileScope: want nil error, got %v", err)
	}
	if found {
		t.Errorf("found = true; want false (no scope_binding row -> hydrator skip path)")
	}
	if gotID != uuid.Nil {
		t.Errorf("scope_id = %s; want uuid.Nil", gotID)
	}
}

// TestPGScopeResolver_URLLookupFailurePropagates pins the
// hard-error path: an upstream RepoURLLookupFunc error
// MUST bubble back so the hydrator aborts (it cannot build
// the canonical signature without the URL).
func TestPGScopeResolver_URLLookupFailurePropagates(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	wantErr := errors.New("repo_url lookup boom")
	urls := func(_ context.Context, _ uuid.UUID) (string, error) { return "", wantErr }

	r, err := coverage.NewPGScopeResolverWithSchema(db, pgResolverTestSchema, urls)
	if err != nil {
		t.Fatalf("NewPGScopeResolverWithSchema: %v", err)
	}
	_, _, found, err := r.ResolveFileScope(context.Background(), pgResolverTestRepoID, "deadbeef", "internal/foo.go")
	if err == nil {
		t.Fatalf("ResolveFileScope: want error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v; want errors.Is wantErr", err)
	}
	if found {
		t.Errorf("found = true; want false on error")
	}
}

// TestPGScopeResolver_QueryFailurePropagates pins the
// hard-error path for an infrastructure failure on the
// SELECT (e.g. connection drop). Distinguished from
// sql.ErrNoRows so the hydrator aborts rather than
// silently dropping the row into the skip counter.
func TestPGScopeResolver_QueryFailurePropagates(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	wantErr := errors.New("connection refused")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT scope_id`)).WillReturnError(wantErr)

	r, err := coverage.NewPGScopeResolverWithSchema(db, pgResolverTestSchema, staticURL("https://example.com/r"))
	if err != nil {
		t.Fatalf("NewPGScopeResolverWithSchema: %v", err)
	}
	_, _, found, err := r.ResolveFileScope(context.Background(), pgResolverTestRepoID, "deadbeef", "internal/foo.go")
	if err == nil {
		t.Fatalf("ResolveFileScope: want error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v; want errors.Is wantErr", err)
	}
	if found {
		t.Errorf("found = true; want false on error")
	}
}

// TestPGScopeResolver_RejectsZeroRepoID pins the
// input-validation guard.
func TestPGScopeResolver_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, err := coverage.NewPGScopeResolverWithSchema(db, pgResolverTestSchema, staticURL("https://example.com/r"))
	if err != nil {
		t.Fatalf("NewPGScopeResolverWithSchema: %v", err)
	}
	_, _, _, err = r.ResolveFileScope(context.Background(), uuid.Nil, "deadbeef", "internal/foo.go")
	if err == nil {
		t.Fatalf("ResolveFileScope: want error for zero repoID, got nil")
	}
}

// TestPGScopeResolver_RejectsEmptyFilePath pins the
// input-validation guard for the file_path argument.
func TestPGScopeResolver_RejectsEmptyFilePath(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, err := coverage.NewPGScopeResolverWithSchema(db, pgResolverTestSchema, staticURL("https://example.com/r"))
	if err != nil {
		t.Fatalf("NewPGScopeResolverWithSchema: %v", err)
	}
	_, _, _, err = r.ResolveFileScope(context.Background(), pgResolverTestRepoID, "deadbeef", "")
	if err == nil {
		t.Fatalf("ResolveFileScope: want error for empty filePath, got nil")
	}
}
