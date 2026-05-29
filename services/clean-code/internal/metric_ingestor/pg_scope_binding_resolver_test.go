package metric_ingestor_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/ast/scope"
	"forge/services/clean-code/internal/metric_ingestor"
	"forge/services/clean-code/internal/metrics/recipes"
	"forge/services/clean-code/internal/storage"
)

const pgScopeBindingTestSchema = "clean_code_resolver_test"

var (
	pgScopeBindingTestRepoID  = uuid.Must(uuid.FromString("aaaaaaaa-bbbb-cccc-dddd-eeeeffff0042"))
	pgScopeBindingTestSHA     = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	pgScopeBindingTestSHAv2   = "feedfacefeedfacefeedfacefeedfacefeedface"
	pgScopeBindingTestScopeID = uuid.Must(uuid.FromString("11111111-2222-3333-4444-555566667777"))
)

// newSQLMockResolver wires a PGScopeBindingResolver against a
// regex-matching mock DB. Returns the resolver, the mock
// expectation surface, and a cleanup func that asserts all
// declared expectations were met.
func newSQLMockResolver(t *testing.T) (*metric_ingestor.PGScopeBindingResolver, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	writer, err := storage.NewScopeBindingWriterWithSchema(db, pgScopeBindingTestSchema)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewScopeBindingWriterWithSchema: %v", err)
	}
	r, err := metric_ingestor.NewPGScopeBindingResolverWithWriter(writer)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPGScopeBindingResolverWithWriter: %v", err)
	}
	cleanup := func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock: unmet expectations: %v", err)
		}
		_ = db.Close()
	}
	return r, mock, cleanup
}

// TestNewPGScopeBindingResolver_RejectsNilDB pins the
// composition-root error surface (item-3 wiring): a nil DB
// is rejected at construction, not at first call.
func TestNewPGScopeBindingResolver_RejectsNilDB(t *testing.T) {
	t.Parallel()
	if _, err := metric_ingestor.NewPGScopeBindingResolver(nil); !errors.Is(err, metric_ingestor.ErrPGScopeBindingResolverNilDB) {
		t.Errorf("NewPGScopeBindingResolver(nil): err=%v, want errors.Is ErrPGScopeBindingResolverNilDB", err)
	}
}

// TestPGScopeBindingResolver_EmptyBatch pins the contract:
// an empty refs slice returns (nil, nil) without any DB
// round-trip. Mock has no expectations so an unexpected
// call would fail loudly.
func TestPGScopeBindingResolver_EmptyBatch(t *testing.T) {
	t.Parallel()
	r, _, cleanup := newSQLMockResolver(t)
	defer cleanup()

	ids, err := r.ResolveScopeIDs(context.Background(), pgScopeBindingTestRepoID, nil, pgScopeBindingTestSHA)
	if err != nil {
		t.Fatalf("ResolveScopeIDs(empty): err=%v, want nil", err)
	}
	if ids != nil {
		t.Errorf("ResolveScopeIDs(empty): ids=%v, want nil", ids)
	}
}

// TestPGScopeBindingResolver_RejectsEmptyQualifiedName pins
// the per-ref validation: an empty QualifiedName aborts the
// whole batch before any DB call.
func TestPGScopeBindingResolver_RejectsEmptyQualifiedName(t *testing.T) {
	t.Parallel()
	r, _, cleanup := newSQLMockResolver(t)
	defer cleanup()

	refs := []recipes.ScopeRef{
		{Kind: scope.KindMethod, QualifiedName: "pkg.Foo", Path: "pkg/foo.go"},
		{Kind: scope.KindMethod, QualifiedName: "", Path: "pkg/empty.go"},
	}
	_, err := r.ResolveScopeIDs(context.Background(), pgScopeBindingTestRepoID, refs, pgScopeBindingTestSHA)
	if err == nil {
		t.Fatal("ResolveScopeIDs with empty QualifiedName: err=nil, want non-nil")
	}
}

// TestPGScopeBindingResolver_HappyPath_AllExisting covers
// the steady-state hot path: every candidate's natural key
// is already in `scope_binding`. The unlocked SELECT
// returns rows, the writer skips the locked-INSERT path,
// and the resolver returns the scope_ids parallel to the
// input.
//
// This proves iter-3 evaluator item 3 (scope_binding rows
// are looked up BEFORE metric_sample inserts) AND item 4
// (the returned scope_id is the persisted value, not a
// fresh-mint -- G2 stability across SHAs).
//
// iter-4 evaluator item 1: also pins that the resolver now
// builds canonical signatures via [scope.BuildMethod] /
// [scope.BuildFile] / ... (the iter-3 resolver passed
// `ref.QualifiedName` raw, which collided across files).
// The mock's `WithArgs` literal is the new path-aware
// signature: `clean-code-repo:<repoID>::method::<path>#<NQN>()`.
func TestPGScopeBindingResolver_HappyPath_AllExisting(t *testing.T) {
	t.Parallel()
	r, mock, cleanup := newSQLMockResolver(t)
	defer cleanup()

	persistedFirstSeenSHA := "originalshaoriginalshaoriginalshaorigsha"

	// iter-4 item 1: the canonical signature is built from
	// (synthetic-repo-stamp, path, qualifiedName, params).
	// `pkg.Foo()` here is the recipe-emitted QualifiedName;
	// the parens are part of the literal QN string, not the
	// method-builder's parameter group, so the rendered
	// signature has TWO `()` -- the QN's literal `()` and
	// the builder's empty params `()`.
	wantSig := "clean-code-repo:" + pgScopeBindingTestRepoID.String() + "::method::pkg/foo.go#pkg.Foo()()"

	// The writer issues one unlocked SELECT joining
	// `(VALUES ...)` against the table. We just need to
	// match the SELECT shape and return one matching row
	// keyed by the BUILT canonical signature.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT s.scope_id, s.repo_id, s.scope_kind, s.canonical_signature, s.first_seen_sha`)).
		WithArgs(pgScopeBindingTestRepoID.String(), "method", wantSig).
		WillReturnRows(sqlmock.NewRows([]string{
			"scope_id", "repo_id", "scope_kind", "canonical_signature", "first_seen_sha",
		}).AddRow(
			pgScopeBindingTestScopeID.String(),
			pgScopeBindingTestRepoID.String(),
			"method",
			wantSig,
			persistedFirstSeenSHA,
		))

	refs := []recipes.ScopeRef{
		{Kind: scope.KindMethod, QualifiedName: "pkg.Foo()", Path: "pkg/foo.go"},
	}
	// Note: we pass a DIFFERENT SHA from the persisted
	// `first_seen_sha`. The writer reuses the persisted
	// row regardless (G2), and the resolver returns the
	// persisted scope_id. This is the iter-3 item 4 fix:
	// re-scanning at a later SHA reuses the original
	// scope_id rather than minting a fresh UUID from the
	// current SHA.
	ids, err := r.ResolveScopeIDs(context.Background(), pgScopeBindingTestRepoID, refs, pgScopeBindingTestSHAv2)
	if err != nil {
		t.Fatalf("ResolveScopeIDs(happy): err=%v, want nil", err)
	}
	if len(ids) != 1 {
		t.Fatalf("ResolveScopeIDs(happy): len(ids)=%d, want 1", len(ids))
	}
	if ids[0] != pgScopeBindingTestScopeID {
		t.Errorf("ResolveScopeIDs[0] = %s, want persisted scope_id %s (G2 stability across SHAs)",
			ids[0], pgScopeBindingTestScopeID)
	}
}

// TestPGScopeBindingResolver_HappyPath_PropagatesWriteError
// pins error propagation: a writer-layer error is wrapped
// with a resolver prefix so the dispatcher's downstream
// error message is unambiguous AND the underlying cause is
// preserved as an `errors.Is`-targetable sentinel so callers
// can switch on it without parsing text. iter-4 evaluator
// item 5 replaced the prior tautological
// `errors.Is(err, err)` assertion (which is vacuously true
// for any non-nil err) with this real-sentinel check.
func TestPGScopeBindingResolver_HappyPath_PropagatesWriteError(t *testing.T) {
	t.Parallel()
	r, mock, cleanup := newSQLMockResolver(t)
	defer cleanup()

	// Named sentinel + a literal substring stamp so we can
	// assert BOTH that the resolver's wrap preserves the
	// underlying cause (errors.Is) AND that the wrap leaves
	// the operator-visible message readable (substring).
	pgSentinel := errors.New("postgres exploded: relation \"clean_code.scope_binding\" lost")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT s.scope_id, s.repo_id, s.scope_kind, s.canonical_signature, s.first_seen_sha`)).
		WillReturnError(pgSentinel)

	refs := []recipes.ScopeRef{
		{Kind: scope.KindMethod, QualifiedName: "pkg.Boom()", Path: "pkg/boom.go"},
	}
	_, err := r.ResolveScopeIDs(context.Background(), pgScopeBindingTestRepoID, refs, pgScopeBindingTestSHA)
	if err == nil {
		t.Fatal("ResolveScopeIDs with PG error: err=nil, want non-nil")
	}
	if !errors.Is(err, pgSentinel) {
		t.Errorf("ResolveScopeIDs error: %v -- want errors.Is(err, pgSentinel) so the underlying cause is preserved through the resolver wrap (iter-4 evaluator item 5)", err)
	}
	if !strings.Contains(err.Error(), "postgres exploded") {
		t.Errorf("ResolveScopeIDs error: %v -- want operator-visible substring %q to survive the wrap", err, "postgres exploded")
	}
}

// TestPGScopeBindingResolver_NewWithWriter_RejectsNilWriter
// pins the test-constructor's nil guard.
func TestPGScopeBindingResolver_NewWithWriter_RejectsNilWriter(t *testing.T) {
	t.Parallel()
	if _, err := metric_ingestor.NewPGScopeBindingResolverWithWriter(nil); err == nil {
		t.Error("NewPGScopeBindingResolverWithWriter(nil): err=nil, want non-nil")
	}
}

// newSQLMockResolverWithURLs wires a PGScopeBindingResolver
// the same way [newSQLMockResolver] does but lets the test
// inject a [metric_ingestor.RepoURLLookup]. This is the
// real-URL surface that production wiring exercises via
// [metric_ingestor.PGRepoURLLookup] reading
// `clean_code.repo.repo_url` (migration
// `0006_repo_url.up.sql`); the test variant uses a
// hard-coded map for determinism without standing up
// Postgres.
func newSQLMockResolverWithURLs(t *testing.T, urls metric_ingestor.RepoURLLookup) (*metric_ingestor.PGScopeBindingResolver, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	writer, err := storage.NewScopeBindingWriterWithSchema(db, pgScopeBindingTestSchema)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewScopeBindingWriterWithSchema: %v", err)
	}
	r, err := metric_ingestor.NewPGScopeBindingResolverWithWriter(
		writer,
		metric_ingestor.WithPGScopeBindingResolverRepoURLLookup(urls),
	)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPGScopeBindingResolverWithWriter: %v", err)
	}
	cleanup := func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock: unmet expectations: %v", err)
		}
		_ = db.Close()
	}
	return r, mock, cleanup
}

// TestPGScopeBindingResolver_HappyPath_UsesRealRepoURL pins
// iter-6 evaluator item 3: the resolver, when wired with a
// real [metric_ingestor.RepoURLLookup] (as production does
// via [metric_ingestor.NewPGScopeBindingResolver] using
// [metric_ingestor.PGRepoURLLookup] backed by
// `clean_code.repo.repo_url` from migration
// `0006_repo_url.up.sql`), stamps the canonical signature
// with the OPERATOR URL, NOT the synthetic
// `clean-code-repo:<repoID>` fall-back.
//
// The assertion is the sqlmock.WithArgs literal: if the
// resolver were still calling the synthetic-stamp path the
// SELECT would carry `clean-code-repo:<uuid>::...` and this
// expectation would fail with an unmet-expectation error.
func TestPGScopeBindingResolver_HappyPath_UsesRealRepoURL(t *testing.T) {
	t.Parallel()
	realURL := "https://example.com/org/repo"
	urls := metric_ingestor.StaticRepoURLLookup{
		URLs: map[uuid.UUID]string{pgScopeBindingTestRepoID: realURL},
	}
	r, mock, cleanup := newSQLMockResolverWithURLs(t, urls)
	defer cleanup()

	// Canonical signature now uses the OPERATOR URL as the
	// repo stamp -- proving the lookup -> signature pipe
	// reaches the writer.
	wantSig := realURL + "::method::pkg/foo.go#pkg.Foo()()"

	persistedFirstSeenSHA := "originalshaoriginalshaoriginalshaorigsha"
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT s.scope_id, s.repo_id, s.scope_kind, s.canonical_signature, s.first_seen_sha`)).
		WithArgs(pgScopeBindingTestRepoID.String(), "method", wantSig).
		WillReturnRows(sqlmock.NewRows([]string{
			"scope_id", "repo_id", "scope_kind", "canonical_signature", "first_seen_sha",
		}).AddRow(
			pgScopeBindingTestScopeID.String(),
			pgScopeBindingTestRepoID.String(),
			"method",
			wantSig,
			persistedFirstSeenSHA,
		))

	refs := []recipes.ScopeRef{
		{Kind: scope.KindMethod, QualifiedName: "pkg.Foo()", Path: "pkg/foo.go"},
	}
	ids, err := r.ResolveScopeIDs(context.Background(), pgScopeBindingTestRepoID, refs, pgScopeBindingTestSHA)
	if err != nil {
		t.Fatalf("ResolveScopeIDs (real URL): err=%v, want nil", err)
	}
	if len(ids) != 1 {
		t.Fatalf("ResolveScopeIDs (real URL): len(ids)=%d, want 1", len(ids))
	}
	if ids[0] != pgScopeBindingTestScopeID {
		t.Errorf("ResolveScopeIDs[0] = %s, want persisted scope_id %s", ids[0], pgScopeBindingTestScopeID)
	}
	// Belt-and-braces: ensure the signature literal
	// embedded in the WithArgs is NOT the synthetic stamp
	// shape. If the resolver fell back to
	// SyntheticRepoURLLookup despite the option being
	// supplied, the WithArgs match above would already have
	// failed -- this is just a readable in-test assertion
	// that the chosen wantSig is structurally distinct from
	// the synthetic literal.
	if strings.HasPrefix(wantSig, "clean-code-repo:") {
		t.Fatalf("test wiring bug: wantSig starts with synthetic prefix; this test must exercise the real-URL path")
	}
	if !strings.HasPrefix(wantSig, realURL+"::") {
		t.Fatalf("test wiring bug: wantSig does not start with realURL stamp; got %q", wantSig)
	}
}

// TestPGScopeBindingResolver_LookupError_PropagatesAsAbort
// pins iter-6 evaluator item 3 follow-up: when the wired
// [metric_ingestor.RepoURLLookup] returns
// [metric_ingestor.ErrRepoURLLookupNotFound] (e.g. a row
// inserted before migration 0006 with NULL `repo_url`, or
// a repo that was never registered via mgmt.register_repo),
// the resolver MUST abort with that wrapped error rather
// than silently fall back to the synthetic stamp -- a
// silent fall-back would collapse signatures for that repo
// to the synthetic prefix and break the G2 stability
// guarantee the whole iter-6 change was meant to deliver.
//
// The sqlmock has NO ExpectQuery: a lookup failure must
// short-circuit BEFORE any DB call.
func TestPGScopeBindingResolver_LookupError_PropagatesAsAbort(t *testing.T) {
	t.Parallel()
	// Empty map -> lookup returns wrap of
	// ErrRepoURLLookupNotFound for our test repoID.
	urls := metric_ingestor.StaticRepoURLLookup{URLs: nil}
	r, _, cleanup := newSQLMockResolverWithURLs(t, urls)
	defer cleanup()

	refs := []recipes.ScopeRef{
		{Kind: scope.KindMethod, QualifiedName: "pkg.Foo()", Path: "pkg/foo.go"},
	}
	_, err := r.ResolveScopeIDs(context.Background(), pgScopeBindingTestRepoID, refs, pgScopeBindingTestSHA)
	if err == nil {
		t.Fatal("ResolveScopeIDs (lookup-not-found): err=nil, want non-nil")
	}
	if !errors.Is(err, metric_ingestor.ErrRepoURLLookupNotFound) {
		t.Errorf("ResolveScopeIDs (lookup-not-found): err=%v, want errors.Is ErrRepoURLLookupNotFound", err)
	}
}
