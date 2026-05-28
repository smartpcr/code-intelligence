//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// requireEnv returns the value of the named environment variable,
// calling t.Skip when unset or empty.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping e2e test", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Scenario 1: wal-scope-only-audit-tables
// ---------------------------------------------------------------------------

type walScopeState struct {
	// repoRoot is the root of the Go module (services/clean-code).
	repoRoot string
	// importers maps each importing directory (relative) to its import line.
	importers []importSite
}

type importSite struct {
	file string
	line string
}

func newWalScopeState() *walScopeState {
	return &walScopeState{}
}

func (s *walScopeState) anyCodePathInTheService() error {
	// Resolve repo root: walk up from the test directory until we
	// find go.mod, or fall back to CLEAN_CODE_REPO_ROOT env var.
	root := os.Getenv("CLEAN_CODE_REPO_ROOT")
	if root == "" {
		// Default: two levels up from the test dir gives us services/clean-code
		root = "../../../.."
	}
	s.repoRoot = root
	return nil
}

func (s *walScopeState) greppingTheWriterCallSites() error {
	// Use grep/ripgrep to find all Go files that import
	// internal/audit/wal (excluding the wal package itself and test
	// helpers inside internal/audit/wal/).
	cmd := exec.Command("grep", "-rn", "--include=*.go",
		"internal/audit/wal", s.repoRoot)
	out, err := cmd.Output()
	if err != nil {
		// grep returns exit 1 when no matches — that is acceptable
		// (no importers means the constraint is trivially satisfied).
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			s.importers = nil
			return nil
		}
		return fmt.Errorf("grep failed: %w", err)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Skip files inside the wal package itself.
		if strings.Contains(line, "internal/audit/wal/") {
			continue
		}
		// Skip test infrastructure (this file, other e2e helpers).
		if strings.Contains(line, "test/e2e/") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		site := importSite{file: parts[0]}
		if len(parts) > 1 {
			site.line = parts[1]
		}
		s.importers = append(s.importers, site)
	}
	return nil
}

func (s *walScopeState) walReferencedOnlyFrom(allowedCSV string) error {
	allowed := strings.Split(allowedCSV, `" and "`)
	for i := range allowed {
		allowed[i] = strings.Trim(allowed[i], `"`)
	}

	var violations []string
	for _, site := range s.importers {
		normalised := strings.ReplaceAll(site.file, "\\", "/")
		ok := false
		for _, prefix := range allowed {
			if strings.Contains(normalised, prefix) {
				ok = true
				break
			}
		}
		if !ok {
			violations = append(violations, site.file)
		}
	}

	if len(violations) > 0 {
		return fmt.Errorf(
			"internal/audit/wal is referenced from disallowed paths:\n  %s",
			strings.Join(violations, "\n  "),
		)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2: no-projection-table
// ---------------------------------------------------------------------------

type schemaState struct {
	db     *sql.DB
	tables []string
}

func newSchemaState() *schemaState {
	return &schemaState{}
}

func (s *schemaState) theDatabaseSchemaIsAvailable() error {
	dsn := os.Getenv("CLEAN_CODE_PG_URL")
	if dsn == "" {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return fmt.Errorf("pinging postgres: %w", err)
	}
	s.db = db
	return nil
}

func (s *schemaState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

func (s *schemaState) listingTablesInSchema(schema string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = $1
		  AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`, schema)
	if err != nil {
		return fmt.Errorf("querying information_schema.tables: %w", err)
	}
	defer rows.Close()

	s.tables = nil
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning table name: %w", err)
		}
		s.tables = append(s.tables, name)
	}
	return rows.Err()
}

func (s *schemaState) noTablesNamedOrExist(name1, name2 string) error {
	for _, tbl := range s.tables {
		if tbl == name1 || tbl == name2 {
			return fmt.Errorf(
				"prohibited table %q exists in the schema; found tables: %s",
				tbl, strings.Join(s.tables, ", "),
			)
		}
	}
	return nil
}

func (s *schemaState) tablesCarryAuditSemantics(tableCSV string) error {
	required := strings.Split(tableCSV, `", "`)
	for i := range required {
		required[i] = strings.Trim(required[i], `"`)
	}

	for _, req := range required {
		found := false
		for _, tbl := range s.tables {
			if tbl == req {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf(
				"expected table %q to exist but it was not found; tables: %s",
				req, strings.Join(s.tables, ", "),
			)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_audit_wal_and_reliability_hardening_audit_wal_frame_writer
// registers all Given/When/Then steps for the audit-wal-frame-writer stage.
func InitializeScenario_audit_wal_and_reliability_hardening_audit_wal_frame_writer(ctx *godog.ScenarioContext) {
	walScope := newWalScopeState()
	schema := newSchemaState()

	ctx.After(func(actx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		schema.close()
		return actx, nil
	})

	// Scenario 1: wal-scope-only-audit-tables
	ctx.Given(`^any code path in the service$`, walScope.anyCodePathInTheService)
	ctx.When(`^grepping the writer call sites$`, walScope.greppingTheWriterCallSites)
	ctx.Then(
		`^"internal/audit/wal" is referenced only from "([^"]*)" and "([^"]*)"$`,
		func(allowed1, allowed2 string) error {
			return walScope.walReferencedOnlyFrom(
				fmt.Sprintf(`"%s" and "%s"`, allowed1, allowed2),
			)
		},
	)

	// Scenario 2: no-projection-table
	ctx.Given(`^the database schema is available$`, schema.theDatabaseSchemaIsAvailable)
	ctx.When(`^listing tables in the "([^"]*)" schema$`, schema.listingTablesInSchema)
	ctx.Then(
		`^no tables named "([^"]*)" or "([^"]*)" exist$`,
		schema.noTablesNamedOrExist,
	)
	ctx.Then(
		`^tables "([^"]*)", "([^"]*)", "([^"]*)" carry audit semantics$`,
		func(t1, t2, t3 string) error {
			return schema.tablesCarryAuditSemantics(
				fmt.Sprintf(`"%s", "%s", "%s"`, t1, t2, t3),
			)
		},
	)
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_audit_wal_and_reliability_hardening_audit_wal_frame_writer(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_audit_wal_and_reliability_hardening_audit_wal_frame_writer,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"audit_wal_and_reliability_hardening_audit_wal_frame_writer.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}