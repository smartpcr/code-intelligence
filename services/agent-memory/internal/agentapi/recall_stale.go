// Package agentapi: recall-side reranker staleness gate.
//
// Stage 6.4 step 5 of implementation-plan.md requires the
// recall path to mark responses
// `degraded_reason='reranker_model_stale'` whenever the
// latest `reranker_model` row is older than
// `rerankerStaleAfter` (7 days, pinned alongside the
// summarize verb's identical constant in `summarize.go`).
//
// Priority order (documented inline on
// `applyRerankerStaleness` and verified by the
// `recall_stale_test.go` suite):
//
//  1. Pre-existing `Degraded=true` (set by
//     `populateDegraded` via the HealthSource) WINS over a
//     "merely stale" reranker — a hard outage carries more
//     actionable information than a stale model.
//  2. Healthy + stale → downgrade the response to
//     `Degraded=true, DegradedReason=reranker_model_stale`.
//  3. Freshness source error → silent (no flip to
//     degraded); a degraded-reason source going down must
//     not change the classifier's verdict (rubber-duck #8).
//  4. Freshness source missing / unwired → silent (the
//     fixture tests `Service{}` with no source rely on
//     this).
//  5. Healthy + fresh → no change.
//
// This file ONLY defines the helper method; the recall
// path's `Recall(ctx, req)` invokes it just before
// returning the populated `RecallResponse`. The shared
// `RerankerFreshnessSource` interface and
// `DegradedReasonRerankerModelStale` / `rerankerStaleAfter`
// constants live in `summarize.go` (one definition, two
// consumers).
package agentapi

import (
	"context"
	"time"
)

// applyRerankerStaleness implements the §6.4 step-5
// "reranker_model_stale" degraded gate. See the package
// doc-comment for the priority order; the comments inline
// below mirror that order so a reader at the call site
// does not need to flip back to the package doc.
func (s *Service) applyRerankerStaleness(ctx context.Context, resp *RecallResponse) {
	if resp == nil {
		return
	}
	// 4. Freshness source missing / unwired → silent.
	if s.rerankerFreshness == nil {
		return
	}
	// 1. Pre-existing Degraded WINS over staleness.
	//    Don't overwrite a hard outage reason with the
	//    softer "merely stale" reason.
	if resp.Degraded {
		return
	}
	trainedAt, hasRow, err := s.rerankerFreshness.LatestRerankerTrainedAt(ctx)
	// 3. Freshness lookup error → silent.
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("agentapi.recall.reranker_freshness_lookup_failed",
				"err", err.Error())
		}
		return
	}
	// 2a. No published row yet (cold-start bootstrap) →
	//     silent. We cannot prove "stale" without a
	//     baseline; the dev/cold-start contract is to
	//     leave the envelope alone.
	if !hasRow {
		return
	}
	// 2b. Healthy + stale → flip.
	if time.Since(trainedAt) > rerankerStaleAfter {
		resp.Degraded = true
		resp.DegradedReason = DegradedReasonRerankerModelStale
	}
	// 5. Healthy + fresh → no change (fallthrough).
}
