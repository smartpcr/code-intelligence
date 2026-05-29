package management_test

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/management"
)

var registerRepoTestID = uuid.Must(uuid.FromString("bbbbbbbb-cccc-dddd-eeee-ffff00007777"))

// TestRegisterRepoRequest_Validate pins the per-field
// validation surface so callers can front-load validation
// without opening a DB connection.
func TestRegisterRepoRequest_Validate(t *testing.T) {
	t.Parallel()

	good := management.RegisterRepoRequest{
		RepoID:        registerRepoTestID,
		DisplayName:   "test-repo",
		DefaultBranch: "main",
		RepoURL:       "https://example.com/org/repo",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good Validate(): err=%v, want nil", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*management.RegisterRepoRequest)
		want error
	}{
		{"zero-RepoID", func(r *management.RegisterRepoRequest) { r.RepoID = uuid.Nil }, management.ErrRegisterRepoZeroID},
		{"empty-DisplayName", func(r *management.RegisterRepoRequest) { r.DisplayName = "" }, management.ErrRegisterRepoEmptyDisplayName},
		{"whitespace-DisplayName", func(r *management.RegisterRepoRequest) { r.DisplayName = "   " }, management.ErrRegisterRepoEmptyDisplayName},
		{"empty-DefaultBranch", func(r *management.RegisterRepoRequest) { r.DefaultBranch = "" }, management.ErrRegisterRepoEmptyDefaultBranch},
		{"empty-RepoURL", func(r *management.RegisterRepoRequest) { r.RepoURL = "" }, management.ErrRegisterRepoEmptyURL},
		{"whitespace-RepoURL", func(r *management.RegisterRepoRequest) { r.RepoURL = "   " }, management.ErrRegisterRepoEmptyURL},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := good
			tc.mut(&req)
			if err := req.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("[%s] Validate(): err=%v, want errors.Is %v", tc.name, err, tc.want)
			}
		})
	}
}

// TestRegisterRepo_HappyPath_IncludesRepoURL is the iter-7
// evaluator item 2 anchor test: the helper MUST include
// `repo_url` in the INSERT column list so the trigger-enforced
// WRITE-ONCE column gets populated at registration time. If
// the helper ever regresses to inserting only
// (repo_id, display_name, default_branch), this regex would
// fail to match and the test would fail loudly.
func TestRegisterRepo_HappyPath_IncludesRepoURL(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	want := "https://example.com/org/repo"
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO clean_code.repo`) +
		`\s*\(repo_id,\s+display_name,\s+default_branch,\s+repo_url\)`).
		WithArgs(registerRepoTestID, "test-repo", "main", want).
		WillReturnResult(sqlmock.NewResult(0, 1))

	affected, err := management.RegisterRepo(context.Background(), db, management.RegisterRepoRequest{
		RepoID:        registerRepoTestID,
		DisplayName:   "test-repo",
		DefaultBranch: "main",
		RepoURL:       want,
	})
	if err != nil {
		t.Fatalf("RegisterRepo: err=%v, want nil", err)
	}
	if affected != 1 {
		t.Errorf("RegisterRepo: affected=%d, want 1", affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: unmet expectations: %v", err)
	}
}

// TestRegisterRepo_HappyPath_WithModeIncludesModeColumn pins
// the variant that supplies an explicit `mode` -- the column
// list MUST include `mode` so the operator-chosen value
// overrides the DB DEFAULT.
func TestRegisterRepo_HappyPath_WithModeIncludesModeColumn(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO clean_code.repo`) +
		`\s*\(repo_id,\s+display_name,\s+default_branch,\s+repo_url,\s+mode\)`).
		WithArgs(registerRepoTestID, "test-repo", "main", "https://example.com/org/repo", "linked").
		WillReturnResult(sqlmock.NewResult(0, 1))

	affected, err := management.RegisterRepo(context.Background(), db, management.RegisterRepoRequest{
		RepoID:        registerRepoTestID,
		DisplayName:   "test-repo",
		DefaultBranch: "main",
		RepoURL:       "https://example.com/org/repo",
		Mode:          "linked",
	})
	if err != nil {
		t.Fatalf("RegisterRepo (with mode): err=%v, want nil", err)
	}
	if affected != 1 {
		t.Errorf("RegisterRepo (with mode): affected=%d, want 1", affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: unmet expectations: %v", err)
	}
}

// TestRegisterRepo_OnConflictDoNothingIdempotent pins the
// re-registration surface: a second call with the same
// repo_id returns affected=0 (ON CONFLICT fired) and no
// error. This is the contract test fixtures rely on for
// idempotent setup.
func TestRegisterRepo_OnConflictDoNothingIdempotent(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO clean_code.repo`) +
		`\s*\(repo_id,\s+display_name,\s+default_branch,\s+repo_url\)` +
		`[\s\S]*ON\s+CONFLICT\s+\(repo_id\)\s+DO\s+NOTHING`).
		WithArgs(registerRepoTestID, "test-repo", "main", "https://example.com/org/repo").
		WillReturnResult(sqlmock.NewResult(0, 0)) // conflict -> 0 rows

	affected, err := management.RegisterRepo(context.Background(), db, management.RegisterRepoRequest{
		RepoID:        registerRepoTestID,
		DisplayName:   "test-repo",
		DefaultBranch: "main",
		RepoURL:       "https://example.com/org/repo",
	})
	if err != nil {
		t.Fatalf("RegisterRepo (conflict): err=%v, want nil", err)
	}
	if affected != 0 {
		t.Errorf("RegisterRepo (conflict): affected=%d, want 0 (existing row)", affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: unmet expectations: %v", err)
	}
}

// TestRegisterRepo_RejectsNilDB pins the composition-root
// error surface (mirrors the pattern used in PGScanRunStore
// and PGScopeBindingResolver constructors).
func TestRegisterRepo_RejectsNilDB(t *testing.T) {
	t.Parallel()
	_, err := management.RegisterRepo(context.Background(), nil, management.RegisterRepoRequest{
		RepoID:        registerRepoTestID,
		DisplayName:   "test-repo",
		DefaultBranch: "main",
		RepoURL:       "https://example.com/org/repo",
	})
	if !errors.Is(err, management.ErrRegisterRepoNilDB) {
		t.Errorf("RegisterRepo(nil db): err=%v, want errors.Is ErrRegisterRepoNilDB", err)
	}
}

// TestRegisterRepo_ValidatesArgsBeforeDB pins the
// fail-fast-before-DB-call surface: a bad request rejects
// without any sqlmock ExpectExec being declared. If the
// helper reached the DB anyway, the mock would fail with
// "unexpected query".
func TestRegisterRepo_ValidatesArgsBeforeDB(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Empty RepoURL -- the iter-7 anchor case.
	_, err = management.RegisterRepo(context.Background(), db, management.RegisterRepoRequest{
		RepoID:        registerRepoTestID,
		DisplayName:   "test-repo",
		DefaultBranch: "main",
		RepoURL:       "",
	})
	if !errors.Is(err, management.ErrRegisterRepoEmptyURL) {
		t.Errorf("RegisterRepo (empty RepoURL): err=%v, want errors.Is ErrRegisterRepoEmptyURL", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: unmet expectations after validation reject: %v", err)
	}
}

// TestRegisterRepoWithSchema_RejectsEmptySchema pins the
// schema-aware variant's input guard.
func TestRegisterRepoWithSchema_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, bad := range []string{"", "   "} {
		_, err := management.RegisterRepoWithSchema(context.Background(), db, management.RegisterRepoRequest{
			RepoID:        registerRepoTestID,
			DisplayName:   "test-repo",
			DefaultBranch: "main",
			RepoURL:       "https://example.com/org/repo",
		}, bad)
		if err == nil {
			t.Errorf("RegisterRepoWithSchema(schema=%q): err=nil, want non-nil", bad)
		}
	}
}
