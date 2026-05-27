// Package agentapi: agent.observe verb implementation.
//
// Stage 5.2 of implementation-plan.md owns this file. The
// verb is the agent's outcome-record path:
//
//  1. The agent calls `agent.recall` and consumes the
//     resulting context.
//  2. The agent reasons locally and produces an action.
//  3. The agent calls `agent.observe(repo_id, session_id,
//     trace_id, action, outcome, signal?, context_id?,
//     observation_refs?)` to durably record the action plus
//     the recall elements that contributed to it.
//
// Architectural invariants (architecture.md §6.1.2 + §5.3.1
// + §5.3.3):
//
//   - C15: `outcome=human_corrected` is reserved for the
//     operator path (`mgmt.feedback`). A caller-supplied
//     human_corrected outcome on agent.observe is rejected
//     with INVALID_ARGUMENT BEFORE any DB write.
//   - C23: `observation_refs[*].role='degraded_recall_context'`
//     is reserved for the server-side auto-stamp path. A
//     caller-supplied degraded_recall_context role is rejected
//     with INVALID_ARGUMENT BEFORE any DB write — the server
//     is the ONLY writer of that role.
//   - When `context_id` references a RecallContextLog row with
//     `served_under_degraded=true`, the server auto-appends
//     one extra Observation with role=degraded_recall_context
//     and degraded_recall_context_id=context_id. The auto-stamp
//     is NEVER skipped: if the resolver cannot decide degraded
//     state we fail the call rather than silently drop the
//     auto-stamp (which would corrupt the operator
//     `mgmt.read.episodes` flow that tells "fell back to stale
//     graph" from "used live graph").
//   - The Episode row carries `kind='agent'` (per §5.3.1: agent
//     observations always use this kind — `feedback` and
//     `synthetic_positive` belong to the operator / consolidator
//     paths).
//   - Episode + N Observations are written in a single
//     transaction (§5.3.1 / §5.3.3 invariant: every Observation
//     belongs to a written Episode).
//
// WAL fallback (architecture.md §7.5):
//
// When the Episode partition is offline (connection-class
// error: net dial, conn refused, SQLSTATE 08xxx admin shutdown,
// etc.) the writer enqueues the prepared Episode + Observations
// onto a local file-based WAL and returns
// `degraded=true, degraded_reason='episodic_log_unavailable'`
// with the pre-minted episode_id surfaced on the response. A
// background flusher drains the WAL in ARRIVAL ORDER once the
// partition recovers and inserts the rows verbatim. The
// pre-minted episode_id appears on the final `Episode` row so a
// later `mgmt.read.episodes` query finds it under the same id
// the agent received at observe time.
//
// Pre-minting requirement (the load-bearing decision)
// ---------------------------------------------------
// The Episode table defaults `episode_id := gen_random_uuid()`
// at INSERT time, but we MINT THE UUID GO-SIDE before issuing
// the write. Two reasons:
//
//  1. The WAL fallback path needs to return the id BEFORE the
//     DB write succeeds (or, in the partition-offline case,
//     before the row reaches the DB at all). Letting the DB
//     mint the id would push the response back to "after
//     INSERT succeeded", contradicting the §7.5 contract.
//  2. Replay determinism. The Episode partition is keyed on
//     `(episode_id, created_at)`. We embed BOTH values in the
//     WAL payload so a replay lands in the EXACT same
//     partition (and under the EXACT same logical identity) as
//     the originally-attempted INSERT. Letting the DB choose
//     `created_at = now()` at flush time would route the
//     replayed row into the wrong partition (e.g. a row
//     attempted at 23:59 on Aug 31 would land in Sep's
//     partition after a recovery on Sep 1).
//
// observation_id values are minted the same way for the same
// determinism guarantee.
package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/degraded"
	"go.opentelemetry.io/otel/trace"
)

// VerbObserve is the canonical verb name passed to
// [degraded.Metric] and [degraded.FaultInjector] for this
// handler. Mirrors the proto method name so the operator
// dashboard's `agent_observe_degraded_total{reason=}` label
// matches the dashboard's RPC labels.
const VerbObserve = "agent.observe"

// allowedOutcomes is the SUBSET of the `outcome` ENUM that
// `agent.observe` accepts (architecture.md §6.1.2). The
// excluded member is `human_corrected` per C15 — reserved for
// `mgmt.feedback`. The set is keyed by the ENUM literal so
// validation rejects with a clear "unknown outcome" message
// before the row reaches the DB CHECK.
var allowedOutcomes = map[string]struct{}{
	"success":  {},
	"failure":  {},
	"refused":  {},
	"degraded": {},
}

// observationRoleNodeHit / EdgeHit / CallEdgeHit / ConceptHit
// are the closed-set role values the caller MAY supply on
// observation_refs[]. `degraded_recall_context` is server-only
// and intentionally NOT in this set.
const (
	observationRoleNodeHit                = "node_hit"
	observationRoleEdgeHit                = "edge_hit"
	observationRoleCallEdgeHit            = "call_edge_hit"
	observationRoleConceptHit             = "concept_hit"
	observationRoleDegradedRecallContext  = "degraded_recall_context"
	episodeKindAgent                      = "agent"
	degradedReasonEpisodicLogUnavailable  = "episodic_log_unavailable"
	degradedReasonConsolidatorBackpressure = "consolidator_backpressure"
)

// callerObservationRoles is the closed set of roles a caller
// MAY supply on observation_refs[*]. `degraded_recall_context`
// is NOT in this set — that role is reserved for the
// server-side auto-stamp path (C23).
var callerObservationRoles = map[string]struct{}{
	observationRoleNodeHit:     {},
	observationRoleEdgeHit:     {},
	observationRoleCallEdgeHit: {},
	observationRoleConceptHit:  {},
}

// -- Public types ----------------------------------------------------

// ObserveRequest is the in-process shape of `agent.observe`.
// Mirrors `proto.ObserveRequest` minus the wire-only fields.
type ObserveRequest struct {
	// RepoID is the textual UUID of the repo this observation
	// is paired with. REQUIRED.
	RepoID string
	// SessionID is the agent's per-conversation correlation id
	// (free-form text). REQUIRED.
	SessionID string
	// TraceID is the per-action trace correlation id (free-form
	// text). REQUIRED.
	TraceID string
	// ActionJSON is the structured action the agent took.
	// REQUIRED and MUST be valid JSON (the `Episode.action
	// jsonb` column rejects non-JSON outright).
	ActionJSON json.RawMessage
	// Outcome is one of the §6.1.2 caller-allowed outcomes:
	// {success, failure, refused, degraded}. `human_corrected`
	// is rejected per C15.
	Outcome string
	// SignalJSON is the optional reward / training signal
	// (architecture.md §5.3.1). Empty omits the column.
	SignalJSON json.RawMessage
	// ContextID is the textual UUID of the `RecallContextLog`
	// row this observation grounds against. REQUIRED for
	// `outcome != feedback` per the §5.3.1 schema CHECK.
	ContextID string
	// ObservationRefs is the caller-supplied list of
	// {role, node_id/edge_id/concept_id, weight} entries. The
	// server appends one Observation row per ref plus, when
	// the resolved RecallContextLog row was served degraded,
	// one extra synthetic ref with role=degraded_recall_context
	// (architecture.md §6.1.2).
	ObservationRefs []ObservationRef
	// EpisodeGroupID is the optional caller-supplied group id
	// (textual UUID). When empty the server mints one.
	EpisodeGroupID string
}

// ObservationRef mirrors `proto.ObservationRef`. Exactly one
// of `NodeID`, `EdgeID`, `ConceptID` MUST be set for caller-
// supplied refs; the role pairing is enforced by validation
// before any DB write.
type ObservationRef struct {
	Role      string
	NodeID    string
	EdgeID    string
	ConceptID string
	Weight    float64
}

// ObserveResponse is the in-process shape of `agent.observe`.
// `EpisodeID` is the pre-minted UUID the server assigned at
// request time (NOT the DB-default value); under WAL fallback
// the same id surfaces on the eventually-flushed row.
type ObserveResponse struct {
	EpisodeID      string
	EpisodeGroupID string
	Degraded       bool
	DegradedReason string
}

// -- Sentinel errors -------------------------------------------------

// ErrHumanCorrectedNotAllowed is returned when a caller supplies
// `outcome=human_corrected` on agent.observe (C15 / §6.2.2).
// The gRPC adapter maps it to codes.InvalidArgument.
var ErrHumanCorrectedNotAllowed = errors.New(
	"agentapi: observe: outcome=human_corrected is reserved for mgmt.feedback (C15)")

// ErrDegradedRecallContextRoleForbidden is returned when a
// caller supplies an observation ref with
// `role=degraded_recall_context` (C23). The server is the only
// allowed writer of that role.
var ErrDegradedRecallContextRoleForbidden = errors.New(
	"agentapi: observe: role=degraded_recall_context is reserved for the server auto-stamp (C23)")

// ErrInvalidObservationRole is returned when a caller-supplied
// `role` is not one of the closed-set caller-allowed roles.
var ErrInvalidObservationRole = errors.New(
	"agentapi: observe: invalid observation role")

// ErrInvalidObservationTarget is returned when an observation
// ref's role/target pairing is wrong (e.g. role=node_hit with
// only concept_id set).
var ErrInvalidObservationTarget = errors.New(
	"agentapi: observe: observation ref role/target pairing invalid")

// ErrInvalidOutcome is returned when `outcome` is not one of
// the caller-allowed outcomes.
var ErrInvalidOutcome = errors.New(
	"agentapi: observe: invalid outcome")

// ErrMissingRepoID / SessionID / TraceID / Action / Context are
// the per-field validation sentinels.
var (
	ErrMissingRepoID    = errors.New("agentapi: observe: repo_id is required")
	ErrMissingSessionID = errors.New("agentapi: observe: session_id is required")
	ErrMissingTraceID   = errors.New("agentapi: observe: trace_id is required")
	ErrMissingAction    = errors.New("agentapi: observe: action_json is required")
	ErrMissingContextID = errors.New("agentapi: observe: context_id is required")
	ErrInvalidJSON      = errors.New("agentapi: observe: payload is not valid JSON")
)

// ErrEpisodicLogUnavailable is the sentinel an EpisodeAppender
// implementation returns when the Episode partition cannot
// accept the INSERT for INFRASTRUCTURE reasons (connection
// refused, admin shutdown, pool exhausted, etc.). When the
// Observe handler sees this sentinel it engages the WAL
// fallback path.
//
// Implementations MUST NOT return this for schema/constraint
// bugs (e.g. SQLSTATE 23xxx check_violation, 42P01 missing
// table) — those failures are loud, not degraded, and the
// caller's input must be fixed.
var ErrEpisodicLogUnavailable = errors.New(
	"agentapi: observe: episodic log unavailable")

// -- Interfaces (narrow, consumer-side) -------------------------------

// EpisodeAppender writes one Episode row plus N Observation
// rows inside a single transaction. The binary composition
// root wires a SQL-backed implementation; tests inject a fake.
//
// Returns `ErrEpisodicLogUnavailable` (or wraps it) when the
// failure is an infrastructure outage that justifies WAL
// fallback. Any other error is treated as a hard failure by
// the Observe handler.
type EpisodeAppender interface {
	Append(ctx context.Context, in EpisodeAppendInput) error
}

// EpisodeAppenderFunc adapts a plain function into the
// EpisodeAppender interface. Used by tests + the binary
// composition root.
type EpisodeAppenderFunc func(ctx context.Context, in EpisodeAppendInput) error

// Append implements EpisodeAppender.
func (f EpisodeAppenderFunc) Append(ctx context.Context, in EpisodeAppendInput) error {
	return f(ctx, in)
}

// EpisodeAppendInput is the prepared row payload the
// EpisodeAppender consumes. All IDs are pre-minted by the
// Observe handler before this struct reaches the writer so
// the WAL fallback path can return the episode_id BEFORE the
// DB sees the row (and so a WAL replay lands in the same
// partition as the original attempt — `CreatedAt` is the
// partition key).
type EpisodeAppendInput struct {
	EpisodeID      string
	EpisodeGroupID string
	RepoID         string
	SessionID      string
	TraceID        string
	Kind           string // always "agent" for the observe path
	ContextID      string
	ActionJSON     json.RawMessage
	SignalJSON     json.RawMessage
	Outcome        string
	CreatedAt      time.Time
	// Degraded + DegradedReason persist the §7.5 fallback
	// signal onto the `episode.degraded` /
	// `episode.degraded_reason` columns. The happy-path
	// writer (Append called synchronously) leaves both
	// zero-valued; only the WAL fallback branch sets
	// Degraded=true + DegradedReason=
	// degradedReasonEpisodicLogUnavailable BEFORE enqueueing,
	// so the eventually-replayed Episode row carries the
	// flag (architecture.md §7.5 / table-cell §6.4: "the
	// Episode is still appended; if the EpisodicLog itself
	// is degraded the writer buffers and replies
	// degraded=true with the eventually-assigned
	// episode_id"). The SQL writer translates an empty
	// DegradedReason as NULL so the episode_degraded_reason_chk
	// CHECK is satisfied in both states.
	Degraded       bool
	DegradedReason string
	Observations   []ObservationAppendInput
}

// ObservationAppendInput is one Observation row payload.
// Exactly one of `NodeID`, `EdgeID`, `ConceptID`,
// `DegradedRecallContextID` is non-empty — the Observe handler
// validates this BEFORE building the slice so the writer can
// trust the invariant.
type ObservationAppendInput struct {
	ObservationID           string
	Role                    string
	NodeID                  string
	EdgeID                  string
	ConceptID               string
	DegradedRecallContextID string
	Weight                  float64
	CreatedAt               time.Time
}

// ContextResolver looks up the §5.4.1 `served_under_degraded`
// flag on a RecallContextLog row. The Observe handler uses it
// to decide whether to auto-stamp the degraded_recall_context
// Observation (architecture.md §6.1.2). Returns
// `(false, nil)` for a healthy context_id; returns
// `(false, ErrContextNotFound)` when the id does not exist
// FOR THE GIVEN REPO — the handler maps that to
// `INVALID_ARGUMENT`.
//
// Resolution MUST be scoped by `(repo_id, context_id)`, not
// by `context_id` alone. A bare-id lookup would let observe
// request from repo A attach its Episode to repo B's
// RecallContextLog and inherit the wrong degraded flag (and,
// downstream, leak repo B's recall lineage into repo A's
// `mgmt.read.episodes` view). The closed-set
// `recall_context_log_repo_created_idx` makes the composite
// lookup as cheap as the prior id-only lookup.
type ContextResolver interface {
	ResolveServedUnderDegraded(ctx context.Context, repoID, contextID string) (bool, error)
}

// ContextResolverFunc adapts a plain function into the
// ContextResolver interface. Used by tests + the binary
// composition root.
type ContextResolverFunc func(ctx context.Context, repoID, contextID string) (bool, error)

// ResolveServedUnderDegraded implements ContextResolver.
func (f ContextResolverFunc) ResolveServedUnderDegraded(ctx context.Context, repoID, contextID string) (bool, error) {
	return f(ctx, repoID, contextID)
}

// ErrContextNotFound is returned by ContextResolver when the
// supplied context_id does not name a `recall_context_log`
// row. The handler maps it to INVALID_ARGUMENT — the caller
// supplied a bogus id.
var ErrContextNotFound = errors.New(
	"agentapi: observe: context_id not found")

// WALSink is the durable enqueue surface. The Observe handler
// calls Enqueue when the EpisodeAppender returned
// `ErrEpisodicLogUnavailable`; the handler returns success
// (with degraded=true) only after Enqueue succeeds, so a WAL
// outage surfaces as a hard error rather than silently losing
// the Episode.
type WALSink interface {
	Enqueue(ctx context.Context, in EpisodeAppendInput) error
}

// ObserveMetrics surfaces the §C22 observability gauges the
// Observe handler updates. Today only the WAL depth gauge is
// pinned by the implementation plan. A nil ObserveMetrics is
// a no-op; the production binary wires the package-level
// `*Metrics` struct.
type ObserveMetrics interface {
	// RecordWALDepth is called whenever the WAL depth changes
	// (enqueue: +1, drain: -1, startup recovery: set to actual
	// file depth). The implementation MUST be safe to call
	// from multiple goroutines.
	RecordWALDepth(depth int64)
}

// ConsolidatorBackpressureSource is the cross-process /
// cross-component signal the Stage 8.1 wiring consults to
// decide whether to stamp
// `degraded_reason=consolidator_backpressure` on the Episode
// row + response.
//
// architecture.md §8.3 + C24 invariant: "An `agent.observe`
// call NEVER fails because the Consolidator is backpressured;
// the Episode is queued and `degraded_reason` set."  The
// Observe handler's "queue" is the existing synchronous
// EpisodicLog append (the Episode row IS the queue entry the
// Consolidator processes on its next tick).  Backpressure is
// therefore a RESPONSE DECORATION + a persisted Episode flag,
// not a new queue.
//
// Backpressured is called once per Observe invocation, BEFORE
// the EpisodeAppender.Append.  An error MUST NOT fail the
// observe call — the handler logs at warn level and treats
// the source as "healthy" so a flaky probe never blocks the
// agent caller.
type ConsolidatorBackpressureSource interface {
	Backpressured(ctx context.Context, repoID string) (bool, error)
}

// ConsolidatorBackpressureSourceFunc adapts a plain function
// into a ConsolidatorBackpressureSource. Used by tests and by
// the binary composition root.
type ConsolidatorBackpressureSourceFunc func(ctx context.Context, repoID string) (bool, error)

// Backpressured implements ConsolidatorBackpressureSource.
func (f ConsolidatorBackpressureSourceFunc) Backpressured(ctx context.Context, repoID string) (bool, error) {
	return f(ctx, repoID)
}

// -- Observe service -------------------------------------------------

// ObserveService is the in-process Stage 5.2 implementation.
// Construct via NewObserveService; the only exported method is
// Observe. Dependencies are wired via ObserveOption — both
// EpisodeAppender and ContextResolver are REQUIRED at
// construction (a nil dep panics), WALSink and ObserveMetrics
// are optional (no-WAL means the handler returns the writer
// error verbatim).
type ObserveService struct {
	writer    EpisodeAppender
	resolver  ContextResolver
	wal       WALSink
	metrics   ObserveMetrics
	logger    *slog.Logger
	now       func() time.Time
	uuidFunc  func() (string, error)

	// Stage 8.1 — Degraded-mode contract wiring
	// (architecture.md §6.3, §8.2; C22, C24).
	//
	// degradedMetric is the per-verb, per-reason counter the
	// operator dashboard graphs. Nil means "no metric wired"
	// (the production composition root may leave it nil
	// while the unit tests pin a *degraded.Counter).
	degradedMetric degraded.Metric
	// faultInjector is the optional test-only seam that
	// overlays a degraded reason on the response. The
	// production composition root MUST NOT wire one.
	faultInjector degraded.FaultInjector
	// consolidatorBackpressure is the cross-component
	// backpressure source the §8.3 / C24 invariant consults
	// before stamping the Episode row. Nil means the source
	// is not wired (legacy / partial-deploy behaviour);
	// production wires either a PG-row health source or the
	// in-process consolidator gauge.
	consolidatorBackpressure ConsolidatorBackpressureSource

	// Stage 8.3 — `agent_observe_duration_seconds` observer
	// wired by `WithObserveLatencyObserver`. A nil observer
	// is a no-op; production wiring at `cmd/agent-api/main.go`
	// binds it to an `*obs.Histogram` so the binary's
	// `/metrics` body exposes the histogram.
	latency LatencyObserver

	// Stage 8.3 step 2 (iter-2 evaluator fix #1) — per-verb
	// OpenTelemetry tracer wired by `WithObserveTracer`. The
	// Observe verb opens an `agent.observe` span and threads
	// the returned context into every downstream call (DB
	// writer, WAL, context resolver). A nil tracer is a
	// no-op (the helpers in `tracing.go` substitute a
	// package noop tracer at call time).
	tracer trace.Tracer
}

// ObserveOption configures an ObserveService.
type ObserveOption func(*ObserveService)

// WithObserveLogger plumbs a structured logger. Defaults to
// slog.Default().
func WithObserveLogger(l *slog.Logger) ObserveOption {
	return func(s *ObserveService) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithObserveWAL plumbs the WAL sink for the §7.5 fallback
// path. Without it the handler propagates ErrEpisodicLogUnavailable
// to the caller (legacy behaviour); production wiring always
// supplies one.
func WithObserveWAL(w WALSink) ObserveOption {
	return func(s *ObserveService) {
		s.wal = w
	}
}

// WithObserveMetrics plumbs the observability surface. A nil
// metrics is treated as no-op.
func WithObserveMetrics(m ObserveMetrics) ObserveOption {
	return func(s *ObserveService) {
		s.metrics = m
	}
}

// WithObserveClock overrides the wall-clock used to stamp
// CreatedAt on Episode + Observation rows. Tests use this to
// pin deterministic timestamps; production never sets it.
func WithObserveClock(now func() time.Time) ObserveOption {
	return func(s *ObserveService) {
		if now != nil {
			s.now = now
		}
	}
}

// WithObserveUUID overrides the UUID generator. Tests use this
// to pin deterministic episode_id / observation_id values;
// production never sets it.
func WithObserveUUID(fn func() (string, error)) ObserveOption {
	return func(s *ObserveService) {
		if fn != nil {
			s.uuidFunc = fn
		}
	}
}

// WithObserveDegradedMetric plumbs the per-verb degraded
// counter (implementation-plan §8.1 step 4). A nil metric is
// a no-op; tests pin a `*degraded.Counter` so they can assert
// per-reason counts.
func WithObserveDegradedMetric(m degraded.Metric) ObserveOption {
	return func(s *ObserveService) {
		if m != nil {
			s.degradedMetric = m
		}
	}
}

// WithObserveFaultInjector plumbs the §8.1 test-only fault
// injector. Production wiring leaves it nil; the e2e
// "closed degraded_reason enforced" scenario wires a
// `*degraded.MapFaultInjector` and asserts the wire response
// either gets rewritten to a closed value OR fails with
// Internal.
func WithObserveFaultInjector(fi degraded.FaultInjector) ObserveOption {
	return func(s *ObserveService) {
		s.faultInjector = fi
	}
}

// WithObserveConsolidatorBackpressure plumbs the §8.3 / C24
// signal so the handler stamps
// `degraded_reason=consolidator_backpressure` on the Episode
// row when the Consolidator queue depth exceeds threshold.
// A nil source disables the check (no-op overlay); production
// wires the consolidator health gauge.
func WithObserveConsolidatorBackpressure(b ConsolidatorBackpressureSource) ObserveOption {
	return func(s *ObserveService) {
		s.consolidatorBackpressure = b
	}
}

// WithObserveLatency plumbs the §8.3
// `agent_observe_duration_seconds` histogram observer. Mirrors
// `WithObserveLatencyObserver` (which is the `Service`-level
// helper exported from `latency.go`); both names exist because
// the recall/expand/summarize verbs hang off `Service` while
// `Observe` hangs off `ObserveService`, and a single option
// type cannot configure both. A nil observer is a no-op.
func WithObserveLatency(o LatencyObserver) ObserveOption {
	return func(s *ObserveService) {
		s.latency = o
	}
}

// NewObserveService constructs an Observe handler. The two
// required dependencies panic on nil — a wiring bug surfacing
// at process start is cheaper than the same bug surfacing on
// the first request.
func NewObserveService(writer EpisodeAppender, resolver ContextResolver, opts ...ObserveOption) *ObserveService {
	if writer == nil {
		panic("agentapi: NewObserveService: nil EpisodeAppender")
	}
	if resolver == nil {
		panic("agentapi: NewObserveService: nil ContextResolver")
	}
	s := &ObserveService{
		writer:   writer,
		resolver: resolver,
		logger:   slog.Default(),
		now:      func() time.Time { return time.Now().UTC() },
		uuidFunc: newUUIDv4,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Observe implements the §6.1.2 contract. The full flow:
//
//  1. Validate the request (fields + outcome + per-ref role/
//     target pairing). Reject with a typed sentinel BEFORE any
//     DB / WAL work — a malformed request must not leave any
//     trace.
//  2. Resolve the RecallContextLog row's served_under_degraded
//     flag. Failure here is a HARD error: missing the
//     auto-stamp would silently violate the architectural
//     "the server is the only writer of degraded_recall_context"
//     promise.
//  3. Pre-mint episode_id, episode_group_id, observation_ids,
//     and the wall-clock CreatedAt timestamp. These travel
//     together to the writer (and to the WAL on fallback) so
//     a replay lands deterministically.
//  4. Issue the single-transaction INSERT. Success returns
//     `{episode_id, degraded:false}`.
//  5. On `ErrEpisodicLogUnavailable` AND a WAL is wired,
//     enqueue the payload and return `{episode_id,
//     degraded:true, degraded_reason:'episodic_log_unavailable'}`.
//     A WAL enqueue failure surfaces as a hard error — we
//     refuse to return success when the row is not durable.
//  6. Any other writer error propagates verbatim — schema /
//     constraint failures must surface loudly.
func (s *ObserveService) Observe(ctx context.Context, req ObserveRequest) (resp ObserveResponse, err error) {
	// Stage 8.3 — record `agent_observe_duration_seconds`
	// for every call (happy path AND degraded). The defer
	// fires AFTER the function returns regardless of which
	// branch handled the request.
	observeStart := time.Now()
	defer func() { recordLatency(s.latency, observeStart) }()

	// Stage 8.3 step 2 — open the `agent.observe` operational
	// span and thread the returned context into every
	// downstream call. The deferred `endVerbSpan` runs AFTER
	// the function returns so the span records the final
	// caller-visible status (error or degraded reason).
	var span trace.Span
	ctx, span = startVerbSpan(ctx, s.tracer, VerbObserve, req.RepoID)
	defer func() { endVerbSpan(span, err, resp.DegradedReason) }()

	if err := validateObserveRequest(req); err != nil {
		return ObserveResponse{}, err
	}

	// Step 2 — context resolver.  See sentinel ErrContextNotFound:
	// the resolver returns it when the row does not exist FOR
	// THE GIVEN REPO; we map that to INVALID_ARGUMENT at the
	// gRPC boundary. The lookup is composite `(repo_id,
	// context_id)` so a caller cannot attach a cross-repo
	// context to their Episode (and inherit the wrong
	// degraded flag).
	servedDegraded, err := s.resolver.ResolveServedUnderDegraded(ctx, req.RepoID, req.ContextID)
	if err != nil {
		// Hard fail.  Missing the auto-stamp would silently
		// violate the architecture's "the server is the ONLY
		// writer of degraded_recall_context" promise; treating
		// resolver outages as a soft-degrade would let a
		// degraded RecallContextLog row look as if it had been
		// observed under healthy conditions.
		s.logger.Error("agentapi.observe.context_resolver_failed",
			slog.String("context_id", req.ContextID),
			slog.String("error", err.Error()))
		return ObserveResponse{}, fmt.Errorf("agentapi: observe: resolve context: %w", err)
	}

	// Step 3 — pre-mint identities + timestamps.  Used by both
	// the writer path and the WAL fallback path so a replay
	// lands in the exact same partition.
	createdAt := s.now()
	episodeID, err := s.uuidFunc()
	if err != nil {
		return ObserveResponse{}, fmt.Errorf("agentapi: observe: mint episode_id: %w", err)
	}
	episodeGroupID := req.EpisodeGroupID
	if episodeGroupID == "" {
		egid, err := s.uuidFunc()
		if err != nil {
			return ObserveResponse{}, fmt.Errorf("agentapi: observe: mint episode_group_id: %w", err)
		}
		episodeGroupID = egid
	}

	// Build the Observations slice.  Caller refs come first
	// (in caller order so reranker training preserves the
	// agent's intent), then the optional auto-stamp.
	observations := make([]ObservationAppendInput, 0, len(req.ObservationRefs)+1)
	for _, ref := range req.ObservationRefs {
		obsID, err := s.uuidFunc()
		if err != nil {
			return ObserveResponse{}, fmt.Errorf("agentapi: observe: mint observation_id: %w", err)
		}
		observations = append(observations, ObservationAppendInput{
			ObservationID: obsID,
			Role:          ref.Role,
			NodeID:        ref.NodeID,
			EdgeID:        ref.EdgeID,
			ConceptID:     ref.ConceptID,
			Weight:        ref.Weight,
			CreatedAt:     createdAt,
		})
	}
	if servedDegraded {
		obsID, err := s.uuidFunc()
		if err != nil {
			return ObserveResponse{}, fmt.Errorf("agentapi: observe: mint observation_id: %w", err)
		}
		observations = append(observations, ObservationAppendInput{
			ObservationID:           obsID,
			Role:                    observationRoleDegradedRecallContext,
			DegradedRecallContextID: req.ContextID,
			Weight:                  0,
			CreatedAt:               createdAt,
		})
		s.logger.Info("agentapi.observe.auto_stamped_degraded_recall_context",
			slog.String("context_id", req.ContextID),
			slog.String("episode_id", episodeID))
	}

	// Stage 8.1 / C24 — consult the Consolidator backpressure
	// source BEFORE Append so the durable Episode row carries
	// the flag (mgmt.read.episodes later surfaces the same
	// degraded_reason the agent saw on the response).  A
	// HealthSource probe error MUST NOT fail the observe call:
	// the §8.3 invariant promises agent.observe never fails on
	// downstream queue pressure, and a flaky probe is treated
	// as "healthy" (we log + carry on).
	consolidatorDegraded := false
	if s.consolidatorBackpressure != nil {
		bp, bpErr := s.consolidatorBackpressure.Backpressured(ctx, req.RepoID)
		if bpErr != nil {
			s.logger.Warn("agentapi.observe.consolidator_backpressure_probe_failed",
				slog.String("repo_id", req.RepoID),
				slog.String("err", bpErr.Error()))
		} else {
			consolidatorDegraded = bp
		}
	}

	in := EpisodeAppendInput{
		EpisodeID:      episodeID,
		EpisodeGroupID: episodeGroupID,
		RepoID:         req.RepoID,
		SessionID:      req.SessionID,
		TraceID:        req.TraceID,
		Kind:           episodeKindAgent,
		ContextID:      req.ContextID,
		ActionJSON:     req.ActionJSON,
		SignalJSON:     req.SignalJSON,
		Outcome:        req.Outcome,
		CreatedAt:      createdAt,
		Observations:   observations,
	}

	// When the Consolidator is backpressured we MUST persist
	// the degraded flag onto the Episode row itself so
	// `mgmt.read.episodes` can later report
	// `degraded_reason='consolidator_backpressure'` for this
	// row (architecture.md §6.3 row "agent.observe": the
	// Episode is still appended AND carries the reason).  The
	// row-level flag is set BEFORE Append so a successful
	// Append also commits the reason.
	if consolidatorDegraded {
		in.Degraded = true
		in.DegradedReason = degradedReasonConsolidatorBackpressure
	}

	// Step 4 — single-tx INSERT.
	writeErr := s.writer.Append(ctx, in)
	if writeErr == nil {
		s.logger.Debug("agentapi.observe.appended",
			slog.String("episode_id", episodeID),
			slog.Int("observations", len(observations)),
			slog.Bool("served_under_degraded", servedDegraded),
			slog.Bool("consolidator_backpressure", consolidatorDegraded))
		resp := ObserveResponse{
			EpisodeID:      episodeID,
			EpisodeGroupID: episodeGroupID,
		}
		if consolidatorDegraded {
			// §8.3 / C24: the Episode is queued (durably
			// appended) and the response carries the flag.
			// Use the constant from the package so the wire
			// string matches the §8.2 closed set verbatim.
			resp.Degraded = true
			resp.DegradedReason = degradedReasonConsolidatorBackpressure
		}
		return s.applyDegradedContract(req.RepoID, resp)
	}

	// Step 5 — partition outage triggers WAL fallback (when
	// wired).  Any other error propagates verbatim so schema
	// bugs surface loudly.
	if !errors.Is(writeErr, ErrEpisodicLogUnavailable) {
		return ObserveResponse{}, fmt.Errorf("agentapi: observe: append: %w", writeErr)
	}
	if s.wal == nil {
		// No WAL wired — preserve the partition error so the
		// operator sees the underlying outage.  This matches
		// the recall-handler "no fallback wired" pattern.
		return ObserveResponse{}, fmt.Errorf("agentapi: observe: append (no WAL configured): %w", writeErr)
	}

	if walErr := s.wal.Enqueue(ctx, walPayloadForDegraded(in)); walErr != nil {
		// WAL enqueue failed — we refuse to lie about
		// durability.  Return the WAL error to the caller so
		// they can retry (or fail their action).
		s.logger.Error("agentapi.observe.wal_enqueue_failed",
			slog.String("episode_id", episodeID),
			slog.String("error", walErr.Error()))
		return ObserveResponse{}, fmt.Errorf("agentapi: observe: wal enqueue: %w", walErr)
	}

	s.logger.Warn("agentapi.observe.wal_fallback_engaged",
		slog.String("episode_id", episodeID),
		slog.String("reason", degradedReasonEpisodicLogUnavailable),
		slog.String("writer_error", writeErr.Error()))
	// Stage 8.1 priority: episodic_log_unavailable dominates
	// over consolidator_backpressure (the WAL outage is the
	// more severe signal — the row has not yet reached the
	// EpisodicLog, so the Consolidator queue position is moot).
	return s.applyDegradedContract(req.RepoID, ObserveResponse{
		EpisodeID:      episodeID,
		EpisodeGroupID: episodeGroupID,
		Degraded:       true,
		DegradedReason: degradedReasonEpisodicLogUnavailable,
	})
}

// applyDegradedContract is the Stage 8.1 wiring helper every
// successful Observe response funnels through. It runs in
// this order:
//
//  1. Apply fault-injection overlay when the injector is
//     wired AND a real degraded signal of equal-or-lower
//     priority is in place. Real outages dominate over
//     injected reasons so the test seam cannot mask a live
//     fallback.
//  2. Run `degraded.Enforce` on the final pair. A non-closed
//     reason fails the call so the gRPC adapter returns
//     codes.Internal — matching the §13 contract scenario
//     "closed degraded_reason enforced".
//  3. Increment the per-verb degraded metric on a valid
//     degraded response. The counter never increments on
//     `degraded=false` (the healthy path), nor on a 500
//     response (because Enforce returned an error first).
func (s *ObserveService) applyDegradedContract(repoID string, resp ObserveResponse) (ObserveResponse, error) {
	if s.faultInjector != nil {
		injDeg, injReason := s.faultInjector.Inject(VerbObserve, repoID)
		if injDeg && injReason != "" {
			// Priority overlay: only overwrite when the
			// injected reason has strictly higher priority,
			// or no real reason is currently set.  This
			// prevents a fault-injection test rule from
			// erasing a real episodic_log_unavailable signal
			// the WAL path just produced.
			if !resp.Degraded || degraded.Priority(injReason) > degraded.Priority(resp.DegradedReason) {
				s.logger.Warn("agentapi.observe.fault_injected",
					slog.String("repo_id", repoID),
					slog.String("reason", injReason))
				resp.Degraded = true
				resp.DegradedReason = injReason
			}
		}
	}
	if err := degraded.Enforce(resp.Degraded, resp.DegradedReason); err != nil {
		// Hard-fail.  The §13 scenario requires the wire to
		// either carry a closed reason OR fail; we choose
		// failure so the call is observable as Internal at the
		// gRPC adapter.
		s.logger.Error("agentapi.observe.closed_set_violation",
			slog.String("repo_id", repoID),
			slog.String("attempted_reason", resp.DegradedReason),
			slog.String("err", err.Error()))
		return ObserveResponse{}, fmt.Errorf("agentapi: observe: %w", err)
	}
	if resp.Degraded {
		if s.degradedMetric != nil {
			s.degradedMetric.IncDegraded(VerbObserve, resp.DegradedReason)
		}
	}
	return resp, nil
}

// walPayloadForDegraded stamps the §7.5 degraded fields onto
// the prepared EpisodeAppendInput before it lands in the WAL.
// The eventually-replayed Episode row thus carries
// `degraded=true, degraded_reason='episodic_log_unavailable'`
// per architecture.md §7.5 (table cell §6.4: "the Episode is
// still appended; if the EpisodicLog itself is degraded the
// writer buffers and replies degraded=true").
//
// We mutate a value copy (not the original) so the
// pre-Append payload remains free of degraded state — that
// matters because the function is called only on the WAL
// fallback path and the original `in` may still appear in
// logs / metrics for the failed direct-write attempt.
func walPayloadForDegraded(in EpisodeAppendInput) EpisodeAppendInput {
	in.Degraded = true
	in.DegradedReason = degradedReasonEpisodicLogUnavailable
	return in
}

// -- Validation ------------------------------------------------------

// validateObserveRequest enforces every Go-side pre-flight
// check.  Order is intentional: outcome check first so the C15
// error is surfaced even when other fields are missing (the
// caller's most-likely confusion is "why is human_corrected
// rejected", and we want to answer it immediately).
func validateObserveRequest(req ObserveRequest) error {
	// C15 first.  This rejection MUST happen before any other
	// validation so the caller's most actionable error message
	// surfaces, and so a malformed request carrying
	// human_corrected can never accidentally enter a later
	// branch that would write a row.
	if req.Outcome == "human_corrected" {
		return ErrHumanCorrectedNotAllowed
	}
	if _, ok := allowedOutcomes[req.Outcome]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidOutcome, req.Outcome)
	}
	if strings.TrimSpace(req.RepoID) == "" {
		return ErrMissingRepoID
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return ErrMissingSessionID
	}
	if strings.TrimSpace(req.TraceID) == "" {
		return ErrMissingTraceID
	}
	if len(req.ActionJSON) == 0 {
		return ErrMissingAction
	}
	if !json.Valid(req.ActionJSON) {
		return fmt.Errorf("%w: action_json", ErrInvalidJSON)
	}
	if len(req.SignalJSON) > 0 && !json.Valid(req.SignalJSON) {
		return fmt.Errorf("%w: signal_json", ErrInvalidJSON)
	}
	if strings.TrimSpace(req.ContextID) == "" {
		return ErrMissingContextID
	}
	for i, ref := range req.ObservationRefs {
		if err := validateObservationRef(i, ref); err != nil {
			return err
		}
	}
	return nil
}

// validateObservationRef enforces:
//   - role is one of the caller-allowed set (C23 rejects
//     degraded_recall_context BEFORE the closed-set check
//     so the error message names the specific architectural
//     violation rather than a generic "unknown role").
//   - exactly one target field matches the role per
//     architecture.md §5.3.3 (`role=node_hit` → `node_id`
//     only; `role=edge_hit`/`call_edge_hit` → `edge_id`;
//     `role=concept_hit` → `concept_id`).
func validateObservationRef(index int, ref ObservationRef) error {
	if ref.Role == observationRoleDegradedRecallContext {
		return fmt.Errorf("%w: observation_refs[%d]", ErrDegradedRecallContextRoleForbidden, index)
	}
	if _, ok := callerObservationRoles[ref.Role]; !ok {
		return fmt.Errorf("%w: observation_refs[%d].role=%q", ErrInvalidObservationRole, index, ref.Role)
	}
	hasNode := strings.TrimSpace(ref.NodeID) != ""
	hasEdge := strings.TrimSpace(ref.EdgeID) != ""
	hasConcept := strings.TrimSpace(ref.ConceptID) != ""
	count := 0
	if hasNode {
		count++
	}
	if hasEdge {
		count++
	}
	if hasConcept {
		count++
	}
	if count != 1 {
		return fmt.Errorf("%w: observation_refs[%d] must set exactly one of node_id/edge_id/concept_id (got %d)",
			ErrInvalidObservationTarget, index, count)
	}
	switch ref.Role {
	case observationRoleNodeHit:
		if !hasNode {
			return fmt.Errorf("%w: observation_refs[%d] role=node_hit requires node_id", ErrInvalidObservationTarget, index)
		}
	case observationRoleEdgeHit, observationRoleCallEdgeHit:
		if !hasEdge {
			return fmt.Errorf("%w: observation_refs[%d] role=%s requires edge_id", ErrInvalidObservationTarget, index, ref.Role)
		}
	case observationRoleConceptHit:
		if !hasConcept {
			return fmt.Errorf("%w: observation_refs[%d] role=concept_hit requires concept_id", ErrInvalidObservationTarget, index)
		}
	}
	return nil
}

// -- UUID v4 ---------------------------------------------------------

// newUUIDv4 mints a fresh RFC 4122 v4 UUID using crypto/rand.
// Kept package-internal (vs pulling in google/uuid) so this
// package's dependency surface stays tight.  The textual form
// is the canonical 8-4-4-4-12 lowercase hex used everywhere
// else in this module.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agentapi: uuid: read random: %w", err)
	}
	// RFC 4122 §4.4: set the version (high nibble of octet 6
	// to 0100b) and the variant (high two bits of octet 8 to
	// 10b).
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var buf [36]byte
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf[:]), nil
}
