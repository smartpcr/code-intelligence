//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="p1_structured_prompt_emitter_jsonl_prompt_emitter_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/flags"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/suggest"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
)

// jsonlEmitterState holds per-scenario state for the JSONL
// prompt emitter e2e scenarios.
type jsonlEmitterState struct {
	art      report.RunArtifact
	tbl      *scopebinding.Table
	emitter  *suggest.JSONL
	output   []byte
	output2  []byte
	emitErr  error
	taskKind string
	badTask  uuid.UUID
	exitCode int
	stderr   string
}

func newJSONLEmitterState() *jsonlEmitterState {
	return &jsonlEmitterState{}
}

// buildArtifact constructs a RunArtifact with n tasks, each
// sharing a distinct ScopeID in the binding table.
func (s *jsonlEmitterState) buildArtifact(n int) error {
	s.tbl = scopebinding.NewTable()

	repoID := uuid.Must(uuid.NewV4())
	planID := uuid.Must(uuid.NewV4())
	policyVersionID := uuid.Must(uuid.NewV4())

	s.art = report.RunArtifact{
		Context: repocontext.RepoContext{
			RepoID:  repoID,
			HeadSHA: "e2edeadbeef",
		},
		Policy: stewardPolicyVersion(policyVersionID),
		Diagnostics: orchestrator.Diagnostics{
			EffortSource: "fallback",
		},
	}

	kinds := []refactor.TaskKind{
		refactor.TaskKindSplitClass,
		refactor.TaskKindExtractMethod,
		refactor.TaskKindInvertDependency,
		refactor.TaskKindBreakCycle,
		refactor.TaskKindConsolidateDuplication,
	}

	for i := 0; i < n; i++ {
		scopeID := uuid.Must(uuid.NewV4())
		taskID := uuid.Must(uuid.NewV4())

		if err := s.tbl.Insert(scopebinding.ScopeBinding{
			ScopeID:   scopeID,
			ScopeKind: "class",
			FilePath:  fmt.Sprintf("pkg/file%d.go", i),
			StartLine: 1,
			EndLine:   10,
			Signature: fmt.Sprintf("pkg.Class%d", i),
			Language:  "go",
		}); err != nil {
			return fmt.Errorf("insert scope binding %d: %w", i, err)
		}

		s.art.Tasks = append(s.art.Tasks, refactor.RefactorTask{
			TaskID:        taskID,
			PlanID:        planID,
			ScopeID:       scopeID,
			Kind:          kinds[i%len(kinds)],
			EffortHours:   float64(i + 1),
			RuleID:        fmt.Sprintf("rule.e2e.%d", i),
			DescriptionMD: fmt.Sprintf("task %d description", i),
		})
	}

	s.emitter = &suggest.JSONL{
		Bindings:              s.tbl,
		DisableSnippetDefault: true,
	}
	return nil
}

// --- helpers (shared) ---

// stewardPolicyVersion builds a minimal steward.PolicyVersion.
func stewardPolicyVersion(id uuid.UUID) steward.PolicyVersion {
	return steward.PolicyVersion{
		PolicyVersionID: id,
		Name:            "e2e-jsonl-policy",
	}
}

// --- Given steps ---------------------------------------------------------

func (s *jsonlEmitterState) aRunArtifactWithNTasks(n int) error {
	return s.buildArtifact(n)
}

func (s *jsonlEmitterState) aRunArtifactWithATaskOfKind(kind string) error {
	if err := s.buildArtifact(1); err != nil {
		return fmt.Errorf("build artifact: %w", err)
	}
	s.taskKind = kind
	s.badTask = s.art.Tasks[0].TaskID
	s.art.Tasks[0].Kind = refactor.TaskKind(kind)
	return nil
}

// --- When steps ----------------------------------------------------------

func (s *jsonlEmitterState) jsonlEmitRuns() error {
	var buf bytes.Buffer
	s.emitErr = s.emitter.Emit(context.Background(), s.art, &buf)
	s.output = buf.Bytes()
	return nil
}

// compositionRootRunsEmitter simulates the CLI composition
// root's emit-prompts dispatch: call JSONL.Emit, map the
// error to an exit code (flags.ExitInternalError = 70 on
// error, flags.ExitOK = 0 on success), and write the error
// to a stderr buffer -- exactly as runAnalyzePipeline does
// in cmd/cleanc/main.go.
func (s *jsonlEmitterState) compositionRootRunsEmitter() error {
	var stdout bytes.Buffer
	var stderrBuf bytes.Buffer
	err := s.emitter.Emit(context.Background(), s.art, &stdout)
	if err != nil {
		fmt.Fprintf(&stderrBuf, "cleanc analyze: emit prompts: %v\n", err)
		s.exitCode = flags.ExitInternalError
	} else {
		s.exitCode = flags.ExitOK
	}
	s.emitErr = err
	s.output = stdout.Bytes()
	s.stderr = stderrBuf.String()
	return nil
}

func (s *jsonlEmitterState) jsonlEmitRunsTwice() error {
	var buf1 bytes.Buffer
	if err := s.emitter.Emit(context.Background(), s.art, &buf1); err != nil {
		return fmt.Errorf("first Emit: %w", err)
	}
	s.output = buf1.Bytes()

	var buf2 bytes.Buffer
	if err := s.emitter.Emit(context.Background(), s.art, &buf2); err != nil {
		return fmt.Errorf("second Emit: %w", err)
	}
	s.output2 = buf2.Bytes()
	return nil
}

// --- Then steps ----------------------------------------------------------

func (s *jsonlEmitterState) theOutputHasExactlyNLines(n int) error {
	if s.emitErr != nil {
		return fmt.Errorf("Emit returned error: %w", s.emitErr)
	}
	lines := nonEmptyLines(s.output)
	if len(lines) != n {
		return fmt.Errorf("expected %d lines, got %d", n, len(lines))
	}
	return nil
}

func (s *jsonlEmitterState) eachLineIsParseableAsStandaloneJSON() error {
	if s.emitErr != nil {
		return fmt.Errorf("Emit returned error: %w", s.emitErr)
	}
	lines := nonEmptyLines(s.output)
	for i, line := range lines {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return fmt.Errorf("line %d not valid JSON: %v\nline: %s", i+1, err, line)
		}
	}
	return nil
}

func (s *jsonlEmitterState) everyEmittedRecordHasPromptFormatVersion(want string) error {
	if s.emitErr != nil {
		return fmt.Errorf("Emit returned error: %w", s.emitErr)
	}
	lines := nonEmptyLines(s.output)
	for i, line := range lines {
		var rec suggest.RefactorPromptRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return fmt.Errorf("line %d unmarshal: %v", i+1, err)
		}
		if rec.PromptFormatVersion != want {
			return fmt.Errorf("line %d: prompt_format_version = %q, want %q",
				i+1, rec.PromptFormatVersion, want)
		}
	}
	return nil
}

func (s *jsonlEmitterState) exitCodeIs(want int) error {
	if s.exitCode != want {
		return fmt.Errorf("exit code = %d, want %d\nstderr: %s", s.exitCode, want, s.stderr)
	}
	return nil
}

func (s *jsonlEmitterState) stderrNamesTheOffendingTaskID() error {
	taskIDStr := s.badTask.String()
	if !strings.Contains(s.stderr, taskIDStr) {
		return fmt.Errorf("stderr does not contain task id %s\nstderr: %s", taskIDStr, s.stderr)
	}
	return nil
}

func (s *jsonlEmitterState) theTwoOutputsAreByteIdentical() error {
	if !bytes.Equal(s.output, s.output2) {
		// Show first differing byte for diagnostics.
		minLen := len(s.output)
		if len(s.output2) < minLen {
			minLen = len(s.output2)
		}
		for i := 0; i < minLen; i++ {
			if s.output[i] != s.output2[i] {
				return fmt.Errorf("outputs differ at byte %d: 0x%02x vs 0x%02x", i, s.output[i], s.output2[i])
			}
		}
		return fmt.Errorf("outputs differ in length: %d vs %d", len(s.output), len(s.output2))
	}
	return nil
}

// --- helpers -------------------------------------------------------------

// nonEmptyLines splits output on newlines and drops empty
// trailing elements.
func nonEmptyLines(b []byte) []string {
	raw := strings.Split(string(b), "\n")
	var out []string
	for _, l := range raw {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// --- godog wiring --------------------------------------------------------

func InitializeScenario_p1_structured_prompt_emitter_jsonl_prompt_emitter(ctx *godog.ScenarioContext) {
	s := newJSONLEmitterState()

	// Given
	ctx.Step(`^a RunArtifact with (\d+) tasks?$`, s.aRunArtifactWithNTasks)
	ctx.Step(`^a RunArtifact with a task of kind "([^"]*)"$`, s.aRunArtifactWithATaskOfKind)

	// When
	ctx.Step(`^JSONL\.Emit runs$`, s.jsonlEmitRuns)
	ctx.Step(`^JSONL\.Emit runs twice on the same artifact$`, s.jsonlEmitRunsTwice)
	ctx.Step(`^the composition root runs the emitter and maps the result$`, s.compositionRootRunsEmitter)

	// Then
	ctx.Step(`^the output has exactly (\d+) lines$`, s.theOutputHasExactlyNLines)
	ctx.Step(`^each line is parseable as a standalone JSON object$`, s.eachLineIsParseableAsStandaloneJSON)
	ctx.Step(`^every emitted record has prompt_format_version "([^"]*)"$`, s.everyEmittedRecordHasPromptFormatVersion)
	ctx.Step(`^exit code is (\d+)$`, s.exitCodeIs)
	ctx.Step(`^stderr names the offending task id$`, s.stderrNamesTheOffendingTaskID)
	ctx.Step(`^the two outputs are byte-identical$`, s.theTwoOutputsAreByteIdentical)
}

func TestE2E_p1_structured_prompt_emitter_jsonl_prompt_emitter(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_p1_structured_prompt_emitter_jsonl_prompt_emitter,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"p1_structured_prompt_emitter_jsonl_prompt_emitter.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
