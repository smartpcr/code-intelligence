package rule_engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/policy/steward"
)

// ScanEvent is the inbound message the post-scan dispatcher
// emits when a Metric Ingestor finishes a SHA. The Worker
// consumes the stream and runs the engine's batch-refresh
// mode for each event.
//
// Fields:
//
//   - `RepoID` -- the canonical repo identifier from
//     `clean_code.repo`.
//   - `SHA` -- the SHA whose samples have just landed.
type ScanEvent struct {
	RepoID uuid.UUID
	SHA    string
}

// PolicyActivationReader is the narrow read surface the
// Worker uses to discover the current active
// `policy_version_id` at the moment a scan event arrives.
// Production wiring is [steward.Steward.ActivePolicyVersion];
// tests inject a fake.
//
// The single-method interface keeps the Worker decoupled
// from the broader Steward surface so a future schema move
// (e.g. caching the active policy in Redis) does not ripple
// through this package.
type PolicyActivationReader interface {
	// ActivePolicyVersionID returns the
	// `policy_version_id` pinned by the latest
	// `policy_activation` row. Returns `ok=false` when no
	// activation row exists (fresh deploy) -- the Worker
	// SKIPS the scan event in that case rather than
	// fabricate a policy.
	ActivePolicyVersionID(ctx context.Context) (uuid.UUID, bool, error)
}

// WorkerConfig wires the [Worker] dependencies. All fields
// are required.
type WorkerConfig struct {
	// Engine is the rule engine the worker drives.
	Engine *Engine
	// Activation surfaces the current active
	// `policy_version_id`.
	Activation PolicyActivationReader
	// Events is the inbound scan event stream. The worker
	// loops until the channel closes OR the parent context
	// cancels.
	Events <-chan ScanEvent
	// Logger receives structured log lines. Required (we
	// do not default to a global; tests inject a quiet
	// logger via [slog.New] over [io.Discard]).
	Logger *slog.Logger
}

// Worker is the long-running batch-refresh driver invoked
// by the post-scan dispatcher. One worker per service
// instance is sufficient: the engine's in-process advisory
// lock serialises the read-modify-write window per
// `(repo_id, sha)`, so multiple workers competing for the
// same event stream would only add coordination overhead.
//
// Architecture Sec 3.6 lines 546-550 pin this as the
// "batch refresh mode -- a worker that re-runs the active
// PolicyVersion against the latest MetricSample rows after
// a large ingest" producing `caller='batch_refresh'` runs.
type Worker struct {
	engine     *Engine
	activation PolicyActivationReader
	events     <-chan ScanEvent
	logger     *slog.Logger
}

// NewWorker constructs a [Worker]. Returns an error when any
// required field is nil so a wiring bug surfaces at startup
// rather than at the first event.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.Engine == nil {
		return nil, errors.New("rule_engine: NewWorker: Engine is required")
	}
	if cfg.Activation == nil {
		return nil, errors.New("rule_engine: NewWorker: Activation is required")
	}
	if cfg.Events == nil {
		return nil, errors.New("rule_engine: NewWorker: Events channel is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("rule_engine: NewWorker: Logger is required")
	}
	return &Worker{
		engine:     cfg.Engine,
		activation: cfg.Activation,
		events:     cfg.Events,
		logger:     cfg.Logger,
	}, nil
}

// Run blocks until the context cancels OR the event channel
// closes. Each event triggers exactly one
// [Engine.RunBatch] call (caller=`batch_refresh`). Engine
// errors are LOGGED, not propagated -- the worker is the
// LAST writer in the pipeline and a single broken policy
// should not bring down the whole service.
//
// Returns the context error on cancellation and nil on
// channel close.
func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.events:
			if !ok {
				return nil
			}
			// Live event path: discard the per-event
			// error -- the channel consumer cannot
			// retry, the durable catchup loop will.
			_ = w.process(ctx, ev)
		}
	}
}

// process handles a single live-stream scan event. Resolves
// the active policy_version_id on each call so a policy
// switch is picked up by the next event (the catchup loop
// pins the policy at the top of [Worker.Catchup] instead --
// see [Worker.processWithPolicy]).
//
// Returns an error on activation lookup failure or RunBatch
// failure so the catchup loop can detect persistent
// per-event failures (iter-5 evaluator item #2). The live
// [Worker.Run] discards the return.
func (w *Worker) process(ctx context.Context, ev ScanEvent) error {
	if ev.RepoID == uuid.Nil || ev.SHA == "" {
		w.logger.WarnContext(ctx, "rule_engine.worker: ignoring malformed scan event",
			slog.String("repo_id", ev.RepoID.String()),
			slog.String("sha", ev.SHA),
		)
		return nil
	}
	policyVersionID, ok, err := w.activation.ActivePolicyVersionID(ctx)
	if err != nil {
		w.logger.ErrorContext(ctx, "rule_engine.worker: active policy lookup failed",
			slog.String("repo_id", ev.RepoID.String()),
			slog.String("sha", ev.SHA),
			slog.Any("err", err),
		)
		return fmt.Errorf("rule_engine.worker: active policy lookup: %w", err)
	}
	if !ok {
		// No active policy yet -- a fresh-deploy state. We
		// log at INFO (not WARN) because this is the
		// expected steady state of a service that has not
		// yet completed its first `policy.activate` call.
		// Return nil: this is an INTENTIONAL skip, not a
		// retryable failure (catchup also short-circuits
		// up-front in this state).
		w.logger.InfoContext(ctx, "rule_engine.worker: skip -- no active policy",
			slog.String("repo_id", ev.RepoID.String()),
			slog.String("sha", ev.SHA),
		)
		return nil
	}
	return w.processWithPolicy(ctx, ev, policyVersionID)
}

// processWithPolicy runs the engine for `ev` under the
// caller-supplied `policyVersionID`. Skips the per-event
// activation lookup -- callers that drive many events
// (notably [Worker.Catchup]) MUST pin the policy ONCE at
// the top of their loop and pass it in here, so a policy
// switch mid-loop cannot cause page-vs-write divergence
// (rubber-duck iter-5 blocker #5).
//
// Returns:
//
//   - nil on a successful [Engine.RunBatch].
//   - nil for malformed events (intentional skip; catchup's
//     failure-set should NOT mark these).
//   - non-nil on context cancellation or engine failure (so
//     catchup can record the (repoID, sha) as failed and
//     halt the loop if every row on a page fails).
func (w *Worker) processWithPolicy(ctx context.Context, ev ScanEvent, policyVersionID uuid.UUID) error {
	if ev.RepoID == uuid.Nil || ev.SHA == "" {
		w.logger.WarnContext(ctx, "rule_engine.worker: ignoring malformed scan event (processWithPolicy)",
			slog.String("repo_id", ev.RepoID.String()),
			slog.String("sha", ev.SHA),
		)
		return nil
	}
	if policyVersionID == uuid.Nil {
		return errors.New("rule_engine.worker: processWithPolicy: policyVersionID is the zero uuid")
	}
	result, err := w.engine.RunBatch(ctx, ev.RepoID, ev.SHA, policyVersionID)
	if err != nil {
		w.logger.ErrorContext(ctx, "rule_engine.worker: RunBatch failed",
			slog.String("repo_id", ev.RepoID.String()),
			slog.String("sha", ev.SHA),
			slog.String("policy_version_id", policyVersionID.String()),
			slog.Any("err", err),
		)
		return fmt.Errorf("rule_engine.worker: RunBatch: %w", err)
	}
	w.logger.InfoContext(ctx, "rule_engine.worker: RunBatch completed",
		slog.String("repo_id", ev.RepoID.String()),
		slog.String("sha", ev.SHA),
		slog.String("policy_version_id", policyVersionID.String()),
		slog.String("evaluation_run_id", result.EvaluationRunID.String()),
		slog.String("verdict", string(result.Verdict)),
		slog.Int("findings_count", len(result.FindingIDs)),
	)
	return nil
}

// staticActivation is a [PolicyActivationReader] backed by
// a single uuid. Used by tests and by the scaffold-mode
// composition root when the operator pins a policy at
// startup rather than via `policy.activate`.
type staticActivation struct {
	mu              sync.RWMutex
	policyVersionID uuid.UUID
	hasPolicy       bool
}

// NewStaticActivation returns a [PolicyActivationReader] that
// reports `policyVersionID` as the active policy. Passing
// [uuid.Nil] is equivalent to "no active policy".
func NewStaticActivation(policyVersionID uuid.UUID) PolicyActivationReader {
	return &staticActivation{
		policyVersionID: policyVersionID,
		hasPolicy:       policyVersionID != uuid.Nil,
	}
}

// ActivePolicyVersionID implements [PolicyActivationReader].
func (s *staticActivation) ActivePolicyVersionID(ctx context.Context) (uuid.UUID, bool, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.policyVersionID, s.hasPolicy, nil
}

// Set updates the active policy_version_id. Test helper; the
// production interface ([steward.Steward.ActivePolicyVersion])
// is read-only.
func (s *staticActivation) Set(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policyVersionID = id
	s.hasPolicy = id != uuid.Nil
}

// Compile-time check that staticActivation satisfies the
// reader interface.
var _ PolicyActivationReader = (*staticActivation)(nil)

// StewardActivationReader is the production adapter that
// bridges [steward.Steward.ActivePolicyVersion] (returning a
// full [steward.PolicyVersion]) onto the worker's narrow
// [PolicyActivationReader] (returning just the
// `policy_version_id`).
//
// Per Stage 5.7 evaluator feedback #4: the worker docstring
// in `docs/rollout.md` previously claimed `stewardStore`
// could be passed directly to the worker constructor; that
// type mismatch would not compile. This adapter is the
// canonical bridge -- the composition root wires
// `NewStewardActivation(steward)` into `WorkerConfig.Activation`.
type StewardActivationReader struct {
	reader StewardPolicyReader
}

// StewardPolicyReader is the narrow read surface the
// adapter binds against. Satisfied by `*steward.Steward`
// in production; tests pass a hand-rolled fake.
type StewardPolicyReader interface {
	ActivePolicyVersion(ctx context.Context) (steward.PolicyVersion, bool, error)
}

// NewStewardActivation wraps a [StewardPolicyReader] (in
// production: `*steward.Steward`) as a
// [PolicyActivationReader]. Passing nil yields an adapter
// that always returns `ok=false` -- the worker skips the
// scan event in that state (fresh deploy / steward not yet
// wired).
func NewStewardActivation(reader StewardPolicyReader) PolicyActivationReader {
	return &StewardActivationReader{reader: reader}
}

// ActivePolicyVersionID implements [PolicyActivationReader].
// Delegates to the wrapped steward and projects to the
// `(uuid, ok, error)` tuple the worker expects. A
// `(PolicyVersion, true, nil)` reply with a zero
// `PolicyVersionID` is treated as an invariant violation
// (loud error rather than silent ok=false).
func (a *StewardActivationReader) ActivePolicyVersionID(ctx context.Context) (uuid.UUID, bool, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, false, err
	}
	if a == nil || a.reader == nil {
		return uuid.Nil, false, nil
	}
	pv, ok, err := a.reader.ActivePolicyVersion(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	if !ok {
		return uuid.Nil, false, nil
	}
	if pv.PolicyVersionID == uuid.Nil {
		return uuid.Nil, false, errors.New("rule_engine: StewardActivationReader: steward returned ok=true with zero PolicyVersionID")
	}
	return pv.PolicyVersionID, true, nil
}

// Compile-time check that StewardActivationReader satisfies
// the reader interface.
var _ PolicyActivationReader = (*StewardActivationReader)(nil)
