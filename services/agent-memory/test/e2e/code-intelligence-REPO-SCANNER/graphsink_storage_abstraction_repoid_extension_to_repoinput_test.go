//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type repoIDExtState struct {
	// Writer and mock wired per-scenario.
	writer  *graphwriter.Writer
	mock    sqlmock.Sqlmock
	closeFn func()

	// Log buffer captures structured logs for parity-gap assertion.
	logBuf *bytes.Buffer

	// Inputs computed in Given steps.
	suppliedID fingerprint.RepoID
	legacyID   fingerprint.RepoID
	url        string

	// Output captured in When steps.
	rec graphwriter.RepoRecord
	err error
}

func newRepoIDExtState() *repoIDExtState {
	return &repoIDExtState{}
}

// initWriter creates a fresh sqlmock-backed Writer with a log
// buffer that captures structured output.
func (st *repoIDExtState) initWriter() {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherRegexp,
	))
	if err != nil {
		panic("sqlmock.New: " + err.Error())
	}
	st.mock = mock
	st.logBuf = &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(st.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st.writer = graphwriter.New(db, logger)
	st.closeFn = func() { _ = db.Close() }
}

// ---------------------------------------------------------------------------
// Scenario: ensurerepowithid-deterministic-insert
// ---------------------------------------------------------------------------

func (st *repoIDExtState) anEmptyRepoTableAndANonZeroRepoID() error {
	st.initWriter()
	st.url = "https://example.com/acme/widgets.git"
	var err error
	st.suppliedID, err = fingerprint.RepoIDFromURL(st.url)
	if err != nil {
		return err
	}
	if st.suppliedID.IsZero() {
		return godog.ErrPending
	}

	// Mock: fresh insert returns the supplied UUID, inserted=true.
	st.mock.ExpectBegin()
	st.mock.ExpectQuery(`INSERT\s+INTO\s+repo`).
		WithArgs(st.suppliedID.String(), st.url, "main", "deadbeef", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow(st.suppliedID.String(), true))
	st.mock.ExpectCommit()
	return nil
}

func (st *repoIDExtState) ensureRepoWithIDRuns() error {
	st.rec, st.err = st.writer.EnsureRepoWithID(context.Background(), graphwriter.RepoInput{
		URL:            st.url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
		LanguageHints:  []string{"go"},
		RepoID:         st.suppliedID,
	})
	return nil
}

func (st *repoIDExtState) theRowRepoIDEqualsTheSuppliedUUID() error {
	defer st.closeFn()
	if st.err != nil {
		return st.err
	}
	if st.rec.RepoID != st.suppliedID.String() {
		return fmt.Errorf("RepoID = %q, want %q", st.rec.RepoID, st.suppliedID.String())
	}
	if st.rec.ID != st.suppliedID {
		return fmt.Errorf("ID = %v, want %v", st.rec.ID, st.suppliedID)
	}
	if !st.rec.Inserted {
		return fmt.Errorf("Inserted = false, want true on fresh insert")
	}
	if err := st.mock.ExpectationsWereMet(); err != nil {
		return fmt.Errorf("unmet sqlmock expectations: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: ensurerepo-zero-id-uses-default
// ---------------------------------------------------------------------------

func (st *repoIDExtState) aZeroValueRepoID() error {
	st.initWriter()
	st.url = "https://example.com/legacy/path.git"
	// RepoID deliberately left as zero value.
	st.suppliedID = fingerprint.RepoID{}

	// The server assigns a UUID via gen_random_uuid().
	serverAssigned := "99999999-8888-7777-6666-555555555555"

	// Mock: EnsureRepo (legacy path) omits repo_id from INSERT columns.
	st.mock.ExpectBegin()
	st.mock.ExpectQuery(`INSERT\s+INTO\s+repo\s*\(\s*url`).
		WithArgs(st.url, "main", "feedface", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow(serverAssigned, true))
	st.mock.ExpectCommit()
	return nil
}

func (st *repoIDExtState) ensureRepoRunsLegacyPath() error {
	st.rec, st.err = st.writer.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL:            st.url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "feedface",
		LanguageHints:  []string{"go"},
		// RepoID is zero — EnsureRepo ignores it.
	})
	return nil
}

func (st *repoIDExtState) theRowRepoIDIsAllocatedByGenRandomUUIDAndIsNonZero() error {
	defer st.closeFn()
	if st.err != nil {
		return st.err
	}
	if st.rec.RepoID == "" {
		return fmt.Errorf("RepoID is empty, want server-assigned UUID")
	}
	if st.rec.ID.IsZero() {
		return fmt.Errorf("ID is zero, want non-zero server-assigned UUID")
	}
	// EnsureRepo must not use the (zero) RepoInput.RepoID.
	expected := "99999999-8888-7777-6666-555555555555"
	if st.rec.RepoID != expected {
		return fmt.Errorf("RepoID = %q, want server-assigned %q", st.rec.RepoID, expected)
	}
	if err := st.mock.ExpectationsWereMet(); err != nil {
		return fmt.Errorf("unmet sqlmock expectations: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: url-collision-returns-existing
// ---------------------------------------------------------------------------

func (st *repoIDExtState) anExistingRowWithURLAndRepoIDA() error {
	st.initWriter()
	st.url = "https://x/y"

	var err error
	// "B" — the precomputed ID the caller supplies.
	st.suppliedID, err = fingerprint.RepoIDFromURL(st.url)
	if err != nil {
		return err
	}

	// "A" — the legacy UUID that already sits on the row.
	st.legacyID, err = fingerprint.ParseRepoID("11111111-2222-3333-4444-555555555555")
	if err != nil {
		return err
	}
	if st.legacyID == st.suppliedID {
		return fmt.Errorf("test bug: legacyID == suppliedID")
	}

	// Mock: ON CONFLICT fires; RETURNING surfaces the PRE-EXISTING
	// repo_id and inserted=false.
	st.mock.ExpectBegin()
	st.mock.ExpectQuery(`INSERT\s+INTO\s+repo`).
		WithArgs(st.suppliedID.String(), st.url, "main", "cafebabe", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow(st.legacyID.String(), false))
	st.mock.ExpectCommit()
	return nil
}

func (st *repoIDExtState) ensureRepoWithIDRunsWithSameURLAndDifferentRepoID() error {
	st.rec, st.err = st.writer.EnsureRepoWithID(context.Background(), graphwriter.RepoInput{
		URL:            st.url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "cafebabe",
		LanguageHints:  []string{"go"},
		RepoID:         st.suppliedID,
	})
	return nil
}

func (st *repoIDExtState) theReturnedRepoIDEqualsAAndLogRecordsParityGap() error {
	defer st.closeFn()
	if st.err != nil {
		return st.err
	}
	if st.rec.RepoID != st.legacyID.String() {
		return fmt.Errorf(
			"RepoID = %q, want legacy %q (existing row wins on URL collision)",
			st.rec.RepoID, st.legacyID.String(),
		)
	}
	if st.rec.ID != st.legacyID {
		return fmt.Errorf("ID = %v, want legacy %v", st.rec.ID, st.legacyID)
	}
	if st.rec.Inserted {
		return fmt.Errorf("Inserted = true, want false on URL collision")
	}
	if st.rec.ID == st.suppliedID {
		return fmt.Errorf("ID should diverge from supplied RepoID on legacy collision")
	}

	// Verify structured log records the parity gap.
	logOutput := st.logBuf.String()
	if logOutput == "" {
		return fmt.Errorf("no structured log output captured")
	}
	// The Writer logs legacy_collision=true when !rec.Inserted && rec.ID != in.RepoID.
	if !bytes.Contains(st.logBuf.Bytes(), []byte("legacy_collision")) {
		return fmt.Errorf(
			"structured log does not contain 'legacy_collision'; got:\n%s",
			logOutput,
		)
	}

	if err := st.mock.ExpectationsWereMet(); err != nil {
		return fmt.Errorf("unmet sqlmock expectations: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_graphsink_storage_abstraction_repoid_extension_to_repoinput(ctx *godog.ScenarioContext) {
	st := newRepoIDExtState()

	// Scenario: ensurerepowithid-deterministic-insert
	ctx.Given(`^an empty "repo" table and a non-zero "RepoInput\.RepoID"$`, st.anEmptyRepoTableAndANonZeroRepoID)
	ctx.When(`^"EnsureRepoWithID" runs$`, st.ensureRepoWithIDRuns)
	ctx.Then(`^the row's "repo_id" equals the supplied UUID$`, st.theRowRepoIDEqualsTheSuppliedUUID)

	// Scenario: ensurerepo-zero-id-uses-default
	ctx.Given(`^a zero-value "RepoInput\.RepoID"$`, st.aZeroValueRepoID)
	ctx.When(`^"EnsureRepo" runs via the legacy path$`, st.ensureRepoRunsLegacyPath)
	ctx.Then(`^the row's "repo_id" is allocated by "gen_random_uuid\(\)" and is non-zero$`, st.theRowRepoIDIsAllocatedByGenRandomUUIDAndIsNonZero)

	// Scenario: url-collision-returns-existing
	ctx.Given(`^an existing row with URL "https://x/y" and "repo_id = A"$`, st.anExistingRowWithURLAndRepoIDA)
	ctx.When(`^"EnsureRepoWithID" runs with the same URL and a different precomputed "repo_id = B"$`, st.ensureRepoWithIDRunsWithSameURLAndDifferentRepoID)
	ctx.Then(`^the returned "RepoRecord\.RepoID" equals "A" and a structured log records the parity gap$`, st.theReturnedRepoIDEqualsAAndLogRecordsParityGap)
}

func TestE2E_graphsink_storage_abstraction_repoid_extension_to_repoinput(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"graphsink_storage_abstraction_repoid_extension_to_repoinput.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_graphsink_storage_abstraction_repoid_extension_to_repoinput,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{featurePath},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
