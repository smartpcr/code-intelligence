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
