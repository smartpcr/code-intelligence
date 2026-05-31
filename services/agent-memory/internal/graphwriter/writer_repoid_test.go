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
	"strings"
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

// ensureRepoWithIDInsertRE pins the full SQL contract for
// EnsureRepoWithID, not just the column list. The regex enforces:
//
//   - `repo_id` IS in the INSERT column list (distinguishes from
//     EnsureRepo, which omits it and relies on the schema's
//     gen_random_uuid() default).
//   - ON CONFLICT target is `(url)` -- not `(repo_id)` and not
//     a DO NOTHING clause.
//   - The DO UPDATE SET clause is exactly the three mutable
//     columns (default_branch, current_head_sha, language_hints)
//     in that order, followed directly by RETURNING. Adding
//     `repo_id = EXCLUDED.repo_id` to the SET clause -- the
//     architecture-S3.4 violation we are guarding against --
//     would shift the SET-clause prefix and fail this match.
//   - RETURNING surfaces `repo_id::text` so the caller can see
//     either the supplied UUID (fresh insert) or the legacy
//     pre-existing UUID (collision).
//
// Whitespace runs are matched flexibly with `\s+` so production
// SQL formatting tweaks don't churn the test, but the structural
// invariants above are load-bearing.
var ensureRepoWithIDInsertRE = `(?s)` +
	`INSERT\s+INTO\s+repo\s*\(\s*repo_id\s*,\s*url\s*,\s*default_branch\s*,\s*current_head_sha\s*,\s*language_hints\s*\)\s*` +
	`VALUES\s*\(\s*\$1::uuid\s*,\s*\$2\s*,\s*\$3\s*,\s*\$4\s*,\s*\$5\s*\)\s*` +
	`ON\s+CONFLICT\s*\(\s*url\s*\)\s+DO\s+UPDATE\s+SET\s+` +
	`default_branch\s*=\s*EXCLUDED\.default_branch\s*,\s*` +
	`current_head_sha\s*=\s*EXCLUDED\.current_head_sha\s*,\s*` +
	`language_hints\s*=\s*EXCLUDED\.language_hints\s+` +
	`RETURNING\s+repo_id::text\s*,\s*\(\s*xmax\s*=\s*0\s*\)\s+AS\s+inserted`

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

// capturingMatcher is a sqlmock.QueryMatcher that records every
// SQL string the writer issues so a downstream assertion can
// reason about its full text -- in particular about clauses that
// must NOT appear (no `repo_id` re-key in DO UPDATE SET). It
// always accepts the query so the surrounding test controls
// flow via the standard mock.ExpectQuery WithArgs/WillReturn*
// chain; the matcher is only there to expose the SQL.
type capturingMatcher struct {
	captured []string
}

func (c *capturingMatcher) Match(_ string, actualSQL string) error {
	c.captured = append(c.captured, actualSQL)
	return nil
}

// TestEnsureRepoWithID_sqlContract_noRepoIDInUpdateSet captures
// the exact SQL string EnsureRepoWithID issues and asserts the
// architecture-S3.4 invariants directly, independent of any
// regex used elsewhere:
//
//  1. ON CONFLICT target is `(url)`.
//  2. The DO UPDATE SET clause does NOT mention `repo_id`
//     (the legacy-collision caveat depends on this -- if SET
//     re-keyed the row, RETURNING would yield the supplied
//     UUID instead of the pre-existing one and the
//     collision-detection contract would silently break).
//  3. RETURNING surfaces repo_id::text so collision detection
//     works.
//
// Using a capturing QueryMatcher keeps the assertion structural
// (substring presence/absence on the actual SQL bytes) rather
// than re-encoding the same intent in a regex.
func TestEnsureRepoWithID_sqlContract_noRepoIDInUpdateSet(t *testing.T) {
	t.Parallel()

	cap := &capturingMatcher{}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(cap))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	w := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const url = "https://example.com/contract/check.git"
	suppliedID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(".*").
		WithArgs(suppliedID.String(), url, "main", "abc", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow(suppliedID.String(), true))
	mock.ExpectCommit()

	if _, err := w.EnsureRepoWithID(context.Background(), RepoInput{
		URL:            url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "abc",
		RepoID:         suppliedID,
	}); err != nil {
		t.Fatalf("EnsureRepoWithID: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}

	if len(cap.captured) != 1 {
		t.Fatalf("captured %d SQL statements, want 1: %#v", len(cap.captured), cap.captured)
	}
	sqlText := cap.captured[0]

	// Required structural elements.
	required := []string{
		"INSERT INTO repo",
		"repo_id",
		"ON CONFLICT (url) DO UPDATE SET",
		"RETURNING repo_id::text",
	}
	for _, want := range required {
		if !strings.Contains(sqlText, want) {
			t.Errorf("EnsureRepoWithID SQL is missing required substring %q;\nfull SQL:\n%s",
				want, sqlText)
		}
	}

	// Slice out the DO UPDATE SET ... RETURNING window and assert
	// `repo_id` does not appear inside it. This is the load-bearing
	// non-rekey invariant from architecture S3.4.
	setIdx := strings.Index(sqlText, "DO UPDATE SET")
	retIdx := strings.Index(sqlText, "RETURNING")
	if setIdx < 0 || retIdx <= setIdx {
		t.Fatalf("could not locate DO UPDATE SET..RETURNING window in SQL:\n%s", sqlText)
	}
	setClause := sqlText[setIdx:retIdx]
	if strings.Contains(setClause, "repo_id") {
		t.Errorf(
			"DO UPDATE SET clause re-keys repo_id (forbidden by architecture S3.4);\nSET clause:\n%s",
			setClause,
		)
	}
}
