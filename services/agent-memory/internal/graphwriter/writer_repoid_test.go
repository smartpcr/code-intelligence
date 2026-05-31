package graphwriter

// Behavioural unit tests for EnsureRepoWithID, the
// precomputed-PK variant of EnsureRepo added by the Stage 3.2
// graphsink storage-abstraction work. Driven through
// `go-sqlmock` so the SQL contract is pinned without a live
// PostgreSQL dependency.
//
// Two scenarios mirror the architecture S3.4 / S6.5 parity
// gap the workstream brief calls out:
//
//   * Fresh insert: the supplied RepoID lands as the row's
//     primary key and RETURNING reflects it. (a)
//
//   * URL collision with a legacy row whose `repo_id` differs:
//     the ON CONFLICT (url) DO UPDATE path does NOT re-key the
//     row, so RETURNING surfaces the PRE-EXISTING `repo_id`.
//     The caller sees inserted=false and ID != in.RepoID --
//     the documented "legacy-collision" caveat. (b)

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// newMockWriter returns a Writer wired to a sqlmock-backed
// *sql.DB and a silent logger. The cleanup verifies all
// expectations were met and closes the DB.
func newMockWriter(t *testing.T) (*Writer, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherRegexp,
	))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	w := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return w, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// ensureRepoWithIDInsertRE matches the EnsureRepoWithID INSERT
// up to whitespace, so the regex stays robust to formatting
// tweaks in the production SQL. The key invariants:
//
//   - `repo_id` IS in the column list (distinguishes from
//     EnsureRepo, which omits it and lets the schema default
//     fire).
//   - ON CONFLICT target is `(url)`.
//   - DO UPDATE SET does NOT mention `repo_id` (S3.4: never
//     re-key the row).
var ensureRepoWithIDInsertRE = regexp.QuoteMeta(
	`INSERT INTO repo (repo_id, url, default_branch, current_head_sha, language_hints)`,
)

func TestEnsureRepoWithID_freshInsert_returnsSuppliedUUID(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newMockWriter(t)
	defer cleanup()

	const url = "https://example.com/acme/widgets.git"
	suppliedID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	suppliedIDStr := suppliedID.String()

	mock.ExpectBegin()
	mock.ExpectQuery(ensureRepoWithIDInsertRE).
		WithArgs(suppliedIDStr, url, "main", "deadbeef", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow(suppliedIDStr, true))
	mock.ExpectCommit()

	rec, err := w.EnsureRepoWithID(context.Background(), RepoInput{
		URL:            url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
		LanguageHints:  []string{"go"},
		RepoID:         suppliedID,
	})
	if err != nil {
		t.Fatalf("EnsureRepoWithID: %v", err)
	}
	if !rec.Inserted {
		t.Errorf("Inserted = false, want true on fresh insert")
	}
	if rec.RepoID != suppliedIDStr {
		t.Errorf("RepoID = %q, want %q", rec.RepoID, suppliedIDStr)
	}
	if rec.ID != suppliedID {
		t.Errorf("ID = %v, want %v (parity: fresh insert == supplied)", rec.ID, suppliedID)
	}
}

func TestEnsureRepoWithID_urlCollision_returnsExistingUUID(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newMockWriter(t)
	defer cleanup()

	const url = "https://example.com/acme/widgets.git"
	suppliedID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	// Simulate a row that was inserted before the caller adopted
	// the deterministic RepoIDFromURL scheme; its repo_id is a
	// random UUID that differs from `suppliedID`.
	legacyIDStr := "11111111-2222-3333-4444-555555555555"
	legacyID, err := fingerprint.ParseRepoID(legacyIDStr)
	if err != nil {
		t.Fatalf("ParseRepoID: %v", err)
	}
	if legacyID == suppliedID {
		t.Fatalf("test bug: legacyID == suppliedID")
	}

	mock.ExpectBegin()
	mock.ExpectQuery(ensureRepoWithIDInsertRE).
		WithArgs(suppliedID.String(), url, "main", "cafebabe", sqlmock.AnyArg()).
		// ON CONFLICT (url) DO UPDATE fires; RETURNING reflects
		// the PRE-EXISTING repo_id (not the supplied one) and
		// `(xmax = 0)` is false for an updated row.
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow(legacyIDStr, false))
	mock.ExpectCommit()

	rec, err := w.EnsureRepoWithID(context.Background(), RepoInput{
		URL:            url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "cafebabe",
		LanguageHints:  []string{"go"},
		RepoID:         suppliedID,
	})
	if err != nil {
		t.Fatalf("EnsureRepoWithID: %v", err)
	}
	if rec.Inserted {
		t.Errorf("Inserted = true, want false on URL collision")
	}
	if rec.RepoID != legacyIDStr {
		t.Errorf("RepoID = %q, want %q (legacy-collision: existing repo_id wins)",
			rec.RepoID, legacyIDStr)
	}
	if rec.ID != legacyID {
		t.Errorf("ID = %v, want legacy %v", rec.ID, legacyID)
	}
	if rec.ID == suppliedID {
		t.Errorf("ID should diverge from supplied RepoID on legacy collision")
	}
}

func TestEnsureRepoWithID_zeroRepoID_rejected(t *testing.T) {
	t.Parallel()
	w, _, cleanup := newMockWriter(t)
	defer cleanup()

	// No mock expectations: the validation guard must fire
	// before any SQL is issued.
	_, err := w.EnsureRepoWithID(context.Background(), RepoInput{
		URL:           "https://example.com/acme/widgets.git",
		DefaultBranch: "main",
		// RepoID intentionally omitted (zero value).
	})
	if err == nil {
		t.Fatal("EnsureRepoWithID with zero RepoID returned nil; want explicit rejection")
	}
}

func TestEnsureRepoWithID_emptyURL_rejected(t *testing.T) {
	t.Parallel()
	w, _, cleanup := newMockWriter(t)
	defer cleanup()

	id, err := fingerprint.RepoIDFromURL("https://example.com/a.git")
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	_, err = w.EnsureRepoWithID(context.Background(), RepoInput{
		URL:    "",
		RepoID: id,
	})
	if err == nil {
		t.Fatal("EnsureRepoWithID with empty URL returned nil; want explicit rejection")
	}
}

// TestEnsureRepo_ignoresRepoIDField pins the behaviour-preserving
// contract: extending RepoInput with the RepoID field must not
// change EnsureRepo's SQL. EnsureRepo continues to omit `repo_id`
// from the column list and rely on the schema default
// (`gen_random_uuid()`), regardless of whether the caller
// populated RepoInput.RepoID.
func TestEnsureRepo_ignoresRepoIDField(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newMockWriter(t)
	defer cleanup()

	const url = "https://example.com/legacy/path.git"
	suppliedID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	serverAssigned := "99999999-8888-7777-6666-555555555555"

	mock.ExpectBegin()
	// Note: the SQL begins `INSERT INTO repo (url, default_branch, ...)` --
	// no `repo_id` column. WithArgs therefore only sees four positional
	// arguments (url, default_branch, current_head_sha, language_hints).
	mock.ExpectQuery(regexp.QuoteMeta(
		`INSERT INTO repo (url, default_branch, current_head_sha, language_hints)`,
	)).
		WithArgs(url, "main", "feedface", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow(serverAssigned, true))
	mock.ExpectCommit()

	rec, err := w.EnsureRepo(context.Background(), RepoInput{
		URL:            url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "feedface",
		LanguageHints:  []string{"go"},
		RepoID:         suppliedID, // must be ignored
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if rec.RepoID != serverAssigned {
		t.Errorf("RepoID = %q, want server-assigned %q (EnsureRepo must ignore RepoInput.RepoID)",
			rec.RepoID, serverAssigned)
	}
}
