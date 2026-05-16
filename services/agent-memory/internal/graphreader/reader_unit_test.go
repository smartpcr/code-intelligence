package graphreader

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// TestNew_panicsOnNilPool guards against the most common
// programmer bug — calling `New(nil, logger)` and getting a
// dangling Reader that NPEs on first read.
func TestNew_panicsOnNilPool(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil, logger) must panic")
		}
	}()
	_ = New(nil, nil)
}

// TestValidateNodeKinds enforces the closed set defined by the
// `node_kind` ENUM in migration 0001. The validator's job is to
// reject typos before the SQL round-trip.
func TestValidateNodeKinds(t *testing.T) {
	t.Run("empty slice ok", func(t *testing.T) {
		if err := validateNodeKinds(nil); err != nil {
			t.Fatalf("empty slice should be valid, got %v", err)
		}
	})
	t.Run("valid kinds ok", func(t *testing.T) {
		valid := []string{"repo", "package", "file", "class", "method", "block"}
		if err := validateNodeKinds(valid); err != nil {
			t.Fatalf("valid kinds should pass, got %v", err)
		}
	})
	t.Run("unknown kind rejected", func(t *testing.T) {
		err := validateNodeKinds([]string{"method", "function"})
		if err == nil {
			t.Fatal("unknown kind must be rejected")
		}
		if !strings.Contains(err.Error(), `"function"`) {
			t.Fatalf("error should name the offending kind, got %v", err)
		}
	})
}

// TestValidateEdgeKinds is the edge-side analogue, ensuring
// the `edge_kind` ENUM members are exhaustively accepted and
// novel kinds rejected.
func TestValidateEdgeKinds(t *testing.T) {
	valid := []string{
		"contains", "imports", "static_calls", "observed_calls",
		"extends", "implements", "reads", "writes", "renamed_to",
	}
	if err := validateEdgeKinds(valid); err != nil {
		t.Fatalf("valid kinds should pass, got %v", err)
	}
	if err := validateEdgeKinds([]string{"observes"}); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}

// TestSelectNodeQuery_hidesRetiredByDefault is the SQL-level
// proof of Stage 2.2's primary test scenario: when
// IncludeRetired is false the query MUST carry the NOT EXISTS
// anti-join, never a LEFT JOIN that would surface retirement
// columns. Without this property a retired Node still leaks
// through GetNode.
func TestSelectNodeQuery_hidesRetiredByDefault(t *testing.T) {
	got := selectNodeQuery(false)
	if !strings.Contains(got, "NOT EXISTS") {
		t.Fatalf("default query must use NOT EXISTS anti-join, got:\n%s", got)
	}
	if strings.Contains(got, "LEFT JOIN node_retirement") {
		t.Fatalf("default query must not LEFT JOIN node_retirement, got:\n%s", got)
	}
	if strings.Contains(got, "retired_at_sha") {
		t.Fatalf("default query must not project retired_at_sha, got:\n%s", got)
	}
}

// TestSelectNodeQuery_includesRetirementOnOptIn confirms the
// opt-in path swaps the NOT EXISTS for a LEFT JOIN AND projects
// the retirement tuple — this is what populates
// `Node.Retirement` in scanNodeRow.
func TestSelectNodeQuery_includesRetirementOnOptIn(t *testing.T) {
	got := selectNodeQuery(true)
	if strings.Contains(got, "NOT EXISTS") {
		t.Fatalf("opt-in query must not use NOT EXISTS, got:\n%s", got)
	}
	for _, want := range []string{
		"LEFT JOIN node_retirement",
		"retired_at_sha",
		"retired_at",
		"superseded_by_node_id",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("opt-in query missing %q, got:\n%s", want, got)
		}
	}
}

// TestSelectEdgeQuery_hidesRetiredByDefault mirrors the Node
// case for the Edge path.
func TestSelectEdgeQuery_hidesRetiredByDefault(t *testing.T) {
	got := selectEdgeQuery(false)
	if !strings.Contains(got, "NOT EXISTS") {
		t.Fatalf("default query must use NOT EXISTS anti-join, got:\n%s", got)
	}
	if strings.Contains(got, "LEFT JOIN edge_retirement") {
		t.Fatalf("default query must not LEFT JOIN edge_retirement, got:\n%s", got)
	}
}

func TestSelectEdgeQuery_includesRetirementOnOptIn(t *testing.T) {
	got := selectEdgeQuery(true)
	if strings.Contains(got, "NOT EXISTS") {
		t.Fatalf("opt-in query must not use NOT EXISTS, got:\n%s", got)
	}
	if !strings.Contains(got, "LEFT JOIN edge_retirement") {
		t.Fatalf("opt-in query must LEFT JOIN edge_retirement, got:\n%s", got)
	}
}

// TestSelectEdgesFromQuery_kindFilter ensures the optional
// kinds slice expands into an ANY clause that uses the next
// available parameter index. Skipping kinds must not introduce
// the clause at all.
func TestSelectEdgesFromQuery_kindFilter(t *testing.T) {
	t.Run("no kinds", func(t *testing.T) {
		q, args := selectEdgesFromQuery("n1", nil, false)
		if len(args) != 1 || args[0] != "n1" {
			t.Fatalf("expected [n1], got %v", args)
		}
		if strings.Contains(q, "ANY(") {
			t.Fatalf("query must not contain ANY filter when kinds is empty, got:\n%s", q)
		}
	})
	t.Run("with kinds", func(t *testing.T) {
		q, args := selectEdgesFromQuery("n1", []string{"static_calls", "observed_calls"}, false)
		if len(args) != 2 {
			t.Fatalf("expected 2 args, got %v", args)
		}
		if !strings.Contains(q, "ANY($2::text[])") {
			t.Fatalf("query must use ANY($2::text[]), got:\n%s", q)
		}
		if !strings.Contains(q, "e.kind::text = ANY(") {
			t.Fatalf("query must compare e.kind::text vs text[] to avoid enum-array binding fragility, got:\n%s", q)
		}
	})
	t.Run("stable sort", func(t *testing.T) {
		q, _ := selectEdgesFromQuery("n1", nil, false)
		if !strings.Contains(q, "ORDER BY e.kind, e.edge_id") {
			t.Fatalf("query must order by (kind, edge_id), got:\n%s", q)
		}
	})
}

// TestSelectNodesQuery_filterClauses ensures every non-empty
// filter contributes its own AND clause with the right
// parameter index, and that the absence of a filter omits the
// clause entirely.
func TestSelectNodesQuery_filterClauses(t *testing.T) {
	rid := fingerprint.MustParseRepoID("11111111-1111-1111-1111-111111111111")

	t.Run("no filters", func(t *testing.T) {
		q, args := selectNodesQuery(rid, nil, ListNodesFilter{}, false)
		if len(args) != 1 {
			t.Fatalf("expected [repo_id], got %v", args)
		}
		for _, banned := range []string{"n.kind::text = ANY", "n.parent_node_id =", "n.from_sha =", "n.canonical_signature ="} {
			if strings.Contains(q, banned) {
				t.Fatalf("query must not include %q without filter, got:\n%s", banned, q)
			}
		}
	})

	t.Run("all filters present", func(t *testing.T) {
		q, args := selectNodesQuery(rid,
			[]string{"method"},
			ListNodesFilter{
				ParentNodeID:       "p1",
				FromSHA:            "sha1",
				CanonicalSignature: "com.example.Foo#bar()",
			},
			false,
		)
		if len(args) != 5 {
			t.Fatalf("expected 5 args (repo + kinds + 3 filters), got %v", args)
		}
		for _, want := range []string{
			"n.kind::text = ANY($2::text[])",
			"n.parent_node_id = $3",
			"n.from_sha = $4",
			"n.canonical_signature = $5",
		} {
			if !strings.Contains(q, want) {
				t.Fatalf("query missing %q, got:\n%s", want, q)
			}
		}
	})

	t.Run("retired filter still applied", func(t *testing.T) {
		q, _ := selectNodesQuery(rid, nil, ListNodesFilter{}, false)
		if !strings.Contains(q, "NOT EXISTS") {
			t.Fatalf("default ListNodes must still anti-join retirement, got:\n%s", q)
		}
	})
}

// TestNeighborhoodEdgesQuery_includesTraceObservation enforces
// the projection mandated by Stage 2.2 scenario #3 — the
// observation columns MUST be in the SELECT list so the result
// resolves observed_calls without a second query.
func TestNeighborhoodEdgesQuery_includesTraceObservation(t *testing.T) {
	q := neighborhoodEdgesQuery(false)
	for _, want := range []string{
		"LEFT JOIN trace_observation",
		"obs.observation_count",
		"obs.p50_latency_ms",
		"obs.p95_latency_ms",
		"obs.latest_span_ref",
		"obs.last_observed_at",
		"ORDER BY e.kind, e.edge_id",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("query missing %q, got:\n%s", want, q)
		}
	}
	if strings.Contains(q, "LEFT JOIN edge_retirement") {
		t.Fatalf("default neighborhood query must not LEFT JOIN edge_retirement, got:\n%s", q)
	}
	if !strings.Contains(q, "NOT EXISTS") {
		t.Fatalf("default neighborhood query must NOT EXISTS-filter retired edges, got:\n%s", q)
	}
}

func TestNeighborhoodEdgesQuery_includeRetiredAddsLeftJoin(t *testing.T) {
	q := neighborhoodEdgesQuery(true)
	if !strings.Contains(q, "LEFT JOIN edge_retirement") {
		t.Fatalf("opt-in query must LEFT JOIN edge_retirement, got:\n%s", q)
	}
	if strings.Contains(q, "NOT EXISTS") {
		t.Fatalf("opt-in query must not anti-join retired rows, got:\n%s", q)
	}
}

// scriptedScanner is a tiny rowScanner that returns either a
// pre-seeded error OR a pre-seeded tuple of column values into
// the dest pointers. It lets the scan helpers be exercised
// without a real *pgx.Rows.
type scriptedScanner struct {
	err  error
	cols []any
}

func (s scriptedScanner) Scan(dest ...any) error {
	if s.err != nil {
		return s.err
	}
	if len(dest) != len(s.cols) {
		return errAssertCount{want: len(s.cols), got: len(dest)}
	}
	for i, d := range dest {
		if err := assign(d, s.cols[i]); err != nil {
			return err
		}
	}
	return nil
}

type errAssertCount struct{ want, got int }

func (e errAssertCount) Error() string {
	return "scripted: column count mismatch"
}

// assign performs the minimum pgx-like dest assignment the
// reader's scan helpers need for unit testing: it handles the
// concrete dest types reader.go and card.go pass in.
func assign(dest any, src any) error {
	switch d := dest.(type) {
	case *string:
		v, ok := src.(string)
		if !ok {
			return errAssertCount{}
		}
		*d = v
	case **string:
		if src == nil {
			*d = nil
			return nil
		}
		v, ok := src.(string)
		if !ok {
			return errAssertCount{}
		}
		*d = &v
	case *[]byte:
		if src == nil {
			*d = nil
			return nil
		}
		v, ok := src.([]byte)
		if !ok {
			return errAssertCount{}
		}
		*d = v
	case **time.Time:
		if src == nil {
			*d = nil
			return nil
		}
		v, ok := src.(time.Time)
		if !ok {
			return errAssertCount{}
		}
		*d = &v
	case **int64:
		if src == nil {
			*d = nil
			return nil
		}
		v, ok := src.(int64)
		if !ok {
			return errAssertCount{}
		}
		*d = &v
	case **float64:
		if src == nil {
			*d = nil
			return nil
		}
		v, ok := src.(float64)
		if !ok {
			return errAssertCount{}
		}
		*d = &v
	default:
		return errAssertCount{}
	}
	return nil
}

// TestScanNodeRow_translatesPgxErrNoRows ensures the reader
// folds the pgx-specific sentinel onto our package-public
// ErrNotFound so callers don't need to import pgx.
func TestScanNodeRow_translatesPgxErrNoRows(t *testing.T) {
	scn := scriptedScanner{err: pgx.ErrNoRows}
	_, err := scanNodeRow(scn, false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestScanEdgeRow_translatesPgxErrNoRows is the edge analogue.
func TestScanEdgeRow_translatesPgxErrNoRows(t *testing.T) {
	scn := scriptedScanner{err: pgx.ErrNoRows}
	_, err := scanEdgeRow(scn, false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestScanNodeRow_nullableParentNodeID checks that a NULL
// parent_node_id (common for the repo Node) decodes to an
// empty string in Node.ParentNodeID rather than crashing on
// pointer deref.
func TestScanNodeRow_nullableParentNodeID(t *testing.T) {
	scn := scriptedScanner{cols: []any{
		"node-1",       // node_id
		"repo-1",       // repo_id
		[]byte(nil),    // fingerprint
		"repo",         // kind
		"my-repo",      // canonical_signature
		any(nil),       // parent_node_id (NULL)
		"sha1",         // from_sha
		[]byte("{}"),   // attrs_json
	}}
	got, err := scanNodeRow(scn, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.ParentNodeID != "" {
		t.Fatalf("expected empty ParentNodeID for NULL, got %q", got.ParentNodeID)
	}
	if got.NodeID != "node-1" {
		t.Fatalf("expected NodeID=node-1, got %q", got.NodeID)
	}
}

// TestGetNode_emptyIDRejected confirms input-validation guards
// catch the caller-side bug before round-tripping to the DB.
func TestGetNode_emptyIDRejected(t *testing.T) {
	r := &Reader{}
	_, err := r.GetNode(context.Background(), "", ReaderOptions{})
	if err == nil || !strings.Contains(err.Error(), "empty node_id") {
		t.Fatalf("expected empty node_id error, got %v", err)
	}
}

func TestGetEdge_emptyIDRejected(t *testing.T) {
	r := &Reader{}
	_, err := r.GetEdge(context.Background(), "", ReaderOptions{})
	if err == nil || !strings.Contains(err.Error(), "empty edge_id") {
		t.Fatalf("expected empty edge_id error, got %v", err)
	}
}

func TestListEdgesFrom_emptySrcRejected(t *testing.T) {
	r := &Reader{}
	_, err := r.ListEdgesFrom(context.Background(), "", nil, ReaderOptions{})
	if err == nil || !strings.Contains(err.Error(), "empty src_node_id") {
		t.Fatalf("expected empty src_node_id error, got %v", err)
	}
}

func TestListNodes_zeroRepoRejected(t *testing.T) {
	r := &Reader{}
	_, err := r.ListNodes(context.Background(), fingerprint.RepoID{}, nil, ListNodesFilter{}, ReaderOptions{})
	if err == nil || !strings.Contains(err.Error(), "zero repo_id") {
		t.Fatalf("expected zero repo_id error, got %v", err)
	}
}

func TestNeighborhoodCard_emptyIDRejected(t *testing.T) {
	r := &Reader{}
	_, err := r.NeighborhoodCard(context.Background(), "", ReaderOptions{})
	if err == nil || !strings.Contains(err.Error(), "empty node_id") {
		t.Fatalf("expected empty node_id error, got %v", err)
	}
}

// --- Fix-4 coverage: IncludeRetired=true scan paths ---------
//
// Iter 1 only asserted the SQL string shape of the
// retirement-aware query; the actual scanNodeRow /
// scanEdgeRow / scanCardEdgeRow column-decode paths were only
// exercised by integration tests that skip when
// AGENT_MEMORY_PG_URL is unset. The tests below drive those
// helpers through scriptedScanner so the behaviour is verified
// on every `go test` run, not only in the live-DB lane.

// TestScanNodeRow_includeRetiredPopulatesRetirement asserts
// the success path: when the retirement triple is present,
// Node.Retirement is materialised with all three fields
// populated.
func TestScanNodeRow_includeRetiredPopulatesRetirement(t *testing.T) {
	retAt := time.Date(2024, 5, 6, 12, 34, 56, 0, time.UTC)
	scn := scriptedScanner{cols: []any{
		"node-42",            // node_id
		"repo-1",             // repo_id
		[]byte(nil),          // fingerprint
		"method",             // kind
		"com.acme.Foo#bar()", // canonical_signature
		any(nil),             // parent_node_id (NULL)
		"sha-old",            // from_sha
		[]byte("{}"),         // attrs_json
		"sha-retired",        // node_retirement.retired_at_sha
		retAt,                // node_retirement.retired_at
		"node-99",            // node_retirement.superseded_by_node_id
	}}
	got, err := scanNodeRow(scn, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Retirement == nil {
		t.Fatal("Retirement must be populated when triple is present")
	}
	if got.Retirement.RetiredAtSHA != "sha-retired" {
		t.Fatalf("RetiredAtSHA: got %q, want sha-retired", got.Retirement.RetiredAtSHA)
	}
	if !got.Retirement.RetiredAt.Equal(retAt) {
		t.Fatalf("RetiredAt: got %v, want %v", got.Retirement.RetiredAt, retAt)
	}
	if got.Retirement.SupersededByNodeID != "node-99" {
		t.Fatalf("SupersededByNodeID: got %q, want node-99", got.Retirement.SupersededByNodeID)
	}
}

// TestScanNodeRow_includeRetiredNullRetirementYieldsNilField
// asserts the LEFT JOIN miss case: when IncludeRetired=true
// but the joined retirement row is NULL (i.e. the node is
// live), Node.Retirement remains nil. This is the contract
// callers depend on to distinguish "live" from "retired" in
// the opt-in opt-in result set.
func TestScanNodeRow_includeRetiredNullRetirementYieldsNilField(t *testing.T) {
	scn := scriptedScanner{cols: []any{
		"node-1",     // node_id
		"repo-1",     // repo_id
		[]byte(nil),  // fingerprint
		"method",     // kind
		"sig",        // canonical_signature
		any(nil),     // parent_node_id (NULL)
		"sha-cur",    // from_sha
		[]byte("{}"), // attrs_json
		any(nil),     // retired_at_sha (NULL — LEFT JOIN miss)
		any(nil),     // retired_at     (NULL)
		any(nil),     // superseded_by  (NULL)
	}}
	got, err := scanNodeRow(scn, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Retirement != nil {
		t.Fatalf("Retirement must remain nil for live node, got %+v", got.Retirement)
	}
}

// TestScanNodeRow_includeRetiredPartialRetirementTriple
// asserts robustness against a writer that materialises the
// retirement row without `superseded_by_node_id` (allowed by
// the schema). The retirement struct is still emitted; only
// the missing field is left zero-valued.
func TestScanNodeRow_includeRetiredPartialRetirementTriple(t *testing.T) {
	retAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	scn := scriptedScanner{cols: []any{
		"node-2",     // node_id
		"repo-1",     // repo_id
		[]byte(nil),  // fingerprint
		"method",     // kind
		"sig",        // canonical_signature
		any(nil),     // parent_node_id (NULL)
		"sha-old",    // from_sha
		[]byte("{}"), // attrs_json
		"sha-ret",    // retired_at_sha (non-NULL: row exists)
		retAt,        // retired_at
		any(nil),     // superseded_by (NULL: no successor yet)
	}}
	got, err := scanNodeRow(scn, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Retirement == nil {
		t.Fatal("Retirement must be populated when retired_at_sha is set")
	}
	if got.Retirement.SupersededByNodeID != "" {
		t.Fatalf("SupersededByNodeID: got %q, want empty", got.Retirement.SupersededByNodeID)
	}
	if !got.Retirement.RetiredAt.Equal(retAt) {
		t.Fatalf("RetiredAt: got %v, want %v", got.Retirement.RetiredAt, retAt)
	}
}

// TestScanEdgeRow_includeRetiredPopulatesRetirement is the
// Edge analogue: opt-in scan path produces a non-nil
// Retirement struct with the SHA and timestamp populated.
func TestScanEdgeRow_includeRetiredPopulatesRetirement(t *testing.T) {
	retAt := time.Date(2024, 8, 9, 10, 11, 12, 0, time.UTC)
	scn := scriptedScanner{cols: []any{
		"edge-1",        // edge_id
		"repo-1",        // repo_id
		[]byte(nil),     // fingerprint
		"observed_calls", // kind
		"node-src",      // src_node_id
		"node-dst",      // dst_node_id
		"sha-old",       // from_sha
		[]byte("{}"),    // attrs_json
		"sha-ret",       // retired_at_sha
		retAt,           // retired_at
	}}
	got, err := scanEdgeRow(scn, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Retirement == nil {
		t.Fatal("Retirement must be populated when triple is present")
	}
	if got.Retirement.RetiredAtSHA != "sha-ret" {
		t.Fatalf("RetiredAtSHA: got %q, want sha-ret", got.Retirement.RetiredAtSHA)
	}
	if !got.Retirement.RetiredAt.Equal(retAt) {
		t.Fatalf("RetiredAt: got %v, want %v", got.Retirement.RetiredAt, retAt)
	}
}

// TestScanEdgeRow_includeRetiredNullRetirementYieldsNilField
// is the inverse: opt-in scan with a LEFT-JOIN miss leaves
// Retirement nil.
func TestScanEdgeRow_includeRetiredNullRetirementYieldsNilField(t *testing.T) {
	scn := scriptedScanner{cols: []any{
		"edge-2",        // edge_id
		"repo-1",        // repo_id
		[]byte(nil),     // fingerprint
		"static_calls",  // kind
		"node-src",      // src_node_id
		"node-dst",      // dst_node_id
		"sha-cur",       // from_sha
		[]byte("{}"),    // attrs_json
		any(nil),        // retired_at_sha (NULL)
		any(nil),        // retired_at     (NULL)
	}}
	got, err := scanEdgeRow(scn, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Retirement != nil {
		t.Fatalf("Retirement must remain nil for live edge, got %+v", got.Retirement)
	}
}

// TestScanCardEdgeRow_populatesTraceObservation mirrors Stage
// 2.2 acceptance scenario #3 ("neighborhood card resolves
// observed_calls") at the scan-helper level: a row whose
// `observation_count = 42` materialises into a CardEdge whose
// `TraceObservation.ObservationCount == 42`.
func TestScanCardEdgeRow_populatesTraceObservation(t *testing.T) {
	lastSeen := time.Date(2024, 7, 8, 9, 10, 11, 0, time.UTC)
	scn := scriptedScanner{cols: []any{
		"edge-3",         // edge_id
		"repo-1",         // repo_id
		[]byte(nil),      // fingerprint
		"observed_calls", // kind
		"node-src",       // src_node_id
		"node-dst",       // dst_node_id
		"sha-cur",        // from_sha
		[]byte("{}"),     // attrs_json
		int64(42),        // obs.observation_count
		float64(11.5),    // obs.p50_latency_ms
		float64(72.3),    // obs.p95_latency_ms
		"span-xyz",       // obs.latest_span_ref
		lastSeen,         // obs.last_observed_at
	}}
	got, err := scanCardEdgeRow(scn, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.TraceObservation == nil {
		t.Fatal("TraceObservation must be populated when observation_count is non-NULL")
	}
	if got.TraceObservation.ObservationCount != 42 {
		t.Fatalf("ObservationCount: got %d, want 42", got.TraceObservation.ObservationCount)
	}
	if got.TraceObservation.P50LatencyMs != 11.5 {
		t.Fatalf("P50LatencyMs: got %v, want 11.5", got.TraceObservation.P50LatencyMs)
	}
	if got.TraceObservation.P95LatencyMs != 72.3 {
		t.Fatalf("P95LatencyMs: got %v, want 72.3", got.TraceObservation.P95LatencyMs)
	}
	if got.TraceObservation.LatestSpanRef != "span-xyz" {
		t.Fatalf("LatestSpanRef: got %q, want span-xyz", got.TraceObservation.LatestSpanRef)
	}
	if !got.TraceObservation.LastObservedAt.Equal(lastSeen) {
		t.Fatalf("LastObservedAt: got %v, want %v", got.TraceObservation.LastObservedAt, lastSeen)
	}
}

// TestScanCardEdgeRow_nilObservationYieldsNilField verifies the
// "never observed" case: a static_calls edge that has no
// matching trace_observation row (LEFT JOIN miss → NULL
// observation_count) must leave CardEdge.TraceObservation nil
// so the caller can distinguish "never observed" from
// "observed zero times since the partition rolled".
func TestScanCardEdgeRow_nilObservationYieldsNilField(t *testing.T) {
	scn := scriptedScanner{cols: []any{
		"edge-4",       // edge_id
		"repo-1",       // repo_id
		[]byte(nil),    // fingerprint
		"static_calls", // kind
		"node-src",     // src_node_id
		"node-dst",     // dst_node_id
		"sha-cur",      // from_sha
		[]byte("{}"),   // attrs_json
		any(nil),       // obs.observation_count (NULL)
		any(nil),       // obs.p50_latency_ms
		any(nil),       // obs.p95_latency_ms
		any(nil),       // obs.latest_span_ref
		any(nil),       // obs.last_observed_at
	}}
	got, err := scanCardEdgeRow(scn, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.TraceObservation != nil {
		t.Fatalf("TraceObservation must be nil for never-observed edge, got %+v", got.TraceObservation)
	}
}

// TestScanCardEdgeRow_includeRetiredAndObserved combines both
// opt-ins: a row that is retired AND has trace observations
// must produce a non-nil Retirement AND a non-nil
// TraceObservation, in the correct column order.
func TestScanCardEdgeRow_includeRetiredAndObserved(t *testing.T) {
	retAt := time.Date(2024, 9, 9, 9, 9, 9, 0, time.UTC)
	lastSeen := time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC)
	scn := scriptedScanner{cols: []any{
		"edge-5",         // edge_id
		"repo-1",         // repo_id
		[]byte(nil),      // fingerprint
		"observed_calls", // kind
		"node-src",       // src_node_id
		"node-dst",       // dst_node_id
		"sha-cur",        // from_sha
		[]byte("{}"),     // attrs_json
		"sha-ret",        // retired_at_sha
		retAt,            // retired_at
		int64(7),         // obs.observation_count
		float64(1.0),     // obs.p50_latency_ms
		float64(2.0),     // obs.p95_latency_ms
		"span-abc",       // obs.latest_span_ref
		lastSeen,         // obs.last_observed_at
	}}
	got, err := scanCardEdgeRow(scn, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Retirement == nil {
		t.Fatal("Retirement must be populated when retired_at_sha is set")
	}
	if got.TraceObservation == nil {
		t.Fatal("TraceObservation must be populated when observation_count is non-NULL")
	}
	if got.TraceObservation.ObservationCount != 7 {
		t.Fatalf("ObservationCount: got %d, want 7", got.TraceObservation.ObservationCount)
	}
}
