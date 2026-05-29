//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Scenario 1: hotspot-score-formula
// ---------------------------------------------------------------------------

type hotspotScoreState struct {
	zScores      []float64
	findingCount int
	weights      []float64
	computed     float64

	// compose-backed fields
	plannerURL string
	db         *sql.DB
}

func newHotspotScoreState() *hotspotScoreState {
	return &hotspotScoreState{}
}

func (s *hotspotScoreState) initEnv() error {
	s.plannerURL = os.Getenv("CLEAN_CODE_PLANNER_URL")
	if s.plannerURL == "" {
		s.plannerURL = "http://localhost:8085"
	}
	dsn := os.Getenv("CLEAN_CODE_PG_URL")
	if dsn != "" {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return fmt.Errorf("opening postgres: %w", err)
		}
		if err := db.PingContext(context.Background()); err != nil {
			db.Close()
			return fmt.Errorf("pinging postgres: %w", err)
		}
		s.db = db
	}
	return nil
}

func (s *hotspotScoreState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// parseFloatCSV splits a comma-separated string into a float64 slice.
func parseFloatCSV(csv string) ([]float64, error) {
	parts := strings.Split(csv, ",")
	result := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, fmt.Errorf("parsing %q: %w", p, err)
		}
		result = append(result, v)
	}
	return result, nil
}

func (s *hotspotScoreState) knownZScoresAndFindingCountWithWeights(
	zCSV string, findingCount int, wCSV string,
) error {
	var err error
	s.zScores, err = parseFloatCSV(zCSV)
	if err != nil {
		return fmt.Errorf("parsing z-scores: %w", err)
	}
	s.findingCount = findingCount
	s.weights, err = parseFloatCSV(wCSV)
	if err != nil {
		return fmt.Errorf("parsing weights: %w", err)
	}
	// weights length must be len(z-scores) + 1 (the extra weight is for finding_count).
	if len(s.weights) != len(s.zScores)+1 {
		return fmt.Errorf("expected %d weights (z-scores + finding_count), got %d",
			len(s.zScores)+1, len(s.weights))
	}
	return nil
}

// hotspotScoreRequest is the JSON body sent to the planner scoring endpoint.
type hotspotScoreRequest struct {
	ZScores      []float64 `json:"z_scores"`
	FindingCount int       `json:"finding_count"`
	Weights      []float64 `json:"weights"`
}

// hotspotScoreResponse is the JSON body returned by the planner scoring endpoint.
type hotspotScoreResponse struct {
	Score float64 `json:"score"`
}

func (s *hotspotScoreState) scoreIsComputed() error {
	reqBody := hotspotScoreRequest{
		ZScores:      s.zScores,
		FindingCount: s.findingCount,
		Weights:      s.weights,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshalling score request: %w", err)
	}

	url := strings.TrimRight(s.plannerURL, "/") + "/v1/planner/hotspot/score"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating score request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("calling planner score endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("planner score endpoint returned HTTP %d", resp.StatusCode)
	}

	var result hotspotScoreResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding score response: %w", err)
	}
	s.computed = result.Score
	return nil
}

func (s *hotspotScoreState) itEquals(expected float64) error {
	// Round to 2 decimal places for comparison.
	rounded := math.Round(s.computed*100) / 100
	if rounded != expected {
		return fmt.Errorf("expected score %.2f, got %.2f (raw %.6f)", expected, rounded, s.computed)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2: hotspot-pins-policy-version
// ---------------------------------------------------------------------------

type hotspotPolicyState struct {
	plannerURL      string
	db              *sql.DB
	policyVersionID string
	hotSpotID       string
}

func newHotspotPolicyState(plannerURL string, db *sql.DB) *hotspotPolicyState {
	return &hotspotPolicyState{
		plannerURL: plannerURL,
		db:         db,
	}
}

func (s *hotspotPolicyState) theActivePolicyVersionID(pvID string) error {
	s.policyVersionID = pvID
	return nil
}

// hotspotWriteRequest is the JSON body sent to trigger a hot_spot write.
type hotspotWriteRequest struct {
	RepoID          string    `json:"repo_id"`
	SHA             string    `json:"sha"`
	FilePath        string    `json:"file_path"`
	PolicyVersionID string    `json:"policy_version_id"`
	ZScores         []float64 `json:"z_scores"`
	FindingCount    int       `json:"finding_count"`
	Weights         []float64 `json:"weights"`
}

// hotspotWriteResponse is the JSON body returned after writing a hot_spot row.
type hotspotWriteResponse struct {
	HotSpotID string `json:"hot_spot_id"`
}

func (s *hotspotPolicyState) aHotSpotRowIsWritten() error {
	reqBody := hotspotWriteRequest{
		RepoID:          "00000000-0000-0000-0000-000000000001",
		SHA:             "e2edeadbeef1234",
		FilePath:        "src/e2e_test_file.go",
		PolicyVersionID: s.policyVersionID,
		ZScores:         []float64{1.0, 1.0, 1.0},
		FindingCount:    1,
		Weights:         []float64{1, 1, 1, 1},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshalling hotspot write request: %w", err)
	}

	url := strings.TrimRight(s.plannerURL, "/") + "/v1/planner/hotspot"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating hotspot write request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("calling planner hotspot endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("planner hotspot endpoint returned HTTP %d", resp.StatusCode)
	}

	var result hotspotWriteResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding hotspot write response: %w", err)
	}
	s.hotSpotID = result.HotSpotID
	return nil
}

func (s *hotspotPolicyState) hotSpotPolicyVersionIDIs(expected string) error {
	if s.db == nil {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set; cannot verify hot_spot row")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var actual string
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT policy_version_id FROM clean_code.hot_spot
			WHERE hot_spot_id = $1
		`, s.hotSpotID).Scan(&actual)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for hot_spot row with id=%s: %w", s.hotSpotID, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if actual != expected {
		return fmt.Errorf("expected policy_version_id=%q, got %q", expected, actual)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_refactor_planner_composite_hotspot_scoring registers all
// Given/When/Then steps for the composite-hotspot-scoring stage's scenarios.
func InitializeScenario_refactor_planner_composite_hotspot_scoring(ctx *godog.ScenarioContext) {
	scoreState := newHotspotScoreState()

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		if err := scoreState.initEnv(); err != nil {
			return bctx, err
		}
		return bctx, nil
	})

	ctx.After(func(actx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		scoreState.close()
		return actx, nil
	})

	// Scenario 1: hotspot-score-formula
	ctx.Given(
		`^known z-scores "([^"]*)" and finding_count (\d+) with weights "([^"]*)"$`,
		func(zCSV string, findingCount int, wCSV string) error {
			return scoreState.knownZScoresAndFindingCountWithWeights(zCSV, findingCount, wCSV)
		},
	)
	ctx.When(`^score is computed$`, func() error {
		return scoreState.scoreIsComputed()
	})
	ctx.Then(`^it equals (\d+\.\d+)$`, func(expected float64) error {
		return scoreState.itEquals(expected)
	})

	// Scenario 2: hotspot-pins-policy-version
	var policyState *hotspotPolicyState

	ctx.Given(`^the active policy_version_id "([^"]*)"$`, func(pvID string) error {
		policyState = newHotspotPolicyState(scoreState.plannerURL, scoreState.db)
		return policyState.theActivePolicyVersionID(pvID)
	})
	ctx.When(`^a hot_spot row is written$`, func() error {
		return policyState.aHotSpotRowIsWritten()
	})
	ctx.Then(`^hot_spot\.policy_version_id is "([^"]*)"$`, func(expected string) error {
		return policyState.hotSpotPolicyVersionIDIs(expected)
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_refactor_planner_composite_hotspot_scoring(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_refactor_planner_composite_hotspot_scoring,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"refactor_planner_composite_hotspot_scoring.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}