package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
)

// TestClassifyPlannerError pins the Stage 9.3 iter-9 error
// taxonomy the HTTP planner surface uses to drive E2E branching.
// Categories are stable strings the harness asserts on; if a
// future refactor renames one of these the E2E suite breaks
// loudly rather than silently mis-attributing a failure.
func TestClassifyPlannerError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"version-mismatch", refactor.ErrMLModelVersionMismatch, "version-mismatch"},
		{"wrapped-version-mismatch", fmt.Errorf("planner: %w", refactor.ErrMLModelVersionMismatch), "version-mismatch"},
		{"uri-missing", refactor.ErrMLModelURIMissing, "ml-model"},
		{"version-missing", refactor.ErrMLModelVersionMissing, "ml-model"},
		{"artefact-invalid", refactor.ErrMLModelArtefactInvalid, "ml-model"},
		{"unknown-source", refactor.ErrUnknownEffortSource, "ml-model"},
		{"nil-model", refactor.ErrNilEffortModel, "ml-model"},
		{"invalid-estimate", refactor.ErrInvalidEffortEstimate, "ml-model"},
		{"cancelled", context.Canceled, "cancelled"},
		{"deadline", context.DeadlineExceeded, "cancelled"},
		{"opaque", errors.New("unexpected"), "internal"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPlannerError(tc.err); got != tc.want {
				t.Fatalf("classifyPlannerError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestPlannerRunHandler_RejectsBadInput verifies the input
// validation branches of the HTTP handler without needing a real
// PG handle. The handler must fail fast on the wrong method, a
// malformed JSON body, missing fields, and an invalid UUID --
// each with a stable error envelope.
func TestPlannerRunHandler_RejectsBadInput(t *testing.T) {
	h := &plannerRunHandler{}

	t.Run("wrong-method", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/planner/run", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status=%d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("malformed-json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/planner/run",
			strings.NewReader("{not json"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", rec.Code, http.StatusBadRequest)
		}
		var resp plannerRunResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Status != "error" || resp.Error == "" {
			t.Fatalf("malformed-json: got %+v", resp)
		}
	})

	t.Run("missing-fields", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/planner/run",
			strings.NewReader(`{"repo_id":"","sha":""}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid-uuid", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/planner/run",
			strings.NewReader(`{"repo_id":"not-a-uuid","sha":"abc"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", rec.Code, http.StatusBadRequest)
		}
		var resp plannerRunResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !strings.Contains(resp.Error, "not a valid UUID") {
			t.Fatalf("invalid-uuid: error %q does not mention UUID", resp.Error)
		}
	})
}
