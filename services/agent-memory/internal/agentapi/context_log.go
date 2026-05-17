// Package agentapi: context-log appender adapter.
//
// Stage 5.1 step 6 of implementation-plan.md mandates
// "append a `RecallContextLog` row via `recallcontext.Append`
// and return the `RecallResponse` envelope." This file owns
// the narrow read-side interface the recall handler depends
// on; the binary composition root in
// `cmd/agent-api/main.go` plugs in `*recallcontext.Log`.
//
// Why a narrow interface
// ----------------------
// `*recallcontext.Log` carries both `Append` and `Resolve`;
// the recall service ONLY needs `Append`. Defining a
// narrower interface here keeps the dependency arrow
// one-directional (agentapi imports the recallcontext package
// only at the binary level, via the adapter), matches the
// pattern set by `PublishFilter` and `HealthSource`, and
// makes unit tests trivial — a test fake implements one
// method, not two.
package agentapi

import (
	"context"
	"encoding/json"
)

// ContextLogAppender writes one row to `recall_context_log`
// and returns the assigned context id. The handler invokes
// it exactly once per Recall call when wired.
//
// Behaviour contract pinned by `recallcontext.Log.Append`:
//   - Returns the textual UUID of the new row on success.
//   - Validation errors (invalid verb, malformed UUID,
//     empty reranker version) propagate unwrapped — the
//     recall handler classifies any error returned here as
//     a soft failure (logged) and returns the response
//     without a context_id, so a downstream `recallcontext`
//     outage cannot block the recall path.
type ContextLogAppender interface {
	Append(ctx context.Context, in ContextLogInput) (ContextLogRecord, error)
}

// ContextLogInput mirrors the load-bearing subset of
// `recallcontext.AppendInput` the recall handler populates.
// Defined locally so the agentapi package does not import
// `recallcontext` directly (one-direction dependency arrow
// preserved).
type ContextLogInput struct {
	// Verb MUST be "recall" for recall responses; the
	// agentapi handler always supplies this literal.
	Verb string
	// RepoID is the textual UUID of the repo this recall
	// scoped. Empty values are rejected by the underlying
	// writer.
	RepoID string
	// QueryJSON is the originating verb's input payload
	// serialised as JSON; the writer stores it verbatim on
	// the `query_json jsonb` column.
	QueryJSON json.RawMessage
	// NodeIDs / EdgeIDs / ConceptIDs are the rank-ordered
	// id slices the recall returned. The writer preserves
	// order in `uuid[]` storage so a later Resolve walks
	// them back in the same order.
	NodeIDs    []string
	EdgeIDs    []string
	ConceptIDs []string
	// RerankerModelVersion pins the ranker the recall used;
	// stored on the row for reproducibility per
	// architecture.md §5.4.1.
	RerankerModelVersion string
	// ServedUnderDegraded marks the snapshot-fallback path
	// (Stage 5.1 step 9). The Stage 5.2 observe handler
	// reads this flag to auto-stamp the
	// `degraded_recall_context` Observation per
	// architecture.md §6.1.2.
	ServedUnderDegraded bool
}

// ContextLogRecord is the response shape Append returns.
// Only `ContextID` is consumed by Stage 5.1 today; the
// timestamp lets a future operator dashboard render the
// append cadence without a follow-up SELECT.
type ContextLogRecord struct {
	ContextID string
}

// ContextLogAppenderFunc adapts a plain function into a
// ContextLogAppender. Used by tests + the binary
// composition root to bridge `*recallcontext.Log` into the
// agentapi interface without forcing an import.
type ContextLogAppenderFunc func(ctx context.Context, in ContextLogInput) (ContextLogRecord, error)

// Append implements ContextLogAppender.
func (f ContextLogAppenderFunc) Append(ctx context.Context, in ContextLogInput) (ContextLogRecord, error) {
	return f(ctx, in)
}
