package rule_engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

func TestWalEvaluationRunRowJSON_NilScopeRendersAsNull(t *testing.T) {
	run := EvaluationRun{
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "abc123",
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		Caller:          CallerBatchRefresh,
		ScopeID:         nil,
		CreatedAt:       time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC),
	}
	raw, err := walEvaluationRunRowJSON(run)
	if err != nil {
		t.Fatalf("walEvaluationRunRowJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["scope_id"] != nil {
		t.Fatalf("scope_id must be null; got %v", got["scope_id"])
	}
	if got["caller"] != "batch_refresh" {
		t.Fatalf("caller mismatch: %v", got["caller"])
	}
	if !strings.Contains(string(raw), `"sha":"abc123"`) {
		t.Fatalf("sha not encoded literally: %s", raw)
	}
}

func TestWalEvaluationRunRowJSON_NonNilScopeRendersAsUUIDString(t *testing.T) {
	scope := uuid.Must(uuid.NewV4())
	run := EvaluationRun{
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "abc",
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		Caller:          CallerEvalGate,
		ScopeID:         &scope,
		CreatedAt:       time.Now().UTC(),
	}
	raw, err := walEvaluationRunRowJSON(run)
	if err != nil {
		t.Fatalf("walEvaluationRunRowJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["scope_id"] != scope.String() {
		t.Fatalf("scope_id: got %v want %s", got["scope_id"], scope.String())
	}
}

func TestWalEvaluationRunRowJSON_RejectsBadShape(t *testing.T) {
	good := EvaluationRun{
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "abc",
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		Caller:          CallerEvalGate,
		CreatedAt:       time.Now().UTC(),
	}
	cases := []struct {
		name  string
		mutat func(*EvaluationRun)
	}{
		{"zero_run_id", func(r *EvaluationRun) { r.EvaluationRunID = uuid.Nil }},
		{"zero_repo_id", func(r *EvaluationRun) { r.RepoID = uuid.Nil }},
		{"empty_sha", func(r *EvaluationRun) { r.SHA = "" }},
		{"zero_policy_version", func(r *EvaluationRun) { r.PolicyVersionID = uuid.Nil }},
		{"bad_caller", func(r *EvaluationRun) { r.Caller = "bad" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := good
			tc.mutat(&r)
			if _, err := walEvaluationRunRowJSON(r); err == nil {
				t.Fatalf("want error for %s; got nil", tc.name)
			}
		})
	}
}

func TestWalEvaluationVerdictRowJSON_EmptyReasonRendersAsNull(t *testing.T) {
	v := EvaluationVerdict{
		VerdictID:       uuid.Must(uuid.NewV4()),
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		Verdict:         VerdictPass,
		Degraded:        false,
		DegradedReason:  "",
		CreatedAt:       time.Now().UTC(),
	}
	raw, err := walEvaluationVerdictRowJSON(v)
	if err != nil {
		t.Fatalf("walEvaluationVerdictRowJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["degraded_reason"] != nil {
		t.Fatalf("degraded_reason must be JSON null; got %v", got["degraded_reason"])
	}
}

func TestWalEvaluationVerdictRowJSON_NonEmptyReasonRendersAsString(t *testing.T) {
	v := EvaluationVerdict{
		VerdictID:       uuid.Must(uuid.NewV4()),
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		Verdict:         VerdictBlock,
		Degraded:        true,
		DegradedReason:  "samples_pending",
		CreatedAt:       time.Now().UTC(),
	}
	raw, err := walEvaluationVerdictRowJSON(v)
	if err != nil {
		t.Fatalf("walEvaluationVerdictRowJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["degraded_reason"] != "samples_pending" {
		t.Fatalf("degraded_reason: got %v", got["degraded_reason"])
	}
	if got["degraded"] != true {
		t.Fatalf("degraded: got %v", got["degraded"])
	}
}

func TestWalFindingRowJSON_MetricSamplesAlwaysArray(t *testing.T) {
	f := Finding{
		FindingID:       uuid.Must(uuid.NewV4()),
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "abc",
		ScopeID:         uuid.Must(uuid.NewV4()),
		RuleID:          "fan-in",
		RuleVersion:     1,
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		MetricSampleIDs: nil,
		Severity:        steward.SeverityWarn,
		Delta:           DeltaNew,
		ExplanationMD:   "exp",
		CreatedAt:       time.Now().UTC(),
	}
	raw, err := walFindingRowJSON(f)
	if err != nil {
		t.Fatalf("walFindingRowJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	arr, ok := got["metric_sample_ids"].([]any)
	if !ok {
		t.Fatalf("metric_sample_ids must be array (never null); got %T", got["metric_sample_ids"])
	}
	if len(arr) != 0 {
		t.Fatalf("metric_sample_ids: want empty array; got %v", arr)
	}
}

func TestWalFindingRowJSON_RejectsZeroScopeID(t *testing.T) {
	f := Finding{
		FindingID:       uuid.Must(uuid.NewV4()),
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "abc",
		ScopeID:         uuid.Nil,
		RuleID:          "fan-in",
		RuleVersion:     1,
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		Severity:        steward.SeverityWarn,
		Delta:           DeltaNew,
		CreatedAt:       time.Now().UTC(),
	}
	if _, err := walFindingRowJSON(f); err == nil {
		t.Fatal("want error for zero ScopeID (finding.scope_id is NOT NULL)")
	}
}
