package snapshot

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// silentLogger discards log output for the test runner so
// per-call structured logs do not clutter `go test` output.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const (
	testRepoID         = "11111111-2222-3333-4444-555555555555"
	testModelVersion   = "stub@v1"
	testPriorPublishID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1"
	testNodeID         = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb1"
	testNewPublishID   = "cccccccc-cccc-cccc-cccc-ccccccccccc1"
	testQdrantPointID  = "dddddddd-dddd-dddd-dddd-ddddddddddd1"
	testConceptVID     = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1"
)

// newTestService wires a Service with sqlmock and deterministic
// snapshot-id / point-id factories so tests can assert on the
// exact arguments the underlying queries received.
func newTestService(t *testing.T, snapshotID, pointID string) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	svc := New(db, testModelVersion,
		WithLogger(silentLogger()),
		WithSnapshotIDFactory(func() (string, error) { return snapshotID, nil }),
	)
	_ = pointID
	return svc, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// expectAssertRepoExists queues the repo-existence probe
// the Service runs at the top of Snapshot.
func expectAssertRepoExists(mock sqlmock.Sqlmock, repoID string, exists bool) {
	q := mock.ExpectQuery(`SELECT 1 FROM repo WHERE repo_id = \$1::uuid`).WithArgs(repoID)
	if exists {
		q.WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	} else {
		q.WillReturnError(sql.ErrNoRows)
	}
}

// nodeTargetRow returns a sqlmock Rows shape compatible with
// scanNodeTargets's SELECT (iter-2 shape: 6 cols incl. in_flight).
func nodeTargetRow(nodeID, kind, signature, priorPublishID, detailsJSON string, inFlight bool) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{
		"node_id", "kind", "canonical_signature", "publish_id", "details_json", "in_flight",
	}).AddRow(nodeID, kind, signature, priorPublishID, detailsJSON, inFlight)
	return r
}

// emptyNodeRows is the rows shape with no data.
func emptyNodeRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"node_id", "kind", "canonical_signature", "publish_id", "details_json", "in_flight",
	})
}

// emptyConceptRows mirrors emptyNodeRows for the concept scan.
func emptyConceptRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"concept_version_id", "canonical_signature", "publish_id", "details_json", "in_flight",
	})
}

// conceptTargetRow is the concept-side equivalent of
// nodeTargetRow (iter-2 shape: 5 cols incl. in_flight).
func conceptTargetRow(cvID, signature, priorPublishID, detailsJSON string, inFlight bool) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{
		"concept_version_id", "canonical_signature", "publish_id", "details_json", "in_flight",
	}).AddRow(cvID, signature, priorPublishID, detailsJSON, inFlight)
	return r
}

// expectNodeScan queues the LATERAL-joined node-target scan
// with the supplied rows.
func expectNodeScan(mock sqlmock.Sqlmock, repoID string, rows *sqlmock.Rows) {
	mock.ExpectQuery(`FROM node n\s+JOIN LATERAL`).
		WithArgs(repoID).
		WillReturnRows(rows)
}

// expectConceptScan queues the concept-target scan.
func expectConceptScan(mock sqlmock.Sqlmock, repoID string, rows *sqlmock.Rows) {
	mock.ExpectQuery(`FROM concept_version cv\s+JOIN concept c`).
		WithArgs(repoID).
		WillReturnRows(rows)
}

// expectAdvisoryLock queues the per-prior_publish_id advisory
// xact lock the per-target tx takes immediately after BEGIN
// (iter-2 fix #1).
func expectAdvisoryLock(mock sqlmock.Sqlmock, priorPublishID string) {
	mock.ExpectExec(`pg_advisory_xact_lock\(hashtextextended\('snapshot:supersede:' \|\| \$1::text, 0\)\)`).
		WithArgs(priorPublishID).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

// expectDedupeNoDup queues a "no prior in-flight snapshot"
// dedupe-probe result for a node target.
func expectDedupeNoDup(mock sqlmock.Sqlmock, nodeID, priorPublishID string) {
	mock.ExpectQuery(`FROM embedding_publish ep\s+JOIN LATERAL[\s\S]+supersedes_publish_id`).
		WithArgs(nodeID, priorPublishID).
		WillReturnError(sql.ErrNoRows)
}

// expectDedupeFoundDup queues a dedupe-probe result indicating
// a snapshot is already in flight for this target.
func expectDedupeFoundDup(mock sqlmock.Sqlmock, nodeID, priorPublishID string) {
	mock.ExpectQuery(`FROM embedding_publish ep\s+JOIN LATERAL[\s\S]+supersedes_publish_id`).
		WithArgs(nodeID, priorPublishID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
}

// queuedDetailsMatcher is a sqlmock argument matcher that
// asserts the queued event's JSONB body has the snapshot-
// required keys + values. Iter-2 fix #5: replaces the
// permissive sqlmock.AnyArg() with an explicit field-by-field
// check so a future regression that drops snapshot_id or
// supersedes_publish_id from the body breaks the test.
type queuedDetailsMatcher struct {
	WantContent             string
	WantSignatureOnly       bool
	WantModelVersion        string
	WantSnapshotID          string
	WantSupersedesPublishID string
}

func (m queuedDetailsMatcher) Match(v driver.Value) bool {
	var raw string
	switch tv := v.(type) {
	case string:
		raw = tv
	case []byte:
		raw = string(tv)
	default:
		return false
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return false
	}
	if s, _ := got["content"].(string); s != m.WantContent {
		return false
	}
	if b, _ := got["signature_only"].(bool); b != m.WantSignatureOnly {
		return false
	}
	if s, _ := got["embedding_model_version"].(string); s != m.WantModelVersion {
		return false
	}
	if s, _ := got["snapshot_id"].(string); s != m.WantSnapshotID {
		return false
	}
	if s, _ := got["supersedes_publish_id"].(string); s != m.WantSupersedesPublishID {
		return false
	}
	return true
}

// driverValue is the driver.Value type alias used by
// sqlmock's Match interface. Re-declared here to avoid
// importing database/sql/driver in every test.
type driverValue = driver.Value

// expectNodeInsertWithBody queues the publish + queued event
// INSERT pair with strict assertions on the JSONB body.
func expectNodeInsertWithBody(mock sqlmock.Sqlmock, nodeID, newPublishID string,
	body queuedDetailsMatcher,
) {
	mock.ExpectQuery(`INSERT INTO embedding_publish\s*\(node_id, embedding_model_version, qdrant_point_id\)`).
		WithArgs(nodeID, testModelVersion, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"publish_id"}).AddRow(newPublishID))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'queued'::embedding_publish_event_kind`).
		WithArgs(newPublishID, body).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectConceptInsertWithBody is the concept-side equivalent.
func expectConceptInsertWithBody(mock sqlmock.Sqlmock, cvID, newPublishID string,
	body queuedDetailsMatcher,
) {
	mock.ExpectQuery(`INSERT INTO embedding_publish\s*\(concept_version_id, embedding_model_version, qdrant_point_id\)`).
		WithArgs(cvID, testModelVersion, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"publish_id"}).AddRow(newPublishID))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'queued'::embedding_publish_event_kind`).
		WithArgs(newPublishID, body).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// conceptQueuedDetailsMatcher is the iter-3-fix-#1 concept-
// shape sqlmock matcher. The snapshot service now writes
// concept-shape queued JSONB (name / description_md /
// fingerprint / concept_id / concept_version_id + snapshot
// discriminators), NOT the node-shape body produced by
// `queuedDetailsMatcher`. Field-by-field assertion catches
// any regression that drops the concept identity fields or
// omits the snapshot discriminators.
type conceptQueuedDetailsMatcher struct {
	WantConceptID           string
	WantConceptVersionID    string
	WantName                string
	WantDescriptionMD       string
	WantFingerprint         string
	WantModelVersion        string
	WantSnapshotID          string
	WantSupersedesPublishID string
	// AssertNoContentKey, when true, fails the match if the
	// emitted JSONB contains a `content` key — proves the
	// snapshot service is NOT leaking the node shape into
	// concept events.
	AssertNoContentKey bool
}

func (m conceptQueuedDetailsMatcher) Match(v driver.Value) bool {
	var raw string
	switch tv := v.(type) {
	case string:
		raw = tv
	case []byte:
		raw = string(tv)
	default:
		return false
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return false
	}
	if s, _ := got["concept_id"].(string); s != m.WantConceptID {
		return false
	}
	if s, _ := got["concept_version_id"].(string); s != m.WantConceptVersionID {
		return false
	}
	if s, _ := got["name"].(string); s != m.WantName {
		return false
	}
	if s, _ := got["description_md"].(string); s != m.WantDescriptionMD {
		return false
	}
	if s, _ := got["fingerprint"].(string); s != m.WantFingerprint {
		return false
	}
	if s, _ := got["embedding_model_version"].(string); s != m.WantModelVersion {
		return false
	}
	if s, _ := got["snapshot_id"].(string); s != m.WantSnapshotID {
		return false
	}
	if s, _ := got["supersedes_publish_id"].(string); s != m.WantSupersedesPublishID {
		return false
	}
	if m.AssertNoContentKey {
		if _, has := got["content"]; has {
			return false
		}
	}
	return true
}

// expectConceptInsertWithConceptBody is the concept-shape
// version of `expectConceptInsertWithBody`. Used by the
// iter-3 concept tests that exercise the real promoter
// queued-event shape.
func expectConceptInsertWithConceptBody(mock sqlmock.Sqlmock, cvID, newPublishID string,
	body conceptQueuedDetailsMatcher,
) {
	mock.ExpectQuery(`INSERT INTO embedding_publish\s*\(concept_version_id, embedding_model_version, qdrant_point_id\)`).
		WithArgs(cvID, testModelVersion, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"publish_id"}).AddRow(newPublishID))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'queued'::embedding_publish_event_kind`).
		WithArgs(newPublishID, body).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// -----------------------------------------------------------
// Scenario: empty repo -> Result with zero counts
// -----------------------------------------------------------

func TestSnapshot_emptyRepo_returnsZeroCounts(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-empty", "")
	defer cleanup()

	expectAssertRepoExists(mock, testRepoID, true)
	expectNodeScan(mock, testRepoID, emptyNodeRows())
	expectConceptScan(mock, testRepoID, emptyConceptRows())

	res, err := svc.Snapshot(context.Background(), testRepoID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if res.MethodsEnqueued != 0 || res.BlocksEnqueued != 0 || res.ConceptsEnqueued != 0 {
		t.Errorf("expected zero counts, got %+v", res)
	}
	if res.SnapshotID != "snap-empty" {
		t.Errorf("snapshot_id = %q, want snap-empty", res.SnapshotID)
	}
	if res.ModelVersion != testModelVersion {
		t.Errorf("model_version = %q, want %q", res.ModelVersion, testModelVersion)
	}
	if svc.Metrics().PendingTotal() != 0 {
		t.Errorf("pending counter = %d, want 0", svc.Metrics().PendingTotal())
	}
}

// -----------------------------------------------------------
// Scenario: unknown repo -> ErrRepoNotFound
// -----------------------------------------------------------

func TestSnapshot_unknownRepo_returnsSentinel(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-x", "")
	defer cleanup()

	expectAssertRepoExists(mock, testRepoID, false)

	_, err := svc.Snapshot(context.Background(), testRepoID)
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

// -----------------------------------------------------------
// Scenario: one method node, no concept -> writes one publish
// row + queued event with supersede metadata. Asserts the
// JSONB body shape via queuedDetailsMatcher (iter-2 fix #5).
// -----------------------------------------------------------

func TestSnapshot_oneMethodTarget_writesPublishAndQueued(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-1method", "")
	defer cleanup()

	expectAssertRepoExists(mock, testRepoID, true)
	priorDetails := `{"content":"func main() {}","signature_only":false,"embedding_model_version":"old@v0"}`
	expectNodeScan(mock, testRepoID,
		nodeTargetRow(testNodeID, "method", "pkg::main()", testPriorPublishID, priorDetails, false))
	mock.ExpectBegin()
	expectAdvisoryLock(mock, testPriorPublishID)
	expectDedupeNoDup(mock, testNodeID, testPriorPublishID)
	expectNodeInsertWithBody(mock, testNodeID, testNewPublishID, queuedDetailsMatcher{
		WantContent:             "func main() {}",
		WantSignatureOnly:       false,
		WantModelVersion:        testModelVersion,
		WantSnapshotID:          "snap-1method",
		WantSupersedesPublishID: testPriorPublishID,
	})
	mock.ExpectCommit()
	expectConceptScan(mock, testRepoID, emptyConceptRows())

	res, err := svc.Snapshot(context.Background(), testRepoID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if res.MethodsEnqueued != 1 {
		t.Errorf("methods_enqueued = %d, want 1", res.MethodsEnqueued)
	}
	if res.BlocksEnqueued != 0 || res.ConceptsEnqueued != 0 {
		t.Errorf("expected only method count to fire, got %+v", res)
	}
	if got := svc.Metrics().PendingTotal(); got != 1 {
		t.Errorf("pending counter = %d, want 1", got)
	}
}

// -----------------------------------------------------------
// Scenario: prior queued event missing content -> target
// skipped (cannot reconstruct the body to re-embed).
// -----------------------------------------------------------

func TestSnapshot_priorContentMissing_skipsTarget(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-missing", "")
	defer cleanup()

	expectAssertRepoExists(mock, testRepoID, true)
	// Empty details_json -> Content == "" -> skipped path.
	expectNodeScan(mock, testRepoID,
		nodeTargetRow(testNodeID, "method", "pkg::orphan()", testPriorPublishID, "", false))
	// No BEGIN / dedupe / insert expected because the empty-content
	// short-circuit fires before any DB write.
	expectConceptScan(mock, testRepoID, emptyConceptRows())

	res, err := svc.Snapshot(context.Background(), testRepoID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if res.MethodsEnqueued != 0 {
		t.Errorf("methods_enqueued = %d, want 0 (target was skipped)", res.MethodsEnqueued)
	}
	if res.MethodBlocksSkipped != 1 {
		t.Errorf("method_blocks_skipped = %d, want 1", res.MethodBlocksSkipped)
	}
}

// -----------------------------------------------------------
// Scenario (iter-2 fix #2): scan flags an already-in-flight
// replacement -> skipped without entering the per-target tx.
// -----------------------------------------------------------

func TestSnapshot_scanFlagsInFlight_countsAsSkipped(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-inflight", "")
	defer cleanup()

	expectAssertRepoExists(mock, testRepoID, true)
	priorDetails := `{"content":"body","signature_only":false,"embedding_model_version":"v0"}`
	// InFlight=true → enqueue loop short-circuits to skip;
	// NO BEGIN/lock/dedupe/insert expectations queued.
	expectNodeScan(mock, testRepoID,
		nodeTargetRow(testNodeID, "method", "pkg::busy()", testPriorPublishID, priorDetails, true))
	expectConceptScan(mock, testRepoID, emptyConceptRows())

	res, err := svc.Snapshot(context.Background(), testRepoID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if res.MethodsEnqueued != 0 {
		t.Errorf("methods_enqueued = %d, want 0", res.MethodsEnqueued)
	}
	if res.MethodBlocksSkipped != 1 {
		t.Errorf("method_blocks_skipped = %d, want 1 (in-flight scan flag)", res.MethodBlocksSkipped)
	}
	if svc.Metrics().PendingTotal() != 0 {
		t.Errorf("pending counter = %d, want 0", svc.Metrics().PendingTotal())
	}
}

// -----------------------------------------------------------
// Scenario: prior publish already has a non-terminal snapshot
// replacement that the SCAN missed (i.e. it appeared in the
// race window between scan and tx) -> dedupe gate inside the
// tx catches it.
// -----------------------------------------------------------

func TestSnapshot_dedupeGateInsideTx_skipped(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-dup", "")
	defer cleanup()

	expectAssertRepoExists(mock, testRepoID, true)
	priorDetails := `{"content":"body","signature_only":false,"embedding_model_version":"v0"}`
	expectNodeScan(mock, testRepoID,
		nodeTargetRow(testNodeID, "block", "pkg::block()", testPriorPublishID, priorDetails, false))
	mock.ExpectBegin()
	expectAdvisoryLock(mock, testPriorPublishID)
	expectDedupeFoundDup(mock, testNodeID, testPriorPublishID)
	mock.ExpectRollback()
	expectConceptScan(mock, testRepoID, emptyConceptRows())

	res, err := svc.Snapshot(context.Background(), testRepoID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if res.BlocksEnqueued != 0 {
		t.Errorf("blocks_enqueued = %d, want 0", res.BlocksEnqueued)
	}
	if res.MethodBlocksSkipped != 1 {
		t.Errorf("method_blocks_skipped = %d, want 1", res.MethodBlocksSkipped)
	}
	if svc.Metrics().PendingTotal() != 0 {
		t.Errorf("pending counter = %d, want 0 (dedupe means no enqueue)", svc.Metrics().PendingTotal())
	}
}

// -----------------------------------------------------------
// Scenario: one concept target -> concept-side INSERT path,
// asserts JSONB body shape (iter-2 fix #5 + iter-3 fix #1).
// -----------------------------------------------------------

func TestSnapshot_oneConceptTarget_writesPublishAndQueued(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-cv", "")
	defer cleanup()

	// Iter-3 fix #1+#2: use the REAL promoter-emitted
	// queued JSONB shape (concept_id / concept_version_id /
	// name / description_md / fingerprint), NOT the
	// impossible {"content":"..."} shape the iter-2 test
	// used. The previous shape masked the schema-mismatch
	// bug where every real promoted-Concept prior was
	// silently skipped.
	priorDetails := `{"concept_id":"77777777-7777-7777-7777-777777777777",` +
		`"concept_version_id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1",` +
		`"name":"Concept-Name",` +
		`"description_md":"A short markdown description.",` +
		`"fingerprint":"deadbeefcafebabe",` +
		`"embedding_model_version":"v0"}`
	expectAssertRepoExists(mock, testRepoID, true)
	expectNodeScan(mock, testRepoID, emptyNodeRows())
	expectConceptScan(mock, testRepoID,
		conceptTargetRow(testConceptVID, "Concept-Name", testPriorPublishID, priorDetails, false))
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock\(hashtextextended\('snapshot:supersede:' \|\| \$1::text, 0\)\)`).
		WithArgs(testPriorPublishID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM embedding_publish ep\s+JOIN LATERAL[\s\S]+supersedes_publish_id`).
		WithArgs(testConceptVID, testPriorPublishID).
		WillReturnError(sql.ErrNoRows)
	expectConceptInsertWithConceptBody(mock, testConceptVID, testNewPublishID, conceptQueuedDetailsMatcher{
		WantConceptID:           "77777777-7777-7777-7777-777777777777",
		WantConceptVersionID:    "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1",
		WantName:                "Concept-Name",
		WantDescriptionMD:       "A short markdown description.",
		WantFingerprint:         "deadbeefcafebabe",
		WantModelVersion:        testModelVersion,
		WantSnapshotID:          "snap-cv",
		WantSupersedesPublishID: testPriorPublishID,
		AssertNoContentKey:      true,
	})
	mock.ExpectCommit()

	res, err := svc.Snapshot(context.Background(), testRepoID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if res.ConceptsEnqueued != 1 {
		t.Errorf("concepts_enqueued = %d, want 1", res.ConceptsEnqueued)
	}
}

// -----------------------------------------------------------
// Regression: real concept queued-event shape (no `content`
// key) MUST be enqueued, not skipped. Iter-3 fix #1: the
// iter-2 code required `priorDetails.Content != ""` for
// concepts, which dropped every real promoted-Concept prior
// because the promoter never writes a `content` key. This
// test pins the corrected gate (name OR description_md
// non-empty) and proves the existing concept publishes
// flow through.
// -----------------------------------------------------------

func TestSnapshot_realConceptShape_isNotSkipped_writesConceptQueuedBody(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-cv-real", "")
	defer cleanup()

	// Real promoter shape — NO `content` key, but with
	// non-empty name + description_md so the new gate
	// admits the row.
	priorDetails := `{"concept_id":"88888888-8888-8888-8888-888888888888",` +
		`"concept_version_id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1",` +
		`"name":"PromotedConcept",` +
		`"description_md":"Body.",` +
		`"fingerprint":"abcdef0123456789",` +
		`"embedding_model_version":"prev@v1"}`

	expectAssertRepoExists(mock, testRepoID, true)
	expectNodeScan(mock, testRepoID, emptyNodeRows())
	expectConceptScan(mock, testRepoID,
		conceptTargetRow(testConceptVID, "PromotedConcept", testPriorPublishID, priorDetails, false))
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock\(hashtextextended\('snapshot:supersede:' \|\| \$1::text, 0\)\)`).
		WithArgs(testPriorPublishID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM embedding_publish ep\s+JOIN LATERAL[\s\S]+supersedes_publish_id`).
		WithArgs(testConceptVID, testPriorPublishID).
		WillReturnError(sql.ErrNoRows)
	expectConceptInsertWithConceptBody(mock, testConceptVID, testNewPublishID, conceptQueuedDetailsMatcher{
		WantConceptID:           "88888888-8888-8888-8888-888888888888",
		WantConceptVersionID:    "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1",
		WantName:                "PromotedConcept",
		WantDescriptionMD:       "Body.",
		WantFingerprint:         "abcdef0123456789",
		// Forced to the snapshot service's modelVersion,
		// NOT the prior's "prev@v1".
		WantModelVersion:        testModelVersion,
		WantSnapshotID:          "snap-cv-real",
		WantSupersedesPublishID: testPriorPublishID,
		AssertNoContentKey:      true,
	})
	mock.ExpectCommit()

	res, err := svc.Snapshot(context.Background(), testRepoID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if res.ConceptsEnqueued != 1 {
		t.Errorf("concepts_enqueued = %d, want 1 (real promoter shape MUST NOT be skipped)", res.ConceptsEnqueued)
	}
	if res.ConceptsSkipped != 0 {
		t.Errorf("concepts_skipped = %d, want 0 (real promoter shape MUST NOT be skipped)", res.ConceptsSkipped)
	}
	if svc.Metrics().PendingTotal() != 1 {
		t.Errorf("pending counter = %d, want 1", svc.Metrics().PendingTotal())
	}
}

// -----------------------------------------------------------
// Regression: concept prior with neither `name` nor
// `description_md` (degenerate row that would embed as
// "(empty concept)") is treated as a no-prior-content skip,
// not enqueued. Pins the stricter iter-3 gate so a fingerprint-
// only row does NOT accidentally qualify.
// -----------------------------------------------------------

func TestSnapshot_conceptPriorWithoutNameOrDescription_skipped(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-cv-empty-text", "")
	defer cleanup()

	// Fingerprint-only prior — name + description_md are
	// both empty. New gate must reject this.
	priorDetails := `{"concept_id":"99999999-9999-9999-9999-999999999999",` +
		`"concept_version_id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1",` +
		`"name":"",` +
		`"description_md":"",` +
		`"fingerprint":"f0f0f0f0f0f0f0f0",` +
		`"embedding_model_version":"v0"}`

	expectAssertRepoExists(mock, testRepoID, true)
	expectNodeScan(mock, testRepoID, emptyNodeRows())
	expectConceptScan(mock, testRepoID,
		conceptTargetRow(testConceptVID, "(empty)", testPriorPublishID, priorDetails, false))
	// NO BEGIN / lock / INSERT — the skip short-circuits
	// before any concept-tx is opened.

	res, err := svc.Snapshot(context.Background(), testRepoID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if res.ConceptsEnqueued != 0 {
		t.Errorf("concepts_enqueued = %d, want 0", res.ConceptsEnqueued)
	}
	if res.ConceptsSkipped != 1 {
		t.Errorf("concepts_skipped = %d, want 1 (fingerprint-only prior MUST be skipped)", res.ConceptsSkipped)
	}
}

// -----------------------------------------------------------
// Scenario (iter-2 fix #2): concept scan flags an in-flight
// concept supersede -> counted as skipped.
// -----------------------------------------------------------

func TestSnapshot_conceptScanFlagsInFlight_countsAsSkipped(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-cv-inflight", "")
	defer cleanup()

	expectAssertRepoExists(mock, testRepoID, true)
	expectNodeScan(mock, testRepoID, emptyNodeRows())
	// Iter-3 fix #2: use real promoter shape here too —
	// the in-flight gate fires BEFORE the
	// hasConceptContent gate (in_flight check happens on
	// the scan-side, in enqueueConceptTargets dispatch),
	// but seeding the wrong shape would still pass-test
	// for the wrong reason. Pin the production shape.
	priorDetails := `{"concept_id":"77777777-7777-7777-7777-777777777777",` +
		`"concept_version_id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1",` +
		`"name":"cv-name","description_md":"body",` +
		`"fingerprint":"f00d","embedding_model_version":"v0"}`
	expectConceptScan(mock, testRepoID,
		conceptTargetRow(testConceptVID, "cv-name", testPriorPublishID, priorDetails, true))

	res, err := svc.Snapshot(context.Background(), testRepoID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if res.ConceptsEnqueued != 0 {
		t.Errorf("concepts_enqueued = %d, want 0", res.ConceptsEnqueued)
	}
	if res.ConceptsSkipped != 1 {
		t.Errorf("concepts_skipped = %d, want 1 (in-flight scan flag)", res.ConceptsSkipped)
	}
}

// -----------------------------------------------------------
// Scenario (iter-2 fix #1): advisory lock acquisition failure
// surfaces as a snapshot error (proves the lock is actually
// issued before the dedupe probe).
// -----------------------------------------------------------

func TestSnapshot_advisoryLockFailure_surfacesError(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-lockfail", "")
	defer cleanup()

	expectAssertRepoExists(mock, testRepoID, true)
	priorDetails := `{"content":"body","signature_only":false,"embedding_model_version":"v0"}`
	expectNodeScan(mock, testRepoID,
		nodeTargetRow(testNodeID, "method", "pkg::m()", testPriorPublishID, priorDetails, false))
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock\(hashtextextended\('snapshot:supersede:' \|\| \$1::text, 0\)\)`).
		WithArgs(testPriorPublishID).
		WillReturnError(fmt.Errorf("simulated PG outage during advisory lock"))
	mock.ExpectRollback()
	// concept scan is NOT queued — the error short-circuits before reaching it.

	_, err := svc.Snapshot(context.Background(), testRepoID)
	if err == nil {
		t.Fatalf("expected error from advisory lock failure, got nil")
	}
	if !strings.Contains(err.Error(), "acquire supersede lock") {
		t.Errorf("err = %v, want 'acquire supersede lock' substring", err)
	}
}

// -----------------------------------------------------------
// Scenario: invalid UUID rejected via PG cast error -> mapped
// to ErrRepoNotFound (defence-in-depth: the handler regex
// pre-check already filters bad UUIDs, but the service does
// not trust the caller).
// -----------------------------------------------------------

func TestSnapshot_invalidUUIDFromPG_returnsRepoNotFound(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newTestService(t, "snap-bad", "")
	defer cleanup()

	mock.ExpectQuery(`SELECT 1 FROM repo WHERE repo_id = \$1::uuid`).
		WithArgs("garbage-not-a-uuid").
		WillReturnError(errors.New(`pq: invalid input syntax for type uuid: "garbage-not-a-uuid"`))

	_, err := svc.Snapshot(context.Background(), "garbage-not-a-uuid")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

// -----------------------------------------------------------
// Scenario: blank repo_id rejected up-front (no DB call).
// -----------------------------------------------------------

func TestSnapshot_blankRepoID_returnsError(t *testing.T) {
	t.Parallel()
	svc, _, cleanup := newTestService(t, "snap-blank", "")
	defer cleanup()

	_, err := svc.Snapshot(context.Background(), "  \t ")
	if err == nil {
		t.Fatalf("expected error on blank repo_id; got nil")
	}
	if !strings.Contains(err.Error(), "repoID is required") {
		t.Errorf("err = %v, want substring 'repoID is required'", err)
	}
}

// -----------------------------------------------------------
// Scenario: New panics on missing model version (boot-time
// configuration guard; risk §9.6 forbids unversioned
// snapshots).
// -----------------------------------------------------------

func TestNew_emptyModelVersion_panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("New(empty model version) did not panic")
		}
	}()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	_ = New(db, "   ")
}

// -----------------------------------------------------------
// Scenario: New panics on nil *sql.DB.
// -----------------------------------------------------------

func TestNew_nilDB_panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("New(nil db) did not panic")
		}
	}()
	_ = New(nil, "model@v1")
}

// -----------------------------------------------------------
// Test: ModelVersion / Metrics getters return wired values.
// -----------------------------------------------------------

func TestService_ModelVersionAndMetricsGetters(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	custom := NewMetrics()
	svc := New(db, " custom-model@v9 ", WithMetrics(custom))
	if got := svc.ModelVersion(); got != "custom-model@v9" {
		t.Errorf("ModelVersion = %q, want trimmed 'custom-model@v9'", got)
	}
	if svc.Metrics() != custom {
		t.Errorf("Metrics() did not return the wired Metrics instance")
	}
}
