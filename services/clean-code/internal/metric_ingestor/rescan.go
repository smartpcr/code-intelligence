package metric_ingestor

// Stage 3.4 -- rescan enqueuer.
//
// `mgmt.rescan(repo_id, sha)` is an operator-initiated
// re-run of the foundation-tier scan at a SPECIFIC SHA. The
// architecture canonical RepoEvent.kind enum at Sec 5.1.4
// has NO `rescan_intent` value -- the four past-tense kinds
// are `registered`, `retired`, `retract_intent`,
// `mode_changed`. The rescan verb is therefore a
// service-internal request (NOT a RepoEvent), per the
// workstream brief verbatim:
//
//   "emit a service-internal rescan request (no canonical
//    RepoEvent kind exists for rescan per architecture
//    Sec 5.1.4) and enqueue a scan_run(kind='full') for
//    the given SHA via the Metric Ingestor."
//
// The dispatcher OPENS a `scan_run(kind='full',
// sha_binding='single', status='running')` for the given
// (repo_id, sha) and hands back the freshly-minted
// scan_run_id. The foundation-tier scanner consumes the
// pending run via the standard state-machine path; this
// rescan path is purely the "enqueue" half of the work.
//
// # Why "enqueue" rather than "execute"
//
// The foundation-tier state machine drains its queue by
// claiming the OLDEST pending commit and running the AST
// recipe loop synchronously. The Stage 3.4 rescan verb
// must NOT block on that synchronous execution -- the
// operator wants a fast "we've queued your rescan"
// response. So the dispatcher opens the scan_run row in
// `running` state but does NOT invoke the recipe loop;
// the in-process [StateMachine] or a future background
// worker picks it up via the canonical path.
//
// In Stage 3.4's in-memory wiring, the rescan verb opens
// the scan_run and immediately marks it `running` so the
// caller can observe the row; the test fixtures finalise
// it manually (or the production composition root wires
// a worker that drains the pending queue and finalises).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"
)

// Sentinel errors emitted by the rescan path.
var (
	// ErrRescanZeroRepoID is returned when a
	// [RescanRequest] carries the zero UUID for `RepoID`.
	ErrRescanZeroRepoID = errors.New("metric_ingestor: RescanRequest.RepoID is the zero UUID")
	// ErrRescanEmptySHA is returned when the `SHA` field
	// is empty or whitespace-only. The DB CHECK
	// constraint pins `sha_binding='single' AND to_sha
	// IS NOT NULL`; the rescan binds to one SHA.
	ErrRescanEmptySHA = errors.New("metric_ingestor: RescanRequest.SHA is empty")
	// ErrRescanEmptyRequestedBy is returned when the
	// audit attribution is empty or whitespace-only.
	// Production callers stamp this with
	// `operator:<oidc-subject>`; the audit log carries
	// the value so operators can attribute the rescan
	// even though no `repo_event` row records it.
	ErrRescanEmptyRequestedBy = errors.New("metric_ingestor: RescanRequest.RequestedBy is empty")
)

// RescanRequest is the per-call input to
// [RescanEnqueuer.Enqueue].
type RescanRequest struct {
	// RepoID is the `clean_code.repo.repo_id` to rescan.
	// Must be non-zero.
	RepoID uuid.UUID
	// SHA is the specific commit SHA to rescan. Must be
	// non-empty (CHECK constraint parity).
	SHA string
	// RequestedBy is the audit attribution for the
	// rescan request. Stamped on the structured log line
	// AND on [RescanResult.RequestedBy] for the wire
	// response. Production callers pass
	// `"operator:<oidc-subject>"`.
	RequestedBy string
}

// Validate runs the cheap field validations and returns
// the first failure.
func (r RescanRequest) Validate() error {
	if r.RepoID == uuid.Nil {
		return ErrRescanZeroRepoID
	}
	if strings.TrimSpace(r.SHA) == "" {
		return ErrRescanEmptySHA
	}
	if strings.TrimSpace(r.RequestedBy) == "" {
		return ErrRescanEmptyRequestedBy
	}
	return nil
}

// RescanScanRunStore is the narrow seam the
// [RescanEnqueuer] uses to open the
// `scan_run(kind='full', ...)` row. The interface lives
// here (not on [ScanRunStore]) so the rescan path stays
// decoupled from the claim/finalize flow the state
// machine drives -- the rescan enqueuer ONLY opens the
// row; the state machine finalises it once the recipe
// loop completes.
//
// The interface is intentionally MINIMAL -- it surfaces
// just the open verb. Finalize remains under the state
// machine path so the foundation-tier scan owns the
// terminal transition.
type RescanScanRunStore interface {
	// OpenRescanRun INSERTs a `scan_run(kind='full',
	// sha_binding='single', status='running',
	// to_sha=$sha, repo_id=$repo)` row and returns its
	// `scan_run_id`. The state machine's claim path
	// will later observe the row and drive it to
	// terminal status when the recipe loop completes.
	OpenRescanRun(ctx context.Context, repoID uuid.UUID, sha string, openedAt time.Time) (uuid.UUID, error)
}

// RescanEnqueuer is the Metric-Ingestor-side handler for
// `mgmt.rescan`. The Management surface calls
// [Enqueue]; the enqueuer opens the scan_run row and
// returns its id.
//
// Construct via [NewRescanEnqueuer]. The enqueuer is
// stateless past its dependencies.
type RescanEnqueuer struct {
	store RescanScanRunStore
	clock func() time.Time
	log   *slog.Logger
}

// RescanEnqueuerOption configures a [RescanEnqueuer] at
// construction time.
type RescanEnqueuerOption func(*RescanEnqueuer)

// WithRescanEnqueuerClock overrides the clock used for
// `scan_run.started_at`. Default is [time.Now].
func WithRescanEnqueuerClock(now func() time.Time) RescanEnqueuerOption {
	return func(e *RescanEnqueuer) {
		e.clock = now
	}
}

// WithRescanEnqueuerLogger overrides the logger. nil
// disables logging.
func WithRescanEnqueuerLogger(log *slog.Logger) RescanEnqueuerOption {
	return func(e *RescanEnqueuer) {
		e.log = log
	}
}

// NewRescanEnqueuer returns a wired [RescanEnqueuer].
// PANICS when `store` is nil.
func NewRescanEnqueuer(store RescanScanRunStore, opts ...RescanEnqueuerOption) *RescanEnqueuer {
	if store == nil {
		panic("metric_ingestor: NewRescanEnqueuer received nil RescanScanRunStore")
	}
	e := &RescanEnqueuer{
		store: store,
		clock: time.Now,
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.clock == nil {
		e.clock = time.Now
	}
	return e
}

// RescanResult is the structured outcome of one
// [RescanEnqueuer.Enqueue] call.
type RescanResult struct {
	// ScanRunID is the freshly-minted scan_run_id. The
	// row is in `running` state -- the foundation-tier
	// state machine finalises it once the recipe loop
	// completes.
	ScanRunID uuid.UUID
	// RepoID echoes back [RescanRequest.RepoID] for the
	// HTTP layer's response body.
	RepoID uuid.UUID
	// SHA echoes back [RescanRequest.SHA].
	SHA string
	// RequestedBy echoes back [RescanRequest.RequestedBy]
	// so the wire response carries the audit attribution
	// (no `repo_event` row records the rescan per
	// architecture Sec 5.1.4 -- the audit trail lives in
	// the structured log AND the scan_run row).
	RequestedBy string
	// OpenedAt is the timestamp stamped on the
	// `scan_run.started_at` column.
	OpenedAt time.Time
}

// Enqueue opens the scan_run row for the rescan and
// returns its id. The row is in `running` state; the
// caller does NOT need to wait for the recipe loop to
// complete -- the response is the operator's "your
// rescan is queued" signal.
func (e *RescanEnqueuer) Enqueue(ctx context.Context, req RescanRequest) (RescanResult, error) {
	if err := req.Validate(); err != nil {
		return RescanResult{}, err
	}
	openedAt := e.clock()
	id, err := e.store.OpenRescanRun(ctx, req.RepoID, strings.TrimSpace(req.SHA), openedAt)
	if err != nil {
		return RescanResult{}, fmt.Errorf("metric_ingestor: open rescan scan_run (repo_id=%s sha=%s): %w", req.RepoID, req.SHA, err)
	}
	if e.log != nil {
		e.log.Info("rescan: scan_run enqueued",
			"component", "metric_ingestor.RescanEnqueuer",
			"scan_run_id", id,
			"repo_id", req.RepoID,
			"sha", req.SHA,
			"requested_by", strings.TrimSpace(req.RequestedBy),
		)
	}
	return RescanResult{
		ScanRunID:   id,
		RepoID:      req.RepoID,
		SHA:         strings.TrimSpace(req.SHA),
		RequestedBy: strings.TrimSpace(req.RequestedBy),
		OpenedAt:    openedAt,
	}, nil
}

// --- in-memory rescan store ------------------------------------------------

// InMemoryRescanStore is the Stage 3.4 in-memory
// implementation of [RescanScanRunStore]. Designed for
// unit + integration tests while the PG implementation
// is incubated under Phase 3.5.
type InMemoryRescanStore struct {
	mu sync.Mutex

	// runs tracks every rescan scan_run row opened by
	// [OpenRescanRun]. Keyed by scan_run_id; the
	// in-memory record carries the values tests assert
	// on.
	runs map[uuid.UUID]*inMemoryScanRunRecord

	newID func() (uuid.UUID, error)
}

// NewInMemoryRescanStore returns a fresh store with no
// seeded runs.
func NewInMemoryRescanStore() *InMemoryRescanStore {
	return &InMemoryRescanStore{
		runs:  make(map[uuid.UUID]*inMemoryScanRunRecord),
		newID: uuid.NewV4,
	}
}

// OpenRescanRun implements [RescanScanRunStore]. Mints a
// fresh scan_run_id and records the row in `running`
// state with `kind='full'` and `sha_binding='single'`.
func (s *InMemoryRescanStore) OpenRescanRun(ctx context.Context, repoID uuid.UUID, sha string, openedAt time.Time) (uuid.UUID, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if repoID == uuid.Nil {
		return uuid.Nil, ErrRescanZeroRepoID
	}
	if strings.TrimSpace(sha) == "" {
		return uuid.Nil, ErrRescanEmptySHA
	}
	if openedAt.IsZero() {
		return uuid.Nil, errors.New("metric_ingestor: OpenRescanRun: openedAt is the zero time")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.newID()
	if err != nil {
		return uuid.Nil, fmt.Errorf("metric_ingestor: mint rescan scan_run id: %w", err)
	}
	s.runs[id] = &inMemoryScanRunRecord{
		ScanRunID:  id,
		RepoID:     repoID,
		ToSHA:      sha,
		Kind:       ScanRunKindFull,
		SHABinding: SHABindingSingle,
		Status:     ScanRunStatusRunning,
		StartedAt:  openedAt,
	}
	return id, nil
}

// ScanRunRecord returns a SNAPSHOT of the rescan scan_run
// row with the given id, or (zero, false) if unknown.
// Exposed for test assertions.
func (s *InMemoryRescanStore) ScanRunRecord(id uuid.UUID) (inMemoryScanRunRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.runs[id]
	if !ok {
		return inMemoryScanRunRecord{}, false
	}
	return *rec, true
}

// CountRuns returns the number of rescan scan_run rows
// the store has opened. Exposed for tests that assert
// the enqueuer wrote exactly one row per call.
func (s *InMemoryRescanStore) CountRuns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// Compile-time interface guard.
var _ RescanScanRunStore = (*InMemoryRescanStore)(nil)
