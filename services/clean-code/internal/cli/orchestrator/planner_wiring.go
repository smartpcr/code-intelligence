// -----------------------------------------------------------------------
// <copyright file="planner_wiring.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package orchestrator

import (
	"context"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// cliPolicyReader is the [refactor.PolicyReader] implementation
// the CLI composition root wires into [refactor.NewPlanner] in
// scaffold mode (no PostgreSQL, no `steward.Steward`).
//
// Where the production [refactor.StewardPolicyReader] dereferences
// the latest `policy_activation` row through the steward, this
// adapter projects the dev-mode [devpolicy.Bundle] -- itself the
// product of `internal/cli/devpolicy.LoadEmbedded` -- onto the
// same [refactor.PolicySnapshot] shape so the planner cannot
// distinguish the two reader sources by their return values.
//
// The bundle carries an UNSIGNED [steward.PolicyVersion] minted
// from the embedded rule-pack YAML; that is the policy the CLI
// run is being scored against, and its
// [steward.PolicyVersion.RefactorWeights] are the weights every
// `hot_spot.score` will be computed with. Returning `ok=true`
// unconditionally is the right semantics for scaffold mode --
// there is ALWAYS an active policy in a dev-mode CLI run (the
// loader either succeeded and the bundle exists, or the
// `Orchestrator.Run` never reached this layer).
type cliPolicyReader struct {
	bundle devpolicy.Bundle
}

// NewCLIPolicyReader wraps a dev-mode [devpolicy.Bundle] as a
// [refactor.PolicyReader] satisfying the planner contract at
// `services/clean-code/internal/refactor/planner.go:39`.
//
// The returned reader is value-shareable across goroutines: it
// holds NO state beyond the immutable bundle reference.
func NewCLIPolicyReader(bundle devpolicy.Bundle) refactor.PolicyReader {
	return &cliPolicyReader{bundle: bundle}
}

// ActivePolicyVersion implements [refactor.PolicyReader] by
// projecting the bundle's PolicyVersionID + RefactorWeights onto
// a [refactor.PolicySnapshot]. The `ok` return is ALWAYS true in
// scaffold mode -- the bundle is the active policy by
// construction.
//
// The returned error mirrors the [steward.Steward.ActivePolicyVersion]
// contract: a context-cancellation error from the caller is the
// only failure path; the in-memory projection itself cannot
// fail.
func (r *cliPolicyReader) ActivePolicyVersion(ctx context.Context) (refactor.PolicySnapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return refactor.PolicySnapshot{}, false, err
	}
	return refactor.PolicySnapshot{
		PolicyVersionID: r.bundle.PolicyVersion.PolicyVersionID,
		Weights:         r.bundle.PolicyVersion.RefactorWeights,
	}, true, nil
}

// BuildMetricSampleReader returns a freshly-populated
// [refactor.InMemoryMetricSampleReader] seeded with every
// [rule_engine.Sample] whose `MetricKind` is in the closed
// [refactor.HotSpotInputMetricKinds] set (`cyclo`,
// `cognitive_complexity`, `modification_count_in_window`,
// `coupling_between_objects`, `fan_out`).
//
// Filtering at the CLI wiring layer mirrors the production
// [refactor.SQLMetricSampleReader.ScopeMetrics] query, which
// passes the same closed set as a `metric_kind = ANY(...)`
// predicate. Pre-filtering means the reader's slice is
// proportional to the foundation-tier corpus -- not the full
// engine sample set -- which keeps the per-Plan() linear scan
// cheap and matches the architecture Sec 3.9 "five foundation
// metrics feed hotspot scoring" invariant.
//
// Samples whose `HasValue` is false are skipped: the recipe
// fleet documents (`internal/metrics/recipes/recipe.go`) that a
// missing input produces no draft, so a `HasValue=false` Sample
// here is a wiring bug, not an empty cell, and feeding it into
// the reader would mint a row with `value=0.0` that the
// planner would treat as a legitimate measurement.
//
// The returned reader is non-nil even when `samples` is nil so
// the caller can pass it directly into [refactor.NewPlanner]
// without an additional nil guard.
func BuildMetricSampleReader(samples []rule_engine.Sample) *refactor.InMemoryMetricSampleReader {
	r := refactor.NewInMemoryMetricSampleReader()
	if len(samples) == 0 {
		return r
	}
	keep := make(map[string]struct{}, len(refactor.HotSpotInputMetricKinds))
	for _, k := range refactor.HotSpotInputMetricKinds {
		keep[k] = struct{}{}
	}
	for _, s := range samples {
		if _, ok := keep[s.MetricKind]; !ok {
			continue
		}
		if !s.HasValue {
			continue
		}
		r.Insert(refactor.InMemoryMetricSample{
			RepoID:        s.RepoID,
			SHA:           s.SHA,
			ScopeID:       s.ScopeID,
			MetricKind:    s.MetricKind,
			MetricVersion: s.MetricVersion,
			Value:         s.Value,
		})
	}
	return r
}

// BuildFindingReader returns a freshly-populated
// [refactor.InMemoryFindingReader] seeded with every
// [rule_engine.Finding] whose `Delta` is in
// [refactor.HotSpotQualifyingDeltas] -- the canonical
// `delta IN ('new', 'newly_failing')` filter from architecture
// Sec 3.5 and Sec 5.4.1 lines 1186-1190.
//
// The filter intentionally drops `unchanged` (chronic findings
// already on the planner's radar) and `resolved` (would invert
// the signal -- the planner would prioritise a scope precisely
// because it just got fixed). Pre-filtering at the wiring layer
// matches the production [refactor.SQLFindingReader] which
// pushes the same predicate into a SQL `WHERE delta IN (...)`
// clause.
//
// Findings whose `PolicyVersionID` is the zero UUID are kept --
// the reader's [refactor.InMemoryFindingReader.FindingCountsByScope]
// method already filters by the active policy_version_id at
// COUNT time, so the responsibility for "right policy"
// attribution stays in the reader where the SQL impl also
// enforces it.
//
// The returned reader is non-nil even when `findings` is nil so
// the caller can pass it directly into [refactor.NewPlanner]
// without an additional nil guard.
func BuildFindingReader(findings []rule_engine.Finding) *refactor.InMemoryFindingReader {
	r := refactor.NewInMemoryFindingReader()
	if len(findings) == 0 {
		return r
	}
	for _, f := range findings {
		if !refactor.IsHotSpotQualifyingDelta(f.Delta) {
			continue
		}
		r.Insert(refactor.InMemoryFinding{
			RepoID:          f.RepoID,
			SHA:             f.SHA,
			ScopeID:         f.ScopeID,
			PolicyVersionID: f.PolicyVersionID,
			Delta:           f.Delta,
		})
	}
	return r
}

// BuildFindingDetailReader returns a freshly-populated
// [refactor.InMemoryFindingDetailReader] seeded with every
// [rule_engine.Finding] whose `Delta` is in
// [refactor.HotSpotQualifyingDeltas]. Unlike the Stage 8.1
// count-only [refactor.InMemoryFindingReader], the Stage 8.2
// detail reader carries the `RuleID` per row so the
// [refactor.TaskPlanner] can map a finding's rule_id onto a
// canonical [refactor.TaskKind] via
// [refactor.DefaultTaskKindForRule].
//
// The pre-filter on Delta mirrors the architecture Sec 3.5 /
// Sec 5.4.1 `delta IN ('new', 'newly_failing')` invariant and
// matches the SQL [refactor.SQLFindingDetailReader] which
// pushes the same predicate into a `WHERE delta IN (...)`
// clause. The reader's own [refactor.InMemoryFindingDetailReader.FindingDetails]
// method ALSO filters by qualifying delta + policy_version_id
// + scope membership; pre-filtering here keeps the in-memory
// row count proportional to the foundation corpus rather than
// the whole engine finding set.
//
// Returns a non-nil reader even for nil/empty findings so the
// caller can pass it directly into [refactor.NewTaskPlanner].
func BuildFindingDetailReader(findings []rule_engine.Finding) *refactor.InMemoryFindingDetailReader {
	r := refactor.NewInMemoryFindingDetailReader()
	if len(findings) == 0 {
		return r
	}
	for _, f := range findings {
		if !refactor.IsHotSpotQualifyingDelta(f.Delta) {
			continue
		}
		r.Insert(refactor.InMemoryFindingWithRule{
			InMemoryFinding: refactor.InMemoryFinding{
				RepoID:          f.RepoID,
				SHA:             f.SHA,
				ScopeID:         f.ScopeID,
				PolicyVersionID: f.PolicyVersionID,
				Delta:           f.Delta,
			},
			RuleID: f.RuleID,
		})
	}
	return r
}

// BuildHotSpotReader returns a freshly-populated
// [refactor.InMemoryHotSpotReader] seeded with the hot_spot
// rows the Stage 8.1 [refactor.Planner.Plan] just persisted
// (i.e. `planResult.HotSpots`).
//
// The Stage 8.2 [refactor.TaskPlanner.PlanFromSnapshot]
// canonical wiring reads the SAME batch the Stage 8.1 planner
// produced -- the reader's [refactor.InMemoryHotSpotReader.LatestHotSpotsByScore]
// implementation filters by `(repo_id, sha,
// policy_version_id)` and returns the latest `CreatedAt`
// cohort, so feeding the just-emitted batch through this
// helper exactly mirrors the SQL [refactor.SQLHotSpotReader]
// path the production composition root uses.
//
// Returns a non-nil reader even for nil/empty input so the
// caller can pass it directly into [refactor.NewTaskPlanner]
// without an additional nil guard.
func BuildHotSpotReader(rows []refactor.HotSpot) *refactor.InMemoryHotSpotReader {
	r := refactor.NewInMemoryHotSpotReader()
	if len(rows) == 0 {
		return r
	}
	r.InsertBatch(rows)
	return r
}

// NewTaskPlannerWiring is the CLI composition root convenience
// that wires a [refactor.TaskPlanner] from the planner-level
// outputs the orchestrator already has on hand:
//
//   - `bundle` -- the dev-mode [devpolicy.Bundle] providing the
//     active PolicyVersion + RefactorWeights (same source as
//     [NewCLIPolicyReader] for Stage 8.1).
//   - `planHotSpots` -- the [refactor.PlanResult.HotSpots] slice
//     from the Stage 8.1 [refactor.Planner.Plan] call (in
//     score-DESC, scope_id-ASC order).
//   - `findings` -- the engine's `[]rule_engine.Finding` snapshot
//     (e.g. `rule_engine.InMemoryStore.Findings()`); filtered
//     to qualifying deltas at wiring time.
//
// Returns the TaskPlanner plus the [refactor.InMemoryRefactorPlanTaskWriter]
// so the composition root can inspect emitted plans/tasks for
// serialisation into the CLI's report.md / findings.json
// surfaces.
//
// Composition root usage (Stage 8.1 -> Stage 8.2):
//
//	planRes, _ := planner.Plan(ctx, repoID, sha)
//	tp, writer, _ := orchestrator.NewTaskPlannerWiring(bundle,
//	    planRes.HotSpots, store.Findings())
//	taskRes, _ := tp.PlanFromSnapshot(ctx, repoID, sha, planRes.Snapshot)
//	plans := writer.Plans()  // for report.md
//	tasks := writer.Tasks()  // for findings.json / prompts.jsonl
//
// `PlanFromSnapshot` is the race-safe entrypoint (rubber-duck
// iter-2 finding #1 on architecture Sec 3.5) -- the Stage 8.1
// snapshot is REUSED so the two planner passes pin the same
// `policy_version_id`.
func NewTaskPlannerWiring(
	bundle devpolicy.Bundle,
	planHotSpots []refactor.HotSpot,
	findings []rule_engine.Finding,
	opts ...refactor.TaskOption,
) (*refactor.TaskPlanner, *refactor.InMemoryRefactorPlanTaskWriter, error) {
	policyR := NewCLIPolicyReader(bundle)
	hotSpotR := BuildHotSpotReader(planHotSpots)
	detailR := BuildFindingDetailReader(findings)
	writer := refactor.NewInMemoryRefactorPlanTaskWriter()
	tp, err := refactor.NewTaskPlanner(policyR, hotSpotR, detailR, writer, opts...)
	if err != nil {
		return nil, nil, err
	}
	return tp, writer, nil
}
