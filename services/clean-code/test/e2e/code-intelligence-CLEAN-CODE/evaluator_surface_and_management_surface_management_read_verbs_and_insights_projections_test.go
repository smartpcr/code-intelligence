//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// requireEnv returns the value of the named environment variable or skips the
// test when the variable is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("required env var %s is not set; skipping E2E test", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Per-scenario state
// ---------------------------------------------------------------------------

type mgmtReadState struct {
	pgURL   string
	mgmtURL string

	db *sql.DB

	// sha-pinned scenario
	repoID     string
	commitSHA  string
	filePath   string
	metricName string
	scope      string
	activeSampleID   string
	retractedSampleID string

	// Response data for metric_sample read
	metricSampleResponse []map[string]interface{}

	// cross_repo scenario
	expectedP50          float64
	expectedP90          float64
	expectedP99          float64
	expectedHistogramJSON string
	crossRepoResponse    map[string]interface{}
}


// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *mgmtReadState) generateUniqueID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func (s *mgmtReadState) httpGet(url string) ([]byte, int, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s failed: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response body: %w", err)
	}
	return body, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// Scenario: sha-pinned-returns-active-row
// ---------------------------------------------------------------------------

func (s *mgmtReadState) twoMetricSampleRowsExistWithOlderRetracted() error {
	s.repoID = s.generateUniqueID("repo")
	s.commitSHA = s.generateUniqueID("sha")
	s.filePath = "src/main.go"
	s.metricName = "cyclomatic_complexity"
	s.scope = "function:main"

	// Insert the older (retracted) row
	s.retractedSampleID = s.generateUniqueID("sample-retracted")
	_, err := s.db.Exec(`
		INSERT INTO metric_sample (id, repo_id, commit_sha, file_path, metric_name, scope, value, retracted, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 12.5, true, NOW() - INTERVAL '1 hour')`,
		s.retractedSampleID, s.repoID, s.commitSHA, s.filePath, s.metricName, s.scope)
	if err != nil {
		return fmt.Errorf("inserting retracted metric_sample: %w", err)
	}

	// Insert the newer (active) row
	s.activeSampleID = s.generateUniqueID("sample-active")
	_, err = s.db.Exec(`
		INSERT INTO metric_sample (id, repo_id, commit_sha, file_path, metric_name, scope, value, retracted, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 8.0, false, NOW())`,
		s.activeSampleID, s.repoID, s.commitSHA, s.filePath, s.metricName, s.scope)
	if err != nil {
		return fmt.Errorf("inserting active metric_sample: %w", err)
	}

	return nil
}

func (s *mgmtReadState) mgmtReadMetricSampleEndpointIsCalled() error {
	url := fmt.Sprintf("%s/api/v1/metric-samples?repo_id=%s&commit_sha=%s&file_path=%s&metric_name=%s&scope=%s",
		s.mgmtURL, s.repoID, s.commitSHA, s.filePath, s.metricName, s.scope)

	body, statusCode, err := s.httpGet(url)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("expected 200, got %d: %s", statusCode, string(body))
	}

	if err := json.Unmarshal(body, &s.metricSampleResponse); err != nil {
		return fmt.Errorf("unmarshalling metric_sample response: %w", err)
	}
	return nil
}

func (s *mgmtReadState) exactlyOneRowIsReturned() error {
	if len(s.metricSampleResponse) != 1 {
		return fmt.Errorf("expected exactly 1 row, got %d", len(s.metricSampleResponse))
	}
	return nil
}

func (s *mgmtReadState) theReturnedRowIsActiveNonRetracted() error {
	if len(s.metricSampleResponse) == 0 {
		return fmt.Errorf("no rows in response")
	}
	row := s.metricSampleResponse[0]

	// Check it's the active sample
	id, _ := row["id"].(string)
	if id != s.activeSampleID {
		return fmt.Errorf("expected active sample id %s, got %s", s.activeSampleID, id)
	}

	// Check retracted is false
	retracted, ok := row["retracted"].(bool)
	if ok && retracted {
		return fmt.Errorf("expected retracted=false, got true")
	}
	return nil
}

func (s *mgmtReadState) theRetractedRowIsNotPresent() error {
	for _, row := range s.metricSampleResponse {
		id, _ := row["id"].(string)
		if id == s.retractedSampleID {
			return fmt.Errorf("retracted sample %s should not be in response", s.retractedSampleID)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: latest-dashboard-returns-snapshot
// ---------------------------------------------------------------------------

func (s *mgmtReadState) populatedCrossRepoPercentileRowExists() error {
	s.expectedP50 = 6.5
	s.expectedP90 = 14.2
	s.expectedP99 = 28.7
	s.expectedHistogramJSON = `{"buckets":[0,5,10,15,20,25,30],"counts":[12,45,30,8,3,1,1]}`

	_, err := s.db.Exec(`
		INSERT INTO cross_repo_percentile (id, metric_name, window_start, window_end, p50, p90, p99, histogram_json, created_at)
		VALUES ($1, $2, NOW() - INTERVAL '7 days', NOW(), $3, $4, $5, $6, NOW())
		ON CONFLICT (metric_name, window_start, window_end)
		DO UPDATE SET p50 = $3, p90 = $4, p99 = $5, histogram_json = $6, created_at = NOW()`,
		s.generateUniqueID("crp"), "cyclomatic_complexity",
		s.expectedP50, s.expectedP90, s.expectedP99, s.expectedHistogramJSON)
	if err != nil {
		return fmt.Errorf("inserting cross_repo_percentile: %w", err)
	}

	return nil
}

func (s *mgmtReadState) mgmtReadCrossRepoEndpointIsCalled() error {
	url := fmt.Sprintf("%s/api/v1/cross-repo-percentiles", s.mgmtURL)

	body, statusCode, err := s.httpGet(url)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("expected 200, got %d: %s", statusCode, string(body))
	}

	// Response could be a single object or an array; try array first
	var arr []map[string]interface{}
	if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
		s.crossRepoResponse = arr[len(arr)-1] // latest entry
		return nil
	}

	// Try single object
	var obj map[string]interface{}
	if err := json.Unmarshal(body, &obj); err == nil {
		s.crossRepoResponse = obj
		return nil
	}

	return fmt.Errorf("could not parse cross_repo response: %s", string(body))
}

func (s *mgmtReadState) responseContainsP50() error {
	v, ok := s.crossRepoResponse["p50"]
	if !ok {
		return fmt.Errorf("response missing p50 field")
	}
	p50, ok := v.(float64)
	if !ok {
		return fmt.Errorf("p50 is not a number: %T", v)
	}
	if p50 != s.expectedP50 {
		return fmt.Errorf("expected p50=%f, got %f", s.expectedP50, p50)
	}
	return nil
}

func (s *mgmtReadState) responseContainsP90() error {
	v, ok := s.crossRepoResponse["p90"]
	if !ok {
		return fmt.Errorf("response missing p90 field")
	}
	p90, ok := v.(float64)
	if !ok {
		return fmt.Errorf("p90 is not a number: %T", v)
	}
	if p90 != s.expectedP90 {
		return fmt.Errorf("expected p90=%f, got %f", s.expectedP90, p90)
	}
	return nil
}

func (s *mgmtReadState) responseContainsP99() error {
	v, ok := s.crossRepoResponse["p99"]
	if !ok {
		return fmt.Errorf("response missing p99 field")
	}
	p99, ok := v.(float64)
	if !ok {
		return fmt.Errorf("p99 is not a number: %T", v)
	}
	if p99 != s.expectedP99 {
		return fmt.Errorf("expected p99=%f, got %f", s.expectedP99, p99)
	}
	return nil
}

func (s *mgmtReadState) responseContainsHistogramJSON() error {
	v, ok := s.crossRepoResponse["histogram_json"]
	if !ok {
		return fmt.Errorf("response missing histogram_json field")
	}

	// histogram_json may come back as a string or a nested object
	var histStr string
	switch hv := v.(type) {
	case string:
		histStr = hv
	default:
		b, err := json.Marshal(hv)
		if err != nil {
			return fmt.Errorf("marshalling histogram_json: %w", err)
		}
		histStr = string(b)
	}

	// Compare as parsed JSON to ignore whitespace differences
	var expected, actual interface{}
	if err := json.Unmarshal([]byte(s.expectedHistogramJSON), &expected); err != nil {
		return fmt.Errorf("parsing expected histogram: %w", err)
	}
	if err := json.Unmarshal([]byte(histStr), &actual); err != nil {
		return fmt.Errorf("parsing actual histogram: %w", err)
	}

	expectedBytes, _ := json.Marshal(expected)
	actualBytes, _ := json.Marshal(actual)
	if string(expectedBytes) != string(actualBytes) {
		return fmt.Errorf("histogram_json mismatch:\n  expected: %s\n  actual:   %s", string(expectedBytes), string(actualBytes))
	}
	return nil
}

func (s *mgmtReadState) responseValuesMatchSeededMaterialisedRow() error {
	// Verify every returned percentile column matches the exact seeded value.
	// This confirms the endpoint returned the materialised row as-is.
	checks := map[string]float64{
		"p50": s.expectedP50,
		"p90": s.expectedP90,
		"p99": s.expectedP99,
	}
	for col, expected := range checks {
		v, ok := s.crossRepoResponse[col]
		if !ok {
			return fmt.Errorf("response missing %s field", col)
		}
		actual, ok := v.(float64)
		if !ok {
			return fmt.Errorf("%s is not a number: %T", col, v)
		}
		if actual != expected {
			return fmt.Errorf("%s mismatch: expected %f, got %f", col, expected, actual)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// State factory
// ---------------------------------------------------------------------------

func newMgmtReadStateFromEnv() *mgmtReadState {
	pgURL := os.Getenv("CLEAN_CODE_PG_URL")
	mgmtURL := os.Getenv("CLEAN_CODE_MGMT_URL")

	s := &mgmtReadState{
		pgURL:   pgURL,
		mgmtURL: mgmtURL,
	}

	if pgURL != "" {
		db, err := sql.Open("postgres", pgURL)
		if err != nil {
			panic(fmt.Sprintf("sql.Open failed for %s: %v", pgURL, err))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pingErr := db.PingContext(ctx); pingErr != nil {
			panic(fmt.Sprintf("cannot reach PG at %s: %v", pgURL, pingErr))
		}
		db.SetMaxOpenConns(5)
		db.SetConnMaxLifetime(2 * time.Minute)
		s.db = db
	}

	return s
}

// ---------------------------------------------------------------------------
// Scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_evaluator_surface_and_management_surface_management_read_verbs_and_insights_projections(ctx *godog.ScenarioContext) {
	s := newMgmtReadStateFromEnv()

	// Scenario: sha-pinned-returns-active-row
	ctx.Step(`^two metric_sample rows exist for the same repo, commit SHA, file path, metric name, and scope with the older one retracted$`, s.twoMetricSampleRowsExistWithOlderRetracted)
	ctx.Step(`^the mgmt\.read\.metric_sample endpoint is called for that quintuple$`, s.mgmtReadMetricSampleEndpointIsCalled)
	ctx.Step(`^exactly one row is returned$`, s.exactlyOneRowIsReturned)
	ctx.Step(`^the returned row is the active non-retracted sample$`, s.theReturnedRowIsActiveNonRetracted)
	ctx.Step(`^the retracted row is not present in the response$`, s.theRetractedRowIsNotPresent)

	// Scenario: latest-dashboard-returns-snapshot
	ctx.Step(`^a populated cross_repo_percentile row exists with p50, p90, p99, and histogram_json columns$`, s.populatedCrossRepoPercentileRowExists)
	ctx.Step(`^the mgmt\.read\.cross_repo endpoint is called$`, s.mgmtReadCrossRepoEndpointIsCalled)
	ctx.Step(`^the response contains the p50 value from the materialised row$`, s.responseContainsP50)
	ctx.Step(`^the response contains the p90 value from the materialised row$`, s.responseContainsP90)
	ctx.Step(`^the response contains the p99 value from the materialised row$`, s.responseContainsP99)
	ctx.Step(`^the response contains the histogram_json from the materialised row$`, s.responseContainsHistogramJSON)
	ctx.Step(`^the response values match the seeded materialised row$`, s.responseValuesMatchSeededMaterialisedRow)
}

// ---------------------------------------------------------------------------
// Test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_evaluator_surface_and_management_surface_management_read_verbs_and_insights_projections(t *testing.T) {
	// Ensure required env vars are present; skip gracefully if not.
	requireEnv(t, "CLEAN_CODE_PG_URL")
	requireEnv(t, "CLEAN_CODE_MGMT_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_evaluator_surface_and_management_surface_management_read_verbs_and_insights_projections,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"evaluator_surface_and_management_surface_management_read_verbs_and_insights_projections.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run E2E tests")
	}
}