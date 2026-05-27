package loadtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/calibration"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/scenarios"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// fakeAgentClient is a deterministic in-process AgentService
// stand-in. Each method records the call count, takes a
// configurable per-call delay (so tests can simulate saturation
// for the dropped-tick scenario), and returns canned payloads.
type fakeAgentClient struct {
	recallCount    atomic.Int64
	observeCount   atomic.Int64
	expandCount    atomic.Int64
	summarizeCount atomic.Int64

	// per-call latency floor.
	delay time.Duration

	// labelledRecall: when non-nil, the fake returns this
	// response on every Recall call so the harness can
	// measure deterministic rank / concept-hit.
	labelledRecall *scenarios.RecallResponse

	// failEvery N: when > 0, every N-th call returns an
	// error so the test can drive the error-budget path.
	failEvery int64
}

func (f *fakeAgentClient) Recall(ctx context.Context, _ scenarios.RecallRequest) (scenarios.RecallResponse, error) {
	n := f.recallCount.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return scenarios.RecallResponse{}, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if f.failEvery > 0 && n%f.failEvery == 0 {
		return scenarios.RecallResponse{}, errors.New("fake: synthetic recall failure")
	}
	if f.labelledRecall != nil {
		return *f.labelledRecall, nil
	}
	return scenarios.RecallResponse{ContextID: fmt.Sprintf("ctx-%d", n)}, nil
}

func (f *fakeAgentClient) Observe(ctx context.Context, _ scenarios.ObserveRequest) (scenarios.ObserveResponse, error) {
	f.observeCount.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return scenarios.ObserveResponse{}, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	return scenarios.ObserveResponse{EpisodeID: "ep-fake"}, nil
}

func (f *fakeAgentClient) Expand(ctx context.Context, _ scenarios.ExpandRequest) (scenarios.ExpandResponse, error) {
	f.expandCount.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return scenarios.ExpandResponse{}, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	return scenarios.ExpandResponse{ContextID: "ctx-expand"}, nil
}

func (f *fakeAgentClient) Summarize(ctx context.Context, _ scenarios.SummarizeRequest) (scenarios.SummarizeResponse, error) {
	f.summarizeCount.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return scenarios.SummarizeResponse{}, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	return scenarios.SummarizeResponse{ContextID: "ctx-sum", TargetKind: "node"}, nil
}

type fakeMgmtClient struct {
	count atomic.Int64
	delay time.Duration
	// degradedEvery: when > 0, every N-th IngestSpans
	// returns Degraded=true with the supplied reason so
	// tests can drive the harness's degraded-responses
	// note emitter without touching the wire.
	degradedEvery  int64
	degradedReason string
}

func (f *fakeMgmtClient) IngestSpans(ctx context.Context, _ scenarios.IngestSpansRequest) (scenarios.IngestSpansResponse, error) {
	n := f.count.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return scenarios.IngestSpansResponse{}, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	resp := scenarios.IngestSpansResponse{Accepted: 50}
	if f.degradedEvery > 0 && n%f.degradedEvery == 0 {
		resp.Degraded = true
		resp.DegradedReason = f.degradedReason
	}
	return resp, nil
}

// shortProfile is a CI-friendly version of the §8.3 envelope.
// Sub-second duration, modest RPS — keeps tests under 2 s while
// still exercising every code path (per-verb tickers, semaphore,
// aggregation, report rendering).
func shortProfile() reliability.LoadProfile {
	return reliability.LoadProfile{
		Name:             "test",
		DefaultDuration:  300 * time.Millisecond,
		ErrorBudgetRatio: 0.05,
		Verbs: []reliability.VerbProfile{
			{Verb: reliability.VerbAgentRecall, MetricName: "test_recall", SustainedRPS: 40, SLO95Seconds: 1, SLO99Seconds: 2},
			{Verb: reliability.VerbAgentObserve, MetricName: "test_observe", SustainedRPS: 40, SLO95Seconds: 1, SLO99Seconds: 2},
			{Verb: reliability.VerbAgentExpand, MetricName: "test_expand", SustainedRPS: 20, SLO95Seconds: 1, SLO99Seconds: 2},
			{Verb: reliability.VerbAgentSummarize, MetricName: "test_summarize", SustainedRPS: 10, SLO95Seconds: 2, SLO99Seconds: 3},
			{Verb: reliability.VerbMgmtIngestSpans, MetricName: "test_ingest", SustainedRPS: 10, SLO95Seconds: 2, SLO99Seconds: 5},
		},
		LearningQuality: reliability.LearningQualityTargets{
			K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25,
		},
	}
}

func TestNewHarness_RejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	_, err := NewHarness(calibration.Config{}, nil)
	if err == nil {
		t.Fatal("expected error on empty config")
	}
}

func TestNewHarness_RejectsNoScenarios(t *testing.T) {
	t.Parallel()
	cfg := calibration.DefaultConfig()
	cfg.Profile = shortProfile()
	cfg.ArtifactPath = "ignored.md"
	_, err := NewHarness(cfg, nil)
	if err == nil {
		t.Fatal("expected error on nil scenarios")
	}
	_, err = NewHarness(cfg, []scenarios.Scenario{})
	if err == nil {
		t.Fatal("expected error on empty scenarios")
	}
}

func TestNewHarness_RejectsVerbNotInProfile(t *testing.T) {
	t.Parallel()
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "tiny", DefaultDuration: time.Second, ErrorBudgetRatio: 0.01,
		Verbs: []reliability.VerbProfile{
			{Verb: reliability.VerbAgentRecall, MetricName: "m", SustainedRPS: 1, SLO95Seconds: 1, SLO99Seconds: 2},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = "ignored.md"
	scen := []scenarios.Scenario{
		&scenarios.ObserveScenario{Client: &fakeAgentClient{}}, // not in profile
	}
	_, err := NewHarness(cfg, scen)
	if err == nil || !strings.Contains(err.Error(), "is not in profile") {
		t.Fatalf("expected profile-mismatch error, got %v", err)
	}
}

func TestHarness_CleanRun_AllScenarios(t *testing.T) {
	t.Parallel()
	agent := &fakeAgentClient{
		delay: 1 * time.Millisecond, // well under any SLO
		labelledRecall: &scenarios.RecallResponse{
			NodeIDs:    []string{"node-target", "n2", "n3"},
			ConceptIDs: []string{"concept-target", "c2"},
		},
	}
	mgmt := &fakeMgmtClient{delay: 1 * time.Millisecond}

	cfg := calibration.DefaultConfig()
	cfg.Profile = shortProfile()
	cfg.ArtifactPath = "test-artifact.md"
	cfg.RepoID = "repo-test"
	cfg.SeededFixtureLOC = 200_000
	cfg.RandomSeed = 42
	cfg.MaxInflightPerVerb = 32

	scen := []scenarios.Scenario{
		&scenarios.RecallScenario{
			Client: agent, RepoID: "repo-test", K: 20,
			Queries: []scenarios.RecallQuery{{
				Query:              "calibration query",
				ExpectedNodeID:     "node-target",
				ExpectedConceptIDs: []string{"concept-target"},
			}},
		},
		&scenarios.ObserveScenario{Client: agent, RepoID: "repo-test"},
		&scenarios.CallChainQueryScenario{Client: agent, RepoID: "repo-test", MaxDepth: 3},
		&scenarios.SummarizeScenario{Client: agent, RepoID: "repo-test"},
		&scenarios.GraphIngestScenario{Client: mgmt, RepoID: "repo-test"},
	}

	h, err := NewHarness(cfg, scen)
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(rep.Verbs) != 5 {
		t.Fatalf("want 5 verb results, got %d", len(rep.Verbs))
	}
	for _, v := range rep.Verbs {
		if v.Sent == 0 {
			t.Errorf("verb %s sent no requests", v.Verb)
		}
		if !v.BudgetMet {
			t.Errorf("verb %s exceeded error budget: %f", v.Verb, v.ErrorRatio)
		}
	}
	if len(rep.BudgetBreaches) != 0 {
		t.Errorf("unexpected budget breaches: %v", rep.BudgetBreaches)
	}
	if rep.LearningQuality.Evaluated == 0 {
		t.Error("expected at least one labelled-query evaluation")
	}
	if rep.LearningQuality.MedianRankOfCorrectNodeAtK != 1 {
		t.Errorf("expected median rank 1 (target at index 0), got %v",
			rep.LearningQuality.MedianRankOfCorrectNodeAtK)
	}
	if rep.LearningQuality.ConceptHitFractionAtK != 1.0 {
		t.Errorf("expected concept-hit fraction 1.0, got %v",
			rep.LearningQuality.ConceptHitFractionAtK)
	}
	if !rep.LearningQuality.RankMet || !rep.LearningQuality.ConceptHitMet {
		t.Error("expected both learning-quality SLOs met")
	}
	if rep.RandomSeed != 42 {
		t.Errorf("seed not echoed: got %d", rep.RandomSeed)
	}
	if rep.ProfileName != "test" {
		t.Errorf("profile name not echoed: got %s", rep.ProfileName)
	}

	body := rep.RenderMarkdown()
	for _, want := range []string{
		"Load-test calibration",
		"profile: test",
		"agent.recall",
		"agent.observe",
		"agent.expand",
		"agent.summarize",
		"mgmt.ingest_spans",
		"Learning-quality SLOs",
		"labelled-query proxy",
		"**Status:** PASS",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered artifact missing %q", want)
		}
	}
}

func TestHarness_LearningQualityMiss_AggregatesAsWorstRank(t *testing.T) {
	t.Parallel()
	// The fake returns a recall response that does NOT include
	// the expected node — the harness should bucket the miss
	// into the K+1 slot so the median is hurt rather than
	// silently improved.
	agent := &fakeAgentClient{
		delay: 1 * time.Millisecond,
		labelledRecall: &scenarios.RecallResponse{
			NodeIDs:    []string{"unrelated-1", "unrelated-2"},
			ConceptIDs: []string{"unrelated-c"},
		},
	}
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "tiny", DefaultDuration: 150 * time.Millisecond, ErrorBudgetRatio: 0.05,
		Verbs: []reliability.VerbProfile{
			{Verb: reliability.VerbAgentRecall, MetricName: "test_recall", SustainedRPS: 40, SLO95Seconds: 1, SLO99Seconds: 2},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = "test-artifact.md"

	scen := []scenarios.Scenario{
		&scenarios.RecallScenario{
			Client: agent, RepoID: "r", K: 20,
			Queries: []scenarios.RecallQuery{{
				Query:              "miss",
				ExpectedNodeID:     "node-target", // not returned by fake
				ExpectedConceptIDs: []string{"concept-target"},
			}},
		},
	}
	h, err := NewHarness(cfg, scen)
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.LearningQuality.Evaluated == 0 {
		t.Fatal("expected the miss to count as an evaluation")
	}
	// Every measurement missed → median should be the worst-rank
	// bucket (K+1 = 21).
	if rep.LearningQuality.MedianRankOfCorrectNodeAtK != 21 {
		t.Errorf("expected median rank 21 (K+1 worst-rank bucket), got %v",
			rep.LearningQuality.MedianRankOfCorrectNodeAtK)
	}
	if rep.LearningQuality.RankMet {
		t.Error("expected RankMet=false on full-miss run")
	}
	if rep.LearningQuality.ConceptHitFractionAtK != 0.0 {
		t.Errorf("expected concept-hit 0, got %v", rep.LearningQuality.ConceptHitFractionAtK)
	}
}

func TestHarness_DroppedTicks_Reported(t *testing.T) {
	t.Parallel()
	// Per-call delay >> tick interval AND in-flight=1 → the
	// open-loop scheduler MUST record dropped ticks rather
	// than serialising.
	mgmt := &fakeMgmtClient{delay: 30 * time.Millisecond}

	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "drop-test", DefaultDuration: 200 * time.Millisecond, ErrorBudgetRatio: 0.5,
		Verbs: []reliability.VerbProfile{
			{Verb: reliability.VerbMgmtIngestSpans, MetricName: "m", SustainedRPS: 100, SLO95Seconds: 1, SLO99Seconds: 2},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = "test-artifact.md"
	cfg.MaxInflightPerVerb = 1

	scen := []scenarios.Scenario{
		&scenarios.GraphIngestScenario{Client: mgmt, RepoID: "r"},
	}

	h, err := NewHarness(cfg, scen)
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Verbs) != 1 {
		t.Fatalf("want 1 verb, got %d", len(rep.Verbs))
	}
	v := rep.Verbs[0]
	if v.DroppedTicks == 0 {
		t.Errorf("expected dropped ticks > 0 (in-flight=1, delay >> interval), got %d", v.DroppedTicks)
	}
	if v.Sent == 0 {
		t.Errorf("expected at least one successful send, got 0")
	}
	// The artifact should surface the drop in the hygiene
	// table AND in notes.
	body := rep.RenderMarkdown()
	if !strings.Contains(body, "Open-loop scheduler hygiene") {
		t.Error("expected dropped-ticks table in artifact")
	}
	foundNote := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "dropped") {
			foundNote = true
			break
		}
	}
	if !foundNote {
		t.Errorf("expected dropped-tick note, got notes: %v", rep.Notes)
	}
}

// TestHarness_DegradedResponses_NoteIncludesReasons pins that
// the harness's degraded-responses note line includes the
// per-reason occurrence counts when the upstream returned a
// non-empty `DegradedReason`. The earlier iter rendered only
// the boolean count even though `Sample.DegradedReason` was
// being captured at the scenario layer — the note emitter
// dropped the reason on the floor and the artifact gave the
// operator no signal about which backpressure mode dominated.
func TestHarness_DegradedResponses_NoteIncludesReasons(t *testing.T) {
	t.Parallel()
	agent := &fakeAgentClient{
		delay: 1 * time.Millisecond,
		labelledRecall: &scenarios.RecallResponse{
			NodeIDs:    []string{"node-target"},
			ConceptIDs: []string{"concept-target"},
		},
	}
	// degradedEvery=2 → roughly half of mgmt responses
	// return degraded=true with a stable reason so the
	// note emitter has counts to render.
	mgmt := &fakeMgmtClient{
		delay:          1 * time.Millisecond,
		degradedEvery:  2,
		degradedReason: "writer_backpressure",
	}

	cfg := calibration.DefaultConfig()
	cfg.Profile = shortProfile()
	cfg.ArtifactPath = "test-artifact.md"
	cfg.RepoID = "repo-test"
	cfg.SeededFixtureLOC = 200_000
	cfg.RandomSeed = 42
	cfg.MaxInflightPerVerb = 32
	scen := []scenarios.Scenario{
		&scenarios.RecallScenario{
			Client: agent, RepoID: "repo-test", K: 20,
			Queries: []scenarios.RecallQuery{{
				Query:              "q",
				ExpectedNodeID:     "node-target",
				ExpectedConceptIDs: []string{"concept-target"},
			}},
		},
		&scenarios.ObserveScenario{Client: agent, RepoID: "repo-test"},
		&scenarios.CallChainQueryScenario{Client: agent, RepoID: "repo-test", MaxDepth: 3},
		&scenarios.SummarizeScenario{Client: agent, RepoID: "repo-test"},
		&scenarios.GraphIngestScenario{Client: mgmt, RepoID: "repo-test"},
	}
	h, err := NewHarness(cfg, scen)
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var note string
	for _, n := range rep.Notes {
		if strings.Contains(n, "mgmt.ingest_spans") && strings.Contains(n, "degraded=true") {
			note = n
			break
		}
	}
	if note == "" {
		t.Fatalf("expected mgmt.ingest_spans degraded note, got notes: %v", rep.Notes)
	}
	if !strings.Contains(note, "reasons:") {
		t.Errorf("expected note to include 'reasons:' prefix; got %q", note)
	}
	if !strings.Contains(note, "writer_backpressure=") {
		t.Errorf("expected note to include per-reason count for writer_backpressure; got %q", note)
	}
}

// TestHarness_DegradedResponses_BooleanOnlyWhenReasonEmpty
// pins the fallback shape: when the upstream returns
// degraded=true with an EMPTY DegradedReason (today: all
// agent.* verbs, whose response envelopes carry only a
// boolean), the note emitter MUST render the boolean-only
// shape without a "reasons:" suffix so the operator doesn't
// see a half-empty "; reasons: " trailer.
func TestHarness_DegradedResponses_BooleanOnlyWhenReasonEmpty(t *testing.T) {
	t.Parallel()
	reasons := map[string]int{}
	if got := formatDegradedReasons(reasons); got != "" {
		t.Errorf("empty reasons map should format to '', got %q", got)
	}
	reasons = nil
	if got := formatDegradedReasons(reasons); got != "" {
		t.Errorf("nil reasons map should format to '', got %q", got)
	}
	// Descending count order; ties broken by reason name.
	got := formatDegradedReasons(map[string]int{
		"qdrant_unreachable":  3,
		"writer_backpressure": 12,
		"unknown_b":           5,
		"unknown_a":           5,
	})
	want := "writer_backpressure=12, unknown_a=5, unknown_b=5, qdrant_unreachable=3"
	if got != want {
		t.Errorf("formatDegradedReasons:\n  got  %q\n  want %q", got, want)
	}
}

func TestHarness_BudgetBreach_NonZeroExitSignal(t *testing.T) {
	t.Parallel()
	// failEvery=1 → every recall call fails → ErrorRatio=1.0
	// breaches the 0.05 budget.
	agent := &fakeAgentClient{
		delay:     1 * time.Millisecond,
		failEvery: 1,
	}
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "fail", DefaultDuration: 150 * time.Millisecond, ErrorBudgetRatio: 0.05,
		Verbs: []reliability.VerbProfile{
			{Verb: reliability.VerbAgentRecall, MetricName: "m", SustainedRPS: 40, SLO95Seconds: 1, SLO99Seconds: 2},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = "test-artifact.md"

	h, err := NewHarness(cfg, []scenarios.Scenario{
		&scenarios.RecallScenario{Client: agent, RepoID: "r", K: 20},
	})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.BudgetBreaches) == 0 {
		t.Error("expected budget breach")
	}
	if !strings.Contains(rep.RenderMarkdown(), "**Status:** FAIL") {
		t.Error("expected FAIL status in artifact")
	}
}

func TestHarness_RandomSeed_Echoed(t *testing.T) {
	t.Parallel()
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "seed", DefaultDuration: 30 * time.Millisecond, ErrorBudgetRatio: 0.5,
		Verbs: []reliability.VerbProfile{
			{Verb: reliability.VerbAgentRecall, MetricName: "m", SustainedRPS: 5, SLO95Seconds: 1, SLO99Seconds: 2},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = "test-artifact.md"
	cfg.RandomSeed = 0 // request a wall-clock seed

	h, err := NewHarness(cfg, []scenarios.Scenario{
		&scenarios.RecallScenario{Client: &fakeAgentClient{}, RepoID: "r", K: 20},
	})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RandomSeed == 0 {
		t.Error("expected a non-zero wall-clock seed echoed when caller passed 0")
	}
}

// fixedRNG is a deterministic RNG: every Intn(n) returns
// `seed % n`. Used by tests that want to assert scenario
// rotation determinism.
type fixedRNG struct{ seed int }

func (r *fixedRNG) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return r.seed % n
}

func TestWithRNG_OverridesFactory(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	factory := func() scenarios.RNG {
		counter.Add(1)
		return &fixedRNG{seed: 7}
	}
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "rng", DefaultDuration: 50 * time.Millisecond, ErrorBudgetRatio: 0.5,
		Verbs: []reliability.VerbProfile{
			{Verb: reliability.VerbAgentRecall, MetricName: "m", SustainedRPS: 20, SLO95Seconds: 1, SLO99Seconds: 2},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = "test-artifact.md"

	h, err := NewHarness(cfg, []scenarios.Scenario{
		&scenarios.RecallScenario{Client: &fakeAgentClient{}, RepoID: "r", K: 20},
	}, WithRNG(factory))
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	if _, err := h.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if counter.Load() == 0 {
		t.Error("expected the custom RNG factory to be invoked")
	}
}

func TestHarness_WriteArtifact_Roundtrip(t *testing.T) {
	t.Parallel()
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "write", DefaultDuration: 30 * time.Millisecond, ErrorBudgetRatio: 0.5,
		Verbs: []reliability.VerbProfile{
			{Verb: reliability.VerbAgentRecall, MetricName: "m", SustainedRPS: 5, SLO95Seconds: 1, SLO99Seconds: 2},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = "any.md"

	h, err := NewHarness(cfg, []scenarios.Scenario{
		&scenarios.RecallScenario{Client: &fakeAgentClient{}, RepoID: "r", K: 20},
	})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sb strings.Builder
	if _, err := rep.WriteTo(&sb); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !strings.Contains(sb.String(), "Load-test calibration") {
		t.Errorf("WriteTo output missing header: %s", sb.String())
	}
}

func TestHarness_ContextCancellation_StopsEarly(t *testing.T) {
	t.Parallel()
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "cancel", DefaultDuration: 5 * time.Second, ErrorBudgetRatio: 0.5,
		Verbs: []reliability.VerbProfile{
			{Verb: reliability.VerbAgentRecall, MetricName: "m", SustainedRPS: 20, SLO95Seconds: 1, SLO99Seconds: 2},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = "any.md"

	h, err := NewHarness(cfg, []scenarios.Scenario{
		&scenarios.RecallScenario{Client: &fakeAgentClient{delay: 1 * time.Millisecond}, RepoID: "r", K: 20},
	})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	rep, err := h.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("expected cancellation to stop run quickly, took %v", elapsed)
	}
	if !rep.Aborted {
		t.Errorf("expected report.Aborted == true after caller cancel, got false")
	}
	if rep.CompletionReason != "aborted-context-cancelled" {
		t.Errorf("expected CompletionReason 'aborted-context-cancelled', got %q", rep.CompletionReason)
	}
	if rep.ActualDuration >= rep.PlannedDuration {
		t.Errorf("expected ActualDuration < PlannedDuration on early cancel, actual=%v planned=%v",
			rep.ActualDuration, rep.PlannedDuration)
	}
}

func TestHarness_CompletedRun_NotAborted(t *testing.T) {
	t.Parallel()
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "completed", DefaultDuration: 150 * time.Millisecond, ErrorBudgetRatio: 0.5,
		Verbs: []reliability.VerbProfile{
			{Verb: "agent.recall", MetricName: "x", SustainedRPS: 5, SLO95Seconds: 1, SLO99Seconds: 1},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = filepath.Join(t.TempDir(), "art.md")
	cfg.RepoID = "r"
	h, err := NewHarness(cfg, []scenarios.Scenario{
		&scenarios.RecallScenario{Client: &fakeAgentClient{}, RepoID: "r", K: 20},
	})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Aborted {
		t.Errorf("expected Aborted == false on natural completion, got true")
	}
	if rep.CompletionReason != "completed" {
		t.Errorf("expected CompletionReason 'completed', got %q", rep.CompletionReason)
	}
}

func TestHarness_SampleObserver_FiredForEachSample(t *testing.T) {
	t.Parallel()
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "obs", DefaultDuration: 200 * time.Millisecond, ErrorBudgetRatio: 0.5,
		Verbs: []reliability.VerbProfile{
			{Verb: "agent.recall", MetricName: "x", SustainedRPS: 20, SLO95Seconds: 1, SLO99Seconds: 1},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = filepath.Join(t.TempDir(), "art.md")
	cfg.RepoID = "r"

	var observed atomic.Int64
	h, err := NewHarness(cfg, []scenarios.Scenario{
		&scenarios.RecallScenario{Client: &fakeAgentClient{}, RepoID: "r", K: 20},
	}, WithSampleObserver(func(s scenarios.Sample) {
		if s.Verb != reliability.VerbAgentRecall {
			t.Errorf("unexpected verb in observer: %q", s.Verb)
		}
		observed.Add(1)
	}))
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sent int
	for _, v := range rep.Verbs {
		sent += v.Sent
	}
	// Observer must fire once per Execute completion.
	if got := int(observed.Load()); got != sent {
		t.Errorf("observer fires: want %d (== sent), got %d", sent, got)
	}
}

func TestHarness_SampleObserver_PanicDoesNotLoseSample(t *testing.T) {
	t.Parallel()
	// A crashing observer MUST NOT drop the sample from the
	// aggregator; observer is best-effort, the aggregator is
	// source of truth.
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "panic", DefaultDuration: 120 * time.Millisecond, ErrorBudgetRatio: 0.5,
		Verbs: []reliability.VerbProfile{
			{Verb: "agent.recall", MetricName: "x", SustainedRPS: 10, SLO95Seconds: 1, SLO99Seconds: 1},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = filepath.Join(t.TempDir(), "art.md")
	cfg.RepoID = "r"

	h, err := NewHarness(cfg, []scenarios.Scenario{
		&scenarios.RecallScenario{Client: &fakeAgentClient{}, RepoID: "r", K: 20},
	}, WithSampleObserver(func(_ scenarios.Sample) {
		panic("observer crashed on purpose")
	}))
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sent int
	for _, v := range rep.Verbs {
		sent += v.Sent
	}
	if sent == 0 {
		t.Errorf("expected the aggregator to still record samples despite observer panic; got zero")
	}
}

func TestHarness_ArtifactNotes_AppendedToReport(t *testing.T) {
	t.Parallel()
	cfg := calibration.DefaultConfig()
	cfg.Profile = reliability.LoadProfile{
		Name: "notes", DefaultDuration: 120 * time.Millisecond, ErrorBudgetRatio: 0.5,
		Verbs: []reliability.VerbProfile{
			{Verb: "agent.recall", MetricName: "x", SustainedRPS: 5, SLO95Seconds: 1, SLO99Seconds: 1},
		},
		LearningQuality: reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25},
	}
	cfg.ArtifactPath = filepath.Join(t.TempDir(), "art.md")
	cfg.RepoID = "r"

	h, err := NewHarness(cfg, []scenarios.Scenario{
		&scenarios.RecallScenario{Client: &fakeAgentClient{}, RepoID: "r", K: 20},
	},
		WithArtifactNote("operator workflow: see docs/code-intelligence/agent-memory/load-test-calibration.md"),
		WithArtifactNote("default --max-inflight is 256"),
		WithArtifactNote(""), // empty MUST be dropped, not emitted as a blank bullet
	)
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	rep, err := h.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	joined := strings.Join(rep.Notes, "\n")
	if !strings.Contains(joined, "operator workflow") {
		t.Errorf("artifact note not appended to Notes: %#v", rep.Notes)
	}
	if !strings.Contains(joined, "default --max-inflight is 256") {
		t.Errorf("second artifact note not appended: %#v", rep.Notes)
	}
	for _, n := range rep.Notes {
		if n == "" {
			t.Errorf("empty artifact note leaked into report Notes: %#v", rep.Notes)
		}
	}
}

// guard against an unused import when the test set shrinks.
var (
	_ = sync.Mutex{}
	_ = rand.Int
	_ = time.Second
)
