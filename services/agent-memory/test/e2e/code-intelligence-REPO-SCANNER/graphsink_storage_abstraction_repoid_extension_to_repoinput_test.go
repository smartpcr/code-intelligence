//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Postgres provisioning — provided instance or embedded ephemeral
// ---------------------------------------------------------------------------

const (
	repoIDExtEnvPGURL = "AGENT_MEMORY_PG_URL"
	repoIDExtTimeout  = 60 * time.Second
)

// pgInstance holds a Postgres connection (provided or ephemeral)
// and a cleanup function that tears it down.
type pgInstance struct {
	db      *sql.DB
	cleanup func()
}

// openRepoIDExtPG returns a *sql.DB connected to a Postgres
// instance with the repo table schema applied. It tries:
//  1. AGENT_MEMORY_PG_URL (provided / compose stack)
//  2. embedded-postgres (ephemeral, no docker)
func openRepoIDExtPG() (*pgInstance, error) {
	if dsn := os.Getenv(repoIDExtEnvPGURL); dsn != "" {
		return openProvidedPG(dsn)
	}
	return openEphemeralPG()
}

// openProvidedPG connects to the provided Postgres and creates a
// per-test schema with migrations applied.
func openProvidedPG(dsn string) (*pgInstance, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open provided: %w", err)
	}
	db.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), repoIDExtTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping provided PG: %w", err)
	}

	schema, err := createTestSchema(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	if err := applyRepoMigrations(ctx, db); err != nil {
		dropTestSchema(db, schema)
		_ = db.Close()
		return nil, err
	}

	return &pgInstance{
		db: db,
		cleanup: func() {
			dropTestSchema(db, schema)
			_ = db.Close()
		},
	}, nil
}

// openEphemeralPG starts an embedded Postgres process, applies
// the repo table schema, and returns a handle.
func openEphemeralPG() (*pgInstance, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	port := 15432 + int(buf[0])%100

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(uint32(port)).
			Database("repoid_e2e_test").
			Username("test").
			Password("test").
			Logger(nil),
	)
	if err := pg.Start(); err != nil {
		return nil, fmt.Errorf("embedded-postgres start: %w", err)
	}

	dsn := fmt.Sprintf(
		"postgres://test:test@localhost:%d/repoid_e2e_test?sslmode=disable",
		port,
	)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		_ = pg.Stop()
		return nil, fmt.Errorf("sql.Open ephemeral: %w", err)
	}
	db.SetMaxOpenConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), repoIDExtTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_ = pg.Stop()
		return nil, fmt.Errorf("ping ephemeral: %w", err)
	}

	if err := applyRepoMigrations(ctx, db); err != nil {
		_ = db.Close()
		_ = pg.Stop()
		return nil, fmt.Errorf("apply migrations ephemeral: %w", err)
	}

	return &pgInstance{
		db: db,
		cleanup: func() {
			_ = db.Close()
			_ = pg.Stop()
		},
	}, nil
}

func createTestSchema(ctx context.Context, db *sql.DB) (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	schema := "repoid_e2e_" + hex.EncodeToString(buf[:])
	if _, err := db.ExecContext(ctx,
		`CREATE SCHEMA `+quoteIdentRepoID(schema),
	); err != nil {
		return "", fmt.Errorf("create schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`SET search_path TO %s, public`, quoteIdentRepoID(schema),
	)); err != nil {
		return "", fmt.Errorf("set search_path: %w", err)
	}
	return schema, nil
}

func dropTestSchema(db *sql.DB, schema string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, `DROP SCHEMA `+quoteIdentRepoID(schema)+` CASCADE`)
}

func quoteIdentRepoID(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// applyRepoMigrations applies the subset of migrations needed
// for the repo table: 0001 (enums) and 0002 (repo + repo_commit).
// Full migration set may fail on ephemeral PG without pg_partman.
func applyRepoMigrations(ctx context.Context, db *sql.DB) error {
	all, err := migrations.All()
	if err != nil {
		return fmt.Errorf("migrations.All: %w", err)
	}
	needed := map[string]bool{
		"0001": true, // enums (some later migrations reference them)
		"0002": true, // repo + repo_commit tables
	}
	for _, mg := range all {
		if !needed[mg.Version] {
			continue
		}
		body := stripTxnStatements(mg.Up)
		if _, err := db.ExecContext(ctx, body); err != nil {
			return fmt.Errorf("apply %s: %w", mg.Filename, err)
		}
	}
	return nil
}

// stripTxnStatements removes BEGIN/COMMIT/ROLLBACK lines so the
// migration SQL runs outside an explicit transaction block.
func stripTxnStatements(body string) string {
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(strings.ToUpper(line))
		if trimmed == "BEGIN;" || trimmed == "COMMIT;" || trimmed == "ROLLBACK;" ||
			trimmed == "BEGIN" || trimmed == "COMMIT" || trimmed == "ROLLBACK" {
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type repoIDExtState struct {
	pg     *pgInstance
	writer *graphwriter.Writer
	logBuf *bytes.Buffer

	suppliedID fingerprint.RepoID
	legacyID   fingerprint.RepoID
	url        string

	rec graphwriter.RepoRecord
	err error
}

// initPG provisions a Postgres connection and Writer. Called once
// per scenario via the Given step.
func (st *repoIDExtState) initPG() error {
	pg, err := openRepoIDExtPG()
	if err != nil {
		return fmt.Errorf("provision Postgres: %w", err)
	}
	st.pg = pg
	st.logBuf = &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(st.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st.writer = graphwriter.New(pg.db, logger)
	return nil
}

func (st *repoIDExtState) teardown() {
	if st.pg != nil {
		st.pg.cleanup()
		st.pg = nil
	}
}

// ---------------------------------------------------------------------------
// Scenario: ensurerepowithid-deterministic-insert
// ---------------------------------------------------------------------------

func (st *repoIDExtState) anEmptyRepoTableAndANonZeroRepoID() error {
	if err := st.initPG(); err != nil {
		return err
	}
	st.url = "https://example.com/acme/widgets.git"
	var err error
	st.suppliedID, err = fingerprint.RepoIDFromURL(st.url)
	if err != nil {
		return err
	}
	if st.suppliedID.IsZero() {
		return fmt.Errorf("RepoIDFromURL returned zero for %q", st.url)
	}
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
	// Verify persisted row in the database.
	var dbRepoID string
	err := st.pg.db.QueryRowContext(context.Background(),
		`SELECT repo_id::text FROM repo WHERE url = $1`, st.url,
	).Scan(&dbRepoID)
	if err != nil {
		return fmt.Errorf("readback repo row: %w", err)
	}
	if dbRepoID != st.suppliedID.String() {
		return fmt.Errorf("persisted repo_id = %q, want %q", dbRepoID, st.suppliedID.String())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: ensurerepo-zero-id-uses-default
// ---------------------------------------------------------------------------

func (st *repoIDExtState) aZeroValueRepoID() error {
	if err := st.initPG(); err != nil {
		return err
	}
	st.url = "https://example.com/legacy/path.git"
	st.suppliedID = fingerprint.RepoID{} // zero
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
	if st.err != nil {
		return st.err
	}
	if st.rec.RepoID == "" {
		return fmt.Errorf("RepoID is empty, want server-assigned UUID")
	}
	if st.rec.ID.IsZero() {
		return fmt.Errorf("ID is zero, want non-zero server-assigned UUID via gen_random_uuid()")
	}
	// Verify the persisted row exists and its repo_id matches.
	var dbRepoID string
	err := st.pg.db.QueryRowContext(context.Background(),
		`SELECT repo_id::text FROM repo WHERE url = $1`, st.url,
	).Scan(&dbRepoID)
	if err != nil {
		return fmt.Errorf("readback repo row: %w", err)
	}
	if dbRepoID != st.rec.RepoID {
		return fmt.Errorf("persisted repo_id = %q, want %q", dbRepoID, st.rec.RepoID)
	}
	// Ensure the allocated UUID is valid (36-char UUID format).
	if len(st.rec.RepoID) != 36 {
		return fmt.Errorf("RepoID length = %d, want 36 (UUID format)", len(st.rec.RepoID))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: url-collision-returns-existing
// ---------------------------------------------------------------------------

func (st *repoIDExtState) anExistingRowWithURLAndRepoIDA() error {
	if err := st.initPG(); err != nil {
		return err
	}
	st.url = "https://x/y"

	// Insert a legacy row using EnsureRepo (gen_random_uuid() assigns its PK).
	legacyRec, err := st.writer.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL:            st.url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "aaa111",
		LanguageHints:  []string{"go"},
	})
	if err != nil {
		return fmt.Errorf("seed legacy row: %w", err)
	}
	st.legacyID = legacyRec.ID

	// Compute the deterministic RepoID the caller would supply.
	st.suppliedID, err = fingerprint.RepoIDFromURL(st.url)
	if err != nil {
		return err
	}
	if st.legacyID == st.suppliedID {
		return fmt.Errorf("test precondition failed: legacy UUID == deterministic UUID; collision scenario is not exercisable")
	}
	return nil
}

func (st *repoIDExtState) ensureRepoWithIDRunsWithSameURLAndDifferentRepoID() error {
	// Reset log buffer to capture only this call's output.
	st.logBuf.Reset()
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

	// Verify the persisted row still has the legacy repo_id.
	var dbRepoID string
	err := st.pg.db.QueryRowContext(context.Background(),
		`SELECT repo_id::text FROM repo WHERE url = $1`, st.url,
	).Scan(&dbRepoID)
	if err != nil {
		return fmt.Errorf("readback repo row: %w", err)
	}
	if dbRepoID != st.legacyID.String() {
		return fmt.Errorf("persisted repo_id = %q, want legacy %q (row must not be re-keyed)",
			dbRepoID, st.legacyID.String())
	}

	// Verify structured log records the parity gap.
	logOutput := st.logBuf.String()
	if !bytes.Contains(st.logBuf.Bytes(), []byte("legacy_collision")) {
		return fmt.Errorf(
			"structured log does not contain 'legacy_collision'; got:\n%s",
			logOutput,
		)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_graphsink_storage_abstraction_repoid_extension_to_repoinput(ctx *godog.ScenarioContext) {
	st := &repoIDExtState{}

	// Ensure Postgres resources are cleaned up even if a Given/When step fails
	// before the Then step runs.
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		st.teardown()
		return ctx, nil
	})

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
