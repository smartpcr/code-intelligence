package embedding

// Unit-test coverage for the §7.4 snapshot fallback path in
// `PublishEventContentResolver.Resolve`.  Driven by go-sqlmock
// so the tests run without a Postgres fixture (the existing
// `publish_event_resolver_integration_test.go` covers the
// round-trip against a real DB; these tests pin the
// fallback-decision logic precisely).
//
// The resolver's job for snapshot-driven publishes is: when
// the queued event's `details_json` carries
// `source = "mgmt.snapshot"` AND the `content` field is
// empty (the snapshot enqueuer does not have source bytes
// in-process), the resolver MUST look up the most recent
// queued event with non-empty content from a prior publish
// for the same `node_id` whose latest event is `published`,
// and substitute its content into the returned
// `PublishRequest`.  The publisher's `Retry` then re-embeds
// the snapshot publish under the current model.

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestResolver_snapshotSource_fallbackHappyPath proves the
// resolver fallback substitutes prior-publish content into
// the snapshot publish's PublishRequest when the latest
// queued event for the snapshot publish has empty content
// and `source = mgmt.snapshot`.  Without this fix, evaluator
// finding #1 stands: the flusher's resolver rejects the
// snapshot-enqueued queued event and the publish stalls
// forever in `queued` state.
func TestResolver_snapshotSource_fallbackHappyPath(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const publishID = "11111111-1111-1111-1111-111111111111"
	const nodeID = "22222222-2222-2222-2222-222222222222"

	// 1) Primary lookup — the latest queued event for the
	//    snapshot publish carries the mgmt.snapshot marker
	//    and an empty `content` field.
	mock.ExpectQuery(`SELECT details_json::text\s+FROM embedding_publish_event\s+WHERE publish_id`).
		WithArgs(publishID).
		WillReturnRows(sqlmock.NewRows([]string{"details_json"}).
			AddRow(`{"snapshot_id":"abc","source":"mgmt.snapshot","embedding_model_version":"model-v1"}`))

	// 2) Fallback lookup — joins via node_id to find the
	//    most recent queued event with non-empty content
	//    whose owning publish reached `published`.  The
	//    SQL MUST carry `latest.event_kind = 'published'`
	//    so a snapshot can only inherit content from a row
	//    the recall path was actually serving.
	mock.ExpectQuery(`FROM embedding_publish_event e\s+JOIN embedding_publish\s+p[\s\S]*latest\.event_kind\s*=\s*'published'`).
		WithArgs(nodeID, publishID).
		WillReturnRows(sqlmock.NewRows([]string{"details_json"}).
			AddRow(`{"content":"func RealCode(){}","signature_only":false,"embedding_model_version":"model-v0"}`))

	r := NewPublishEventContentResolver(db)
	req, err := r.Resolve(context.Background(), ContentLookup{
		PublishID:           publishID,
		NodeID:              nodeID,
		RepoID:              "33333333-3333-3333-3333-333333333333",
		Kind:                NodeKindMethod,
		CanonicalSignature:  "pkg/path::Func",
		ModelVersion:        "model-v1",
		CurrentModelVersion: "model-v1",
	})
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if req.Content != "func RealCode(){}" {
		t.Fatalf("Content = %q; want fallback content %q",
			req.Content, "func RealCode(){}")
	}
	if req.SignatureOnly {
		t.Fatalf("SignatureOnly = true; fallback row reported false")
	}
	if req.NodeID != nodeID || req.Kind != NodeKindMethod {
		t.Fatalf("lookup metadata not preserved: %+v", req)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestResolver_snapshotSource_noFallback_returnsExplanatoryError
// proves the resolver surfaces a CLEAR error (mentioning
// "fallback") when the snapshot row's content is empty AND
// no prior published row carries content the snapshot can
// re-embed.  This is the "snapshot called on a never-
// published node" edge case — the publish stays queued and
// operator triage can identify it from the error message
// instead of the generic "empty content" message.
func TestResolver_snapshotSource_noFallback_returnsExplanatoryError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const publishID = "11111111-1111-1111-1111-111111111111"
	const nodeID = "22222222-2222-2222-2222-222222222222"

	mock.ExpectQuery(`SELECT details_json::text\s+FROM embedding_publish_event\s+WHERE publish_id`).
		WithArgs(publishID).
		WillReturnRows(sqlmock.NewRows([]string{"details_json"}).
			AddRow(`{"snapshot_id":"abc","source":"mgmt.snapshot","embedding_model_version":"model-v1"}`))

	// Fallback lookup returns no rows.
	mock.ExpectQuery(`FROM embedding_publish_event e\s+JOIN embedding_publish\s+p`).
		WithArgs(nodeID, publishID).
		WillReturnRows(sqlmock.NewRows([]string{"details_json"}))

	r := NewPublishEventContentResolver(db)
	_, err = r.Resolve(context.Background(), ContentLookup{
		PublishID: publishID,
		NodeID:    nodeID,
		RepoID:    "33333333-3333-3333-3333-333333333333",
		Kind:      NodeKindMethod,
	})
	if err == nil {
		t.Fatalf("Resolve: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("Resolve error must mention 'fallback' for operator triage; got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestResolver_nonSnapshotEmptyContent_skipsFallback proves
// that a queued event with empty `content` AND no
// `source` marker (e.g. a legacy hand-inserted row) does
// NOT trigger the fallback query — the resolver still
// returns the original empty-content error, preserving the
// pre-snapshot diagnostic shape for non-snapshot rows.  If
// the resolver accidentally fired the fallback query for
// every empty-content row, the sqlmock would surface the
// unexpected query as a failed expectation.
func TestResolver_nonSnapshotEmptyContent_skipsFallback(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const publishID = "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`SELECT details_json::text\s+FROM embedding_publish_event\s+WHERE publish_id`).
		WithArgs(publishID).
		WillReturnRows(sqlmock.NewRows([]string{"details_json"}).
			AddRow(`{"content":"","signature_only":false,"embedding_model_version":"model-v1"}`))

	// No fallback query is expected.  sqlmock fails closed:
	// if Resolve fires the fallback the test reports an
	// unmatched-expectation error.

	r := NewPublishEventContentResolver(db)
	_, err = r.Resolve(context.Background(), ContentLookup{
		PublishID: publishID,
		NodeID:    "22222222-2222-2222-2222-222222222222",
		RepoID:    "33333333-3333-3333-3333-333333333333",
		Kind:      NodeKindMethod,
	})
	if err == nil {
		t.Fatalf("Resolve: expected empty-content error")
	}
	if strings.Contains(err.Error(), "fallback") {
		t.Fatalf("Resolve error must NOT mention fallback for non-snapshot rows; got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}
