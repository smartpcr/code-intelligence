//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/suggest"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// promptRecordState holds per-scenario state for snippet extractor
// and metric-evidence-join e2e scenarios.
type promptRecordState struct {
	fixtureDir  string
	fixturePath string
	maxLines    int
	startLine   int
	endLine     int
	snippet     string
	truncated   bool
	err         error

	// rawContent is the exact bytes written to disk for
	// the raw-bytes scenario so the assertion can compare.
	rawContent string

	// record holds the assembled RefactorPromptRecord for
	// the metric-evidence-join scenario.
	record suggest.RefactorPromptRecord

	// engine-level state for the metric evidence join scenario
	store           *rule_engine.InMemoryStore
	engine          *rule_engine.Engine
	repoID          uuid.UUID
	sha             string
	scopeID         uuid.UUID
	policyVersionID uuid.UUID
	thresholdID     uuid.UUID
	sampleID        uuid.UUID
	thresholdValue  float64
	thresholdOp     string // symbol form: ">="
	thresholdOpDSL  string // DSL form: "ge"
	findings        []rule_engine.Finding
	samples         []rule_engine.Sample
}

func newPromptRecordState() *promptRecordState {
	return &promptRecordState{}
}

func (s *promptRecordState) cleanup() {
	if s.fixtureDir != "" {
		os.RemoveAll(s.fixtureDir)
	}
}

// writeFixture creates a temp file with the given content and
// records fixtureDir / fixturePath.
func (s *promptRecordState) writeFixture(name, content string) error {
	dir, err := os.MkdirTemp("", "snippet-e2e-*")
	if err != nil {
		return err
	}
	s.fixtureDir = dir
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		return err
	}
	s.fixturePath = p
	return nil
}

// --- helpers: op translation ---------------------------------------------

// opDSLToSymbol translates the DSL threshold-op (e.g. "ge") to
// the wire-format symbol (e.g. ">=") used by MetricEvidence.Op.
func opDSLToSymbol(op string) string {
	switch dsl.ThresholdOp(op) {
	case dsl.OpGT:
		return ">"
	case dsl.OpGE:
		return ">="
	case dsl.OpLT:
		return "<"
	case dsl.OpLE:
		return "<="
	case dsl.OpEQ:
		return "=="
	default:
		return op
	}
}

// opSymbolToDSL translates a symbol op (e.g. ">=") to the DSL
// enum label (e.g. "ge") stored in steward.Threshold.Op.
func opSymbolToDSL(op string) string {
	switch op {
	case ">":
		return string(dsl.OpGT)
	case ">=":
		return string(dsl.OpGE)
	case "<":
		return string(dsl.OpLT)
	case "<=":
		return string(dsl.OpLE)
	case "==":
		return string(dsl.OpEQ)
	default:
		return op
	}
}

// deterministicIDGen returns a uuid generator that produces
// strictly increasing deterministic values for test stability.
func promptRecordDeterministicIDGen() func() (uuid.UUID, error) {
	var (
		mu      sync.Mutex
		counter uint64
	)
	return func() (uuid.UUID, error) {
		mu.Lock()
		counter++
		c := counter
		mu.Unlock()
		var id uuid.UUID
		for i := 0; i < 8; i++ {
			id[8+i] = byte(c >> (8 * (7 - i)))
		}
		return id, nil
	}
}

// --- Given steps ---------------------------------------------------------

func (s *promptRecordState) a500LineFixtureFile() error {
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "line%03d\n", i)
	}
	return s.writeFixture("big.txt", sb.String())
}

func (s *promptRecordState) a50LineFixtureFile() error {
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "line%02d\n", i)
	}
	return s.writeFixture("small.txt", sb.String())
}

func (s *promptRecordState) maxLinesIsSetTo(n int) error {
	s.maxLines = n
	return nil
}

func (s *promptRecordState) aFixtureFileContainingTabAndUTF8() error {
	s.rawContent = "\t日本語 // コメント\n  spaced\t/* c */  \n"
	return s.writeFixture("utf8.go", s.rawContent)
}

func (s *promptRecordState) aRuleEngineStoreWithLocThreshold(thresholdVal float64, op string) error {
	s.thresholdValue = thresholdVal
	s.thresholdOp = op
	s.thresholdOpDSL = opSymbolToDSL(op)

	s.store = rule_engine.NewInMemoryStore()
	s.repoID = uuid.Must(uuid.NewV4())
	s.sha = "abc123deadbeef"
	s.scopeID = uuid.Must(uuid.NewV4())
	s.thresholdID = uuid.Must(uuid.NewV4())
	s.policyVersionID = uuid.Must(uuid.NewV4())

	// Seed threshold: loc >= 1500
	s.store.InsertThreshold(steward.Threshold{
		ThresholdID: s.thresholdID,
		MetricKind:  "loc",
		ScopeKind:   "method",
		Op:          s.thresholdOpDSL,
		Value:       thresholdVal,
		CreatedAt:   time.Now(),
	})

	// Seed rule: fires when loc crosses the threshold
	ruleID := "complexity.loc_high"
	s.store.InsertRule(steward.Rule{
		RuleID:          ruleID,
		Version:         1,
		PackID:          "complexity",
		PredicateDSL:    "threshold('" + s.thresholdID.String() + "')",
		SeverityDefault: steward.SeverityWarn,
		DescriptionMD:   "Function LOC exceeds the complexity threshold.",
		CreatedAt:       time.Now(),
	})

	// Seed policy version referencing the rule and threshold
	freshness := 3600
	s.store.InsertPolicyVersion(steward.PolicyVersion{
		PolicyVersionID: s.policyVersionID,
		Name:            "e2e-loc-policy",
		RuleRefs:        []steward.RuleRef{{RuleID: ruleID, Version: 1}},
		ThresholdRefs:   []steward.ThresholdRef{{ThresholdID: s.thresholdID}},
		RefactorWeights: steward.RefactorWeights{
			Alpha:                  0.4,
			Beta:                   0.3,
			Gamma:                  0.2,
			Delta:                  0.1,
			EffortModelVersion:     "v0",
			WindowDays:             30,
			FreshnessWindowSeconds: &freshness,
		},
		Signature: []byte("e2e-test-signature"),
		CreatedAt: time.Now(),
	})

	// Create engine
	var err error
	s.engine, err = rule_engine.New(rule_engine.Config{
		Store: s.store,
		Cache: dsl.NewCache(),
		Clock: func() time.Time {
			return time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
		},
		NewID: promptRecordDeterministicIDGen(),
	})
	if err != nil {
		return fmt.Errorf("rule_engine.New: %w", err)
	}

	return nil
}

func (s *promptRecordState) aMetricSampleWithKindAndValue(metricKind string, value float64) error {
	s.sampleID = uuid.Must(uuid.NewV4())
	sample := rule_engine.Sample{
		Sample: dsl.Sample{
			SampleID:      s.sampleID,
			RepoID:        s.repoID,
			SHA:           s.sha,
			ScopeID:       s.scopeID,
			ScopeKind:     "method",
			MetricKind:    metricKind,
			MetricVersion: 1,
			Value:         value,
			HasValue:      true,
			Pack:          "complexity",
			Source:        "computed",
		},
		ScopeSignature: "com.example.BigFunction_" + s.scopeID.String()[:8],
	}
	s.samples = []rule_engine.Sample{sample}
	s.store.InsertSamples(s.repoID, s.sha, s.samples)
	return nil
}

// --- When steps ----------------------------------------------------------

func (s *promptRecordState) extractSnippetRunsOverLinesToEndLine(start, end int) error {
	s.startLine = start
	s.endLine = end
	s.snippet, s.truncated, s.err = suggest.ExtractSnippet(
		s.fixturePath, s.startLine, s.endLine, s.maxLines,
	)
	return s.err
}

func (s *promptRecordState) extractSnippetRunsOverFullFileRange() error {
	lineCount := strings.Count(s.rawContent, "\n")
	if !strings.HasSuffix(s.rawContent, "\n") {
		lineCount++
	}
	s.startLine = 1
	s.endLine = lineCount
	s.maxLines = lineCount + 100
	s.snippet, s.truncated, s.err = suggest.ExtractSnippet(
		s.fixturePath, s.startLine, s.endLine, s.maxLines,
	)
	return s.err
}

func (s *promptRecordState) theRuleEngineRunsAndProducesAFinding() error {
	ctx := context.Background()
	_, err := s.engine.RunBatch(ctx, s.repoID, s.sha, s.policyVersionID)
	if err != nil {
		return fmt.Errorf("RunBatch: %w", err)
	}
	s.findings = s.store.Findings()
	if len(s.findings) == 0 {
		return fmt.Errorf("engine produced zero findings; expected at least one for loc=2000 >= 1500")
	}
	return nil
}

func (s *promptRecordState) theAggregatorJoinsTheFindingWithMetricSamplesAndThreshold() error {
	// Pick the first finding that carries MetricSampleIDs
	var finding rule_engine.Finding
	found := false
	for _, f := range s.findings {
		if len(f.MetricSampleIDs) > 0 {
			finding = f
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no finding with MetricSampleIDs found among %d findings", len(s.findings))
	}

	// Build a sample lookup map
	sampleMap := make(map[uuid.UUID]rule_engine.Sample, len(s.samples))
	for _, sm := range s.samples {
		sampleMap[sm.SampleID] = sm
	}

	// Join: for each MetricSampleID in the finding, look up the
	// sample and the threshold to assemble MetricEvidence.
	var evidence []suggest.MetricEvidence
	for _, sampleID := range finding.MetricSampleIDs {
		sm, ok := sampleMap[sampleID]
		if !ok {
			return fmt.Errorf("sample %s referenced by finding but not in sample map", sampleID)
		}
		// Look up the threshold for this metric_kind
		ctx := context.Background()
		th, err := s.store.GetThreshold(ctx, s.thresholdID)
		if err != nil {
			return fmt.Errorf("GetThreshold: %w", err)
		}
		evidence = append(evidence, suggest.MetricEvidence{
			MetricKind: sm.MetricKind,
			Value:      sm.Value,
			Threshold:  th.Value,
			Op:         opDSLToSymbol(th.Op),
		})
	}

	s.record = suggest.RefactorPromptRecord{
		TaskID:              finding.FindingID.String(),
		PlanID:              "plan-e2e",
		RepoID:              finding.RepoID.String(),
		HeadSHA:             finding.SHA,
		PolicyVersionID:     finding.PolicyVersionID.String(),
		RuleID:              finding.RuleID,
		RuleVersion:         finding.RuleVersion,
		Severity:            string(finding.Severity),
		MetricEvidence:      evidence,
		PromptFormatVersion: suggest.PromptFormatVersion,
	}
	return nil
}

// --- Then steps ----------------------------------------------------------

func (s *promptRecordState) theReturnedStringHasExactlyNLines(n int) error {
	got := countSnippetLines(s.snippet)
	if got != n {
		return fmt.Errorf("expected %d lines, got %d", n, got)
	}
	return nil
}

func (s *promptRecordState) truncatedIsTrue() error {
	if !s.truncated {
		return fmt.Errorf("expected truncated=true, got false")
	}
	return nil
}

func (s *promptRecordState) truncatedIsFalse() error {
	if s.truncated {
		return fmt.Errorf("expected truncated=false, got true")
	}
	return nil
}

func (s *promptRecordState) theLastLineIs(want string) error {
	lines := splitSnippetLines(s.snippet)
	if len(lines) == 0 {
		return fmt.Errorf("snippet is empty")
	}
	last := lines[len(lines)-1]
	if last != want {
		return fmt.Errorf("last line = %q, want %q", last, want)
	}
	return nil
}

func (s *promptRecordState) theSnippetContainsExactlyNLines(n int) error {
	return s.theReturnedStringHasExactlyNLines(n)
}

func (s *promptRecordState) theReturnedSnippetPreservesExactByteSequence() error {
	if s.snippet != s.rawContent {
		return fmt.Errorf(
			"raw bytes not preserved.\n got=%q\nwant=%q",
			s.snippet, s.rawContent,
		)
	}
	return nil
}

func (s *promptRecordState) metricEvidenceContainsExactlyNEntries(n int) error {
	got := len(s.record.MetricEvidence)
	if got != n {
		return fmt.Errorf("expected %d metric_evidence entries, got %d", n, got)
	}
	return nil
}

func (s *promptRecordState) theEntryHas(
	metricKind string, value float64, threshold float64, op string,
) error {
	if len(s.record.MetricEvidence) == 0 {
		return fmt.Errorf("metric_evidence is empty")
	}
	e := s.record.MetricEvidence[0]
	if e.MetricKind != metricKind {
		return fmt.Errorf("metric_kind = %q, want %q", e.MetricKind, metricKind)
	}
	if e.Value != value {
		return fmt.Errorf("value = %v, want %v", e.Value, value)
	}
	if e.Threshold != threshold {
		return fmt.Errorf("threshold = %v, want %v", e.Threshold, threshold)
	}
	if e.Op != op {
		return fmt.Errorf("op = %q, want %q", e.Op, op)
	}
	return nil
}

// --- helpers -------------------------------------------------------------

// countSnippetLines counts lines in a snippet. A line is
// terminated by '\n' or is a non-empty tail without one.
func countSnippetLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// splitSnippetLines splits on '\n' and strips trailing empty
// element when the input ends with '\n'.
func splitSnippetLines(s string) []string {
	if s == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(s, "\n")
	parts := strings.Split(trimmed, "\n")
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}

// --- godog wiring --------------------------------------------------------

func InitializeScenario_p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor(ctx *godog.ScenarioContext) {
	s := newPromptRecordState()

	ctx.After(func(ctx2 context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a 500-line fixture file$`, s.a500LineFixtureFile)
	ctx.Step(`^a 50-line fixture file$`, s.a50LineFixtureFile)
	ctx.Step(`^maxLines is set to (\d+)$`, s.maxLinesIsSetTo)
	ctx.Step(`^a fixture file containing a literal tab followed by a multi-byte UTF-8 sequence$`, s.aFixtureFileContainingTabAndUTF8)
	ctx.Step(`^a rule engine store with a loc threshold of (\d+) and op "([^"]*)"$`, s.aRuleEngineStoreWithLocThreshold)
	ctx.Step(`^a metric sample with metric_kind "([^"]*)" and value (\d+)$`, s.aMetricSampleWithKindAndValue)

	// When
	ctx.Step(`^ExtractSnippet runs over lines (\d+) to (\d+)$`, s.extractSnippetRunsOverLinesToEndLine)
	ctx.Step(`^ExtractSnippet runs over the full file range$`, s.extractSnippetRunsOverFullFileRange)
	ctx.Step(`^the rule engine runs and produces a finding$`, s.theRuleEngineRunsAndProducesAFinding)
	ctx.Step(`^the aggregator joins the finding with its metric samples and threshold$`, s.theAggregatorJoinsTheFindingWithMetricSamplesAndThreshold)

	// Then
	ctx.Step(`^the returned string has exactly (\d+) lines$`, s.theReturnedStringHasExactlyNLines)
	ctx.Step(`^truncated is true$`, s.truncatedIsTrue)
	ctx.Step(`^truncated is false$`, s.truncatedIsFalse)
	ctx.Step(`^the last line is "([^"]*)"$`, s.theLastLineIs)
	ctx.Step(`^the snippet contains exactly (\d+) lines$`, s.theSnippetContainsExactlyNLines)
	ctx.Step(`^the returned snippet preserves the exact byte sequence$`, s.theReturnedSnippetPreservesExactByteSequence)
	ctx.Step(`^metric_evidence contains exactly (\d+) entry$`, s.metricEvidenceContainsExactlyNEntries)
	ctx.Step(`^the entry has metric_kind "([^"]*)" value (\d+) threshold (\d+) op "([^"]*)"$`, s.theEntryHas)
}

func TestE2E_p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
