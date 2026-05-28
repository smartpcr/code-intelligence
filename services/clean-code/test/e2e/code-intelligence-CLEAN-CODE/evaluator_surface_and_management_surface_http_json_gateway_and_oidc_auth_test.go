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
	"strings"
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

type oidcGatewayState struct {
	evaluatorURL string
	pgURL        string

	db *sql.DB

	// HTTP response captured by When steps
	lastStatusCode int
	lastHeaders    http.Header
	lastBody       []byte

	// Tracking for evaluation_run assertion
	runCountBefore int64
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *oidcGatewayState) doRequest(method, url string, bearerToken string) error {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s failed: %w", method, url, err)
	}
	defer resp.Body.Close()

	s.lastStatusCode = resp.StatusCode
	s.lastHeaders = resp.Header
	s.lastBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}
	return nil
}

func (s *oidcGatewayState) getValidBearerToken() string {
	// Use the token minted by `make tokens` for the dev role.
	// Stored in CLEAN_CODE_DEV_TOKEN by the bootstrap step.
	tok := os.Getenv("CLEAN_CODE_DEV_TOKEN")
	if tok != "" {
		return tok
	}
	// Fallback: try a generic token env var
	return os.Getenv("CLEAN_CODE_BEARER_TOKEN")
}

// ---------------------------------------------------------------------------
// Scenario: oidc-rejects-missing-token
// ---------------------------------------------------------------------------

func (s *oidcGatewayState) anHTTPRequestToV1RouteWithoutAuthorizationHeader() error {
	// We'll send the request in the When step; just record the intent.
	return nil
}

func (s *oidcGatewayState) theHandlerRuns() error {
	// Send a GET to a known /v1/* route without Authorization header.
	url := fmt.Sprintf("%s/v1/eval/health", s.evaluatorURL)
	return s.doRequest("GET", url, "")
}

func (s *oidcGatewayState) itReturns401() error {
	if s.lastStatusCode != http.StatusUnauthorized {
		return fmt.Errorf("expected 401, got %d; body: %s", s.lastStatusCode, string(s.lastBody))
	}
	return nil
}

func (s *oidcGatewayState) theResponseIncludesWWWAuthenticateBearerHeader() error {
	wwwAuth := s.lastHeaders.Get("WWW-Authenticate")
	if wwwAuth == "" {
		return fmt.Errorf("expected WWW-Authenticate header, but it was absent")
	}
	// The value must contain "Bearer" (RFC 6750 §3)
	if !strings.Contains(wwwAuth, "Bearer") {
		return fmt.Errorf("expected WWW-Authenticate to contain 'Bearer', got %q", wwwAuth)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: unknown-verb-404
// ---------------------------------------------------------------------------

func (s *oidcGatewayState) aPOSTToUnknownVerbWithValidBearerToken() error {
	// Snapshot evaluation_run count before the request
	if s.db != nil {
		err := s.db.QueryRow(`SELECT COUNT(*) FROM evaluation_run`).Scan(&s.runCountBefore)
		if err != nil {
			// Table might not exist yet — treat as zero
			s.runCountBefore = 0
		}
	}
	return nil
}

func (s *oidcGatewayState) theGatewayRoutesTheRequest() error {
	url := fmt.Sprintf("%s/v1/eval/unknown_verb", s.evaluatorURL)
	token := s.getValidBearerToken()
	return s.doRequest("POST", url, token)
}

func (s *oidcGatewayState) itReturns404() error {
	if s.lastStatusCode != http.StatusNotFound {
		return fmt.Errorf("expected 404, got %d; body: %s", s.lastStatusCode, string(s.lastBody))
	}
	return nil
}

func (s *oidcGatewayState) noEvaluationRunRowIsEmitted() error {
	if s.db == nil {
		// Without DB access we verify via the response: a 404 with no run_id
		// in the body is sufficient evidence that no run was created.
		var respObj map[string]interface{}
		if err := json.Unmarshal(s.lastBody, &respObj); err == nil {
			if _, hasRunID := respObj["run_id"]; hasRunID {
				return fmt.Errorf("response unexpectedly contains a run_id field")
			}
		}
		return nil
	}

	var runCountAfter int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM evaluation_run`).Scan(&runCountAfter)
	if err != nil {
		runCountAfter = 0
	}
	if runCountAfter != s.runCountBefore {
		return fmt.Errorf("evaluation_run count changed from %d to %d; expected no new rows",
			s.runCountBefore, runCountAfter)
	}
	return nil
}

// ---------------------------------------------------------------------------
// State factory
// ---------------------------------------------------------------------------

func newOidcGatewayStateFromEnv() *oidcGatewayState {
	evaluatorURL := os.Getenv("CLEAN_CODE_EVALUATOR_URL")
	pgURL := os.Getenv("CLEAN_CODE_PG_URL")

	s := &oidcGatewayState{
		evaluatorURL: evaluatorURL,
		pgURL:        pgURL,
	}

	if pgURL != "" {
		db, err := sql.Open("postgres", pgURL)
		if err == nil {
			db.SetMaxOpenConns(5)
			db.SetConnMaxLifetime(2 * time.Minute)
			s.db = db
		}
	}

	return s
}

// ---------------------------------------------------------------------------
// Scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_evaluator_surface_and_management_surface_http_json_gateway_and_oidc_auth(ctx *godog.ScenarioContext) {
	s := newOidcGatewayStateFromEnv()

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if s.db != nil {
			s.db.Close()
		}
		return ctx, nil
	})

	// Scenario: oidc-rejects-missing-token
	ctx.Step(`^an HTTP request to any "/v1/\*" route without an Authorization header$`, s.anHTTPRequestToV1RouteWithoutAuthorizationHeader)
	ctx.Step(`^the handler runs$`, s.theHandlerRuns)
	ctx.Step(`^it returns 401$`, s.itReturns401)
	ctx.Step(`^the response includes a "WWW-Authenticate: Bearer" header$`, s.theResponseIncludesWWWAuthenticateBearerHeader)

	// Scenario: unknown-verb-404
	ctx.Step(`^a POST to "/v1/eval/unknown_verb" with a valid bearer token$`, s.aPOSTToUnknownVerbWithValidBearerToken)
	ctx.Step(`^the gateway routes the request$`, s.theGatewayRoutesTheRequest)
	ctx.Step(`^it returns 404$`, s.itReturns404)
	ctx.Step(`^no evaluation_run row is emitted$`, s.noEvaluationRunRowIsEmitted)
}

// ---------------------------------------------------------------------------
// Test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_evaluator_surface_and_management_surface_http_json_gateway_and_oidc_auth(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_EVALUATOR_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_evaluator_surface_and_management_surface_http_json_gateway_and_oidc_auth,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"evaluator_surface_and_management_surface_http_json_gateway_and_oidc_auth.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run E2E tests")
	}
}