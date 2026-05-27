package calibration_test

import (
	"bytes"
	"io/fs"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/calibration"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

func TestPercentiles_EmptyInput(t *testing.T) {
	p50, p95, p99 := calibration.Percentiles(nil)
	if p50 != 0 || p95 != 0 || p99 != 0 {
		t.Errorf("empty Percentiles should be all 0, got %v / %v / %v", p50, p95, p99)
	}
}

func TestPercentiles_NearestRank(t *testing.T) {
	// 10 samples, p95 nearest-rank = ceil(0.95*10)-1 = 9 (largest).
	samples := []time.Duration{}
	for i := 1; i <= 10; i++ {
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}
	p50, p95, p99 := calibration.Percentiles(samples)
	if want := 5 * time.Millisecond; p50 != want {
		t.Errorf("p50: want %v, got %v", want, p50)
	}
	if want := 10 * time.Millisecond; p95 != want {
		t.Errorf("p95: want %v, got %v", want, p95)
	}
	if want := 10 * time.Millisecond; p99 != want {
		t.Errorf("p99: want %v, got %v", want, p99)
	}
}

func TestPercentiles_HundredSamples(t *testing.T) {
	samples := []time.Duration{}
	for i := 1; i <= 100; i++ {
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}
	p50, p95, p99 := calibration.Percentiles(samples)
	if want := 50 * time.Millisecond; p50 != want {
		t.Errorf("p50: want %v, got %v", want, p50)
	}
	if want := 95 * time.Millisecond; p95 != want {
		t.Errorf("p95: want %v, got %v", want, p95)
	}
	if want := 99 * time.Millisecond; p99 != want {
		t.Errorf("p99: want %v, got %v", want, p99)
	}
}

func TestAggregateVerb_ComputesAllFields(t *testing.T) {
	verb := reliability.VerbProfile{
		Verb:         "agent.recall",
		MetricName:   "agent_recall_duration_seconds",
		SustainedRPS: 50,
		SLO95Seconds: 1.5,
		SLO99Seconds: 4,
	}
	latencies := []time.Duration{}
	for i := 1; i <= 100; i++ {
		latencies = append(latencies, time.Duration(i)*time.Millisecond)
	}
	result := calibration.AggregateVerb(verb, latencies, 99, 1, 0, 2*time.Second, 2*time.Second, 0.01)
	if result.Sent != 100 {
		t.Errorf("Sent: want 100, got %d", result.Sent)
	}
	if result.Failed != 1 {
		t.Errorf("Failed: want 1, got %d", result.Failed)
	}
	if math.Abs(result.ErrorRatio-0.01) > 1e-9 {
		t.Errorf("ErrorRatio: want 0.01, got %v", result.ErrorRatio)
	}
	if math.Abs(result.AchievedRPS-50.0) > 1e-9 {
		t.Errorf("AchievedRPS: want 50.0, got %v", result.AchievedRPS)
	}
	if result.P95 != 95*time.Millisecond {
		t.Errorf("P95: want 95ms, got %v", result.P95)
	}
	if !result.SLO95Met {
		t.Error("SLO95Met should be true (95ms < 1500ms)")
	}
	if !result.SLO99Met {
		t.Error("SLO99Met should be true")
	}
	if !result.BudgetMet {
		t.Error("BudgetMet should be true (1% == 1%)")
	}
	if result.Min != time.Millisecond {
		t.Errorf("Min: want 1ms, got %v", result.Min)
	}
	if result.Max != 100*time.Millisecond {
		t.Errorf("Max: want 100ms, got %v", result.Max)
	}
}

func TestAggregateVerb_BudgetBreach(t *testing.T) {
	verb := reliability.NominalLoadProfile().Verbs[0]
	latencies := []time.Duration{10 * time.Millisecond}
	res := calibration.AggregateVerb(verb, latencies, 99, 100, 0, time.Second, time.Second, 0.01)
	if res.BudgetMet {
		t.Error("BudgetMet should be false when 100/199 failed > 1%")
	}
}

func TestAggregateLearningQuality_EmptyInputs(t *testing.T) {
	r := calibration.AggregateLearningQuality(reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25}, nil, nil)
	if !math.IsNaN(r.MedianRankOfCorrectNodeAtK) {
		t.Errorf("MedianRankOfCorrectNodeAtK with empty input should be NaN, got %v", r.MedianRankOfCorrectNodeAtK)
	}
	if !math.IsNaN(r.ConceptHitFractionAtK) {
		t.Errorf("ConceptHitFractionAtK with empty input should be NaN, got %v", r.ConceptHitFractionAtK)
	}
	if r.SLOSource != "labelled-query proxy" {
		t.Errorf("SLOSource: want %q, got %q", "labelled-query proxy", r.SLOSource)
	}
	if r.Evaluated != 0 {
		t.Errorf("Evaluated: want 0, got %d", r.Evaluated)
	}
}

func TestAggregateLearningQuality_MissedQueriesCountAsWorstRank(t *testing.T) {
	targets := reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25}
	// 4 queries: ranks 1, 2, 0 (missed), 0 (missed) → bucket [1, 2, 21, 21] → median = (2+21)/2 = 11.5.
	ranks := []int{1, 2, 0, 0}
	r := calibration.AggregateLearningQuality(targets, ranks, nil)
	if r.MedianRankOfCorrectNodeAtK != 11.5 {
		t.Errorf("median with missed queries: want 11.5, got %v", r.MedianRankOfCorrectNodeAtK)
	}
	if r.RankMet {
		t.Error("RankMet should be false (11.5 > 5)")
	}
}

func TestAggregateLearningQuality_ConceptHitFraction(t *testing.T) {
	targets := reliability.LearningQualityTargets{K: 20, MaxMedianRankAtK: 5, MinConceptHitFractionAtK: 0.25}
	hits := []bool{true, true, false, false, false} // 2/5 = 0.4 ≥ 0.25.
	r := calibration.AggregateLearningQuality(targets, nil, hits)
	if math.Abs(r.ConceptHitFractionAtK-0.4) > 1e-9 {
		t.Errorf("ConceptHitFractionAtK: want 0.4, got %v", r.ConceptHitFractionAtK)
	}
	if !r.ConceptHitMet {
		t.Error("ConceptHitMet should be true (0.4 ≥ 0.25)")
	}
}

func TestReport_RenderMarkdown_ContainsRequiredSections(t *testing.T) {
	r := calibration.Report{
		ProfileName:      "nominal",
		StartedAt:        time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		FinishedAt:       time.Date(2025, 1, 2, 3, 34, 6, 0, time.UTC),
		PlannedDuration:  30 * time.Minute,
		ActualDuration:   30 * time.Minute,
		RepoID:           "repo-abc",
		SeededFixtureLOC: 200_000,
		RandomSeed:       42,
		ErrorBudgetRatio: 0.01,
		Verbs: []calibration.VerbResult{
			{
				Verb:         "agent.recall",
				MetricName:   "agent_recall_duration_seconds",
				RequestedRPS: 50,
				AchievedRPS:  49.95,
				Sent:         89900,
				Succeeded:    89800,
				Failed:       100,
				ErrorRatio:   100.0 / 89900.0,
				P50:          100 * time.Millisecond,
				P95:          800 * time.Millisecond,
				P99:          1900 * time.Millisecond,
				SLO95Seconds: 1.5,
				SLO99Seconds: 4,
				SLO95Met:     true,
				SLO99Met:     true,
				BudgetMet:    true,
			},
		},
		LearningQuality: calibration.LearningQualityResult{
			K:                            20,
			Evaluated:                    50,
			MedianRankOfCorrectNodeAtK:   3,
			ConceptHitFractionAtK:        0.4,
			MaxMedianRankAtK:             5,
			MinConceptHitFractionAtK:     0.25,
			RankMet:                      true,
			ConceptHitMet:                true,
			SLOSource:                    "labelled-query proxy",
		},
		Notes: []string{"first calibration run after partition-maintainer ramp"},
	}
	md := r.RenderMarkdown()
	mustContain := []string{
		"# Load-test calibration — iter 1",
		"profile: nominal",
		"random_seed: 42",
		"seeded_fixture_loc: 200000",
		"**Status:** PASS",
		"## Per-verb percentiles",
		"agent.recall",
		"## Learning-quality SLOs",
		"`rank_of_correct_node_at_k20`",
		"`concept_hit_fraction_at_k20`",
		"labelled-query proxy",
		"## Operator notes",
		"first calibration run",
	}
	for _, want := range mustContain {
		if !strings.Contains(md, want) {
			t.Errorf("RenderMarkdown output missing %q", want)
		}
	}
}

func TestReport_RenderMarkdown_FAIL_StatusWhenBreach(t *testing.T) {
	r := calibration.Report{
		ProfileName:      "nominal",
		ErrorBudgetRatio: 0.01,
		BudgetBreaches:   []string{"agent.recall"},
		LearningQuality:  calibration.LearningQualityResult{K: 20, MedianRankOfCorrectNodeAtK: math.NaN(), ConceptHitFractionAtK: math.NaN(), SLOSource: "labelled-query proxy"},
	}
	md := r.RenderMarkdown()
	if !strings.Contains(md, "**Status:** FAIL") {
		t.Errorf("FAIL status missing from\n%s", md)
	}
	if !strings.Contains(md, "## Error-budget breaches") {
		t.Errorf("Error-budget breaches section missing from\n%s", md)
	}
	if !strings.Contains(md, "n/a (no labelled queries supplied)") {
		t.Errorf("NaN should render as n/a placeholder, got\n%s", md)
	}
}

func TestReport_RenderMarkdown_DroppedTicksTable(t *testing.T) {
	r := calibration.Report{
		ProfileName: "nominal",
		Verbs: []calibration.VerbResult{
			{Verb: "agent.recall", PlannedRequests: 100, Sent: 90, DroppedTicks: 10},
		},
		LearningQuality: calibration.LearningQualityResult{K: 20, MedianRankOfCorrectNodeAtK: math.NaN(), ConceptHitFractionAtK: math.NaN(), SLOSource: "labelled-query proxy"},
	}
	md := r.RenderMarkdown()
	if !strings.Contains(md, "Open-loop scheduler hygiene") {
		t.Errorf("dropped-ticks table missing; got\n%s", md)
	}
}

func TestReport_WriteFile_InjectableWriter(t *testing.T) {
	r := calibration.Report{
		ProfileName:     "smoke",
		LearningQuality: calibration.LearningQualityResult{K: 20, MedianRankOfCorrectNodeAtK: math.NaN(), ConceptHitFractionAtK: math.NaN(), SLOSource: "labelled-query proxy"},
	}
	got := map[string][]byte{}
	err := r.WriteFile("/tmp/x.md", func(path string, body []byte, _ os.FileMode) error {
		got[path] = body
		return nil
	})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !bytes.Contains(got["/tmp/x.md"], []byte("# Load-test calibration")) {
		t.Errorf("written body missing title; got %q", got["/tmp/x.md"])
	}
}

func TestReport_WriteFile_NilWriter(t *testing.T) {
	r := calibration.Report{}
	err := r.WriteFile("/tmp/x.md", nil)
	if err == nil {
		t.Fatal("WriteFile with nil writer should error")
	}
}

func TestReport_WriteTo_StreamsBody(t *testing.T) {
	r := calibration.Report{ProfileName: "smoke", LearningQuality: calibration.LearningQualityResult{K: 20, MedianRankOfCorrectNodeAtK: math.NaN(), ConceptHitFractionAtK: math.NaN(), SLOSource: "labelled-query proxy"}}
	var buf bytes.Buffer
	n, err := r.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n == 0 {
		t.Error("WriteTo returned 0 bytes")
	}
	if !strings.Contains(buf.String(), "# Load-test calibration") {
		t.Errorf("streamed body missing title")
	}
}

func TestRenderMarkdown_YamlStringEscapesSpecials(t *testing.T) {
	r := calibration.Report{
		ProfileName:     "nominal",
		RepoID:          `with:colon "and"`,
		LearningQuality: calibration.LearningQualityResult{K: 20, MedianRankOfCorrectNodeAtK: math.NaN(), ConceptHitFractionAtK: math.NaN()},
	}
	md := r.RenderMarkdown()
	if !strings.Contains(md, `repo_id: "with:colon \"and\""`) {
		t.Errorf("yamlString should quote-escape; got\n%s", md)
	}
}

// TestRenderMarkdown_ProvenanceBannerVisible pins the iter-4
// F2 fix: when Config.Provenance is plumbed through into the
// rendered artifact, a reviewer scanning the markdown MUST see
// the banner above the front-matter so they can distinguish an
// in-process stub baseline from a real deploy/local-stack §8.3
// calibration without cross-referencing the operator doc.
func TestRenderMarkdown_ProvenanceBannerVisible(t *testing.T) {
	r := calibration.Report{
		ProfileName:     "nominal",
		Provenance:      "IN-PROCESS STUB BASELINE — gen_artifact.go against httptest stubs; NOT a §8.3 production seal.",
		LearningQuality: calibration.LearningQualityResult{K: 20, MedianRankOfCorrectNodeAtK: math.NaN(), ConceptHitFractionAtK: math.NaN()},
	}
	md := r.RenderMarkdown()

	// The banner must render as a markdown blockquote callout
	// (`> ⚠ **Provenance:** ...`) so it stands out visually.
	if !strings.Contains(md, "> ⚠ **Provenance:** IN-PROCESS STUB BASELINE") {
		t.Errorf("expected provenance callout in artifact; got\n%s", md)
	}

	// The banner must appear BEFORE the front-matter so a
	// reviewer cannot skip past it.
	bannerIdx := strings.Index(md, "**Provenance:**")
	frontMatterIdx := strings.Index(md, "```yaml")
	if bannerIdx < 0 || frontMatterIdx < 0 || bannerIdx > frontMatterIdx {
		t.Errorf("provenance banner must appear before YAML front matter; bannerIdx=%d, frontMatterIdx=%d\n%s",
			bannerIdx, frontMatterIdx, md)
	}

	// The YAML front matter must echo the provenance string so a
	// downstream parser can ingest it without a markdown reader.
	if !strings.Contains(md, "provenance: ") {
		t.Errorf("expected `provenance:` key in YAML front matter; got\n%s", md)
	}
}

// TestRenderMarkdown_ProvenanceOmittedWhenEmpty pins the empty-
// Provenance contract: the banner is suppressed (an empty
// callout would be visual noise) AND the YAML front-matter does
// not emit a `provenance:` key (the parser-shape must stay
// stable for runs that don't supply a banner).
func TestRenderMarkdown_ProvenanceOmittedWhenEmpty(t *testing.T) {
	r := calibration.Report{
		ProfileName:     "nominal",
		Provenance:      "",
		LearningQuality: calibration.LearningQualityResult{K: 20, MedianRankOfCorrectNodeAtK: math.NaN(), ConceptHitFractionAtK: math.NaN()},
	}
	md := r.RenderMarkdown()
	if strings.Contains(md, "**Provenance:**") {
		t.Errorf("empty Provenance should not render the callout; got\n%s", md)
	}
	if strings.Contains(md, "provenance:") {
		t.Errorf("empty Provenance should not render the YAML key; got\n%s", md)
	}
}

// TestRenderMarkdown_PendingProductionSealPointerVisible pins the
// iter-7 structural fix for the long-running "committed artifact is
// not the production seal" evaluator finding: when the provenance
// flags a non-seal run (in-process baseline, synthetic seed, smoke
// preflight, or any unrecognised vocabulary), the rendered artifact
// MUST surface a pointer to the operator-action-items tracker so a
// reviewer who reads the committed artifact alone can see that the
// production-seal gap is structurally tracked (not silently
// outstanding). The implementation-plan acceptance-criterion line
// is cited verbatim so a future plan reshuffle that orphans the
// pointer is caught at test time.
func TestRenderMarkdown_PendingProductionSealPointerVisible(t *testing.T) {
	cases := []struct {
		name       string
		provenance string
	}{
		{"in-process baseline", "IN-PROCESS STUB BASELINE — gen_artifact.go against httptest stubs; NOT a §8.3 production seal."},
		{"synthetic seed", "SYNTHETIC SEED — seed-fixture-200k Makefile target; structural stand-in only."},
		{"smoke preflight", "SMOKE PREFLIGHT — sub-second CI calibration; not a release gate."},
		{"unrecognised vocabulary (fail-safe)", "custom operator string with no recognised marker"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := calibration.Report{
				ProfileName:     "nominal",
				Provenance:      tc.provenance,
				LearningQuality: calibration.LearningQualityResult{K: 20, MedianRankOfCorrectNodeAtK: math.NaN(), ConceptHitFractionAtK: math.NaN()},
			}
			md := r.RenderMarkdown()
			if !strings.Contains(md, "**Production-seal artifact pending.**") {
				t.Errorf("expected pending-production-seal callout for %q provenance; got\n%s", tc.provenance, md)
			}
			if !strings.Contains(md, "operator-action-items.md") {
				t.Errorf("expected pointer to operator-action-items.md tracker; got\n%s", md)
			}
			if !strings.Contains(md, "implementation-plan.md:1479-1486") {
				t.Errorf("expected citation of the implementation-plan acceptance-criterion line range; got\n%s", md)
			}
		})
	}
}

// TestRenderMarkdown_ProductionSealSuppressesPendingPointer pins
// the inverse: when an operator stamps a true production-seal
// provenance (the DEPLOY/LOCAL STACK + NOMINAL CALIBRATION
// vocabulary the operator-action-items tracker prescribes), the
// pending-tracker pointer is suppressed so the resulting artifact
// reads cleanly as a production-seal deliverable.
func TestRenderMarkdown_ProductionSealSuppressesPendingPointer(t *testing.T) {
	r := calibration.Report{
		ProfileName:     "nominal",
		Provenance:      "DEPLOY/LOCAL STACK NOMINAL CALIBRATION — 2026-06-01; seeded acme-monorepo/abc123.",
		LearningQuality: calibration.LearningQualityResult{K: 20, MedianRankOfCorrectNodeAtK: math.NaN(), ConceptHitFractionAtK: math.NaN()},
	}
	md := r.RenderMarkdown()
	if strings.Contains(md, "**Production-seal artifact pending.**") {
		t.Errorf("production-seal provenance must NOT render the pending callout; got\n%s", md)
	}
	if strings.Contains(md, "operator-action-items.md") {
		t.Errorf("production-seal provenance must NOT render the operator-action-items pointer; got\n%s", md)
	}
}

// _ guards future API surface so an unused-import lint cannot
// hide a renaming break.
var _ fs.FileMode = 0
