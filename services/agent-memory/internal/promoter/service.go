package promoter

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// Default parameter values surfaced both as package constants
// (so the binary's loadConfig can reference them in env-var
// help text) and as the zero-value fallback inside `New`.
const (
	// DefaultConfidenceThreshold is the minimum
	// concept_version.confidence value required for a Concept
	// to cross the §7.8 publishable threshold. The promoter's
	// candidate query AND the per-Concept recheck both gate on
	// `confidence >= DefaultConfidenceThreshold`.
	DefaultConfidenceThreshold = 0.7

	// DefaultSupportThreshold is the minimum
	// concept_version.support_count value required for a
	// Concept to cross the §7.8 publishable threshold.
	DefaultSupportThreshold = 5

	// DefaultRunInterval is the long-poll cadence per
	// architecture §7.7 / implementation-plan.md §6.2. The
	// promoter wakes after each consolidator run, but
	// operationally we drive it on an interval so a single
	// stalled consolidator does not silently stop the promoter.
	DefaultRunInterval = 1 * time.Minute

	// DefaultTickTimeout bounds a single Tick invocation so a
	// stuck embedder or Qdrant call cannot stall the poll loop
	// indefinitely. Generous enough that the candidate-scan +
	// per-concept publishes complete on a typical dev cluster.
	DefaultTickTimeout = 5 * time.Minute

	// DefaultCandidateBatchSize caps how many candidates we
	// process per tick. Acts as backpressure so a sudden
	// avalanche of newly-promoted concepts does not starve
	// the embedder or hold the advisory lock for minutes.
	DefaultCandidateBatchSize = 64

	// DefaultRetryBatchSize caps how many stalled publishes we
	// retry per tick. Smaller than the candidate batch because
	// each retry re-runs the embed+upsert+confirm chain.
	DefaultRetryBatchSize = 16

	// DefaultMaxRetryAttempts is the per-publish hard cap on
	// the number of retry attempts the Promoter will spend on
	// a stalled embedding_publish row before abandoning it for
	// the rest of its lifetime. Without this cap a permanently-
	// failing publish (corrupt fingerprint payload, Qdrant
	// schema mismatch, an embedding that consistently fails
	// confirm, etc.) is re-queued every single tick, burning
	// an embedder API call + a Qdrant upsert + DB writes per
	// tick with zero chance of success — and crowds legitimate
	// retries out of the RetryBatchSize budget.
	//
	// The cap is intentionally generous (10) so a transient
	// outage of any single downstream (embedder, Qdrant, PG)
	// has many ticks' worth of headroom to clear before a
	// publish is given up on. Once a publish hits the cap, the
	// row is left untouched in the database (latest event
	// stays at whatever non-terminal state it was in) and
	// surfaced as a TickResult.RetriesAbandoned increment
	// plus a structured Error log per occurrence, so an
	// operator can investigate the underlying poison row
	// without the promoter wasting downstream call budget.
	//
	// 10 is the default rather than e.g. 3 because the
	// embed → upsert → confirm chain has three independent
	// failure surfaces; a row that fails once at each surface
	// in alternating ticks should still complete inside the
	// budget.
	DefaultMaxRetryAttempts = 10

	// PromoterAdvisoryLockKey is the cluster-wide bigint
	// pg_try_advisory_lock key the Promoter uses to serialise
	// its tick across replicas. The numeric value is the
	// big-endian ASCII encoding of "PROMOTE!" -- distinct
	// from the consolidator's "CONSOLID" so a single
	// PostgreSQL instance can host both workers without one
	// blocking the other.
	//
	// 0x50 0x52 0x4F 0x4D 0x4F 0x54 0x45 0x21 = "PROMOTE!"
	//
	// Reproducible from the literal bytes via
	//   printf 'PROMOTE!' | od -An -tx1 | tr -d ' '
	// yielding 0x50524F4D4F544521.
	PromoterAdvisoryLockKey int64 = 0x50524F4D4F544521
)

// ────────────────────────────────────────────────────────────
// promoter_run.status lifecycle (CANONICAL REFERENCE)
// ────────────────────────────────────────────────────────────
//
// Every Tick OPENs a promoter_run row with status=StatusRunning
// and finalises it with EXACTLY ONE of the three terminal
// statuses below. The status column has NO CHECK constraint in
// migration 0012 precisely so new values can be introduced
// without a schema change. Mirrors the consolidator package's
// lifecycle pattern verbatim.
const (
	// StatusRunning is written by openRun on the in-flight
	// promoter_run row.
	StatusRunning = "running"

	// StatusDone is the success terminal status.
	StatusDone = "done"

	// StatusLockSkipped is the terminal status used when
	// pg_try_advisory_lock returned FALSE (another promoter
	// instance holds the lock). The tick is a no-op: no
	// candidate scan, no per-concept work, no metric
	// adjustment. The row exists for operator observability.
	StatusLockSkipped = "lock_skipped"

	// StatusFailed is the terminal status written by the
	// deferred cleanup path when any step after openRun
	// returns an error.
	StatusFailed = "failed"
)

// ────────────────────────────────────────────────────────────
// PUBLISH MODE — what runAttempt is processing on this call
// ────────────────────────────────────────────────────────────
//
// The promoter has three entry points into runAttempt (the
// shared steps 4-7 of §9.6a):
//
//   - publishModeFresh: a brand-new ConceptVersion just got
//     emitted; the embedding_publish row + queued event are
//     fresh; attempt_index starts at 0.
//   - publishModeRetry: an existing embedding_publish row
//     stalled at queued / vector_written / failed; we are
//     re-running the chain. attempt_index increments.
//   - publishModeOrphan: the ConceptVersion(promoted=true)
//     row exists from a prior tick but its tx2 sibling
//     embedding_publish row was never committed (tx2 failure
//     after tx1 committed). We INSERT a fresh
//     embedding_publish + queued event, then drive the chain
//     at attempt_index 0. Distinct from publishModeFresh
//     because the ConceptVersion already exists (no tx1).
//     Evaluator-2 finding #1 fix; without this orphans were
//     durably skipped (NOT EXISTS-promoted filter on the
//     forward phase AND empty-publish-row filter on the
//     retry phase).
const (
	publishModeFresh  = "fresh"
	publishModeRetry  = "retry"
	publishModeOrphan = "orphan"
)

// pgUniqueViolationCode is the PostgreSQL SQLSTATE returned
// when a UNIQUE constraint (e.g.
// concept_version_concept_version_uidx) is violated. Inspected
// in the per-Concept transaction's INSERT path so we can log
// and skip a race-loser instead of failing the whole tick.
const pgUniqueViolationCode = "23505"

// Config is the env-derived (or programmatic) configuration the
// Service consumes. Construct via `Config{...}` literal and
// pass to `New`; missing optional fields fall back to the
// corresponding Default* constant.
type Config struct {
	// ConfidenceThreshold gates which concept_version rows are
	// candidates for promotion. Zero or negative falls back to
	// DefaultConfidenceThreshold (0.7).
	ConfidenceThreshold float64

	// SupportThreshold is the minimum support_count required
	// for promotion. Zero or negative falls back to
	// DefaultSupportThreshold (5).
	SupportThreshold int

	// RunInterval is the long-poll cadence. Zero or negative
	// falls back to DefaultRunInterval (1 minute).
	RunInterval time.Duration

	// TickTimeout bounds a single Tick invocation. Zero or
	// negative falls back to DefaultTickTimeout (5 minutes).
	TickTimeout time.Duration

	// CandidateBatchSize caps how many fresh candidates we
	// process per tick. Zero or negative falls back to
	// DefaultCandidateBatchSize.
	CandidateBatchSize int

	// RetryBatchSize caps how many stalled publishes we retry
	// per tick. Zero or negative falls back to
	// DefaultRetryBatchSize.
	RetryBatchSize int

	// MaxRetryAttempts is the per-publish hard cap on retry
	// attempts. Once a stalled publish row's
	// max(attempt_index) reaches this value, processRetries
	// stops re-queueing it (the row is logged at Error level
	// and counted in TickResult.RetriesAbandoned). Zero or
	// negative falls back to DefaultMaxRetryAttempts (10).
	MaxRetryAttempts int

	// AdvisoryLockKey is the bigint key the Service uses for
	// pg_try_advisory_lock-based cross-replica serialisation.
	// Zero falls back to PromoterAdvisoryLockKey. Tests
	// override to per-test random keys.
	AdvisoryLockKey int64
}

// Service is the long-lived promoter object the binary hosts.
// All public methods are goroutine-safe (the only mutable
// state is the atomic counters in `metrics`).
type Service struct {
	db       *sql.DB
	embedder embedding.Embedder
	qdrant   embedding.Qdrant
	cfg      Config
	logger   *slog.Logger
	metrics  *Metrics

	// newUUID is overridable so tests can pin deterministic
	// publish_id / point_id values. Production passes nil →
	// uses embedding.NewUUIDv4.
	newUUID func() (string, error)
}

// Option is the functional-options shape used to construct a
// Service without bloating the constructor's positional
// arguments.
type Option func(*Service)

// WithUUIDFactory overrides the UUID minter. Tests pin a
// deterministic point_id this way; production omits the
// option and the default embedding.NewUUIDv4 is used.
func WithUUIDFactory(fn func() (string, error)) Option {
	return func(s *Service) {
		if fn != nil {
			s.newUUID = fn
		}
	}
}

// New constructs a Service. Panics on nil *sql.DB / Embedder /
// Qdrant — a nil-anything Service has no useful behaviour and
// silently swallowing nil-deref panics would mask the wiring
// bug.
func New(db *sql.DB, embedder embedding.Embedder, qdrant embedding.Qdrant, cfg Config, logger *slog.Logger, opts ...Option) (*Service, error) {
	if db == nil {
		panic("promoter: nil *sql.DB")
	}
	if embedder == nil {
		panic("promoter: nil Embedder")
	}
	if qdrant == nil {
		panic("promoter: nil Qdrant")
	}
	if cfg.ConfidenceThreshold <= 0 {
		cfg.ConfidenceThreshold = DefaultConfidenceThreshold
	}
	if cfg.SupportThreshold <= 0 {
		cfg.SupportThreshold = DefaultSupportThreshold
	}
	if cfg.RunInterval <= 0 {
		cfg.RunInterval = DefaultRunInterval
	}
	if cfg.TickTimeout <= 0 {
		cfg.TickTimeout = DefaultTickTimeout
	}
	if cfg.CandidateBatchSize <= 0 {
		cfg.CandidateBatchSize = DefaultCandidateBatchSize
	}
	if cfg.RetryBatchSize <= 0 {
		cfg.RetryBatchSize = DefaultRetryBatchSize
	}
	if cfg.MaxRetryAttempts <= 0 {
		cfg.MaxRetryAttempts = DefaultMaxRetryAttempts
	}
	if cfg.AdvisoryLockKey == 0 {
		cfg.AdvisoryLockKey = PromoterAdvisoryLockKey
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{
		db:       db,
		embedder: embedder,
		qdrant:   qdrant,
		cfg:      cfg,
		logger:   logger,
		metrics:  NewMetrics(),
		newUUID:  embedding.NewUUIDv4,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Metrics exposes the package counters for the binary's
// /metrics endpoint and for integration tests.
func (s *Service) Metrics() *Metrics { return s.metrics }

// Config returns the resolved configuration (with defaults
// substituted in). Read-only; mutations to the returned struct
// are NOT reflected in the Service.
func (s *Service) Config() Config { return s.cfg }

// TickResult carries the outcome of a single Tick call.
type TickResult struct {
	// RunID is the promoter_run row UUID opened by this tick.
	// Always populated when err is nil; populated when the
	// run row was successfully opened even if a later step
	// failed.
	RunID string

	// LockSkipped is true when pg_try_advisory_lock returned
	// false (a sibling Promoter replica is mid-tick). In that
	// case the tick is a no-op and the run row was finalised
	// with status=StatusLockSkipped.
	LockSkipped bool

	// CandidatesEvaluated is the count of per-Concept
	// transactions that PASSED the threshold recheck (i.e.
	// the Promoter actually appended a ConceptVersion with
	// promoted=true on this tick).
	CandidatesEvaluated uint64

	// CandidatesPending is the count of candidates the
	// threshold query returned BEFORE the per-Concept
	// recheck loop. Includes candidates that the recheck
	// later dropped.
	CandidatesPending uint64

	// RetriesAttempted is the count of stalled publishes the
	// retry phase picked up on this tick.
	RetriesAttempted uint64

	// RetriesAbandoned is the count of stalled publishes the
	// retry phase REFUSED to pick up on this tick because
	// their existing max(attempt_index) had reached
	// Config.MaxRetryAttempts. Each abandoned row is also
	// surfaced as a structured `promoter.retry_abandoned`
	// Error log so the operator can locate the poison row
	// without scraping metrics. A row that crosses the cap is
	// left untouched in the DB (its latest event_kind stays
	// where it was) so a future manual recovery can re-drive
	// it after the underlying defect is fixed.
	RetriesAbandoned uint64

	// ConceptsPromoted is the count of publish chains that
	// reached the terminal `published` event on this tick.
	// Includes both fresh promotions and successful retries.
	// This is the value written into
	// `promoter_run.concepts_promoted` on finalise.
	ConceptsPromoted uint64

	// PublishFailures is the count of `failed` events
	// appended on this tick (either embedder or Qdrant
	// surfaced an error that we DURABLY recorded).
	PublishFailures uint64

	// OrphansRecovered is the count of orphaned promoted
	// ConceptVersion rows (rows that committed in tx1 but
	// whose tx2 EmbeddingPublish INSERT failed in a prior
	// tick, leaving them invisible to BOTH selectCandidates
	// AND selectStalled) that the orphan-recovery phase
	// converted into a published vector on this tick.
	// Evaluator-2 finding #1: without this phase those rows
	// were durably skipped forever.
	OrphansRecovered uint64

	// OrphansPending is the per-tick gauge of orphaned
	// promoted ConceptVersion rows the orphan-recovery scan
	// returned BEFORE the per-orphan driver loop. Captures
	// the depth of the orphan queue at the moment of the
	// scan; a steady non-zero value indicates a persistent
	// tx2 failure mode (DB outage, schema drift) that
	// keeps generating fresh orphans tick-after-tick.
	OrphansPending uint64
}

// Tick runs ONE promoter pass. The lifecycle is:
//
//  1. INSERT promoter_run(status='running', started_at=now())
//     in its own transaction so any later step that needs to
//     reference run_id sees a committed row (architecture.md
//     §5.5.2 line 620 makes ConceptVersion.producer_run_id an
//     FK in spirit, app-enforced).
//
//  2. Acquire pg_try_advisory_lock on a dedicated session
//     connection; if not acquired, finalise the run row with
//     status=StatusLockSkipped.
//
//  3. RETRY PHASE: scan embedding_publish rows whose target is
//     a concept_version_id and whose LATEST event_kind is NOT
//     in ('published', 'superseded'). For each, append a fresh
//     `queued` event at attempt_index+1, then re-run steps
//     4-7 of §9.6a. Runs BEFORE the forward phase so stalled
//     chains do not starve.
//
//  4. FORWARD PHASE: query candidate Concepts whose LATEST
//     ConceptVersion crosses (confidence >= ConfidenceThreshold
//     AND support_count >= SupportThreshold) AND has not yet
//     been promoted. For each candidate:
//
//     (a) Open per-Concept tx. SELECT 1 FROM concept FOR UPDATE
//     (cooperates with the Consolidator's existing row-level
//     lock on the same row).
//     Re-read MAX(version_index) under the lock. Re-verify
//     threshold (a competing Promoter replica or a fresh
//     Consolidator ConceptVersion may have made the candidate
//     stale).
//     INSERT concept_version(producer='promoter', promoted=true,
//     version_index=max+1, ...) RETURNING concept_version_id.
//     Commit tx.
//
//     (b) Open tx2 (separate transaction so PostgreSQL's
//     `now()` returns a strictly-later wall-clock timestamp
//     than tx1's). Mint qdrant_point_id. INSERT
//     embedding_publish row referencing concept_version_id.
//     INSERT embedding_publish_event(event_kind='queued',
//     attempt_index=0). Commit.
//
//     (c) Run runAttempt (steps 4-7): embed → upsert →
//     vector_written → confirm → published. Cancellation
//     during embed/upsert/confirm leaves the chain in its
//     last durable state; the next tick's retry phase picks
//     it up.
//
//  5. RELEASE the advisory lock (Conn.Close auto-releases; we
//     ALSO call pg_advisory_unlock explicitly so a long-lived
//     test conn pool cannot leak the lock between ticks).
//
//  6. UPDATE the run row to status='done' with
//     concepts_promoted=<count of chains that reached
//     'published' on this tick>.
//
// On any error in steps 2+, the run row is finalised as
// 'failed' (best-effort) so the lifecycle gate is never left
// dangling.
//
// DEADLOCK PREVENTION
// -------------------
// Step 6's UPDATE goes through the pool (s.db.ExecContext),
// NOT through the pinned conn from step 2. The pinned conn is
// CLOSED before step 6 executes.
func (s *Service) Tick(ctx context.Context) (TickResult, error) {
	s.metrics.IncRuns()

	tickCtx, cancel := context.WithTimeout(ctx, s.cfg.TickTimeout)
	defer cancel()

	result := TickResult{}

	// Step 1: open the run row in its own transaction.
	runID, err := s.openRun(tickCtx)
	if err != nil {
		s.metrics.IncErrors()
		return result, fmt.Errorf("promoter: open run: %w", err)
	}
	result.RunID = runID

	// Defer the "failed" finalisation so any error path closes
	// the run row. Flipped to a no-op once we reach the 'done'
	// finalisation below. Uses Background() with a bounded
	// timeout so a caller-cancelled ctx still gets the
	// cleanup write.
	finalised := false
	defer func() {
		if finalised {
			return
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if ferr := s.finalizeRun(closeCtx, runID, 0, StatusFailed); ferr != nil {
			s.logger.Warn("promoter.finalize_failed",
				slog.String("run_id", runID),
				slog.String("error", ferr.Error()))
		}
	}()

	// Steps 2-5 inside a closure so the pinned conn is
	// released BEFORE step 6's finalize runs.
	if err := s.runEmissionPhase(tickCtx, runID, &result); err != nil {
		s.metrics.IncErrors()
		return result, fmt.Errorf("promoter: emission: %w", err)
	}

	// Step 6: finalise the run row.
	finalStatus := StatusDone
	promoted := result.ConceptsPromoted
	if result.LockSkipped {
		finalStatus = StatusLockSkipped
		promoted = 0
	}
	if err := s.finalizeRun(tickCtx, runID, int(promoted), finalStatus); err != nil {
		s.metrics.IncErrors()
		return result, fmt.Errorf("promoter: finalize %s: %w", finalStatus, err)
	}
	finalised = true

	s.logger.Info("promoter.tick.done",
		slog.String("run_id", runID),
		slog.Bool("lock_skipped", result.LockSkipped),
		slog.Uint64("candidates_pending", result.CandidatesPending),
		slog.Uint64("candidates_evaluated", result.CandidatesEvaluated),
		slog.Uint64("retries_attempted", result.RetriesAttempted),
		slog.Uint64("retries_abandoned", result.RetriesAbandoned),
		slog.Uint64("orphans_pending", result.OrphansPending),
		slog.Uint64("orphans_recovered", result.OrphansRecovered),
		slog.Uint64("concepts_promoted", result.ConceptsPromoted),
		slog.Uint64("publish_failures", result.PublishFailures))

	return result, nil
}

// runEmissionPhase pins one DB connection, acquires the
// advisory lock, runs the retry + forward phases, then
// releases everything. Splitting this out of Tick is what
// guarantees the pinned conn is returned to the pool BEFORE
// Tick's step-6 finalise runs -- mandatory for correctness
// under a small pool.
func (s *Service) runEmissionPhase(ctx context.Context, runID string, result *TickResult) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var locked bool
	if err := conn.QueryRowContext(ctx,
		`SELECT pg_try_advisory_lock($1)`, s.cfg.AdvisoryLockKey,
	).Scan(&locked); err != nil {
		return fmt.Errorf("try advisory lock: %w", err)
	}
	if !locked {
		s.logger.Info("promoter.tick.lock_skipped",
			slog.String("run_id", runID),
			slog.Int64("lock_key", s.cfg.AdvisoryLockKey))
		result.LockSkipped = true
		s.metrics.IncLockSkipped()
		return nil
	}
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer unlockCancel()
		if _, uerr := conn.ExecContext(unlockCtx,
			`SELECT pg_advisory_unlock($1)`, s.cfg.AdvisoryLockKey,
		); uerr != nil {
			s.logger.Warn("promoter.advisory_unlock_failed",
				slog.String("error", uerr.Error()))
		}
	}()

	// Orphan-recovery phase FIRST. An orphan is a
	// ConceptVersion(promoted=true, producer='promoter')
	// whose §8.7.1 sibling embedding_publish row never
	// committed (tx2 failure after tx1 committed in a prior
	// tick). selectCandidates excludes it (the NOT EXISTS
	// promoted=true filter), AND selectStalled excludes it
	// (the LATERAL join keys off embedding_publish rows
	// that DO exist) — so without this phase the orphan is
	// durably skipped FOREVER. We re-attempt tx2 +
	// runAttempt to drive the existing CV to `published`.
	// Evaluator-2 finding #1.
	if err := s.processOrphans(ctx, conn, runID, result); err != nil {
		return fmt.Errorf("orphan phase: %w", err)
	}

	// Retry phase next so stalled chains do not starve. All
	// per-tick work runs on the pinned conn so it inherits the
	// session-scoped advisory lock (operations on `s.db` would
	// borrow a DIFFERENT pool conn that does NOT hold the
	// lock — a correctness hole AND a deadlock under small
	// pools).
	if err := s.processRetries(ctx, conn, runID, result); err != nil {
		return fmt.Errorf("retry phase: %w", err)
	}

	// Forward phase.
	if err := s.processCandidates(ctx, conn, runID, result); err != nil {
		return fmt.Errorf("forward phase: %w", err)
	}

	return nil
}

// Run executes the poll loop. Runs Tick once immediately so a
// fresh deploy does not have to wait a full interval before
// the first sweep, then on the K-minute long-poll ticker.
// Per-tick errors are logged at Warn but do NOT stop the loop.
// The loop exits only on ctx cancellation.
func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("promoter.run.start",
		slog.Float64("confidence_threshold", s.cfg.ConfidenceThreshold),
		slog.Int("support_threshold", s.cfg.SupportThreshold),
		slog.Duration("run_interval", s.cfg.RunInterval),
		slog.Duration("tick_timeout", s.cfg.TickTimeout),
		slog.Int("candidate_batch_size", s.cfg.CandidateBatchSize),
		slog.Int("retry_batch_size", s.cfg.RetryBatchSize),
		slog.Int("max_retry_attempts", s.cfg.MaxRetryAttempts),
		slog.Int64("advisory_lock_key", s.cfg.AdvisoryLockKey))

	if _, err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("promoter.run.initial_tick_failed",
			slog.String("error", err.Error()))
	}

	ticker := time.NewTicker(s.cfg.RunInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("promoter.run.shutdown",
				slog.String("reason", ctx.Err().Error()))
			return ctx.Err()
		case <-ticker.C:
			if _, err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("promoter.run.tick_failed",
					slog.String("error", err.Error()))
			}
		}
	}
}

// openRun INSERTs a fresh promoter_run row in its own
// transaction and returns its run_id. status is explicitly set
// to StatusRunning so an operator inspecting `promoter_run`
// while a tick is in flight sees the expected lifecycle phase.
func (s *Service) openRun(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO promoter_run (started_at, status)
		VALUES (now(), $1)
		RETURNING run_id::text
	`, StatusRunning).Scan(&id)
	return id, err
}

// finalizeRun UPDATEs the promoter_run row to its terminal
// shape. status MUST be one of {StatusDone, StatusLockSkipped,
// StatusFailed} per the lifecycle contract.
func (s *Service) finalizeRun(ctx context.Context, runID string, conceptsPromoted int, status string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE promoter_run
		   SET finished_at       = now(),
		       concepts_promoted = $1,
		       status            = $2
		 WHERE run_id = $3::uuid
	`, conceptsPromoted, status, runID)
	return err
}

// ────────────────────────────────────────────────────────────
// Forward phase: fresh candidates → promote
// ────────────────────────────────────────────────────────────

// candidate is a row from the threshold-crossing query: the
// Concept whose latest ConceptVersion is past the threshold and
// has NOT already been promoted.
type candidate struct {
	conceptID         string
	name              string
	descriptionMD     string
	fingerprint       []byte
	latestVersionIdx  int
	latestConfidence  float64
	latestSupport     int
	latestNegative    int
}

// processCandidates runs the forward phase: scan candidates,
// for each one do tx1 (CV insert) + tx2 (EmbeddingPublish
// insert) + runAttempt (publish chain). Cancellation during
// the loop returns immediately and the surviving rows are
// picked up by the next tick's retry phase.
//
// `conn` is the advisory-lock-holding pinned connection;
// every DB call inside this method MUST go through `conn`
// (not `s.db`) so the work inherits the lock's mutual
// exclusion.
func (s *Service) processCandidates(ctx context.Context, conn *sql.Conn, runID string, result *TickResult) error {
	candidates, err := s.selectCandidates(ctx, conn)
	if err != nil {
		return fmt.Errorf("select candidates: %w", err)
	}
	result.CandidatesPending = uint64(len(candidates))
	s.metrics.SetCandidatesPending(int64(len(candidates)))

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		promoted, evaluated, failures, err := s.promoteOne(ctx, conn, runID, c)
		if err != nil {
			// Per-concept failure is logged but does NOT
			// abort the whole tick — other candidates may
			// still succeed, and the failed candidate is
			// picked up by the next tick's retry phase
			// (if it reached the EmbeddingPublish insert)
			// or the next tick's forward phase (if it
			// did not).
			s.logger.Warn("promoter.candidate_failed",
				slog.String("run_id", runID),
				slog.String("concept_id", c.conceptID),
				slog.String("error", err.Error()))
			continue
		}
		if evaluated {
			result.CandidatesEvaluated++
			s.metrics.AddCandidatesEvaluated(1)
		}
		if promoted {
			result.ConceptsPromoted++
			s.metrics.AddConceptsPromoted(1)
		}
		if failures > 0 {
			result.PublishFailures += uint64(failures)
			s.metrics.AddPublishFailures(uint64(failures))
		}
	}

	return nil
}

// selectCandidates returns every Concept whose latest
// ConceptVersion crosses the threshold AND has NOT been
// promoted yet (no ConceptVersion with promoted=true exists
// for the concept). Capped at CandidateBatchSize so a single
// tick cannot stall on an avalanche of fresh promotions.
//
// The query uses DISTINCT ON to take the latest ConceptVersion
// per concept, then filters on threshold AND not-promoted. The
// recheck inside promoteOne handles the race window between
// this scan and the per-Concept transaction.
func (s *Service) selectCandidates(ctx context.Context, conn *sql.Conn) ([]candidate, error) {
	const q = `
		WITH latest AS (
			SELECT DISTINCT ON (cv.concept_id)
			       cv.concept_id,
			       cv.version_index,
			       cv.confidence,
			       cv.support_count,
			       cv.negative_count
			  FROM concept_version cv
			 ORDER BY cv.concept_id, cv.version_index DESC
		)
		SELECT c.concept_id::text,
		       c.name,
		       c.description_md,
		       c.fingerprint,
		       latest.version_index,
		       latest.confidence,
		       latest.support_count,
		       latest.negative_count
		  FROM latest
		  JOIN concept c ON c.concept_id = latest.concept_id
		 WHERE latest.confidence    >= $1
		   AND latest.support_count >= $2
		   AND NOT EXISTS (
		       SELECT 1 FROM concept_version cv2
		        WHERE cv2.concept_id = c.concept_id
		          AND cv2.promoted   = true
		   )
		 ORDER BY latest.confidence DESC, latest.support_count DESC
		 LIMIT $3
	`
	rows, err := conn.QueryContext(ctx, q,
		s.cfg.ConfidenceThreshold, s.cfg.SupportThreshold, s.cfg.CandidateBatchSize,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.conceptID, &c.name, &c.descriptionMD, &c.fingerprint,
			&c.latestVersionIdx, &c.latestConfidence, &c.latestSupport, &c.latestNegative); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// promoteOne implements the §8.7.1 lines 818-833 write
// protocol for a single Concept candidate. Returns
// (promoted=true, evaluated=true, failures, nil) on the happy
// path. The 3-way return separates "the recheck dropped the
// candidate so we did nothing" (evaluated=false) from "we
// emitted a ConceptVersion AND a published vector"
// (promoted=true).
//
// The two-transaction split is load-bearing for the §8.7.1
// strict-ordering invariant: ConceptVersion.created_at MUST
// be STRICTLY EARLIER than the associated
// EmbeddingPublish.created_at. PostgreSQL's `now()` returns
// the transaction start time, so two distinct transactions
// guarantee two distinct timestamps (microsecond precision; a
// tx commit + new tx begin takes well over 1µs in practice).
//
// CANCELLATION HANDLING — if ctx is cancelled between tx1
// commit and tx2 commit, we have an orphan promoted
// ConceptVersion with no EmbeddingPublish row. The next
// tick's forward phase will see this Concept already
// promoted (the NOT EXISTS filter) and skip it. The
// orphan-recovery phase (Service.processOrphans, called
// from runEmissionPhase BEFORE retries + forward) picks up
// these rows on the next tick, runs tx2 + the publish
// chain, and converts them into a published vector.
// Evaluator-2 finding #1 fix: prior behaviour left orphans
// durably skipped because selectCandidates filters them on
// the NOT EXISTS(promoted=true) check AND selectStalled
// keys off existing embedding_publish rows.
func (s *Service) promoteOne(ctx context.Context, conn *sql.Conn, runID string, c candidate) (promoted bool, evaluated bool, failures int, err error) {
	// Pre-tx1 model-resolution warm-up. In pinned mode this
	// is a no-op; in unpinned HTTP mode the FIRST candidate's
	// content seeds the embedder's model_version cache so
	// tx2 below has a non-empty modelVersion to write.
	//
	// Evaluator-3 finding #1 fix: warming BEFORE tx1 (per
	// rubber-duck blocker #2) means a transient embedder
	// outage during unpinned bootstrap does NOT create a
	// promoted-ConceptVersion orphan — the candidate stays
	// in the pool and gets re-evaluated on the next tick.
	content := buildConceptContent(c.name, c.descriptionMD)
	prefetched, modelVersion, mvErr := s.ensureModelReady(ctx, content)
	if mvErr != nil {
		if errors.Is(mvErr, context.Canceled) || errors.Is(mvErr, context.DeadlineExceeded) {
			return false, false, 0, nil
		}
		s.logger.Warn("promoter.model_resolution_failed",
			slog.String("run_id", runID),
			slog.String("concept_id", c.conceptID),
			slog.String("phase", "fresh"),
			slog.String("error", mvErr.Error()))
		return false, false, 0, fmt.Errorf("ensure model ready: %w", mvErr)
	}

	// tx1: re-check + INSERT ConceptVersion.
	cvID, ok, err := s.insertConceptVersion(ctx, conn, runID, c)
	if err != nil {
		return false, false, 0, fmt.Errorf("insert concept_version: %w", err)
	}
	if !ok {
		// Recheck dropped the candidate (already promoted by
		// a sibling replica, or the latest version_index
		// moved past our snapshot). Not an error.
		return false, false, 0, nil
	}

	// tx2: INSERT embedding_publish + queued event using the
	// modelVersion the helper resolved above.
	publishID, pointID, err := s.insertPublishAndQueued(ctx, conn, cvID, c, modelVersion)
	if err != nil {
		// The ConceptVersion was committed in tx1 but the
		// publish row failed. Without orphan recovery this
		// would be invisible to BOTH selectStalled (no
		// embedding_publish row exists yet) AND
		// selectCandidates (the NOT EXISTS promoted=true
		// filter). The orphan-recovery phase (registered
		// in runEmissionPhase BEFORE retries + forward,
		// see Service.processOrphans + Service.selectOrphans)
		// scans for these rows and re-attempts tx2 +
		// runAttempt on the next tick. Log the orphan loudly
		// so an operator can see the failure mode even when
		// recovery succeeds.
		s.logger.Warn("promoter.orphan_concept_version",
			slog.String("run_id", runID),
			slog.String("concept_id", c.conceptID),
			slog.String("concept_version_id", cvID),
			slog.String("error", err.Error()),
			slog.String("recovery", "auto: next tick's processOrphans"))
		return false, true, 0, fmt.Errorf("insert embedding_publish: %w", err)
	}

	// runAttempt: embed → upsert → vector_written → confirm
	// → published.
	state := publishState{
		publishID:     publishID,
		pointID:       pointID,
		modelVersion:  modelVersion,
		attemptIndex:  0,
		mode:          publishModeFresh,
		conceptID:     c.conceptID,
		versionID:     cvID,
		fingerprint:   c.fingerprint,
		prefetchedVec: prefetched,
	}
	lastEvent, runErr := s.runAttempt(ctx, conn, state, content)
	if runErr != nil {
		// A non-nil error from runAttempt means EITHER
		// cancellation (no event recorded, retry will pick
		// up) OR a recordable §9.6a failure (the 'failed'
		// event was already inserted) OR a non-recordable
		// DB outage (also accounted-for above as failures).
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return false, true, 0, nil
		}
		if lastEvent == embedding.EventKindFailed {
			return false, true, 1, nil
		}
		// Non-recordable: surface error but still log
		// candidate as evaluated.
		return false, true, 0, runErr
	}
	return true, true, 0, nil
}

// insertConceptVersion is the tx1 step: row-lock the concept
// (cooperating with the Consolidator's existing
// `SELECT ... FROM concept WHERE concept_id = ... FOR UPDATE`
// lock), re-read MAX(version_index), re-verify threshold,
// then INSERT the promoted ConceptVersion.
//
// Returns (cvID, true, nil) on success. Returns ("", false,
// nil) when the recheck drops the candidate (no error — a
// concurrent writer raced us and won). Returns ("", false,
// err) on any genuine error path.
//
// Unique-violation errors on (concept_id, version_index) are
// treated as recheck-drops (a sibling replica beat us to the
// version bump). The DB's unique index is the backstop; this
// app-level handling is the soft path.
func (s *Service) insertConceptVersion(ctx context.Context, conn *sql.Conn, runID string, c candidate) (string, bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin tx1: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Row-lock the concept. Cooperates with the
	// Consolidator's FOR UPDATE on the same row so the
	// version_index bump below is serialised against any
	// concurrent Consolidator insert.
	var found int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM concept WHERE concept_id = $1::uuid FOR UPDATE`, c.conceptID,
	).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Concept was deleted between scan and lock.
			// Soft-drop the candidate.
			return "", false, nil
		}
		return "", false, fmt.Errorf("lock concept: %w", err)
	}

	// Re-read latest version under the lock.
	var latestIdx int
	var latestConf float64
	var latestSupport int
	var latestNeg int
	var alreadyPromoted bool
	if err := tx.QueryRowContext(ctx, `
		SELECT cv.version_index,
		       cv.confidence,
		       cv.support_count,
		       cv.negative_count,
		       EXISTS (SELECT 1 FROM concept_version cv2
		                WHERE cv2.concept_id = $1::uuid
		                  AND cv2.promoted   = true) AS already_promoted
		  FROM concept_version cv
		 WHERE cv.concept_id = $1::uuid
		 ORDER BY cv.version_index DESC
		 LIMIT 1
	`, c.conceptID).Scan(&latestIdx, &latestConf, &latestSupport, &latestNeg, &alreadyPromoted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Concept exists but has no versions — should
			// be unreachable (the scan found a version),
			// but defensively soft-drop.
			return "", false, nil
		}
		return "", false, fmt.Errorf("recheck latest version: %w", err)
	}

	if alreadyPromoted {
		// A sibling Promoter replica beat us. Soft-drop.
		return "", false, nil
	}
	if latestConf < s.cfg.ConfidenceThreshold || latestSupport < s.cfg.SupportThreshold {
		// A fresh Consolidator ConceptVersion landed
		// between the scan and the lock, and it moved the
		// counters back under the threshold. Soft-drop;
		// the next tick will reconsider once the
		// Consolidator's append-only stream advances the
		// counters past the threshold again.
		return "", false, nil
	}

	// version_index = latest + 1 under the lock.
	nextIndex := latestIdx + 1
	band := bandOf(latestConf)

	var cvID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO concept_version
		    (concept_id, version_index, confidence, confidence_band,
		     support_count, negative_count, producer, producer_run_id,
		     promoted, created_at)
		VALUES ($1::uuid, $2, $3, $4::concept_band,
		        $5, $6, 'promoter'::producer, $7::uuid,
		        true, clock_timestamp())
		RETURNING concept_version_id::text
	`, c.conceptID, nextIndex, latestConf, band,
		latestSupport, latestNeg, runID).Scan(&cvID); err != nil {
		// Unique-violation on (concept_id, version_index)
		// is the race-loss case: a concurrent writer
		// inserted version=nextIndex between our
		// MAX(version_index) read and our INSERT, despite
		// the row-lock. (Possible if the competing writer
		// did NOT take the same row-lock — e.g. a future
		// non-cooperating writer.) Treat as soft-drop.
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && string(pqErr.Code) == pgUniqueViolationCode {
			s.logger.Info("promoter.version_index_race",
				slog.String("run_id", runID),
				slog.String("concept_id", c.conceptID),
				slog.Int("next_index", nextIndex))
			return "", false, nil
		}
		return "", false, fmt.Errorf("insert concept_version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit tx1: %w", err)
	}
	return cvID, true, nil
}

// insertPublishAndQueued is the tx2 step: INSERT
// embedding_publish + queued event in a SEPARATE transaction
// from tx1 so PostgreSQL's `now()` returns a strictly-later
// wall-clock value than the ConceptVersion.created_at.
// The caller MUST pass a non-empty modelVersion (obtained via
// s.ensureModelReady before calling this — that helper warms
// the embedder cache for unpinned HTTP mode).
// Returns (publishID, pointID, nil) on success.
//
// Evaluator-3 finding #1 fix: modelVersion used to be read
// inline via s.embedder.ModelVersion(), which returned empty
// in unpinned HTTP mode before Embed had been called — every
// fresh/orphan publish failed before the cache could warm.
// The helper-then-pass pattern eliminates that deadlock.
func (s *Service) insertPublishAndQueued(ctx context.Context, conn *sql.Conn, cvID string, c candidate, modelVersion string) (string, string, error) {
	if strings.TrimSpace(modelVersion) == "" {
		return "", "", errors.New(
			"promoter: insertPublishAndQueued: empty modelVersion; " +
				"caller must call ensureModelReady before tx2 " +
				"(risk §9.6 requires a non-empty version per publish)")
	}

	pointID, err := s.newUUID()
	if err != nil {
		return "", "", fmt.Errorf("mint point_id: %w", err)
	}

	queuedDetails, err := marshalQueuedDetails(c, cvID, modelVersion)
	if err != nil {
		return "", "", err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("begin tx2: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var publishID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO embedding_publish
		    (concept_version_id, embedding_model_version, qdrant_point_id)
		VALUES ($1::uuid, $2, $3::uuid)
		RETURNING publish_id::text
	`, cvID, modelVersion, pointID).Scan(&publishID); err != nil {
		return "", "", fmt.Errorf("insert embedding_publish: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO embedding_publish_event
		    (publish_id, event_kind, attempt_index, details_json)
		VALUES ($1::uuid, $2::embedding_publish_event_kind, $3, $4::jsonb)
	`, publishID, embedding.EventKindQueued, 0, string(queuedDetails)); err != nil {
		return "", "", fmt.Errorf("insert queued event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("commit tx2: %w", err)
	}
	return publishID, pointID, nil
}

// ensureModelReady guarantees s.embedder.ModelVersion() is
// non-empty before tx2 INSERTs the embedding_publish row. The
// HTTP embedder operates in two modes:
//
//   - PINNED — operator set AGENT_MEMORY_EMBEDDER_MODEL_VERSION.
//     ModelVersion() returns the pinned value from
//     construction time; never empty. ensureModelReady returns
//     (nil, pinnedVersion, nil) immediately.
//
//   - UNPINNED — operator left AGENT_MEMORY_EMBEDDER_MODEL_VERSION
//     unset; the FIRST successful Embed() response's
//     `model_version` field is cached and re-returned by every
//     subsequent ModelVersion() call. Until the first Embed
//     succeeds, ModelVersion() returns "" and tx2 cannot
//     proceed (the embedding_publish row's
//     embedding_model_version column is NOT NULL and the
//     §9.6a contract requires a non-empty version per
//     publish).
//
// In UNPINNED mode with an empty cache, ensureModelReady
// performs a single bootstrap Embed(content) call. On success,
// it re-reads ModelVersion(); if STILL empty, the embedder is
// misbehaving (returned 2xx without populating
// model_version) and we surface a typed error.
//
// The returned vector — when non-nil — is the bootstrap
// embedding for `content`. The caller MUST thread it through
// to runAttempt via publishState.prefetchedVec so the
// upstream Embed call is NOT duplicated for the same content.
//
// Evaluator-3 finding #1 fix. Prior to this helper:
//   - promoteOne's tx2 path was dead in unpinned HTTP mode.
//   - processOrphans's tx2 path was equally dead — every
//     orphan recovery attempt failed identically, so the
//     promoter could NEVER bootstrap unpinned mode.
//
// In PINNED mode this helper is a one-line no-op and changes
// none of the existing test expectations.
func (s *Service) ensureModelReady(ctx context.Context, content string) ([]float32, string, error) {
	if mv := strings.TrimSpace(s.embedder.ModelVersion()); mv != "" {
		return nil, mv, nil
	}
	vec, err := s.embedder.Embed(ctx, content)
	if err != nil {
		return nil, "", fmt.Errorf("model-resolution embed: %w", err)
	}
	mv := strings.TrimSpace(s.embedder.ModelVersion())
	if mv == "" {
		return nil, "", errors.New(
			"promoter: embedder returned 2xx but ModelVersion() is still empty " +
				"after the bootstrap embed; check upstream model_version field " +
				"(unpinned HTTP mode requires the response to populate it)")
	}
	return vec, mv, nil
}

// ────────────────────────────────────────────────────────────
// Orphan-recovery phase: tx1-committed CVs with no tx2
// (evaluator-2 finding #1)
// ────────────────────────────────────────────────────────────

// orphan is a row from the orphan-recovery scan: a
// ConceptVersion that committed in promoteOne's tx1
// (promoted=true, producer='promoter') but whose §8.7.1
// sibling embedding_publish row never committed (tx2 failure
// after tx1 in a prior tick). Without this phase the row is
// invisible to BOTH selectStalled (no embedding_publish to
// scan) AND selectCandidates (the NOT EXISTS promoted=true
// filter), so the promoted ConceptVersion is durably skipped
// forever — a silent §8.7.1 violation (an evaluable
// promotion never reaches Qdrant) AND a permanent recall
// gap.
type orphan struct {
	conceptVersionID string
	conceptID        string
	name             string
	descriptionMD    string
	fingerprint      []byte
}

// processOrphans scans for orphaned promoted ConceptVersion
// rows and drives each through tx2 (insertPublishAndQueued)
// + runAttempt. Runs FIRST in runEmissionPhase so a fresh
// orphan generated by an in-flight tick's cancellation is
// recoverable on the very next tick (no extra cooldown).
//
// `conn` is the advisory-lock-holding pinned connection;
// every DB call inside this method MUST go through `conn`
// (not `s.db`) so the work inherits the lock's mutual
// exclusion. Cancellation in the per-orphan loop returns
// immediately and the surviving rows are picked up on the
// next tick.
//
// Bounded by RetryBatchSize (semantically these are stalled
// chains in the same family as the retry phase, just with
// no publish row to retry yet) so a sudden flood of orphans
// from a DB outage cannot stall a single tick on an
// avalanche of recovery work.
func (s *Service) processOrphans(ctx context.Context, conn *sql.Conn, runID string, result *TickResult) error {
	orphans, err := s.selectOrphans(ctx, conn)
	if err != nil {
		return fmt.Errorf("select orphans: %w", err)
	}
	result.OrphansPending = uint64(len(orphans))
	s.metrics.SetOrphansPending(int64(len(orphans)))

	for _, o := range orphans {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Reuse insertPublishAndQueued via a synthetic
		// candidate so the publish-row shape (queued
		// event details_json, model version handling)
		// stays IDENTICAL to the fresh-promotion path.
		// The synthetic candidate's confidence/support
		// counters are not consulted by tx2 (only the
		// concept identifiers + name + description_md +
		// fingerprint flow into queuedEventDetails).
		synth := candidate{
			conceptID:     o.conceptID,
			name:          o.name,
			descriptionMD: o.descriptionMD,
			fingerprint:   o.fingerprint,
		}
		// Pre-tx2 model-resolution warm-up. In pinned mode
		// this is a no-op; in unpinned HTTP mode the orphan's
		// content seeds the embedder's model_version cache so
		// insertPublishAndQueued below has a non-empty
		// modelVersion to write.
		//
		// Evaluator-3 finding #1 fix: this is the
		// orphan-recovery counterpart to promoteOne's
		// pre-tx1 warm-up. Without it, unpinned HTTP mode
		// would orphan a CV in promoteOne (bootstrap embed
		// failure) AND then perma-fail to recover it because
		// processOrphans's own tx2 call would ALSO see an
		// empty ModelVersion(). Deadlock.
		content := buildConceptContent(o.name, o.descriptionMD)
		prefetched, modelVersion, mvErr := s.ensureModelReady(ctx, content)
		if mvErr != nil {
			if errors.Is(mvErr, context.Canceled) || errors.Is(mvErr, context.DeadlineExceeded) {
				return mvErr
			}
			s.logger.Warn("promoter.model_resolution_failed",
				slog.String("run_id", runID),
				slog.String("concept_id", o.conceptID),
				slog.String("concept_version_id", o.conceptVersionID),
				slog.String("phase", "orphan"),
				slog.String("error", mvErr.Error()))
			continue
		}
		publishID, pointID, perr := s.insertPublishAndQueued(ctx, conn, o.conceptVersionID, synth, modelVersion)
		if perr != nil {
			// tx2 failed again. Leave the orphan in
			// place; the next tick re-tries. Logged at
			// Warn so a persistent failure (e.g. schema
			// drift) is visible without spamming.
			s.logger.Warn("promoter.orphan_tx2_failed",
				slog.String("run_id", runID),
				slog.String("concept_id", o.conceptID),
				slog.String("concept_version_id", o.conceptVersionID),
				slog.String("error", perr.Error()))
			continue
		}

		state := publishState{
			publishID:     publishID,
			pointID:       pointID,
			modelVersion:  modelVersion,
			attemptIndex:  0,
			mode:          publishModeOrphan,
			conceptID:     o.conceptID,
			versionID:     o.conceptVersionID,
			fingerprint:   o.fingerprint,
			prefetchedVec: prefetched,
		}
		lastEvent, runErr := s.runAttempt(ctx, conn, state, content)
		if runErr != nil {
			if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
				// Cancellation: the publish row now
				// exists with a `queued` event; the
				// next tick's processRetries owns it
				// from here. Surface cancellation so
				// the lifecycle finalises cleanly.
				return runErr
			}
			if lastEvent == embedding.EventKindFailed {
				result.PublishFailures++
				s.metrics.AddPublishFailures(1)
				continue
			}
			s.logger.Warn("promoter.orphan_attempt_failed",
				slog.String("run_id", runID),
				slog.String("publish_id", publishID),
				slog.String("error", runErr.Error()))
			continue
		}
		result.OrphansRecovered++
		s.metrics.AddOrphansRecovered(1)
		// The orphan is now a fully-published vector;
		// count it toward ConceptsPromoted too so the
		// per-binary promoted_total counter reflects the
		// terminal outcome (regardless of which phase
		// finished the chain).
		result.ConceptsPromoted++
		s.metrics.AddConceptsPromoted(1)
		s.logger.Info("promoter.orphan_recovered",
			slog.String("run_id", runID),
			slog.String("concept_id", o.conceptID),
			slog.String("concept_version_id", o.conceptVersionID),
			slog.String("publish_id", publishID))
	}
	return nil
}

// selectOrphans returns every ConceptVersion that the
// promoter committed (promoted=true, producer='promoter')
// but for which NO embedding_publish row exists. Bound by
// RetryBatchSize. Ordered by created_at ASC so the oldest
// orphan recovers first (FIFO fairness across recoveries).
//
// The NOT EXISTS subquery filters on concept_version_id
// (the §8.7.1 sibling-row key) — a publish row that targets
// the SAME concept but a DIFFERENT version is not the
// sibling we are looking for; only the row whose
// concept_version_id matches counts.
func (s *Service) selectOrphans(ctx context.Context, conn *sql.Conn) ([]orphan, error) {
	const q = `
		SELECT cv.concept_version_id::text,
		       cv.concept_id::text,
		       c.name,
		       c.description_md,
		       c.fingerprint
		  FROM concept_version cv
		  JOIN concept         c ON c.concept_id = cv.concept_id
		 WHERE cv.promoted = true
		   AND cv.producer = 'promoter'::producer
		   AND NOT EXISTS (
		       SELECT 1 FROM embedding_publish ep
		        WHERE ep.concept_version_id = cv.concept_version_id
		   )
		 ORDER BY cv.created_at ASC
		 LIMIT $1
	`
	rows, err := conn.QueryContext(ctx, q, s.cfg.RetryBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.conceptVersionID, &o.conceptID,
			&o.name, &o.descriptionMD, &o.fingerprint); err != nil {
			return nil, fmt.Errorf("scan orphan: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ────────────────────────────────────────────────────────────
// Retry phase: stalled publishes → re-attempt
// ────────────────────────────────────────────────────────────

// stalled is a row from the retry-phase scan: an
// embedding_publish row whose target is a concept_version_id
// (NOT a node_id, those are the Repo Indexer's responsibility)
// and whose latest event is NOT 'published' or 'superseded'.
type stalled struct {
	publishID        string
	conceptVersionID string
	conceptID        string
	pointID          string
	publishModel     string
	name             string
	descriptionMD    string
	fingerprint      []byte
	latestEvent      string
	maxAttempt       int
}

// processRetries scans concept-targeted publish rows whose
// latest event is NOT 'published' or 'superseded' and
// re-attempts the publish chain. The retry MUST reuse the
// existing publish_id + qdrant_point_id (so the Qdrant upsert
// remains idempotent) and append a fresh `queued` event at
// attempt_index = max(prior)+1 BEFORE re-running steps 4-7.
//
// A model-version mismatch (operator bumped the embedder mid-
// flight) is logged and the row is left alone — the
// supersede flow owns model bumps.
func (s *Service) processRetries(ctx context.Context, conn *sql.Conn, runID string, result *TickResult) error {
	stalls, err := s.selectStalled(ctx, conn)
	if err != nil {
		return fmt.Errorf("select stalled: %w", err)
	}

	// Early return when there are no stalls. Avoids the
	// model-version check (and its potential warm-up) when
	// there is no retry work to do — important so the forward
	// phase can still bootstrap in unpinned HTTP mode on a
	// fresh schema with zero pre-existing publish rows.
	//
	// Evaluator-3 finding #1 fix (rubber-duck blocker #1):
	// previously this method always required ModelVersion()
	// to be non-empty, which would abort the tick in
	// unpinned mode before promoteOne ever ran.
	if len(stalls) == 0 {
		return nil
	}

	// Warm the embedder model cache off the first stall's
	// content. In pinned mode this is a no-op; in unpinned
	// HTTP mode this is the bootstrap point if we got here
	// before promoteOne/processOrphans warmed the cache
	// (e.g. an orphans-empty + stalls-non-empty tick where
	// the publish_event rows survived a prior process
	// restart). The returned vec is discarded — runAttempt
	// re-embeds every stall under its own content for the
	// fresh upsert, and accepting an asymmetric "prefetch
	// for first stall only" optimisation would add plumbing
	// complexity for at most one saved Embed call per warm-
	// up event.
	warmContent := buildConceptContent(stalls[0].name, stalls[0].descriptionMD)
	if _, _, mvErr := s.ensureModelReady(ctx, warmContent); mvErr != nil {
		if errors.Is(mvErr, context.Canceled) || errors.Is(mvErr, context.DeadlineExceeded) {
			return mvErr
		}
		s.logger.Warn("promoter.model_resolution_failed",
			slog.String("run_id", runID),
			slog.String("phase", "retry"),
			slog.String("error", mvErr.Error()))
		return fmt.Errorf("retry: ensure model ready: %w", mvErr)
	}
	currentModel := strings.TrimSpace(s.embedder.ModelVersion())
	if currentModel == "" {
		return errors.New("promoter: Embedder.ModelVersion() returned empty in retry phase even after warm-up")
	}

	for _, st := range stalls {
		if err := ctx.Err(); err != nil {
			return err
		}

		if st.publishModel != currentModel {
			// Model bumped between the original publish
			// and this retry attempt. The supersede flow
			// owns this transition; we MUST NOT retry
			// under a different model.
			s.logger.Warn("promoter.retry_model_mismatch",
				slog.String("run_id", runID),
				slog.String("publish_id", st.publishID),
				slog.String("publish_model", st.publishModel),
				slog.String("current_model", currentModel))
			continue
		}

		// Per-publish retry-budget gate. Without this cap a
		// permanently-failing row (corrupt fingerprint payload,
		// Qdrant schema mismatch, embedding that consistently
		// fails confirm, etc.) would be re-queued every tick
		// forever — burning an embedder API call + Qdrant
		// upsert + DB writes per tick AND crowding healthy
		// retries out of the RetryBatchSize budget. The check
		// runs BEFORE the RetriesAttempted increment and the
		// `queued` event insert so an abandoned row produces
		// zero side-effects beyond the structured Error log +
		// the in-memory TickResult.RetriesAbandoned counter.
		// The row itself is left untouched (its latest
		// event_kind stays where it was), so a future manual
		// recovery can re-drive the chain once the underlying
		// defect is fixed.
		if st.maxAttempt >= s.cfg.MaxRetryAttempts {
			result.RetriesAbandoned++
			s.logger.Error("promoter.retry_abandoned",
				slog.String("run_id", runID),
				slog.String("publish_id", st.publishID),
				slog.String("concept_id", st.conceptID),
				slog.String("concept_version_id", st.conceptVersionID),
				slog.String("latest_event", st.latestEvent),
				slog.Int("max_attempt", st.maxAttempt),
				slog.Int("max_retry_attempts", s.cfg.MaxRetryAttempts))
			continue
		}

		result.RetriesAttempted++
		s.metrics.AddRetriesAttempted(1)

		nextAttempt := st.maxAttempt + 1
		details, err := marshalRetryQueuedDetails(st, currentModel)
		if err != nil {
			s.logger.Warn("promoter.retry_marshal_failed",
				slog.String("publish_id", st.publishID),
				slog.String("error", err.Error()))
			continue
		}
		if err := s.insertEvent(ctx, conn, st.publishID, embedding.EventKindQueued,
			nextAttempt, details); err != nil {
			s.logger.Warn("promoter.retry_insert_queued_failed",
				slog.String("publish_id", st.publishID),
				slog.String("error", err.Error()))
			continue
		}

		state := publishState{
			publishID:    st.publishID,
			pointID:      st.pointID,
			modelVersion: currentModel,
			attemptIndex: nextAttempt,
			mode:         publishModeRetry,
			conceptID:    st.conceptID,
			versionID:    st.conceptVersionID,
			fingerprint:  st.fingerprint,
		}
		content := buildConceptContent(st.name, st.descriptionMD)
		lastEvent, runErr := s.runAttempt(ctx, conn, state, content)
		if runErr != nil {
			if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
				return runErr
			}
			if lastEvent == embedding.EventKindFailed {
				result.PublishFailures++
				s.metrics.AddPublishFailures(1)
				continue
			}
			s.logger.Warn("promoter.retry_failed",
				slog.String("publish_id", st.publishID),
				slog.String("error", runErr.Error()))
			continue
		}
		result.ConceptsPromoted++
		s.metrics.AddConceptsPromoted(1)
	}
	return nil
}

// selectStalled returns every concept-targeted publish row
// whose latest event_kind is NOT 'published' or 'superseded'.
// The lateral subquery picks the latest event per publish_id
// via the (publish_id, created_at DESC) index from
// migration 0015.
//
// Bounded by RetryBatchSize so a tick cannot stall on a huge
// backlog. Stalled rows past the batch are picked up on
// subsequent ticks.
//
// Includes the originating Concept's name + description_md +
// fingerprint so runAttempt can re-build the embedding
// content without a second round-trip.
func (s *Service) selectStalled(ctx context.Context, conn *sql.Conn) ([]stalled, error) {
	const q = `
		SELECT ep.publish_id::text,
		       ep.concept_version_id::text,
		       cv.concept_id::text,
		       ep.qdrant_point_id::text,
		       ep.embedding_model_version,
		       c.name,
		       c.description_md,
		       c.fingerprint,
		       latest.event_kind::text,
		       coalesce(latest.max_attempt, 0)
		  FROM embedding_publish ep
		  JOIN concept_version  cv ON cv.concept_version_id = ep.concept_version_id
		  JOIN concept          c  ON c.concept_id          = cv.concept_id
		  CROSS JOIN LATERAL (
		      SELECT epe.event_kind,
		             (SELECT max(attempt_index) FROM embedding_publish_event
		               WHERE publish_id = ep.publish_id) AS max_attempt
		        FROM embedding_publish_event epe
		       WHERE epe.publish_id = ep.publish_id
		       -- evaluator-2 finding #2 fix: the §9.6a canonical
		       -- "latest event" tie-break is (created_at DESC,
		       -- event_id DESC) -- see internal/promoter/doc.go:95
		       -- and the mirror queries in
		       -- internal/embedding/flusher.go:656 +
		       -- internal/embedding/publish_event_resolver.go:117.
		       -- Without the event_id tie-break, two events that
		       -- share a timestamp (cheap to create when a single
		       -- tick inserts vector_written + published
		       -- back-to-back) could be selected non-deterministically,
		       -- letting a 'vector_written' row look "latest" when
		       -- 'published' was actually appended at the same
		       -- microsecond -- which would re-queue an
		       -- already-finished chain.
		       ORDER BY epe.created_at DESC, epe.event_id DESC
		       LIMIT 1
		  ) AS latest
		 WHERE ep.concept_version_id IS NOT NULL
		   AND cv.promoted            = true
		   AND latest.event_kind NOT IN ('published', 'superseded')
		 ORDER BY ep.created_at ASC
		 LIMIT $1
	`
	rows, err := conn.QueryContext(ctx, q, s.cfg.RetryBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []stalled
	for rows.Next() {
		var st stalled
		if err := rows.Scan(&st.publishID, &st.conceptVersionID, &st.conceptID,
			&st.pointID, &st.publishModel, &st.name, &st.descriptionMD,
			&st.fingerprint, &st.latestEvent, &st.maxAttempt); err != nil {
			return nil, fmt.Errorf("scan stalled: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ────────────────────────────────────────────────────────────
// runAttempt — shared steps 4-7 of §9.6a for Concept publishes
// ────────────────────────────────────────────────────────────

// publishState carries the per-attempt state the publish chain
// needs across the embed → upsert → vector_written → confirm →
// published transitions.
type publishState struct {
	publishID    string
	pointID      string
	modelVersion string
	attemptIndex int
	mode         string

	// Payload provenance (for the Qdrant payload + retry
	// snapshot details_json).
	conceptID   string
	versionID   string
	fingerprint []byte

	// prefetchedVec is a pre-computed embedding the caller
	// already obtained (typically via ensureModelReady when
	// the embedder needed bootstrapping in unpinned HTTP
	// mode). When non-nil, runAttempt skips the Step 4a
	// Embed call and uses this vector for the upsert.
	// When nil, runAttempt calls Embed normally.
	//
	// Evaluator-3 finding #1 fix: without this, the bootstrap
	// Embed call that warms the unpinned HTTP embedder's
	// model_version cache would be redundantly re-issued by
	// runAttempt for the same content.
	prefetchedVec []float32
}

// runAttempt executes the steps-4-through-7 publish chain
// against the Concept collection. Returns (lastEvent, nil) on
// the happy path with lastEvent=='published'.
//
// Cancellation handling mirrors embedding.Publisher.runAttempt
// (rubber-duck #5): on ctx.Err() in any of the embed / upsert
// / confirm calls we leave the chain at its last durable
// state and surface the cancellation error verbatim — the
// retry phase picks it up on the next tick.
//
// On a recordable failure (embedder error, Qdrant error,
// missing point after upsert) we insert a `failed` event,
// return lastEvent=='failed', and surface the underlying
// error wrapped for caller-side accounting.
func (s *Service) runAttempt(ctx context.Context, conn *sql.Conn, state publishState, content string) (string, error) {
	// Step 4a: embedder call. Skip when the caller already
	// computed the vector (typically promoteOne or
	// processOrphans after ensureModelReady warmed the
	// unpinned HTTP embedder's model_version cache).
	vec := state.prefetchedVec
	if vec == nil {
		v, err := s.embedder.Embed(ctx, content)
		if err != nil {
			if cancelled := ctx.Err(); cancelled != nil {
				return embedding.EventKindQueued, fmt.Errorf("promoter: embedder cancelled: %w", cancelled)
			}
			recordErr := s.insertEvent(ctx, conn, state.publishID, embedding.EventKindFailed,
				state.attemptIndex, failureDetails("embedder", err))
			if recordErr != nil {
				return embedding.EventKindQueued, fmt.Errorf(
					"promoter: embedder failed (%v) AND failed-event insert failed: %w",
					err, recordErr)
			}
			s.logger.Warn("promoter.embedder_failed",
				slog.String("publish_id", state.publishID),
				slog.String("concept_version_id", state.versionID),
				slog.Int("attempt", state.attemptIndex),
				slog.String("error", err.Error()))
			return embedding.EventKindFailed, fmt.Errorf("promoter: embedder: %w", err)
		}
		vec = v
	}

	payload := s.buildPayload(state)

	// Step 4b: Qdrant upsert.
	if err := s.qdrant.Upsert(ctx, embedding.CollectionConcept, state.pointID, vec, payload); err != nil {
		if cancelled := ctx.Err(); cancelled != nil {
			return embedding.EventKindQueued, fmt.Errorf("promoter: qdrant upsert cancelled: %w", cancelled)
		}
		recordErr := s.insertEvent(ctx, conn, state.publishID, embedding.EventKindFailed,
			state.attemptIndex, failureDetails("qdrant_upsert", err))
		if recordErr != nil {
			return embedding.EventKindQueued, fmt.Errorf(
				"promoter: qdrant upsert failed (%v) AND failed-event insert failed: %w",
				err, recordErr)
		}
		s.logger.Warn("promoter.qdrant_upsert_failed",
			slog.String("publish_id", state.publishID),
			slog.String("concept_version_id", state.versionID),
			slog.Int("attempt", state.attemptIndex),
			slog.String("error", err.Error()))
		return embedding.EventKindFailed, fmt.Errorf("promoter: qdrant upsert: %w", err)
	}

	// Step 4c: vector_written event.
	if err := s.insertEvent(ctx, conn, state.publishID, embedding.EventKindVectorWritten,
		state.attemptIndex, nil); err != nil {
		// PG outage after a successful Qdrant upsert — the
		// durable event log diverged from Qdrant. NOT
		// wrapped as a §9.6a failure; caller must abort.
		return embedding.EventKindVectorWritten, fmt.Errorf("promoter: insert vector_written: %w", err)
	}

	// Step 5: read-after-write confirm.
	ok, err := s.qdrant.PointExists(ctx, embedding.CollectionConcept, state.pointID)
	if err != nil || !ok {
		if cancelled := ctx.Err(); cancelled != nil {
			return embedding.EventKindVectorWritten, fmt.Errorf("promoter: qdrant confirm cancelled: %w", cancelled)
		}
		details := failureDetails("qdrant_confirm", err)
		if err == nil {
			details = json.RawMessage(`{"phase":"qdrant_confirm","error":"point not found after upsert"}`)
		}
		recordErr := s.insertEvent(ctx, conn, state.publishID, embedding.EventKindFailed,
			state.attemptIndex, details)
		if recordErr != nil {
			return embedding.EventKindVectorWritten, fmt.Errorf(
				"promoter: qdrant confirm failed AND failed-event insert failed: %w", recordErr)
		}
		if err != nil {
			s.logger.Warn("promoter.qdrant_confirm_failed",
				slog.String("publish_id", state.publishID),
				slog.String("error", err.Error()))
			return embedding.EventKindFailed, fmt.Errorf("promoter: qdrant confirm: %w", err)
		}
		s.logger.Warn("promoter.qdrant_confirm_missing",
			slog.String("publish_id", state.publishID),
			slog.String("point_id", state.pointID))
		return embedding.EventKindFailed, fmt.Errorf("promoter: qdrant confirm: point %s not found in %s",
			state.pointID, embedding.CollectionConcept)
	}

	// Step 6: published event AND atomic supersede of any
	// prior-published row for the same concept_version_id.
	// Mirrors the publisher's same-target supersede path
	// (see internal/embedding/publisher.go) including the
	// per-target advisory xact lock guard
	// (`pg_advisory_xact_lock(hash('embedding_supersede_concept:<version_id>'))`)
	// — that lock is required for correctness, not just
	// performance, because READ COMMITTED would otherwise
	// let two concurrent CTEs miss each other's uncommitted
	// `cur` inserts (independent MVCC snapshots) and leave
	// two `published` events as the latest event for the
	// same target.  The strict-older race-guard predicate
	// is retained as belt-and-suspenders defence.
	supCount, err := s.insertPublishedAndSupersedePrior(ctx, conn,
		state.publishID, state.versionID, state.attemptIndex)
	if err != nil {
		return embedding.EventKindVectorWritten, fmt.Errorf("promoter: insert published+supersede: %w", err)
	}
	if supCount > 0 {
		s.logger.Info("promoter.published_superseded_prior",
			slog.String("publish_id", state.publishID),
			slog.String("concept_version_id", state.versionID),
			slog.Int("superseded_count", supCount))
	}
	s.logger.Info("promoter.published",
		slog.String("publish_id", state.publishID),
		slog.String("concept_version_id", state.versionID),
		slog.String("concept_id", state.conceptID),
		slog.Int("attempt", state.attemptIndex),
		slog.String("mode", state.mode))

	// Snapshot-driven publishes increment `snapshot_published_total`
	// (shared metric name with the embedding-side publisher).
	// Best-effort classification: a failure here does NOT
	// turn a successful publish into a failed one.
	if snap, cErr := s.publishIsSnapshotDriven(ctx, conn, state.publishID); cErr != nil {
		s.logger.Warn("promoter.snapshot_classify_failed",
			slog.String("publish_id", state.publishID),
			slog.String("error", cErr.Error()))
	} else if snap {
		s.metrics.AddSnapshotPublished(1)
	}
	return embedding.EventKindPublished, nil
}

// insertPublishedAndSupersedePrior runs the concept-side §9.6a
// step 6 — appending the `published` event AND atomically
// superseding every OTHER concept-side publish row for the
// same `concept_version_id` whose latest event is `published`
// AND was inserted strictly before the new `published` event.
//
// Mirrors the embedding-side
// `Publisher.insertPublishedAndSupersedePrior` so the two
// writers leave byte-identical event-log shapes AND obey the
// same per-target advisory-lock discipline.  The lock is
// required for correctness: under READ COMMITTED, two
// concurrent statements use independent MVCC snapshots, so
// without the lock two same-target concept publishes could
// both insert `published` without superseding each other,
// leaving the §9.6a recall path with two latest events that
// are `published` for the same `concept_version_id`.
//
// The transaction is opened on the supplied `conn` (the
// session-pinned conn that already holds the tick's
// `pg_try_advisory_lock` for cross-replica serialisation).
// The xact lock acquired inside this sub-transaction is
// disjoint from the session-level lock (different key, different
// scope) and releases automatically on COMMIT / ROLLBACK.
//
// `clock_timestamp()` is used for the `published` and
// `superseded` inserts so the `(created_at, event_id)`
// ordering reflects ACTUAL append order; the column default
// `now()` would otherwise resolve to the sub-tx's start
// timestamp which can predate a publish that committed while
// we were waiting on the lock.
func (s *Service) insertPublishedAndSupersedePrior(
	ctx context.Context,
	conn *sql.Conn,
	publishID, versionID string,
	attempt int,
) (int, error) {
	const cteQuery = `
		WITH cur AS (
			INSERT INTO embedding_publish_event
			    (publish_id, event_kind, attempt_index, details_json, created_at)
			VALUES ($1::uuid, 'published'::embedding_publish_event_kind, $3, NULL, clock_timestamp())
			RETURNING publish_id, event_id, created_at
		),
		prior AS (
			SELECT p.publish_id,
			       coalesce(
			           (SELECT max(ee.attempt_index)
			              FROM embedding_publish_event ee
			             WHERE ee.publish_id = p.publish_id),
			           0
			       ) AS max_attempt
			  FROM embedding_publish p
			  CROSS JOIN LATERAL (
			      SELECT epe.event_kind, epe.event_id, epe.created_at
			        FROM embedding_publish_event epe
			       WHERE epe.publish_id = p.publish_id
			       ORDER BY epe.created_at DESC, epe.event_id DESC
			       LIMIT 1
			  ) latest
			 WHERE p.concept_version_id = $2::uuid
			   AND p.publish_id        <> $1::uuid
			   AND latest.event_kind    = 'published'
			   AND (latest.created_at,  latest.event_id)
			       < ((SELECT created_at FROM cur), (SELECT event_id FROM cur))
		),
		sup AS (
			INSERT INTO embedding_publish_event
			    (publish_id, event_kind, attempt_index, details_json, created_at)
			SELECT publish_id,
			       'superseded'::embedding_publish_event_kind,
			       max_attempt,
			       jsonb_build_object(
			           'superseded_by_publish_id', $1::uuid,
			           'source', 'promoter.runAttempt'
			       ),
			       clock_timestamp()
			  FROM prior
			RETURNING publish_id
		)
		SELECT (SELECT count(*) FROM cur)::bigint AS published_count,
		       (SELECT count(*) FROM sup)::bigint AS superseded_count
	`
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("publish+supersede begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		embedding.SupersedeLockKey(embedding.SupersedeLockDomainConcept, versionID),
	); err != nil {
		return 0, fmt.Errorf("publish+supersede acquire lock: %w", err)
	}

	var publishedCount, supersededCount int64
	if err := tx.QueryRowContext(ctx, cteQuery, publishID, versionID, attempt).
		Scan(&publishedCount, &supersededCount); err != nil {
		return 0, fmt.Errorf("publish+supersede CTE: %w", err)
	}
	if publishedCount != 1 {
		return int(supersededCount), fmt.Errorf(
			"publish+supersede CTE: published_count=%d (want 1)", publishedCount)
	}
	if err := tx.Commit(); err != nil {
		return int(supersededCount), fmt.Errorf("publish+supersede commit: %w", err)
	}
	return int(supersededCount), nil
}

// publishIsSnapshotDriven returns true when the publish's
// queued-event log carries ANY event whose
// `details_json->>'source'` equals `mgmt.snapshot`.  Mirrors
// the embedding-side classifier so the two writers agree on
// what counts as "snapshot-driven" for the
// `snapshot_published_total` counter.
func (s *Service) publishIsSnapshotDriven(ctx context.Context, conn *sql.Conn, publishID string) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1
			  FROM embedding_publish_event
			 WHERE publish_id = $1::uuid
			   AND event_kind = 'queued'
			   AND details_json IS NOT NULL
			   AND details_json->>'source' = 'mgmt.snapshot'
		)
	`
	var snapshot bool
	if err := conn.QueryRowContext(ctx, q, publishID).Scan(&snapshot); err != nil {
		return false, fmt.Errorf("classify snapshot-driven: %w", err)
	}
	return snapshot, nil
}

// insertEvent appends a single embedding_publish_event row.
// Append-only by design — no UPDATE path. Mirrors the
// embedding.Publisher.insertEvent shape so the two writers
// emit byte-identical event rows. Runs on the supplied conn
// (NOT the pool) so the call inherits the session-scoped
// advisory lock from runEmissionPhase's pinned connection.
func (s *Service) insertEvent(
	ctx context.Context,
	conn *sql.Conn,
	publishID, kind string,
	attempt int,
	details json.RawMessage,
) error {
	var detailsArg any
	if len(details) > 0 {
		detailsArg = string(details)
	}
	const q = `
		INSERT INTO embedding_publish_event
		    (publish_id, event_kind, attempt_index, details_json)
		VALUES ($1::uuid, $2::embedding_publish_event_kind, $3, $4::jsonb)
	`
	if _, err := conn.ExecContext(ctx, q, publishID, kind, attempt, detailsArg); err != nil {
		return fmt.Errorf("insert %s event: %w", kind, err)
	}
	return nil
}

// buildPayload assembles the Qdrant payload for a Concept
// publish. Includes the canonical identifiers a recall reader
// needs to dereference a Qdrant hit back to a PostgreSQL row
// without a second join (rubber-duck-style provenance).
func (s *Service) buildPayload(state publishState) map[string]any {
	return map[string]any{
		"kind":                    "concept",
		"concept_id":              state.conceptID,
		"concept_version_id":      state.versionID,
		"publish_id":              state.publishID,
		"fingerprint":             hex.EncodeToString(state.fingerprint),
		"embedding_model_version": state.modelVersion,
		"attempt_index":           state.attemptIndex,
	}
}

// buildConceptContent is the embedding-input template for a
// Concept (architecture §7.8 step 2: "description + canonical
// feature signature"). The current implementation concatenates
// name + description_md; the fingerprint serves as the
// canonical signature via the Qdrant payload rather than the
// embedded text (the fingerprint is not text-meaningful for
// the embedder, but every Concept payload carries it for
// downstream debugging).
func buildConceptContent(name, descriptionMD string) string {
	switch {
	case name == "" && descriptionMD == "":
		return "(empty concept)"
	case descriptionMD == "":
		return name
	case name == "":
		return descriptionMD
	default:
		return name + "\n\n" + descriptionMD
	}
}

// failureDetails marshals a phase + error pair into the
// JSONB-compatible shape the embedding_publish_event.details_json
// column expects. Mirrors embedding.failureDetails.
func failureDetails(phase string, err error) json.RawMessage {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	body := map[string]any{
		"phase": phase,
		"error": msg,
	}
	raw, _ := json.Marshal(body)
	return raw
}

// queuedEventDetails is the JSONB body the promoter writes for
// every `queued` event (initial publish AND every retry). It
// is the §9.6a-compliant snapshot a future operator-side
// resolver can use to round-trip the publish through the
// event log without holding the embedding content in memory
// across failures.
type queuedEventDetails struct {
	ConceptID             string `json:"concept_id"`
	ConceptVersionID      string `json:"concept_version_id"`
	Name                  string `json:"name"`
	DescriptionMD         string `json:"description_md"`
	Fingerprint           string `json:"fingerprint"`
	EmbeddingModelVersion string `json:"embedding_model_version"`
}

func marshalQueuedDetails(c candidate, cvID, modelVersion string) (json.RawMessage, error) {
	body := queuedEventDetails{
		ConceptID:             c.conceptID,
		ConceptVersionID:      cvID,
		Name:                  c.name,
		DescriptionMD:         c.descriptionMD,
		Fingerprint:           hex.EncodeToString(c.fingerprint),
		EmbeddingModelVersion: modelVersion,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal queued details: %w", err)
	}
	return raw, nil
}

func marshalRetryQueuedDetails(st stalled, modelVersion string) (json.RawMessage, error) {
	body := queuedEventDetails{
		ConceptID:             st.conceptID,
		ConceptVersionID:      st.conceptVersionID,
		Name:                  st.name,
		DescriptionMD:         st.descriptionMD,
		Fingerprint:           hex.EncodeToString(st.fingerprint),
		EmbeddingModelVersion: modelVersion,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal retry queued details: %w", err)
	}
	return raw, nil
}

// bandOf maps a confidence value to the concept_band enum
// literal. Mirrors consolidator.bandOf — the threshold for
// "high" (0.7) coincides with DefaultConfidenceThreshold which
// is not an accident: a row that crosses the promotion
// threshold is by definition high-band.
func bandOf(confidence float64) string {
	switch {
	case confidence < 0.3:
		return "low"
	case confidence < 0.7:
		return "medium"
	default:
		return "high"
	}
}
