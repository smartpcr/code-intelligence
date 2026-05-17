package agentapi

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// stubHealth lets a test inject a fixed (state, err) tuple
// the Service's populateDegraded path consumes.
type stubHealth struct {
	state HealthState
	err   error

	lastRepoID string
	calls      int
}

func (s *stubHealth) HealthForRepo(_ context.Context, repoID string) (HealthState, error) {
	s.calls++
	s.lastRepoID = repoID
	return s.state, s.err
}

func newDegradedSvc(t *testing.T, h HealthSource) (*Service, *recordingSearcher, *allowListFilter) {
	t.Helper()
	emb := fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	search := &recordingSearcher{hits: []embedding.SearchHit{
		{PointID: "p1", Score: 0.9, Payload: map[string]any{"kind": "method"}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"p1": {}}}
	opts := []Option{WithLogger(quietLogger())}
	if h != nil {
		opts = append(opts, WithHealthSource(h))
	}
	svc := NewService(emb, search, filter, opts...)
	return svc, search, filter
}

// TestRecall_degradedFlagPopulatedFromHealthSource is the
// happy-path test for Stage 4.2 Scenario 3: when the Span
// Ingestor has UPSERTed `repo_health.degraded = true` and the
// HealthSource exposes it, the recall response must carry
// `Degraded = true` and the `DegradedReason` ENUM literal.
func TestRecall_degradedFlagPopulatedFromHealthSource(t *testing.T) {
	health := &stubHealth{state: HealthState{
		Degraded: true,
		Reason:   "span_ingestor_backpressure",
		Source:   "span-ingestor",
	}}
	svc, _, _ := newDegradedSvc(t, health)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query: "find me a method", Kind: embedding.NodeKindMethod, K: 5,
		RepoID: "11111111-2222-3333-4444-555555555555",
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !resp.Degraded {
		t.Errorf("Degraded = false, want true")
	}
	if resp.DegradedReason != "span_ingestor_backpressure" {
		t.Errorf("DegradedReason = %q, want %q", resp.DegradedReason, "span_ingestor_backpressure")
	}
	if health.calls != 1 {
		t.Errorf("HealthForRepo calls = %d, want 1", health.calls)
	}
	if health.lastRepoID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("HealthForRepo repo_id = %q", health.lastRepoID)
	}
}

// TestRecall_healthyRepoLeavesDegradedFalse confirms a
// zero-value HealthState (Degraded=false, Reason="") leaves
// the response defaults intact.
func TestRecall_healthyRepoLeavesDegradedFalse(t *testing.T) {
	health := &stubHealth{state: HealthState{}}
	svc, _, _ := newDegradedSvc(t, health)
	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query: "x", Kind: embedding.NodeKindMethod, K: 5,
		RepoID: "11111111-2222-3333-4444-555555555555",
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if resp.Degraded {
		t.Errorf("Degraded = true, want false on healthy repo")
	}
	if resp.DegradedReason != "" {
		t.Errorf("DegradedReason = %q, want empty", resp.DegradedReason)
	}
}

// TestRecall_healthSourceErrorIsSoftFailure confirms the
// rubber-duck contract: an error from HealthForRepo must NOT
// fail the recall; it is logged and the response carries the
// zero-value Degraded fields.
func TestRecall_healthSourceErrorIsSoftFailure(t *testing.T) {
	health := &stubHealth{err: errors.New("connection refused")}
	svc, _, _ := newDegradedSvc(t, health)
	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query: "x", Kind: embedding.NodeKindMethod, K: 5,
		RepoID: "11111111-2222-3333-4444-555555555555",
	})
	if err != nil {
		t.Fatalf("Recall returned error on health failure (want soft-fail): %v", err)
	}
	if resp.Degraded || resp.DegradedReason != "" {
		t.Errorf("Degraded surfaced from error path: %+v", resp)
	}
	if len(resp.Hits) != 1 {
		t.Errorf("Hits = %d, want 1 — recall should still serve results", len(resp.Hits))
	}
}

// TestRecall_nilHealthSourceIsNoop confirms a Service
// constructed without WithHealthSource never calls a health
// source and the response defaults are intact.
func TestRecall_nilHealthSourceIsNoop(t *testing.T) {
	svc, _, _ := newDegradedSvc(t, nil)
	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query: "x", Kind: embedding.NodeKindMethod, K: 5,
		RepoID: "11111111-2222-3333-4444-555555555555",
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if resp.Degraded || resp.DegradedReason != "" {
		t.Errorf("Degraded surfaced without HealthSource: %+v", resp)
	}
}

// TestRecall_unscopedRepoSkipsHealthLookup confirms an
// unscoped recall (RepoID == "") never calls HealthForRepo.
// Stage 4.2 health is per-repo so a global recall has no
// well-defined health state to read.
func TestRecall_unscopedRepoSkipsHealthLookup(t *testing.T) {
	health := &stubHealth{state: HealthState{Degraded: true, Reason: "span_ingestor_backpressure"}}
	svc, _, _ := newDegradedSvc(t, health)
	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query: "x", Kind: embedding.NodeKindMethod, K: 5,
		// RepoID intentionally empty
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if health.calls != 0 {
		t.Errorf("HealthForRepo calls = %d, want 0 on unscoped recall", health.calls)
	}
	if resp.Degraded {
		t.Errorf("Degraded = true on unscoped recall; should be false")
	}
}

// TestRecall_emptyHitsStillSurfacesDegraded proves evaluator
// iter-1 fix #4: when the candidate set is empty (zero qdrant
// hits, or all filtered), the Service must STILL populate
// Degraded/DegradedReason on the response. The prior
// implementation early-returned an empty RecallResponse
// without consulting the HealthSource, suppressing the §C22
// degraded contract for any quiet repo whose Span Ingestor
// was simultaneously backpressured.
func TestRecall_emptyHitsStillSurfacesDegraded(t *testing.T) {
	health := &stubHealth{state: HealthState{
		Degraded: true,
		Reason:   "span_ingestor_backpressure",
		Source:   "span-ingestor",
	}}
	// Wire a searcher that returns ZERO hits + an
	// allow-list that would filter everything anyway.
	emb := fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	search := &recordingSearcher{hits: []embedding.SearchHit{}}
	filter := &allowListFilter{allow: map[string]struct{}{}}
	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithHealthSource(health))

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query: "find me a method", Kind: embedding.NodeKindMethod, K: 5,
		RepoID: "11111111-2222-3333-4444-555555555555",
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Hits) != 0 {
		t.Fatalf("Hits = %d, want 0", len(resp.Hits))
	}
	if !resp.Degraded {
		t.Errorf("Degraded = false, want true on empty-hits path")
	}
	if resp.DegradedReason != "span_ingestor_backpressure" {
		t.Errorf("DegradedReason = %q, want span_ingestor_backpressure", resp.DegradedReason)
	}
	if health.calls != 1 {
		t.Errorf("HealthForRepo calls = %d, want 1 (must be consulted on empty-hits)", health.calls)
	}
}

// TestHealthSourceFunc_adapter validates the adapter type
// satisfies HealthSource (this is also a compile-time guard:
// `var _ HealthSource = HealthSourceFunc(nil)` would catch
// the same drift).
func TestHealthSourceFunc_adapter(t *testing.T) {
	var captured string
	f := HealthSourceFunc(func(_ context.Context, repoID string) (HealthState, error) {
		captured = repoID
		return HealthState{Degraded: true, Reason: "x"}, nil
	})
	state, err := f.HealthForRepo(context.Background(), "repo-A")
	if err != nil {
		t.Fatalf("HealthForRepo: %v", err)
	}
	if captured != "repo-A" {
		t.Errorf("captured = %q, want %q", captured, "repo-A")
	}
	if !state.Degraded || state.Reason != "x" {
		t.Errorf("state = %+v", state)
	}
}
