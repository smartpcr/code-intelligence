package recallcontext

// Behavioural unit tests for the recall-context Append + Resolve
// path. These cover the Stage 2.4 acceptance scenarios at the
// pure-unit layer (no environment gate, no live PostgreSQL),
// complementing the live-DB tests in log_integration_test.go.
//
// Stage 2.4 scenarios pinned here:
//
//   1. "ordering preserved" -- Append called with node_ids=[A,B,C]
//      must wire the slice through pq.Array in input order; Resolve
//      called with a log row whose node_ids/edge_ids/concept_ids
//      arrays come back as [A,B,C] must dereference in that order
//      AND each GetNode / GetEdge / GetConcept call must be issued
//      with ReaderOptions.IncludeRetired = true (per the
//      implementation-plan brief, risk §9.13).
//
//   2. "degraded snapshot flag" -- served_under_degraded=true is
//      passed through to PostgreSQL as a parameter AND surfaces on
//      the ResolvedContext.ServedUnderDegraded field.
//
// Plus the validation surface and the WriteContractViolation
// classifier:
//
//   * Empty / unknown verb rejected before SQL.
//   * Zero RepoID rejected.
//   * Invalid JSON rejected.
//   * Malformed UUID in any id slice rejected with a descriptive
//     error that names the offending field + index.
//   * SQLSTATE 42501 surfaces as *WriteContractViolation.
//   * Multiple matching rows surface as a corruption error.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// silentLogger is the slog handler we use in unit tests so the
// audit-line emission machinery runs end-to-end but produces no
// terminal noise during `go test ./...`.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeResolver is the test-only cardResolver implementation
// the order-preservation tests inject. It records every call in
// `calls` (with the kind + id) so the test can assert on the
// exact ordering and option propagation.
type fakeResolver struct {
	nodes    map[string]graphreader.Node
	edges    map[string]graphreader.Edge
	concepts map[string]graphreader.Concept

	calls []resolverCall

	// missingNode / missingEdge / missingConcept force the
	// corresponding Get* to return ErrNotFound for any id the
	// caller did not pre-register in the maps above.
	missingNode    bool
	missingEdge    bool
	missingConcept bool
}

// resolverCall records one Get* invocation. Used by the
// ordering test to assert that the order of calls (and thus
// the order of returned slices) matches the input id ordering.
type resolverCall struct {
	Kind           string // "node", "edge", or "concept"
	ID             string
	IncludeRetired bool
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		nodes:    map[string]graphreader.Node{},
		edges:    map[string]graphreader.Edge{},
		concepts: map[string]graphreader.Concept{},
	}
}

func (f *fakeResolver) GetNode(
	_ context.Context, nodeID string, opts graphreader.ReaderOptions,
) (graphreader.Node, error) {
	f.calls = append(f.calls, resolverCall{
		Kind: "node", ID: nodeID, IncludeRetired: opts.IncludeRetired,
	})
	if n, ok := f.nodes[nodeID]; ok {
		return n, nil
	}
	if f.missingNode {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	// Default: synthesize a Node carrying the id so the test can
	// confirm the slice order without pre-populating every fake
	// id's metadata.
	return graphreader.Node{NodeID: nodeID}, nil
}

func (f *fakeResolver) GetEdge(
	_ context.Context, edgeID string, opts graphreader.ReaderOptions,
) (graphreader.Edge, error) {
	f.calls = append(f.calls, resolverCall{
		Kind: "edge", ID: edgeID, IncludeRetired: opts.IncludeRetired,
	})
	if e, ok := f.edges[edgeID]; ok {
		return e, nil
	}
	if f.missingEdge {
		return graphreader.Edge{}, graphreader.ErrNotFound
	}
	return graphreader.Edge{EdgeID: edgeID}, nil
}

func (f *fakeResolver) GetConcept(
	_ context.Context, conceptID string,
) (graphreader.Concept, error) {
	f.calls = append(f.calls, resolverCall{
		Kind: "concept", ID: conceptID,
	})
	if c, ok := f.concepts[conceptID]; ok {
		return c, nil
	}
	if f.missingConcept {
		return graphreader.Concept{}, graphreader.ErrNotFound
	}
	return graphreader.Concept{ConceptID: conceptID}, nil
}

// arrayArg is a sqlmock-compatible argument matcher that
// asserts the driver value produced by `pq.Array([]string{...})`
// equals a literal PostgreSQL array string built from the
// expected slice. The matcher is necessary because sqlmock's
// default deep-equal would compare the *pq.StringArray reflect
// shape rather than the wire-level array literal -- which
// would let an out-of-order slice silently pass the
// equality check. pq.StringArray.Value() emits
// `{"id1","id2","id3"}` in input order; we build the same
// string and match on it byte-for-byte.
type arrayArg struct {
	want []string
}

// Match implements sqlmock.Argument.
func (a arrayArg) Match(v driver.Value) bool {
	got, err := pq.StringArray(a.want).Value()
	if err != nil {
		return false
	}
	gotStr, ok := got.(string)
	if !ok {
		return false
	}
	switch t := v.(type) {
	case string:
		return t == gotStr
	case []byte:
		return string(t) == gotStr
	default:
		return false
	}
}

// newMockLog wires a sqlmock-backed *sql.DB to a Log that uses
// the supplied resolver. The returned cleanup verifies every
// sqlmock expectation was met before the test exits.
func newMockLog(t *testing.T, r cardResolver) (*Log, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherRegexp,
	))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	log := newWithResolver(db, r, silentLogger())
	return log, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// ----- Constructor sanity ------------------------------------------

func TestNew_panicsOnNilDB(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil, reader, logger) must panic")
		}
	}()
	_ = New(nil, &graphreader.Reader{}, nil)
}

func TestNew_panicsOnNilReader(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(db, nil, logger) must panic")
		}
	}()
	_ = New(&sql.DB{}, nil, nil)
}

func TestNewWithResolver_panicsOnNilDB(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("newWithResolver(nil, r, logger) must panic")
		}
	}()
	_ = newWithResolver(nil, newFakeResolver(), nil)
}

func TestNewWithResolver_panicsOnNilResolver(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("newWithResolver(db, nil, logger) must panic")
		}
	}()
	_ = newWithResolver(&sql.DB{}, nil, nil)
}

// ----- Append validation -------------------------------------------

// validInput is the minimum-viable AppendInput used by validation
// tests; tests mutate one field at a time and assert the
// resulting error.
func validInput() AppendInput {
	return AppendInput{
		Verb:                 "recall",
		RepoID:               fingerprint.MustParseRepoID("11111111-1111-1111-1111-111111111111"),
		QueryJSON:            json.RawMessage(`{"q":"foo"}`),
		RerankerModelVersion: "rerank-v1",
	}
}

func TestAppend_rejectsInvalidVerb(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()
	in := validInput()
	in.Verb = "garbage"

	_, err := log.Append(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "invalid verb") {
		t.Fatalf("want invalid-verb error, got %v", err)
	}
}

func TestAppend_rejectsEmptyVerb(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()
	in := validInput()
	in.Verb = ""

	_, err := log.Append(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "invalid verb") {
		t.Fatalf("want invalid-verb error, got %v", err)
	}
}

func TestAppend_rejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()
	in := validInput()
	in.RepoID = fingerprint.RepoID{}

	_, err := log.Append(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "zero repo_id") {
		t.Fatalf("want zero-repo_id error, got %v", err)
	}
}

func TestAppend_rejectsEmptyQueryJSON(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()
	in := validInput()
	in.QueryJSON = nil

	_, err := log.Append(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "empty query_json") {
		t.Fatalf("want empty-query_json error, got %v", err)
	}
}

func TestAppend_rejectsInvalidQueryJSON(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()
	in := validInput()
	in.QueryJSON = json.RawMessage(`{not valid}`)

	_, err := log.Append(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("want invalid-json error, got %v", err)
	}
}

func TestAppend_rejectsEmptyRerankerVersion(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()
	in := validInput()
	in.RerankerModelVersion = ""

	_, err := log.Append(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "empty reranker_model_version") {
		t.Fatalf("want empty-reranker error, got %v", err)
	}
}

func TestAppend_rejectsMalformedNodeUUID(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()
	in := validInput()
	in.NodeIDs = []string{
		"11111111-1111-1111-1111-111111111111",
		"not-a-uuid",
	}

	_, err := log.Append(context.Background(), in)
	if err == nil ||
		!strings.Contains(err.Error(), "node_ids[1]") ||
		!strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("want node_ids[1] not-valid-UUID error, got %v", err)
	}
}

func TestAppend_rejectsEmptyEdgeUUID(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()
	in := validInput()
	in.EdgeIDs = []string{
		"22222222-2222-2222-2222-222222222222",
		"",
	}

	_, err := log.Append(context.Background(), in)
	if err == nil ||
		!strings.Contains(err.Error(), "edge_ids[1]") ||
		!strings.Contains(err.Error(), "is empty") {
		t.Fatalf("want edge_ids[1] empty error, got %v", err)
	}
}

// ----- Append happy path & ordering --------------------------------

// TestAppend_preservesNodeIDsOrder_unit_sqlmock is the unit-test
// mirror of the integration "ordering preserved" scenario. It
// asserts the writer wires the input slice through pq.Array
// in input order by matching the exact PostgreSQL array literal
// `pq.Array` would have emitted -- `{"A","B","C"}` -- on the
// node_ids parameter slot. A buggy implementation that sorted
// or reversed the slice would surface as a sqlmock match
// failure rather than a deceptively-passing test.
func TestAppend_preservesNodeIDsOrder_unit_sqlmock(t *testing.T) {
	t.Parallel()
	log, mock, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()

	in := validInput()
	in.NodeIDs = []string{
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"cccccccc-cccc-cccc-cccc-cccccccccccc",
	}
	in.EdgeIDs = []string{
		"dddddddd-dddd-dddd-dddd-dddddddddddd",
	}
	in.ConceptIDs = []string{
		"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	}
	in.ServedUnderDegraded = true

	wantCtxID := "99999999-9999-9999-9999-999999999999"
	wantCreatedAt := time.Date(2025, 5, 15, 19, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO recall_context_log`).
		WithArgs(
			in.RepoID.String(),
			"recall",
			`{"q":"foo"}`,
			arrayArg{want: in.NodeIDs},
			arrayArg{want: in.EdgeIDs},
			arrayArg{want: in.ConceptIDs},
			"rerank-v1",
			true,
		).
		WillReturnRows(sqlmock.NewRows(
			[]string{"context_id", "created_at"},
		).AddRow(wantCtxID, wantCreatedAt))
	mock.ExpectCommit()

	rec, err := log.Append(context.Background(), in)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if rec.ContextID != wantCtxID {
		t.Errorf("ContextID = %q, want %q", rec.ContextID, wantCtxID)
	}
	if !rec.CreatedAt.Equal(wantCreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", rec.CreatedAt, wantCreatedAt)
	}
}

// TestAppend_nilSlicesEncodeAsEmptyArray confirms the
// `nonNil` helper kicks in: a nil Go slice on any of the
// uuid[] fields must encode as PostgreSQL's empty array `{}`
// rather than NULL (which the NOT NULL column would reject).
func TestAppend_nilSlicesEncodeAsEmptyArray(t *testing.T) {
	t.Parallel()
	log, mock, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()

	in := validInput() // NodeIDs / EdgeIDs / ConceptIDs all nil
	wantCtxID := "99999999-9999-9999-9999-999999999999"

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO recall_context_log`).
		WithArgs(
			in.RepoID.String(),
			"recall",
			`{"q":"foo"}`,
			arrayArg{want: []string{}}, // expect "{}" wire literal
			arrayArg{want: []string{}},
			arrayArg{want: []string{}},
			"rerank-v1",
			false,
		).
		WillReturnRows(sqlmock.NewRows(
			[]string{"context_id", "created_at"},
		).AddRow(wantCtxID, time.Now()))
	mock.ExpectCommit()

	if _, err := log.Append(context.Background(), in); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// TestAppend_writeContractViolation pins the SQLSTATE 42501
// classifier. The mock makes the INSERT return a *pq.Error
// with Code = 42501 (the role-grant denial); the writer must
// surface *WriteContractViolation with Op = "Append".
func TestAppend_writeContractViolation(t *testing.T) {
	t.Parallel()
	log, mock, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()

	in := validInput()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO recall_context_log`).
		WillReturnError(&pq.Error{
			Code:    pgErrCodeInsufficientPrivilege,
			Message: "permission denied for table recall_context_log",
		})
	mock.ExpectRollback()

	_, err := log.Append(context.Background(), in)
	var cv *WriteContractViolation
	if !errors.As(err, &cv) {
		t.Fatalf("want *WriteContractViolation, got %T: %v", err, err)
	}
	if cv.Op != "Append" {
		t.Errorf("Op = %q, want Append", cv.Op)
	}
	if cv.SQLState != pgErrCodeInsufficientPrivilege {
		t.Errorf("SQLState = %q, want %q", cv.SQLState, pgErrCodeInsufficientPrivilege)
	}
}

// ----- Resolve validation ------------------------------------------

func TestResolve_rejectsEmptyContextID(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()

	_, err := log.Resolve(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "empty context_id") {
		t.Fatalf("want empty-context_id error, got %v", err)
	}
}

func TestResolve_rejectsMalformedContextID(t *testing.T) {
	t.Parallel()
	log, _, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()

	_, err := log.Resolve(context.Background(), "not-a-uuid")
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("want invalid-UUID error, got %v", err)
	}
}

func TestResolve_notFoundSentinel(t *testing.T) {
	t.Parallel()
	log, mock, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()

	const ctxID = "99999999-9999-9999-9999-999999999999"
	mock.ExpectQuery(`SELECT[\s\S]+FROM recall_context_log`).
		WithArgs(ctxID).
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "repo_id", "verb", "query_json",
			"node_ids", "edge_ids", "concept_ids",
			"reranker_model_version", "served_under_degraded",
			"created_at",
		})) // zero rows

	_, err := log.Resolve(context.Background(), ctxID)
	if !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("want ErrContextNotFound, got %v", err)
	}
}

// TestResolve_multipleRowsSurfaceCorruption pins the LIMIT 2
// corruption guard. Composite PK (context_id, created_at) does
// NOT enforce global uniqueness on context_id alone; if a real
// collision ever happened the Resolve path must refuse to
// silently return the first row.
func TestResolve_multipleRowsSurfaceCorruption(t *testing.T) {
	t.Parallel()
	log, mock, cleanup := newMockLog(t, newFakeResolver())
	defer cleanup()

	const ctxID = "99999999-9999-9999-9999-999999999999"
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT[\s\S]+FROM recall_context_log`).
		WithArgs(ctxID).
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "repo_id", "verb", "query_json",
			"node_ids", "edge_ids", "concept_ids",
			"reranker_model_version", "served_under_degraded",
			"created_at",
		}).
			AddRow(ctxID, "11111111-1111-1111-1111-111111111111", "recall",
				"{}", "{}", "{}", "{}", "rerank-v1", false, now).
			AddRow(ctxID, "11111111-1111-1111-1111-111111111111", "recall",
				"{}", "{}", "{}", "{}", "rerank-v1", false, now))

	_, err := log.Resolve(context.Background(), ctxID)
	if err == nil || !strings.Contains(err.Error(), "matched >1 partition row") {
		t.Fatalf("want corruption error, got %v", err)
	}
}

// ----- Resolve ordering + IncludeRetired propagation -------------

// TestResolve_preservesNodeIDsOrder_unit_sqlmock is the unit-test
// mirror of the integration "ordering preserved" scenario. It
// stubs out the SELECT to return the recall_context_log row's
// arrays in [A,B,C] order, then verifies:
//
//  1. The fakeResolver's GetNode / GetEdge / GetConcept calls
//     land in exactly that order (the recallcontext.Resolve
//     loop must not sort / reorder).
//
//  2. Each Get* call propagates ReaderOptions.IncludeRetired = true
//     per the implementation-plan brief ("with IncludeRetired=true
//     so historical contexts are inspectable per risk §9.13").
//     A bug that defaulted to false would surface here, not as
//     a runtime regression in mgmt.read.context.
//
//  3. The returned ResolvedContext slices echo the input order
//     verbatim.
//
//  4. ServedUnderDegraded round-trips through the scan path
//     (covers the "degraded snapshot flag" scenario at the
//     unit layer).
func TestResolve_preservesNodeIDsOrder_unit_sqlmock(t *testing.T) {
	t.Parallel()
	r := newFakeResolver()
	log, mock, cleanup := newMockLog(t, r)
	defer cleanup()

	const ctxID = "11111111-2222-3333-4444-555555555555"
	const repoID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	nodeIDs := []string{
		"00000000-0000-0000-0000-000000000aaa",
		"00000000-0000-0000-0000-000000000bbb",
		"00000000-0000-0000-0000-000000000ccc",
	}
	edgeIDs := []string{
		"00000000-0000-0000-0000-000000000ee1",
		"00000000-0000-0000-0000-000000000ee2",
	}
	conceptIDs := []string{
		"00000000-0000-0000-0000-000000000fa1",
	}
	now := time.Date(2025, 5, 15, 12, 0, 0, 0, time.UTC)

	// Pre-populate the fake so the returned cards carry a
	// distinguishable marker per id (the id itself in NodeID).
	for _, id := range nodeIDs {
		r.nodes[id] = graphreader.Node{NodeID: id, Kind: "method"}
	}
	for _, id := range edgeIDs {
		r.edges[id] = graphreader.Edge{EdgeID: id, Kind: "static_calls"}
	}
	for _, id := range conceptIDs {
		r.concepts[id] = graphreader.Concept{ConceptID: id, Name: "concept-" + id}
	}

	// pq.Array(&[]string) scan format: when sqlmock returns a
	// string in the result column, lib/pq's StringArray scanner
	// parses the `{"a","b","c"}` literal back into the slice in
	// the same order. Build the literal here to mirror what the
	// driver would produce against a live PostgreSQL.
	pgArrayLit := func(ss []string) string {
		val, _ := pq.StringArray(ss).Value()
		return val.(string)
	}

	mock.ExpectQuery(`SELECT[\s\S]+FROM recall_context_log`).
		WithArgs(ctxID).
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "repo_id", "verb", "query_json",
			"node_ids", "edge_ids", "concept_ids",
			"reranker_model_version", "served_under_degraded",
			"created_at",
		}).AddRow(
			ctxID, repoID, "recall",
			`{"q":"hello"}`,
			pgArrayLit(nodeIDs),
			pgArrayLit(edgeIDs),
			pgArrayLit(conceptIDs),
			"rerank-v1",
			true,
			now,
		))

	got, err := log.Resolve(context.Background(), ctxID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// (1) Order of dereference calls must match the input id
	//     ordering AND the call shape per kind.
	var (
		wantCalls []resolverCall
	)
	for _, id := range nodeIDs {
		wantCalls = append(wantCalls, resolverCall{
			Kind: "node", ID: id, IncludeRetired: true,
		})
	}
	for _, id := range edgeIDs {
		wantCalls = append(wantCalls, resolverCall{
			Kind: "edge", ID: id, IncludeRetired: true,
		})
	}
	for _, id := range conceptIDs {
		// Concept has no IncludeRetired toggle (no retirement
		// table); the recorded call must reflect zero-value.
		wantCalls = append(wantCalls, resolverCall{
			Kind: "concept", ID: id, IncludeRetired: false,
		})
	}
	if len(r.calls) != len(wantCalls) {
		t.Fatalf("got %d resolver calls, want %d (%+v)",
			len(r.calls), len(wantCalls), r.calls)
	}
	for i := range wantCalls {
		if r.calls[i] != wantCalls[i] {
			t.Errorf("call[%d] = %+v, want %+v", i, r.calls[i], wantCalls[i])
		}
	}

	// (2) IncludeRetired propagation -- already covered by the
	//     call-tuple assertion above.

	// (3) The returned slices preserve input order verbatim.
	if len(got.Nodes) != len(nodeIDs) {
		t.Fatalf("Nodes len = %d, want %d", len(got.Nodes), len(nodeIDs))
	}
	for i, id := range nodeIDs {
		if got.Nodes[i].NodeID != id {
			t.Errorf("Nodes[%d].NodeID = %q, want %q",
				i, got.Nodes[i].NodeID, id)
		}
	}
	if len(got.Edges) != len(edgeIDs) {
		t.Fatalf("Edges len = %d, want %d", len(got.Edges), len(edgeIDs))
	}
	for i, id := range edgeIDs {
		if got.Edges[i].EdgeID != id {
			t.Errorf("Edges[%d].EdgeID = %q, want %q",
				i, got.Edges[i].EdgeID, id)
		}
	}
	if len(got.Concepts) != len(conceptIDs) {
		t.Fatalf("Concepts len = %d, want %d",
			len(got.Concepts), len(conceptIDs))
	}
	for i, id := range conceptIDs {
		if got.Concepts[i].ConceptID != id {
			t.Errorf("Concepts[%d].ConceptID = %q, want %q",
				i, got.Concepts[i].ConceptID, id)
		}
	}

	// (4) Degraded flag round-tripped through the SELECT.
	if !got.ServedUnderDegraded {
		t.Errorf("ServedUnderDegraded = false, want true")
	}
	if got.ContextID != ctxID {
		t.Errorf("ContextID = %q, want %q", got.ContextID, ctxID)
	}
	if got.RepoID != repoID {
		t.Errorf("RepoID = %q, want %q", got.RepoID, repoID)
	}
	if got.Verb != "recall" {
		t.Errorf("Verb = %q, want recall", got.Verb)
	}
	if got.RerankerModelVersion != "rerank-v1" {
		t.Errorf("RerankerModelVersion = %q", got.RerankerModelVersion)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}
	if string(got.QueryJSON) != `{"q":"hello"}` {
		t.Errorf("QueryJSON = %q", string(got.QueryJSON))
	}
}

// TestResolve_missingReferencedNodeSurfacesCorruption asserts
// the corruption-detection branch: when the log row references
// a node that the GraphReader cannot find, Resolve returns an
// error that satisfies BOTH `errors.Is(err, ErrContextNotFound)`
// (so the mgmt-api layer can render a consistent "context
// partly unavailable" message without re-classifying) AND
// `errors.Is(err, graphreader.ErrNotFound)` (so callers that
// want to drill into "which entity vanished" can route the
// error to a different alert pipeline per the dual-sentinel
// contract documented at log.go ErrContextNotFound).
func TestResolve_missingReferencedNodeSurfacesCorruption(t *testing.T) {
	t.Parallel()
	r := newFakeResolver()
	r.missingNode = true // every GetNode call returns ErrNotFound
	log, mock, cleanup := newMockLog(t, r)
	defer cleanup()

	const ctxID = "11111111-2222-3333-4444-555555555555"
	nodeIDs := []string{"00000000-0000-0000-0000-000000000aaa"}
	pgArrayLit := func(ss []string) string {
		val, _ := pq.StringArray(ss).Value()
		return val.(string)
	}

	mock.ExpectQuery(`SELECT[\s\S]+FROM recall_context_log`).
		WithArgs(ctxID).
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "repo_id", "verb", "query_json",
			"node_ids", "edge_ids", "concept_ids",
			"reranker_model_version", "served_under_degraded",
			"created_at",
		}).AddRow(
			ctxID, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "recall",
			`{}`,
			pgArrayLit(nodeIDs),
			"{}", "{}",
			"rerank-v1",
			false,
			time.Now(),
		))

	_, err := log.Resolve(context.Background(), ctxID)
	if !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("want errors.Is(err, ErrContextNotFound), got %v", err)
	}
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("want errors.Is(err, graphreader.ErrNotFound), got %v", err)
	}
	if !strings.Contains(err.Error(), "node id") {
		t.Errorf("error should name the offending kind, got %v", err)
	}
}

// TestResolve_missingReferencedEdgeSurfacesCorruption mirrors
// TestResolve_missingReferencedNodeSurfacesCorruption for the
// edge dereference branch. Pinned separately so a regression
// to the edge loop's error-wrapping (e.g. dropping one of the
// two %w verbs) cannot pass with the node-branch test as the
// sole signal.
func TestResolve_missingReferencedEdgeSurfacesCorruption(t *testing.T) {
	t.Parallel()
	r := newFakeResolver()
	r.missingEdge = true
	log, mock, cleanup := newMockLog(t, r)
	defer cleanup()

	const ctxID = "22222222-3333-4444-5555-666666666666"
	edgeIDs := []string{"00000000-0000-0000-0000-000000000ee1"}
	pgArrayLit := func(ss []string) string {
		val, _ := pq.StringArray(ss).Value()
		return val.(string)
	}

	mock.ExpectQuery(`SELECT[\s\S]+FROM recall_context_log`).
		WithArgs(ctxID).
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "repo_id", "verb", "query_json",
			"node_ids", "edge_ids", "concept_ids",
			"reranker_model_version", "served_under_degraded",
			"created_at",
		}).AddRow(
			ctxID, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "recall",
			`{}`,
			"{}",
			pgArrayLit(edgeIDs),
			"{}",
			"rerank-v1",
			false,
			time.Now(),
		))

	_, err := log.Resolve(context.Background(), ctxID)
	if !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("want errors.Is(err, ErrContextNotFound), got %v", err)
	}
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("want errors.Is(err, graphreader.ErrNotFound), got %v", err)
	}
	if !strings.Contains(err.Error(), "edge id") {
		t.Errorf("error should name the offending kind, got %v", err)
	}
}

// TestResolve_missingReferencedConceptSurfacesCorruption mirrors
// the previous two tests for the concept dereference branch
// (which goes through GetConcept, sans ReaderOptions, per the
// no-retirement design in graphreader/concept.go). Pinned
// separately for the same reason as the edge variant.
func TestResolve_missingReferencedConceptSurfacesCorruption(t *testing.T) {
	t.Parallel()
	r := newFakeResolver()
	r.missingConcept = true
	log, mock, cleanup := newMockLog(t, r)
	defer cleanup()

	const ctxID = "33333333-4444-5555-6666-777777777777"
	conceptIDs := []string{"00000000-0000-0000-0000-000000000fa1"}
	pgArrayLit := func(ss []string) string {
		val, _ := pq.StringArray(ss).Value()
		return val.(string)
	}

	mock.ExpectQuery(`SELECT[\s\S]+FROM recall_context_log`).
		WithArgs(ctxID).
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "repo_id", "verb", "query_json",
			"node_ids", "edge_ids", "concept_ids",
			"reranker_model_version", "served_under_degraded",
			"created_at",
		}).AddRow(
			ctxID, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "recall",
			`{}`,
			"{}", "{}",
			pgArrayLit(conceptIDs),
			"rerank-v1",
			false,
			time.Now(),
		))

	_, err := log.Resolve(context.Background(), ctxID)
	if !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("want errors.Is(err, ErrContextNotFound), got %v", err)
	}
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("want errors.Is(err, graphreader.ErrNotFound), got %v", err)
	}
	if !strings.Contains(err.Error(), "concept id") {
		t.Errorf("error should name the offending kind, got %v", err)
	}
}

// ----- Misc plumbing -----------------------------------------------

// TestWriteContractViolation_unwrapAndIsAs makes sure the typed
// error participates in the standard errors.As / errors.Is
// machinery. Mirrors the equivalent test in the graphwriter
// package by intent.
func TestWriteContractViolation_unwrapAndIsAs(t *testing.T) {
	t.Parallel()
	inner := &pq.Error{Code: pgErrCodeInsufficientPrivilege, Message: "denied"}
	wrapped := &WriteContractViolation{
		Op:       "Append",
		SQLState: pgErrCodeInsufficientPrivilege,
		Err:      inner,
	}
	var target *WriteContractViolation
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As must extract *WriteContractViolation")
	}
	if target.Op != "Append" {
		t.Errorf("Op = %q, want Append", target.Op)
	}
	var innerTarget *pq.Error
	if !errors.As(wrapped, &innerTarget) {
		t.Fatal("errors.As must reach the wrapped *pq.Error")
	}
}
