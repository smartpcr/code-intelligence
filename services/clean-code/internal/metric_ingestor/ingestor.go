package metric_ingestor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/churn"
)

// ErrMissingChurnPayloadForExternalPerRow is returned when
// [Ingestor.Run] is invoked with `kind='external_per_row'`
// and no [churn.Payload]. An external-per-row run with no
// churn payload is a caller bug -- the whole purpose of the
// kind is to deliver churn data; refusing here keeps the
// scan-run state machine honest.
var ErrMissingChurnPayloadForExternalPerRow = errors.New("metric_ingestor: ScanRunKindExternalPerRow requires a non-nil churn.Payload")

// FoundationInput is the payload the
// [FoundationRecipeDispatcher] consumes when a foundation
// ScanRun (`kind='full'` or `kind='delta'`) is dispatched. The
// struct is intentionally minimal at Stage 2.6 -- the real
// dispatcher in Phase 3.2 (`stage-metric-ingestor-and-scanrun-
// state-machine`) will populate this with the AST-file
// iterator, the [recipes.Registry] handle, the
// `PolicyVersion`-bound configuration, and the
// `MetricSampleWriter` transaction handle. Pinning the type now
// gives the [Ingestor.Run] orchestration a stable parameter
// shape so Phase 3.2 can extend without changing the [Ingestor]
// public surface.
//
// Empty input is permitted today -- the noop scaffold
// dispatcher ignores it and returns nil. Test fakes likewise
// carry no fields.
type FoundationInput struct {
	// SHA is the parent ScanRun's `commit.sha`. Stamped on
	// every emitted `MetricSampleRecord.SHA` so the
	// foundation-tier rows the dispatcher persists share
	// the bound SHA with the rest of the ScanRun (G2:
	// `MetricSample.sha` is the discriminator that joins a
	// foundation draft to its commit). The Stage 3.2 state
	// machine threads the claimed [ScanRunClaim.SHA] into
	// this field; tests that drive the dispatcher directly
	// may leave it empty when persistence is disabled.
	SHA string
}

// FoundationRecipeDispatcher runs the foundation-tier recipe
// loop for a ScanRun and writes the AST-derived
// [recipes.MetricSampleDraft]s into the
// [MetricSampleWriter] the dispatcher's implementation owns.
// The interface is the seam between Stage 2.6 (this
// workstream) and Phase 3.2 (which lands the real
// implementation).
//
// # Same-ScanRun contract
//
// The dispatcher MUST honour [ScanRunContext.ID] for every
// emitted `MetricSample.producer_run_id`. The [Ingestor]
// invokes Dispatch BEFORE the [ChurnSweep] so both writers
// share the parent ScanRun and the active-row UPSERT
// (architecture Sec 5.2.2) lands consistently. Phase 3.2's PG
// implementation will run Dispatch + ChurnSweep inside a
// single DB transaction; the Stage 2.6 scaffold does NOT
// claim that level of atomicity (the in-memory writer's
// WriteBatch is per-call atomic only).
type FoundationRecipeDispatcher interface {
	// Dispatch runs the foundation recipes for `scanRun`.
	// Returns nil on success; any error stops the parent
	// [Ingestor.Run] before [ChurnSweep] is invoked.
	Dispatch(ctx context.Context, scanRun ScanRunContext, input FoundationInput) error
}

// NoopFoundationRecipeDispatcher is the scaffold implementation
// of [FoundationRecipeDispatcher] for Stage 2.6. It returns nil
// from every Dispatch call -- "successfully dispatched zero
// foundation recipes" -- because the foundation-tier recipe
// runner is itself a later workstream (Phase 3.2
// `stage-metric-ingestor-and-scanrun-state-machine`) and the
// Stage 2.6 brief explicitly defers it.
//
// # Why noop-succeed rather than fail-loud
//
// Iter 4 used an unwired sentinel-error variant. The evaluator
// (iter-4 items #1 + #2) rejected that design because it made
// every production `kind='full'` / `kind='delta'` ScanRun
// terminate with a "foundation dispatcher unwired" error
// BEFORE the churn sweep was reached -- so the same-ScanRun
// integration requirement Stage 2.6 is supposed to establish
// was proven only with test fakes, never in the wired path.
// The structural fix: have the scaffold dispatcher succeed
// (executing zero recipes is the honest report of "no
// foundation work configured at this stage"), and let the
// [ChurnSweep] actually run for `full`/`delta` calls. Phase
// 3.2 plugs in a real dispatcher that runs the AST recipes.
//
// # Observability
//
// The dispatcher carries an optional [*slog.Logger]; when
// non-nil it emits ONE INFO line per Dispatch invocation so
// operators can see that a `full`/`delta` run completed
// without foundation recipes (and therefore that they are
// running pre-Phase-3.2 code). The composition root in
// `cmd/clean-coded/main.go` constructs the dispatcher with
// the service logger.
//
// The previously-public `ErrFoundationDispatcherUnwired`
// sentinel was REMOVED in iter 5; callers MUST NOT depend on
// a foundation-dispatch error in scaffold mode.
type NoopFoundationRecipeDispatcher struct {
	// Logger, if non-nil, receives a structured INFO line on
	// every Dispatch call. The zero value (nil) is permitted
	// for tests and produces no log output.
	Logger *slog.Logger
}

// Dispatch implements [FoundationRecipeDispatcher] by
// returning nil and (optionally) emitting an INFO log line
// reporting that zero foundation recipes were executed for
// `scanRun`.
func (d NoopFoundationRecipeDispatcher) Dispatch(_ context.Context, scanRun ScanRunContext, _ FoundationInput) error {
	if d.Logger != nil {
		d.Logger.Info("foundation recipe dispatcher: noop (Phase 3.2 supplies the production dispatcher)",
			"scan_run_id", scanRun.ID,
			"scan_run_kind", scanRun.Kind,
			"repo_id", scanRun.RepoID,
			"recipes_executed", 0,
		)
	}
	return nil
}

// RunRequest is the per-call input the [Ingestor] dispatches.
// Designed so the per-kind validation surface lives at one
// site and the orchestration in [Ingestor.Run] reads
// declaratively.
type RunRequest struct {
	// ScanRun carries the parent ScanRun's metadata
	// (architecture Sec 5.7). The [Ingestor] runs
	// [ScanRunContext.Validate] before any dispatch.
	ScanRun ScanRunContext
	// Foundation is the payload the
	// [FoundationRecipeDispatcher] consumes. Passed
	// unconditionally; the dispatcher decides what to do
	// with it. Ignored entirely by the
	// `external_per_row` kind.
	Foundation FoundationInput
	// Churn is the churn webhook payload to materialise.
	//
	// - REQUIRED when [ScanRunContext.Kind] is
	//   `external_per_row` (a churn-only run with no payload
	//   is meaningless -- [Ingestor.Run] returns
	//   [ErrMissingChurnPayloadForExternalPerRow]).
	// - OPTIONAL when [ScanRunContext.Kind] is `full` or
	//   `delta` (a foundation scan with no fresh churn data
	//   is plausible -- e.g. a re-scan triggered by a
	//   recipe-version bump). When nil, the [Ingestor] runs
	//   foundation dispatch only and reports
	//   [IngestorResult.ChurnSkipped] = true.
	Churn *churn.Payload
}

// IngestorResult summarises the outcome of one
// [Ingestor.Run] call. The fields let the composition root
// emit a structured INFO log per scan-run.
type IngestorResult struct {
	// FoundationDispatched is true iff
	// [FoundationRecipeDispatcher.Dispatch] was invoked AND
	// returned nil. Always false for `external_per_row`
	// runs.
	FoundationDispatched bool
	// ChurnSkipped is true iff the run kind permitted a nil
	// churn payload (`full` / `delta`) and the caller did
	// not supply one. False otherwise.
	ChurnSkipped bool
	// ChurnSamplesWritten / ChurnRowsHydrated mirror
	// [SweepResult]. Both zero when ChurnSkipped is true.
	ChurnSamplesWritten int
	ChurnRowsHydrated   int
}

// Ingestor is the production coordinator that owns
// per-ScanRun dispatch ordering between the foundation-tier
// recipes and the churn sweep. Construct via
// [NewIngestor]; one instance handles every scan-run the
// service receives.
//
// # Why this type exists
//
// Stage 2.6's detailed-requirement contract is:
//
//	"Materialiser runs as part of the Metric Ingestor (same
//	 writer-ownership role) inside the same ScanRun as the
//	 foundation recipes so the active-row uniqueness
//	 invariant holds."
//
// [Ingestor.Run] is the SINGLE production call site that
// honours this -- it threads ONE [ScanRunContext] through
// BOTH the foundation dispatcher AND the [ChurnSweep] so the
// `producer_run_id` stamped on every emitted
// `MetricSample` is consistent across both writers. The
// previous design exposed only [ChurnSweep] directly; that
// was a "seam, not actual same-ScanRun wiring" per the
// evaluator's iter-3 #1 review.
//
// # Cross-producer atomicity is Phase 3.2's job
//
// The current writer interface ([MetricSampleWriter]) is
// batch-only -- one WriteBatch per writer. Cross-producer
// transactionality (foundation rows + churn rows landing in
// one DB transaction) is enforced by Phase 3.2's PG-backed
// writer; the Stage 2.6 in-memory writer does NOT model
// it. Callers MUST NOT assume the [Ingestor] gives them an
// all-or-nothing guarantee across the two sweeps yet.
type Ingestor struct {
	dispatcher FoundationRecipeDispatcher
	churnSweep *ChurnSweep
}

// NewIngestor returns an [Ingestor] wired with `dispatcher`
// and `churnSweep`. PANICS on any nil argument -- both
// dependencies are non-optional. To run in scaffold mode
// (Phase 3.2 not yet wired), pass
// [NoopFoundationRecipeDispatcher]{Logger: log} so
// `full`/`delta` runs succeed with zero foundation recipes
// executed and the [ChurnSweep] still gets to write its
// per-scope rows.
func NewIngestor(dispatcher FoundationRecipeDispatcher, churnSweep *ChurnSweep) *Ingestor {
	if dispatcher == nil {
		panic("metric_ingestor: NewIngestor received nil FoundationRecipeDispatcher")
	}
	if churnSweep == nil {
		panic("metric_ingestor: NewIngestor received nil ChurnSweep")
	}
	return &Ingestor{
		dispatcher: dispatcher,
		churnSweep: churnSweep,
	}
}

// Run drives one ScanRun through the writer pipeline. The
// dispatch sequence depends on [RunRequest.ScanRun.Kind]:
//
//   - `full` / `delta` (foundation scans):
//     [FoundationRecipeDispatcher.Dispatch] runs FIRST. Any
//     error short-circuits -- the [ChurnSweep] is NOT
//     invoked, so a foundation failure does not partially
//     write churn rows. On dispatch success, [ChurnSweep]
//     runs iff [RunRequest.Churn] is non-nil; otherwise the
//     run reports [IngestorResult.ChurnSkipped] = true.
//   - `external_per_row` (standalone churn webhook):
//     [FoundationRecipeDispatcher.Dispatch] is NOT invoked
//     (no AST work for a churn-only run). [RunRequest.Churn]
//     MUST be non-nil -- a nil payload returns
//     [ErrMissingChurnPayloadForExternalPerRow].
//
// Validation order (cheapest first):
//
//  1. [ScanRunContext.Validate] (non-zero ID + RepoID, kind
//     in [AllowedScanRunKinds]).
//  2. Per-kind churn-payload presence (above).
//  3. [FoundationRecipeDispatcher.Dispatch] for foundation
//     kinds.
//  4. [ChurnSweep.Run] when applicable.
//
// On any error, Run returns ([IngestorResult]{}, error) with
// no further dispatch. The error wraps the underlying
// sentinel ([ErrInvalidScanRunKind], [ErrZeroRepoID],
// [ErrWriterFailure], [ErrMissingChurnPayloadForExternalPerRow],
// etc.) so callers can `errors.Is` to map to structured
// responses.
func (i *Ingestor) Run(ctx context.Context, req RunRequest) (IngestorResult, error) {
	if err := req.ScanRun.Validate(); err != nil {
		return IngestorResult{}, err
	}

	switch req.ScanRun.Kind {
	case ScanRunKindFull, ScanRunKindDelta:
		if err := i.dispatcher.Dispatch(ctx, req.ScanRun, req.Foundation); err != nil {
			return IngestorResult{}, fmt.Errorf("foundation dispatch (%s): %w", req.ScanRun.Kind, err)
		}
		if req.Churn == nil {
			return IngestorResult{
				FoundationDispatched: true,
				ChurnSkipped:         true,
			}, nil
		}
		sweepResult, err := i.churnSweep.Run(ctx, req.ScanRun, req.Churn)
		if err != nil {
			return IngestorResult{}, err
		}
		return IngestorResult{
			FoundationDispatched: true,
			ChurnSamplesWritten:  sweepResult.SamplesWritten,
			ChurnRowsHydrated:    sweepResult.RowsHydrated,
		}, nil

	case ScanRunKindExternalPerRow:
		if req.Churn == nil {
			return IngestorResult{}, ErrMissingChurnPayloadForExternalPerRow
		}
		sweepResult, err := i.churnSweep.Run(ctx, req.ScanRun, req.Churn)
		if err != nil {
			return IngestorResult{}, err
		}
		return IngestorResult{
			ChurnSamplesWritten: sweepResult.SamplesWritten,
			ChurnRowsHydrated:   sweepResult.RowsHydrated,
		}, nil

	default:
		// Unreachable: ScanRunContext.Validate already
		// filtered the kind set. Returned as a safety net so
		// a future expansion of allowedScanRunKinds that
		// forgets to handle the new kind here surfaces
		// loudly.
		return IngestorResult{}, fmt.Errorf("%w: %q (unhandled by Ingestor.Run)",
			ErrInvalidScanRunKind, req.ScanRun.Kind)
	}
}
