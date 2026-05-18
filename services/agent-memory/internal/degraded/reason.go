package degraded

import "errors"

// Closed-set degraded_reason ENUM literals.
//
// architecture.md §8.2 pins exactly six values for the
// degraded_reason field that every Agent and Management
// verb may surface; tech-spec.md §C22 reasserts the set;
// the `degraded_reason` PostgreSQL ENUM in
// `migrations/0001_enums.sql` mirrors it. The constants
// below are the SINGLE source of truth for those wire
// strings.
//
// Stage 8.1 wires every verb through [Enforce] so a
// programming error that smuggles a non-closed-set value
// (e.g. `summariser_unavailable`, `qdrant_partition_split`)
// onto the wire fails fast as a server error rather than
// silently corrupting the operator's dashboard.
//
// Why `summariser_unavailable` is NOT in the set:
// summarize.go's `classifySummariserFailure` emits the
// internal classifier value `summariser_unavailable` so
// the verb's logs / mgmt audit can distinguish a LLM
// outage from a graph outage.  That value MUST be
// translated to a closed-set reason before it crosses the
// wire — the Stage 8.1 contract chokepoint
// [agentapi.applySummarizeDegradedContract] maps it to
// `embedding_index_unavailable` (the closest existing
// "model-serving infrastructure unavailable" reason).
// See iter-3 evaluator finding #1.
const (
	// ReasonEpisodicLogUnavailable — Episode partition is
	// unreachable; Observe falls back to the WAL.
	// (`architecture.md` §7.5, `agentapi/observe.go`).
	ReasonEpisodicLogUnavailable = "episodic_log_unavailable"

	// ReasonGraphStoreUnavailable — Hybrid Graph Store
	// (pgxpool) is unreachable; Recall/Expand/Summarize fall
	// back to the snapshot. (`architecture.md` §7.6).
	ReasonGraphStoreUnavailable = "graph_store_unavailable"

	// ReasonEmbeddingIndexUnavailable — Qdrant is
	// unreachable; Recall falls back to the structural-prior
	// snapshot. (`architecture.md` §7.6).
	//
	// ALSO surfaces summarize.go's `summariser_unavailable`
	// after closed-set translation (the LLM summariser is a
	// model-serving infra outage and shares the operator
	// triage path with embedding-index outages).
	ReasonEmbeddingIndexUnavailable = "embedding_index_unavailable"

	// ReasonRerankerModelStale — the published reranker
	// snapshot is older than the freshness window.
	// (`architecture.md` §6.4, §C22, Stage 5.4).
	ReasonRerankerModelStale = "reranker_model_stale"

	// ReasonSpanIngestorBackpressure — the Span Ingestor
	// queue has been sustained above the high-water mark;
	// per-repo `repo_health.degraded=true`.
	// (`architecture.md` §8.3, `spaningestor/backpressure.go`).
	ReasonSpanIngestorBackpressure = "span_ingestor_backpressure"

	// ReasonConsolidatorBackpressure — the Consolidator
	// queue depth exceeds the high-water threshold.
	// Stage 8.1 wires this so Observe queues the Episode
	// (the EpisodicLog append still runs) and surfaces the
	// flag instead of failing the agent caller
	// (`architecture.md` §8.3, C24).
	ReasonConsolidatorBackpressure = "consolidator_backpressure"
)

// closedSet is the §8.2 ENUM membership predicate.  Built
// from the constants above so adding a new reason in one
// place propagates automatically.
var closedSet = map[string]struct{}{
	ReasonEpisodicLogUnavailable:    {},
	ReasonGraphStoreUnavailable:     {},
	ReasonEmbeddingIndexUnavailable: {},
	ReasonRerankerModelStale:        {},
	ReasonSpanIngestorBackpressure:  {},
	ReasonConsolidatorBackpressure:  {},
}

// AllReasons returns the closed set as a slice in stable
// order (definition order). Used by test fixtures and by
// the metric pre-registration helper so a label
// combination that has never been incremented still
// appears on the dashboard at 0.
func AllReasons() []string {
	return []string{
		ReasonEpisodicLogUnavailable,
		ReasonGraphStoreUnavailable,
		ReasonEmbeddingIndexUnavailable,
		ReasonRerankerModelStale,
		ReasonSpanIngestorBackpressure,
		ReasonConsolidatorBackpressure,
	}
}

// IsClosed reports whether `reason` is one of the six
// architecture.md §8.2 closed-set values. The empty string
// is NOT in the closed set (Enforce treats it as a separate
// case: empty + degraded=false is the healthy shape).
func IsClosed(reason string) bool {
	_, ok := closedSet[reason]
	return ok
}

// ErrUnknownReason is returned by [Enforce] when a verb tried
// to surface a `degraded_reason` not in the closed set, or
// supplied a reason while declaring `degraded=false`. The
// gRPC / HTTP adapter layer translates this into a hard
// server error (gRPC `codes.Internal`, HTTP 500) so the
// closed-set contract is enforced server-side per
// e2e-scenarios.md §13.
var ErrUnknownReason = errors.New(
	"degraded: degraded_reason is not in the architecture.md §8.2 closed set")

// Enforce validates a `(degraded, reason)` response pair
// against the §8.2 contract.  Returns nil iff:
//
//   - degraded=false AND reason=="" (the healthy shape), OR
//   - degraded=true  AND IsClosed(reason).
//
// Any other combination (degraded with empty reason; not-
// degraded with a non-empty reason; degraded with a non-
// closed reason) returns [ErrUnknownReason] so the caller
// can hard-fail.  The wrapping error message includes the
// offending reason so the operator log captures what was
// attempted.
func Enforce(degraded bool, reason string) error {
	if !degraded {
		if reason != "" {
			return wrapBadReason(reason, "non-degraded response with reason set")
		}
		return nil
	}
	if reason == "" {
		return wrapBadReason("", "degraded response with empty reason")
	}
	if !IsClosed(reason) {
		return wrapBadReason(reason, "reason not in §8.2 closed set")
	}
	return nil
}

// reasonError is the typed error wrapper [Enforce] returns so
// callers can extract the offending reason without parsing
// the message string.  It satisfies `errors.Is(err,
// ErrUnknownReason)` so existing `errors.Is` switches keep
// working.
type reasonError struct {
	Reason string
	Note   string
}

func (e *reasonError) Error() string {
	return "degraded: " + e.Note + ": reason=" + quoteReason(e.Reason)
}

func (e *reasonError) Is(target error) bool {
	return target == ErrUnknownReason
}

// Reason extracts the offending reason from an error
// returned by [Enforce]. Returns "", false when the error
// did not come from [Enforce].
func Reason(err error) (string, bool) {
	var re *reasonError
	if errors.As(err, &re) {
		return re.Reason, true
	}
	return "", false
}

func wrapBadReason(reason, note string) error {
	return &reasonError{Reason: reason, Note: note}
}

func quoteReason(r string) string {
	if r == "" {
		return `""`
	}
	return `"` + r + `"`
}

// Priority is the §8.1 overlay-ordering table.  When two or
// more degraded signals stack (e.g. consolidator backpressure
// AND fault injection asks for `reranker_model_stale`), the
// response carries the signal with the HIGHER priority.
// Hard outages dominate over staleness; staleness dominates
// over backpressure; backpressure dominates over an injected
// test-only reason.
//
// The priority numbers are intentionally sparse so a future
// reason can slot in without renumbering.
//
// Unknown reasons return 0 so the caller's higher-priority
// known reason always wins; a "" reason returns 0 as well so
// the caller can treat "" as the empty / lowest-priority
// signal.
func Priority(reason string) int {
	switch reason {
	case ReasonEpisodicLogUnavailable:
		return 100
	case ReasonGraphStoreUnavailable:
		return 90
	case ReasonEmbeddingIndexUnavailable:
		return 85
	case ReasonRerankerModelStale:
		return 50
	case ReasonSpanIngestorBackpressure:
		return 30
	case ReasonConsolidatorBackpressure:
		return 20
	default:
		return 0
	}
}
