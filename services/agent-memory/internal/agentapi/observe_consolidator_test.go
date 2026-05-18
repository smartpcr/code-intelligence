package agentapi

// Stage 8.1 / C24 contract test:
//
//	"An `agent.observe` call NEVER fails because the
//	 Consolidator is backpressured; the Episode is queued
//	 and `degraded_reason` set."
//
// The test wires a `ConsolidatorBackpressureSource` that
// always reports backpressure, fires 100 sequential
// `Observe` calls (the "burst" pinned by §13 scenario
// "observe never fails on consolidator pressure"), and
// asserts:
//
//   - every response is `Degraded=true,
//     DegradedReason="consolidator_backpressure"`,
//   - every call succeeded (no error),
//   - exactly 100 Episode rows were Append-ed (so the
//     queued-not-dropped invariant holds),
//   - every Append-ed row carries the §6.3 row-level
//     `degraded=true, degraded_reason='consolidator_backpressure'`
//     flag so `mgmt.read.episodes` later observes the same
//     reason the agent saw on the wire,
//   - the per-verb degraded counter reports 100 increments
//     under `(agent.observe, consolidator_backpressure)`.

import (
	"context"
	"sync"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/degraded"
)

// stubBackpressureSource is a `ConsolidatorBackpressureSource`
// that returns a configurable fixed value. Records each call's
// repoID so a test can assert the verb passed the request's
// repo through verbatim.
type stubBackpressureSource struct {
	mu      sync.Mutex
	bp      bool
	err     error
	repoIDs []string
}

func (s *stubBackpressureSource) Backpressured(_ context.Context, repoID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repoIDs = append(s.repoIDs, repoID)
	return s.bp, s.err
}

func (s *stubBackpressureSource) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.repoIDs))
	copy(out, s.repoIDs)
	return out
}

func TestObserve_consolidatorBackpressure_burst(t *testing.T) {
	const burst = 100

	writer := &fakeEpisodeWriter{}
	resolver := &fakeContextResolver{}
	bp := &stubBackpressureSource{bp: true}
	metric := degraded.NewCounter()

	svc := newTestService(t, writer, resolver,
		WithObserveConsolidatorBackpressure(bp),
		WithObserveDegradedMetric(metric),
	)

	ctx := context.Background()
	for i := 0; i < burst; i++ {
		resp, err := svc.Observe(ctx, validReq())
		if err != nil {
			t.Fatalf("call %d: Observe must NEVER fail on consolidator backpressure: %v", i, err)
		}
		if !resp.Degraded {
			t.Fatalf("call %d: resp.Degraded=false, want true", i)
		}
		if resp.DegradedReason != degraded.ReasonConsolidatorBackpressure {
			t.Fatalf("call %d: resp.DegradedReason=%q, want %q",
				i, resp.DegradedReason, degraded.ReasonConsolidatorBackpressure)
		}
		if resp.EpisodeID == "" {
			t.Fatalf("call %d: resp.EpisodeID empty — Episode must still be queued", i)
		}
	}

	rows := writer.snapshot()
	if len(rows) != burst {
		t.Fatalf("Append calls = %d, want %d (every burst Observe MUST queue an Episode)",
			len(rows), burst)
	}
	for i, row := range rows {
		if !row.Degraded {
			t.Fatalf("row %d: Episode row Degraded=false, want true (§6.3 row-level flag)", i)
		}
		if row.DegradedReason != degraded.ReasonConsolidatorBackpressure {
			t.Fatalf("row %d: Episode row DegradedReason=%q, want %q",
				i, row.DegradedReason, degraded.ReasonConsolidatorBackpressure)
		}
	}

	probes := bp.snapshot()
	if len(probes) != burst {
		t.Fatalf("Backpressure probes = %d, want %d (one per Observe)", len(probes), burst)
	}
	for i, repoID := range probes {
		if repoID != "repo-uuid" {
			t.Fatalf("probe %d: repoID = %q, want %q (verb MUST pass req.RepoID through)",
				i, repoID, "repo-uuid")
		}
	}

	got := metric.Count(VerbObserve, degraded.ReasonConsolidatorBackpressure)
	if got != burst {
		t.Fatalf("degraded metric %s/%s = %d, want %d",
			VerbObserve, degraded.ReasonConsolidatorBackpressure, got, burst)
	}
}

// Backpressure probe errors must NOT fail the observe call —
// the §8.3 invariant promises agent.observe is the most
// resilient verb on the API.  A flaky probe is treated as
// "healthy" and the response carries no consolidator reason.
func TestObserve_consolidatorBackpressure_probeError_treatedHealthy(t *testing.T) {
	writer := &fakeEpisodeWriter{}
	resolver := &fakeContextResolver{}
	bp := &stubBackpressureSource{err: context.DeadlineExceeded}

	svc := newTestService(t, writer, resolver,
		WithObserveConsolidatorBackpressure(bp),
	)

	resp, err := svc.Observe(context.Background(), validReq())
	if err != nil {
		t.Fatalf("Observe must not fail on a backpressure probe error: %v", err)
	}
	if resp.Degraded {
		t.Fatalf("resp.Degraded=true on probe error, want false (treated as healthy)")
	}
	if resp.DegradedReason != "" {
		t.Fatalf("resp.DegradedReason=%q on probe error, want empty", resp.DegradedReason)
	}

	rows := writer.snapshot()
	if len(rows) != 1 {
		t.Fatalf("Append calls = %d, want 1", len(rows))
	}
	if rows[0].Degraded {
		t.Fatalf("Episode row stamped degraded on probe error, want false")
	}
}
