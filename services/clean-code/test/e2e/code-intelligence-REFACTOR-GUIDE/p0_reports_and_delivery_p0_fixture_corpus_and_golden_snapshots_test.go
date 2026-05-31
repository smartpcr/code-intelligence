//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="p0_reports_and_delivery_p0_fixture_corpus_and_golden_snapshots_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/walk"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	rule_engine "github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// ---------------------------------------------------------------------------
// Deterministic clock and UUID factory (mirrors orchestrator_golden_test.go)
// ---------------------------------------------------------------------------

var goldenFixedTime = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

func goldenClock() time.Time { return goldenFixedTime }

func goldenUUIDFactory(namespace string) func() (uuid.UUID, error) {
	var n int
	return func() (uuid.UUID, error) {
		n++
		return uuid.NewV5(uuid.NamespaceURL, fmt.Sprintf("%s/%06d", namespace, n)), nil
	}
}

// ---------------------------------------------------------------------------
// Fixture row definition
// ---------------------------------------------------------------------------

type fixtureRow struct {
	scenario          string
	fixtureDir        string
	lang              string
	syntheticRepoPath string
}

var goldenFixtureRows = []fixtureRow{
	{scenario: "p0-go-cycle", fixtureDir: "go", lang: parser.LanguageGo, syntheticRepoPath: "/fixtures/go"},
	{scenario: "p0-python-cycle", fixtureDir: "python", lang: parser.LanguagePython, syntheticRepoPath: "/fixtures/python"},
	{scenario: "p0-typescript-cycle", fixtureDir: "typescript", lang: parser.LanguageTypeScript, syntheticRepoPath: "/fixtures/typescript"},
	{scenario: "p0-java-cycle", fixtureDir: "java", lang: parser.LanguageJava, syntheticRepoPath: "/fixtures/java"},
}

// ---------------------------------------------------------------------------
// Per-scenario state
// ---------------------------------------------------------------------------

type goldenSnapshotState struct {
	moduleRoot string

	// Single-fixture state (Go scenarios).
	singleArt        report.RunArtifact
	singleMarkdown   []byte
	singleFindings   []byte

	// Multi-language state (cross-language scenario).
	multiArtifacts map[string]report.RunArtifact
	multiMarkdown  map[string][]byte

	// Deterministic re-run state.
	rerunMarkdown1  []byte
	rerunFindings1  []byte
	rerunMarkdown2  []byte
	rerunFindings2  []byte
}

func newGoldenSnapshotState() *goldenSnapshotState {
	return &goldenSnapshotState{
		multiArtifacts: make(map[string]report.RunArtifact),
		multiMarkdown:  make(map[string][]byte),
	}
}

// ---------------------------------------------------------------------------
// Pipeline helper (mirrors orchestrator_golden_test.go runGoldenPipeline)
// ---------------------------------------------------------------------------

func (s *goldenSnapshotState) resolveModuleRoot() string {
	if s.moduleRoot != "" {
		return s.moduleRoot
	}
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			s.moduleRoot = dir
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not locate go.mod above test file")
		}
		dir = parent
	}
}

func (s *goldenSnapshotState) runPipeline(row fixtureRow) (report.RunArtifact, []byte, []byte, error) {
	root := s.resolveModuleRoot()
	fixturePath := filepath.Join(root, "internal", "cli", "testdata", "fixtures", row.fixtureDir)
	absFixturePath, err := filepath.Abs(fixturePath)
	if err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("abs fixture path: %w", err)
	}

	ctx := context.Background()
	repoCtx := repocontext.RepoContext{
		RootPath:   row.syntheticRepoPath,
		RepoID:     repocontext.MintRepoID(row.syntheticRepoPath),
		HeadSHA:    repocontext.HeadSHAWorkingCopySentinel,
		ModulePath: repocontext.DetectModulePath(absFixturePath, row.lang),
		IsGitRepo:  false,
	}

	orch := orchestrator.New(orchestrator.Options{Workers: 1})
	result, err := orch.Run(ctx, repoCtx, absFixturePath)
	if err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("orchestrator run: %w", err)
	}

	bundle, err := devpolicy.NewLoader().Load(ctx, devpolicy.LoaderSource{UseEmbedded: true})
	if err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("load dev policy: %w", err)
	}
	pinGoldenBundleTimes(&bundle)

	samples := orchestrator.BuildSamples(repoCtx, result.Drafts, orch.ScopeBindings(), result.ScopeIDs)
	store, err := orchestrator.LoadStore(bundle, samples, repoCtx)
	if err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("load store: %w", err)
	}
	store.SetClock(goldenClock)

	engineIDs := goldenUUIDFactory("golden/rule-engine/" + row.syntheticRepoPath)
	engine, err := rule_engine.New(rule_engine.Config{Store: store, Clock: goldenClock, NewID: engineIDs})
	if err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("rule engine init: %w", err)
	}
	runRes, err := engine.RunBatch(ctx, repoCtx.RepoID, repoCtx.HeadSHA, bundle.PolicyVersion.PolicyVersionID)
	if err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("rule engine run: %w", err)
	}
	evalRun, verdict := goldenLookupRunAndVerdict(store, runRes)
	findings := store.Findings()

	policyR := orchestrator.NewCLIPolicyReader(bundle)
	metricsR := orchestrator.BuildMetricSampleReader(samples)
	findingsR := orchestrator.BuildFindingReader(findings)
	hotSpotWriter := refactor.NewInMemoryHotSpotWriter()
	planner, err := refactor.NewPlanner(policyR, metricsR, findingsR, hotSpotWriter,
		refactor.WithIDFactory(goldenUUIDFactory("golden/hotspot/"+row.syntheticRepoPath)),
		refactor.WithClock(goldenClock),
	)
	if err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("planner init: %w", err)
	}
	planRes, err := planner.Plan(ctx, repoCtx.RepoID, repoCtx.HeadSHA)
	if err != nil && !errors.Is(err, refactor.ErrNoActivePolicy) {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("planner run: %w", err)
	}

	plans, tasks, err := goldenRunTaskPlanner(ctx, bundle, planRes, findings, repoCtx, row.syntheticRepoPath)
	if err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("task planner: %w", err)
	}
	var refactorPlan refactor.RefactorPlan
	if len(plans) > 0 {
		refactorPlan = plans[0]
	}

	art := report.RunArtifact{
		SchemaVersion: report.SchemaVersionCurrent,
		Context:       repoCtx,
		Policy:        bundle.PolicyVersion,
		Files:         goldenBuildFileSummaries(result),
		Skips:         result.Skips,
		DarkMetrics:   result.Diagnostics.DarkMetrics,
		Samples:       samples,
		Run:           evalRun,
		Verdict:       verdict,
		Findings:      findings,
		HotSpots:      planRes.HotSpots,
		Plan:          refactorPlan,
		Tasks:         tasks,
		Diagnostics:   result.Diagnostics,
	}

	var mdBuf bytes.Buffer
	if err := (report.Markdown{}).Render(ctx, art, &mdBuf); err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("render markdown: %w", err)
	}
	var findBuf bytes.Buffer
	if err := (report.JSON{}).Render(ctx, art, &findBuf); err != nil {
		return report.RunArtifact{}, nil, nil, fmt.Errorf("render findings json: %w", err)
	}

	return art, mdBuf.Bytes(), findBuf.Bytes(), nil
}

func pinGoldenBundleTimes(bundle *devpolicy.Bundle) {
	bundle.PolicyVersion.CreatedAt = goldenFixedTime
	for i := range bundle.RulePacks {
		bundle.RulePacks[i].CreatedAt = goldenFixedTime
	}
	for i := range bundle.Rules {
		bundle.Rules[i].CreatedAt = goldenFixedTime
	}
	for i := range bundle.Thresholds {
		bundle.Thresholds[i].CreatedAt = goldenFixedTime
	}
}

func goldenLookupRunAndVerdict(store *rule_engine.InMemoryStore, res rule_engine.RunResult) (rule_engine.EvaluationRun, rule_engine.EvaluationVerdict) {
	var run rule_engine.EvaluationRun
	for _, r := range store.Runs() {
		if r.EvaluationRunID == res.EvaluationRunID {
			run = r
			break
		}
	}
	var verdict rule_engine.EvaluationVerdict
	for _, v := range store.Verdicts() {
		if v.VerdictID == res.EvaluationVerdictID {
			verdict = v
			break
		}
	}
	if verdict.Verdict == "" && res.Verdict != "" {
		verdict.Verdict = res.Verdict
		verdict.VerdictID = res.EvaluationVerdictID
		verdict.EvaluationRunID = res.EvaluationRunID
	}
	return run, verdict
}

func goldenRunTaskPlanner(ctx context.Context, bundle devpolicy.Bundle, planRes refactor.PlanResult, findings []rule_engine.Finding, repoCtx repocontext.RepoContext, syntheticRepoPath string) ([]refactor.RefactorPlan, []refactor.RefactorTask, error) {
	if planRes.Snapshot.PolicyVersionID.String() == "00000000-0000-0000-0000-000000000000" {
		return nil, nil, nil
	}
	tp, writer, err := orchestrator.NewTaskPlannerWiring(bundle, planRes.HotSpots, findings,
		refactor.WithTaskIDFactory(goldenUUIDFactory("golden/task/"+syntheticRepoPath)),
		refactor.WithTaskClock(goldenClock),
		refactor.WithRuleKindMapper(func(ruleID string) (refactor.TaskKind, bool) {
			if strings.Contains(ruleID, "cycle_member") {
				return refactor.TaskKindBreakCycle, true
			}
			return refactor.DefaultTaskKindForRule(ruleID)
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("NewTaskPlannerWiring: %w", err)
	}
	if _, err := tp.PlanFromSnapshot(ctx, repoCtx.RepoID, repoCtx.HeadSHA, planRes.Snapshot); err != nil {
		return nil, nil, fmt.Errorf("PlanFromSnapshot: %w", err)
	}
	return writer.Plans(), writer.Tasks(), nil
}

func goldenBuildFileSummaries(result *orchestrator.Result) []report.WalkedFileSummary {
	if result == nil {
		return nil
	}
	out := make([]report.WalkedFileSummary, 0, len(result.Files)+len(result.Skips))
	for _, f := range result.Files {
		out = append(out, report.WalkedFileSummary{
			Path:        f.GetPath(),
			Language:    f.GetLanguage(),
			SizeBytes:   int64(len(f.GetAttrs()[parser.AttrSourceBytes])),
			ParseStatus: "parsed",
		})
	}
	for _, sk := range result.Skips {
		out = append(out, report.WalkedFileSummary{Path: sk.Path, ParseStatus: goldenSkipReasonToParseStatus(sk.Reason)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func goldenSkipReasonToParseStatus(reason string) string {
	switch reason {
	case orchestrator.SkipReasonParserError, orchestrator.SkipReasonScopeBindingError:
		return "parser_error"
	case orchestrator.SkipReasonParserPanic:
		return "parser_panic"
	case walk.SkipReasonDirectory,
		walk.SkipReasonGitignore,
		walk.SkipReasonSizeCap,
		walk.SkipReasonUnsupportedLanguage,
		walk.SkipReasonSymlinkLoop,
		walk.SkipReasonSymlink:
		return "skipped"
	default:
		return "skipped"
	}
}

func (s *goldenSnapshotState) goldenDir(scenario string) string {
	return filepath.Join(s.resolveModuleRoot(), "internal", "cli", "testdata", "golden", scenario)
}

func (s *goldenSnapshotState) readGolden(scenario, filename string) ([]byte, error) {
	path := filepath.Join(s.goldenDir(scenario), filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read golden %s/%s: %w (run UPDATE=1 go test -run TestAnalyzeGolden ./internal/cli/orchestrator/...)", scenario, filename, err)
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *goldenSnapshotState) theGoFixtureIsLoaded() error {
	_ = s.resolveModuleRoot()
	return nil
}

func (s *goldenSnapshotState) theFourLanguageFixtureSetIsLoaded() error {
	_ = s.resolveModuleRoot()
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *goldenSnapshotState) runAnalyzeRunsAgainstTheGoFixture() error {
	row := goldenFixtureRows[0] // Go
	art, md, findings, err := s.runPipeline(row)
	if err != nil {
		return fmt.Errorf("runAnalyze (Go): %w", err)
	}
	s.singleArt = art
	s.singleMarkdown = md
	s.singleFindings = findings
	return nil
}

func (s *goldenSnapshotState) runAnalyzeRunsSequentiallyPerLanguage() error {
	for _, row := range goldenFixtureRows {
		art, md, _, err := s.runPipeline(row)
		if err != nil {
			return fmt.Errorf("runAnalyze (%s): %w", row.scenario, err)
		}
		s.multiArtifacts[row.scenario] = art
		s.multiMarkdown[row.scenario] = md
	}
	return nil
}

func (s *goldenSnapshotState) runAnalyzeRunsTwiceBackToBack() error {
	row := goldenFixtureRows[0] // Go
	_, md1, find1, err := s.runPipeline(row)
	if err != nil {
		return fmt.Errorf("runAnalyze run-1: %w", err)
	}
	s.rerunMarkdown1 = md1
	s.rerunFindings1 = find1

	_, md2, find2, err := s.runPipeline(row)
	if err != nil {
		return fmt.Errorf("runAnalyze run-2: %w", err)
	}
	s.rerunMarkdown2 = md2
	s.rerunFindings2 = find2
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *goldenSnapshotState) reportMDByteMatchesTheGoldenFileFor(scenario string) error {
	want, err := s.readGolden(scenario, "report.md")
	if err != nil {
		return err
	}
	if !bytes.Equal(want, s.singleMarkdown) {
		return fmt.Errorf("report.md golden mismatch for %s\n%s", scenario, goldenUnifiedDiff(string(want), string(s.singleMarkdown), 30))
	}
	return nil
}

func (s *goldenSnapshotState) findingsJSONByteMatchesTheGoldenFileFor(scenario string) error {
	want, err := s.readGolden(scenario, "findings.json")
	if err != nil {
		return err
	}
	if !bytes.Equal(want, s.singleFindings) {
		return fmt.Errorf("findings.json golden mismatch for %s\n%s", scenario, goldenUnifiedDiff(string(want), string(s.singleFindings), 30))
	}
	return nil
}

func (s *goldenSnapshotState) findingsJSONFindingsContainsCycleMember(ruleSubstring string) error {
	for _, f := range s.singleArt.Findings {
		if strings.Contains(f.RuleID, ruleSubstring) {
			return nil
		}
	}
	ruleIDs := make([]string, 0, len(s.singleArt.Findings))
	for _, f := range s.singleArt.Findings {
		ruleIDs = append(ruleIDs, f.RuleID)
	}
	return fmt.Errorf("no finding with RuleID matching %q; found rules: %v", ruleSubstring, ruleIDs)
}

func (s *goldenSnapshotState) atLeastOneRefactorTaskHasKind(kind string) error {
	for _, task := range s.singleArt.Tasks {
		if string(task.Kind) == kind {
			return nil
		}
	}
	kinds := make([]string, 0, len(s.singleArt.Tasks))
	for _, task := range s.singleArt.Tasks {
		kinds = append(kinds, string(task.Kind))
	}
	return fmt.Errorf("no RefactorTask with Kind %q; found kinds: %v", kind, kinds)
}

func (s *goldenSnapshotState) eachLanguagesReportMDByteMatchesItsGoldenFile() error {
	for _, row := range goldenFixtureRows {
		want, err := s.readGolden(row.scenario, "report.md")
		if err != nil {
			return err
		}
		got, ok := s.multiMarkdown[row.scenario]
		if !ok {
			return fmt.Errorf("no markdown produced for %s", row.scenario)
		}
		if !bytes.Equal(want, got) {
			return fmt.Errorf("report.md golden mismatch for %s\n%s", row.scenario, goldenUnifiedDiff(string(want), string(got), 30))
		}
	}
	return nil
}

func (s *goldenSnapshotState) bothRunsProduceByteIdenticalOutputs() error {
	if !bytes.Equal(s.rerunMarkdown1, s.rerunMarkdown2) {
		return fmt.Errorf("report.md differs between run-1 and run-2\n%s", goldenUnifiedDiff(string(s.rerunMarkdown1), string(s.rerunMarkdown2), 30))
	}
	if !bytes.Equal(s.rerunFindings1, s.rerunFindings2) {
		return fmt.Errorf("findings.json differs between run-1 and run-2\n%s", goldenUnifiedDiff(string(s.rerunFindings1), string(s.rerunFindings2), 30))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Diff helper
// ---------------------------------------------------------------------------

func goldenUnifiedDiff(want, got string, maxLines int) string {
	wantLines := strings.SplitAfter(want, "\n")
	gotLines := strings.SplitAfter(got, "\n")
	if len(wantLines) > 0 && wantLines[len(wantLines)-1] == "" {
		wantLines = wantLines[:len(wantLines)-1]
	}
	if len(gotLines) > 0 && gotLines[len(gotLines)-1] == "" {
		gotLines = gotLines[:len(gotLines)-1]
	}
	var b strings.Builder
	b.WriteString("--- golden\n+++ actual\n")
	limit := len(wantLines)
	if len(gotLines) > limit {
		limit = len(gotLines)
	}
	emitted := 0
	for i := 0; i < limit && emitted < maxLines; i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w == g {
			continue
		}
		b.WriteString(fmt.Sprintf("@@ line %d @@\n", i+1))
		if w != "" {
			b.WriteString("-" + goldenEnsureLine(w))
		}
		if g != "" {
			b.WriteString("+" + goldenEnsureLine(g))
		}
		emitted += 3
	}
	if emitted >= maxLines {
		b.WriteString("... diff truncated ...\n")
	}
	return b.String()
}

func goldenEnsureLine(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// ---------------------------------------------------------------------------
// Scenario initializer and test entry point
// ---------------------------------------------------------------------------

func InitializeScenario_p0_reports_and_delivery_p0_fixture_corpus_and_golden_snapshots(ctx *godog.ScenarioContext) {
	s := newGoldenSnapshotState()

	// Given
	ctx.Step(`^the Go fixture is loaded$`, s.theGoFixtureIsLoaded)
	ctx.Step(`^the four-language fixture set is loaded$`, s.theFourLanguageFixtureSetIsLoaded)

	// When
	ctx.Step(`^runAnalyze runs against the Go fixture$`, s.runAnalyzeRunsAgainstTheGoFixture)
	ctx.Step(`^runAnalyze runs sequentially per language$`, s.runAnalyzeRunsSequentiallyPerLanguage)
	ctx.Step(`^runAnalyze runs twice back-to-back$`, s.runAnalyzeRunsTwiceBackToBack)

	// Then
	ctx.Step(`^report\.md byte-matches the golden file for "([^"]*)"$`, s.reportMDByteMatchesTheGoldenFileFor)
	ctx.Step(`^findings\.json byte-matches the golden file for "([^"]*)"$`, s.findingsJSONByteMatchesTheGoldenFileFor)
	ctx.Step(`^findings\.json Findings contains at least one row with RuleID matching "([^"]*)"$`, s.findingsJSONFindingsContainsCycleMember)
	ctx.Step(`^at least one RefactorTask has Kind "([^"]*)"$`, s.atLeastOneRefactorTaskHasKind)
	ctx.Step(`^each language's report\.md byte-matches its golden file$`, s.eachLanguagesReportMDByteMatchesItsGoldenFile)
	ctx.Step(`^both runs produce byte-identical report\.md and findings\.json outputs$`, s.bothRunsProduceByteIdenticalOutputs)
}

func TestE2E_p0_reports_and_delivery_p0_fixture_corpus_and_golden_snapshots(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_p0_reports_and_delivery_p0_fixture_corpus_and_golden_snapshots,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"p0_reports_and_delivery_p0_fixture_corpus_and_golden_snapshots.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
