package spaningestor

// PG-backed `Lookup` implementation. The binary
// (cmd/span-ingestor/main.go) wires this with the read-only
// pool authenticated as `agent_memory_ro`. The resolver uses
// only three queries; we hand-code them here so we don't have
// to expand the `graphreader.Reader` public surface for the
// Span Ingestor's specific lookups.
//
// Why direct SQL (not graphreader)
// --------------------------------
//   * `graphreader.Reader` is the centralized read library, but
//     its current surface (Stage 2.2) is focused on the recall
//     path: full Node hydration, neighborhood walks, embedding
//     publish-state reads. The Span Ingestor needs only three
//     narrow queries over `node.canonical_signature` /
//     `attrs_json` — adding them to graphreader would force a
//     larger packaging change in the read library than this
//     stage warrants.
//   * Keeping them here also localizes the query coupling: any
//     change to the canonical-signature format
//     (`<url>::method::<relPath>#<qualifiedName>(<params>)`,
//     see `repoindexer/ast/dispatcher.go.methodSignature`) is
//     visible alongside the resolver code that consumes it.
//
// Schema coupling
// ---------------
// All three queries rely on the §3.3 contract that Method
// Nodes carry `start_line` / `end_line` / `params_raw` in
// `attrs_json` (mirrored in `repoindexer/ast/dispatcher.go.methodAttrs`).
// A breaking change there should be paired with a change here;
// the integration test in `pglookup_integration_test.go`
// validates the coupling against the real schema.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PGLookup implements Lookup over a *sql.DB. Construct via
// NewPGLookup.
type PGLookup struct {
	db *sql.DB
}

// NewPGLookup constructs a PGLookup. The supplied *sql.DB must
// have SELECT on `node`; `agent_memory_ro` satisfies this.
func NewPGLookup(db *sql.DB) *PGLookup {
	if db == nil {
		panic("spaningestor: NewPGLookup: nil *sql.DB")
	}
	return &PGLookup{db: db}
}

// LookupMethodsByName implements Lookup.
//
// Strategy: the canonical signature for a Method ends in
// `#<qualifiedName>(<params>)` where `qualifiedName` is
// `<Namespace>.<Function>`. We match on the suffix
// `#<namespace>.<function>(` (literal — `LIKE` with the
// surrounding `%` so the relPath / repo URL are wildcards).
//
// User-supplied segments (`namespace`, `function`) flow in
// from OTel span attributes and routinely contain LIKE
// metacharacters — `_` is endemic in identifiers (`my_method`,
// `__init__`) and `%` can appear in URL-encoded paths or
// generated names. We therefore escape `\`, `%`, `_` in the
// interpolated segments and pin the escape character with
// `ESCAPE '\'`. Without this, `my_method` would also match
// `myXmethod`, broadening the candidate set (extra scan + risk
// of wrong-candidate selection downstream).
//
// A more selective index could be built on a generated column
// extracting the qualifiedName from canonical_signature, but
// v1 relies on the (repo_id, kind) index from migration 0003
// to narrow the scan first.
func (l *PGLookup) LookupMethodsByName(
	ctx context.Context, repoID, namespace, function string,
) ([]MethodCandidate, error) {
	// The §8.6 mapping forbids calling LookupMethodsByName
	// with an empty namespace OR function (see resolver.go);
	// defence in depth, fail fast.
	if namespace == "" || function == "" {
		return nil, nil
	}
	qualified := namespace + "." + function
	// `% + #<qualified>(% + %` matches signatures of the form
	// `<url>::method::<relPath>#<namespace>.<function>(...)`.
	// The trailing `(` ensures we don't accept a prefix match
	// (e.g. searching for `foo.bar` does not match `foo.baz`).
	pattern := "%#" + escapeLikePattern(qualified) + `(%`
	const q = `
		SELECT
		    node_id::text,
		    canonical_signature,
		    COALESCE(attrs_json->>'params_raw', '')
		FROM node
		WHERE repo_id = $1
		  AND kind = 'method'
		  AND canonical_signature LIKE $2 ESCAPE '\'
	`
	rows, err := l.db.QueryContext(ctx, q, repoID, pattern)
	if err != nil {
		return nil, fmt.Errorf("spaningestor: LookupMethodsByName: %w", err)
	}
	defer rows.Close()
	var out []MethodCandidate
	for rows.Next() {
		var cand MethodCandidate
		if err := rows.Scan(&cand.NodeID, &cand.CanonicalSignature, &cand.ParamSignature); err != nil {
			return nil, fmt.Errorf("spaningestor: LookupMethodsByName scan: %w", err)
		}
		cand.FilePath = filePathFromSignature(cand.CanonicalSignature)
		out = append(out, cand)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("spaningestor: LookupMethodsByName rows: %w", err)
	}
	return out, nil
}

// LookupMethodByLocation implements Lookup.
//
// Strategy: find Methods in the supplied repo + filepath whose
// `start_line` / `end_line` bracket the supplied lineno.
// Returns the most-specific (smallest line range) one — when
// multiple Methods overlap (rare, but possible with nested
// closures) the smallest range is the most precise.
//
// `filepath` comes from OTel span attributes and frequently
// contains `_` (any non-trivial source tree has underscored
// filenames). We escape `\`, `%`, `_` in the interpolated
// segment and pin `ESCAPE '\'` on the LIKE; otherwise a query
// for `foo_bar.py` would also match `fooXbar.py`, and the
// `LIMIT 1` smallest-range ordering could silently pick the
// wrong file's method.
func (l *PGLookup) LookupMethodByLocation(
	ctx context.Context, repoID, filepath string, lineno int,
) (*MethodCandidate, error) {
	if filepath == "" || lineno <= 0 {
		return nil, nil
	}
	// `::method::<filepath>#` is the exact in-signature segment
	// that pins a Method to its file. `%::method::filepath#%`
	// is the LIKE pattern; the leading `%` covers the
	// arbitrary-length repo URL prefix.
	pattern := "%::method::" + escapeLikePattern(filepath) + "#%"
	const q = `
		SELECT
		    node_id::text,
		    canonical_signature,
		    COALESCE(attrs_json->>'params_raw', ''),
		    COALESCE((attrs_json->>'start_line')::int, 0),
		    COALESCE((attrs_json->>'end_line')::int,   0)
		FROM node
		WHERE repo_id = $1
		  AND kind = 'method'
		  AND canonical_signature LIKE $2 ESCAPE '\'
		  AND COALESCE((attrs_json->>'start_line')::int, 0) <= $3
		  AND COALESCE((attrs_json->>'end_line')::int,   0) >= $3
		ORDER BY (COALESCE((attrs_json->>'end_line')::int, 0)
		          - COALESCE((attrs_json->>'start_line')::int, 0)) ASC
		LIMIT 1
	`
	var cand MethodCandidate
	err := l.db.QueryRowContext(ctx, q, repoID, pattern, lineno).Scan(
		&cand.NodeID,
		&cand.CanonicalSignature,
		&cand.ParamSignature,
		&cand.BodyStartLine,
		&cand.BodyEndLine,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("spaningestor: LookupMethodByLocation: %w", err)
	}
	cand.FilePath = filepath
	return &cand, nil
}

// LookupBlockForMethod implements Lookup.
//
// Strategy: Blocks live under a Method via `node.parent_node_id`
// (the parent_node_idx in migration 0003). Each Block's
// `attrs_json` carries `start_line` / `end_line` / `block_kind`.
// We pick the most specific covering Block (smallest range).
func (l *PGLookup) LookupBlockForMethod(
	ctx context.Context, methodNodeID string, lineno int,
) (*BlockCandidate, error) {
	if methodNodeID == "" || lineno <= 0 {
		return nil, nil
	}
	const q = `
		SELECT
		    node_id::text,
		    canonical_signature,
		    COALESCE(attrs_json->>'block_kind', ''),
		    COALESCE((attrs_json->>'start_line')::int, 0),
		    COALESCE((attrs_json->>'end_line')::int,   0)
		FROM node
		WHERE parent_node_id = $1
		  AND kind = 'block'
		  AND COALESCE((attrs_json->>'start_line')::int, 0) <= $2
		  AND COALESCE((attrs_json->>'end_line')::int,   0) >= $2
		ORDER BY (COALESCE((attrs_json->>'end_line')::int, 0)
		          - COALESCE((attrs_json->>'start_line')::int, 0)) ASC
		LIMIT 1
	`
	var cand BlockCandidate
	err := l.db.QueryRowContext(ctx, q, methodNodeID, lineno).Scan(
		&cand.NodeID,
		&cand.CanonicalSignature,
		&cand.Kind,
		&cand.StartLine,
		&cand.EndLine,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("spaningestor: LookupBlockForMethod: %w", err)
	}
	return &cand, nil
}

// CurrentHeadSHA implements SHAReader (see repo_sha.go).
// Returns the repo's `current_head_sha` (NOT NULL per migration
// 0002). Returns ("", nil) when no row matches the supplied
// repo_id — the ingestor falls back to the "observed" sentinel
// on EdgeInput.FromSHA in that case.
func (l *PGLookup) CurrentHeadSHA(ctx context.Context, repoID string) (string, error) {
	if repoID == "" {
		return "", nil
	}
	const q = `SELECT current_head_sha FROM repo WHERE repo_id = $1`
	var sha string
	err := l.db.QueryRowContext(ctx, q, repoID).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("spaningestor: CurrentHeadSHA: %w", err)
	}
	return sha, nil
}

// filePathFromSignature extracts the relPath segment from a
// Method canonical_signature of the form
// `<url>::method::<relPath>#<qualifiedName>(<params>)`. Returns
// the empty string when the segment is missing; the resolver
// only uses the field for diagnostic logging so a miss is non-
// fatal.
func filePathFromSignature(sig string) string {
	const marker = "::method::"
	i := strings.Index(sig, marker)
	if i < 0 {
		return ""
	}
	rest := sig[i+len(marker):]
	j := strings.Index(rest, "#")
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// escapeLikePattern escapes the SQL LIKE metacharacters `%`
// and `_` (and the escape character `\` itself) in a user-
// supplied segment so it can be safely interpolated into a
// LIKE pattern that already contains literal wildcards. The
// companion query MUST be issued with `LIKE $N ESCAPE '\'`
// (PostgreSQL's default escape character is also `\` when no
// ESCAPE clause is supplied, but pinning it explicitly keeps
// the contract obvious at the call site and survives any
// future change to the server default).
//
// We rune-iterate rather than byte-iterate so multi-byte
// UTF-8 sequences (common in international identifiers and
// path segments) are preserved verbatim — none of the LIKE
// metacharacters are multi-byte, so the byte-level fast path
// would also be correct, but ranging by rune is clearer.
func escapeLikePattern(s string) string {
	if !strings.ContainsAny(s, `\%_`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
