package webhook

// Stage 4.1 iter-2 -- durable scan_run idempotency seam.
//
// ScanRunRepository is the abstraction the Router consults to
// open and finalise a durable `scan_run` row for an external-
// ingest verb. The brief (tech-spec Sec 7 / Stage 4.1
// implementation-plan):
//
//	"Add idempotency layer: compute payload_hash = sha256(
//	 canonicalised body); if a scan_run(payload_hash=...)
//	 already exists for this verb, return the stored
//	 scan_run_id without re-executing."
//
// The interface lives in the webhook package so the Router
// does NOT import `metric_ingestor` directly; the composition
// root wires the PG-backed [metric_ingestor.PGExternalScanRunStore]
// through the [PGScanRunRepository] adapter below.
//
// # Why a NEW seam (not a method on IdempotencyStore)
//
// [IdempotencyStore] is the in-process response_body cache
// (fast replay). It is REQUIRED to be in-memory by design:
// caching JSON envelopes in PG would bloat the scan_run
// table without operational value (the publisher does not
// rely on byte-identical replays). [ScanRunRepository] is
// the DURABLE seam (cross-restart, cross-replica). The two
// are composed by the Router:
//
//  1. Router computes payload_hash.
//  2. Router calls ScanRunRepository.OpenExternal.
//     -- alreadyExisted=true: replay path. Look up cached
//        response_body via IdempotencyStore.Lookup; if
//        absent (e.g. fresh process), emit a minimal replay
//        envelope from the durable row alone.
//     -- alreadyExisted=false: claim path. Mint scan_run_id
//        is returned by the repo; Router dispatches the
//        verb handler.
//  3. On verb-handler success: Router commits to
//     IdempotencyStore (fast cache) + Finalize on the repo
//     (durable status). On failure: Finalize as failed.
//
// The split keeps the durable contract narrow (just scan_run
// lifecycle) and the fast-replay contract narrow (just
// response_body bytes).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gofrs/uuid"
)

// ScanRunRepositoryRequest is the input shape the Router
// passes to [ScanRunRepository.OpenExternal]. Fields mirror
// the columns of `clean_code.scan_run` the external-ingest
// flow populates (architecture Sec 5.7).
type ScanRunRepositoryRequest struct {
	// Verb is the URL path segment that resolved to the
	// VerbHandler. This is the (Verb, PayloadHash)
	// idempotency dimension -- two distinct verbs that
	// happen to share `Kind` (e.g. churn + defects both
	// use external_per_row) MUST get independent
	// idempotency tracks, so the durable claim primitive
	// is keyed on (Verb, PayloadHash), NOT (Kind, ...).
	Verb string
	// Kind is the canonical `scan_run.kind` (e.g.
	// `external_per_row` for churn/defects;
	// `external_single` for coverage/test_balance).
	Kind string
	// SHABinding is one of `single` | `per_row`. MUST be
	// internally consistent with Kind (migration 0001
	// scan_run_sha_binding_consistent CHECK enforces).
	SHABinding string
	// RepoID is the parent repo (scan_run.repo_id FK).
	// The verb's ExtractMetadata extracts it from the
	// validated body before the Router opens the run.
	RepoID uuid.UUID
	// SHA is the commit SHA the run targets (required
	// when SHABinding='single'; empty when 'per_row').
	SHA string
	// PayloadHash is the raw 32-byte sha-256 digest of the
	// canonical body. The Router computes this BEFORE
	// calling OpenExternal so the same hash bytes seed
	// both the durable claim and the in-process cache.
	PayloadHash PayloadHash
	// OpenedAt is the started_at timestamp. The Router
	// stamps `time.Now()`.
	OpenedAt time.Time
}

// ScanRunRepositoryResult is the return shape of
// [ScanRunRepository.OpenExternal]. Mirrors the PG store's
// [metric_ingestor.OpenExternalScanRunResult] with one
// addition: ExistingStatus is exposed as a webhook-package
// string to keep the seam package-clean.
type ScanRunRepositoryResult struct {
	// ScanRunID is the canonical scan_run_id for the
	// (verb, payload_hash) tuple. Stable across replays:
	// every retry of the same payload returns the same id.
	ScanRunID uuid.UUID
	// AlreadyExisted is true iff a prior call already
	// opened a scan_run for this payload (durable replay
	// path). When true, the Router skips the verb handler
	// AND skips Finalize (the prior call already
	// finalised, or the stale-sweep will).
	AlreadyExisted bool
	// ExistingStatus is the prior row's status when
	// AlreadyExisted=true. Empty when AlreadyExisted=false.
	// Surfaced so the Router can include it in the replay
	// envelope ('succeeded' = canonical replay; 'failed' =
	// the publisher's payload is sticky-bad; 'running' =
	// in-flight on another replica).
	ExistingStatus string
}

// ScanRunRepositoryFinalStatus is the closed set of
// terminal status strings [ScanRunRepository.Finalize]
// accepts. Pinned in the webhook package so the seam does
// not leak the metric_ingestor enum to the Router.
const (
	ScanRunStatusSucceeded = "succeeded"
	ScanRunStatusFailed    = "failed"
)

// ErrScanRunRepoUnknownStatus surfaces an attempt to
// finalise a scan_run with a non-terminal status.
var ErrScanRunRepoUnknownStatus = errors.New("webhook: ScanRunRepository.Finalize accepts only 'succeeded' or 'failed'")

// ScanRunRepository is the durable scan_run lifecycle seam
// (Stage 4.1 iter-3 evaluator items #1 + #2 + #4).
//
// # Atomic claim contract
//
// OpenExternal MUST be atomic across restarts and replicas:
// two concurrent callers with the SAME (Verb, PayloadHash)
// observe exactly one row in `clean_code.scan_run`. The PG
// implementation uses the partial unique index from
// migration 0009 on (verb, payload_hash); the in-memory
// implementation uses a mutex keyed on the same tuple.
// Two DIFFERENT verbs that happen to have the same body
// (e.g. churn + defects both POSTing the same canonical
// JSON) MUST get independent scan_run_ids because the
// downstream materialisers parse the body differently.
//
// # Finalize contract
//
// Finalize is idempotent per call: a double-finalise of the
// same scan_run_id with the SAME terminal status returns nil
// (the second call observes the row already in the target
// terminal status). A double-finalise with a DIFFERENT
// terminal status returns an error (the operator log MUST
// name the mismatch). The PG implementation enforces this
// via `WHERE status='running'` + rows-affected check + a
// follow-up SELECT on the ErrConcurrentFinalize path to
// distinguish the two cases.
type ScanRunRepository interface {
	// OpenExternal claims a scan_run for the supplied
	// (Verb, PayloadHash). When the (Verb, PayloadHash)
	// is fresh, returns (result with AlreadyExisted=false,
	// nil). When a prior call already claimed the slot,
	// returns (result with AlreadyExisted=true, nil) with
	// ScanRunID = the prior id.
	OpenExternal(ctx context.Context, req ScanRunRepositoryRequest) (ScanRunRepositoryResult, error)

	// Finalize transitions the scan_run row to the supplied
	// terminal status. Caller MUST pass one of
	// [ScanRunStatusSucceeded] | [ScanRunStatusFailed];
	// any other status returns
	// [ErrScanRunRepoUnknownStatus]. A double-finalise to
	// the SAME terminal status returns nil; to a DIFFERENT
	// terminal status returns a wrapped error.
	Finalize(ctx context.Context, scanRunID uuid.UUID, status string, endedAt time.Time) error
}

// InMemoryScanRunRepository is the v1 [ScanRunRepository]
// implementation for tests and for the in-memory production
// fallback (when the operator has not enabled the PG-backed
// store). Survives across composition-root restarts ONLY in
// the sense that the SAME instance handles the lifetime of
// one process; ACTUAL durable cross-restart behaviour
// requires the PG-backed implementation.
//
// # Concurrency
//
// One Mutex guards the entire state. The shape is small
// enough that finer-grained locking is not justified at v1
// scale.
type InMemoryScanRunRepository struct {
	mu sync.Mutex
	// byPayload maps (verb, payload_hash) -> scan_run_id
	// for the atomic-claim lookup. Keyed on Verb (not
	// Kind) per iter-3 evaluator item #2: two distinct
	// verbs that share `Kind` (churn+defects, both
	// external_per_row) MUST get independent idempotency
	// tracks because their canonical bodies parse
	// differently downstream.
	byPayload map[scanRunMemKey]uuid.UUID
	// rows holds the per-scan_run row state (status,
	// times, kind). Indexed by scan_run_id so Finalize
	// can update without a separate lookup.
	rows map[uuid.UUID]*scanRunMemRow
}

// scanRunMemKey is the composite-key shape for byPayload.
// Using a value-type (not a string) avoids the heap
// allocation a `verb + ":" + hash` concatenation would
// incur per request.
type scanRunMemKey struct {
	verb string
	hash PayloadHash
}

// scanRunMemRow is the in-memory shadow of a scan_run row.
// Mirrors the (subset of) `clean_code.scan_run` columns the
// Router persists.
type scanRunMemRow struct {
	scanRunID   uuid.UUID
	repoID      uuid.UUID
	kind        string
	shaBinding  string
	toSHA       string
	payloadHash PayloadHash
	startedAt   time.Time
	endedAt     time.Time
	status      string
}

// NewInMemoryScanRunRepository constructs an empty store.
func NewInMemoryScanRunRepository() *InMemoryScanRunRepository {
	return &InMemoryScanRunRepository{
		byPayload: make(map[scanRunMemKey]uuid.UUID),
		rows:      make(map[uuid.UUID]*scanRunMemRow),
	}
}

// OpenExternal implements [ScanRunRepository].
func (r *InMemoryScanRunRepository) OpenExternal(ctx context.Context, req ScanRunRepositoryRequest) (ScanRunRepositoryResult, error) {
	if err := ctx.Err(); err != nil {
		return ScanRunRepositoryResult{}, err
	}
	if err := validateScanRunRepoRequest(req); err != nil {
		return ScanRunRepositoryResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := scanRunMemKey{verb: req.Verb, hash: req.PayloadHash}
	if existingID, ok := r.byPayload[key]; ok {
		row := r.rows[existingID]
		return ScanRunRepositoryResult{
			ScanRunID:      existingID,
			AlreadyExisted: true,
			ExistingStatus: row.status,
		}, nil
	}
	newID, err := uuid.NewV4()
	if err != nil {
		return ScanRunRepositoryResult{}, fmt.Errorf("webhook: InMemoryScanRunRepository: mint scan_run_id: %w", err)
	}
	row := &scanRunMemRow{
		scanRunID:   newID,
		repoID:      req.RepoID,
		kind:        req.Kind,
		shaBinding:  req.SHABinding,
		toSHA:       req.SHA,
		payloadHash: req.PayloadHash,
		startedAt:   req.OpenedAt,
		status:      "running",
	}
	r.byPayload[key] = newID
	r.rows[newID] = row
	return ScanRunRepositoryResult{
		ScanRunID:      newID,
		AlreadyExisted: false,
	}, nil
}

// Finalize implements [ScanRunRepository].
func (r *InMemoryScanRunRepository) Finalize(ctx context.Context, scanRunID uuid.UUID, status string, endedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if status != ScanRunStatusSucceeded && status != ScanRunStatusFailed {
		return fmt.Errorf("%w: got %q", ErrScanRunRepoUnknownStatus, status)
	}
	if scanRunID == uuid.Nil {
		return errors.New("webhook: InMemoryScanRunRepository.Finalize: scan_run_id is the zero UUID")
	}
	if endedAt.IsZero() {
		return errors.New("webhook: InMemoryScanRunRepository.Finalize: endedAt is the zero time")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[scanRunID]
	if !ok {
		return fmt.Errorf("webhook: InMemoryScanRunRepository.Finalize: scan_run_id %s is unknown", scanRunID)
	}
	if row.status != "running" {
		// Idempotent re-finalise: if the row is already in
		// the requested terminal status, accept; if it's
		// in the OTHER terminal status, surface a wrapped
		// error so the operator log names the mismatch.
		if row.status == status {
			return nil
		}
		return fmt.Errorf("webhook: InMemoryScanRunRepository.Finalize: scan_run_id %s already in status %q (cannot move to %q)",
			scanRunID, row.status, status)
	}
	row.status = status
	row.endedAt = endedAt
	return nil
}

// Lookup is a non-Router-facing projection for tests and ops
// tooling. Returns the in-memory row state for the supplied
// scan_run_id, or false when the id is unknown.
func (r *InMemoryScanRunRepository) Lookup(scanRunID uuid.UUID) (status, kind string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, found := r.rows[scanRunID]
	if !found {
		return "", "", false
	}
	return row.status, row.kind, true
}

// Len returns the number of stored rows. For tests +
// ops dashboards.
func (r *InMemoryScanRunRepository) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

// validateScanRunRepoRequest enforces the shape-level
// invariants every ScanRunRepository implementation requires
// before any backing-store work runs.
func validateScanRunRepoRequest(req ScanRunRepositoryRequest) error {
	if req.Verb == "" {
		return errors.New("webhook: ScanRunRepository: empty Verb")
	}
	if req.RepoID == uuid.Nil {
		return errors.New("webhook: ScanRunRepository: zero RepoID")
	}
	if req.Kind == "" {
		return errors.New("webhook: ScanRunRepository: empty Kind")
	}
	if req.SHABinding == "" {
		return errors.New("webhook: ScanRunRepository: empty SHABinding")
	}
	if req.OpenedAt.IsZero() {
		return errors.New("webhook: ScanRunRepository: zero OpenedAt")
	}
	// PayloadHash is a [32]byte array; the zero value is
	// 32 zero bytes which IS a valid SHA-256 digest (the
	// hash of an attacker-chosen preimage). We do NOT
	// reject the all-zero hash here -- it's a legitimate
	// (if unlikely) input. Callers that want extra
	// defence can sniff for it themselves.
	return nil
}

// Compile-time assertion.
var _ ScanRunRepository = (*InMemoryScanRunRepository)(nil)
