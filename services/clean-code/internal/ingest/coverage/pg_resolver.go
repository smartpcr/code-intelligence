package coverage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// ErrPGScopeResolverNilDB is returned by [NewPGScopeResolver]
// when its `*sql.DB` argument is nil. A nil DB is always a
// composition-root wiring bug; surfacing the failure at
// construction (not at first request) keeps the error
// pointed at the missing seam.
var ErrPGScopeResolverNilDB = errors.New("coverage: NewPGScopeResolver: *sql.DB is nil")

// ErrPGScopeResolverNilURLLookup mirrors
// [ErrPGScopeResolverNilDB] for the [RepoURLLookupFunc]
// argument. The lookup is REQUIRED -- the natural key for
// `scope_binding` is `(repo_id, scope_kind,
// canonical_signature)` and the canonical signature
// embeds the repo's URL (see [scope.BuildFile]). Without a
// URL the resolver could only synthesise a placeholder,
// breaking G2 stability across services.
var ErrPGScopeResolverNilURLLookup = errors.New("coverage: NewPGScopeResolver: RepoURLLookupFunc is nil")

// ErrPGScopeResolverEmptySchema is returned by
// [NewPGScopeResolverWithSchema] when its `schema` argument
// is empty. The default constructor [NewPGScopeResolver]
// substitutes the canonical `clean_code` schema; this
// error is only reachable from the schema-explicit
// constructor.
var ErrPGScopeResolverEmptySchema = errors.New("coverage: NewPGScopeResolverWithSchema: schema is empty")

// RepoURLLookupFunc is the function shape the
// [PGScopeResolver] uses to convert a `repo_id` into the
// operator-registered repo URL it needs to build the
// `scope_binding.canonical_signature` key. Structurally
// compatible with the [metric_ingestor.RepoURLLookup]
// interface's `LookupRepoURL` method -- the composition
// root passes `metric_ingestor.NewPGRepoURLLookup(db).LookupRepoURL`
// directly. The function-type seam lets this package stay
// out of the metric_ingestor import path (the reverse
// direction is already taken by [coverage_sweep.go]).
//
// Contract:
//
//   - Returns `(url, nil)` on success.
//   - Returns `("", err)` when the row is missing, the URL
//     is empty, or infrastructure failed. The error is
//     wrapped into [ErrScopeResolutionFailed] by the
//     resolver so the verb classifier maps it to 422 /
//     SCOPE_RESOLUTION_FAILED.
//
// Implementations MUST be safe for concurrent use; the
// resolver invokes the lookup once per `ResolveFileScope`
// call, which is once per file in the uploaded payload.
type RepoURLLookupFunc func(ctx context.Context, repoID uuid.UUID) (string, error)

// PGScopeResolver is the production [ScopeResolver]: a
// READ-ONLY lookup against `clean_code.scope_binding`
// (architecture Sec 5.2.3). For each (repoID, filePath)
// pair the resolver:
//
//  1. Looks up the operator-registered repo URL via
//     [RepoURLLookupFunc] (cached at the lookup
//     implementation's layer).
//  2. Builds the canonical signature
//     `<repoURL>::file::<filePath>` via [scope.BuildFile].
//  3. SELECTs `scope_id` from `scope_binding` on the
//     natural key
//     `(repo_id, scope_kind='file', canonical_signature)`.
//  4. Returns `(scope_id, ref, true, nil)` on match,
//     `(uuid.Nil, _, false, nil)` on no match (the skip-
//     and-count path the
//     `coverage_skipped_unbound_scope` counter relies on),
//     and `(_, _, false, err)` on infrastructure failure.
//
// # Why read-only (no auto-mint)
//
// The implementation-plan Stage 4.2 brief mandates "skip
// the row and log a `coverage_skipped_unbound_scope`
// counter (do NOT invent a scope)". Auto-minting a
// scope_binding row from coverage data would break the
// G2 stability contract -- the canonical signature for a
// file scope is established by the foundation pipeline
// (AST adapter + scope_binding writer); a coverage
// publisher MUST NOT race with that pipeline to mint the
// row first.
//
// # SHA argument
//
// The `sha` parameter is the upload SHA. It is NOT used
// in the natural key -- scope identity is SHA-stable per
// the G2 invariant (architecture Sec 5.2.3 line 1044) --
// but it IS passed through to the returned [recipes.ScopeRef]
// so downstream loggers can correlate the resolver's
// outcome to the parent ScanRun's SHA.
//
// # Concurrent use
//
// Safe for concurrent use. The `*sql.DB` connection pool
// is concurrent-safe; the [RepoURLLookupFunc] contract
// requires concurrent safety from the implementation.
type PGScopeResolver struct {
	db     *sql.DB
	schema string
	urls   RepoURLLookupFunc
}

// NewPGScopeResolver wraps `db` using the canonical
// [storage.SchemaName] (`clean_code`). Returns an error
// if `db` or `urls` is nil.
func NewPGScopeResolver(db *sql.DB, urls RepoURLLookupFunc) (*PGScopeResolver, error) {
	return NewPGScopeResolverWithSchema(db, "clean_code", urls)
}

// NewPGScopeResolverWithSchema is the test-friendly
// constructor that lets callers inject an isolated PG
// schema (e.g. `clean_code_coverage_test`). Production
// callers should prefer [NewPGScopeResolver].
func NewPGScopeResolverWithSchema(db *sql.DB, schema string, urls RepoURLLookupFunc) (*PGScopeResolver, error) {
	if db == nil {
		return nil, ErrPGScopeResolverNilDB
	}
	if schema == "" {
		return nil, ErrPGScopeResolverEmptySchema
	}
	if urls == nil {
		return nil, ErrPGScopeResolverNilURLLookup
	}
	return &PGScopeResolver{db: db, schema: schema, urls: urls}, nil
}

// ResolveFileScope implements [ScopeResolver] by SELECTing
// `scope_id` FROM `<schema>.scope_binding` WHERE
// `(repo_id, scope_kind='file', canonical_signature)`
// matches. The canonical signature is built via
// [scope.BuildFile] from the per-repo URL resolved via
// the configured [RepoURLLookupFunc].
//
// Missing scope: returns `(uuid.Nil, _, false, nil)` so
// the hydrator's skip-and-count path runs.
//
// Infrastructure / URL lookup failure: returns
// `(_, _, false, err)` so the hydrator aborts the upload
// rather than silently dropping rows. The wrapping
// [ErrScopeResolutionFailed] is applied by the hydrator
// so this resolver returns the bare underlying error.
func (r *PGScopeResolver) ResolveFileScope(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
	filePath string,
) (uuid.UUID, recipes.ScopeRef, bool, error) {
	if r == nil {
		return uuid.Nil, recipes.ScopeRef{}, false, errors.New("coverage: PGScopeResolver.ResolveFileScope on nil receiver")
	}
	if repoID == uuid.Nil {
		return uuid.Nil, recipes.ScopeRef{}, false, errors.New("coverage: PGScopeResolver.ResolveFileScope: repoID is the zero UUID")
	}
	if filePath == "" {
		return uuid.Nil, recipes.ScopeRef{}, false, errors.New("coverage: PGScopeResolver.ResolveFileScope: filePath is empty")
	}

	repoURL, err := r.urls(ctx, repoID)
	if err != nil {
		return uuid.Nil, recipes.ScopeRef{}, false, fmt.Errorf("coverage: PGScopeResolver: repo URL lookup for repo_id=%s: %w", repoID, err)
	}
	signature, err := scope.BuildFile(repoURL, filePath)
	if err != nil {
		return uuid.Nil, recipes.ScopeRef{}, false, fmt.Errorf("coverage: PGScopeResolver: build canonical signature for %q: %w", filePath, err)
	}

	stmt := fmt.Sprintf(`
		SELECT scope_id
		  FROM %s.scope_binding
		 WHERE repo_id = $1::uuid
		   AND scope_kind = $2::%s.scope_kind
		   AND canonical_signature = $3::text
		 LIMIT 1`,
		pq.QuoteIdentifier(r.schema), pq.QuoteIdentifier(r.schema))

	var scopeIDText string
	err = r.db.QueryRowContext(ctx, stmt, repoID.String(), string(scope.KindFile), signature).Scan(&scopeIDText)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Documented missing-binding path: hydrator
			// skips and increments
			// `coverage_skipped_unbound_scope`.
			return uuid.Nil, recipes.ScopeRef{}, false, nil
		}
		return uuid.Nil, recipes.ScopeRef{}, false, fmt.Errorf("coverage: PGScopeResolver: query scope_binding for repo_id=%s file_path=%q: %w", repoID, filePath, err)
	}

	scopeID, err := uuid.FromString(scopeIDText)
	if err != nil {
		return uuid.Nil, recipes.ScopeRef{}, false, fmt.Errorf("coverage: PGScopeResolver: parse scope_id %q from scope_binding row: %w", scopeIDText, err)
	}

	// The returned ref carries the writer-side LocalID so
	// downstream consumers can round-trip the durable id
	// without re-querying. `sha` is passed through unused
	// for now -- documented in the type comment as the
	// upload SHA correlation field.
	_ = sha
	return scopeID, recipes.ScopeRef{
		LocalID:       scopeID.String(),
		Kind:          scope.KindFile,
		QualifiedName: filePath,
		Path:          filePath,
	}, true, nil
}

// Compile-time interface assertion: a signature drift in
// [ScopeResolver] surfaces at build time, not at first
// request.
var _ ScopeResolver = (*PGScopeResolver)(nil)
