package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// envScopeBindingURL is the libpq DSN the ScopeBindingWriter
// live tests connect to. Shared with the steward / keys
// SQLStore live tests so a single `export CLEAN_CODE_PG_URL=...`
// turns every live path on at once.
const envScopeBindingURL = "CLEAN_CODE_PG_URL"

// scopeBindingTestSchemaName is the ISOLATED PostgreSQL schema
// the ScopeBindingWriter live tests own. Distinct from the
// canonical `clean_code` schema (the migrate test DROP SCHEMA
// CASCADEs it on prep) AND from `clean_code_steward_test` /
// `clean_code_keys_test` so the four test suites run in
// parallel without racing on a shared DROP.
const scopeBindingTestSchemaName = "clean_code_scope_test"

// scopeBindingSchemaPrepTemplate materialises ONLY the
// `scope_binding` table + its `scope_kind` enum dependency
// (and a stand-in `repo` table the FK constraint demands).
// Mirrors migration 0002 lines 142-219 byte-for-byte so the
// writer exercises the same column shape it sees in
// production; intentionally omits the rest of the measurement
// schema so the test stays decoupled from migration ordering.
const scopeBindingSchemaPrepTemplate = `
DROP SCHEMA IF EXISTS %[1]s CASCADE;
CREATE SCHEMA %[1]s;

CREATE TYPE %[1]s.scope_kind AS ENUM (
    'repo', 'package', 'file', 'class', 'interface', 'method', 'block'
);

CREATE TABLE %[1]s.repo (
    repo_id  uuid  PRIMARY KEY,
    url      text  NOT NULL UNIQUE
);

CREATE TABLE %[1]s.scope_binding (
    scope_id              uuid                       PRIMARY KEY,
    repo_id               uuid                       NOT NULL
                          REFERENCES %[1]s.repo (repo_id)
                          ON DELETE RESTRICT,
    scope_kind            %[1]s.scope_kind           NOT NULL,
    canonical_signature   text                       NOT NULL,
    first_seen_sha        text                       NOT NULL,
    agent_memory_node_id  uuid,
    attrs_json            jsonb                      NOT NULL
                          DEFAULT '{}'::jsonb,
    created_at            timestamptz                NOT NULL
                          DEFAULT now(),
    CONSTRAINT scope_binding_natural_key_uniq
        UNIQUE (repo_id, scope_kind, canonical_signature, first_seen_sha)
);
`

const scopeBindingSchemaTeardownTemplate = `
DROP SCHEMA IF EXISTS %[1]s CASCADE;
`

// openScopeBindingWriter opens the live PostgreSQL handle for
// the ScopeBindingWriter test suite and returns a *sql.DB and
// a *ScopeBindingWriter wired to the isolated test schema.
// Seeds one `repo` row keyed by `defaultTestRepoID` so the FK
// constraint is satisfied; helpers that need a different
// repo_id must seed their own row.
func openScopeBindingWriter(t *testing.T) (*sql.DB, *ScopeBindingWriter, bool) {
	t.Helper()
	url := strings.TrimSpace(os.Getenv(envScopeBindingURL))
	if url == "" {
		t.Skipf("skipping: %s is unset; ScopeBindingWriter live tests require PostgreSQL", envScopeBindingURL)
		return nil, nil, false
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("sql.Open(postgres, %s): %v", envScopeBindingURL, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("db.Ping(%s): %v (set %s to a live PostgreSQL DSN OR unset it to skip)",
			envScopeBindingURL, err, envScopeBindingURL)
	}
	prep := fmt.Sprintf(scopeBindingSchemaPrepTemplate, scopeBindingTestSchemaName)
	if _, err := db.ExecContext(ctx, prep); err != nil {
		_ = db.Close()
		t.Fatalf("schema prep: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		teardown := fmt.Sprintf(scopeBindingSchemaTeardownTemplate, scopeBindingTestSchemaName)
		_, _ = db.ExecContext(ctx, teardown)
		_ = db.Close()
	})

	w, err := NewScopeBindingWriterWithSchema(db, scopeBindingTestSchemaName)
	if err != nil {
		t.Fatalf("NewScopeBindingWriterWithSchema: %v", err)
	}
	return db, w, true
}

// defaultTestRepoID is the canonical seeded repo_id every
// ScopeBindingWriter live test uses unless it explicitly seeds
// its own row. Fixed value keeps the test output deterministic
// across runs.
var defaultTestRepoID = uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111"))

// seedRepo INSERTs a single repo row (or no-ops if it is
// already present) so the `scope_binding.repo_id` FK is always
// satisfied. Idempotent so a test can call it in a loop without
// guarding.
func seedRepo(t *testing.T, db *sql.DB, schema string, repoID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stmt := fmt.Sprintf(`INSERT INTO %s.repo (repo_id, url)
		VALUES ($1, $2)
		ON CONFLICT (repo_id) DO NOTHING`, schema)
	url := "https://github.com/acme/" + repoID.String()
	if _, err := db.ExecContext(ctx, stmt, repoID.String(), url); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
}

func TestNewScopeBindingWriter_RejectsNilDB(t *testing.T) {
	t.Parallel()
	_, err := NewScopeBindingWriter(nil)
	if err == nil {
		t.Fatal("NewScopeBindingWriter(nil): err = nil; want non-nil")
	}
}

func TestNewScopeBindingWriter_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	_, err := NewScopeBindingWriterWithSchema(&sql.DB{}, "")
	if err == nil {
		t.Fatal("NewScopeBindingWriterWithSchema(_, \"\"): err = nil; want non-nil")
	}
}

// TestScopeBindingWriter_Validation pins the API-boundary
// rejection rules: every guard that fires WITHOUT a DB round-
// trip lives here so the helper compiles into a single fast
// `go test -run Validation` run with no PostgreSQL.
func TestScopeBindingWriter_Validation(t *testing.T) {
	t.Parallel()
	// Use a real *sql.DB only because the constructor demands
	// non-nil; the validate path runs before any query.
	db, err := sql.Open("postgres", "host=invalid")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	w, err := NewScopeBindingWriterWithSchema(db, "any_schema")
	if err != nil {
		t.Fatalf("NewScopeBindingWriterWithSchema: %v", err)
	}

	good := ScopeBindingCandidate{
		RepoID:             defaultTestRepoID,
		Kind:               scope.KindFile,
		CanonicalSignature: "https://github.com/acme/repo::file::foo.go",
		CurrentSHA:         strings.Repeat("a", 40),
	}

	cases := []struct {
		name string
		cand ScopeBindingCandidate
		want error
	}{
		{
			name: "zero repo_id",
			cand: func() ScopeBindingCandidate { c := good; c.RepoID = uuid.Nil; return c }(),
			want: scope.ErrZeroRepoID,
		},
		{
			name: "invalid kind",
			cand: func() ScopeBindingCandidate { c := good; c.Kind = scope.Kind("function"); return c }(),
			want: scope.ErrInvalidKind,
		},
		{
			name: "empty signature",
			cand: func() ScopeBindingCandidate { c := good; c.CanonicalSignature = ""; return c }(),
			want: scope.ErrEmptyField,
		},
		{
			name: "NUL in signature",
			cand: func() ScopeBindingCandidate { c := good; c.CanonicalSignature = "a\x00b"; return c }(),
			want: scope.ErrEmbeddedNUL,
		},
		{
			name: "empty sha",
			cand: func() ScopeBindingCandidate { c := good; c.CurrentSHA = ""; return c }(),
			want: scope.ErrEmptyField,
		},
		{
			name: "NUL in sha",
			cand: func() ScopeBindingCandidate { c := good; c.CurrentSHA = "a\x00b"; return c }(),
			want: scope.ErrEmbeddedNUL,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.Write(context.Background(), []ScopeBindingCandidate{tc.cand})
			if err == nil {
				t.Fatalf("expected error %v, got nil", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("expected errors.Is to match %v, got %v", tc.want, err)
			}
		})
	}

	// Invalid AttrsJSON: validate guards before round-trip.
	bad := good
	bad.AttrsJSON = json.RawMessage("{not-json")
	_, err = w.Write(context.Background(), []ScopeBindingCandidate{bad})
	if err == nil || !strings.Contains(err.Error(), "AttrsJSON is not valid JSON") {
		t.Errorf("expected AttrsJSON validation error, got %v", err)
	}
}

// TestScopeBindingWriter_EmptyBatch is the no-op fast path:
// passing an empty slice MUST NOT touch the DB. Verifies by
// using a *sql.DB with a clearly-broken DSN; if the writer
// touched it, sql.Open's lazy validation would surface here.
func TestScopeBindingWriter_EmptyBatch(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("postgres", "host=invalid-host-that-must-not-resolve.example")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	w, err := NewScopeBindingWriterWithSchema(db, "any")
	if err != nil {
		t.Fatalf("NewScopeBindingWriterWithSchema: %v", err)
	}
	result, err := w.Write(context.Background(), nil)
	if err != nil {
		t.Fatalf("Write(nil): %v", err)
	}
	if result.Rows != nil || result.Inserted != 0 {
		t.Errorf("expected zero-value result for nil batch, got %+v", result)
	}
	result, err = w.Write(context.Background(), []ScopeBindingCandidate{})
	if err != nil {
		t.Fatalf("Write([]): %v", err)
	}
	if result.Rows != nil || result.Inserted != 0 {
		t.Errorf("expected zero-value result for empty batch, got %+v", result)
	}
}

// TestScopeBindingWriter_InsertNewRows covers the happy path:
// a brand-new batch of distinct natural keys produces one row
// each, each scope_id matches `scope.DeriveScopeID`, each
// first_seen_sha is the candidate's CurrentSHA, and a parallel
// SELECT confirms attrs_json round-trips.
func TestScopeBindingWriter_InsertNewRows(t *testing.T) {
	// NOT t.Parallel(): the live PG tests share `scopeBindingTestSchemaName`
	// and each one DROP/CREATE-prepares the schema, so two concurrent
	// tests would race on the SCHEMA-create. The non-live tests above
	// stay parallel.
	db, w, ok := openScopeBindingWriter(t)
	if !ok {
		return
	}
	seedRepo(t, db, scopeBindingTestSchemaName, defaultTestRepoID)

	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	candidates := []ScopeBindingCandidate{
		{
			RepoID:             defaultTestRepoID,
			Kind:               scope.KindRepo,
			CanonicalSignature: mustBuildRepo(t, "https://github.com/acme/repo"),
			CurrentSHA:         sha,
			AttrsJSON:          json.RawMessage(`{"lang":"go"}`),
		},
		{
			RepoID:             defaultTestRepoID,
			Kind:               scope.KindFile,
			CanonicalSignature: mustBuildFile(t, "https://github.com/acme/repo", "src/foo.go"),
			CurrentSHA:         sha,
			AgentMemoryNodeID:  uuid.NullUUID{UUID: uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")), Valid: true},
		},
		{
			RepoID:             defaultTestRepoID,
			Kind:               scope.KindMethod,
			CanonicalSignature: mustBuildMethod(t, "https://github.com/acme/repo", "src/foo.go", "pkg.Foo.bar", []string{"int"}),
			CurrentSHA:         sha,
		},
	}
	result, err := w.Write(ctx, candidates)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := result.Inserted, len(candidates); got != want {
		t.Errorf("Inserted = %d, want %d", got, want)
	}
	if got, want := result.ReusedExisting, 0; got != want {
		t.Errorf("ReusedExisting = %d, want %d", got, want)
	}
	if got, want := len(result.Rows), len(candidates); got != want {
		t.Fatalf("len(Rows) = %d, want %d", got, want)
	}
	for i, r := range result.Rows {
		if r.AlreadyExisted {
			t.Errorf("Rows[%d].AlreadyExisted = true, want false", i)
		}
		want, err := scope.DeriveScopeID(r.Candidate.RepoID, r.Candidate.Kind, r.Candidate.CanonicalSignature, r.Candidate.CurrentSHA)
		if err != nil {
			t.Fatalf("derive: %v", err)
		}
		if r.ScopeID != want {
			t.Errorf("Rows[%d].ScopeID = %s, want %s", i, r.ScopeID, want)
		}
		if r.FirstSeenSHA != sha {
			t.Errorf("Rows[%d].FirstSeenSHA = %q, want %q", i, r.FirstSeenSHA, sha)
		}
	}

	// Spot-check the second row's attrs_json round-trips and
	// the agent_memory_node_id column is non-NULL for the FK
	// row that set it. The first row's attrs round-trip is
	// implied; the third (no attrs, no node_id) is also
	// covered by the JSONB DEFAULT.
	verifyRow(t, db, scopeBindingTestSchemaName, result.Rows[0].ScopeID, expectRow{
		repoID:       defaultTestRepoID,
		kind:         scope.KindRepo,
		sig:          candidates[0].CanonicalSignature,
		firstSeenSHA: sha,
		hasNodeID:    false,
		attrsJSON:    `{"lang": "go"}`,
	})
	verifyRow(t, db, scopeBindingTestSchemaName, result.Rows[1].ScopeID, expectRow{
		repoID:       defaultTestRepoID,
		kind:         scope.KindFile,
		sig:          candidates[1].CanonicalSignature,
		firstSeenSHA: sha,
		hasNodeID:    true,
		nodeID:       uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")),
		attrsJSON:    `{}`,
	})
	verifyRow(t, db, scopeBindingTestSchemaName, result.Rows[2].ScopeID, expectRow{
		repoID:       defaultTestRepoID,
		kind:         scope.KindMethod,
		sig:          candidates[2].CanonicalSignature,
		firstSeenSHA: sha,
		hasNodeID:    false,
		attrsJSON:    `{}`,
	})
}

// TestScopeBindingWriter_G2StableAcrossSHAs covers the
// implementation-plan `scope-id-stable-across-shas` scenario:
// a tuple first seen at SHA A and observed again at SHA B
// resolves to the SAME scope_id AND preserves the original
// first_seen_sha (A). This is the load-bearing G2 invariant.
func TestScopeBindingWriter_G2StableAcrossSHAs(t *testing.T) {
	// NOT t.Parallel(): see TestScopeBindingWriter_InsertNewRows.
	db, w, ok := openScopeBindingWriter(t)
	if !ok {
		return
	}
	seedRepo(t, db, scopeBindingTestSchemaName, defaultTestRepoID)

	ctx := context.Background()
	const shaA = "1111111111111111111111111111111111111111"
	const shaB = "2222222222222222222222222222222222222222"
	sig := mustBuildMethod(t, "https://github.com/acme/repo", "src/foo.go", "pkg.Foo.bar", []string{"int"})

	// First observation at SHA A.
	first, err := w.Write(ctx, []ScopeBindingCandidate{{
		RepoID:             defaultTestRepoID,
		Kind:               scope.KindMethod,
		CanonicalSignature: sig,
		CurrentSHA:         shaA,
	}})
	if err != nil {
		t.Fatalf("Write SHA A: %v", err)
	}
	if first.Inserted != 1 {
		t.Errorf("first write Inserted = %d, want 1", first.Inserted)
	}
	if first.Rows[0].AlreadyExisted {
		t.Error("first write AlreadyExisted = true; want false")
	}
	firstScopeID := first.Rows[0].ScopeID
	if first.Rows[0].FirstSeenSHA != shaA {
		t.Errorf("first write FirstSeenSHA = %q, want %q", first.Rows[0].FirstSeenSHA, shaA)
	}

	// Second observation at SHA B (the producer "forgot" to
	// cache first_seen_sha and is passing the current SHA;
	// this is the EXACT bug the writer's natural-key lookup
	// must absorb).
	second, err := w.Write(ctx, []ScopeBindingCandidate{{
		RepoID:             defaultTestRepoID,
		Kind:               scope.KindMethod,
		CanonicalSignature: sig,
		CurrentSHA:         shaB,
	}})
	if err != nil {
		t.Fatalf("Write SHA B: %v", err)
	}
	if second.Inserted != 0 {
		t.Errorf("second write Inserted = %d, want 0 (existing row reused)", second.Inserted)
	}
	if second.ReusedExisting != 1 {
		t.Errorf("second write ReusedExisting = %d, want 1", second.ReusedExisting)
	}
	if second.SHADivergences != 1 {
		t.Errorf("second write SHADivergences = %d, want 1 (CurrentSHA differs from persisted)", second.SHADivergences)
	}
	if !second.Rows[0].AlreadyExisted {
		t.Error("second write AlreadyExisted = false; want true")
	}
	if second.Rows[0].ScopeID != firstScopeID {
		t.Errorf("scope_id NOT stable across SHAs: first=%s second=%s", firstScopeID, second.Rows[0].ScopeID)
	}
	if second.Rows[0].FirstSeenSHA != shaA {
		t.Errorf("second write FirstSeenSHA = %q, want %q (persisted value reused)", second.Rows[0].FirstSeenSHA, shaA)
	}

	// Confirm the underlying row's first_seen_sha column is
	// STILL shaA (writer must NOT have UPDATEd the column).
	verifyRow(t, db, scopeBindingTestSchemaName, firstScopeID, expectRow{
		repoID:       defaultTestRepoID,
		kind:         scope.KindMethod,
		sig:          sig,
		firstSeenSHA: shaA,
		attrsJSON:    `{}`,
	})

	// Count rows: still exactly 1 for this natural key.
	if got := countByNaturalKey(t, db, defaultTestRepoID, scope.KindMethod, sig); got != 1 {
		t.Errorf("row count for natural key = %d, want 1 (G2 invariant)", got)
	}
}

// TestScopeBindingWriter_Idempotent: calling Write twice with
// the exact same batch yields the same Rows on both calls and
// the second call inserts zero rows.
func TestScopeBindingWriter_Idempotent(t *testing.T) {
	// NOT t.Parallel(): see TestScopeBindingWriter_InsertNewRows.
	db, w, ok := openScopeBindingWriter(t)
	if !ok {
		return
	}
	seedRepo(t, db, scopeBindingTestSchemaName, defaultTestRepoID)

	ctx := context.Background()
	sha := strings.Repeat("d", 40)
	cands := []ScopeBindingCandidate{{
		RepoID:             defaultTestRepoID,
		Kind:               scope.KindClass,
		CanonicalSignature: mustBuildClass(t, "https://github.com/acme/repo", "src/Bar.go", "pkg.Bar"),
		CurrentSHA:         sha,
	}}

	r1, err := w.Write(ctx, cands)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	r2, err := w.Write(ctx, cands)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if r1.Rows[0].ScopeID != r2.Rows[0].ScopeID {
		t.Errorf("scope_id drifted between identical Write calls: %s vs %s", r1.Rows[0].ScopeID, r2.Rows[0].ScopeID)
	}
	if r2.Inserted != 0 {
		t.Errorf("second Write.Inserted = %d, want 0", r2.Inserted)
	}
	if r2.ReusedExisting != 1 {
		t.Errorf("second Write.ReusedExisting = %d, want 1", r2.ReusedExisting)
	}
	if !r2.Rows[0].AlreadyExisted {
		t.Error("second Write.Rows[0].AlreadyExisted = false; want true")
	}
}

// TestScopeBindingWriter_BatchWithDuplicates: a batch containing
// the same natural key twice (with the SAME CurrentSHA) is
// collapsed to a single INSERT and BOTH result rows resolve to
// the same scope_id. The first occurrence wins (AlreadyExisted
// false); the second is reported as AlreadyExisted=true because
// only ONE row was minted -- the second candidate did not
// contribute a row to the DB.
//
// Implementation: the intra-batch dedupe pass groups candidates
// by natural key BEFORE deriving any scope_ids and BEFORE the
// SELECT/INSERT round-trips, so duplicate slots share the
// winner's scope_id by construction. The per-repo advisory
// lock acquired in writeFreshLocked then serializes any
// concurrent writers on the same repo (defect #4 fix).
func TestScopeBindingWriter_BatchWithDuplicates(t *testing.T) {
	// NOT t.Parallel(): see TestScopeBindingWriter_InsertNewRows.
	db, w, ok := openScopeBindingWriter(t)
	if !ok {
		return
	}
	seedRepo(t, db, scopeBindingTestSchemaName, defaultTestRepoID)

	ctx := context.Background()
	sha := strings.Repeat("e", 40)
	sig := mustBuildPackage(t, "https://github.com/acme/repo", "internal/storage")
	cand := ScopeBindingCandidate{
		RepoID:             defaultTestRepoID,
		Kind:               scope.KindPackage,
		CanonicalSignature: sig,
		CurrentSHA:         sha,
	}
	result, err := w.Write(ctx, []ScopeBindingCandidate{cand, cand})
	if err != nil {
		t.Fatalf("Write duplicates: %v", err)
	}
	if got, want := len(result.Rows), 2; got != want {
		t.Fatalf("len(Rows) = %d, want %d", got, want)
	}
	if result.Rows[0].ScopeID != result.Rows[1].ScopeID {
		t.Errorf("duplicate candidates resolved to DIFFERENT scope_ids: %s vs %s",
			result.Rows[0].ScopeID, result.Rows[1].ScopeID)
	}
	if result.Rows[0].AlreadyExisted {
		t.Errorf("Rows[0].AlreadyExisted = true, want false (winner of intra-batch dedupe group)")
	}
	if !result.Rows[1].AlreadyExisted {
		t.Errorf("Rows[1].AlreadyExisted = false, want true (sibling of intra-batch dedupe group did NOT mint a row)")
	}
	if result.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1 (one row minted for the dedupe group)", result.Inserted)
	}
	if result.ReusedExisting != 1 {
		t.Errorf("ReusedExisting = %d, want 1 (the second slot reused the first slot's row)", result.ReusedExisting)
	}
	// Only one row landed in the DB.
	if got := countByNaturalKey(t, db, defaultTestRepoID, scope.KindPackage, sig); got != 1 {
		t.Errorf("row count for duplicate natural key = %d, want 1", got)
	}
}

// TestScopeBindingWriter_NaturalKeyDistinct: ensures rows
// distinguished only by kind / signature land as DIFFERENT
// rows. Pins the natural-key UNIQUE plus the writer's
// kind+signature differentiation in one pass.
func TestScopeBindingWriter_NaturalKeyDistinct(t *testing.T) {
	// NOT t.Parallel(): see TestScopeBindingWriter_InsertNewRows.
	db, w, ok := openScopeBindingWriter(t)
	if !ok {
		return
	}
	seedRepo(t, db, scopeBindingTestSchemaName, defaultTestRepoID)

	ctx := context.Background()
	sha := strings.Repeat("f", 40)
	base := ScopeBindingCandidate{
		RepoID:             defaultTestRepoID,
		Kind:               scope.KindClass,
		CanonicalSignature: mustBuildClass(t, "https://github.com/acme/repo", "src/Baz.go", "pkg.Baz"),
		CurrentSHA:         sha,
	}
	differentKind := base
	differentKind.Kind = scope.KindInterface
	// agent-memory parity: a class and an interface with the
	// same (relPath, qualifiedName) produce IDENTICAL
	// canonical_signature strings (both use the `::class::`
	// discriminator). They differ only in the scope_kind
	// column, which is independently part of the natural-key
	// UNIQUE and the scope_id UUIDv5 pre-image -- so the
	// `(class, sig=X)` and `(interface, sig=X)` rows are
	// legitimately distinct ScopeBinding rows with distinct
	// scope_ids. This pin documents that intent.
	differentKind.CanonicalSignature = mustBuildInterface(t, "https://github.com/acme/repo", "src/Baz.go", "pkg.Baz")
	differentSig := base
	differentSig.CanonicalSignature = mustBuildClass(t, "https://github.com/acme/repo", "src/Baz.go", "pkg.Qux")

	result, err := w.Write(ctx, []ScopeBindingCandidate{base, differentKind, differentSig})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if result.Inserted != 3 {
		t.Errorf("Inserted = %d, want 3", result.Inserted)
	}
	seen := map[uuid.UUID]struct{}{}
	for _, r := range result.Rows {
		if _, dup := seen[r.ScopeID]; dup {
			t.Errorf("scope_id collision: %s", r.ScopeID)
		}
		seen[r.ScopeID] = struct{}{}
	}
}

// expectRow is the structural assertion shape used by
// [verifyRow] to spot-check a freshly-INSERTed row.
type expectRow struct {
	repoID       uuid.UUID
	kind         scope.Kind
	sig          string
	firstSeenSHA string
	hasNodeID    bool
	nodeID       uuid.UUID
	attrsJSON    string
}

// verifyRow re-reads a row from the test schema and asserts
// every column matches the expected shape. Catches a writer
// that returns the right shape in memory but writes the wrong
// thing on the wire (e.g. column-shift bugs in the VALUES list).
//
// `created_at` is asserted to be populated AND within a
// reasonable window of the current wall clock (the writer emits
// `NOW()` inline so the column reflects the server-side wall
// clock at INSERT time; addressing evaluator iter-2 #1).
func verifyRow(t *testing.T, db *sql.DB, schema string, scopeID uuid.UUID, exp expectRow) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stmt := fmt.Sprintf(`SELECT repo_id, scope_kind, canonical_signature, first_seen_sha,
		agent_memory_node_id, attrs_json, created_at FROM %s.scope_binding WHERE scope_id = $1`, schema)
	var (
		repoIDText string
		kindText   string
		sigText    string
		firstSHA   string
		nodeIDText sql.NullString
		attrsJSONB []byte
		createdAt  time.Time
	)
	if err := db.QueryRowContext(ctx, stmt, scopeID.String()).Scan(
		&repoIDText, &kindText, &sigText, &firstSHA, &nodeIDText, &attrsJSONB, &createdAt); err != nil {
		t.Fatalf("verifyRow scope_id=%s: %v", scopeID, err)
	}
	if got, want := repoIDText, exp.repoID.String(); got != want {
		t.Errorf("repo_id = %s, want %s", got, want)
	}
	if got, want := kindText, string(exp.kind); got != want {
		t.Errorf("scope_kind = %s, want %s", got, want)
	}
	if got, want := sigText, exp.sig; got != want {
		t.Errorf("canonical_signature = %q, want %q", got, want)
	}
	if got, want := firstSHA, exp.firstSeenSHA; got != want {
		t.Errorf("first_seen_sha = %q, want %q", got, want)
	}
	if exp.hasNodeID {
		if !nodeIDText.Valid {
			t.Error("agent_memory_node_id = NULL, want non-NULL")
		} else if got, want := nodeIDText.String, exp.nodeID.String(); got != want {
			t.Errorf("agent_memory_node_id = %s, want %s", got, want)
		}
	} else if nodeIDText.Valid {
		t.Errorf("agent_memory_node_id = %s, want NULL", nodeIDText.String)
	}
	if exp.attrsJSON != "" {
		// PostgreSQL re-formats jsonb so an empty {} stays "{}"
		// but a populated object may re-pretty-print. Compare
		// canonical forms via Unmarshal to dodge spacing.
		var gotV, wantV any
		if err := json.Unmarshal(attrsJSONB, &gotV); err != nil {
			t.Errorf("attrs_json (%q) is not valid JSON: %v", string(attrsJSONB), err)
		}
		if err := json.Unmarshal([]byte(exp.attrsJSON), &wantV); err != nil {
			t.Errorf("expected attrs_json (%q) is not valid JSON: %v", exp.attrsJSON, err)
		}
		if !jsonEqual(gotV, wantV) {
			t.Errorf("attrs_json = %s, want %s", string(attrsJSONB), exp.attrsJSON)
		}
	}
	// `created_at` is writer-owned (the INSERT emits `NOW()`
	// inline). Assert it is populated AND within a generous
	// window of the test's wall clock so a column-shift bug
	// that put e.g. the zero value in this slot is caught.
	if createdAt.IsZero() {
		t.Error("created_at is zero -- writer did not populate the column")
	}
	now := time.Now()
	if createdAt.After(now.Add(10 * time.Second)) {
		t.Errorf("created_at = %s is more than 10s in the future of now=%s -- server clock skew or column-shift bug", createdAt, now)
	}
	if createdAt.Before(now.Add(-1 * time.Hour)) {
		t.Errorf("created_at = %s is more than 1h before now=%s -- writer used a stale clock or read the wrong column", createdAt, now)
	}
}

// jsonEqual compares two decoded JSON values structurally (so
// `{"a":1}` matches `{ "a" : 1 }`). Sufficient for the small
// flat attrs_json shapes the tests use.
func jsonEqual(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// TestScopeBindingWriter_BatchSameKeyDifferentSHAs covers the
// intra-batch G2 violation flagged by evaluator iter-1 #3: two
// candidates sharing `(repo_id, scope_kind, canonical_signature)`
// but carrying DIFFERENT CurrentSHA values would, in the iter-1
// implementation, derive two different scope_ids (because
// first_seen_sha is in the UUIDv5 pre-image) and both INSERT
// (because the natural-key UNIQUE includes first_seen_sha and
// so doesn't fire). Result: two scope_binding rows for one
// logical scope.
//
// The iter-2 fix: the intra-batch dedupe pass groups by
// natural key BEFORE deriving scope_ids and picks the FIRST
// occurrence's CurrentSHA as the first_seen_sha for the whole
// group. Both result rows MUST resolve to the same scope_id;
// only ONE row MUST land; the SHADivergences counter MUST
// reflect the intra-batch sibling that disagreed on the SHA.
func TestScopeBindingWriter_BatchSameKeyDifferentSHAs(t *testing.T) {
	// NOT t.Parallel(): see TestScopeBindingWriter_InsertNewRows.
	db, w, ok := openScopeBindingWriter(t)
	if !ok {
		return
	}
	seedRepo(t, db, scopeBindingTestSchemaName, defaultTestRepoID)

	ctx := context.Background()
	sig := mustBuildMethod(t, "https://github.com/acme/repo", "src/race.go", "pkg.Race.same", nil)
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)

	candA := ScopeBindingCandidate{
		RepoID:             defaultTestRepoID,
		Kind:               scope.KindMethod,
		CanonicalSignature: sig,
		CurrentSHA:         shaA,
	}
	candB := candA
	candB.CurrentSHA = shaB

	result, err := w.Write(ctx, []ScopeBindingCandidate{candA, candB})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := len(result.Rows), 2; got != want {
		t.Fatalf("len(Rows) = %d, want %d", got, want)
	}
	// G2 invariant: same logical scope, same scope_id.
	if result.Rows[0].ScopeID != result.Rows[1].ScopeID {
		t.Errorf("two candidates with same natural key resolved to DIFFERENT scope_ids: %s vs %s -- G2 violated",
			result.Rows[0].ScopeID, result.Rows[1].ScopeID)
	}
	// Winner's SHA wins (first occurrence).
	if got, want := result.Rows[0].FirstSeenSHA, shaA; got != want {
		t.Errorf("Rows[0].FirstSeenSHA = %s, want %s (winner's SHA)", got, want)
	}
	if got, want := result.Rows[1].FirstSeenSHA, shaA; got != want {
		t.Errorf("Rows[1].FirstSeenSHA = %s, want %s (sibling reuses winner's SHA)", got, want)
	}
	// Inserted accounting: ONE row was minted; the second was
	// folded into the winner.
	if got, want := result.Inserted, 1; got != want {
		t.Errorf("Inserted = %d, want %d", got, want)
	}
	if got, want := result.ReusedExisting, 1; got != want {
		t.Errorf("ReusedExisting = %d, want %d", got, want)
	}
	// Sibling's CurrentSHA disagrees with the persisted SHA;
	// SHADivergences exposes this as a producer-side signal.
	if got, want := result.SHADivergences, 1; got != want {
		t.Errorf("SHADivergences = %d, want %d", got, want)
	}
	// Only ONE row landed in the DB -- this is the load-bearing
	// assertion. Without the iter-2 fix this would be 2.
	if got, want := countByNaturalKey(t, db, defaultTestRepoID, scope.KindMethod, sig), 1; got != want {
		t.Errorf("DB row count for one logical scope = %d, want %d -- G2 violated by writer", got, want)
	}
}

// TestScopeBindingWriter_ConcurrentRaceDifferentSHAs covers the
// cross-process G2 violation flagged by evaluator iter-1 #4:
// two writers race on a BRAND-NEW natural key at DIFFERENT
// CurrentSHAs. In the iter-1 implementation, both SELECT-miss,
// both derive different scope_ids (different SHA in the
// UUIDv5 pre-image), both INSERT (ON CONFLICT (scope_id) does
// not fire because the UUIDs differ; natural-key UNIQUE does
// not fire because the first_seen_sha column is part of it).
// Result: two scope_binding rows for one logical scope.
//
// The iter-2 fix: the writer takes a per-repo
// `pg_advisory_xact_lock` and re-SELECTs the natural key
// INSIDE the lock. The loser's re-SELECT finds the winner's
// row and reuses its scope_id / first_seen_sha. Exactly ONE
// row lands; both writers' result rows agree on scope_id; the
// loser sees AlreadyExisted=true with the winner's
// first_seen_sha.
func TestScopeBindingWriter_ConcurrentRaceDifferentSHAs(t *testing.T) {
	// NOT t.Parallel(): see TestScopeBindingWriter_InsertNewRows.
	db, w, ok := openScopeBindingWriter(t)
	if !ok {
		return
	}
	seedRepo(t, db, scopeBindingTestSchemaName, defaultTestRepoID)

	const concurrency = 8
	sig := mustBuildMethod(t, "https://github.com/acme/repo", "src/concurrent.go", "pkg.Concurrent.race", nil)

	// Each goroutine offers a DISTINCT CurrentSHA for the SAME
	// natural key so iter-1's race manifests as two distinct
	// scope_ids. With the iter-2 fix only one writer can win
	// and the others must converge on its scope_id.
	type outcome struct {
		scopeID      uuid.UUID
		firstSeenSHA string
		alreadyExisted bool
		err          error
	}
	results := make([]outcome, concurrency)
	start := make(chan struct{})
	done := make(chan int, concurrency)

	for g := 0; g < concurrency; g++ {
		go func(idx int) {
			cand := ScopeBindingCandidate{
				RepoID:             defaultTestRepoID,
				Kind:               scope.KindMethod,
				CanonicalSignature: sig,
				// Each goroutine has its own unique SHA so any
				// "two different scope_ids land" bug is loud.
				CurrentSHA: fmt.Sprintf("%040x", idx+1),
			}
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := w.Write(ctx, []ScopeBindingCandidate{cand})
			if err != nil {
				results[idx] = outcome{err: err}
				done <- idx
				return
			}
			results[idx] = outcome{
				scopeID:        res.Rows[0].ScopeID,
				firstSeenSHA:   res.Rows[0].FirstSeenSHA,
				alreadyExisted: res.Rows[0].AlreadyExisted,
			}
			done <- idx
		}(g)
	}
	close(start)
	for i := 0; i < concurrency; i++ {
		<-done
	}

	for i, o := range results {
		if o.err != nil {
			t.Fatalf("goroutine %d: Write returned error %v -- with the per-repo advisory lock no caller should see a 23505 surface", i, o.err)
		}
	}

	// Every writer MUST converge on the same scope_id.
	first := results[0].scopeID
	for i, o := range results {
		if o.scopeID != first {
			t.Errorf("goroutine %d resolved scope_id %s, want %s (must agree with goroutine 0 per G2)", i, o.scopeID, first)
		}
	}
	// Exactly ONE row in the DB. This is the load-bearing G2
	// assertion the iter-1 race violated.
	if got, want := countByNaturalKey(t, db, defaultTestRepoID, scope.KindMethod, sig), 1; got != want {
		t.Errorf("DB row count for one logical scope after %d concurrent writers = %d, want %d -- G2 violated by writer", concurrency, got, want)
	}
	// The persisted first_seen_sha matches some goroutine's
	// CurrentSHA (the winner). Every writer reports that SAME
	// first_seen_sha back via Rows[0].FirstSeenSHA.
	persistedSHA := results[0].firstSeenSHA
	for i, o := range results {
		if o.firstSeenSHA != persistedSHA {
			t.Errorf("goroutine %d reported first_seen_sha %s, want %s", i, o.firstSeenSHA, persistedSHA)
		}
	}
	// Exactly one writer is the minter (AlreadyExisted=false);
	// the others observed a racer winner.
	minters := 0
	for _, o := range results {
		if !o.alreadyExisted {
			minters++
		}
	}
	if minters != 1 {
		t.Errorf("AlreadyExisted=false count = %d across %d goroutines, want 1", minters, concurrency)
	}
}

// countByNaturalKey counts `scope_binding` rows matching the
// given natural key. Used to assert the writer never produces
// two rows for the same `(repo_id, scope_kind, canonical_signature)`.
func countByNaturalKey(t *testing.T, db *sql.DB, repoID uuid.UUID, kind scope.Kind, sig string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stmt := fmt.Sprintf(`SELECT count(*) FROM %s.scope_binding
		WHERE repo_id = $1 AND scope_kind = $2::%s.scope_kind AND canonical_signature = $3`,
		scopeBindingTestSchemaName, scopeBindingTestSchemaName)
	var n int
	if err := db.QueryRowContext(ctx, stmt, repoID.String(), string(kind), sig).Scan(&n); err != nil {
		t.Fatalf("countByNaturalKey: %v", err)
	}
	return n
}

// mustBuildRepo / mustBuildFile / ... are test-only wrappers
// that hide the per-helper error return so the test fixtures
// stay terse. Each panics-via-t.Fatalf on the validation error
// path, which is what we want for hard-coded literal inputs.

func mustBuildRepo(t *testing.T, url string) string {
	t.Helper()
	s, err := scope.BuildRepo(url)
	if err != nil {
		t.Fatalf("BuildRepo: %v", err)
	}
	return s
}

func mustBuildFile(t *testing.T, url, rel string) string {
	t.Helper()
	s, err := scope.BuildFile(url, rel)
	if err != nil {
		t.Fatalf("BuildFile: %v", err)
	}
	return s
}

func mustBuildClass(t *testing.T, url, rel, qn string) string {
	t.Helper()
	s, err := scope.BuildClass(url, rel, qn)
	if err != nil {
		t.Fatalf("BuildClass: %v", err)
	}
	return s
}

func mustBuildInterface(t *testing.T, url, rel, qn string) string {
	t.Helper()
	s, err := scope.BuildInterface(url, rel, qn)
	if err != nil {
		t.Fatalf("BuildInterface: %v", err)
	}
	return s
}

func mustBuildMethod(t *testing.T, url, rel, qn string, params []string) string {
	t.Helper()
	s, err := scope.BuildMethod(url, rel, qn, params)
	if err != nil {
		t.Fatalf("BuildMethod: %v", err)
	}
	return s
}

func mustBuildPackage(t *testing.T, url, dir string) string {
	t.Helper()
	s, err := scope.BuildPackage(url, dir)
	if err != nil {
		t.Fatalf("BuildPackage: %v", err)
	}
	return s
}

// TestScopeBindingWriter_CreatedAtPopulated pins evaluator
// iter-2 #1: the writer's INSERT carries `created_at` as an
// explicit column (filled by the inline `NOW()` SQL literal,
// NOT the table DEFAULT), so a future edit that drops the
// column from the INSERT will be caught even if the DB DEFAULT
// is still in place. Asserts the column is populated AND lands
// within a narrow window of the test's wall clock (catches a
// column-shift bug that would put the wrong value in this
// slot, e.g. the epoch).
//
// Additionally re-reads the row a second time after a brief
// delay and asserts the `created_at` value is byte-identical
// (G3 append-only: the writer never UPDATEs an existing row).
func TestScopeBindingWriter_CreatedAtPopulated(t *testing.T) {
	// NOT t.Parallel(): see TestScopeBindingWriter_InsertNewRows.
	db, w, ok := openScopeBindingWriter(t)
	if !ok {
		return
	}
	seedRepo(t, db, scopeBindingTestSchemaName, defaultTestRepoID)

	ctx := context.Background()
	sig := mustBuildMethod(t, "https://github.com/acme/repo", "src/created_at.go", "pkg.Stamped.fn", nil)
	before := time.Now().Add(-1 * time.Second) // tolerate small client/server clock drift
	cand := ScopeBindingCandidate{
		RepoID:             defaultTestRepoID,
		Kind:               scope.KindMethod,
		CanonicalSignature: sig,
		CurrentSHA:         strings.Repeat("c", 40),
	}
	result, err := w.Write(ctx, []ScopeBindingCandidate{cand})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	after := time.Now().Add(1 * time.Second)
	scopeID := result.Rows[0].ScopeID

	// Read the freshly-INSERTed row's created_at directly.
	stmt := fmt.Sprintf(`SELECT created_at FROM %s.scope_binding WHERE scope_id = $1`, scopeBindingTestSchemaName)
	var createdAt time.Time
	if err := db.QueryRowContext(ctx, stmt, scopeID.String()).Scan(&createdAt); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if createdAt.IsZero() {
		t.Fatal("created_at = zero value -- writer did not populate the column")
	}
	if createdAt.Before(before) {
		t.Errorf("created_at = %s is before the test's `before` checkpoint %s -- column-shift or stale clock", createdAt, before)
	}
	if createdAt.After(after) {
		t.Errorf("created_at = %s is after the test's `after` checkpoint %s -- column-shift or future clock", createdAt, after)
	}

	// G3 append-only: a second observation must NOT change
	// created_at. Write the same candidate again and re-read.
	time.Sleep(50 * time.Millisecond)
	if _, err := w.Write(ctx, []ScopeBindingCandidate{cand}); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	var createdAt2 time.Time
	if err := db.QueryRowContext(ctx, stmt, scopeID.String()).Scan(&createdAt2); err != nil {
		t.Fatalf("re-read created_at: %v", err)
	}
	if !createdAt.Equal(createdAt2) {
		t.Errorf("created_at changed between writes: first=%s, second=%s -- G3 append-only violated (writer must NOT UPDATE existing rows)",
			createdAt, createdAt2)
	}
}

// TestScopeBindingWriter_ChunkingBoundary pins evaluator
// iter-2 #3: the writer must chunk its lookup and insert SQL
// statements so the bound-parameter count never approaches
// PostgreSQL's 65535-parameter ceiling. With 7 params per row
// the per-statement ceiling is 9362 rows, and the writer's
// default insert chunk is well below that.
//
// To exercise the chunk boundary without staging tens of
// thousands of rows per test, this test temporarily drops the
// chunk sizes to small values (insert=37, lookup=29 -- both
// PRIME so chunk boundaries don't accidentally align with the
// total batch size) and writes a batch large enough to force
// MANY chunks on both the unlocked initial lookup AND the
// fresh INSERT. The assertions confirm:
//   - every candidate's scope_id matches `scope.DeriveScopeID`
//     (no chunk-boundary scope_id drift);
//   - exactly the expected number of rows lands in the DB;
//   - a SECOND Write() with the same candidates resolves
//     entirely from the chunked lookup path, returning zero
//     fresh INSERTs (exercises the lookup-chunk merge path);
//   - no chunk's SQL statement exceeds the parameter ceiling
//     (the in-helper [pgMaxBindParameters] guard would surface
//     a precise error here if the chunk size were misset).
func TestScopeBindingWriter_ChunkingBoundary(t *testing.T) {
	// NOT t.Parallel(): see TestScopeBindingWriter_InsertNewRows.
	// ALSO: swaps package-level chunk-size vars; running in
	// parallel with another live test that depends on the
	// defaults would corrupt that test's run.
	db, w, ok := openScopeBindingWriter(t)
	if !ok {
		return
	}
	seedRepo(t, db, scopeBindingTestSchemaName, defaultTestRepoID)

	savedInsert, savedLookup := scopeBindingInsertChunkSize, scopeBindingLookupChunkSize
	scopeBindingInsertChunkSize = 37
	scopeBindingLookupChunkSize = 29
	t.Cleanup(func() {
		scopeBindingInsertChunkSize = savedInsert
		scopeBindingLookupChunkSize = savedLookup
	})

	// 300 rows / 37 per insert chunk = 9 chunks (last chunk
	// partial -- exercises the trailing-chunk path).
	// 300 rows / 29 per lookup chunk = 11 chunks (also partial).
	// Both chunk counts are > 1 so multi-chunk fan-out is
	// genuinely exercised.
	const total = 300
	ctx := context.Background()
	sha := strings.Repeat("9", 40)
	cands := make([]ScopeBindingCandidate, total)
	for i := 0; i < total; i++ {
		sig := mustBuildMethod(t,
			"https://github.com/acme/repo",
			fmt.Sprintf("src/chunk_%04d.go", i),
			fmt.Sprintf("pkg.Chunk_%04d.fn", i),
			nil)
		cands[i] = ScopeBindingCandidate{
			RepoID:             defaultTestRepoID,
			Kind:               scope.KindMethod,
			CanonicalSignature: sig,
			CurrentSHA:         sha,
		}
	}

	// First call: every candidate is fresh; expect `total`
	// INSERTs across N chunks.
	first, err := w.Write(ctx, cands)
	if err != nil {
		t.Fatalf("first Write across chunks: %v", err)
	}
	if first.Inserted != total {
		t.Errorf("first Write Inserted = %d, want %d", first.Inserted, total)
	}
	if len(first.Rows) != total {
		t.Fatalf("first Write len(Rows) = %d, want %d", len(first.Rows), total)
	}
	for i, r := range first.Rows {
		want, err := scope.DeriveScopeID(r.Candidate.RepoID, r.Candidate.Kind, r.Candidate.CanonicalSignature, r.Candidate.CurrentSHA)
		if err != nil {
			t.Fatalf("derive[%d]: %v", i, err)
		}
		if r.ScopeID != want {
			t.Errorf("Rows[%d].ScopeID = %s, want %s -- chunk-boundary scope_id drift", i, r.ScopeID, want)
		}
		if r.AlreadyExisted {
			t.Errorf("Rows[%d].AlreadyExisted = true on first write, want false", i)
		}
		if r.FirstSeenSHA != sha {
			t.Errorf("Rows[%d].FirstSeenSHA = %q, want %q", i, r.FirstSeenSHA, sha)
		}
	}

	// Confirm DB row count: exactly `total` rows landed.
	var count int
	if err := db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT count(*) FROM %s.scope_binding WHERE first_seen_sha = $1`,
		scopeBindingTestSchemaName), sha).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != total {
		t.Errorf("DB row count = %d, want %d -- chunked INSERT lost rows", count, total)
	}

	// Second call: every candidate is already present;
	// exercises the multi-chunk LOOKUP path. Expect zero new
	// INSERTs and `total` reused.
	second, err := w.Write(ctx, cands)
	if err != nil {
		t.Fatalf("second Write across chunks: %v", err)
	}
	if second.Inserted != 0 {
		t.Errorf("second Write Inserted = %d, want 0", second.Inserted)
	}
	if second.ReusedExisting != total {
		t.Errorf("second Write ReusedExisting = %d, want %d", second.ReusedExisting, total)
	}
	for i, r := range second.Rows {
		if !r.AlreadyExisted {
			t.Errorf("second Write Rows[%d].AlreadyExisted = false, want true", i)
		}
		if r.ScopeID != first.Rows[i].ScopeID {
			t.Errorf("second Write Rows[%d].ScopeID drifted across calls: first=%s second=%s",
				i, first.Rows[i].ScopeID, r.ScopeID)
		}
	}
}

// TestScopeBindingWriter_ChunkBoundaryParamCeilingGuard pins
// the in-helper [pgMaxBindParameters] guard in
// `insertFreshChunk`: temporarily raising the chunk size
// past the ceiling MUST surface a precise pre-flight error
// rather than letting the driver emit a confusing "got N
// parameters, expected at most 65535". Direct unit-style
// invocation of the helper -- no live PG round-trip needed.
func TestScopeBindingWriter_ChunkBoundaryParamCeilingGuard(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("postgres", "host=invalid-host-that-must-not-resolve.example")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	w, err := NewScopeBindingWriterWithSchema(db, "any_schema")
	if err != nil {
		t.Fatalf("NewScopeBindingWriterWithSchema: %v", err)
	}

	// 9363 rows * 7 params/row = 65541 > 65535 ceiling. The
	// pre-flight guard fires BEFORE we attempt to send the
	// statement, so the broken DB handle is never contacted.
	const overCeiling = 9363
	rows := make([]ScopeBindingResolved, overCeiling)
	for i := range rows {
		rows[i] = ScopeBindingResolved{
			ScopeID: uuid.Must(uuid.NewV4()),
			Candidate: ScopeBindingCandidate{
				RepoID:             defaultTestRepoID,
				Kind:               scope.KindFile,
				CanonicalSignature: fmt.Sprintf("sig-%d", i),
			},
			FirstSeenSHA: strings.Repeat("0", 40),
		}
	}
	_, err = w.insertFreshChunk(context.Background(), db, rows)
	if err == nil {
		t.Fatal("insertFreshChunk: err = nil; want bound-parameter ceiling guard to fire")
	}
	if !strings.Contains(err.Error(), "exceeds PostgreSQL bound-parameter ceiling") {
		t.Errorf("insertFreshChunk error message mismatch: got %q, want one mentioning the bound-parameter ceiling", err.Error())
	}
}
