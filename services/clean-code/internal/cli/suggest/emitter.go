// -----------------------------------------------------------------------
// <copyright file="emitter.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package suggest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// PromptEmitter is the single output contract the
// `cleanc analyze --emit-prompts <path>` composition root
// dispatches against. One emitter per requested output (the
// default [JSONL] today; future SARIF / NDJSON variants only
// add new implementors, not new dispatch sites). Anchor:
// REFACTOR-GUIDE `architecture.md` Sec 5.7.
//
// Implementors MUST be safe to invoke once per [report.RunArtifact]
// and MUST NOT mutate `art`; the composition root re-uses the
// same artifact across every writer in the dispatch loop.
type PromptEmitter interface {
	// Emit serialises one [RefactorPromptRecord] per
	// `art.Tasks` entry onto `w`. The returned error is the
	// first I/O, marshal, or consistency-check failure;
	// callers MUST treat a non-nil error as a hard failure
	// (no partial-success retries) so the operator-facing
	// `--emit-prompts` diagnostic flags the offending task.
	//
	// `ctx` is honoured for cancellation between records.
	Emit(ctx context.Context, art report.RunArtifact, w io.Writer) error
}

// ErrNilBindingTable is returned by [JSONL.Emit] when the
// emitter is constructed without a non-nil [scopebinding.Table].
// The table is the single source of truth for the
// `scope.signature` / `scope.kind` / `scope.file_path` /
// `scope.start_line` / `scope.end_line` quintuple per the
// workstream brief, so the emitter refuses to run without it
// rather than fabricate zero-valued scope rows.
var ErrNilBindingTable = errors.New("suggest: JSONL.Bindings must not be nil")

// MissingScopeBindingError is returned by [JSONL.Emit] when a
// task's [refactor.RefactorTask.ScopeID] is not present in the
// configured [scopebinding.Table]. The condition is a wiring
// bug -- every task ScopeID is inherited from a [HotSpot]
// row, every hot_spot ScopeID is itself inserted into the
// binding table by the Stage 2.2 orchestrator -- so the
// emitter fails-closed per the workstream brief
// ("consistency check, never expected in a healthy run").
//
// Exported so downstream callers can `errors.As` the failure
// and surface the offending TaskID / ScopeID without
// re-parsing the error string.
type MissingScopeBindingError struct {
	TaskID  uuid.UUID
	ScopeID uuid.UUID
}

// Error implements the `error` interface.
func (e *MissingScopeBindingError) Error() string {
	return fmt.Sprintf(
		"suggest: scope binding not found for task %s (scope_id %s); orchestrator wiring bug",
		e.TaskID, e.ScopeID,
	)
}

// SnippetExtractor optionally resolves a `(SourceSnippet,
// SourceSnippetTruncated)` pair for one [Scope]. Pluggable so
// the default emitter can stay decoupled from disk I/O when a
// test harness wants to inject a deterministic snippet, and
// so production callers can wire [FileSnippetExtractor]
// (which calls [ExtractSnippet]).
//
// When [JSONL.SnippetExtractor] is nil the emitter leaves
// [RefactorPromptRecord.SourceSnippet] empty and
// [RefactorPromptRecord.SourceSnippetTruncated] false; the
// brief scopes the Stage 4.2 emitter to the scope-binding
// resolution + JSONL framing path, so the snippet wiring is
// strictly opt-in.
type SnippetExtractor func(scope Scope) (snippet string, truncated bool, err error)

// JSONL is the canonical [PromptEmitter] implementation: one
// JSON-encoded [RefactorPromptRecord] per line, LF-terminated,
// per REFACTOR-GUIDE `architecture.md` Sec 5.7 / Sec 4.6.
//
// Zero value is NOT usable; callers MUST populate Bindings
// before Emit is invoked.
type JSONL struct {
	// Bindings resolves [refactor.RefactorTask.ScopeID] to
	// the scope-coordinate quintuple
	// (signature/kind/file_path/start_line/end_line) per
	// the workstream brief. Required; [Emit] returns
	// [ErrNilBindingTable] when nil.
	Bindings *scopebinding.Table

	// SnippetExtractor optionally populates
	// [RefactorPromptRecord.SourceSnippet] /
	// [RefactorPromptRecord.SourceSnippetTruncated]. Nil
	// leaves both at their zero values.
	SnippetExtractor SnippetExtractor
}

// Compile-time assertion that JSONL satisfies PromptEmitter.
var _ PromptEmitter = (*JSONL)(nil)

// Emit walks `art.Tasks` and writes one JSON-encoded
// [RefactorPromptRecord] per task, each terminated by a
// single `'\n'` byte (the JSONL wire format). Records are
// emitted in the input order so the orchestrator's
// deterministic task ordering (score-DESC then rule_id-ASC,
// per [refactor.TaskPlanner]) carries through to the prompt
// artifact byte-for-byte.
//
// Fail-closed contract (workstream brief): every task whose
// ScopeID is absent from [JSONL.Bindings] causes Emit to
// return a [*MissingScopeBindingError] without writing the
// offending record AND without writing any subsequent
// record. Records emitted before the failure are NOT rolled
// back -- the caller's writer owns flushing/truncation.
//
// Field population:
//   - `task_id`, `plan_id`, `task_kind`, `rule_id`,
//     `effort_hours`, `prose_suggestion` come from
//     [refactor.RefactorTask] (the latter copied verbatim
//     from `RefactorTask.DescriptionMD`).
//   - `repo_id`, `head_sha` come from `art.Context`.
//   - `policy_version_id` comes from
//     `art.Policy.PolicyVersionID`.
//   - `scope.*` comes from `Bindings.Get(task.ScopeID)`.
//   - `rule_version`, `severity`, `metric_evidence` are
//     resolved by joining `art.Findings` (by
//     `(ScopeID, RuleID)`) with `art.Samples` (by
//     `SampleID`). Joins are best-effort: a task with no
//     matching finding still emits a record with empty
//     rule_version / severity and a `[]` metric_evidence
//     slice so downstream consumers never see `null`.
//   - `source_snippet` / `source_snippet_truncated` come
//     from [JSONL.SnippetExtractor] when configured; left
//     at zero values otherwise.
//   - `effort_source` is `art.Diagnostics.EffortSource`.
//   - `prompt_format_version` is the package-level
//     [PromptFormatVersion] constant.
//
// MetricEvidence sub-records carry `metric_kind` and
// `value` directly from the matched [rule_engine.Sample].
// `threshold` and `op` are left at zero values for now;
// the Stage 4.3 DSL-predicate decomposition workstream is
// the canonical place to fill those columns once the DSL
// evaluator exposes a per-firing trace. The empty
// `threshold`/`op` slot keeps the wire shape stable
// (PromptFormatVersion is unchanged) -- consumers MUST
// tolerate the zero values per the field doc comment.
func (j *JSONL) Emit(ctx context.Context, art report.RunArtifact, w io.Writer) error {
	if j == nil || j.Bindings == nil {
		return ErrNilBindingTable
	}

	// Index findings by (ScopeID, RuleID). One task per
	// (ScopeID, RuleID) pair is the canonical mapping (a
	// task is motivated by exactly one rule firing at one
	// scope, per `RefactorTask.RuleID` doc). If multiple
	// findings collide on the key we keep the first --
	// deterministic because `art.Findings` itself is in
	// the engine's deterministic order.
	type findingKey struct {
		scopeID uuid.UUID
		ruleID  string
	}
	findingByKey := make(map[findingKey]rule_engine.Finding, len(art.Findings))
	for _, f := range art.Findings {
		k := findingKey{scopeID: f.ScopeID, ruleID: f.RuleID}
		if _, ok := findingByKey[k]; !ok {
			findingByKey[k] = f
		}
	}

	// Index samples by SampleID so the MetricEvidence
	// rebuild does not re-scan `art.Samples` per task.
	sampleByID := make(map[uuid.UUID]rule_engine.Sample, len(art.Samples))
	for _, s := range art.Samples {
		sampleByID[s.SampleID] = s
	}

	for i := range art.Tasks {
		if err := ctx.Err(); err != nil {
			return err
		}
		task := art.Tasks[i]

		binding, ok := j.Bindings.Get(task.ScopeID)
		if !ok {
			return &MissingScopeBindingError{
				TaskID:  task.TaskID,
				ScopeID: task.ScopeID,
			}
		}

		rec := RefactorPromptRecord{
			TaskID:              task.TaskID.String(),
			PlanID:              task.PlanID.String(),
			RepoID:              art.Context.RepoID.String(),
			HeadSHA:             art.Context.HeadSHA,
			PolicyVersionID:     art.Policy.PolicyVersionID.String(),
			TaskKind:            string(task.Kind),
			RuleID:              task.RuleID,
			Scope:               scopeFromBinding(binding),
			MetricEvidence:      []MetricEvidence{},
			ProseSuggestion:     task.DescriptionMD,
			EffortHours:         task.EffortHours,
			EffortSource:        art.Diagnostics.EffortSource,
			PromptFormatVersion: PromptFormatVersion,
		}

		if f, ok := findingByKey[findingKey{scopeID: task.ScopeID, ruleID: task.RuleID}]; ok {
			rec.RuleVersion = f.RuleVersion
			rec.Severity = string(f.Severity)
			for _, sid := range f.MetricSampleIDs {
				s, ok := sampleByID[sid]
				if !ok {
					continue
				}
				rec.MetricEvidence = append(rec.MetricEvidence, MetricEvidence{
					MetricKind: s.MetricKind,
					Value:      s.Value,
				})
			}
		}

		if j.SnippetExtractor != nil {
			snippet, truncated, err := j.SnippetExtractor(rec.Scope)
			if err != nil {
				return fmt.Errorf("suggest: extract snippet for task %s: %w", task.TaskID, err)
			}
			rec.SourceSnippet = snippet
			rec.SourceSnippetTruncated = truncated
		}

		line, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("suggest: marshal task %s: %w", task.TaskID, err)
		}
		if _, err := w.Write(line); err != nil {
			return fmt.Errorf("suggest: write task %s: %w", task.TaskID, err)
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("suggest: write newline after task %s: %w", task.TaskID, err)
		}
	}

	return nil
}

// scopeFromBinding lifts a [scopebinding.ScopeBinding] into
// the wire-shape [Scope] sub-record. Field mapping is
// 1:1 except for ScopeKind -> Kind (the binding struct uses
// `ScopeKind` to disambiguate the noun; the wire shape uses
// the shorter `kind` per the JSON field tag pinned in
// record.go).
func scopeFromBinding(b scopebinding.ScopeBinding) Scope {
	return Scope{
		Signature: b.Signature,
		Kind:      b.ScopeKind,
		FilePath:  b.FilePath,
		StartLine: b.StartLine,
		EndLine:   b.EndLine,
	}
}
