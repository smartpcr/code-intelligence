package evaluator

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"
)

// walTimeFormat is the canonical RFC3339-style UTC timestamp
// the WAL row JSON uses. Must match the rule_engine's format
// so the Stage 9.2 reconciler parses both writers' frames
// with one layout.
const walTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// walDegradedRunRowJSON returns the column-keyed snake_case
// JSON for a degraded-path `evaluation_run` insert. The
// `caller` is hard-coded to `eval_gate` because the gate is
// the only writer that reaches this path (verified by the
// SQL literal in [SQLDegradedRunStore.AppendDegradedRun]).
// `scope_id` renders as JSON null when the in-memory pointer
// is nil (the canonical whole-SHA path), matching the SQL
// NULL the writer passes via lib/pq.
func walDegradedRunRowJSON(run DegradedRun) ([]byte, error) {
	if run.EvaluationRunID == uuid.Nil {
		return nil, errors.New("evaluator: walDegradedRunRowJSON: run.EvaluationRunID is the zero uuid")
	}
	if run.RepoID == uuid.Nil {
		return nil, errors.New("evaluator: walDegradedRunRowJSON: run.RepoID is the zero uuid")
	}
	if run.SHA == "" {
		return nil, errors.New("evaluator: walDegradedRunRowJSON: run.SHA is empty")
	}
	if run.PolicyVersionID == uuid.Nil {
		return nil, errors.New("evaluator: walDegradedRunRowJSON: run.PolicyVersionID is the zero uuid")
	}
	var scopeID any
	if run.ScopeID != nil {
		scopeID = run.ScopeID.String()
	}
	createdAt := time.Unix(0, run.CreatedAt).UTC().Format(walTimeFormat)
	row := map[string]any{
		"evaluation_run_id": run.EvaluationRunID.String(),
		"repo_id":           run.RepoID.String(),
		"sha":               run.SHA,
		"policy_version_id": run.PolicyVersionID.String(),
		"caller":            "eval_gate",
		"scope_id":          scopeID,
		"created_at":        createdAt,
	}
	return json.Marshal(row)
}

// walDegradedVerdictRowJSON returns the column-keyed
// snake_case JSON for a degraded-path `evaluation_verdict`
// insert. `degraded_reason` is non-empty by construction (the
// caller validates that BEFORE staging), so the
// `degraded_reason` field always renders as a non-null
// string.
func walDegradedVerdictRowJSON(verdict DegradedVerdict) ([]byte, error) {
	if verdict.VerdictID == uuid.Nil {
		return nil, errors.New("evaluator: walDegradedVerdictRowJSON: verdict.VerdictID is the zero uuid")
	}
	if verdict.EvaluationRunID == uuid.Nil {
		return nil, errors.New("evaluator: walDegradedVerdictRowJSON: verdict.EvaluationRunID is the zero uuid")
	}
	if !verdict.Verdict.IsValid() {
		return nil, fmt.Errorf("evaluator: walDegradedVerdictRowJSON: verdict.Verdict=%q is not canonical", verdict.Verdict)
	}
	if verdict.DegradedReason == "" {
		return nil, errors.New("evaluator: walDegradedVerdictRowJSON: verdict.DegradedReason is empty (degraded paths MUST carry a reason)")
	}
	createdAt := time.Unix(0, verdict.CreatedAt).UTC().Format(walTimeFormat)
	row := map[string]any{
		"verdict_id":        verdict.VerdictID.String(),
		"evaluation_run_id": verdict.EvaluationRunID.String(),
		"verdict":           string(verdict.Verdict),
		"degraded":          verdict.Degraded,
		"degraded_reason":   string(verdict.DegradedReason),
		"created_at":        createdAt,
	}
	return json.Marshal(row)
}
