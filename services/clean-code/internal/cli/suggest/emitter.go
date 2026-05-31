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
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// PromptEmitter is the single output contract the
// `cleanc analyze --emit-prompts <path>` composition root
// dispatches against. One emitter per requested output (the
// default [JSONL] today; future SARIF / NDJSON variants only
// add new implementors, not new dispatch sites). Anchor:
// REFACTOR-GUIDE `architecture.md` Sec 5.7 / Sec 4.6.
//
// Implementors MUST be safe to invoke once per [report.RunArtifact]
// and MUST NOT mutate `art`; the composition root re-uses the
// same artifact across every writer in the dispatch loop.
type PromptEmitter interface {
	// Emit serialises one [RefactorPromptRecord] per
	// `art.Tasks` entry onto `w`. The returned error is
	// the first I/O, marshal, or consistency-check
	// failure; callers MUST treat a non-nil error as a
	// hard failure (no partial-success retries) so the
	// operator-facing `--emit-prompts` diagnostic flags the
	// offending task.
	//
	// `ctx` is honoured for cancellation between records.
	Emit(ctx context.Context, art report.RunArtifact, w io.Writer) error
}

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

// ErrNilBindingTable is returned by [JSONL.Emit] when the
// emitter is constructed without a non-nil [scopebinding.Table].
// The table is the single source of truth for the
// `scope.signature` / `scope.kind` / `scope.file_path` /
// `scope.start_line` / `scope.end_line` quintuple per the
// workstream brief, so the emitter refuses to run without it
// rather than fabricate zero-valued scope rows.
var ErrNilBindingTable = errors.New("suggest: JSONL.Bindings must not be nil")

// ErrNilWriter is returned by [JSONL.Emit] when `w` is nil.
// The previous iteration would panic inside `w.Write` instead
// of surfacing a normal error; the explicit guard mirrors the
// nil-writer pre-condition the report renderers ([report.Markdown],
// [report.JSON]) already enforce so a mis-wired composition root
// fails closed at the dispatch site rather than panicking inside
// the marshal loop.
var ErrNilWriter = errors.New("suggest: writer must not be nil")

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

// InvalidTaskKindError is returned by [JSONL.Emit] when a
// task carries a [refactor.TaskKind] that is not in the
// canonical five-value set ([refactor.CanonicalTaskKinds]).
// The wrapped error is one of [refactor.ErrRejectedTaskKindAlias]
// or [refactor.ErrUnknownTaskKind] so callers can
// `errors.Is`-distinguish a deliberate iter-3 alias drift
// signal from a typo / future-spec drift. Anchor: architecture
// Sec 4.8 task-kind gate.
type InvalidTaskKindError struct {
	TaskID uuid.UUID
	Kind   refactor.TaskKind
	Err    error
}

// Error implements the `error` interface.
func (e *InvalidTaskKindError) Error() string {
	return fmt.Sprintf("suggest: task %s carries non-canonical kind %q: %v",
		e.TaskID, e.Kind, e.Err)
}

// Unwrap exposes the underlying [refactor.ErrRejectedTaskKindAlias]
// / [refactor.ErrUnknownTaskKind] for `errors.Is` callers.
func (e *InvalidTaskKindError) Unwrap() error { return e.Err }

// ---------------------------------------------------------------------------
// Pluggable resolvers
// ---------------------------------------------------------------------------

// SnippetExtractor resolves a `(SourceSnippet,
// SourceSnippetTruncated)` pair for one [Scope]. The
// production-mode default is [FileSnippetExtractor];
// composition roots that want a deterministic snippet
// (golden tests, in-memory fixtures) inject their own.
type SnippetExtractor func(scope Scope) (snippet string, truncated bool, err error)

// RuleResolver looks up the canonical [steward.Rule] row for
// the `(ruleID, version)` composite key. The Stage 4.2
// emitter uses it to populate
// [RefactorPromptRecord.ProseSuggestion] from the rule pack
// author's `DescriptionMD` markdown -- the rule pack YAML is
// the canonical authoring surface for remediation prose per
// architecture Sec 3.7.3 / Sec 4.6.
//
// Returns `(zero, false)` when the rule is unknown; the
// emitter then falls back to [refactor.RefactorTask.DescriptionMD]
// (which Stage 8.2 populates with a deterministic default).
type RuleResolver interface {
	Lookup(ruleID string, version int) (steward.Rule, bool)
}

// RuleResolverFunc adapts a plain function to [RuleResolver].
type RuleResolverFunc func(ruleID string, version int) (steward.Rule, bool)

// Lookup implements [RuleResolver].
func (f RuleResolverFunc) Lookup(ruleID string, version int) (steward.Rule, bool) {
	return f(ruleID, version)
}

// NewSliceRuleResolver wraps a slice of [steward.Rule] rows
// in a [RuleResolver]. The slice is indexed once on
// construction (O(N)) so lookup is O(1); the resolver holds a
// reference to the input slice, do not mutate it after the
// call returns.
func NewSliceRuleResolver(rules []steward.Rule) RuleResolver {
	type key struct {
		id      string
		version int
	}
	idx := make(map[key]steward.Rule, len(rules))
	for _, r := range rules {
		idx[key{r.RuleID, r.Version}] = r
	}
	return RuleResolverFunc(func(ruleID string, version int) (steward.Rule, bool) {
		r, ok := idx[key{ruleID, version}]
		return r, ok
	})
}

// ThresholdResolver looks up the canonical `(op, value)`
// pair for the `(metricKind, scopeKind)` coordinate. The
// emitter uses it to populate [MetricEvidence.Threshold] /
// [MetricEvidence.Op] without fabricating a `0` /  `""`
// silent zero per the iter-1 evaluator's feedback item 3.
//
// Returns `(_, _, false)` when no threshold applies; the
// emitter then SKIPS the offending [MetricEvidence] row
// rather than emit a row whose `0` looks like a real
// threshold of zero. The metric_evidence array therefore
// reflects only rows for which a real threshold was
// resolvable.
//
// `op` MUST be the SYMBOLIC form (">=", ">", "<=", "<", "==")
// that the wire format pins, NOT the DSL enum label
// ("ge", "gt", ...). [ThresholdOpSymbol] is the canonical
// translator from [dsl.ThresholdOp] to the symbolic form.
type ThresholdResolver interface {
	Lookup(metricKind, scopeKind string) (op string, value float64, ok bool)
}

// ThresholdResolverFunc adapts a plain function to [ThresholdResolver].
type ThresholdResolverFunc func(metricKind, scopeKind string) (string, float64, bool)

// Lookup implements [ThresholdResolver].
func (f ThresholdResolverFunc) Lookup(metricKind, scopeKind string) (string, float64, bool) {
	return f(metricKind, scopeKind)
}

// NewSliceThresholdResolver wraps a slice of
// [steward.Threshold] rows in a [ThresholdResolver]. The
// thresholds are indexed once on construction; the inner
// `steward.Threshold.Op` enum label is translated to the
// symbolic wire form via [ThresholdOpSymbol] so the
// resolver returns wire-ready values.
//
// When multiple thresholds collide on `(MetricKind, ScopeKind)`
// the FIRST row wins (deterministic; callers SHOULD dedupe
// upstream).
func NewSliceThresholdResolver(thresholds []steward.Threshold) ThresholdResolver {
	type key struct {
		metric string
		scope  string
	}
	idx := make(map[key]steward.Threshold, len(thresholds))
	for _, t := range thresholds {
		k := key{t.MetricKind, t.ScopeKind}
		if _, ok := idx[k]; !ok {
			idx[k] = t
		}
	}
	return ThresholdResolverFunc(func(metricKind, scopeKind string) (string, float64, bool) {
		t, ok := idx[key{metricKind, scopeKind}]
		if !ok {
			return "", 0, false
		}
		return ThresholdOpSymbol(dsl.ThresholdOp(t.Op)), t.Value, true
	})
}

// ThresholdOpSymbol translates a [dsl.ThresholdOp] enum label
// (`"gt"`, `"ge"`, `"lt"`, `"le"`, `"eq"`) into the
// symbolic wire form (`">"`, `">="`, `"<"`, `"<="`, `"=="`)
// pinned by [MetricEvidence.Op] / record.go Sec "Op". Returns
// the input verbatim when it is already a recognised symbol
// (idempotent) and an empty string when the input is neither
// a canonical enum label nor a canonical symbol -- the empty
// string is the explicit "unknown operator" signal
// downstream consumers MUST tolerate.
func ThresholdOpSymbol(op dsl.ThresholdOp) string {
	switch op {
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
	}
	switch string(op) {
	case ">", ">=", "<", "<=", "==", "!=":
		return string(op)
	}
	return ""
}

// ---------------------------------------------------------------------------
// JSONL emitter
// ---------------------------------------------------------------------------

// JSONL is the canonical [PromptEmitter] implementation: one
// JSON-encoded [RefactorPromptRecord] per line, LF-terminated,
// per REFACTOR-GUIDE `architecture.md` Sec 5.7 / Sec 4.6.
//
// The zero value with `Bindings` set is usable: when
// `SnippetExtractor` is nil the emitter wires
// [FileSnippetExtractor] against `art.Context.RootPath` so
// every emitted record carries the source snippet by default
// (architecture Sec 4.6 + iter-1 evaluator feedback item 1).
// Tests that want a snippet-free emitter set
// [JSONL.DisableSnippetDefault] = true.
//
// Construct via [NewJSONL] for the convenience-defaults form,
// or as a literal `&JSONL{Bindings: tbl, Rules: ..., ...}`
// when overriding individual fields.
type JSONL struct {
	// Bindings resolves [refactor.RefactorTask.ScopeID] to
	// the scope-coordinate quintuple
	// (signature/kind/file_path/start_line/end_line) per
	// the workstream brief. Required; [Emit] returns
	// [ErrNilBindingTable] when nil.
	Bindings *scopebinding.Table

	// SnippetExtractor populates
	// [RefactorPromptRecord.SourceSnippet] /
	// [RefactorPromptRecord.SourceSnippetTruncated].
	// When nil AND DisableSnippetDefault is false AND
	// `art.Context.RootPath` is non-empty, Emit lazily
	// constructs `FileSnippetExtractor(rootPath,
	// DefaultSnippetMaxLines)` for this invocation so
	// records are actionable by default.
	SnippetExtractor SnippetExtractor

	// DisableSnippetDefault opts OUT of the default
	// [FileSnippetExtractor] wiring. When true AND
	// [SnippetExtractor] is nil, records carry an empty
	// source_snippet. Used by unit tests with no real
	// filesystem behind `art.Context.RootPath`.
	DisableSnippetDefault bool

	// Rules optionally resolves the rule pack author's
	// `DescriptionMD` markdown for each task's RuleID. When
	// nil OR the lookup misses, the emitter falls back to
	// [refactor.RefactorTask.DescriptionMD]. Architecture
	// Sec 3.7.3 / Sec 4.6 pin the rule's prose as the
	// canonical remediation text.
	Rules RuleResolver

	// Thresholds optionally resolves
	// [MetricEvidence.Threshold] / [MetricEvidence.Op] for
	// each sample. When nil, evidence rows are SKIPPED so
	// the wire format never carries silent `threshold: 0`
	// / `op: ""` pairs that look like real thresholds of
	// zero (iter-1 evaluator feedback item 3).
	Thresholds ThresholdResolver
}

// Compile-time assertion that JSONL satisfies PromptEmitter.
var _ PromptEmitter = (*JSONL)(nil)

// NewJSONL is the convenience constructor that returns a
// JSONL emitter with the required Bindings populated and all
// optional resolvers at their zero values. The caller may
// then set [JSONL.Rules] / [JSONL.Thresholds] /
// [JSONL.SnippetExtractor] before invoking Emit; the default
// snippet extractor is wired lazily at Emit time when
// [JSONL.SnippetExtractor] is still nil.
func NewJSONL(bindings *scopebinding.Table) *JSONL {
	return &JSONL{Bindings: bindings}
}

// Emit walks `art.Tasks` and writes one JSON-encoded
// [RefactorPromptRecord] per task, each terminated by a
// single `'\n'` byte (the JSONL wire format). Records are
// emitted in the input order so the orchestrator's
// deterministic task ordering (score-DESC then rule_id-ASC,
// per [refactor.TaskPlanner]) carries through to the prompt
// artifact byte-for-byte.
//
// Fail-closed contract (workstream brief + iter-1 evaluator
// feedback):
//   - Bindings == nil          -> [ErrNilBindingTable]
//   - w == nil                  -> [ErrNilWriter]
//   - Task.ScopeID not in Bindings -> [*MissingScopeBindingError]
//   - Task.Kind not in [refactor.CanonicalTaskKinds]
//                                -> [*InvalidTaskKindError]
//                                   (wraps
//                                   [refactor.ErrRejectedTaskKindAlias]
//                                   or [refactor.ErrUnknownTaskKind])
//   - SnippetExtractor returns err -> wrapped, fail-closed
//   - json.Marshal / w.Write err  -> wrapped, fail-closed
//
// Records emitted before the failure are NOT rolled back --
// the caller's writer owns flushing/truncation.
//
// Field population (per architecture Sec 4.6):
//   - `task_id`, `plan_id`, `task_kind`, `rule_id`,
//     `effort_hours` come from [refactor.RefactorTask].
//   - `repo_id`, `head_sha` come from `art.Context`.
//   - `policy_version_id` is `art.Policy.PolicyVersionID`.
//   - `scope.*` is `Bindings.Get(task.ScopeID)`.
//   - `rule_version`, `severity` come from the matching
//     [rule_engine.Finding] joined by `(ScopeID, RuleID)`.
//   - `metric_evidence` is the matched finding's
//     [rule_engine.Sample] rows joined by `SampleID`; each
//     row's `(metric_kind, value)` comes from the sample
//     and `(threshold, op)` comes from
//     [JSONL.Thresholds]. When `Thresholds` is nil OR the
//     resolver misses for a sample, that evidence row is
//     SKIPPED (no fabricated zero).
//   - `prose_suggestion` is `rule.DescriptionMD` from
//     [JSONL.Rules] when configured AND the lookup hits;
//     otherwise falls back to
//     [refactor.RefactorTask.DescriptionMD].
//   - `source_snippet` / `source_snippet_truncated` come
//     from [JSONL.SnippetExtractor] -- the default lazy
//     [FileSnippetExtractor] wiring uses
//     `art.Context.RootPath`.
//   - `effort_source` is `art.Diagnostics.EffortSource`.
//   - `prompt_format_version` is the package-level
//     [PromptFormatVersion] constant.
func (j *JSONL) Emit(ctx context.Context, art report.RunArtifact, w io.Writer) error {
	if j == nil || j.Bindings == nil {
		return ErrNilBindingTable
	}
	if w == nil {
		return ErrNilWriter
	}

	// Lazily construct the default snippet extractor so
	// every emitted record carries the source snippet
	// without the composition root having to wire it
	// explicitly. Opt-out via DisableSnippetDefault.
	extract := j.SnippetExtractor
	if extract == nil && !j.DisableSnippetDefault && art.Context.RootPath != "" {
		extract = FileSnippetExtractor(art.Context.RootPath, DefaultSnippetMaxLines)
	}

	// Index findings by (ScopeID, RuleID). One task per
	// (ScopeID, RuleID) pair is the canonical mapping
	// (a task is motivated by exactly one rule firing at
	// one scope, per `RefactorTask.RuleID`'s doc). If
	// multiple findings collide on the key we keep the
	// first -- deterministic because `art.Findings` is
	// itself in the engine's deterministic order.
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

		if err := refactor.ValidateTaskKind(task.Kind); err != nil {
			return &InvalidTaskKindError{TaskID: task.TaskID, Kind: task.Kind, Err: err}
		}

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
			EffortHours:         task.EffortHours,
			EffortSource:        art.Diagnostics.EffortSource,
			PromptFormatVersion: PromptFormatVersion,
		}

		f, haveFinding := findingByKey[findingKey{scopeID: task.ScopeID, ruleID: task.RuleID}]
		if haveFinding {
			rec.RuleVersion = f.RuleVersion
			rec.Severity = string(f.Severity)
		}

		// prose_suggestion: prefer the rule pack author's
		// DescriptionMD (architecture canonical source);
		// fall back to the task's DescriptionMD so the
		// field is never empty for a well-wired pipeline.
		rec.ProseSuggestion = resolveProse(j.Rules, task, f, haveFinding)

		// metric_evidence: join sample rows by SampleID,
		// then resolve threshold/op via Thresholds.
		// Without a Thresholds resolver, no evidence row
		// is emitted -- the array stays `[]` rather than
		// carrying silent zero thresholds.
		if haveFinding && j.Thresholds != nil {
			for _, sid := range f.MetricSampleIDs {
				s, ok := sampleByID[sid]
				if !ok {
					continue
				}
				op, threshold, ok := j.Thresholds.Lookup(s.MetricKind, s.ScopeKind)
				if !ok {
					// Skip: no real threshold available.
					// Emitting `0` would look like a
					// genuine zero per iter-1 feedback #3.
					continue
				}
				rec.MetricEvidence = append(rec.MetricEvidence, MetricEvidence{
					MetricKind: s.MetricKind,
					Value:      s.Value,
					Threshold:  threshold,
					Op:         op,
				})
			}
		}

		if extract != nil {
			snippet, truncated, err := extract(rec.Scope)
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

// resolveProse picks the canonical remediation prose for a
// task. The rule pack author's `DescriptionMD` is the
// architecture-pinned source (Sec 3.7.3 / Sec 4.6); the task
// planner's `DescriptionMD` is a deterministic fallback that
// the planner emits when no LLM-explainer overlay is
// configured.
func resolveProse(
	rules RuleResolver,
	task refactor.RefactorTask,
	finding rule_engine.Finding,
	haveFinding bool,
) string {
	if rules != nil {
		version := 0
		if haveFinding {
			version = finding.RuleVersion
		}
		if rule, ok := rules.Lookup(task.RuleID, version); ok && rule.DescriptionMD != "" {
			return rule.DescriptionMD
		}
	}
	return task.DescriptionMD
}

// scopeFromBinding lifts a [scopebinding.ScopeBinding] into
// the wire-shape [Scope] sub-record.
func scopeFromBinding(b scopebinding.ScopeBinding) Scope {
	return Scope{
		Signature: b.Signature,
		Kind:      b.ScopeKind,
		FilePath:  b.FilePath,
		StartLine: b.StartLine,
		EndLine:   b.EndLine,
	}
}
