// -----------------------------------------------------------------------
// <copyright file="emitter_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package suggest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/suggest"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id
}

// fixture assembles a minimal report.RunArtifact + scopebinding.Table
// + matching steward fixtures. The (Sample, Finding, Task, Threshold,
// Rule) web shares ScopeID/RuleID/SampleID consistently so the join
// path through findingByKey + sampleByID + Thresholds + Rules
// exercises end-to-end.
func fixture(t *testing.T) (report.RunArtifact, *scopebinding.Table, steward.Threshold, steward.Rule) {
	t.Helper()

	repoID := mustUUID(t)
	scopeID := mustUUID(t)
	sampleID := mustUUID(t)
	planID := mustUUID(t)
	taskID := mustUUID(t)
	policyVersionID := mustUUID(t)
	thresholdID := mustUUID(t)

	tbl := scopebinding.NewTable()
	if err := tbl.Insert(scopebinding.ScopeBinding{
		ScopeID:   scopeID,
		ScopeKind: "class",
		FilePath:  "pkg/foo.go",
		StartLine: 1,
		EndLine:   3,
		Signature: "pkg.Foo",
		Language:  "go",
	}); err != nil {
		t.Fatalf("insert binding: %v", err)
	}

	threshold := steward.Threshold{
		ThresholdID: thresholdID,
		MetricKind:  "lcom4",
		ScopeKind:   "class",
		Op:          string(dsl.OpGE),
		Value:       10,
	}
	rule := steward.Rule{
		RuleID:          "solid.srp.lcom4_high",
		Version:         3,
		PackID:          "solid",
		PredicateDSL:    "metric_kind == 'lcom4' AND value >= 10",
		SeverityDefault: steward.SeverityWarn,
		DescriptionMD:   "Split the class along cohesion boundaries (SRP).",
	}

	art := report.RunArtifact{
		Context: repocontext.RepoContext{
			RootPath: "", // overridden per test when snippet is exercised
			RepoID:   repoID,
			HeadSHA:  "deadbeef",
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: policyVersionID,
			Name:            "cleanc-dev-policy",
		},
		Samples: []rule_engine.Sample{{
			Sample: dsl.Sample{
				SampleID:   sampleID,
				RepoID:     repoID,
				SHA:        "deadbeef",
				ScopeID:    scopeID,
				ScopeKind:  "class",
				MetricKind: "lcom4",
				Value:      12,
				HasValue:   true,
			},
			ScopeSignature: "pkg.Foo",
		}},
		Findings: []rule_engine.Finding{{
			FindingID:       mustUUID(t),
			EvaluationRunID: mustUUID(t),
			RepoID:          repoID,
			SHA:             "deadbeef",
			ScopeID:         scopeID,
			RuleID:          rule.RuleID,
			RuleVersion:     rule.Version,
			PolicyVersionID: policyVersionID,
			MetricSampleIDs: []uuid.UUID{sampleID},
			Severity:        steward.SeverityWarn,
		}},
		Tasks: []refactor.RefactorTask{{
			TaskID:        taskID,
			PlanID:        planID,
			ScopeID:       scopeID,
			Kind:          refactor.TaskKindSplitClass,
			EffortHours:   4.5,
			RuleID:        rule.RuleID,
			DescriptionMD: "fallback task prose",
		}},
		Diagnostics: orchestrator.Diagnostics{EffortSource: "fallback"},
	}

	return art, tbl, threshold, rule
}

// withTempRepo writes the fixture file at the path the binding
// references so the default FileSnippetExtractor finds bytes
// to read. Returns the rootPath the caller stamps onto art.Context.
func withTempRepo(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "pkg", "foo.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return root
}

func decodeOne(t *testing.T, raw []byte) suggest.RefactorPromptRecord {
	t.Helper()
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("output missing trailing LF: %q", raw)
	}
	var rec suggest.RefactorPromptRecord
	if err := json.Unmarshal(bytes.TrimRight(raw, "\n"), &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	return rec
}

// =====================================================================
// Happy-path
// =====================================================================

func TestJSONL_Emit_FullyWiredHappyPath(t *testing.T) {
	t.Parallel()
	art, tbl, th, rule := fixture(t)
	art.Context.RootPath = withTempRepo(t, "line1\nline2\nline3\n")

	var buf bytes.Buffer
	em := &suggest.JSONL{
		Bindings:   tbl,
		Rules:      suggest.NewSliceRuleResolver([]steward.Rule{rule}),
		Thresholds: suggest.NewSliceThresholdResolver([]steward.Threshold{th}),
	}
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	rec := decodeOne(t, buf.Bytes())
	task := art.Tasks[0]

	checks := []struct {
		got, want any
		field     string
	}{
		{rec.TaskID, task.TaskID.String(), "TaskID"},
		{rec.PlanID, task.PlanID.String(), "PlanID"},
		{rec.RepoID, art.Context.RepoID.String(), "RepoID"},
		{rec.HeadSHA, "deadbeef", "HeadSHA"},
		{rec.PolicyVersionID, art.Policy.PolicyVersionID.String(), "PolicyVersionID"},
		{rec.TaskKind, string(refactor.TaskKindSplitClass), "TaskKind"},
		{rec.RuleID, rule.RuleID, "RuleID"},
		{rec.RuleVersion, rule.Version, "RuleVersion"},
		{rec.Severity, string(steward.SeverityWarn), "Severity"},
		{rec.Scope.Signature, "pkg.Foo", "Scope.Signature"},
		{rec.Scope.Kind, "class", "Scope.Kind"},
		{rec.Scope.FilePath, "pkg/foo.go", "Scope.FilePath"},
		{rec.Scope.StartLine, 1, "Scope.StartLine"},
		{rec.Scope.EndLine, 3, "Scope.EndLine"},
		{rec.EffortHours, 4.5, "EffortHours"},
		{rec.EffortSource, "fallback", "EffortSource"},
		{rec.PromptFormatVersion, suggest.PromptFormatVersion, "PromptFormatVersion"},
		{rec.ProseSuggestion, rule.DescriptionMD, "ProseSuggestion (from rule)"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}

	if len(rec.MetricEvidence) != 1 {
		t.Fatalf("MetricEvidence len = %d, want 1", len(rec.MetricEvidence))
	}
	ev := rec.MetricEvidence[0]
	if ev.MetricKind != "lcom4" || ev.Value != 12 || ev.Threshold != 10 || ev.Op != ">=" {
		t.Errorf("MetricEvidence mismatch: %+v", ev)
	}

	// Default FileSnippetExtractor read from disk.
	if rec.SourceSnippet == "" {
		t.Errorf("SourceSnippet should be populated by default extractor")
	}
	if !strings.Contains(rec.SourceSnippet, "line1") || !strings.Contains(rec.SourceSnippet, "line3") {
		t.Errorf("SourceSnippet content unexpected: %q", rec.SourceSnippet)
	}
	if rec.SourceSnippetTruncated {
		t.Errorf("SourceSnippetTruncated should be false (fits in cap)")
	}
}

// =====================================================================
// Iter-1 evaluator item 1: default snippet extractor
// =====================================================================

func TestJSONL_Emit_DefaultSnippetExtractorWiredFromRootPath(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	art.Context.RootPath = withTempRepo(t, "package foo\nfunc Bar() {}\nvar X = 1\n")

	var buf bytes.Buffer
	em := &suggest.JSONL{Bindings: tbl}
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rec := decodeOne(t, buf.Bytes())
	if rec.SourceSnippet == "" {
		t.Fatalf("default extractor did not populate SourceSnippet")
	}
}

func TestJSONL_Emit_DisableSnippetDefaultLeavesEmpty(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	art.Context.RootPath = withTempRepo(t, "anything\n")

	var buf bytes.Buffer
	em := &suggest.JSONL{Bindings: tbl, DisableSnippetDefault: true}
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rec := decodeOne(t, buf.Bytes())
	if rec.SourceSnippet != "" {
		t.Errorf("opt-out failed; SourceSnippet=%q", rec.SourceSnippet)
	}
}

func TestJSONL_Emit_ExplicitSnippetExtractorOverridesDefault(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	art.Context.RootPath = withTempRepo(t, "ignored\n")

	em := &suggest.JSONL{
		Bindings: tbl,
		SnippetExtractor: func(suggest.Scope) (string, bool, error) {
			return "<injected>", true, nil
		},
	}
	var buf bytes.Buffer
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rec := decodeOne(t, buf.Bytes())
	if rec.SourceSnippet != "<injected>" || !rec.SourceSnippetTruncated {
		t.Errorf("explicit extractor not honored: %+v", rec)
	}
}

func TestJSONL_Emit_SnippetExtractorErrorPropagates(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	sentinel := errors.New("disk on fire")
	em := &suggest.JSONL{
		Bindings: tbl,
		SnippetExtractor: func(suggest.Scope) (string, bool, error) {
			return "", false, sentinel
		},
	}
	err := em.Emit(context.Background(), art, io.Discard)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Emit returned %v; want wrap of sentinel", err)
	}
}

// =====================================================================
// Iter-1 evaluator item 2: prose_suggestion from Rule.DescriptionMD
// =====================================================================

func TestJSONL_Emit_ProseFromRuleDescriptionMD(t *testing.T) {
	t.Parallel()
	art, tbl, _, rule := fixture(t)
	em := &suggest.JSONL{
		Bindings:              tbl,
		Rules:                 suggest.NewSliceRuleResolver([]steward.Rule{rule}),
		DisableSnippetDefault: true,
	}
	var buf bytes.Buffer
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rec := decodeOne(t, buf.Bytes())
	if rec.ProseSuggestion != rule.DescriptionMD {
		t.Errorf("ProseSuggestion = %q, want rule.DescriptionMD %q",
			rec.ProseSuggestion, rule.DescriptionMD)
	}
}

func TestJSONL_Emit_ProseFallsBackToTaskWhenRuleUnknown(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	// Configure a resolver that knows NO rules — lookup misses.
	em := &suggest.JSONL{
		Bindings:              tbl,
		Rules:                 suggest.NewSliceRuleResolver(nil),
		DisableSnippetDefault: true,
	}
	var buf bytes.Buffer
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rec := decodeOne(t, buf.Bytes())
	if rec.ProseSuggestion != art.Tasks[0].DescriptionMD {
		t.Errorf("expected fallback to task.DescriptionMD %q, got %q",
			art.Tasks[0].DescriptionMD, rec.ProseSuggestion)
	}
}

func TestJSONL_Emit_ProseFallsBackToTaskWhenRulesResolverNil(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	em := &suggest.JSONL{Bindings: tbl, DisableSnippetDefault: true}
	var buf bytes.Buffer
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rec := decodeOne(t, buf.Bytes())
	if rec.ProseSuggestion != art.Tasks[0].DescriptionMD {
		t.Errorf("expected fallback prose, got %q", rec.ProseSuggestion)
	}
}

// =====================================================================
// Iter-1 evaluator item 3: metric_evidence.threshold / op
// =====================================================================

func TestJSONL_Emit_MetricEvidenceCarriesThresholdAndOp(t *testing.T) {
	t.Parallel()
	art, tbl, th, _ := fixture(t)
	em := &suggest.JSONL{
		Bindings:              tbl,
		Thresholds:            suggest.NewSliceThresholdResolver([]steward.Threshold{th}),
		DisableSnippetDefault: true,
	}
	var buf bytes.Buffer
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rec := decodeOne(t, buf.Bytes())
	if len(rec.MetricEvidence) != 1 {
		t.Fatalf("MetricEvidence len = %d, want 1", len(rec.MetricEvidence))
	}
	ev := rec.MetricEvidence[0]
	if ev.MetricKind != "lcom4" || ev.Value != 12 || ev.Threshold != 10 || ev.Op != ">=" {
		t.Errorf("MetricEvidence mismatch: %+v", ev)
	}
}

func TestJSONL_Emit_NoThresholdResolverEmitsEmptyEvidenceNotZeros(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	em := &suggest.JSONL{Bindings: tbl, DisableSnippetDefault: true}

	var buf bytes.Buffer
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rec := decodeOne(t, buf.Bytes())

	// No fabricated 0/"" -- evidence stays empty, never appears
	// as a row with threshold:0,op:"".
	if rec.MetricEvidence == nil || len(rec.MetricEvidence) != 0 {
		t.Errorf("expected empty MetricEvidence (no fabricated zeros); got %+v", rec.MetricEvidence)
	}
	// The JSON literal MUST be `[]`, not `null`, so consumers
	// indexing .metric_evidence[0] don't NPE.
	if !bytes.Contains(buf.Bytes(), []byte(`"metric_evidence":[]`)) {
		t.Errorf("expected `\"metric_evidence\":[]` literal; got %s", buf.String())
	}
	// And critically, no `"threshold":0,"op":""` substring.
	if bytes.Contains(buf.Bytes(), []byte(`"threshold":0,"op":""`)) {
		t.Errorf("output contains silent zero threshold pair: %s", buf.String())
	}
}

func TestJSONL_Emit_ThresholdMissForOneSampleSkipsThatRowOnly(t *testing.T) {
	t.Parallel()
	art, tbl, th, _ := fixture(t)
	// Configure a resolver that returns ok ONLY for some other
	// (metric, scope) pair so the single sample misses.
	em := &suggest.JSONL{
		Bindings: tbl,
		Thresholds: suggest.ThresholdResolverFunc(func(mk, sk string) (string, float64, bool) {
			if mk == "loc" {
				return ">", th.Value, true
			}
			return "", 0, false
		}),
		DisableSnippetDefault: true,
	}
	var buf bytes.Buffer
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rec := decodeOne(t, buf.Bytes())
	if len(rec.MetricEvidence) != 0 {
		t.Fatalf("expected miss to skip the evidence row, got %+v", rec.MetricEvidence)
	}
}

func TestThresholdOpSymbol(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   dsl.ThresholdOp
		want string
	}{
		{dsl.OpGT, ">"},
		{dsl.OpGE, ">="},
		{dsl.OpLT, "<"},
		{dsl.OpLE, "<="},
		{dsl.OpEQ, "=="},
		{dsl.ThresholdOp(">="), ">="}, // idempotent
		{dsl.ThresholdOp("bogus"), ""},
	}
	for _, c := range cases {
		if got := suggest.ThresholdOpSymbol(c.in); got != c.want {
			t.Errorf("ThresholdOpSymbol(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// =====================================================================
// Iter-1 evaluator item 4: task-kind gate
// =====================================================================

func TestJSONL_Emit_FailsClosedOnRejectedTaskKindAlias(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	art.Tasks[0].Kind = refactor.TaskKind("extract_function") // rejected alias
	em := &suggest.JSONL{Bindings: tbl, DisableSnippetDefault: true}

	var buf bytes.Buffer
	err := em.Emit(context.Background(), art, &buf)
	var iae *suggest.InvalidTaskKindError
	if !errors.As(err, &iae) {
		t.Fatalf("Emit returned %v; want *InvalidTaskKindError", err)
	}
	if !errors.Is(err, refactor.ErrRejectedTaskKindAlias) {
		t.Errorf("expected wrap of refactor.ErrRejectedTaskKindAlias; got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected zero bytes on fail-closed; got %d", buf.Len())
	}
}

func TestJSONL_Emit_FailsClosedOnUnknownTaskKind(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	art.Tasks[0].Kind = refactor.TaskKind("frobnicate")
	em := &suggest.JSONL{Bindings: tbl, DisableSnippetDefault: true}

	err := em.Emit(context.Background(), art, io.Discard)
	if !errors.Is(err, refactor.ErrUnknownTaskKind) {
		t.Fatalf("Emit returned %v; want wrap of ErrUnknownTaskKind", err)
	}
}

// =====================================================================
// Iter-1 evaluator item 5: nil writer guard
// =====================================================================

func TestJSONL_Emit_NilWriterReturnsErrNilWriter(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	em := &suggest.JSONL{Bindings: tbl, DisableSnippetDefault: true}
	err := em.Emit(context.Background(), art, nil)
	if !errors.Is(err, suggest.ErrNilWriter) {
		t.Fatalf("Emit(nil writer) = %v, want ErrNilWriter", err)
	}
}

// =====================================================================
// Existing fail-closed contract regressions
// =====================================================================

func TestJSONL_Emit_FailsClosedOnMissingBinding(t *testing.T) {
	t.Parallel()
	art, _, _, _ := fixture(t)
	em := &suggest.JSONL{Bindings: scopebinding.NewTable(), DisableSnippetDefault: true}

	var buf bytes.Buffer
	err := em.Emit(context.Background(), art, &buf)
	var mbe *suggest.MissingScopeBindingError
	if !errors.As(err, &mbe) {
		t.Fatalf("Emit returned %v; want *MissingScopeBindingError", err)
	}
	if mbe.TaskID != art.Tasks[0].TaskID || mbe.ScopeID != art.Tasks[0].ScopeID {
		t.Errorf("MissingScopeBindingError mismatched IDs: %+v", mbe)
	}
	if buf.Len() != 0 {
		t.Errorf("expected zero bytes on fail-closed; got %d", buf.Len())
	}
}

func TestJSONL_Emit_NilBindingsReturnsErrNilBindingTable(t *testing.T) {
	t.Parallel()
	em := &suggest.JSONL{}
	err := em.Emit(context.Background(), report.RunArtifact{}, io.Discard)
	if !errors.Is(err, suggest.ErrNilBindingTable) {
		t.Fatalf("Emit returned %v; want ErrNilBindingTable", err)
	}
}

func TestJSONL_Emit_EmptyTasksProducesEmptyOutput(t *testing.T) {
	t.Parallel()
	em := &suggest.JSONL{Bindings: scopebinding.NewTable(), DisableSnippetDefault: true}
	var buf bytes.Buffer
	if err := em.Emit(context.Background(), report.RunArtifact{}, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty output; got %q", buf.String())
	}
}

func TestJSONL_Emit_HonoursContextCancellation(t *testing.T) {
	t.Parallel()
	art, tbl, _, _ := fixture(t)
	em := &suggest.JSONL{Bindings: tbl, DisableSnippetDefault: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := em.Emit(ctx, art, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Emit returned %v; want context.Canceled", err)
	}
}

func TestJSONL_Emit_DeterministicOutputOnRepeatedInvocations(t *testing.T) {
	t.Parallel()
	art, tbl, th, rule := fixture(t)
	art.Context.RootPath = withTempRepo(t, "a\nb\nc\n")
	em := &suggest.JSONL{
		Bindings:   tbl,
		Rules:      suggest.NewSliceRuleResolver([]steward.Rule{rule}),
		Thresholds: suggest.NewSliceThresholdResolver([]steward.Threshold{th}),
	}

	var first, second bytes.Buffer
	if err := em.Emit(context.Background(), art, &first); err != nil {
		t.Fatalf("first Emit: %v", err)
	}
	if err := em.Emit(context.Background(), art, &second); err != nil {
		t.Fatalf("second Emit: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("Emit is non-deterministic:\n%s\n---\n%s", first.String(), second.String())
	}
}

// =====================================================================
// Constructor
// =====================================================================

func TestNewJSONL_ReturnsEmitterWithBindingsSet(t *testing.T) {
	t.Parallel()
	tbl := scopebinding.NewTable()
	em := suggest.NewJSONL(tbl)
	if em == nil || em.Bindings != tbl {
		t.Fatalf("NewJSONL did not wire Bindings: %+v", em)
	}
}
