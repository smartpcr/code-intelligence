package retirement

// Behavioural unit tests driven through go-sqlmock. These cover
// the two Stage 2.3 acceptance scenarios at the unit-test layer
// with no environment gate, complementing the live-DB tests in
// service_integration_test.go (which remain in place to catch
// schema / role-grant drift):
//
//   1. Second retirement of the same id surfaces *AlreadyRetired
//      (PostgreSQL SQLSTATE 23505 on the UNIQUE index).
//
//   2. RetireNode with a non-empty SupersededByNodeID binds the
//      `superseded_by_node_id` column on the INSERT; this is the
//      database half of the "rename retirement links new node"
//      scenario. The parallel `renamed_to` Edge insertion is the
//      caller's responsibility (architecture.md §5.2.4) -- a
//      retirement-side unit test cannot drive that step because
//      Edge inserts go through the graphwriter package.
//
// sqlmock gives us a real *sql.DB backed by a fake driver, so the
// service's runInTx / assertNodeExists / classifyErr plumbing
// runs end-to-end -- only the PostgreSQL wire layer is mocked.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// newMockService returns a *Service wired to a sqlmock-backed
// *sql.DB and a silent logger. The returned cleanup verifies all
// expectations were met and closes the DB.
func newMockService(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherRegexp,
	))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	svc := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// TestRetireNode_secondRetirementReturnsAlreadyRetired_unit_sqlmock
// is the unit-test mirror of the integration scenario "double
// retirement rejected". The first RetireNode call succeeds; the
// second call's INSERT trips the UNIQUE index on
// node_retirement(node_id) and PostgreSQL returns SQLSTATE 23505.
// The service must surface *AlreadyRetired with the offending
// node_id populated so callers can pattern-match on it.
func TestRetireNode_secondRetirementReturnsAlreadyRetired_unit_sqlmock(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newMockService(t)
	defer cleanup()

	const nodeID = "11111111-1111-1111-1111-111111111111"
	const sha = "deadbeefcafef00d"

	// First call: succeeds.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM node WHERE node_id = \$1`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{"col"}).AddRow(1))
	mock.ExpectQuery(`INSERT INTO node_retirement`).
		WithArgs(nodeID, sha, sql.NullString{}).
		WillReturnRows(sqlmock.NewRows(
			[]string{"retirement_id", "retired_at"},
		).AddRow("22222222-2222-2222-2222-222222222222", time.Now()))
	mock.ExpectCommit()

	if _, err := svc.RetireNode(context.Background(), NodeRetirementInput{
		NodeID: nodeID, RetiredAtSHA: sha,
	}); err != nil {
		t.Fatalf("first RetireNode: %v", err)
	}

	// Second call: pre-check still finds the node (append-only),
	// INSERT trips UNIQUE -> SQLSTATE 23505 -> *AlreadyRetired.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM node WHERE node_id = \$1`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{"col"}).AddRow(1))
	mock.ExpectQuery(`INSERT INTO node_retirement`).
		WithArgs(nodeID, sha, sql.NullString{}).
		WillReturnError(&pq.Error{
			Code:       pgErrCodeUniqueViolation,
			Constraint: "node_retirement_node_id_uidx",
			Detail:     "Key (node_id)=(" + nodeID + ") already exists.",
		})
	mock.ExpectRollback()

	_, err := svc.RetireNode(context.Background(), NodeRetirementInput{
		NodeID: nodeID, RetiredAtSHA: sha,
	})
	if err == nil {
		t.Fatal("second RetireNode: want error, got nil")
	}
	var already *AlreadyRetired
	if !errors.As(err, &already) {
		t.Fatalf("second RetireNode: want *AlreadyRetired, got %T: %v", err, err)
	}
	if already.TargetID != nodeID {
		t.Errorf("AlreadyRetired.TargetID = %q, want %q",
			already.TargetID, nodeID)
	}
	if already.Kind != KindNode {
		t.Errorf("AlreadyRetired.Kind = %q, want %q",
			already.Kind, KindNode)
	}
}

// TestRetireEdge_secondRetirementReturnsAlreadyRetired_unit_sqlmock
// is the edge-side mirror of the above. Edges share the same
// SQLSTATE 23505 classifier path; this test guards the
// independent UNIQUE index on edge_retirement(edge_id).
func TestRetireEdge_secondRetirementReturnsAlreadyRetired_unit_sqlmock(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newMockService(t)
	defer cleanup()

	const edgeID = "33333333-3333-3333-3333-333333333333"
	const sha = "feedfacecafebeef"

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM edge WHERE edge_id = \$1`).
		WithArgs(edgeID).
		WillReturnRows(sqlmock.NewRows([]string{"col"}).AddRow(1))
	mock.ExpectQuery(`INSERT INTO edge_retirement`).
		WithArgs(edgeID, sha).
		WillReturnError(&pq.Error{
			Code:       pgErrCodeUniqueViolation,
			Constraint: "edge_retirement_edge_id_uidx",
		})
	mock.ExpectRollback()

	_, err := svc.RetireEdge(context.Background(), EdgeRetirementInput{
		EdgeID: edgeID, RetiredAtSHA: sha,
	})
	var already *AlreadyRetired
	if !errors.As(err, &already) {
		t.Fatalf("want *AlreadyRetired, got %T: %v", err, err)
	}
	if already.Kind != KindEdge {
		t.Errorf("Kind = %q, want %q", already.Kind, KindEdge)
	}
	if already.TargetID != edgeID {
		t.Errorf("TargetID = %q, want %q", already.TargetID, edgeID)
	}
}

// TestRetireNode_supersedeIsBoundInInsert_unit_sqlmock is the
// database half of the Stage 2.3 "rename retirement links new
// node" scenario, asserted at the unit-test layer.
//
// When RetireNode is invoked with a non-empty SupersededByNodeID
// the service MUST:
//
//	(a) run a second `SELECT 1 FROM node WHERE node_id = $1` to
//	    prove the replacement node exists (so the FK never trips
//	    at INSERT time), and
//	(b) bind that id as the third positional argument on the
//	    INSERT into node_retirement(node_id, retired_at_sha,
//	    superseded_by_node_id).
//
// sqlmock's WithArgs check on the INSERT enforces (b); the
// ordered SELECT expectations enforce (a). The companion
// `renamed_to` Edge insertion is the caller's responsibility
// (architecture.md §5.2.4) and is exercised separately by the
// integration test against a live database.
func TestRetireNode_supersedeIsBoundInInsert_unit_sqlmock(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newMockService(t)
	defer cleanup()

	const oldNodeID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const newNodeID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const sha = "abc1234def5678"

	mock.ExpectBegin()
	// Pre-check on the target.
	mock.ExpectQuery(`SELECT 1 FROM node WHERE node_id = \$1`).
		WithArgs(oldNodeID).
		WillReturnRows(sqlmock.NewRows([]string{"col"}).AddRow(1))
	// Pre-check on the supersede id -- this is the second SELECT
	// the service issues only when SupersededByNodeID is non-empty.
	mock.ExpectQuery(`SELECT 1 FROM node WHERE node_id = \$1`).
		WithArgs(newNodeID).
		WillReturnRows(sqlmock.NewRows([]string{"col"}).AddRow(1))
	// INSERT must bind the supersede id as the third positional arg.
	mock.ExpectQuery(`INSERT INTO node_retirement`).
		WithArgs(
			oldNodeID,
			sha,
			sql.NullString{String: newNodeID, Valid: true},
		).
		WillReturnRows(sqlmock.NewRows(
			[]string{"retirement_id", "retired_at"},
		).AddRow("cccccccc-cccc-cccc-cccc-cccccccccccc", time.Now()))
	mock.ExpectCommit()

	rec, err := svc.RetireNode(context.Background(), NodeRetirementInput{
		NodeID:             oldNodeID,
		RetiredAtSHA:       sha,
		SupersededByNodeID: newNodeID,
	})
	if err != nil {
		t.Fatalf("RetireNode: %v", err)
	}
	if rec.SupersededByNodeID != newNodeID {
		t.Errorf("rec.SupersededByNodeID = %q, want %q",
			rec.SupersededByNodeID, newNodeID)
	}
	if rec.NodeID != oldNodeID {
		t.Errorf("rec.NodeID = %q, want %q", rec.NodeID, oldNodeID)
	}
}

// TestRetireNode_omittedSupersedeBindsNullString_unit_sqlmock pins
// the negative half of the supersede contract: when the caller
// passes an empty SupersededByNodeID the INSERT must bind a
// non-Valid sql.NullString (NULL on the wire), and the
// supersede-side pre-check SELECT must NOT be issued -- that
// would be a wasted round-trip on the hot path for outright
// deletions (most retirements are deletions, not renames).
func TestRetireNode_omittedSupersedeBindsNullString_unit_sqlmock(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newMockService(t)
	defer cleanup()

	const nodeID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	const sha = "0123456789abcdef"

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM node WHERE node_id = \$1`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{"col"}).AddRow(1))
	// Exactly one SELECT (no supersede pre-check), then the INSERT
	// with a non-Valid NullString in the third slot.
	mock.ExpectQuery(`INSERT INTO node_retirement`).
		WithArgs(nodeID, sha, sql.NullString{Valid: false}).
		WillReturnRows(sqlmock.NewRows(
			[]string{"retirement_id", "retired_at"},
		).AddRow("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", time.Now()))
	mock.ExpectCommit()

	rec, err := svc.RetireNode(context.Background(), NodeRetirementInput{
		NodeID: nodeID, RetiredAtSHA: sha,
	})
	if err != nil {
		t.Fatalf("RetireNode: %v", err)
	}
	if rec.SupersededByNodeID != "" {
		t.Errorf("rec.SupersededByNodeID = %q, want empty",
			rec.SupersededByNodeID)
	}
}

// TestRetireMany_unit_sqlmock proves the node-batch entry point
// builds a single multi-row INSERT through `unnest($1::uuid[])`
// and surfaces *AlreadyRetired on the batch-level UNIQUE
// violation, with TargetID intentionally empty per the doc
// comment.
func TestRetireMany_unit_sqlmock(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newMockService(t)
	defer cleanup()

	const sha = "batch-sha"
	ids := []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
	}

	// Happy path: one batch INSERT, both rows RETURNed.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO node_retirement`).
		WithArgs(sqlmock.AnyArg(), sha).
		WillReturnRows(sqlmock.NewRows(
			[]string{"retirement_id", "node_id", "retired_at"},
		).
			AddRow("r1", ids[0], time.Now()).
			AddRow("r2", ids[1], time.Now()))
	mock.ExpectCommit()

	res, err := svc.RetireMany(context.Background(), ids, sha)
	if err != nil {
		t.Fatalf("RetireMany happy path: %v", err)
	}
	if res.InsertedCount != 2 {
		t.Errorf("InsertedCount = %d, want 2", res.InsertedCount)
	}

	// Duplicate path: batch INSERT trips UNIQUE -> *AlreadyRetired
	// with empty TargetID.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO node_retirement`).
		WithArgs(sqlmock.AnyArg(), sha).
		WillReturnError(&pq.Error{
			Code:       pgErrCodeUniqueViolation,
			Constraint: "node_retirement_node_id_uidx",
		})
	mock.ExpectRollback()

	_, err = svc.RetireMany(context.Background(), ids, sha)
	var already *AlreadyRetired
	if !errors.As(err, &already) {
		t.Fatalf("want *AlreadyRetired, got %T: %v", err, err)
	}
	if already.TargetID != "" {
		t.Errorf("AlreadyRetired.TargetID = %q, want empty (batch path)",
			already.TargetID)
	}
}

// TestRetireManyEdges_unit_sqlmock is the edge-side mirror; it
// pins that the new edge-batch entry point exists, issues exactly
// one INSERT INTO edge_retirement, and maps the unique-violation
// SQLSTATE to *AlreadyRetired the same way the node batch does.
func TestRetireManyEdges_unit_sqlmock(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newMockService(t)
	defer cleanup()

	const sha = "edge-batch-sha"
	ids := []string{
		"55555555-5555-5555-5555-555555555555",
		"66666666-6666-6666-6666-666666666666",
	}

	// Happy path.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO edge_retirement`).
		WithArgs(sqlmock.AnyArg(), sha).
		WillReturnRows(sqlmock.NewRows(
			[]string{"retirement_id", "edge_id", "retired_at"},
		).
			AddRow("er1", ids[0], time.Now()).
			AddRow("er2", ids[1], time.Now()))
	mock.ExpectCommit()

	res, err := svc.RetireManyEdges(context.Background(), ids, sha)
	if err != nil {
		t.Fatalf("RetireManyEdges happy path: %v", err)
	}
	if res.InsertedCount != 2 {
		t.Errorf("InsertedCount = %d, want 2", res.InsertedCount)
	}
	if res.Records[0].EdgeID != ids[0] {
		t.Errorf("Records[0].EdgeID = %q, want %q",
			res.Records[0].EdgeID, ids[0])
	}

	// Duplicate path.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO edge_retirement`).
		WithArgs(sqlmock.AnyArg(), sha).
		WillReturnError(&pq.Error{
			Code:       pgErrCodeUniqueViolation,
			Constraint: "edge_retirement_edge_id_uidx",
		})
	mock.ExpectRollback()

	_, err = svc.RetireManyEdges(context.Background(), ids, sha)
	var already *AlreadyRetired
	if !errors.As(err, &already) {
		t.Fatalf("want *AlreadyRetired, got %T: %v", err, err)
	}
	if already.Kind != KindEdge {
		t.Errorf("Kind = %q, want %q", already.Kind, KindEdge)
	}
}

// TestRetireManyEdges_validatesArgs covers the input-validation
// half of the new edge-batch entry point: empty sha, empty id at
// some index, empty batch (no-op).
func TestRetireManyEdges_validatesArgs(t *testing.T) {
	t.Parallel()
	svc := &Service{
		db:     nil,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	cases := []struct {
		name    string
		ids     []string
		sha     string
		wantErr string
	}{
		{"empty sha", []string{"id"}, "", "empty retired_at_sha"},
		{"empty id in slice", []string{""}, "sha", "edgeIDs[0] is empty"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := svc.RetireManyEdges(context.Background(), tc.ids, tc.sha)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
	// Empty batch is a no-op success.
	if _, err := svc.RetireManyEdges(context.Background(), nil, "sha"); err != nil {
		t.Errorf("empty batch: want nil err, got %v", err)
	}
}
