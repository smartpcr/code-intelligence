package evaluator

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gofrs/uuid"
)

func TestWalDegradedRunRowJSON_CallerHardcodedEvalGate(t *testing.T) {
	run := DegradedRun{
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "abc",
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		CreatedAt:       time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC).UnixNano(),
	}
	raw, err := walDegradedRunRowJSON(run)
	if err != nil {
		t.Fatalf("walDegradedRunRowJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["caller"] != "eval_gate" {
		t.Fatalf("caller must be hard-coded to eval_gate; got %v", got["caller"])
	}
	if got["scope_id"] != nil {
		t.Fatalf("nil scope_id must render as JSON null; got %v", got["scope_id"])
	}
}

func TestWalDegradedRunRowJSON_NonNilScopeRendersAsUUIDString(t *testing.T) {
	scope := uuid.Must(uuid.NewV4())
	run := DegradedRun{
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "abc",
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		ScopeID:         &scope,
		CreatedAt:       time.Now().UnixNano(),
	}
	raw, err := walDegradedRunRowJSON(run)
	if err != nil {
		t.Fatalf("walDegradedRunRowJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["scope_id"] != scope.String() {
		t.Fatalf("scope_id: got %v want %s", got["scope_id"], scope.String())
	}
}

func TestWalDegradedRunRowJSON_RejectsBadShape(t *testing.T) {
	good := DegradedRun{
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "abc",
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		CreatedAt:       time.Now().UnixNano(),
	}
	cases := []struct {
		name  string
		mutat func(*DegradedRun)
	}{
		{"zero_run_id", func(r *DegradedRun) { r.EvaluationRunID = uuid.Nil }},
		{"zero_repo_id", func(r *DegradedRun) { r.RepoID = uuid.Nil }},
		{"empty_sha", func(r *DegradedRun) { r.SHA = "" }},
		{"zero_policy_version", func(r *DegradedRun) { r.PolicyVersionID = uuid.Nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := good
			tc.mutat(&r)
			if _, err := walDegradedRunRowJSON(r); err == nil {
				t.Fatalf("want error for %s; got nil", tc.name)
			}
		})
	}
}

func TestWalDegradedVerdictRowJSON_RequiresReason(t *testing.T) {
	v := DegradedVerdict{
		VerdictID:       uuid.Must(uuid.NewV4()),
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		Verdict:         VerdictWarn,
		Degraded:        true,
		CreatedAt:       time.Now().UnixNano(),
	}
	if _, err := walDegradedVerdictRowJSON(v); err == nil {
		t.Fatal("want error for empty DegradedReason on degraded path")
	}
}

func TestWalDegradedVerdictRowJSON_HappyShape(t *testing.T) {
	v := DegradedVerdict{
		VerdictID:       uuid.Must(uuid.NewV4()),
		EvaluationRunID: uuid.Must(uuid.NewV4()),
		Verdict:         VerdictWarn,
		Degraded:        true,
		DegradedReason:  DegradedReasonSamplesPending,
		CreatedAt:       time.Now().UnixNano(),
	}
	raw, err := walDegradedVerdictRowJSON(v)
	if err != nil {
		t.Fatalf("walDegradedVerdictRowJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["verdict"] != "warn" {
		t.Fatalf("verdict mismatch: got %v", got["verdict"])
	}
	if got["degraded_reason"] != string(DegradedReasonSamplesPending) {
		t.Fatalf("degraded_reason: got %v", got["degraded_reason"])
	}
	if got["degraded"] != true {
		t.Fatalf("degraded must be true; got %v", got["degraded"])
	}
}
