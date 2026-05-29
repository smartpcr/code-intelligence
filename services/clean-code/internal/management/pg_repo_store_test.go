package management_test

// Stage 6.2 -- sqlmock tests for [management.PGRepoStore].
//
// Pins the canonical transactional shape of the two write
// verbs against the schema in `migrations/0001` + `0006`:
//
//   * `RegisterRepo` runs BEGIN -> advisory_xact_lock ->
//     SELECT-lookup -> (INSERT repo + INSERT repo_event)
//     -> COMMIT.
//   * `SetRepoMode` runs BEGIN -> SELECT-FOR-UPDATE ->
//     (UPDATE + INSERT repo_event) -> COMMIT.
//
// Mock-mode tests verify the SQL shape (column lists,
// transactional boundaries, payload contents); E2E against
// a real Postgres is covered by the live-DB integration
// harness when CLEAN_CODE_TEST_PG_URL is set.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	management "github.com/smartpcr/code-intelligence/services/clean-code/internal/management"
)

const pgRepoStoreTestSchema = "clean_code_mgmt_test"

func TestPGRepoStore_NewRejectsNilDB(t *testing.T) {
	t.Parallel()
	_, err := management.NewPGRepoStore(nil)
	if !errors.Is(err, management.ErrPGRepoStoreNilDB) {
		t.Fatalf("err: got %v; want ErrPGRepoStoreNilDB", err)
	}
}

func TestPGRepoStore_NewRejectsEmptySchema(t *testing.T) {
	t.Parallel()
	db, _, _ := sqlmock.New()
	defer db.Close()
	_, err := management.NewPGRepoStoreWithSchema(db, "  ")
	if !errors.Is(err, management.ErrPGRepoStoreEmptySchema) {
		t.Fatalf("err: got %v; want ErrPGRepoStoreEmptySchema", err)
	}
}

func newRegexMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// TestPGRepoStore_RegisterRepo_FreshTransaction pins the
// happy-path SQL shape: BEGIN, advisory lock, SELECT (no
// rows), INSERT repo RETURNING, INSERT repo_event, COMMIT.
// All five operations are inside a single transaction.
func TestPGRepoStore_RegisterRepo_FreshTransaction(t *testing.T) {
	t.Parallel()
	db, mock := newRegexMock(t)
	store, err := management.NewPGRepoStoreWithSchema(db, pgRepoStoreTestSchema)
	if err != nil {
		t.Fatalf("NewPGRepoStoreWithSchema: %v", err)
	}

	freshRepoID := uuid.Must(uuid.NewV4())
	expectedURL := "https://example.com/org/repo.git"

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtext\(\$1::text\)\)`).
		WithArgs(expectedURL).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT repo_id, mode FROM "clean_code_mgmt_test"\."repo" WHERE repo_url = \$1`).
		WithArgs(expectedURL).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO "clean_code_mgmt_test"\."repo"\s+\(display_name, mode, default_branch, repo_url\)\s+VALUES \(\$1, \$2, \$3, \$4\)\s+RETURNING repo_id`).
		WithArgs("repo", management.RepoModeEmbedded, "main", expectedURL).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id"}).AddRow(freshRepoID))
	mock.ExpectExec(`INSERT INTO "clean_code_mgmt_test"\."repo_event"\s+\(repo_id, kind, payload_json\)\s+VALUES \(\$1, \$2::"clean_code_mgmt_test"\."repo_event_kind", \$3::jsonb\)`).
		WithArgs(freshRepoID, management.RepoEventKindRegistered, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	res, err := store.RegisterRepo(context.Background(), management.RegisterRepoRowRequest{
		RepoURL:       expectedURL,
		DefaultBranch: "main",
		Actor:         "alice@example.test",
	})
	if err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	if !res.Created {
		t.Errorf("Created=%v, want true", res.Created)
	}
	if res.RepoID != freshRepoID {
		t.Errorf("RepoID=%s, want %s", res.RepoID, freshRepoID)
	}
	if res.Mode != management.RepoModeEmbedded {
		t.Errorf("Mode=%q, want %q", res.Mode, management.RepoModeEmbedded)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

// TestPGRepoStore_RegisterRepo_IdempotentTransaction pins
// the idempotent path: a SELECT that returns the existing
// row short-circuits to COMMIT (releasing the advisory
// lock) without issuing either INSERT.
func TestPGRepoStore_RegisterRepo_IdempotentTransaction(t *testing.T) {
	t.Parallel()
	db, mock := newRegexMock(t)
	store, _ := management.NewPGRepoStoreWithSchema(db, pgRepoStoreTestSchema)

	existingID := uuid.Must(uuid.NewV4())
	url := "https://example.com/org/repo"

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs(url).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT repo_id, mode FROM "clean_code_mgmt_test"\."repo"`).
		WithArgs(url).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "mode"}).AddRow(existingID, management.RepoModeLinked))
	// NO INSERT expected.
	mock.ExpectCommit()

	res, err := store.RegisterRepo(context.Background(), management.RegisterRepoRowRequest{
		RepoURL:       url,
		DefaultBranch: "main",
		Mode:          management.RepoModeEmbedded, // requested != existing
		Actor:         "alice",
	})
	if err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	if res.Created {
		t.Errorf("Created=%v, want false (idempotent path)", res.Created)
	}
	if res.RepoID != existingID {
		t.Errorf("RepoID=%s, want %s", res.RepoID, existingID)
	}
	// Existing row's mode wins over the request -- callers
	// MUST use set_mode to change.
	if res.Mode != management.RepoModeLinked {
		t.Errorf("Mode=%q, want %q (existing row's mode)", res.Mode, management.RepoModeLinked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

// TestPGRepoStore_RegisterRepo_RollsBackOnEventInsertFailure
// pins the atomicity invariant: if the `repo_event` INSERT
// fails, the transaction is rolled back so the `repo` row
// is NOT committed. Sqlmock's `ExpectRollback` is the only
// way to verify the deferred Rollback ran.
func TestPGRepoStore_RegisterRepo_RollsBackOnEventInsertFailure(t *testing.T) {
	t.Parallel()
	db, mock := newRegexMock(t)
	store, _ := management.NewPGRepoStoreWithSchema(db, pgRepoStoreTestSchema)

	freshRepoID := uuid.Must(uuid.NewV4())
	url := "https://example.com/atomic"
	injectedErr := errors.New("payload too large")

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs(url).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT repo_id, mode FROM "clean_code_mgmt_test"\."repo"`).
		WithArgs(url).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO "clean_code_mgmt_test"\."repo"`).
		WithArgs("atomic", management.RepoModeEmbedded, "main", url).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id"}).AddRow(freshRepoID))
	mock.ExpectExec(`INSERT INTO "clean_code_mgmt_test"\."repo_event"`).
		WillReturnError(injectedErr)
	mock.ExpectRollback()

	_, err := store.RegisterRepo(context.Background(), management.RegisterRepoRowRequest{
		RepoURL:       url,
		DefaultBranch: "main",
		Actor:         "alice",
	})
	if err == nil {
		t.Fatal("RegisterRepo: want error, got nil")
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("err=%v, want chain containing %v", err, injectedErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

// TestPGRepoStore_RegisterRepo_PayloadShape verifies the
// canonical `repo_event(kind='registered')` payload
// contains the documented keys: repo_url, default_branch,
// mode, display_name, actor.
func TestPGRepoStore_RegisterRepo_PayloadShape(t *testing.T) {
	t.Parallel()
	db, mock := newRegexMock(t)
	store, _ := management.NewPGRepoStoreWithSchema(db, pgRepoStoreTestSchema)

	freshRepoID := uuid.Must(uuid.NewV4())
	url := "https://example.com/payload"

	var capturedPayload string
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT repo_id, mode FROM`).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO "clean_code_mgmt_test"\."repo"`).WillReturnRows(sqlmock.NewRows([]string{"repo_id"}).AddRow(freshRepoID))
	mock.ExpectExec(`INSERT INTO "clean_code_mgmt_test"\."repo_event"`).
		WithArgs(freshRepoID, management.RepoEventKindRegistered, capturedPayloadMatcher{captured: &capturedPayload}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if _, err := store.RegisterRepo(context.Background(), management.RegisterRepoRowRequest{
		RepoURL:       url,
		DefaultBranch: "main",
		Mode:          management.RepoModeLinked,
		DisplayName:   "Friendly Name",
		Actor:         "alice@example.test",
	}); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(capturedPayload), &parsed); err != nil {
		t.Fatalf("payload not JSON: %v; raw=%q", err, capturedPayload)
	}
	want := map[string]string{
		"repo_url":       url,
		"default_branch": "main",
		"mode":           management.RepoModeLinked,
		"display_name":   "Friendly Name",
		"actor":          "operator:alice@example.test",
	}
	for k, v := range want {
		got, _ := parsed[k].(string)
		if got != v {
			t.Errorf("payload[%q]=%q, want %q", k, got, v)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

// TestPGRepoStore_SetRepoMode_TransitionTransaction pins
// the happy-path transition: BEGIN, SELECT-FOR-UPDATE
// returning the previous mode, UPDATE, INSERT repo_event,
// COMMIT.
func TestPGRepoStore_SetRepoMode_TransitionTransaction(t *testing.T) {
	t.Parallel()
	db, mock := newRegexMock(t)
	store, _ := management.NewPGRepoStoreWithSchema(db, pgRepoStoreTestSchema)

	repoID := uuid.Must(uuid.NewV4())

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT mode FROM "clean_code_mgmt_test"\."repo" WHERE repo_id = \$1 FOR UPDATE`).
		WithArgs(repoID).
		WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow(management.RepoModeEmbedded))
	mock.ExpectExec(`UPDATE "clean_code_mgmt_test"\."repo" SET mode = \$2 WHERE repo_id = \$1`).
		WithArgs(repoID, management.RepoModeLinked).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO "clean_code_mgmt_test"\."repo_event"\s+\(repo_id, kind, payload_json\)\s+VALUES \(\$1, \$2::"clean_code_mgmt_test"\."repo_event_kind", \$3::jsonb\)`).
		WithArgs(repoID, management.RepoEventKindModeChanged, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	res, err := store.SetRepoMode(context.Background(), management.SetRepoModeRequest{
		RepoID: repoID,
		Mode:   management.RepoModeLinked,
		Actor:  "alice",
	})
	if err != nil {
		t.Fatalf("SetRepoMode: %v", err)
	}
	if !res.Changed {
		t.Errorf("Changed=%v, want true", res.Changed)
	}
	if res.PreviousMode != management.RepoModeEmbedded {
		t.Errorf("PreviousMode=%q, want %q", res.PreviousMode, management.RepoModeEmbedded)
	}
	if res.Mode != management.RepoModeLinked {
		t.Errorf("Mode=%q, want %q", res.Mode, management.RepoModeLinked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

// TestPGRepoStore_SetRepoMode_NoOpCommitsWithoutEvent
// verifies the canonical no-op: when the row is already
// at the target mode, the txn commits to release the
// FOR UPDATE lock but issues NO UPDATE and NO event INSERT.
func TestPGRepoStore_SetRepoMode_NoOpCommitsWithoutEvent(t *testing.T) {
	t.Parallel()
	db, mock := newRegexMock(t)
	store, _ := management.NewPGRepoStoreWithSchema(db, pgRepoStoreTestSchema)

	repoID := uuid.Must(uuid.NewV4())
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT mode FROM`).
		WithArgs(repoID).
		WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow(management.RepoModeLinked))
	// NO UPDATE expected. NO INSERT expected.
	mock.ExpectCommit()

	res, err := store.SetRepoMode(context.Background(), management.SetRepoModeRequest{
		RepoID: repoID,
		Mode:   management.RepoModeLinked,
		Actor:  "alice",
	})
	if err != nil {
		t.Fatalf("SetRepoMode: %v", err)
	}
	if res.Changed {
		t.Errorf("Changed=%v, want false (no-op)", res.Changed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

// TestPGRepoStore_SetRepoMode_UnknownRepoMapsToSentinel
// verifies that a sql.ErrNoRows on the SELECT-FOR-UPDATE
// surfaces as ErrRepoStoreUnknownRepo (the handler maps
// this to 404).
func TestPGRepoStore_SetRepoMode_UnknownRepoMapsToSentinel(t *testing.T) {
	t.Parallel()
	db, mock := newRegexMock(t)
	store, _ := management.NewPGRepoStoreWithSchema(db, pgRepoStoreTestSchema)

	repoID := uuid.Must(uuid.NewV4())
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT mode FROM`).
		WithArgs(repoID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	_, err := store.SetRepoMode(context.Background(), management.SetRepoModeRequest{
		RepoID: repoID,
		Mode:   management.RepoModeLinked,
		Actor:  "alice",
	})
	if !errors.Is(err, management.ErrRepoStoreUnknownRepo) {
		t.Fatalf("err=%v, want chain containing ErrRepoStoreUnknownRepo", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

// TestPGRepoStore_SetRepoMode_RejectsZeroRepoID + invalid
// mode short-circuit BEFORE the transaction opens.
func TestPGRepoStore_SetRepoMode_RejectsZeroRepoIDBeforeTx(t *testing.T) {
	t.Parallel()
	db, mock := newRegexMock(t)
	store, _ := management.NewPGRepoStoreWithSchema(db, pgRepoStoreTestSchema)
	// No mock expectations -- the store must NOT open a txn
	// for an invalid input.

	_, err := store.SetRepoMode(context.Background(), management.SetRepoModeRequest{
		RepoID: uuid.Nil,
		Mode:   management.RepoModeLinked,
	})
	if !errors.Is(err, management.ErrRepoStoreZeroRepoID) {
		t.Fatalf("err=%v, want ErrRepoStoreZeroRepoID", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v (txn must NOT open for invalid input)", err)
	}
}

func TestPGRepoStore_RegisterRepo_RejectsEmptyURLBeforeTx(t *testing.T) {
	t.Parallel()
	db, mock := newRegexMock(t)
	store, _ := management.NewPGRepoStoreWithSchema(db, pgRepoStoreTestSchema)

	_, err := store.RegisterRepo(context.Background(), management.RegisterRepoRowRequest{
		RepoURL:       "   ",
		DefaultBranch: "main",
	})
	if !errors.Is(err, management.ErrRepoStoreEmptyURL) {
		t.Fatalf("err=%v, want ErrRepoStoreEmptyURL", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

// capturedPayloadMatcher is a sqlmock.Argument that
// captures the JSON payload bound as the 3rd arg of the
// `repo_event` INSERT so the test can assert on its
// content.
type capturedPayloadMatcher struct {
	captured *string
}

func (m capturedPayloadMatcher) Match(v driver.Value) bool {
	switch s := v.(type) {
	case string:
		*m.captured = s
		return true
	case []byte:
		*m.captured = string(s)
		return true
	}
	return false
}

// Compile-time sanity: the regex matcher must compile.
var _ = regexp.MustCompile
