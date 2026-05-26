package metric_ingestor

// Stage 3.4 -- retraction dispatcher.
//
// The Management surface NEVER writes the Measurement
// sub-store directly (architecture Sec 6.3 / Sec 1.5.1 row
// 5; tech-spec Sec 7.2 + the `mgmt.retract_sample` row in
// the writer-ownership table). When an operator calls
// `mgmt.retract_sample(sample_id, reason, actor)`:
//
//  1. The Management surface APPENDS a
//     `repo_event(kind='retract_intent', payload={sample_id,
//     reason})` row in the Catalog / Lifecycle sub-store
//     (architecture Sec 5.1.4 line 883 -- `retract_intent`
//     is the third of four canonical past-tense kinds).
//
//  2. The Management surface DELEGATES to the Metric
//     Ingestor's [RetractDispatcher.Dispatch] which:
//       a. opens a `scan_run(kind='retract',
//          sha_binding='single', status='running')`,
//       b. appends a `metric_retraction(retraction_id,
//          sample_id, reason, appended_by, created_at)` row
//          in the Measurement sub-store, and
//       c. transitions the `scan_run` to `succeeded` /
//          `failed`.
//
// The `metric_sample_active` pointer row is NEVER deleted
// (tech-spec Sec 7.2 line 1248 REVOKEs DELETE on
// `metric_sample_active` from every role): SHA-pinned
// readers (`mgmt.read.metric_sample`, `mgmt.read.metric_samples`,
// `eval.gate`) filter out the retracted sample by joining
// through `metric_retraction` -- the active pointer remains
// in place as the audit trail.
//
// # Idempotency (impl-plan Stage 3.4 line 331)
//
// A retract call against an already-retracted sample is a
// no-op: the dispatcher returns the existing retraction's
// id WITHOUT opening a new scan_run AND without appending a
// second `metric_retraction` row (the schema's UNIQUE on
// `sample_id` would reject the second INSERT anyway, but
// the dispatcher short-circuits BEFORE the DB call to keep
// the operator-side wire response stable).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/repo_indexer"
)

// Sentinel errors emitted by the retract path.
var (
	// ErrRetractZeroSampleID is returned when a
	// [RetractRequest] carries the zero UUID as
	// `SampleID`. A zero sample_id is always an
	// uninitialised caller value.
	ErrRetractZeroSampleID = errors.New("metric_ingestor: RetractRequest.SampleID is the zero UUID")
	// ErrRetractEmptyReason is returned when the reason
	// field is empty or whitespace-only. The schema
	// column is `NOT NULL` and the audit trail must
	// carry an operator-facing string.
	ErrRetractEmptyReason = errors.New("metric_ingestor: RetractRequest.Reason is empty")
	// ErrRetractEmptyAppendedBy is returned when the
	// `appended_by` attribution is empty or whitespace-
	// only. Production callers stamp this with
	// `operator:<oidc-subject>` per architecture
	// Sec 5.2.2 line 1033.
	ErrRetractEmptyAppendedBy = errors.New("metric_ingestor: RetractRequest.AppendedBy is empty")
	// ErrRetractUnknownSample is returned when the
	// retraction store cannot find a `metric_sample` row
	// with the requested `sample_id`. Surfaced as a
	// distinct sentinel so the HTTP layer maps it to 404
	// rather than 500.
	ErrRetractUnknownSample = errors.New("metric_ingestor: sample_id not found in metric_sample")
	// ErrRetractStoreUnwired is returned by
	// [RetractDispatcher.Dispatch] when its store
	// dependency is nil. A wired dispatcher with no store
	// cannot append anything; surfacing the wiring bug
	// here keeps the composition-root error pointed at
	// the missing seam.
	ErrRetractStoreUnwired = errors.New("metric_ingestor: RetractDispatcher.Store is nil")
)

// RetractRequest is the per-call input to
// [RetractDispatcher.Dispatch]. Pinned as a struct (not
// positional args) so future fields (`actor_oidc_email`,
// `correlation_id`, ...) can be added without breaking
// callers.
type RetractRequest struct {
	// SampleID is the `metric_sample.sample_id` of the
	// row being retracted. Must be non-zero.
	SampleID uuid.UUID
	// Reason is the free-form operator-facing string
	// pinned into `metric_retraction.reason`. Common
	// values: `"file is vendored"`, `"defective recipe
	// emission"`, `"superseded"`.
	Reason string
	// AppendedBy is the audit attribution stamped into
	// `metric_retraction.appended_by`. Production callers
	// pass `"operator:<oidc-subject>"`; the
	// Ingestor-internal supersede path passes
	// `"ingestor"`.
	AppendedBy string
}

// Validate runs the cheap field validations and returns
// the first failure. Wraps the dispatcher-side sentinels
// so callers can `errors.Is`.
func (r RetractRequest) Validate() error {
	if r.SampleID == uuid.Nil {
		return ErrRetractZeroSampleID
	}
	if strings.TrimSpace(r.Reason) == "" {
		return ErrRetractEmptyReason
	}
	if strings.TrimSpace(r.AppendedBy) == "" {
		return ErrRetractEmptyAppendedBy
	}
	return nil
}

// RetractionRow mirrors one `clean_code.metric_retraction`
// row. Returned from the store on a fresh INSERT and on
// the idempotent re-retract path so callers always have
// the canonical row to return on the wire.
type RetractionRow struct {
	// RetractionID is the row's `retraction_id` PK.
	RetractionID uuid.UUID
	// SampleID is the retracted `metric_sample.sample_id`.
	SampleID uuid.UUID
	// Reason mirrors the `reason` column.
	Reason string
	// AppendedBy mirrors the `appended_by` column.
	AppendedBy string
	// CreatedAt mirrors the `created_at` column.
	CreatedAt time.Time
}

// RetractionStore is the persistence seam the
// [RetractDispatcher] uses for the Measurement-sub-store
// append. The interface is intentionally narrow -- it
// owns ONLY the `metric_retraction` table; the parent
// scan_run lifecycle is owned by [ScanRunStore].
//
// The production implementation (Phase 3.5 / 3.6) wires
// this against PG. Stage 3.4 ships an in-memory
// implementation that emulates the schema's UNIQUE
// constraint on `sample_id` so the idempotency contract
// is exercised end-to-end.
//
// # Required semantics
//
//  1. [RetractionStore.Lookup] returns the EXISTING
//     `metric_retraction` row for `sample_id` if any.
//     Dispatchers call this BEFORE opening a scan_run so
//     the sequential idempotency path returns
//     `ScanRunID=uuid.Nil` (per the contract on
//     [RetractResult.ScanRunID]). On the race-loser path
//     [RetractionStore.Append] returns inserted=false
//     even though Lookup said no -- that case finalises
//     the already-opened scan_run as succeeded; see
//     [RetractDispatcher.Dispatch] for the audit trail.
//  2. [RetractionStore.Append] MUST be idempotent on
//     `sample_id`: a second call with the same
//     `sample_id` returns the EXISTING row (found=true)
//     without inserting a second row. The schema's UNIQUE
//     on `sample_id` enforces this at the DB layer; the
//     store interface mirrors it so callers see uniform
//     semantics across in-memory / PG.
//  3. [RetractionStore.SampleExists] reports whether the
//     referenced `metric_sample` row exists. Returning
//     (false, nil) maps to [ErrRetractUnknownSample] at
//     the dispatcher.
type RetractionStore interface {
	// SampleExists returns (true, nil) iff a
	// `metric_sample` row with the given `sample_id`
	// exists. Returns (false, nil) when no such row
	// exists. Infrastructure failure returns (false, err).
	SampleExists(ctx context.Context, sampleID uuid.UUID) (bool, error)

	// Lookup returns the EXISTING `metric_retraction`
	// row for `sample_id` if any. Returns (zero, false,
	// nil) when no retraction exists for the sample.
	// Infrastructure failure returns (zero, false, err).
	//
	// Dispatchers MUST probe Lookup BEFORE opening a
	// scan_run so the sequential idempotency contract is
	// honoured (no fresh scan_run row when the sample is
	// already retracted).
	Lookup(ctx context.Context, sampleID uuid.UUID) (RetractionRow, bool, error)

	// Append inserts a new `metric_retraction` row or
	// returns the existing one when a row with the same
	// `sample_id` already exists.
	//
	// On a fresh insert returns (row, inserted=true, nil).
	// On an idempotent no-op (sample already retracted)
	// returns (existingRow, inserted=false, nil).
	// On infrastructure failure returns (zero, false, err).
	Append(ctx context.Context, row RetractionRow) (RetractionRow, bool, error)
}

// RetractScanRunStore is the narrow subset of the parent
// [ScanRunStore] that the [RetractDispatcher] needs --
// just the open + finalize lifecycle for a
// `kind='retract'` row that has NO claimed commit
// (retract scans do NOT mutate `commit.scan_status`; the
// architecture only transitions the four canonical commit
// states through Repo Indexer + Metric Ingestor on
// foundation scans).
//
// The interface lives here (not on [ScanRunStore]) so the
// retract path stays decoupled from the foundation-tier
// claim flow.
type RetractScanRunStore interface {
	// OpenRetractScanRun INSERTs a fresh
	// `scan_run(kind='retract', sha_binding='single',
	// status='running', to_sha=$sha)` row. Returns the
	// minted `scan_run_id`. `repoID` is the parent
	// `repo` row; production wiring resolves it from the
	// retracted sample's `metric_sample.repo_id`.
	//
	// SHA is required because the DB CHECK constraint on
	// `scan_run` pins `sha_binding='single' AND
	// to_sha IS NOT NULL`; the retract scan binds to the
	// retracted sample's SHA so the audit trail names the
	// commit whose row was retracted.
	OpenRetractScanRun(ctx context.Context, repoID uuid.UUID, sha string, openedAt time.Time) (uuid.UUID, error)

	// FinalizeRetractScanRun transitions the row's
	// `scan_run.status` to `status` and stamps `ended_at`.
	// `status` MUST be `succeeded` or `failed`; the
	// implementation rejects `running` to prevent
	// double-finalize.
	FinalizeRetractScanRun(ctx context.Context, scanRunID uuid.UUID, status ScanRunStatus, endedAt time.Time) error
}

// SampleResolver is the seam the dispatcher uses to
// resolve `(sample_id) -> (repo_id, sha)` for the
// retract scan_run row. The Measurement sub-store owns
// `metric_sample.repo_id` and `metric_sample.sha`; the
// retract dispatcher needs both to OPEN the scan_run row
// (the row's `repo_id` FKs to `clean_code.repo`, and the
// CHECK constraint pins `to_sha IS NOT NULL` for
// `sha_binding='single'`).
type SampleResolver interface {
	// ResolveSample returns the (repo_id, sha) tuple for
	// the named sample. Returns (zero, zero, false, nil)
	// when no such sample exists. Infrastructure failure
	// returns (zero, zero, false, err).
	ResolveSample(ctx context.Context, sampleID uuid.UUID) (repoID uuid.UUID, sha string, found bool, err error)
}

// RetractDispatcher is the Metric-Ingestor-side handler
// for the `mgmt.retract_sample` verb. The Management
// surface CALLS this; the dispatcher owns the
// Measurement-sub-store append.
//
// Construct via [NewRetractDispatcher]. The dispatcher is
// stateless past its dependencies; one instance handles
// every concurrent request.
type RetractDispatcher struct {
	scanStore   RetractScanRunStore
	retractions RetractionStore
	resolver    SampleResolver
	clock       func() time.Time
	log         *slog.Logger
}

// RetractDispatcherOption configures a [RetractDispatcher]
// at construction time.
type RetractDispatcherOption func(*RetractDispatcher)

// WithRetractDispatcherClock overrides the clock the
// dispatcher uses for `scan_run.started_at` /
// `scan_run.ended_at` / `metric_retraction.created_at`.
// Default is [time.Now]. Tests inject a fixed clock so
// timestamps are deterministic.
func WithRetractDispatcherClock(now func() time.Time) RetractDispatcherOption {
	return func(d *RetractDispatcher) {
		d.clock = now
	}
}

// WithRetractDispatcherLogger overrides the logger. nil
// disables logging (the zero value is permitted).
func WithRetractDispatcherLogger(log *slog.Logger) RetractDispatcherOption {
	return func(d *RetractDispatcher) {
		d.log = log
	}
}

// NewRetractDispatcher returns a wired [RetractDispatcher].
// PANICS when any of the three dependencies is nil; each
// is non-optional and a missing seam is always a wiring
// bug.
func NewRetractDispatcher(scanStore RetractScanRunStore, retractions RetractionStore, resolver SampleResolver, opts ...RetractDispatcherOption) *RetractDispatcher {
	if scanStore == nil {
		panic("metric_ingestor: NewRetractDispatcher received nil RetractScanRunStore")
	}
	if retractions == nil {
		panic("metric_ingestor: NewRetractDispatcher received nil RetractionStore")
	}
	if resolver == nil {
		panic("metric_ingestor: NewRetractDispatcher received nil SampleResolver")
	}
	d := &RetractDispatcher{
		scanStore:   scanStore,
		retractions: retractions,
		resolver:    resolver,
		clock:       time.Now,
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.clock == nil {
		d.clock = time.Now
	}
	return d
}

// RetractResult is the structured outcome of one
// [RetractDispatcher.Dispatch] call. The shape is
// identical on a fresh insert and on the idempotent
// re-retract path so the HTTP layer's response body
// stays stable.
type RetractResult struct {
	// Retraction is the persisted (or already-existing)
	// `metric_retraction` row.
	Retraction RetractionRow
	// ScanRunID is the `scan_run.scan_run_id` the
	// dispatcher opened for THIS call. On the SEQUENTIAL
	// idempotency path (Lookup says retraction already
	// exists) the value is [uuid.Nil] -- no new scan_run
	// is opened when the retraction already exists; the
	// audit trail of the original scan_run is sufficient.
	//
	// On a RACE-LOSER path (a concurrent writer landed
	// the retraction between this dispatcher's Lookup and
	// Append) the value is the freshly-opened scan_run's
	// id -- the row exists in `scan_run(status=succeeded)`
	// and surfacing its id is the honest answer so an
	// operator can correlate the duplicated audit trail.
	// Combined with Inserted=false the caller can tell
	// the two paths apart.
	ScanRunID uuid.UUID
	// Inserted is true iff a new `metric_retraction` row
	// was appended by this call. false on EITHER
	// idempotency path (sequential or race-loser).
	Inserted bool
}

// Dispatch executes the retract flow:
//
//  1. Validate the request shape.
//  2. Resolve `(repo_id, sha)` for the sample. A missing
//     sample maps to [ErrRetractUnknownSample] -- the
//     wire layer returns 404.
//  3. SEQUENTIAL-IDEMPOTENCY SHORT-CIRCUIT: probe
//     [RetractionStore.Lookup]. If a retraction already
//     exists for the sample, return immediately with
//     [RetractResult.Inserted]=false and
//     [RetractResult.ScanRunID]=uuid.Nil -- NO new
//     `scan_run` row is opened. This is the canonical
//     "operator clicked retract twice" path; the audit
//     trail of the original scan_run is sufficient.
//  4. Open `scan_run(kind='retract', sha_binding='single',
//     status='running', to_sha=sha)`.
//  5. Append `metric_retraction(retraction_id, sample_id,
//     reason, appended_by, created_at)`. If the Append
//     races against a concurrent retract (Lookup said
//     "no row" at step 3 but Append finds the UNIQUE on
//     `sample_id` already taken), the dispatcher finalises
//     the scan_run as `succeeded` and returns the existing
//     row WITH the freshly-minted scan_run_id (honest
//     audit-trail surface): the operator's intent landed,
//     a scan_run row was actually opened on the wire, and
//     Inserted=false flags the dedupe.
//  6. Finalise `scan_run.status='succeeded'`. The
//     finalize path uses [context.WithoutCancel] so a
//     cancelled caller still completes the row
//     transition (the retraction row is already durable).
//
// Errors at any step finalise the scan_run as `failed`
// (when a scan_run was opened) before returning.
func (d *RetractDispatcher) Dispatch(ctx context.Context, req RetractRequest) (RetractResult, error) {
	if err := req.Validate(); err != nil {
		return RetractResult{}, err
	}

	repoID, sha, found, err := d.resolver.ResolveSample(ctx, req.SampleID)
	if err != nil {
		return RetractResult{}, fmt.Errorf("metric_ingestor: resolve sample %s: %w", req.SampleID, err)
	}
	if !found {
		return RetractResult{}, fmt.Errorf("%w: sample_id=%s", ErrRetractUnknownSample, req.SampleID)
	}

	// Step 3 -- SEQUENTIAL idempotency probe.
	//
	// Architecture-level idempotency at the `repo_event`
	// layer is the Management surface's responsibility
	// (it controls whether to append a new
	// `retract_intent` row); this dispatcher's job is to
	// ensure the `metric_retraction` row exists
	// regardless of how many times Dispatch is called AND
	// to avoid opening a wasted `scan_run` row when the
	// retraction already exists (the workstream brief's
	// invariant and the runbook's documented contract).
	if existing, ok, lookupErr := d.retractions.Lookup(ctx, req.SampleID); lookupErr != nil {
		return RetractResult{}, fmt.Errorf("metric_ingestor: lookup metric_retraction (sample_id=%s): %w", req.SampleID, lookupErr)
	} else if ok {
		if d.log != nil {
			d.log.Info("retract: sequential idempotent no-op",
				"component", "metric_ingestor.RetractDispatcher",
				"retraction_id", existing.RetractionID,
				"sample_id", req.SampleID,
				"repo_id", repoID,
				"sha", sha,
				"scan_run_opened", false,
			)
		}
		return RetractResult{
			Retraction: existing,
			ScanRunID:  uuid.Nil,
			Inserted:   false,
		}, nil
	}

	openedAt := d.clock()
	scanRunID, err := d.scanStore.OpenRetractScanRun(ctx, repoID, sha, openedAt)
	if err != nil {
		return RetractResult{}, fmt.Errorf("metric_ingestor: open retract scan_run (sample_id=%s): %w", req.SampleID, err)
	}

	retractionID, err := uuid.NewV4()
	if err != nil {
		// scan_run is open and we cannot mint an id --
		// finalise the run as failed before returning.
		d.finalizeOrLog(ctx, scanRunID, ScanRunStatusFailed)
		return RetractResult{ScanRunID: scanRunID}, fmt.Errorf("metric_ingestor: mint retraction_id: %w", err)
	}
	row := RetractionRow{
		RetractionID: retractionID,
		SampleID:     req.SampleID,
		Reason:       strings.TrimSpace(req.Reason),
		AppendedBy:   strings.TrimSpace(req.AppendedBy),
		CreatedAt:    openedAt,
	}
	stored, inserted, err := d.retractions.Append(ctx, row)
	if err != nil {
		d.finalizeOrLog(ctx, scanRunID, ScanRunStatusFailed)
		return RetractResult{ScanRunID: scanRunID}, fmt.Errorf("metric_ingestor: append metric_retraction (sample_id=%s): %w", req.SampleID, err)
	}

	endedAt := d.clock()
	if err := d.scanStore.FinalizeRetractScanRun(ctx, scanRunID, ScanRunStatusSucceeded, endedAt); err != nil {
		// The retraction row IS durable; we just couldn't
		// flip scan_run.status. Caller sees the row + a
		// non-nil finalize error so the sweep loop can
		// reconcile.
		return RetractResult{
			Retraction: stored,
			ScanRunID:  scanRunID,
			Inserted:   inserted,
		}, fmt.Errorf("metric_ingestor: finalize retract scan_run %s: %w", scanRunID, err)
	}

	if d.log != nil {
		level := "succeeded"
		if !inserted {
			// Race-loser path: Lookup said no, Append
			// said yes (another writer landed the row
			// between our two calls). Surface the rare
			// path so operators can correlate the
			// duplicated audit trail.
			level = "succeeded-race-loser"
		}
		d.log.Info("retract: scan_run finalised",
			"component", "metric_ingestor.RetractDispatcher",
			"scan_run_id", scanRunID,
			"retraction_id", stored.RetractionID,
			"sample_id", req.SampleID,
			"repo_id", repoID,
			"sha", sha,
			"inserted", inserted,
			"appended_by", row.AppendedBy,
			"outcome", level,
		)
	}

	return RetractResult{
		Retraction: stored,
		ScanRunID:  scanRunID,
		Inserted:   inserted,
	}, nil
}

// finalizeOrLog finalises the scan_run on the error path.
// Uses a fresh context (decoupled from ctx) with a short
// deadline so a cancelled parent still gets the
// scan_run.status transition. Logs the error -- the
// finalize failure does NOT propagate because the caller
// already has an error to report.
func (d *RetractDispatcher) finalizeOrLog(ctx context.Context, scanRunID uuid.UUID, status ScanRunStatus) {
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultFinalizeTimeout)
	defer cancel()
	if err := d.scanStore.FinalizeRetractScanRun(finalizeCtx, scanRunID, status, d.clock()); err != nil {
		if d.log != nil {
			d.log.Warn("retract: finalize-on-error failed",
				"component", "metric_ingestor.RetractDispatcher",
				"scan_run_id", scanRunID,
				"target_status", status,
				"err", err.Error(),
			)
		}
	}
}

// --- in-memory retract stores -------------------------------------------

// InMemoryRetractStore is the Stage 3.4 in-memory
// implementation of [RetractScanRunStore] +
// [RetractionStore] + [SampleResolver]. Designed for unit
// tests AND for the integration-test harness while the
// PG implementation is incubated under Phase 3.5.
//
// The store is thread-safe via an internal mutex. The
// schema's UNIQUE on `metric_retraction.sample_id` is
// emulated by a sample_id -> retraction map so a
// double-Append returns the existing row rather than a
// second insert.
type InMemoryRetractStore struct {
	mu sync.Mutex

	// scanRuns tracks the retract scan_run rows opened
	// by [OpenRetractScanRun]. Keyed by scan_run_id; the
	// in-memory record carries the values the unit tests
	// assert on (status, started_at, ended_at, kind).
	scanRuns map[uuid.UUID]*inMemoryScanRunRecord
	// retractions maps sample_id -> the persisted
	// retraction row. The UNIQUE on sample_id is
	// enforced by checking this map BEFORE inserting.
	retractions map[uuid.UUID]RetractionRow
	// samples maps sample_id -> (repo_id, sha) for
	// [SampleResolver.ResolveSample]. Tests seed this
	// via [InMemoryRetractStore.SeedSample].
	samples map[uuid.UUID]inMemorySampleLocator

	newID func() (uuid.UUID, error)
}

type inMemorySampleLocator struct {
	RepoID uuid.UUID
	SHA    string
}

// NewInMemoryRetractStore returns a fresh store with no
// seeded data.
func NewInMemoryRetractStore() *InMemoryRetractStore {
	return &InMemoryRetractStore{
		scanRuns:    make(map[uuid.UUID]*inMemoryScanRunRecord),
		retractions: make(map[uuid.UUID]RetractionRow),
		samples:     make(map[uuid.UUID]inMemorySampleLocator),
		newID:       uuid.NewV4,
	}
}

// SeedSample registers (sample_id -> repo_id, sha) so
// [ResolveSample] returns found=true. Stage 3.4 unit
// tests use this to set up the precondition without
// wiring the full Measurement-sub-store fake.
func (s *InMemoryRetractStore) SeedSample(sampleID, repoID uuid.UUID, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples[sampleID] = inMemorySampleLocator{RepoID: repoID, SHA: sha}
}

// SampleExists implements [RetractionStore].
func (s *InMemoryRetractStore) SampleExists(ctx context.Context, sampleID uuid.UUID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.samples[sampleID]
	return ok, nil
}

// ResolveSample implements [SampleResolver].
func (s *InMemoryRetractStore) ResolveSample(ctx context.Context, sampleID uuid.UUID) (uuid.UUID, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	loc, ok := s.samples[sampleID]
	if !ok {
		return uuid.Nil, "", false, nil
	}
	return loc.RepoID, loc.SHA, true, nil
}

// OpenRetractScanRun implements [RetractScanRunStore]. It
// inserts a record for the new run; the in-memory record
// is keyed by scan_run_id so [FinalizeRetractScanRun] can
// look it up.
func (s *InMemoryRetractStore) OpenRetractScanRun(ctx context.Context, repoID uuid.UUID, sha string, openedAt time.Time) (uuid.UUID, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if repoID == uuid.Nil {
		return uuid.Nil, ErrZeroRepoID
	}
	if strings.TrimSpace(sha) == "" {
		return uuid.Nil, errors.New("metric_ingestor: OpenRetractScanRun: sha is empty (CHECK sha_binding='single' AND to_sha IS NOT NULL would reject)")
	}
	if openedAt.IsZero() {
		return uuid.Nil, errors.New("metric_ingestor: OpenRetractScanRun: openedAt is the zero time")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.newID()
	if err != nil {
		return uuid.Nil, fmt.Errorf("metric_ingestor: mint retract scan_run id: %w", err)
	}
	s.scanRuns[id] = &inMemoryScanRunRecord{
		ScanRunID:  id,
		RepoID:     repoID,
		ToSHA:      sha,
		Kind:       ScanRunKindRetract,
		SHABinding: SHABindingSingle,
		Status:     ScanRunStatusRunning,
		StartedAt:  openedAt,
	}
	return id, nil
}

// FinalizeRetractScanRun implements [RetractScanRunStore].
func (s *InMemoryRetractStore) FinalizeRetractScanRun(ctx context.Context, scanRunID uuid.UUID, status ScanRunStatus, endedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if status == ScanRunStatusRunning {
		return fmt.Errorf("metric_ingestor: FinalizeRetractScanRun rejects running terminal: %w", ErrUnknownScanRunStatus)
	}
	if err := ValidateScanRunStatus(status); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.scanRuns[scanRunID]
	if !ok {
		return fmt.Errorf("%w: scan_run_id=%s", ErrUnknownScanRunID, scanRunID)
	}
	if rec.Status != ScanRunStatusRunning {
		return fmt.Errorf("%w: scan_run_id=%s current_status=%s", ErrClaimedRunNotInProgress, scanRunID, rec.Status)
	}
	rec.Status = status
	rec.EndedAt = endedAt
	return nil
}

// Lookup implements [RetractionStore]. Returns the
// EXISTING retraction row keyed by `sample_id` if any.
func (s *InMemoryRetractStore) Lookup(ctx context.Context, sampleID uuid.UUID) (RetractionRow, bool, error) {
	if err := ctx.Err(); err != nil {
		return RetractionRow{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.retractions[sampleID]
	if !ok {
		return RetractionRow{}, false, nil
	}
	return r, true, nil
}

// Append implements [RetractionStore]. Emulates the
// UNIQUE constraint on `sample_id`: a second Append for
// the same sample_id returns the existing row
// (inserted=false). This is the dispatcher's race-free
// safety net.
func (s *InMemoryRetractStore) Append(ctx context.Context, row RetractionRow) (RetractionRow, bool, error) {
	if err := ctx.Err(); err != nil {
		return RetractionRow{}, false, err
	}
	if row.RetractionID == uuid.Nil {
		return RetractionRow{}, false, errors.New("metric_ingestor: RetractionStore.Append: RetractionID is zero (caller must mint)")
	}
	if row.SampleID == uuid.Nil {
		return RetractionRow{}, false, ErrRetractZeroSampleID
	}
	if strings.TrimSpace(row.Reason) == "" {
		return RetractionRow{}, false, ErrRetractEmptyReason
	}
	if strings.TrimSpace(row.AppendedBy) == "" {
		return RetractionRow{}, false, ErrRetractEmptyAppendedBy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.retractions[row.SampleID]; ok {
		return existing, false, nil
	}
	s.retractions[row.SampleID] = row
	return row, true, nil
}

// RetractionFor returns the persisted retraction row for
// the given sample_id, or (zero, false) if no retraction
// exists. Exposed for test assertions.
func (s *InMemoryRetractStore) RetractionFor(sampleID uuid.UUID) (RetractionRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.retractions[sampleID]
	return r, ok
}

// ScanRunRecord returns a SNAPSHOT of the retract scan_run
// row with the given id, or (zero, false) if unknown.
// Exposed for test assertions; the snapshot is a value
// copy so mutating it does not affect the store.
func (s *InMemoryRetractStore) ScanRunRecord(id uuid.UUID) (inMemoryScanRunRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.scanRuns[id]
	if !ok {
		return inMemoryScanRunRecord{}, false
	}
	return *rec, true
}

// CountScanRuns returns the number of retract scan_run
// rows the store has opened. Exposed for tests that
// assert idempotency at the scan_run layer.
func (s *InMemoryRetractStore) CountScanRuns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.scanRuns)
}

// CountRetractions returns the number of persisted
// retraction rows. Exposed for tests that assert
// idempotency at the `metric_retraction` layer.
func (s *InMemoryRetractStore) CountRetractions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.retractions)
}

// Compile-time interface guards. A future refactor that
// breaks any of the three contracts will fail to compile
// here rather than only at the dispatcher's call site.
var (
	_ RetractScanRunStore = (*InMemoryRetractStore)(nil)
	_ RetractionStore     = (*InMemoryRetractStore)(nil)
	_ SampleResolver      = (*InMemoryRetractStore)(nil)
)

// Sentinel so the package can still reference
// repo_indexer's transition gate from this file if a
// future refactor decides to write `commit.scan_status`
// from the retract path. Today the retract scan_run does
// NOT touch any commit row -- the four-state Commit
// machine (pending/scanning/scanned/failed) has no edge
// for a retract intent, by design.
var _ = repo_indexer.ScanStatusPending
