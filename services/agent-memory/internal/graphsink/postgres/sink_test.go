package postgres_test

// sqlmock-driven forwarding tests for the Postgres sink
// adapter. Three focal points:
//
//  1. EnsureRepo routes to EnsureRepoWithID when
//     `RepoInput.RepoID` is non-zero, and to EnsureRepo
//     otherwise (impl-plan Stage 3.3 scenario
//     `postgres-forwarding`). Verified by asserting the SQL
//     statement issued by the wrapped writer -- the
//     `EnsureRepoWithID` path explicitly inserts the
//     `repo_id` column ("INSERT INTO repo (repo_id, url, ...")
//     while `EnsureRepo` omits it ("INSERT INTO repo (url, ...").
//
//  2. Write failures propagate verbatim. A SQLSTATE 42501
//     (insufficient privilege) error from the mocked driver
//     surfaces as a typed `*graphwriter.WriteContractViolation`,
//     never unwrapped or shadowed by the adapter layer
//     (impl-plan Stage 3.3 scenario
//     `write-contract-violation-propagates-verbatim`).
//
//  3. The `graphsink.Sink` lifecycle contract holds: after
//     `Close` returns, EVERY other method on the Sink yields
//     `postgresadapter.ErrSinkClosed`
//     (`internal/graphsink/sink.go:121` -- "calls to any other
//     method on the Sink yield an error").

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	postgresadapter "github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/postgres"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// newMockSinkFixture spins up a sqlmock-backed *graphwriter.Writer
// wrapped in a Postgres adapter. The QueryMatcherRegexp option
// lets tests pin the load-bearing SQL fragments (e.g. the
// presence / absence of `repo_id` in `INSERT INTO repo (...)`)
// without re-asserting whitespace.
func newMockSinkFixture(t *testing.T) (*postgresadapter.Sink, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	w := graphwriter.New(db, nil)
	sink := postgresadapter.NewSink(w)
	return sink, mock, db
}

// --------------------------------------------------------------
// EnsureRepo routing -- the workstream's distinctive requirement
// --------------------------------------------------------------

func TestSink_EnsureRepo_zeroRepoID_routesToEnsureRepo(t *testing.T) {
	t.Parallel()
	sink, mock, db := newMockSinkFixture(t)
	defer db.Close()

	// EnsureRepo path issues `INSERT INTO repo (url, ...)` --
	// the projection list begins with `url`, NOT `repo_id`.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO repo \(url, default_branch`).
		WithArgs("https://example.test/r", "main", "abc", pq.Array([]string{})).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow("11111111-1111-1111-1111-111111111111", true))
	mock.ExpectCommit()

	rec, err := sink.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL:            "https://example.test/r",
		DefaultBranch:  "main",
		CurrentHeadSHA: "abc",
		// RepoID intentionally left zero.
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if rec.RepoID != "11111111-1111-1111-1111-111111111111" || !rec.Inserted {
		t.Errorf("unexpected record: %+v", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestSink_EnsureRepo_nonZeroRepoID_routesToEnsureRepoWithID(t *testing.T) {
	t.Parallel()
	sink, mock, db := newMockSinkFixture(t)
	defer db.Close()

	repoID := fingerprint.MustParseRepoID("22222222-2222-2222-2222-222222222222")

	// EnsureRepoWithID path issues `INSERT INTO repo (repo_id, url, ...)` --
	// the projection list begins with `repo_id`. The
	// distinctive fragment proves the adapter took the
	// precomputed-PK branch.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO repo \(repo_id, url, default_branch`).
		WithArgs(repoID.String(), "https://example.test/r", "main", "abc", pq.Array([]string{})).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow(repoID.String(), true))
	mock.ExpectCommit()

	rec, err := sink.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL:            "https://example.test/r",
		DefaultBranch:  "main",
		CurrentHeadSHA: "abc",
		RepoID:         repoID,
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if rec.RepoID != repoID.String() || !rec.Inserted {
		t.Errorf("unexpected record: %+v", rec)
	}
	if rec.ID != repoID {
		t.Errorf("rec.ID = %v, want %v (backend-parity ID lost across forwarding)", rec.ID, repoID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --------------------------------------------------------------
// WriteContractViolation propagation -- typed error survives the adapter
// --------------------------------------------------------------

func TestSink_InsertNode_propagatesWriteContractViolationVerbatim(t *testing.T) {
	t.Parallel()
	sink, mock, db := newMockSinkFixture(t)
	defer db.Close()

	repoID := fingerprint.MustParseRepoID("33333333-3333-3333-3333-333333333333")

	mock.ExpectBegin()
	pqErr := &pq.Error{
		Code:    pq.ErrorCode("42501"),
		Message: "permission denied for table node",
	}
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO node`)).
		WillReturnError(pqErr)
	mock.ExpectRollback()

	_, err := sink.InsertNode(context.Background(), graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "method",
		CanonicalSignature: "sig://example",
		FromSHA:            "abc",
	})
	if err == nil {
		t.Fatal("InsertNode: expected error, got nil")
	}
	var wcv *graphwriter.WriteContractViolation
	if !errors.As(err, &wcv) {
		t.Fatalf("err = %T (%v); want *graphwriter.WriteContractViolation", err, err)
	}
	if wcv.SQLState != "42501" {
		t.Errorf("WriteContractViolation.SQLState = %q, want %q", wcv.SQLState, "42501")
	}
	if wcv.Op != "InsertNode" {
		t.Errorf("WriteContractViolation.Op = %q, want %q", wcv.Op, "InsertNode")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestSink_InsertEdge_propagatesWriteContractViolationVerbatim(t *testing.T) {
	t.Parallel()
	sink, mock, db := newMockSinkFixture(t)
	defer db.Close()

	repoID := fingerprint.MustParseRepoID("44444444-4444-4444-4444-444444444444")

	mock.ExpectBegin()
	pqErr := &pq.Error{
		Code:    pq.ErrorCode("42501"),
		Message: "permission denied for table edge",
	}
	// InsertEdge first SELECTs the src/dst node fingerprints
	// before issuing the INSERT; rather than mocking that
	// pre-flight (which differs across graphwriter
	// implementation revisions), we instead return the
	// permission error on the FIRST query of the transaction.
	// `^` anchors the regex so this matches whatever the first
	// statement is.
	mock.ExpectQuery(`.+`).WillReturnError(pqErr)
	mock.ExpectRollback()

	_, err := sink.InsertEdge(context.Background(), graphwriter.EdgeInput{
		RepoID:    repoID,
		Kind:      "static_calls",
		SrcNodeID: "src-node",
		DstNodeID: "dst-node",
		FromSHA:   "abc",
	})
	if err == nil {
		t.Fatal("InsertEdge: expected error, got nil")
	}
	var wcv *graphwriter.WriteContractViolation
	if !errors.As(err, &wcv) {
		t.Fatalf("err = %T (%v); want *graphwriter.WriteContractViolation", err, err)
	}
	if wcv.SQLState != "42501" {
		t.Errorf("WriteContractViolation.SQLState = %q, want %q", wcv.SQLState, "42501")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --------------------------------------------------------------
// EnsureCommit forwarding (the remaining write method)
// --------------------------------------------------------------

func TestSink_EnsureCommit_forwardsToWriter(t *testing.T) {
	t.Parallel()
	sink, mock, db := newMockSinkFixture(t)
	defer db.Close()

	repoID := fingerprint.MustParseRepoID("55555555-5555-5555-5555-555555555555")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO repo_commit`)).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha"}).
			AddRow(repoID.String(), "abc"))
	mock.ExpectCommit()

	rec, err := sink.EnsureCommit(context.Background(), graphwriter.CommitInput{
		RepoID:      repoID,
		SHA:         "abc",
		CommittedAt: time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("EnsureCommit: %v", err)
	}
	if rec.SHA != "abc" || !rec.Inserted {
		t.Errorf("unexpected record: %+v", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// --------------------------------------------------------------
// Lifecycle contract: post-Close calls return ErrSinkClosed
// --------------------------------------------------------------

func TestSink_Close_isIdempotentAndReturnsNil(t *testing.T) {
	t.Parallel()
	sink, _, db := newMockSinkFixture(t)
	defer db.Close()

	if err := sink.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("second Close: %v (must be idempotent and return nil)", err)
	}
}

func TestSink_postClose_allMethodsReturnErrSinkClosed(t *testing.T) {
	t.Parallel()
	sink, _, db := newMockSinkFixture(t)
	defer db.Close()

	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx := context.Background()
	repoID := fingerprint.MustParseRepoID("66666666-6666-6666-6666-666666666666")

	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, postgresadapter.ErrSinkClosed) {
			t.Errorf("%s after Close: err = %v; want errors.Is ErrSinkClosed", name, err)
		}
	}

	_, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{URL: "x"})
	check("EnsureRepo(zero RepoID)", err)

	_, err = sink.EnsureRepo(ctx, graphwriter.RepoInput{URL: "x", RepoID: repoID})
	check("EnsureRepo(non-zero RepoID)", err)

	_, err = sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID: repoID, SHA: "abc",
		CommittedAt: time.Now(),
	})
	check("EnsureCommit", err)

	_, err = sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "method", CanonicalSignature: "s", FromSHA: "abc",
	})
	check("InsertNode", err)

	_, err = sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID: repoID, Kind: "static_calls",
		SrcNodeID: "s", DstNodeID: "d", FromSHA: "abc",
	})
	check("InsertEdge", err)

	check("Flush", sink.Flush(ctx))
}

func TestSink_NewSink_panicsOnNilWriter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewSink(nil) did not panic")
		}
	}()
	_ = postgresadapter.NewSink(nil)
}
