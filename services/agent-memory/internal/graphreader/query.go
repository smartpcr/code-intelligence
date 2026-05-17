package graphreader

import (
	"fmt"
	"strings"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// nodeKinds is the closed set defined by the `node_kind` ENUM in
// migration 0001 (architecture.md §5.2.1). The reader rejects
// any kind value that is not in this set before issuing a SQL
// query — the database would also reject it with an enum cast
// error, but a Go-side guard produces a clearer diagnostic and
// avoids a wasted round-trip.
var nodeKinds = map[string]struct{}{
	"repo":    {},
	"package": {},
	"file":    {},
	"class":   {},
	"method":  {},
	"block":   {},
}

// edgeKinds is the closed set defined by the `edge_kind` ENUM in
// migration 0001 (architecture.md §5.2.2). Same Go-side guard
// rationale as nodeKinds.
var edgeKinds = map[string]struct{}{
	"contains":       {},
	"imports":        {},
	"static_calls":   {},
	"observed_calls": {},
	"extends":        {},
	"implements":     {},
	"reads":          {},
	"writes":         {},
	"renamed_to":     {},
}

// validateNodeKinds returns an error if any element of `kinds`
// is not a member of the `node_kind` ENUM. An empty slice
// passes — callers use that to mean "all kinds".
func validateNodeKinds(kinds []string) error {
	for _, k := range kinds {
		if _, ok := nodeKinds[k]; !ok {
			return fmt.Errorf("graphreader: invalid node kind %q "+
				"(allowed: repo/package/file/class/method/block)", k)
		}
	}
	return nil
}

// validateEdgeKinds is the edge analogue of validateNodeKinds.
func validateEdgeKinds(kinds []string) error {
	for _, k := range kinds {
		if _, ok := edgeKinds[k]; !ok {
			return fmt.Errorf("graphreader: invalid edge kind %q "+
				"(allowed: contains/imports/static_calls/observed_calls/"+
				"extends/implements/reads/writes/renamed_to)", k)
		}
	}
	return nil
}

// Column slices are the single source of truth for what each
// Node / Edge read projects. The package-level projection
// strings below are derived from these slices via joinColumns
// (one-shot init); downstream code (query.go, card.go) consumes
// only the joined strings.
//
// Why slices rather than raw multi-line string literals: the
// previous shape composed `nodeProjectionWithRetirement` by
// concatenating `nodeProjectionCurrent + ",\n\t..."`, which
// silently produced a double comma if the trailing comma was
// ever added to the current-projection literal. Building from
// slices and joining with ", " makes that class of formatting
// bug structurally unrepresentable — the join inserts exactly
// one separator between elements regardless of how the slices
// are spliced together.
//
// Order MUST match the dest slice in scanNodeRow / scanEdgeRow
// (see reader.go). The retirement-column slices are appended to
// (not interleaved with) the current-column slices, mirroring
// the optional tail scanNodeRow / scanEdgeRow append when
// `includeRetired` is true.
//
// These are slices (mutable) rather than arrays only because Go
// has no concise const-slice literal; the projection strings
// below snapshot the joined form at init, so any later mutation
// of the slices would not propagate to the SQL anyway.
var (
	nodeColumnsCurrent = []string{
		"n.node_id::text",
		"n.repo_id::text",
		"n.fingerprint",
		"n.kind::text",
		"n.canonical_signature",
		"n.parent_node_id::text",
		"n.from_sha",
		"n.attrs_json::text",
	}
	nodeRetirementColumns = []string{
		"nr.retired_at_sha",
		"nr.retired_at",
		"nr.superseded_by_node_id::text",
	}
	edgeColumnsCurrent = []string{
		"e.edge_id::text",
		"e.repo_id::text",
		"e.fingerprint",
		"e.kind::text",
		"e.src_node_id::text",
		"e.dst_node_id::text",
		"e.from_sha",
		"e.attrs_json::text",
	}
	edgeRetirementColumns = []string{
		"er.retired_at_sha",
		"er.retired_at",
	}
)

// joinColumns flattens one or more column-name slices into a
// single comma-separated projection string. No leading or
// trailing separator is emitted, so callers may safely
// concatenate further SQL text on either side (e.g. card.go
// appends ", obs.observation_count, ..." after the projection).
func joinColumns(groups ...[]string) string {
	var all []string
	for _, g := range groups {
		all = append(all, g...)
	}
	return strings.Join(all, ", ")
}

// Projection strings consumed by every Node / Edge SELECT in
// this package and by sibling card.go. Built once at init from
// the column slices above; never end with a comma and never
// start with whitespace, so they compose cleanly with arbitrary
// surrounding SQL.
var (
	// nodeProjectionCurrent is the column list every Node read
	// returns when retired rows are filtered out.
	nodeProjectionCurrent = joinColumns(nodeColumnsCurrent)
	// nodeProjectionWithRetirement is the column list returned
	// when `IncludeRetired = true`. The trailing three columns
	// are nullable — they're populated only when the LEFT JOIN
	// onto node_retirement matched.
	nodeProjectionWithRetirement = joinColumns(nodeColumnsCurrent, nodeRetirementColumns)
	// edgeProjectionCurrent / edgeProjectionWithRetirement are
	// the edge analogues. Edges have no `superseded_by_*`
	// column.
	edgeProjectionCurrent        = joinColumns(edgeColumnsCurrent)
	edgeProjectionWithRetirement = joinColumns(edgeColumnsCurrent, edgeRetirementColumns)
)

// selectNodeQuery returns the SQL used by GetNode. When
// `includeRetired = false` we apply the G5 anti-join in the
// WHERE clause; when true we LEFT JOIN node_retirement so the
// caller still sees retirement metadata for surfaced rows.
//
// Parameter shape: $1 = node_id (text).
func selectNodeQuery(includeRetired bool) string {
	if includeRetired {
		return `
			SELECT ` + nodeProjectionWithRetirement + `
			FROM node n
			LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
			WHERE n.node_id = $1
		`
	}
	return `
		SELECT ` + nodeProjectionCurrent + `
		FROM node n
		WHERE n.node_id = $1
		AND NOT EXISTS (
			SELECT 1 FROM node_retirement nr WHERE nr.node_id = n.node_id
		)
	`
}

// selectEdgeQuery is the Edge analogue of selectNodeQuery.
//
// Parameter shape: $1 = edge_id (text).
func selectEdgeQuery(includeRetired bool) string {
	if includeRetired {
		return `
			SELECT ` + edgeProjectionWithRetirement + `
			FROM edge e
			LEFT JOIN edge_retirement er ON er.edge_id = e.edge_id
			WHERE e.edge_id = $1
		`
	}
	return `
		SELECT ` + edgeProjectionCurrent + `
		FROM edge e
		WHERE e.edge_id = $1
		AND NOT EXISTS (
			SELECT 1 FROM edge_retirement er WHERE er.edge_id = e.edge_id
		)
	`
}

// selectEdgesFromQuery returns the SQL + parameter slice for
// ListEdgesFrom. Optional `kinds` filter expands into an
// `ANY($N::text[])` clause matched against `e.kind::text`.
//
// Why text[] rather than edge_kind[]
// ----------------------------------
// pgx v5 encodes `[]string` as `text[]` by default and does
// NOT auto-register custom ENUM array types. Casting the
// parameter to `edge_kind[]` and comparing against the
// `edge_kind`-typed column relies on PostgreSQL's implicit
// `text→edge_kind` coercion, which works today but breaks if
// a future migration recreates the enum with a different
// type oid or if the connection's session search_path hides
// the enum. Comparing on the `text` projection of the column
// (`e.kind::text = ANY($N::text[])`) sidesteps both risks —
// it requires no pgx-side type registration, no implicit
// enum coercion, and `validateEdgeKinds` (called before the
// query is issued) already guarantees every parameter value
// is a legal enum member.
//
// Stable sort `kind, edge_id` so successive calls return rows
// in identical order (snapshot-test friendly).
func selectEdgesFromQuery(srcNodeID string, kinds []string, includeRetired bool) (string, []any) {
	args := []any{srcNodeID}
	var kindClause string
	if len(kinds) > 0 {
		args = append(args, kinds)
		kindClause = fmt.Sprintf(" AND e.kind::text = ANY($%d::text[])", len(args))
	}
	if includeRetired {
		return `
			SELECT ` + edgeProjectionWithRetirement + `
			FROM edge e
			LEFT JOIN edge_retirement er ON er.edge_id = e.edge_id
			WHERE e.src_node_id = $1` + kindClause + `
			ORDER BY e.kind, e.edge_id
		`, args
	}
	return `
		SELECT ` + edgeProjectionCurrent + `
		FROM edge e
		WHERE e.src_node_id = $1` + kindClause + `
		AND NOT EXISTS (
			SELECT 1 FROM edge_retirement er WHERE er.edge_id = e.edge_id
		)
		ORDER BY e.kind, e.edge_id
	`, args
}

// selectEdgesToQuery is the inbound analogue of
// selectEdgesFromQuery: pivots on `e.dst_node_id` so the
// `agent.expand(direction='callers')` walker can pull the
// edges whose destination is the supplied node. Shares the
// edge-kind filter, retirement-aware projection, and stable
// `kind, edge_id` ordering with the outbound query so a
// future schema change to one half cannot silently drift the
// other.
//
// Parameter shape: $1 = dst_node_id (text), $N = kinds
// (text[]).
func selectEdgesToQuery(dstNodeID string, kinds []string, includeRetired bool) (string, []any) {
	args := []any{dstNodeID}
	var kindClause string
	if len(kinds) > 0 {
		args = append(args, kinds)
		kindClause = fmt.Sprintf(" AND e.kind::text = ANY($%d::text[])", len(args))
	}
	if includeRetired {
		return `
			SELECT ` + edgeProjectionWithRetirement + `
			FROM edge e
			LEFT JOIN edge_retirement er ON er.edge_id = e.edge_id
			WHERE e.dst_node_id = $1` + kindClause + `
			ORDER BY e.kind, e.edge_id
		`, args
	}
	return `
		SELECT ` + edgeProjectionCurrent + `
		FROM edge e
		WHERE e.dst_node_id = $1` + kindClause + `
		AND NOT EXISTS (
			SELECT 1 FROM edge_retirement er WHERE er.edge_id = e.edge_id
		)
		ORDER BY e.kind, e.edge_id
	`, args
}

// selectNodesQuery returns the SQL + parameter slice for
// ListNodes. Per-field filters compose into AND clauses; the
// `kinds` slice expands into `ANY($N::text[])` matched against
// `n.kind::text` (see selectEdgesFromQuery for the text[] vs
// node_kind[] rationale).
//
// Stable sort `kind, canonical_signature, node_id`.
func selectNodesQuery(
	repoID fingerprint.RepoID,
	kinds []string,
	f ListNodesFilter,
	includeRetired bool,
) (string, []any) {
	args := []any{repoID.String()}
	var clauses []string

	if len(kinds) > 0 {
		args = append(args, kinds)
		clauses = append(clauses, fmt.Sprintf("n.kind::text = ANY($%d::text[])", len(args)))
	}
	if f.ParentNodeID != "" {
		args = append(args, f.ParentNodeID)
		clauses = append(clauses, fmt.Sprintf("n.parent_node_id = $%d", len(args)))
	}
	if f.FromSHA != "" {
		args = append(args, f.FromSHA)
		clauses = append(clauses, fmt.Sprintf("n.from_sha = $%d", len(args)))
	}
	if f.CanonicalSignature != "" {
		args = append(args, f.CanonicalSignature)
		clauses = append(clauses, fmt.Sprintf("n.canonical_signature = $%d", len(args)))
	}

	andTail := ""
	if len(clauses) > 0 {
		andTail = " AND " + strings.Join(clauses, " AND ")
	}

	if includeRetired {
		return `
			SELECT ` + nodeProjectionWithRetirement + `
			FROM node n
			LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
			WHERE n.repo_id = $1::uuid` + andTail + `
			ORDER BY n.kind, n.canonical_signature, n.node_id
		`, args
	}
	return `
		SELECT ` + nodeProjectionCurrent + `
		FROM node n
		WHERE n.repo_id = $1::uuid` + andTail + `
		AND NOT EXISTS (
			SELECT 1 FROM node_retirement nr WHERE nr.node_id = n.node_id
		)
		ORDER BY n.kind, n.canonical_signature, n.node_id
	`, args
}
