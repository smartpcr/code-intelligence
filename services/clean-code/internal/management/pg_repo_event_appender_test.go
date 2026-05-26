package management_test

// Stage 3.4 -- sqlmock tests for [PGRepoEventAppender].
// Verifies the canonical INSERT shape against the
// `clean_code.repo_event` schema (migrations/0001
// lines 298-319): `(repo_id, kind, payload_json)` with
// `kind` cast to the canonical enum and `payload_json`
// cast to jsonb.

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	management "github.com/microsoft/code-intelligence/services/clean-code/internal/management"
)

const pgRepoEventTestSchema = "clean_code_mgmt_test"

func TestPGRepoEventAppender_RejectsNilDB(t *testing.T) {
	_, err := management.NewPGRepoEventAppender(nil)
	if !errors.Is(err, management.ErrPGRepoEventAppenderNilDB) {
		t.Fatalf("err: got %v; want ErrPGRepoEventAppenderNilDB", err)
	}
}

func TestPGRepoEventAppender_RejectsEmptySchema(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	_, err := management.NewPGRepoEventAppenderWithSchema(db, "  ")
	if !errors.Is(err, management.ErrPGRepoEventAppenderEmptySchema) {
		t.Fatalf("err: got %v; want ErrPGRepoEventAppenderEmptySchema", err)
	}
}

func TestPGRepoEventAppender_AppendRepoEvent_InsertsCanonicalShape(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	appender, err := management.NewPGRepoEventAppenderWithSchema(db, pgRepoEventTestSchema)
	if err != nil {
		t.Fatalf("NewPGRepoEventAppenderWithSchema: %v", err)
	}

	repoID := uuid.Must(uuid.NewV4())
	sampleID := uuid.Must(uuid.NewV4())
	payload := map[string]any{
		"sample_id": sampleID.String(),
		"reason":    "vendored file",
	}

	mock.ExpectExec(`INSERT INTO "clean_code_mgmt_test"."repo_event"\s+\(repo_id, kind, payload_json\)\s+VALUES \(\$1, \$2::"clean_code_mgmt_test"\."repo_event_kind", \$3::jsonb\)`).
		WithArgs(repoID, "retract_intent", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := appender.AppendRepoEvent(context.Background(), repoID, "retract_intent", payload); err != nil {
		t.Fatalf("AppendRepoEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

func TestPGRepoEventAppender_AppendRepoEvent_NilPayloadBindsEmptyObject(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	appender, _ := management.NewPGRepoEventAppenderWithSchema(db, pgRepoEventTestSchema)

	repoID := uuid.Must(uuid.NewV4())

	mock.ExpectExec(`INSERT INTO "clean_code_mgmt_test"."repo_event"`).
		WithArgs(repoID, "registered", "{}").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := appender.AppendRepoEvent(context.Background(), repoID, "registered", nil); err != nil {
		t.Fatalf("AppendRepoEvent (nil payload): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

func TestPGRepoEventAppender_AppendRepoEvent_RejectsZeroRepoID(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	appender, _ := management.NewPGRepoEventAppenderWithSchema(db, pgRepoEventTestSchema)

	if err := appender.AppendRepoEvent(context.Background(), uuid.Nil, "registered", nil); err == nil {
		t.Fatal("AppendRepoEvent(zero repoID): expected error")
	}
}

func TestPGRepoEventAppender_AppendRepoEvent_RejectsEmptyKind(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	appender, _ := management.NewPGRepoEventAppenderWithSchema(db, pgRepoEventTestSchema)

	if err := appender.AppendRepoEvent(context.Background(), uuid.Must(uuid.NewV4()), "  ", nil); err == nil {
		t.Fatal("AppendRepoEvent(empty kind): expected error")
	}
}
