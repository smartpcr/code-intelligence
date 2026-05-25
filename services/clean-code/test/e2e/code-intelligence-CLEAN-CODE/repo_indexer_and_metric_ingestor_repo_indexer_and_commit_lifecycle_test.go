//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"

	repo_indexer "github.com/microsoft/code-intelligence/services/clean-code/internal/repo_indexer"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// requireEnv returns the value of the named environment variable or skips the
// test when the variable is unset / empty.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping e2e test", name)
	}
	return v
}

// computeHMACSHA256 returns the hex-encoded HMAC-SHA256 of body using secret.
func computeHMACSHA256(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// state for the commit-states-only-canonical scenario
// ---------------------------------------------------------------------------

type scanStatusEnumState struct {
	values   []string
	typeName string
}

func (s *scanStatusEnumState) theScanStatusEnumAtCompileTime() error {
	// Obtain the type name via reflection on the real Go enum, exactly as
	// the acceptance scenario specifies: reflect.TypeOf(ScanStatus(0)).
	s.typeName = reflect.TypeOf(repo_indexer.ScanStatus(0)).String()

	// Populate the value set from the compiled AllScanStatuses() function
	// so we are testing the actual source-of-truth, not a hard-coded list.
	all := repo_indexer.AllScanStatuses()
	s.values = make([]string, len(all))
	for i, st := range all {
		s.values[i] = st.String()
	}
	return nil
}

func (s *scanStatusEnumState) weEnumerateItsValuesViaAllScanStatuses() error {
	// Verify the reflected type is the expected Go type.
	if s.typeName != "repo_indexer.ScanStatus" {
		return fmt.Errorf("expected reflected type repo_indexer.ScanStatus, got %q", s.typeName)
	}
	// When CLEAN_CODE_PG_URL is set, also cross-check the PostgreSQL enum
	// to ensure the DB migration and Go enum stay in sync.
	dsn := os.Getenv("CLEAN_CODE_PG_URL")
	if dsn == "" {
		return nil
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(context.Background(), `
		SELECT unnest(enum_range(NULL::clean_code.scan_status))::text
	`)
	if err != nil {
		return fmt.Errorf("querying scan_status enum: %w", err)
	}
	defer rows.Close()

	var dbValues []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return fmt.Errorf("scanning enum value: %w", err)
		}
		dbValues = append(dbValues, v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating enum rows: %w", err)
	}

	// Verify the DB enum matches the Go enum exactly.
	if len(dbValues) != len(s.values) {
		return fmt.Errorf("Go enum has %d values %v but PostgreSQL enum has %d values %v",
			len(s.values), s.values, len(dbValues), dbValues)
	}
	goSet := make(map[string]bool, len(s.values))
	for _, v := range s.values {
		goSet[v] = true
	}
	for _, dbv := range dbValues {
		if !goSet[dbv] {
			return fmt.Errorf("PostgreSQL enum value %q not present in Go AllScanStatuses(): %v", dbv, s.values)
		}
	}
	return nil
}

func (s *scanStatusEnumState) exactlyArePresentCSV(expected string) error {
	expectedParts := strings.Split(expected, ", ")
	if len(s.values) != len(expectedParts) {
		return fmt.Errorf("expected %d enum values %v, got %d: %v",
			len(expectedParts), expectedParts, len(s.values), s.values)
	}
	valSet := make(map[string]bool, len(s.values))
	for _, v := range s.values {
		valSet[v] = true
	}
	for _, e := range expectedParts {
		if !valSet[e] {
			return fmt.Errorf("expected enum value %q not found in %v", e, s.values)
		}
	}
	return nil
}

func (s *scanStatusEnumState) noValueExistsInTheEnum(forbidden string) error {
	for _, v := range s.values {
		if v == forbidden {
			return fmt.Errorf("forbidden value %q found in ScanStatus enum: %v", forbidden, s.values)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// state for the new-sha-inserts-pending scenario
// ---------------------------------------------------------------------------

type newSHAState struct {
	db         *sql.DB
	indexerURL string
	hmacSecret string
	sha        string
	repoID     string
}

func newNewSHAState() *newSHAState {
	return &newSHAState{
		repoID: "00000000-0000-0000-0000-000000000001",
	}
}

func (s *newSHAState) aRunningRepoIndexerConnectedToPostgreSQL() error {
	dsn := os.Getenv("CLEAN_CODE_PG_URL")
	if dsn == "" {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return fmt.Errorf("pinging postgres: %w", err)
	}
	s.db = db

	s.indexerURL = os.Getenv("CLEAN_CODE_INDEXER_URL")
	if s.indexerURL == "" {
		s.indexerURL = "http://localhost:8082"
	}
	s.hmacSecret = os.Getenv("CLEAN_CODE_INDEXER_HMAC_SECRET")
	return nil
}

func (s *newSHAState) theDatabaseIsMigratedAndSeeded() error {
	// Ensure the repo fixture row exists (mirrors seed-fixtures-phase-03).
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ($1, 'e2e-test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`, s.repoID)
	if err != nil {
		return fmt.Errorf("ensuring repo fixture: %w", err)
	}
	return nil
}

func (s *newSHAState) aWebhookPayloadForANewSHAIsProcessed(sha string) error {
	s.sha = sha

	// Clean up any previous test data for this SHA.
	_, _ = s.db.ExecContext(context.Background(),
		`DELETE FROM clean_code.repo_event WHERE commit_sha = $1`, sha)
	_, _ = s.db.ExecContext(context.Background(),
		`DELETE FROM clean_code.commit WHERE sha = $1`, sha)

	// Build webhook payload matching the indexer's expected format.
	payload := map[string]interface{}{
		"repo_id": s.repoID,
		"sha":     sha,
		"ref":     "refs/heads/main",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling webhook payload: %w", err)
	}

	webhookURL := strings.TrimRight(s.indexerURL, "/") + "/v1/indexer/webhook"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// If an HMAC secret is configured, sign the request.
	if s.hmacSecret != "" {
		sig := computeHMACSHA256([]byte(s.hmacSecret), body)
		req.Header.Set("X-Hub-Signature-256", "sha256="+sig)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *newSHAState) aCommitRowAppearsWithScanStatus(expected string) error {
	// Poll briefly — the indexer may process asynchronously.
	var actual string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT scan_status::text FROM clean_code.commit
			WHERE sha = $1
		`, s.sha).Scan(&actual)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for commit row with sha=%s: %w", s.sha, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if actual != expected {
		return fmt.Errorf("expected scan_status=%q, got %q", expected, actual)
	}
	return nil
}

func (s *newSHAState) aSingleRepoEventWithKindIsAppended(expectedKind string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var count int
	var kind string
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*), MIN(kind::text) FROM clean_code.repo_event
			WHERE commit_sha = $1
		`, s.sha).Scan(&count, &kind)
		if err == nil && count > 0 {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for repo_event for sha=%s", s.sha)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if count != 1 {
		return fmt.Errorf("expected exactly 1 repo_event for sha=%s, got %d", s.sha, count)
	}
	if kind != expectedKind {
		return fmt.Errorf("expected repo_event kind=%q, got %q", expectedKind, kind)
	}
	return nil
}

func (s *newSHAState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_repo_indexer_and_metric_ingestor_repo_indexer_and_commit_lifecycle
// registers all Given/When/Then steps for the repo-indexer-and-commit-lifecycle stage.
func InitializeScenario_repo_indexer_and_metric_ingestor_repo_indexer_and_commit_lifecycle(ctx *godog.ScenarioContext) {
	// --- Scenario: commit-states-only-canonical ---
	enumState := &scanStatusEnumState{}

	ctx.Step(`^the ScanStatus enum at compile time$`, enumState.theScanStatusEnumAtCompileTime)
	ctx.Step(`^we enumerate its values via AllScanStatuses$`, enumState.weEnumerateItsValuesViaAllScanStatuses)
	ctx.Step(`^exactly "([^"]*)" are present$`, enumState.exactlyArePresentCSV)
	ctx.Step(`^no value "([^"]*)" exists in the enum$`, enumState.noValueExistsInTheEnum)

	// --- Scenario: new-sha-inserts-pending ---
	// shaState is only used by the new-sha-inserts-pending scenario. The
	// commit-states-only-canonical scenario uses enumState above and does
	// not touch the indexer DB connection, so we only allocate shaState
	// for the scenario that actually needs it. The @setup-compose tag is
	// declared at the Feature level and applies to all scenarios, so it
	// cannot distinguish between them; we gate on scenario name instead.
	var shaState *newSHAState

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		if sc.Name == "new-sha-inserts-pending" {
			shaState = newNewSHAState()
		} else {
			shaState = nil
		}
		return bctx, nil
	})

	ctx.After(func(actx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if shaState != nil {
			shaState.close()
		}
		return actx, nil
	})

	ctx.Step(`^a running Repo Indexer connected to PostgreSQL$`, func() error {
		return shaState.aRunningRepoIndexerConnectedToPostgreSQL()
	})
	ctx.Step(`^the database is migrated and seeded$`, func() error {
		return shaState.theDatabaseIsMigratedAndSeeded()
	})
	ctx.Step(`^a webhook payload for a new SHA "([^"]*)" is processed$`, func(sha string) error {
		return shaState.aWebhookPayloadForANewSHAIsProcessed(sha)
	})
	ctx.Step(`^a commit row appears with scan_status "([^"]*)"$`, func(status string) error {
		return shaState.aCommitRowAppearsWithScanStatus(status)
	})
	ctx.Step(`^a single repo_event with kind "([^"]*)" is appended for that commit$`, func(kind string) error {
		return shaState.aSingleRepoEventWithKindIsAppended(kind)
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_repo_indexer_and_metric_ingestor_repo_indexer_and_commit_lifecycle(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_repo_indexer_and_metric_ingestor_repo_indexer_and_commit_lifecycle,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"repo_indexer_and_metric_ingestor_repo_indexer_and_commit_lifecycle.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}
