//go:build cgo

// reader.go is the read-half of the SQLite graphsink backend.
// It implements `graphsink.Reader` against the same `*sql.DB`
// the writer half in `sink.go` opens, so a single Sink value
// satisfies BOTH halves of the graphsink contract and the
// diagram projector (`internal/diagram`, Stage 4) + `codeintel
// serve` HTTP handlers (Stage 6) can read the rows the scan
// just wrote without re-opening the database.
//
// SCHEMA ASSUMPTIONS. The reader queries the `repo`,
// `repo_commit`, `node`, and `edge` tables exactly as defined
// in `schema.sql`. None of those tables carry retirement /
// tombstone columns -- the SQLite backend is a scan-snapshot
// store (a full re-scan replaces the file), so the Postgres
// reader's `LEFT JOIN node_retirement` / `IncludeRetired`
// machinery has no equivalent here. `opts.IncludeRetired` is
// silently ignored across every method below; rows are always
// "current" because there is no other state to be in.
//
// ORDERING (S3.6 brief).
//
//   - ListNodes orders by `kind, canonical_signature, node_id`
//     to match the Postgres reader (graphreader/query.go:340).
//   - ListEdgesFrom / ListEdgesTo order by
//     `(kind, dst_node_id, edge_id)`. The first two columns
//     are the workstream brief's literal directive
//     ("ordered by `(kind, dst_node_id)`"); the `edge_id`
//     tail is the deterministic tie-breaker required because
//     ListEdgesTo filters on a fixed `dst_node_id`, which
//     would otherwise leave same-kind rows in undefined order.
//     With the tie-breaker, every same-kind row in any
//     ListEdgesTo / ListEdgesFrom call has a totally ordered
//     position, satisfying the brief's "deterministic order"
//     intent. The Postgres / memory readers' `(kind, edge_id)`
//     ordering is a strict refinement of ours when same-kind
//     rows share dst_node_id, so cross-backend consumers that
//     only depend on "stable per call" semantics observe
//     compatible output. The brief's literal column list and
//     its "match the Postgres reader's deterministic order"
//     rationale contradict each other; this implementation
//     follows the literal columns. See iter-4 open question
//     `edge-order-key` for the operator decision pin.
//
// CONCURRENCY. The Sink pins `*sql.DB` to one connection so
// writes are serialised through the WAL log without
// SQLITE_BUSY storms. Reader calls share that single
// connection. For the local CLI workflow this is fine; if a
// future change needs concurrent reads during a long write,
// the right fix is to open a SEPARATE read-only `*sql.DB`
// handle in `Open` rather than raise the pool limit (raising
// the pool reintroduces the writer-collision hazard the
// single-conn policy is designed to prevent).

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// Compile-time assertion: *Sink satisfies graphsink.Reader as
// well as graphsink.Sink. If a future change widens the Reader
// surface this fails at build time inside the `sqlite` package
// so the gap is caught before the CLI wires the backend in.
var _ graphsink.Reader = (*Sink)(nil)

// nodeKindsAllowed mirrors the closed node_kind set the SQLite
// schema's CHECK constraint enforces (schema.sql line 109) and
// matches the in-memory backend's identical list. Kept as a
// package-local set so `validateNodeKinds` runs O(1) per call.
var nodeKindsAllowed = map[string]struct{}{
	"repo":    {},
	"package": {},
	"file":    {},
	"class":   {},
	"method":  {},
	"block":   {},
}

// edgeKindsAllowed mirrors the closed edge_kind set the SQLite
// schema's CHECK constraint enforces (schema.sql lines 134-138).
var edgeKindsAllowed = map[string]struct{}{
	"contains":       {},
	"imports":        {},
	"static_calls":   {},
	"observed_calls": {},
	"extends":        {},
	"implements":     {},
	"reads":          {},
	"writes":         {},
	"renamed_to":     {},
	"overrides":      {},
}

// validateNodeKinds returns an error when any element is not a
// member of the node_kind closed set. Empty slice = "all kinds"
// and passes. Same contract as `graphreader.validateNodeKinds`
// so a typo in a CLI flag fails identically against either
// backend.
func validateNodeKinds(kinds []string) error {
	for _, k := range kinds {
		if _, ok := nodeKindsAllowed[k]; !ok {
			return fmt.Errorf("graphsink/sqlite: invalid node kind %q "+
				"(allowed: repo/package/file/class/method/block)", k)
		}
	}
	return nil
}

// validateEdgeKinds is the edge analogue of validateNodeKinds.
func validateEdgeKinds(kinds []string) error {
	for _, k := range kinds {
		if _, ok := edgeKindsAllowed[k]; !ok {
			return fmt.Errorf("graphsink/sqlite: invalid edge kind %q "+
				"(allowed: contains/imports/static_calls/observed_calls/"+
				"extends/implements/reads/writes/renamed_to/overrides)", k)
		}
	}
	return nil
}

// normaliseLimit applies the standard limit policy: <=0 or
// > MaxListLimit clamps to MaxListLimit; every other value is
// used as-is. Matches `graphreader.normaliseLimit`.
func normaliseLimit(requested int) int {
	if requested <= 0 || requested > graphreader.MaxListLimit {
		return graphreader.MaxListLimit
	}
	return requested
}

// kindPlaceholders builds the `(?, ?, ?, …)` placeholder block
// for an `IN (...)` clause of length `n`.
func kindPlaceholders(n int) string {
	if n == 0 {
		return ""
	}
	return "?" + strings.Repeat(",?", n-1)
}

// ListRepos returns one RepoSummary per `repo` row. Each row's
// `GeneratedAt` is the MOST RECENT `repo_commit.committed_at`
// for that repo (the diagram envelope's `generatedAt` field --
// see `graphreader.RepoSummary`'s field doc). When a repo has
// NO commits yet (registered but never scanned), GeneratedAt
// falls back to `repo.created_at` so the `GET /api/repos`
// consumer always sees a non-zero timestamp.
//
// The SHA field is taken from `repo.current_head_sha`, matching
// the Postgres mgmt-api handler's projection. The RepoUUID
// field is left empty: the SQLite backend's natural key is the
// `RepoID` (a deterministic UUID derived from the URL via
// `fingerprint.RepoIDFromURL`), and there is no separate
// surrogate-key namespace.
//
// Stable order: `url ASC, repo_id ASC` (URL is the natural
// human-friendly sort; repo_id breaks ties for repos with
// identical normalised URLs, which shouldn't happen but the
// tie-breaker keeps results deterministic).
//
// `opts.IncludeRetired` is silently ignored (SQLite schema has
// no retirement columns). `opts.Limit` is clamped at
// `MaxListLimit`.
func (s *Sink) ListRepos(ctx context.Context, opts graphreader.ReaderOptions) ([]graphreader.RepoSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}

	// LEFT JOIN against the aggregate so a repo with zero
	// commits still produces a row; the COALESCE then picks
	// `repo.created_at` when no commit exists yet.
	const q = `
		SELECT
		    r.repo_id,
		    r.url,
		    r.current_head_sha,
		    COALESCE(c.last_committed_at, r.created_at) AS generated_at_ms
		  FROM repo r
		  LEFT JOIN (
		    SELECT repo_id, MAX(committed_at) AS last_committed_at
		      FROM repo_commit
		     GROUP BY repo_id
		  ) c ON c.repo_id = r.repo_id
		 ORDER BY r.url ASC, r.repo_id ASC
		 LIMIT ?
	`
	rows, err := s.db.QueryContext(ctx, q, normaliseLimit(opts.Limit))
	if err != nil {
		return nil, fmt.Errorf("graphsink/sqlite: ListRepos query: %w", err)
	}
	defer rows.Close()

	var out []graphreader.RepoSummary
	for rows.Next() {
		var (
			repoID         string
			url            string
			currentHeadSHA string
			generatedAtMs  int64
		)
		if err := rows.Scan(&repoID, &url, &currentHeadSHA, &generatedAtMs); err != nil {
			return nil, fmt.Errorf("graphsink/sqlite: ListRepos scan: %w", err)
		}
		out = append(out, graphreader.RepoSummary{
			RepoID:      repoID,
			URL:         url,
			SHA:         currentHeadSHA,
			GeneratedAt: time.UnixMilli(generatedAtMs).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphsink/sqlite: ListRepos rows: %w", err)
	}
	return out, nil
}

// ListNodes enumerates Nodes matching the supplied filters.
// Honours `ListNodesFilter.ParentNodeID`, `Kinds`,
// `CanonicalSignature`, and (for parity with the Postgres
// reader) `FromSHA`. `f.Limit` is clamped at MaxListLimit and
// pushed into the SQL as a `LIMIT ?` so even an unbounded
// caller cannot trip the OOM hazard a large repo would pose.
//
// Stable order: `kind ASC, canonical_signature ASC, node_id
// ASC` matches the Postgres reader's ORDER BY
// (graphreader/query.go:340) so the diagram projector sees the
// same row order regardless of backend.
//
// `opts.IncludeRetired` is silently ignored (no retirement
// columns in the SQLite schema). `opts.Limit` is ignored on
// this method -- callers configure list shape via `f.Limit`.
func (s *Sink) ListNodes(
	ctx context.Context,
	repoID fingerprint.RepoID,
	kinds []string,
	f graphreader.ListNodesFilter,
	opts graphreader.ReaderOptions,
) ([]graphreader.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if repoID.IsZero() {
		return nil, errors.New("graphsink/sqlite: ListNodes: zero repo_id")
	}
	if err := validateNodeKinds(kinds); err != nil {
		return nil, err
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}

	var (
		b    strings.Builder
		args []any
	)
	b.WriteString(`SELECT node_id, fingerprint, repo_id, kind, canonical_signature,
		COALESCE(parent_node_id, ''), from_sha, attrs_json
		FROM node WHERE repo_id = ?`)
	args = append(args, repoID.String())

	if len(kinds) > 0 {
		b.WriteString(" AND kind IN (")
		b.WriteString(kindPlaceholders(len(kinds)))
		b.WriteByte(')')
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	if f.ParentNodeID != "" {
		b.WriteString(" AND parent_node_id = ?")
		args = append(args, f.ParentNodeID)
	}
	if f.FromSHA != "" {
		b.WriteString(" AND from_sha = ?")
		args = append(args, f.FromSHA)
	}
	if f.CanonicalSignature != "" {
		b.WriteString(" AND canonical_signature = ?")
		args = append(args, f.CanonicalSignature)
	}
	b.WriteString(" ORDER BY kind ASC, canonical_signature ASC, node_id ASC LIMIT ?")
	args = append(args, normaliseLimit(f.Limit))

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("graphsink/sqlite: ListNodes query: %w", err)
	}
	defer rows.Close()

	var out []graphreader.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphsink/sqlite: ListNodes rows: %w", err)
	}
	return out, nil
}

// ListEdgesFrom returns every outbound Edge from srcNodeID
// matching `kinds`. Empty `kinds` = all kinds.
//
// Order: `(kind ASC, dst_node_id ASC, edge_id ASC)` -- the
// brief's literal `(kind, dst_node_id)` plus an `edge_id`
// tie-breaker so the result is totally ordered even when
// multiple rows share both `kind` and `dst_node_id`.
func (s *Sink) ListEdgesFrom(
	ctx context.Context, srcNodeID string, kinds []string, opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	if srcNodeID == "" {
		return nil, errors.New("graphsink/sqlite: ListEdgesFrom: empty src_node_id")
	}
	return s.listEdges(ctx, "src_node_id", srcNodeID, kinds, opts)
}

// ListEdgesTo returns every inbound Edge to dstNodeID matching
// `kinds`. Empty `kinds` = all kinds.
//
// Order: `(kind ASC, dst_node_id ASC, edge_id ASC)`. Within a
// single ListEdgesTo result every row shares the same
// `dst_node_id`, so the `edge_id` tail is what guarantees
// deterministic ordering across calls -- without it SQLite
// returns rows in `rowid` order, which is stable but not
// reproducible across re-scans (insert order depends on the
// dispatcher's per-file walk).
func (s *Sink) ListEdgesTo(
	ctx context.Context, dstNodeID string, kinds []string, opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	if dstNodeID == "" {
		return nil, errors.New("graphsink/sqlite: ListEdgesTo: empty dst_node_id")
	}
	return s.listEdges(ctx, "dst_node_id", dstNodeID, kinds, opts)
}

// listEdges is the shared implementation of ListEdgesFrom /
// ListEdgesTo. `column` selects which endpoint we filter on;
// caller-supplied IDs flow in as a positional `?` so the
// column name is the only string we interpolate (and it is a
// package-local constant set, so injection is impossible).
func (s *Sink) listEdges(
	ctx context.Context, column, nodeID string, kinds []string, opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateEdgeKinds(kinds); err != nil {
		return nil, err
	}
	if err := s.checkOpen(); err != nil {
		return nil, err
	}

	var (
		b    strings.Builder
		args []any
	)
	b.WriteString(`SELECT edge_id, fingerprint, repo_id, kind, src_node_id, dst_node_id,
		from_sha, attrs_json
		FROM edge WHERE `)
	b.WriteString(column)
	b.WriteString(" = ?")
	args = append(args, nodeID)

	if len(kinds) > 0 {
		b.WriteString(" AND kind IN (")
		b.WriteString(kindPlaceholders(len(kinds)))
		b.WriteByte(')')
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	// S3.6 brief: order by (kind, dst_node_id). Add edge_id as
	// the final tie-breaker so ListEdgesTo (where every row
	// shares the same dst_node_id) still has totally-ordered
	// output. See package doc for the cross-backend rationale
	// and iter-4 open question `edge-order-key`.
	b.WriteString(" ORDER BY kind ASC, dst_node_id ASC, edge_id ASC LIMIT ?")
	args = append(args, normaliseLimit(opts.Limit))

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("graphsink/sqlite: listEdges query: %w", err)
	}
	defer rows.Close()

	var out []graphreader.Edge
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphsink/sqlite: listEdges rows: %w", err)
	}
	return out, nil
}

// GetNode fetches a single Node by id. Returns
// `graphreader.ErrNotFound` for an unknown id so callers can
// pattern-match with `errors.Is(err, graphreader.ErrNotFound)`
// portably across backends. `opts.Limit` is ignored (single-row
// lookup); `opts.IncludeRetired` is ignored (no retirement
// schema in SQLite).
func (s *Sink) GetNode(
	ctx context.Context, nodeID string, opts graphreader.ReaderOptions,
) (graphreader.Node, error) {
	if err := ctx.Err(); err != nil {
		return graphreader.Node{}, err
	}
	if nodeID == "" {
		return graphreader.Node{}, errors.New("graphsink/sqlite: GetNode: empty node_id")
	}
	if err := s.checkOpen(); err != nil {
		return graphreader.Node{}, err
	}

	const q = `SELECT node_id, fingerprint, repo_id, kind, canonical_signature,
		COALESCE(parent_node_id, ''), from_sha, attrs_json
		FROM node WHERE node_id = ?`
	row := s.db.QueryRowContext(ctx, q, nodeID)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	if err != nil {
		return graphreader.Node{}, err
	}
	return n, nil
}

// LookupBySignature resolves (repoID, kind, canonicalSignature)
// to its Node. Returns `graphreader.ErrNotFound` when no match
// exists. Translates a CLI `--seed <canonical_signature>` (or
// the HTTP `?seed=<signature>` query) into the concrete `Node`
// the call-chain BFS expects.
func (s *Sink) LookupBySignature(
	ctx context.Context,
	repoID fingerprint.RepoID,
	kind string,
	canonicalSignature string,
	opts graphreader.ReaderOptions,
) (graphreader.Node, error) {
	if err := ctx.Err(); err != nil {
		return graphreader.Node{}, err
	}
	if repoID.IsZero() {
		return graphreader.Node{}, errors.New("graphsink/sqlite: LookupBySignature: zero repo_id")
	}
	if kind == "" {
		return graphreader.Node{}, errors.New("graphsink/sqlite: LookupBySignature: empty kind")
	}
	if _, ok := nodeKindsAllowed[kind]; !ok {
		return graphreader.Node{}, fmt.Errorf("graphsink/sqlite: LookupBySignature: invalid node kind %q", kind)
	}
	if canonicalSignature == "" {
		return graphreader.Node{}, errors.New("graphsink/sqlite: LookupBySignature: empty canonical_signature")
	}
	if err := s.checkOpen(); err != nil {
		return graphreader.Node{}, err
	}

	const q = `SELECT node_id, fingerprint, repo_id, kind, canonical_signature,
		COALESCE(parent_node_id, ''), from_sha, attrs_json
		FROM node
		WHERE repo_id = ? AND kind = ? AND canonical_signature = ?
		ORDER BY node_id ASC
		LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, repoID.String(), kind, canonicalSignature)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	if err != nil {
		return graphreader.Node{}, err
	}
	return n, nil
}

// ----- row scanners -----------------------------------------------

// rowScanner abstracts `*sql.Row` and `*sql.Rows` so the same
// `scanNode` / `scanEdge` helpers serve both single-row and
// multi-row read paths.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanNode decodes one row from a `SELECT node_id, fingerprint,
// repo_id, kind, canonical_signature, COALESCE(parent_node_id,
// ''), from_sha, attrs_json FROM node …` query into a
// `graphreader.Node`.
func scanNode(row rowScanner) (graphreader.Node, error) {
	var (
		nodeID, repoID, kind, canonicalSig, parentID, fromSHA string
		fp                                                    []byte
		attrs                                                 []byte
	)
	if err := row.Scan(&nodeID, &fp, &repoID, &kind, &canonicalSig, &parentID, &fromSHA, &attrs); err != nil {
		return graphreader.Node{}, err
	}
	sum, err := fingerprint.SumFromBytes(fp)
	if err != nil {
		return graphreader.Node{}, fmt.Errorf("graphsink/sqlite: scanNode fingerprint: %w", err)
	}
	return graphreader.Node{
		NodeID:             nodeID,
		RepoID:             repoID,
		Fingerprint:        sum,
		Kind:               kind,
		CanonicalSignature: canonicalSig,
		ParentNodeID:       parentID,
		FromSHA:            fromSHA,
		AttrsJSON:          cloneAttrs(attrs),
	}, nil
}

// scanEdge decodes one row from a `SELECT edge_id, fingerprint,
// repo_id, kind, src_node_id, dst_node_id, from_sha, attrs_json
// FROM edge …` query into a `graphreader.Edge`.
func scanEdge(row rowScanner) (graphreader.Edge, error) {
	var (
		edgeID, repoID, kind, srcID, dstID, fromSHA string
		fp                                          []byte
		attrs                                       []byte
	)
	if err := row.Scan(&edgeID, &fp, &repoID, &kind, &srcID, &dstID, &fromSHA, &attrs); err != nil {
		return graphreader.Edge{}, err
	}
	sum, err := fingerprint.SumFromBytes(fp)
	if err != nil {
		return graphreader.Edge{}, fmt.Errorf("graphsink/sqlite: scanEdge fingerprint: %w", err)
	}
	return graphreader.Edge{
		EdgeID:      edgeID,
		RepoID:      repoID,
		Fingerprint: sum,
		Kind:        kind,
		SrcNodeID:   srcID,
		DstNodeID:   dstID,
		FromSHA:     fromSHA,
		AttrsJSON:   cloneAttrs(attrs),
	}, nil
}

// cloneAttrs copies the raw attrs bytes into a fresh
// `json.RawMessage` so callers cannot mutate the database
// driver's underlying buffer. Empty input maps to `{}` to match
// the writer's normaliseAttrs default.
func cloneAttrs(in []byte) json.RawMessage {
	if len(in) == 0 {
		return json.RawMessage("{}")
	}
	out := make(json.RawMessage, len(in))
	copy(out, in)
	return out
}
