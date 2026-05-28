package rule_engine

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
)

// walTimeFormat is the canonical RFC3339-style UTC timestamp
// the WAL row JSON uses. The reconciler parses with the same
// layout when replaying via the matching INSERT statement.
const walTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// walEvaluationRunRowJSON returns the column-keyed
// snake_case JSON the WAL stages for an
// `evaluation_run` insert. The shape mirrors
// [appendEvaluationInTx]'s INSERT positional arguments so the
// Stage 9.2 reconciler can pass the same column list back
// into PostgreSQL without rewriting the row.
//
// Notes:
//   - `scope_id` is nullable in SQL (migration 0008); a nil
//     Go pointer renders as JSON null, matching the SQL NULL
//     path. A non-nil pointer renders as a UUID string.
//   - `created_at` renders as UTC with nanosecond precision
//     so a round-trip through PG `timestamptz` preserves it.
func walEvaluationRunRowJSON(run EvaluationRun) ([]byte, error) {
	if run.EvaluationRunID == uuid.Nil {
		return nil, errors.New("rule_engine: walEvaluationRunRowJSON: run.EvaluationRunID is the zero uuid")
	}
	if run.RepoID == uuid.Nil {
		return nil, errors.New("rule_engine: walEvaluationRunRowJSON: run.RepoID is the zero uuid")
	}
	if run.SHA == "" {
		return nil, errors.New("rule_engine: walEvaluationRunRowJSON: run.SHA is empty")
	}
	if run.PolicyVersionID == uuid.Nil {
		return nil, errors.New("rule_engine: walEvaluationRunRowJSON: run.PolicyVersionID is the zero uuid")
	}
	if !run.Caller.IsValid() {
		return nil, fmt.Errorf("rule_engine: walEvaluationRunRowJSON: run.Caller=%q is not canonical", run.Caller)
	}
	var scopeID any
	if run.ScopeID != nil {
		scopeID = run.ScopeID.String()
	}
	row := map[string]any{
		"evaluation_run_id": run.EvaluationRunID.String(),
		"repo_id":           run.RepoID.String(),
		"sha":               run.SHA,
		"policy_version_id": run.PolicyVersionID.String(),
		"caller":            string(run.Caller),
		"scope_id":          scopeID,
		"created_at":        run.CreatedAt.UTC().Format(walTimeFormat),
	}
	return json.Marshal(row)
}

// walEvaluationVerdictRowJSON returns the column-keyed
// snake_case JSON for an `evaluation_verdict` insert. The
// `degraded_reason` column is rendered as JSON null when the
// in-memory Go string is empty, mirroring the
// `NULLIF($5, '')` cast in [appendEvaluationInTx].
func walEvaluationVerdictRowJSON(verdict EvaluationVerdict) ([]byte, error) {
	if verdict.VerdictID == uuid.Nil {
		return nil, errors.New("rule_engine: walEvaluationVerdictRowJSON: verdict.VerdictID is the zero uuid")
	}
	if verdict.EvaluationRunID == uuid.Nil {
		return nil, errors.New("rule_engine: walEvaluationVerdictRowJSON: verdict.EvaluationRunID is the zero uuid")
	}
	if !verdict.Verdict.IsValid() {
		return nil, fmt.Errorf("rule_engine: walEvaluationVerdictRowJSON: verdict.Verdict=%q is not canonical", verdict.Verdict)
	}
	var reason any
	if verdict.DegradedReason != "" {
		reason = verdict.DegradedReason
	}
	row := map[string]any{
		"verdict_id":        verdict.VerdictID.String(),
		"evaluation_run_id": verdict.EvaluationRunID.String(),
		"verdict":           string(verdict.Verdict),
		"degraded":          verdict.Degraded,
		"degraded_reason":   reason,
		"created_at":        verdict.CreatedAt.UTC().Format(walTimeFormat),
	}
	return json.Marshal(row)
}

// walFindingRowJSON returns the column-keyed snake_case JSON
// for a `finding` insert. `metric_sample_ids` is always
// rendered as a JSON array (never null) so the reconciler can
// pass it back through the same `$9::jsonb` cast on replay.
// `scope_id` is NOT NULL on the finding table (migration 0003)
// so the zero UUID is rejected here.
func walFindingRowJSON(f Finding) ([]byte, error) {
	if f.FindingID == uuid.Nil {
		return nil, errors.New("rule_engine: walFindingRowJSON: f.FindingID is the zero uuid")
	}
	if f.EvaluationRunID == uuid.Nil {
		return nil, errors.New("rule_engine: walFindingRowJSON: f.EvaluationRunID is the zero uuid")
	}
	if f.RepoID == uuid.Nil {
		return nil, errors.New("rule_engine: walFindingRowJSON: f.RepoID is the zero uuid")
	}
	if f.SHA == "" {
		return nil, errors.New("rule_engine: walFindingRowJSON: f.SHA is empty")
	}
	if f.ScopeID == uuid.Nil {
		return nil, errors.New("rule_engine: walFindingRowJSON: f.ScopeID is the zero uuid (NOT NULL in finding schema)")
	}
	if f.RuleID == "" {
		return nil, errors.New("rule_engine: walFindingRowJSON: f.RuleID is empty")
	}
	if f.PolicyVersionID == uuid.Nil {
		return nil, errors.New("rule_engine: walFindingRowJSON: f.PolicyVersionID is the zero uuid")
	}
	samples := make([]string, 0, len(f.MetricSampleIDs))
	for _, s := range f.MetricSampleIDs {
		samples = append(samples, s.String())
	}
	row := map[string]any{
		"finding_id":        f.FindingID.String(),
		"evaluation_run_id": f.EvaluationRunID.String(),
		"repo_id":           f.RepoID.String(),
		"sha":               f.SHA,
		"scope_id":          f.ScopeID.String(),
		"rule_id":           f.RuleID,
		"rule_version":      f.RuleVersion,
		"policy_version_id": f.PolicyVersionID.String(),
		"metric_sample_ids": samples,
		"severity":          string(f.Severity),
		"delta":             string(f.Delta),
		"explanation_md":    f.ExplanationMD,
		"created_at":        f.CreatedAt.UTC().Format(walTimeFormat),
	}
	return json.Marshal(row)
}
