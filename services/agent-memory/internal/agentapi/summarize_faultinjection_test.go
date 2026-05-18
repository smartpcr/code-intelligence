package agentapi

// Stage 8.1 / e2e §13 contract test for agent.summarize.
// Mirrors recall_faultinjection_test.go for the summarize
// verb. Summarize funnels every successful exit through
// `applySummarizeDegradedContract`. The summarize chokepoint
// translates the internal `summariser_unavailable`
// classifier to the §C22 closed-set wire reason
// `embedding_index_unavailable` BEFORE Enforce so the
// six-value closed contract holds without losing audit
// fidelity (the rich classifier is preserved on the
// `degraded_reason_raw` slog field).

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/degraded"
)

func TestSummarize_faultInjection_closedSetReason_overlay(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{
		output: SummariserOutput{SummaryMD: "## Summary\nSeed orchestrates A, B, C."},
		model:  "fake-llm-v1",
	}
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbSummarize, degraded.ReasonRerankerModelStale)

	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithDegradedMetric(metric),
		WithFaultInjector(fi),
	)

	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err != nil {
		t.Fatalf("Summarize with closed-set injection must succeed, got err: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("resp.Degraded=false, want true under injection")
	}
	if resp.DegradedReason != degraded.ReasonRerankerModelStale {
		t.Fatalf("resp.DegradedReason=%q, want %q",
			resp.DegradedReason, degraded.ReasonRerankerModelStale)
	}
	if got := metric.Count(VerbSummarize, degraded.ReasonRerankerModelStale); got != 1 {
		t.Fatalf("metric increment under injection = %d, want 1", got)
	}
}

func TestSummarize_faultInjection_nonClosedReason_returnsInternalError(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{
		output: SummariserOutput{SummaryMD: "## Summary\nSeed orchestrates A, B, C."},
	}
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbSummarize, "oops")

	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithDegradedMetric(metric),
		WithFaultInjector(fi),
	)

	_, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err == nil {
		t.Fatalf("Summarize must fail when injector returns a non-closed reason")
	}
	if !errors.Is(err, degraded.ErrUnknownReason) {
		t.Fatalf("err = %v, want wraps ErrUnknownReason", err)
	}
	if got := metric.Count(VerbSummarize, "oops"); got != 0 {
		t.Fatalf("metric MUST NOT count non-closed reasons, got %d", got)
	}
}

// TestSummarize_summariserUnavailable_isTranslatedToClosedSet
// pins the Stage 8.1 / iter-3 resolution of evaluator finding
// #1: the rich internal classifier value
// `summariser_unavailable` (emitted by
// `classifySummariserFailure` in summarize.go) is NOT in the
// §8.2 closed set. The Stage 8.1 chokepoint
// `applySummarizeDegradedContract` translates it to
// `embedding_index_unavailable` (both are model-serving
// infrastructure outages and share the operator triage path)
// BEFORE it crosses the wire, so the verb returns a clean
// degraded envelope that passes `degraded.Enforce`.
func TestSummarize_summariserUnavailable_isTranslatedToClosedSet(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	// No summariser wired → the natural degraded path runs
	// (deterministic template + summariser_unavailable
	// classifier → embedding_index_unavailable on the wire).
	metric := degraded.NewCounter()
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithDegradedMetric(metric),
	)

	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err != nil {
		t.Fatalf("Summarize without LLM must succeed via deterministic template: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("resp.Degraded=false, want true (no summariser)")
	}
	if resp.DegradedReason != degraded.ReasonEmbeddingIndexUnavailable {
		t.Fatalf("resp.DegradedReason=%q, want %q (translated from %q)",
			resp.DegradedReason, degraded.ReasonEmbeddingIndexUnavailable,
			DegradedReasonSummariserUnavailable)
	}
	if got := metric.Count(VerbSummarize, degraded.ReasonEmbeddingIndexUnavailable); got != 1 {
		t.Fatalf("metric increment on translated degraded path = %d, want 1", got)
	}
	// The metric MUST NOT have any count under the
	// untranslated rich classifier — the wire envelope is
	// what dashboards graph, and the translation is the whole
	// point of the chokepoint.
	if got := metric.Count(VerbSummarize, DegradedReasonSummariserUnavailable); got != 0 {
		t.Fatalf("metric count under rich classifier = %d, want 0 (translation drops it)", got)
	}
}
