package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
)

// TestHandleScanRun_RejectsPerRowWithCommitSHA pins iter-7
// evaluator feedback #2: kind='external_per_row' implies
// sha_binding='per_row', and per the canonical
// `scan_run_sha_binding_consistent` CHECK constraint
// (migration 0001 lines 351-389) per_row binding REQUIRES
// `to_sha IS NULL`. The prior handler always inserted
// to_sha=$3 which violated this for `external_per_row`. The
// handler now rejects any non-empty commit_sha for per_row
// kinds at the application layer with HTTP 400 BEFORE the
// INSERT so a per-row scan cannot be silently mis-shaped as
// a single-bound one.
func TestHandleScanRun_RejectsPerRowWithCommitSHA(t *testing.T) {
	repoID := uuid.Must(uuid.NewV4()).String()
	body := `{"commit_sha":"abc123","repo_id":"` + repoID + `","kind":"external_per_row"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/ingestor/scan-run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handleScanRun(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d (per_row + commit_sha MUST be rejected at the app layer)", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "commit_sha must be empty") {
		t.Errorf("body = %q, want error mentioning that commit_sha must be empty for per_row", rr.Body.String())
	}
}

// TestHandleScanRun_RejectsSingleWithEmptyCommitSHA pins
// the dual of the prior test: single-bound kinds require
// to_sha non-null per the CHECK constraint. Reject any
// empty commit_sha at the application layer with HTTP 400.
func TestHandleScanRun_RejectsSingleWithEmptyCommitSHA(t *testing.T) {
	repoID := uuid.Must(uuid.NewV4()).String()
	for _, kind := range []string{"full", "delta", "external_single", "retract"} {
		t.Run(kind, func(t *testing.T) {
			body := `{"commit_sha":"","repo_id":"` + repoID + `","kind":"` + kind + `"}`
			req := httptest.NewRequest(http.MethodPost, "/v1/ingestor/scan-run", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			handleScanRun(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("kind=%q status code = %d, want %d (single-bound kind with empty commit_sha MUST be rejected)", kind, rr.Code, http.StatusBadRequest)
			}
			if !strings.Contains(rr.Body.String(), "commit_sha is required") {
				t.Errorf("kind=%q body = %q, want error mentioning that commit_sha is required", kind, rr.Body.String())
			}
		})
	}
}

// TestHandleScanRun_RejectsUnknownKind pins the unchanged
// behaviour that an unknown kind is rejected with HTTP 400
// BEFORE either the sha_binding switch or any database
// access. Regression guard for the e2e behaviour scenario
// "ScanRun writer is asked to insert kind 'external_double'".
func TestHandleScanRun_RejectsUnknownKind(t *testing.T) {
	repoID := uuid.Must(uuid.NewV4()).String()
	body := `{"commit_sha":"abc","repo_id":"` + repoID + `","kind":"external_double"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/ingestor/scan-run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handleScanRun(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d (unknown scan_run kind MUST be rejected)", rr.Code, http.StatusBadRequest)
	}
}

// TestHandleScanRun_RejectsMissingRepoID pins the
// repo_id-required guard: the canonical scan_run schema has
// repo_id NOT NULL with an FK to clean_code.repo (migration
// 0001 lines 341-343), so the handler MUST reject any
// request that omits or mangles repo_id BEFORE the INSERT.
func TestHandleScanRun_RejectsMissingRepoID(t *testing.T) {
	body := `{"commit_sha":"abc","repo_id":"","kind":"full"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/ingestor/scan-run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handleScanRun(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d (empty repo_id MUST be rejected)", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "repo_id is required") {
		t.Errorf("body = %q, want error mentioning that repo_id is required", rr.Body.String())
	}
}
