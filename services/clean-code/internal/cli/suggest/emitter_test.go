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

// must wraps uuid.NewV4 for terser fixture construction.
func must(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id
}

// fixtureArtifact assembles a minimal report.RunArtifact +
// scopebinding.Table consistent enough for the emitter to
// resolve every task. The (Sample, Finding, Task) triple
// shares the same ScopeID/RuleID/SampleID web so the join
// path through findingByKey + sampleByID exercises end-to-end.
func fixtureArtifact(t *testing.T) (report.RunArtifact, *scopebinding.Table) {
	t.Helper()

	repoID := must(t)
	scopeID := must(t)
	sampleID := must(t)
	planID := must(t)
	taskID := must(t)
	policyVersionID := must(t)

	tbl := scopebinding.NewTable()
	if err := tbl.Insert(scopebinding.ScopeBinding{
		ScopeID:   scopeID,
		ScopeKind: "class",
		FilePath:  "pkg/foo.go",
		StartLine: 10,
		EndLine:   42,
		Signature: "pkg.Foo",
		Language:  "go",
	}); err != nil {
		t.Fatalf("insert binding: %v", err)
	}

	art := report.RunArtifact{
		Context: repocontext.RepoContext{
			RootPath: "/repo",
			RepoID:   repoID,
			HeadSHA:  "deadbeef",
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: policyVersionID,
			Name:            "cleanc-dev-policy",
		},
		Samples: []rule_engine.Sample{
			{
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
			},
		},
		Findings: []rule_engine.Finding{
			{
				FindingID:       must(t),
				EvaluationRunID: must(t),
				RepoID:          repoID,
				SHA:             "deadbeef",
				ScopeID:         scopeID,
				RuleID:          "solid.srp.lcom4_high",
				RuleVersion:     3,
				PolicyVersionID: policyVersionID,
				MetricSampleIDs: []uuid.UUID{sampleID},
				Severity:        steward.SeverityWarn,
			},
		},
		Tasks: []refactor.RefactorTask{
			{
				TaskID:        taskID,
				PlanID:        planID,
				ScopeID:       scopeID,
				Kind:          refactor.TaskKindSplitClass,
				EffortHours:   4.5,
				RuleID:        "solid.srp.lcom4_high",
				DescriptionMD: "Split the class along cohesion boundaries.",
			},
		},
		Diagnostics: orchestrator.Diagnostics{
			EffortSource: "fallback",
		},
	}

	return art, tbl
}

func TestJSONL_Emit_OneJSONLineWithTrailingLFPerTask(t *testing.T) {
	t.Parallel()
	art, tbl := fixtureArtifact(t)

	var buf bytes.Buffer
	em := &suggest.JSONL{Bindings: tbl}
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("output missing trailing LF: %q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 record line, got %d: %q", len(lines), out)
	}
	if strings.Contains(lines[0], "\n") {
		t.Fatalf("a single record line must not contain an embedded LF: %q", lines[0])
	}
}

func TestJSONL_Emit_PopulatesExpectedFields(t *testing.T) {
	t.Parallel()
	art, tbl := fixtureArtifact(t)

	var buf bytes.Buffer
	em := &suggest.JSONL{Bindings: tbl}
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var rec suggest.RefactorPromptRecord
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	task := art.Tasks[0]
	if rec.TaskID != task.TaskID.String() {
		t.Errorf("TaskID = %q, want %q", rec.TaskID, task.TaskID.String())
	}
	if rec.PlanID != task.PlanID.String() {
		t.Errorf("PlanID = %q, want %q", rec.PlanID, task.PlanID.String())
	}
	if rec.RepoID != art.Context.RepoID.String() {
		t.Errorf("RepoID mismatch")
	}
	if rec.HeadSHA != "deadbeef" {
		t.Errorf("HeadSHA = %q, want deadbeef", rec.HeadSHA)
	}
	if rec.PolicyVersionID != art.Policy.PolicyVersionID.String() {
		t.Errorf("PolicyVersionID mismatch")
	}
	if rec.TaskKind != string(refactor.TaskKindSplitClass) {
		t.Errorf("TaskKind = %q, want split_class", rec.TaskKind)
	}
	if rec.RuleID != "solid.srp.lcom4_high" {
		t.Errorf("RuleID = %q", rec.RuleID)
	}
	if rec.RuleVersion != 3 {
		t.Errorf("RuleVersion = %d, want 3", rec.RuleVersion)
	}
	if rec.Severity != string(steward.SeverityWarn) {
		t.Errorf("Severity = %q, want warn", rec.Severity)
	}
	if rec.Scope.Signature != "pkg.Foo" || rec.Scope.Kind != "class" ||
		rec.Scope.FilePath != "pkg/foo.go" || rec.Scope.StartLine != 10 || rec.Scope.EndLine != 42 {
		t.Errorf("Scope mismatch: %+v", rec.Scope)
	}
	if rec.EffortHours != 4.5 {
		t.Errorf("EffortHours = %v", rec.EffortHours)
	}
	if rec.EffortSource != "fallback" {
		t.Errorf("EffortSource = %q", rec.EffortSource)
	}
	if rec.ProseSuggestion != "Split the class along cohesion boundaries." {
		t.Errorf("ProseSuggestion = %q", rec.ProseSuggestion)
	}
	if rec.PromptFormatVersion != suggest.PromptFormatVersion {
		t.Errorf("PromptFormatVersion = %q, want %q", rec.PromptFormatVersion, suggest.PromptFormatVersion)
	}
	if len(rec.MetricEvidence) != 1 ||
		rec.MetricEvidence[0].MetricKind != "lcom4" ||
		rec.MetricEvidence[0].Value != 12 {
		t.Errorf("MetricEvidence mismatch: %+v", rec.MetricEvidence)
	}
	if rec.SourceSnippet != "" || rec.SourceSnippetTruncated {
		t.Errorf("SourceSnippet should be empty when no extractor configured; got %q / %v",
			rec.SourceSnippet, rec.SourceSnippetTruncated)
	}
}

func TestJSONL_Emit_FailsClosedOnMissingBinding(t *testing.T) {
	t.Parallel()
	art, _ := fixtureArtifact(t)
	// Replace the table with an empty one -- the task's
	// ScopeID is now unresolvable and Emit must refuse.
	em := &suggest.JSONL{Bindings: scopebinding.NewTable()}

	var buf bytes.Buffer
	err := em.Emit(context.Background(), art, &buf)
	if err == nil {
		t.Fatalf("Emit returned nil error; expected MissingScopeBindingError")
	}
	var mbe *suggest.MissingScopeBindingError
	if !errors.As(err, &mbe) {
		t.Fatalf("Emit returned %v (%T); want *MissingScopeBindingError", err, err)
	}
	if mbe.TaskID != art.Tasks[0].TaskID || mbe.ScopeID != art.Tasks[0].ScopeID {
		t.Errorf("MissingScopeBindingError mismatched IDs: %+v", mbe)
	}
	if buf.Len() != 0 {
		t.Errorf("expected zero bytes written on fail-closed; got %d", buf.Len())
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
	tbl := scopebinding.NewTable()
	em := &suggest.JSONL{Bindings: tbl}
	var buf bytes.Buffer
	if err := em.Emit(context.Background(), report.RunArtifact{}, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty output for zero tasks; got %q", buf.String())
	}
}

func TestJSONL_Emit_HonoursContextCancellation(t *testing.T) {
	t.Parallel()
	art, tbl := fixtureArtifact(t)
	em := &suggest.JSONL{Bindings: tbl}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := em.Emit(ctx, art, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Emit returned %v; want context.Canceled", err)
	}
}

func TestJSONL_Emit_DeterministicOutputOnRepeatedInvocations(t *testing.T) {
	t.Parallel()
	art, tbl := fixtureArtifact(t)
	em := &suggest.JSONL{Bindings: tbl}

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

func TestJSONL_Emit_SnippetExtractorWired(t *testing.T) {
	t.Parallel()
	art, tbl := fixtureArtifact(t)

	want := "package foo\n"
	em := &suggest.JSONL{
		Bindings: tbl,
		SnippetExtractor: func(scope suggest.Scope) (string, bool, error) {
			if scope.FilePath != "pkg/foo.go" {
				t.Errorf("extractor got scope %+v", scope)
			}
			return want, true, nil
		},
	}

	var buf bytes.Buffer
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	var rec suggest.RefactorPromptRecord
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.SourceSnippet != want || !rec.SourceSnippetTruncated {
		t.Fatalf("snippet not wired: %+v", rec)
	}
}

func TestJSONL_Emit_SnippetExtractorErrorPropagates(t *testing.T) {
	t.Parallel()
	art, tbl := fixtureArtifact(t)
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

func TestJSONL_Emit_MissingFindingLeavesEvidenceEmptyButRecordEmitted(t *testing.T) {
	t.Parallel()
	art, tbl := fixtureArtifact(t)
	// Strip findings so the (ScopeID, RuleID) join misses.
	art.Findings = nil

	var buf bytes.Buffer
	em := &suggest.JSONL{Bindings: tbl}
	if err := em.Emit(context.Background(), art, &buf); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	var rec suggest.RefactorPromptRecord
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.RuleVersion != 0 || rec.Severity != "" {
		t.Errorf("expected zero rule_version + empty severity when finding missing; got %d / %q",
			rec.RuleVersion, rec.Severity)
	}
	if rec.MetricEvidence == nil || len(rec.MetricEvidence) != 0 {
		t.Errorf("MetricEvidence must serialise as `[]`, not null; got %+v", rec.MetricEvidence)
	}
	// And the JSON itself must contain `"metric_evidence":[]`, not `null`,
	// so downstream prompt templates that index .metric_evidence[0]
	// don't NPE.
	if !bytes.Contains(buf.Bytes(), []byte(`"metric_evidence":[]`)) {
		t.Errorf("expected literal `\"metric_evidence\":[]` in output; got %s", buf.String())
	}
}
