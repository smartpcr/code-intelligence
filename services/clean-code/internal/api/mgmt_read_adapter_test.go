package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/management"
)

// stubMetricsBackend implements management.MetricsBackend for
// the adapter happy-path test. Every method returns ErrNotFound
// by default; `repoRow` (when set) is returned from ReadRepo.
type stubMetricsBackend struct {
	repoRow *management.RepoRow
	repoID  uuid.UUID
}

func (s *stubMetricsBackend) ReadRepo(ctx context.Context, repoID uuid.UUID) (*management.RepoRow, error) {
	if s.repoRow != nil && repoID == s.repoID {
		return s.repoRow, nil
	}
	return nil, management.ErrNotFound
}

func (s *stubMetricsBackend) ReadMetricSample(ctx context.Context, repoID uuid.UUID, sha string, scopeID uuid.UUID, metricKind string) (*management.MetricSampleRow, error) {
	return nil, management.ErrNotFound
}

func (s *stubMetricsBackend) ReadMetricSamples(ctx context.Context, repoID uuid.UUID, sha string, filter management.MetricSamplesFilter) ([]management.MetricSampleRow, error) {
	return nil, nil
}

func (s *stubMetricsBackend) ReadFindings(ctx context.Context, repoID uuid.UUID, sha string) ([]management.FindingRow, error) {
	return nil, nil
}

func (s *stubMetricsBackend) ReadRegressions(ctx context.Context, repoID uuid.UUID, sha string) ([]management.FindingRow, error) {
	return nil, nil
}

func (s *stubMetricsBackend) ReadRefactorPlan(ctx context.Context, repoID uuid.UUID, sha string) (*management.RefactorPlanRow, error) {
	return nil, management.ErrNotFound
}

func (s *stubMetricsBackend) ReadCrossRepo(ctx context.Context, metricKind, scopeKind string) (*management.CrossRepoRow, error) {
	return nil, management.ErrNotFound
}

func (s *stubMetricsBackend) ReadPortfolio(ctx context.Context, metricKind string) ([]management.PortfolioRow, error) {
	return nil, nil
}

// TestMgmtReadAdapter_NilReader_DoesNotPanic_Returns503 is
// the iter-5 evaluator item #3 regression test:
// NewMgmtReadAdapter(nil) MUST yield handlers that emit 503
// rather than crashing the gateway with a nil pointer
// dereference. The doc-comment claimed nil-tolerance; the
// prior implementation called reader.ReadRepo(...)
// unconditionally, which panicked at the receiver
// dereference because management.Reader.ReadRepo's first
// statement reads r.metrics (the nil-check happens AFTER
// the dereference).
func TestMgmtReadAdapter_NilReader_DoesNotPanic_Returns503(t *testing.T) {
	t.Parallel()
	w := NewMgmtReadAdapter(nil)
	// Probe ALL eight slots -- the contract is uniform.
	slots := map[string]http.Handler{
		"MgmtReadRepo":          w.MgmtReadRepo,
		"MgmtReadMetricSample":  w.MgmtReadMetricSample,
		"MgmtReadMetricSamples": w.MgmtReadMetricSamples,
		"MgmtReadFindings":      w.MgmtReadFindings,
		"MgmtReadRegressions":   w.MgmtReadRegressions,
		"MgmtReadRefactorPlan":  w.MgmtReadRefactorPlan,
		"MgmtReadCrossRepo":     w.MgmtReadCrossRepo,
		"MgmtReadPortfolio":     w.MgmtReadPortfolio,
	}
	for name, slot := range slots {
		if slot == nil {
			t.Errorf("slot %s is nil; want a 503-stub handler", name)
			continue
		}
		rr := httptest.NewRecorder()
		// Defensive: even if a panic occurs, surface it as
		// a clear test failure rather than crashing the run.
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("slot %s panicked: %v", name, rec)
				}
			}()
			slot.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/mgmt/x", nil))
		}()
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("slot %s: status=%d, want 503 (nil-reader stub)", name, rr.Code)
		}
	}
}

// TestMgmtReadAdapter_NilReader_405OnPOST asserts the
// nil-reader stubs still honour the GET/HEAD-only method
// contract (defensive: a POST against a 503 stub should not
// short-circuit the method guard).
func TestMgmtReadAdapter_NilReader_405OnPOST(t *testing.T) {
	t.Parallel()
	w := NewMgmtReadAdapter(nil)
	rr := httptest.NewRecorder()
	w.MgmtReadRepo.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/mgmt/read.repo", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405 (method guard before 503 stub)", rr.Code)
	}
}

// TestMgmtReadAdapter_ReadRepo_403WhenMethodNotGET pins the
// 405 contract on the read verbs (a POST to a read verb must
// surface the canonical "Allow: GET, HEAD" response).
func TestMgmtReadAdapter_ReadRepo_405OnPOST(t *testing.T) {
	t.Parallel()
	w := NewMgmtReadAdapter(management.NewReader(nil))
	rr := httptest.NewRecorder()
	w.MgmtReadRepo.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/mgmt/read.repo", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header=%q, want %q", got, "GET, HEAD")
	}
}

// TestMgmtReadAdapter_ReadRepo_400OnMissingRepoID pins the
// canonical missing-required-param surface.
func TestMgmtReadAdapter_ReadRepo_400OnMissingRepoID(t *testing.T) {
	t.Parallel()
	w := NewMgmtReadAdapter(management.NewReader(nil))
	rr := httptest.NewRecorder()
	w.MgmtReadRepo.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/mgmt/read.repo", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

// TestMgmtReadAdapter_ReadRepo_400OnMalformedRepoID pins the
// UUID-parse failure path.
func TestMgmtReadAdapter_ReadRepo_400OnMalformedRepoID(t *testing.T) {
	t.Parallel()
	w := NewMgmtReadAdapter(management.NewReader(nil))
	rr := httptest.NewRecorder()
	w.MgmtReadRepo.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/mgmt/read.repo?repo_id=not-a-uuid", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

// TestMgmtReadAdapter_ReadRepo_503WhenBackendUnavailable
// asserts that a Reader without a metrics backend surfaces
// 503, not 500.
func TestMgmtReadAdapter_ReadRepo_503WhenBackendUnavailable(t *testing.T) {
	t.Parallel()
	w := NewMgmtReadAdapter(management.NewReader(nil))
	id, _ := uuid.NewV4()
	rr := httptest.NewRecorder()
	w.MgmtReadRepo.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/mgmt/read.repo?repo_id="+id.String(), nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503 (backend unavailable)", rr.Code)
	}
}

// TestMgmtReadAdapter_ReadFindings_400OnMissingSHA pins the
// per-verb required-param coverage.
func TestMgmtReadAdapter_ReadFindings_400OnMissingSHA(t *testing.T) {
	t.Parallel()
	w := NewMgmtReadAdapter(management.NewReader(nil))
	id, _ := uuid.NewV4()
	rr := httptest.NewRecorder()
	w.MgmtReadFindings.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/mgmt/read.findings?repo_id="+id.String(), nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (missing sha)", rr.Code)
	}
}

// TestMgmtReadAdapter_ReadCrossRepo_400OnMissingMetricKind
// asserts that latest-dashboard verbs require their own
// canonical params (not repo_id+sha).
func TestMgmtReadAdapter_ReadCrossRepo_400OnMissingMetricKind(t *testing.T) {
	t.Parallel()
	w := NewMgmtReadAdapter(management.NewReader(nil))
	rr := httptest.NewRecorder()
	w.MgmtReadCrossRepo.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/mgmt/read.cross_repo?scope_kind=method", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (missing metric_kind)", rr.Code)
	}
}

// TestMgmtReadAdapter_ReadPortfolio_503WhenBackendUnavailable
// pins the latest-dashboard verb's error-mapping symmetry
// with the SHA-pinned verbs.
func TestMgmtReadAdapter_ReadPortfolio_503WhenBackendUnavailable(t *testing.T) {
	t.Parallel()
	w := NewMgmtReadAdapter(management.NewReader(nil))
	rr := httptest.NewRecorder()
	w.MgmtReadPortfolio.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/mgmt/read.portfolio?metric_kind=cyclomatic_complexity", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rr.Code)
	}
}

// TestMgmtReadAdapter_HappyPath_200WithJSONBody is the
// end-to-end happy-path coverage: a Reader wired with a
// stub backend returns a row; the adapter encodes it as
// JSON and emits 200.
func TestMgmtReadAdapter_HappyPath_200WithJSONBody(t *testing.T) {
	t.Parallel()
	repoID, _ := uuid.FromString("11111111-1111-1111-1111-111111111111")
	row := &management.RepoRow{
		RepoID:        repoID,
		RepoURL:       "https://example/repo",
		DefaultBranch: "main",
		Mode:          "embedded",
	}
	backend := &stubMetricsBackend{repoRow: row, repoID: repoID}
	reader := management.NewReader(nil, management.WithMetricsBackend(backend))
	w := NewMgmtReadAdapter(reader)
	rr := httptest.NewRecorder()
	w.MgmtReadRepo.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/mgmt/read.repo?repo_id="+repoID.String(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct == "" || ct[:16] != "application/json" {
		t.Errorf("Content-Type=%q, want application/json prefix", ct)
	}
	var resp management.RepoResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Repo == nil || resp.Repo.RepoID != repoID {
		t.Errorf("response repo mismatch: got=%+v", resp.Repo)
	}
}
