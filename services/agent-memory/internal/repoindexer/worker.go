package repoindexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// Mode is the ingest_jobs.mode discriminator. The values are
// the closed set defined by the `ingest_mode` ENUM in migration
// 0006a; the Go-side mirror is a typed string so callers cannot
// pass an unrecognised mode to the dispatcher.
type Mode string

const (
	ModeFull   Mode = "full"
	ModeDelta  Mode = "delta"
	ModeManual Mode = "manual"
)

// jobStatus is the ingest_jobs.status discriminator. Mirrors
// `ingest_status` from migration 0006a.
const (
	statusPending = "pending"
	statusClaimed = "claimed"
	statusRunning = "running"
	statusDone    = "done"
	statusFailed  = "failed"
)

// errModeNotImplemented is returned by the dispatcher for
// `delta` and `manual` jobs in Stage 3.1. Stages 3.4/3.5 swap
// in real handlers; the worker treats the error as a terminal
// failure so the queue does not livelock on jobs it cannot
// process.
var errModeNotImplemented = errors.New("repoindexer: handler mode not implemented in this stage")

// ErrNoJob is the sentinel claim() returns when no pending
// `ingest_jobs` row is available. The worker treats this as a
// non-error "queue empty" signal and sleeps for `PollEvery`
// before the next attempt.
var ErrNoJob = errors.New("repoindexer: no pending job")

// Job is the in-memory shape of one `ingest_jobs` row claimed
// by `Worker.Claim`. Exposed so tests can inject synthetic
// jobs into `Worker.runFull` without touching the queue.
type Job struct {
	JobID        string
	RepoID       fingerprint.RepoID
	Mode         Mode
	FromSHA      string
	ToSHA        string
	AttemptIndex int
}

// FullSummary describes what a full-mode run actually wrote.
// Exposed so the integration test for the "small fixture"
// scenario can assert on row counts without re-querying the DB.
type FullSummary struct {
	// RepoNodeID is the textual UUID of the root Repo Node
	// the run ensured.
	RepoNodeID string
	// PackagesEnsured is the count of distinct Package Nodes
	// the run touched (newly-inserted + already-present).
	PackagesEnsured int
	// FilesEnsured is the count of distinct File Nodes the
	// run touched.
	FilesEnsured int
	// PackagesInserted / FilesInserted are the subset of the
	// counts above that produced a newly-inserted Node row.
	// Stage 2.1 idempotent re-ingest asserts these are zero on
	// the second pass.
	PackagesInserted int
	FilesInserted    int
	// ContainsEdgesInserted is the count of `contains` edges
	// newly inserted (Repo→Package and Package→File). Zero on
	// idempotent re-ingest.
	ContainsEdgesInserted int
	// EmitterCalls is the count of `ASTEmitter.EmitFile`
	// invocations. Equal to FilesEnsured on a clean run.
	EmitterCalls int
	// CommitInserted is true when EnsureCommit reported the
	// row was newly inserted (cold registration). Returned for
	// caller introspection / structured-logging only -- the
	// `repo.registered` event is no longer gated on this
	// field; see worker.markDoneAndPublish for the
	// "first done row for (repo, mode='full', from_sha,
	// to_sha)" predicate that survives mid-pipeline retries.
	CommitInserted bool
}

// WorkerOptions wires the dependencies the Worker needs that
// are not in the (db, writer) core pair.
//
// REQUIRED fields (the constructor panics on nil):
//
//   - `Materializer` -- without it the full-mode handler has
//     nowhere to walk file ancestry from.
//   - `Publisher` -- lifecycle events are part of the Stage 3.1
//     acceptance contract. Allowing a nil publisher to silently
//     drop `repo.registered` / `repo.full_ingested` would defeat
//     the LISTEN/NOTIFY hand-off downstream stages depend on.
//
// All other fields are optional; `NewWorker` applies the
// defaults documented per-field.
type WorkerOptions struct {
	// WorkerID is the textual identifier written to
	// `ingest_jobs.claimed_by` on a successful claim. Defaults
	// to "repoindexer-worker-<hostname>-<random>" when empty.
	// Two workers MUST NOT share a WorkerID -- the claim path
	// uses it for diagnostic accounting only, but operators
	// reading the queue rely on it for fanout debugging.
	WorkerID string
	// PollEvery bounds the sleep between consecutive "no job
	// available" polls. Defaults to `DefaultPollEvery`.
	PollEvery time.Duration
	// Materializer prepares the workspace for full/delta mode.
	// Must be non-nil for full-mode jobs; the Worker constructor
	// rejects a nil Materializer when full mode is enabled.
	Materializer Materializer
	// Emitter is the AST emitter. Defaults to `NoopASTEmitter{}`
	// so Stage 3.1 wiring is complete; Stage 3.2 swaps in the
	// tree-sitter dispatcher.
	Emitter ASTEmitter
	// Publisher delivers `repo.registered` / `repo.full_ingested`
	// events. REQUIRED -- the constructor panics on a nil
	// publisher, because lifecycle events are part of the
	// Stage 3.1 acceptance contract (downstream embedding /
	// concept workers LISTEN on `EventChannel` and would never
	// see a completed ingest if the worker silently dropped
	// the publish step). Tests inject `&recordingEventPublisher{}`;
	// production wiring instantiates `NewPGNotifyPublisher`.
	Publisher EventPublisher
	// Logger is the structured logger for worker lifecycle and
	// per-job records. Defaults to `slog.Default()`.
	Logger *slog.Logger
	// Now is the clock used for `created_at` style fields. Tests
	// can pin it for reproducible event payloads. Defaults to
	// `time.Now`.
	Now func() time.Time
	// MaxAttempts caps how many times a single ingest_jobs row
	// may be claimed before the worker gives up and pushes the
	// row to a terminal `failed` state. The cap protects the
	// queue against livelock when a transient publish failure
	// (NOTIFY broker outage, network partition) becomes
	// permanent: each unsuccessful attempt re-queues the row to
	// `pending` (so any worker can retry) until the
	// AttemptIndex on the row crosses MaxAttempts, at which
	// point the row is marked `failed` for operator triage.
	// Zero means use `DefaultMaxAttempts`.
	MaxAttempts int
}

// DefaultMaxAttempts is the operator default for
// `WorkerOptions.MaxAttempts`. The number is small on purpose:
// publish-side failures are usually transient (NOTIFY backend
// flap, brief connectivity loss), so a handful of retries
// covers the recoverable cases without letting a permanently
// broken publisher hammer the materializer / graph writer for
// thousands of attempts before an operator notices.
const DefaultMaxAttempts = 5

// DefaultPollEvery is the default sleep between queue polls
// when no claimable row is available. The value is small enough
// that the §8.3 "first new Node visible within 30 s of webhook
// receipt" delta-ingest target stays within reach (delta-ingest
// polling latency is one PollEvery on the median path), and
// large enough that an idle service does not flood the queue
// with empty SELECTs.
const DefaultPollEvery = 1 * time.Second

// Worker is one polling-loop instance. Spawn N of them through
// `Pool` to hit the §8.3 throughput target; each worker owns its
// own claim transaction so the queue's `FOR UPDATE SKIP LOCKED`
// path is exercised correctly.
//
// Safe for concurrent use across goroutines? NO -- a single
// Worker is a single polling identity. Spawn additional Workers
// via `Pool.Run` for parallelism.
type Worker struct {
	db           *sql.DB
	writer       *graphwriter.Writer
	materializer Materializer
	emitter      ASTEmitter
	publisher    EventPublisher

	workerID    string
	pollEvery   time.Duration
	maxAttempts int

	logger *slog.Logger
	now    func() time.Time
}

// NewWorker constructs a Worker over `db` (an `agent_memory_app`
// connection) and `writer` (a graphwriter.Writer over the same
// role). A nil `db` or `writer` panics -- the worker cannot
// operate without either. Materializer is required for full
// mode; if you intend to only process `delta`/`manual` jobs
// before those handlers exist, pass a stub Materializer to keep
// the constructor honest.
func NewWorker(db *sql.DB, writer *graphwriter.Writer, opts WorkerOptions) *Worker {
	if db == nil {
		panic("repoindexer: NewWorker: nil *sql.DB")
	}
	if writer == nil {
		panic("repoindexer: NewWorker: nil *graphwriter.Writer")
	}
	if opts.Materializer == nil {
		panic("repoindexer: NewWorker: nil Materializer")
	}
	if opts.Publisher == nil {
		// Lifecycle events are an acceptance requirement of
		// Stage 3.1. Refusing nil at construction time keeps
		// production wiring honest: the cmd/repo-indexer
		// entry point cannot accidentally instantiate a
		// worker that silently drops `repo.registered` /
		// `repo.full_ingested` events.
		panic("repoindexer: NewWorker: nil Publisher")
	}
	emitter := opts.Emitter
	if emitter == nil {
		emitter = NoopASTEmitter{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	pollEvery := opts.PollEvery
	if pollEvery <= 0 {
		pollEvery = DefaultPollEvery
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	workerID := opts.WorkerID
	if workerID == "" {
		workerID = defaultWorkerID()
	}
	return &Worker{
		db:           db,
		writer:       writer,
		materializer: opts.Materializer,
		emitter:      emitter,
		publisher:    opts.Publisher,
		workerID:     workerID,
		pollEvery:    pollEvery,
		maxAttempts:  maxAttempts,
		logger:       logger,
		now:          now,
	}
}

// WorkerID exposes the identity the worker writes into
// `ingest_jobs.claimed_by`. Useful for tests that want to assert
// "this worker, not the sibling, claimed the row".
func (w *Worker) WorkerID() string { return w.workerID }

// Run is the polling loop. It exits when `ctx` is cancelled
// (returning ctx.Err()) or when a non-recoverable database error
// surfaces. Per-job processing failures are logged and recorded
// against the job row (status=`failed`); they do NOT exit the
// loop -- a workstream-level catastrophic failure (database
// outage, etc.) does.
//
// Run is the only method that owns the lifecycle; tests that
// want to drive one iteration in isolation should call
// `ProcessOnce` directly.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.Info("repoindexer.worker.start",
		slog.String("op", "worker_run"),
		slog.String("worker_id", w.workerID),
		slog.Duration("poll_every", w.pollEvery),
	)
	defer w.logger.Info("repoindexer.worker.stop",
		slog.String("op", "worker_run"),
		slog.String("worker_id", w.workerID),
	)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		processed, err := w.ProcessOnce(ctx)
		if err != nil {
			// Surface the error and exit -- this is reserved
			// for catastrophic conditions like the DB being
			// down. Per-job handler failures are absorbed by
			// ProcessOnce and never surface here.
			return err
		}
		if processed {
			// Pull again immediately while the queue may
			// still have work; only sleep when we ran dry.
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.pollEvery):
		}
	}
}

// ProcessOnce attempts to claim and run exactly one job. The
// return value `processed` is true when a job was claimed (and
// either succeeded or failed); false means the queue was empty.
// A non-nil error indicates a database-level failure during the
// claim phase that the caller should treat as fatal -- handler
// failures are absorbed and surfaced via the job's
// status='failed' transition instead.
func (w *Worker) ProcessOnce(ctx context.Context) (processed bool, err error) {
	job, err := w.Claim(ctx)
	if err != nil {
		if errors.Is(err, ErrNoJob) {
			return false, nil
		}
		return false, err
	}
	// Beyond this point we have ownership of the row.
	// Any handler failure is recorded against the row; we
	// always return processed=true so the polling loop knows
	// it executed work this tick.
	w.processClaimed(ctx, job)
	return true, nil
}

// Claim wraps the `SELECT ... FOR UPDATE SKIP LOCKED` claim
// pattern in a single transaction that:
//
//  1. selects the oldest pending row matching the partial index
//     `ingest_jobs_pending_idx` (created_at WHERE status='pending'),
//  2. updates it to status='claimed' with claimed_by=worker_id and
//     attempt_index incremented,
//  3. commits.
//
// The combination above is the load-bearing claim-exclusivity
// primitive: two workers racing on the same row see the lock
// taken by the winner and skip it. Exposed (not a private
// helper) so the claim-exclusivity test can call it directly
// against the integration fixture.
func (w *Worker) Claim(ctx context.Context) (Job, error) {
	var job Job
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return job, fmt.Errorf("repoindexer: claim begin: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	const selectQ = `
		SELECT job_id::text, repo_id::text, mode::text,
		       COALESCE(from_sha, ''), to_sha, attempt_index
		FROM ingest_jobs
		WHERE status = 'pending'
		ORDER BY created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`
	var repoIDStr, modeStr string
	row := tx.QueryRowContext(ctx, selectQ)
	if err := row.Scan(
		&job.JobID, &repoIDStr, &modeStr,
		&job.FromSHA, &job.ToSHA, &job.AttemptIndex,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return job, ErrNoJob
		}
		return job, fmt.Errorf("repoindexer: claim select: %w", err)
	}
	repoID, err := fingerprint.ParseRepoID(repoIDStr)
	if err != nil {
		return job, fmt.Errorf("repoindexer: claim parse repo_id: %w", err)
	}
	job.RepoID = repoID
	job.Mode = Mode(modeStr)
	job.AttemptIndex++

	const updateQ = `
		UPDATE ingest_jobs
		SET status        = 'claimed',
		    claimed_by    = $1,
		    attempt_index = $2
		WHERE job_id = $3
	`
	if _, err := tx.ExecContext(ctx, updateQ, w.workerID, job.AttemptIndex, job.JobID); err != nil {
		return job, fmt.Errorf("repoindexer: claim update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return job, fmt.Errorf("repoindexer: claim commit: %w", err)
	}
	rollback = false

	w.logger.Info("repoindexer.worker.claim",
		slog.String("op", "claim_job"),
		slog.String("worker_id", w.workerID),
		slog.String("job_id", job.JobID),
		slog.String("repo_id", repoIDStr),
		slog.String("mode", string(job.Mode)),
		slog.String("from_sha", job.FromSHA),
		slog.String("to_sha", job.ToSHA),
		slog.Int("attempt_index", job.AttemptIndex),
	)
	return job, nil
}

// processClaimed drives the claimed row through the
// running → done|failed terminal transition and calls the
// mode-specific handler. Errors are absorbed: a handler failure
// flips the row to failed and the worker logs the error but
// returns to the polling loop.
func (w *Worker) processClaimed(ctx context.Context, job Job) {
	if err := w.markRunning(ctx, job.JobID); err != nil {
		// We cannot mark the row running -- the cluster is
		// likely in a bad state. We do NOT try to mark it
		// failed (that would be another UPDATE on the same
		// connection). The row stays in `claimed` and an
		// operator-side reaper (out of scope for this stage)
		// will pick it up.
		w.logger.Error("repoindexer.worker.mark_running_failed",
			slog.String("op", "mark_running"),
			slog.String("worker_id", w.workerID),
			slog.String("job_id", job.JobID),
			slog.String("error", err.Error()),
		)
		return
	}
	summary, runErr := w.dispatch(ctx, job)
	if runErr != nil {
		if err := w.markFailed(ctx, job.JobID, runErr); err != nil {
			w.logger.Error("repoindexer.worker.mark_failed_failed",
				slog.String("op", "mark_failed"),
				slog.String("worker_id", w.workerID),
				slog.String("job_id", job.JobID),
				slog.String("error", err.Error()),
				slog.String("run_error", runErr.Error()),
			)
		}
		return
	}
	if err := w.markDoneAndPublish(ctx, job, summary); err != nil {
		// The publish + done-transition atomic tx failed.
		// Per the publisher contract, NEITHER the events
		// nor the status='done' transition committed (or
		// the commit ack was ambiguous -- see below) -- so
		// downstream subscribers cannot have RELIABLY seen
		// a completed ingest for this job.
		//
		// Recovery policy:
		//
		//   1. If the row's AttemptIndex is below MaxAttempts,
		//      transition the row back to `pending` so the
		//      next Claim picks it up. This gives transient
		//      publisher-side failures (NOTIFY broker
		//      flap, brief connectivity loss) a real retry
		//      path -- the prior implementation marked the
		//      row terminal-failed after one publish error,
		//      which Claim could not recover from since it
		//      only consumes pending rows.
		//
		//   2. Once AttemptIndex hits MaxAttempts, fall
		//      through to markFailed so the queue does not
		//      hot-loop on a permanently broken publisher.
		//
		// Both transitions are gated `WHERE status='running'`
		// to defend against the ambiguous-commit case: if
		// PostgreSQL committed the markDoneAndPublish tx
		// successfully but the client received a network
		// error reading the response, the row is already
		// `done` on the server. A blind UPDATE to `pending`
		// would resurrect a successfully-completed job
		// (duplicate events on retry); the WHERE clause
		// makes the recovery path a no-op in that case.
		if job.AttemptIndex < w.maxAttempts {
			if rqErr := w.requeueForRetry(ctx, job.JobID); rqErr != nil {
				w.logger.Error("repoindexer.worker.requeue_failed",
					slog.String("op", "requeue_for_retry"),
					slog.String("worker_id", w.workerID),
					slog.String("job_id", job.JobID),
					slog.String("error", rqErr.Error()),
					slog.String("publish_error", err.Error()),
				)
				return
			}
			w.logger.Warn("repoindexer.worker.requeued",
				slog.String("op", "requeue_for_retry"),
				slog.String("worker_id", w.workerID),
				slog.String("job_id", job.JobID),
				slog.Int("attempt_index", job.AttemptIndex),
				slog.Int("max_attempts", w.maxAttempts),
				slog.String("publish_error", err.Error()),
			)
			return
		}
		if mfErr := w.markFailed(ctx, job.JobID, err); mfErr != nil {
			w.logger.Error("repoindexer.worker.mark_failed_failed",
				slog.String("op", "mark_failed"),
				slog.String("worker_id", w.workerID),
				slog.String("job_id", job.JobID),
				slog.String("error", mfErr.Error()),
				slog.String("run_error", err.Error()),
			)
		}
		return
	}
}

func (w *Worker) dispatch(ctx context.Context, job Job) (FullSummary, error) {
	switch job.Mode {
	case ModeFull:
		return w.runFull(ctx, job)
	case ModeDelta, ModeManual:
		return FullSummary{}, fmt.Errorf("%w: %s", errModeNotImplemented, job.Mode)
	default:
		return FullSummary{}, fmt.Errorf("repoindexer: unknown mode %q", job.Mode)
	}
}

// markRunning transitions a claimed row to running with a single
// UPDATE outside any application transaction -- the row's
// status is the worker's heartbeat surface; we want the change
// visible to operators querying the queue immediately.
func (w *Worker) markRunning(ctx context.Context, jobID string) error {
	_, err := w.db.ExecContext(ctx,
		`UPDATE ingest_jobs SET status='running' WHERE job_id=$1`,
		jobID,
	)
	if err != nil {
		return fmt.Errorf("repoindexer: mark_running: %w", err)
	}
	return nil
}

// markDoneAndPublish writes the lifecycle events AND the
// status='done' transition in a SINGLE PostgreSQL transaction.
// PostgreSQL queues NOTIFY payloads in the issuing tx and only
// delivers them when the tx commits, so the two effects are
// committed atomically:
//
//   - Either the tx commits, status='done' is durable, AND every
//     subscribed LISTEN'er sees `repo.registered` (when applicable)
//     and `repo.full_ingested`.
//   - Or the tx rolls back, status stays at the pre-call value
//     (`running`), no notifications are delivered, and the
//     caller falls into the requeue-for-retry path so transient
//     publisher failures get a real retry instead of silently
//     marking the row terminal-failed (see processClaimed).
//
// This satisfies the publisher contract in publisher.go: a
// publish failure NEVER leaves the queue in a state where
// `status='done'` for a job whose events were silently dropped.
//
// Two-event semantics:
//
//   - `repo.registered` fires on the FIRST `done` ingest_jobs
//     row for the (repo_id, mode='full', from_sha, to_sha)
//     tuple. The predicate is evaluated INSIDE the same tx via
//     `NOT EXISTS(SELECT 1 FROM ingest_jobs WHERE ... status='done'
//     AND job_id != $current)`, so it is consistent with the
//     UPDATE→done that follows. Using this predicate (instead of
//     the prior `EnsureCommit.Inserted`) means a transient
//     post-EnsureCommit failure that succeeds on retry STILL
//     fires `repo.registered` -- the prior implementation
//     suppressed the event because the commit row already existed
//     and EnsureCommit returned Inserted=false on the second
//     attempt.
//   - `repo.full_ingested` fires unconditionally on every
//     successful full-mode completion (cold AND idempotent
//     re-ingest of an already-indexed SHA).
//
// Concurrency: the dedupe UNIQUE INDEX on
// (repo_id, mode, COALESCE(from_sha,”), to_sha) prevents two
// distinct ingest_jobs rows for the same tuple, so concurrent
// jobs racing the predicate is impossible by construction. The
// predicate's `from_sha` clause matches the COALESCE shape of
// the unique index so the safety property survives any future
// schema change that broadens the dedupe key.
func (w *Worker) markDoneAndPublish(ctx context.Context, job Job, summary FullSummary) (err error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("repoindexer: mark_done begin: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	repoIDStr := job.RepoID.String()
	now := w.now().UTC()

	// Predicate: is this the FIRST ingest_jobs row for the
	// (repo, mode='full', from_sha, to_sha) tuple to reach
	// status='done'? The query is bounded to mode='full' so a
	// future delta-mode re-ingest of an already-indexed SHA
	// does not retroactively suppress repo.registered.
	var priorDoneExists bool
	if pErr := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
		    SELECT 1 FROM ingest_jobs
		    WHERE repo_id = $1
		      AND mode    = 'full'
		      AND to_sha  = $2
		      AND COALESCE(from_sha, '') = COALESCE($3, '')
		      AND status  = 'done'
		      AND job_id != $4
		)
	`, repoIDStr, job.ToSHA, nullableSHA(job.FromSHA), job.JobID).Scan(&priorDoneExists); pErr != nil {
		return fmt.Errorf("repoindexer: prior_done lookup: %w", pErr)
	}
	firstSuccessfulIngest := !priorDoneExists

	if firstSuccessfulIngest {
		ev := Event{
			Kind:   EventKindRepoRegistered,
			RepoID: repoIDStr,
			SHA:    job.ToSHA,
			JobID:  job.JobID,
			Time:   now,
		}
		if pErr := w.publisher.PublishTx(ctx, tx, ev); pErr != nil {
			w.logger.Error("repoindexer.worker.publish_failed",
				slog.String("op", "publish_completion"),
				slog.String("kind", EventKindRepoRegistered),
				slog.String("job_id", job.JobID),
				slog.String("error", pErr.Error()),
			)
			return fmt.Errorf("repoindexer: publish %s: %w", EventKindRepoRegistered, pErr)
		}
	}
	{
		ev := Event{
			Kind:   EventKindRepoFullIngested,
			RepoID: repoIDStr,
			SHA:    job.ToSHA,
			JobID:  job.JobID,
			Time:   now,
		}
		if pErr := w.publisher.PublishTx(ctx, tx, ev); pErr != nil {
			w.logger.Error("repoindexer.worker.publish_failed",
				slog.String("op", "publish_completion"),
				slog.String("kind", EventKindRepoFullIngested),
				slog.String("job_id", job.JobID),
				slog.String("error", pErr.Error()),
			)
			return fmt.Errorf("repoindexer: publish %s: %w", EventKindRepoFullIngested, pErr)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE ingest_jobs SET status='done' WHERE job_id=$1`, job.JobID,
	); err != nil {
		return fmt.Errorf("repoindexer: mark_done update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("repoindexer: mark_done commit: %w", err)
	}
	rollback = false

	w.logger.Info("repoindexer.worker.done",
		slog.String("op", "mark_done"),
		slog.String("worker_id", w.workerID),
		slog.String("job_id", job.JobID),
		slog.String("repo_node_id", summary.RepoNodeID),
		slog.Int("packages_ensured", summary.PackagesEnsured),
		slog.Int("packages_inserted", summary.PackagesInserted),
		slog.Int("files_ensured", summary.FilesEnsured),
		slog.Int("files_inserted", summary.FilesInserted),
		slog.Int("contains_edges_inserted", summary.ContainsEdgesInserted),
		slog.Int("emitter_calls", summary.EmitterCalls),
		slog.Bool("commit_inserted", summary.CommitInserted),
		slog.Bool("first_successful_ingest", firstSuccessfulIngest),
	)
	return nil
}

// nullableSHA preserves the NULL/empty distinction the
// ingest_jobs schema's `from_sha text` column uses. The
// dedupe UNIQUE INDEX is keyed on COALESCE(from_sha,”), so
// passing NULL for an empty Go-side string keeps the predicate
// in markDoneAndPublish aligned with the index. lib/pq decodes
// `nil any` to a SQL NULL literal.
func nullableSHA(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// requeueForRetry transitions a `running` row back to `pending`
// so the next Claim picks it up. The `WHERE status='running'`
// guard is load-bearing: if the prior markDoneAndPublish tx
// committed successfully on the server but the client lost the
// commit ack (network partition, broker crash mid-response), the
// row is already `done` -- and a blind `UPDATE status='pending'`
// would resurrect a successfully-completed job, causing
// duplicate events on the eventual retry. The conditional
// UPDATE makes that case a no-op (the affected-row count is
// zero) without surfacing the ambiguity to the caller.
//
// Returning a nil error means "we attempted the recovery
// transition cleanly"; the caller does NOT introspect the
// affected-row count because either outcome (requeued or
// ambiguous-already-done) is operationally fine.
func (w *Worker) requeueForRetry(ctx context.Context, jobID string) error {
	_, err := w.db.ExecContext(ctx,
		`UPDATE ingest_jobs SET status='pending' WHERE job_id=$1 AND status='running'`,
		jobID,
	)
	if err != nil {
		return fmt.Errorf("repoindexer: requeue_for_retry: %w", err)
	}
	return nil
}

// markFailed transitions the row to status='failed' and logs
// the underlying error. The `WHERE status='running'` guard
// defends against the same ambiguous-commit edge requeueForRetry
// guards against: if a prior commit silently succeeded, blindly
// flipping status would corrupt a real done state. The trigger
// keeps `updated_at` fresh either way (or skips when the WHERE
// matches no row).
//
// The queue does NOT store the error message itself (no column
// for it in 0006a); structured logs are the source of truth for
// failure diagnosis.
func (w *Worker) markFailed(ctx context.Context, jobID string, runErr error) error {
	_, err := w.db.ExecContext(ctx,
		`UPDATE ingest_jobs SET status='failed' WHERE job_id=$1 AND status='running'`,
		jobID,
	)
	if err != nil {
		return fmt.Errorf("repoindexer: mark_failed: %w", err)
	}
	w.logger.Error("repoindexer.worker.failed",
		slog.String("op", "handler"),
		slog.String("worker_id", w.workerID),
		slog.String("job_id", jobID),
		slog.String("error", runErr.Error()),
	)
	return nil
}

// publishCompletion was the original Stage 3.1 publish surface
// — kept removed (replaced by `markDoneAndPublish` which delivers
// events ATOMICALLY with the status='done' transition). The
// function is intentionally absent so the only place lifecycle
// events fire is the atomic-tx path; no parallel non-atomic
// publish surface can be reintroduced by accident.

// ----- Full mode handler -----------------------------------------

// fullModeAttrs is the JSON shape stored on `Node.attrs_json` for
// every Node the full-mode handler emits in Stage 3.1. Kept as a
// typed struct so future fields (block_kind, language hints,
// etc.) are added in one place rather than scattered json.Marshal
// call sites. The wire format is stable (snake_case tags) so
// downstream consumers can decode without depending on this Go
// module.
type fullModeAttrs struct {
	// RelPath is the workspace-relative path of the entity.
	// Empty for the root Repo Node (which has no path).
	RelPath string `json:"rel_path,omitempty"`
	// Producer pins the source of the Node so downstream
	// consumers can distinguish "structural ancestry written
	// by Stage 3.1" from "AST-derived Node written by Stage
	// 3.2".
	Producer string `json:"producer"`
}

// runFull is the §3.1-step-3 entry point. The handler:
//
//  1. Looks up the repo URL from the `repo` row keyed by
//     job.RepoID (required for canonical signatures).
//  2. Materialises the workspace via the injected Materializer.
//     This validates the SHA exists in the remote BEFORE we
//     write any persistent state -- a failed `git fetch` or
//     unknown SHA returns here with NO `repo_commit` row left
//     behind that a later retry would interpret as "this SHA
//     was already cold-registered" (suppressing the
//     `repo.registered` event on the eventual successful run).
//  3. Ensures the Commit row exists via EnsureCommit; remembers
//     whether the row was newly inserted (for the
//     `repo.registered` event branch).
//  4. Ensures the root Repo Node.
//  5. Walks every file. For each file:
//     a. Ensures the parent Package Node (one per unique
//     directory) with parent_node_id pointing at the Repo
//     Node.
//     b. Inserts a Repo→Package `contains` Edge the first
//     time the package is seen.
//     c. Inserts the File Node with parent_node_id pointing
//     at the Package Node.
//     d. Inserts a Package→File `contains` Edge.
//     e. Delegates to the ASTEmitter for per-file Class /
//     Method / Block emission (Stage 3.2 surface; the
//     default emitter is a no-op).
//
// All structural inserts are routed through the graphwriter
// library so the role-grant policy (§8.7.4) and G2/G5 invariants
// are enforced at the database layer. The handler is idempotent:
// re-running it against the same SHA produces zero net new rows
// (Stage 2.1 dedupe path).
func (w *Worker) runFull(ctx context.Context, job Job) (FullSummary, error) {
	// 1. Resolve repo URL + language_hints. The worker is the
	// only caller that needs this read on the `repo` table;
	// the app role has SELECT (per migration 0016 USAGE +
	// per-table grants). `language_hints` flows through to
	// EmitFileEvent so each per-file dispatcher invocation
	// receives the repo's own hint set -- the evaluator-
	// flagged correctness gate (Stage 3.2 iter-2 finding #4).
	var (
		repoURL  string
		repoLang []string
	)
	if err := w.db.QueryRowContext(ctx,
		`SELECT url, language_hints FROM repo WHERE repo_id = $1`, job.RepoID.String(),
	).Scan(&repoURL, pq.Array(&repoLang)); err != nil {
		return FullSummary{}, fmt.Errorf("repoindexer: lookup repo url: %w", err)
	}

	// 2. Materialise the workspace FIRST. A bad SHA, a network
	// failure, or an auth error surfaces here BEFORE any
	// `repo_commit` row is written -- so a subsequent retry
	// against the same SHA still sees CommitInserted=true on
	// EnsureCommit and correctly fires the `repo.registered`
	// cold-registration event.
	ws, err := w.materializer.Materialize(ctx, repoURL, job.ToSHA)
	if err != nil {
		return FullSummary{}, fmt.Errorf("repoindexer: materialize: %w", err)
	}
	defer func() {
		if cerr := ws.Close(); cerr != nil {
			w.logger.Warn("repoindexer.worker.workspace_close_failed",
				slog.String("op", "workspace_close"),
				slog.String("job_id", job.JobID),
				slog.String("error", cerr.Error()),
			)
		}
	}()

	// 3. EnsureCommit so the commit ancestry is in place. Only
	// reached after Materialize confirmed the SHA exists.
	commitRec, err := w.writer.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      job.RepoID,
		SHA:         job.ToSHA,
		ParentSHA:   job.FromSHA,
		CommittedAt: w.now().UTC(),
	})
	if err != nil {
		return FullSummary{}, fmt.Errorf("repoindexer: ensure commit: %w", err)
	}

	// 4. Ensure the root Repo Node.
	repoAttrs, err := json.Marshal(fullModeAttrs{Producer: "repoindexer.full"})
	if err != nil {
		return FullSummary{}, fmt.Errorf("repoindexer: marshal repo attrs: %w", err)
	}
	repoNode, err := w.writer.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             job.RepoID,
		Kind:               "repo",
		CanonicalSignature: canonicalRepoSig(repoURL),
		FromSHA:            job.ToSHA,
		AttrsJSON:          repoAttrs,
	})
	if err != nil {
		return FullSummary{}, fmt.Errorf("repoindexer: insert repo node: %w", err)
	}

	summary := FullSummary{
		RepoNodeID:     repoNode.NodeID,
		CommitInserted: commitRec.Inserted,
	}

	// 5. Walk files. We keep one cache keyed by package
	// directory so each unique dir only triggers one
	// InsertNode + one Repo→Package contains-edge.
	type pkgEntry struct {
		nodeID string
	}
	packages := make(map[string]pkgEntry)

	walkErr := ws.Walk(func(file WalkFile) error {
		dir := canonicalPackageDir(file.RelPath)
		pkg, ok := packages[dir]
		if !ok {
			pkgAttrs, mErr := json.Marshal(fullModeAttrs{
				RelPath: dir, Producer: "repoindexer.full",
			})
			if mErr != nil {
				return fmt.Errorf("repoindexer: marshal pkg attrs: %w", mErr)
			}
			pkgRec, pErr := w.writer.InsertNode(ctx, graphwriter.NodeInput{
				RepoID:             job.RepoID,
				Kind:               "package",
				CanonicalSignature: canonicalPackageSig(repoURL, dir),
				ParentNodeID:       repoNode.NodeID,
				FromSHA:            job.ToSHA,
				AttrsJSON:          pkgAttrs,
			})
			if pErr != nil {
				return fmt.Errorf("repoindexer: insert package node (%s): %w", dir, pErr)
			}
			pkg = pkgEntry{nodeID: pkgRec.NodeID}
			packages[dir] = pkg

			// Repo→Package contains edge.
			edgeRec, eErr := w.writer.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    job.RepoID,
				Kind:      "contains",
				SrcNodeID: repoNode.NodeID,
				DstNodeID: pkg.nodeID,
				FromSHA:   job.ToSHA,
			})
			if eErr != nil {
				return fmt.Errorf("repoindexer: insert repo->pkg edge: %w", eErr)
			}
			summary.PackagesEnsured++
			if pkgRec.Inserted {
				summary.PackagesInserted++
			}
			if edgeRec.Inserted {
				summary.ContainsEdgesInserted++
			}
		}

		// File Node.
		fileAttrs, mErr := json.Marshal(fullModeAttrs{
			RelPath: file.RelPath, Producer: "repoindexer.full",
		})
		if mErr != nil {
			return fmt.Errorf("repoindexer: marshal file attrs: %w", mErr)
		}
		fileRec, fErr := w.writer.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             job.RepoID,
			Kind:               "file",
			CanonicalSignature: canonicalFileSig(repoURL, file.RelPath),
			ParentNodeID:       pkg.nodeID,
			FromSHA:            job.ToSHA,
			AttrsJSON:          fileAttrs,
		})
		if fErr != nil {
			return fmt.Errorf("repoindexer: insert file node (%s): %w", file.RelPath, fErr)
		}
		summary.FilesEnsured++
		if fileRec.Inserted {
			summary.FilesInserted++
		}

		// Package→File contains edge.
		edgeRec, eErr := w.writer.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    job.RepoID,
			Kind:      "contains",
			SrcNodeID: pkg.nodeID,
			DstNodeID: fileRec.NodeID,
			FromSHA:   job.ToSHA,
		})
		if eErr != nil {
			return fmt.Errorf("repoindexer: insert pkg->file edge: %w", eErr)
		}
		if edgeRec.Inserted {
			summary.ContainsEdgesInserted++
		}

		// Delegate Class/Method/Block emission to Stage 3.2.
		emErr := w.emitter.EmitFile(ctx, EmitFileEvent{
			RepoID:        job.RepoID,
			RepoURL:       repoURL,
			SHA:           job.ToSHA,
			FileNodeID:    fileRec.NodeID,
			RepoNodeID:    repoNode.NodeID,
			RelPath:       file.RelPath,
			AbsPath:       file.AbsPath,
			LanguageHints: repoLang,
			Open:          file.Reader,
		})
		if emErr != nil {
			return fmt.Errorf("repoindexer: ast emitter (%s): %w", file.RelPath, emErr)
		}
		summary.EmitterCalls++
		return nil
	})
	if walkErr != nil {
		return summary, walkErr
	}
	return summary, nil
}

// canonicalRepoSig is the canonical signature for the root Repo
// Node. Just the URL -- there's only one Repo Node per repo so a
// richer signature would be redundant.
func canonicalRepoSig(repoURL string) string { return repoURL }

// canonicalPackageDir normalises the directory key the
// package cache uses. Returns "" for files at the repo root (so
// the root "package" has a stable signature) and the
// forward-slash directory path otherwise.
//
// path.Dir from Go's standard library returns "." for files
// without a directory; we collapse that to "" so the canonical
// signature reads as `<url>::pkg::` rather than `<url>::pkg::.`,
// matching how operators expect to read the value.
func canonicalPackageDir(relPath string) string {
	d := path.Dir(relPath)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// canonicalPackageSig is the canonical signature for a Package
// Node. The format is `<repo url>::pkg::<dir path>` where the
// dir path is forward-slash relative. Choosing a distinct
// `::pkg::` separator prevents collisions with the file-level
// canonical signature `::file::<path>` -- a directory named
// `foo.go` cannot collide with a file named `foo.go` because
// the segment between `<repo url>` and the path differs.
func canonicalPackageSig(repoURL, dir string) string {
	return repoURL + "::pkg::" + dir
}

// canonicalFileSig is the canonical signature for a File Node.
// Format `<repo url>::file::<rel path>`.
func canonicalFileSig(repoURL, relPath string) string {
	return repoURL + "::file::" + relPath
}
