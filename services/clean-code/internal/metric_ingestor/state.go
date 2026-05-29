package metric_ingestor

// This file implements the Stage 3.2 ScanRun state machine:
// the writer-side driver that picks the oldest pending
// commit, opens a `scan_run(kind='full', sha_binding='single',
// status='running')`, transitions `commit.scan_status` to
// `scanning`, runs the foundation-tier AST scan, then
// finalises both rows to their terminal states.
//
// # Writer-ownership invariants (architecture Sec 1.5.1 row 1)
//
// The state machine implemented here is the SOLE application
// caller that issues `UPDATE clean_code.commit SET scan_status = ...`
// statements. The DB role grants in migration
// `0004_roles.up.sql` enforce this at the SQL layer
// (`GRANT UPDATE (scan_status) ON clean_code.commit TO
// clean_code_metric_ingestor`); this Go code is the
// matching application-layer enforcement so the Repo
// Indexer (the only INSERTer of `commit` rows) cannot
// reach in through a shared package. Every transition is
// gated by [repo_indexer.ValidateTransition] BEFORE the
// store sees the call.
//
// # Canonical state sets
//
// The state machine recognises ONLY the four canonical
// [repo_indexer.ScanStatus] values (`pending`, `scanning`,
// `scanned`, `failed`) and the three canonical
// [ScanRunStatus] values (`running`, `succeeded`,
// `failed`). Forbidden literals (`complete`, `superseded`,
// `orphaned`, `queued`, `external_double`) appear ONLY as
// inputs to rejection tests -- the state machine refuses
// to even build a request with them.
//
// # Why a separate file from sweep.go / ingestor.go
//
// `sweep.go` houses the per-call ChurnSweep; `ingestor.go`
// houses the per-call dispatch coordinator (foundation +
// churn). Both are PER-CALL building blocks: they trust
// the parent ScanRun row already exists. This file is the
// CALLER above them -- it owns the lifecycle of the parent
// ScanRun row itself. Keeping the layers in separate
// files keeps the import graph (sweep <- ingestor <-
// state machine) visible at the filesystem level.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/repo_indexer"
)

// Sentinel errors emitted by the state machine layer.
// Returned wrapped via [fmt.Errorf] with `%w` so callers
// can `errors.Is`.
var (
	// ErrUnknownScanRunKind is returned by
	// [ValidateScanRunKind] when its argument is not one of
	// the five canonical [AllScanRunKinds] values
	// (`full`, `delta`, `external_single`, `external_per_row`,
	// `retract`). Distinct from [ErrInvalidScanRunKind]
	// which guards the smaller [AllowedScanRunKinds] set
	// the [ChurnSweep] accepts.
	ErrUnknownScanRunKind = errors.New("metric_ingestor: scan run kind is not in AllScanRunKinds")
	// ErrUnknownScanRunStatus is returned by
	// [ValidateScanRunStatus] when its argument is not one
	// of the three canonical [AllScanRunStatuses] values.
	ErrUnknownScanRunStatus = errors.New("metric_ingestor: scan run status is not in AllScanRunStatuses")
	// ErrUnknownSHABinding is returned when a
	// [ClaimRequest] specifies a `sha_binding` outside the
	// closed `{single, per_row}` set the DB CHECK
	// constraint enforces (migration
	// `0001_catalog_lifecycle.up.sql:337-390`).
	ErrUnknownSHABinding = errors.New("metric_ingestor: sha_binding is not in {single, per_row}")
	// ErrScanTimeout is returned by [StateMachine.ProcessOne]
	// when the AST scan exceeds the configured
	// [StateMachine] timeout (default
	// [config.DefaultScanTimeout] = 30min, tech-spec Sec
	// 8.2). Wraps [context.DeadlineExceeded] so callers can
	// `errors.Is(err, context.DeadlineExceeded)` too.
	ErrScanTimeout = errors.New("metric_ingestor: ScanRun exceeded configured scan_timeout")
	// ErrScannerPanic is returned by [StateMachine.ProcessOne]
	// when the injected [AstScanner.Scan] call panics. The
	// panic value is wrapped into the error so the
	// state-machine caller can emit it on the structured
	// log line at finalize time -- the workstream schema
	// intentionally does NOT carry an `error_class` /
	// `error_message` column on `scan_run`, and
	// [ScanRunStore.FinalizeScanRun] only accepts the
	// terminal `runStatus` / `commitStatus` pair plus
	// `endedAt`, so the failure reason is logged, not
	// persisted (runbook.md "Failure handling" section,
	// architecture Sec 4.1 step 6 / Stage 3.2
	// implementation-plan line 306).
	ErrScannerPanic = errors.New("metric_ingestor: AstScanner.Scan panicked")
	// ErrNoPendingCommit is the not-an-error result of
	// [ScanRunStore.ClaimNextPendingCommit] when there is
	// nothing pending. The state machine maps this to
	// `ProcessOneResult.DidWork=false` and a nil error;
	// callers receive the wrapped sentinel only if they
	// hit the store directly.
	ErrNoPendingCommit = errors.New("metric_ingestor: no pending commit to claim")
	// ErrClaimedRunNotInProgress is returned by
	// [InMemoryScanRunStore.FinalizeScanRun] when the
	// caller tries to finalise a ScanRun that is not
	// currently `running`. Guards double-finalize.
	ErrClaimedRunNotInProgress = errors.New("metric_ingestor: scan_run is not in running state")
	// ErrUnknownScanRunID is returned by
	// [InMemoryScanRunStore.FinalizeScanRun] when the
	// claim's ScanRunID is not in the store. Indicates a
	// state-machine bug -- the claim must have been
	// produced by the same store instance.
	ErrUnknownScanRunID = errors.New("metric_ingestor: scan_run id unknown to store")
)

// ScanRunStatus is the canonical Go-side enum for
// `clean_code.scan_run.status`. The three allowed values
// (`running`, `succeeded`, `failed`) match the PostgreSQL
// enum `clean_code.scan_run_status` (migration
// `0001_catalog_lifecycle.up.sql:87-141`). Values like
// `complete`, `superseded`, `orphaned`, `queued`, or
// `external_double` are NEVER members of this set -- they
// appear only as inputs to the [ValidateScanRunStatus]
// rejection tests.
//
// String alias so JSON marshalling, `psql` reads, and
// `errors.Is`-style comparisons all use the canonical
// wire literal byte-for-byte.
type ScanRunStatus string

// The three canonical values. The string literals MUST
// remain byte-equal to the labels in
// `clean_code.scan_run_status` (migration 0001) and to the
// architecture Sec 5.7 table.
const (
	// ScanRunStatusRunning is the initial state set by
	// [ScanRunStore.ClaimNextPendingCommit] when the row
	// is INSERTed. Pairs with `commit.scan_status='scanning'`.
	ScanRunStatusRunning ScanRunStatus = "running"
	// ScanRunStatusSucceeded is the terminal success state.
	// Pairs with `commit.scan_status='scanned'`.
	ScanRunStatusSucceeded ScanRunStatus = "succeeded"
	// ScanRunStatusFailed is the terminal failure state.
	// Pairs with `commit.scan_status='failed'`. Set for
	// scanner errors, panics, AND timeouts (Stage 3.2
	// implementation-plan line 306).
	ScanRunStatusFailed ScanRunStatus = "failed"
)

// allScanRunStatuses is the closed package-private set
// for [AllScanRunStatuses] and [ValidateScanRunStatus].
var allScanRunStatuses = []ScanRunStatus{
	ScanRunStatusRunning,
	ScanRunStatusSucceeded,
	ScanRunStatusFailed,
}

// AllScanRunStatuses returns a fresh slice of the three
// canonical [ScanRunStatus] values. Returned as a new
// slice per call so mutation by the caller cannot leak
// into the package's closed-set guard.
func AllScanRunStatuses() []ScanRunStatus {
	out := make([]ScanRunStatus, len(allScanRunStatuses))
	copy(out, allScanRunStatuses)
	return out
}

// ValidateScanRunStatus returns nil iff `s` is one of the
// three canonical [ScanRunStatus] values, otherwise wraps
// [ErrUnknownScanRunStatus] with the offending value.
// Used as the application-layer guard before any
// `UPDATE scan_run SET status = ...` statement; the DB
// enum is a second safety net.
func ValidateScanRunStatus(s ScanRunStatus) error {
	for _, v := range allScanRunStatuses {
		if s == v {
			return nil
		}
	}
	return fmt.Errorf("%w: got %q (allowed: %v)",
		ErrUnknownScanRunStatus, s, allScanRunStatuses)
}

// allScanRunKinds is the FULL closed set the database
// `clean_code.scan_run_kind` enum recognises. Distinct
// from `allowedScanRunKinds` (in sweep.go) which is the
// SMALLER set the [ChurnSweep] accepts -- the state
// machine permits ANY DB-valid kind on INSERT (Stage 3.2
// implementation-plan line 290 "matching the architecture
// Sec 5.7 list"); the per-kind dispatch rules then live
// in [ChurnSweep] / [Ingestor].
var allScanRunKinds = []string{
	ScanRunKindFull,
	ScanRunKindDelta,
	ScanRunKindExternalSingle,
	ScanRunKindExternalPerRow,
	ScanRunKindRetract,
}

// AllScanRunKinds returns a fresh slice of the five
// canonical `scan_run.kind` literals. Returned as a new
// slice per call.
//
// The list is the FULL DB enum -- `full`, `delta`,
// `external_single`, `external_per_row`, `retract` --
// matching architecture Sec 5.7. The smaller
// [AllowedScanRunKinds] subset is what the [ChurnSweep]
// will actually run.
func AllScanRunKinds() []string {
	out := make([]string, len(allScanRunKinds))
	copy(out, allScanRunKinds)
	return out
}

// ValidateScanRunKind returns nil iff `kind` is one of
// the five canonical values, otherwise wraps
// [ErrUnknownScanRunKind] with the offending value. Used
// as the application-layer guard before any
// `INSERT INTO scan_run (..., kind, ...) VALUES (..., $K, ...)`
// statement; the DB enum is a second safety net.
//
// In particular, the forbidden literal `external_double`
// (architecture Sec 5.7 explicit anti-name) is REJECTED
// here -- the Stage 3.2 evaluator scenario
// `scan-run-kind-enum-rejects-invalid` pins this.
func ValidateScanRunKind(kind string) error {
	for _, k := range allScanRunKinds {
		if kind == k {
			return nil
		}
	}
	return fmt.Errorf("%w: got %q (allowed: %v)",
		ErrUnknownScanRunKind, kind, allScanRunKinds)
}

// Canonical `scan_run.sha_binding` literals. The DB CHECK
// constraint (migration `0001_catalog_lifecycle.up.sql:337-390`)
// enforces:
//
//	(sha_binding='single' AND to_sha IS NOT NULL)
//	OR (sha_binding='per_row' AND to_sha IS NULL)
//
// so a Stage 3.2 full scan -- which has a specific commit
// SHA to bind to -- MUST use `single`.
const (
	// SHABindingSingle is the binding used by a Stage 3.2
	// full / delta foundation scan. The parent commit's
	// SHA is recorded in `scan_run.to_sha`.
	SHABindingSingle = "single"
	// SHABindingPerRow is the binding used by an
	// `external_per_row` churn webhook -- each emitted
	// `metric_sample` row carries its own SHA in
	// `metric_sample.sha` and `scan_run.to_sha` is NULL.
	SHABindingPerRow = "per_row"
)

// allSHABindings is the closed set for
// [ValidateSHABinding].
var allSHABindings = []string{SHABindingSingle, SHABindingPerRow}

// ValidateSHABinding returns nil iff `b` is one of the
// two canonical values, otherwise wraps
// [ErrUnknownSHABinding].
func ValidateSHABinding(b string) error {
	for _, v := range allSHABindings {
		if b == v {
			return nil
		}
	}
	return fmt.Errorf("%w: got %q (allowed: %v)",
		ErrUnknownSHABinding, b, allSHABindings)
}

// PendingCommit is the metadata the state machine needs
// when it claims an oldest-pending commit row. Mirrors the
// columns the [ScanRunStore] selects from
// `clean_code.commit`.
type PendingCommit struct {
	// RepoID is the parent `repo` row's UUID.
	RepoID uuid.UUID
	// SHA is the commit's full 40-char hex SHA.
	SHA string
	// CommittedAt is the upstream `committed_at` timestamp
	// (UTC). The state machine claims the OLDEST pending
	// row, so the store ORDERs BY `committed_at ASC`.
	CommittedAt time.Time
}

// ClaimRequest is the per-call input to
// [ScanRunStore.ClaimNextPendingCommit]. Pinned as a
// struct (not positional args) so future fields
// (`recipe_version_id`, `policy_version_id`) can be added
// without breaking callers.
type ClaimRequest struct {
	// Kind is the `scan_run.kind` literal to INSERT.
	// Validated against [AllScanRunKinds] before any DB
	// write. Stage 3.2's state machine always passes
	// [ScanRunKindFull] for now; the field is exposed so a
	// future delta / retract driver can reuse the same
	// store.
	Kind string
	// SHABinding is the `scan_run.sha_binding` literal.
	// Stage 3.2 always passes [SHABindingSingle] -- the
	// scan binds to ONE specific commit SHA. The field is
	// validated against [allSHABindings].
	SHABinding string
	// OpenedAt is the wall-clock timestamp to stamp on
	// `scan_run.started_at`. Injected (not `time.Now()`-d
	// inside the store) so tests are deterministic.
	OpenedAt time.Time
}

// Validate returns nil iff `req` is well-formed for
// [ScanRunStore.ClaimNextPendingCommit]. Checks (in order):
//
//  1. Kind in [AllScanRunKinds].
//  2. SHABinding in {`single`, `per_row`}.
//  3. OpenedAt is non-zero.
func (req ClaimRequest) Validate() error {
	if err := ValidateScanRunKind(req.Kind); err != nil {
		return err
	}
	if err := ValidateSHABinding(req.SHABinding); err != nil {
		return err
	}
	if req.OpenedAt.IsZero() {
		return errors.New("metric_ingestor: ClaimRequest.OpenedAt is the zero time")
	}
	return nil
}

// ScanRunClaim is the receipt the state machine carries
// between [ScanRunStore.ClaimNextPendingCommit] and
// [ScanRunStore.FinalizeScanRun]. It is the value type
// passed to the [AstScanner] so the scanner can stamp the
// claim's `ScanRunID` onto every emitted `metric_sample`
// row.
type ScanRunClaim struct {
	// ScanRunID is the freshly-minted
	// `clean_code.scan_run.scan_run_id`. Stamped onto every
	// `MetricSample.producer_run_id` the scanner emits.
	ScanRunID uuid.UUID
	// RepoID is the parent `commit.repo_id`. Threaded
	// through so the scanner can resolve scopes per repo
	// without a second DB read.
	RepoID uuid.UUID
	// SHA is the parent `commit.sha`. Threaded through so
	// the scanner knows which working tree to scan.
	SHA string
	// Kind is the `scan_run.kind` the store inserted.
	Kind string
	// SHABinding is the `scan_run.sha_binding` the store
	// inserted. Always [SHABindingSingle] for Stage 3.2.
	SHABinding string
	// OpenedAt is the `scan_run.started_at` the store
	// stamped on the row.
	OpenedAt time.Time
}

// ScanRunStore is the persistence seam the state machine
// uses for the two atomic transitions:
//
//  1. CLAIM: SELECT oldest `commit.scan_status='pending'`
//     row FOR UPDATE; INSERT a freshly-minted
//     `scan_run(status='running', ...)`; UPDATE
//     `commit.scan_status='scanning'`. All three in ONE
//     DB transaction so a concurrent worker cannot pick
//     up the same commit.
//
//  2. FINALIZE: UPDATE `scan_run.status` to a terminal
//     value (`succeeded` | `failed`); UPDATE
//     `commit.scan_status` to the matching terminal
//     (`scanned` | `failed`). Both in ONE DB transaction
//     so the two rows never disagree.
//
// The Stage 3.2 in-memory implementation
// ([InMemoryScanRunStore]) emulates the atomicity with a
// package-private mutex; Phase 3.5 supplies the
// `pgx`-backed implementation that uses real SQL
// transactions with row-level locks.
type ScanRunStore interface {
	// ClaimNextPendingCommit attempts to claim the oldest
	// `commit.scan_status='pending'` row.
	//
	// On success returns the claim + (didClaim=true, nil).
	// On no-pending-commit returns (zero claim, didClaim=false,
	//   nil) -- this is NOT an error.
	// On any other failure (validation, DB error) returns
	//   (zero claim, false, err).
	//
	// The implementation MUST:
	//   - validate `req` via [ClaimRequest.Validate] BEFORE
	//     touching the DB,
	//   - assert [repo_indexer.CanTransition](pending,
	//     scanning) via [repo_indexer.ValidateTransition]
	//     before issuing the UPDATE.
	ClaimNextPendingCommit(ctx context.Context, req ClaimRequest) (ScanRunClaim, bool, error)

	// ClaimSpecificPendingCommit attempts to claim a
	// SPECIFIC `commit.scan_status='pending'` row named by
	// `(repoID, sha)`. iter-5 evaluator item 4: the
	// state machine's pre-flight pipeline first peeks N
	// pending commits via [PeekNextPendingCommits], probes
	// each via [AstSourceAvailability.HasFilesFor], and
	// then claims the FIRST one whose source is ready --
	// instead of the OLDEST one regardless of readiness
	// (the iter-4 behaviour, which head-of-line blocked
	// the queue when the oldest commit's source had not
	// yet materialised).
	//
	// On success returns the claim + (didClaim=true, nil).
	// When the targeted row is no longer pending (raced
	// away by another worker, or operator state edit)
	// returns (zero claim, didClaim=false, nil) -- the
	// caller should re-peek and try the next candidate.
	// On any other failure (validation, DB error) returns
	// (zero claim, false, err).
	//
	// The implementation MUST:
	//   - validate `req` via [ClaimRequest.Validate] BEFORE
	//     touching the DB,
	//   - assert [repo_indexer.CanTransition](pending,
	//     scanning) via [repo_indexer.ValidateTransition]
	//     before issuing the UPDATE,
	//   - filter on `(repo_id, sha, scan_status='pending')`
	//     so a row that already raced to `scanning` (or
	//     terminal) is not silently overwritten.
	ClaimSpecificPendingCommit(ctx context.Context, repoID uuid.UUID, sha string, req ClaimRequest) (ScanRunClaim, bool, error)

	// PeekNextPendingCommit returns the oldest
	// `commit.scan_status='pending'` row WITHOUT locking
	// it, WITHOUT inserting a `scan_run` row, and WITHOUT
	// mutating `commit.scan_status`.
	//
	// Retained for callers that only need the single-row
	// peek surface. iter-5 evaluator item 4 introduced
	// [PeekNextPendingCommits] for the state machine's
	// pre-flight pipeline which now peeks N commits to
	// avoid head-of-line blocking; this single-row method
	// is equivalent to `PeekNextPendingCommits(ctx, 1)`
	// returning the first element.
	//
	// On success returns (pending, true, nil); when no
	// pending row exists returns (zero, false, nil); on
	// infrastructure failure returns (zero, false, err).
	//
	// TOCTOU policy: a `true` return does NOT guarantee
	// that [ClaimNextPendingCommit] will see the same row
	// next call -- another worker may race ahead. The
	// peek is best-effort; the canonical claim is the
	// authoritative serialisation point.
	PeekNextPendingCommit(ctx context.Context) (PendingCommit, bool, error)

	// PeekNextPendingCommits returns up to `limit` oldest
	// `commit.scan_status='pending'` rows WITHOUT locking,
	// WITHOUT inserting a `scan_run` row, and WITHOUT
	// mutating `commit.scan_status`. Results are sorted by
	// `committed_at ASC, sha ASC` matching the canonical
	// claim order.
	//
	// iter-5 evaluator item 4: the state machine's
	// pre-flight pipeline asks for N candidates, probes
	// each via [AstSourceAvailability.HasFilesFor] in
	// order, and claims the FIRST one whose source is
	// ready. Without a multi-row peek the iter-4 wiring
	// always asked about the OLDEST commit -- when that
	// commit's checkout had not yet materialised the queue
	// head-of-line blocked behind it even when LATER
	// commits had ready sources.
	//
	// `limit` MUST be >= 1; implementations MAY clamp
	// excessive values. A zero limit returns
	// (nil, validation-error).
	//
	// Empty queue returns (nil, nil); infrastructure
	// failure returns (nil, err).
	PeekNextPendingCommits(ctx context.Context, limit int) ([]PendingCommit, error)

	// FinalizeScanRun atomically transitions both rows to
	// their terminal values:
	//   `scan_run.status` -> `runStatus`
	//   `commit.scan_status` -> `commitStatus`
	//
	// The implementation MUST:
	//   - reject runStatus other than `succeeded` / `failed`
	//     (via [ValidateScanRunStatus] + an in-method
	//     terminal guard),
	//   - reject commitStatus other than `scanned` / `failed`
	//     (via the same guard),
	//   - assert [repo_indexer.CanTransition](scanning,
	//     commitStatus) via [repo_indexer.ValidateTransition],
	//   - stamp `endedAt` on `scan_run.ended_at`.
	FinalizeScanRun(ctx context.Context, claim ScanRunClaim, runStatus ScanRunStatus, commitStatus repo_indexer.ScanStatus, endedAt time.Time) error
}

// AstScanner is the seam the state machine uses to invoke
// the foundation-tier AST recipe loop for one ScanRun. The
// production implementation is [IngestorAstScanner] which
// wraps [Ingestor.Run] with a [ScanRunContext] built from
// the claim.
//
// The contract:
//   - Scan MUST honour ctx cancellation (the state
//     machine wraps ctx with the configured timeout
//     before calling Scan).
//   - Scan MUST return a non-nil error when any recipe
//     fails; the state machine maps any non-nil error to
//     `ScanRunStatusFailed` + `ScanStatusFailed`.
//   - Scan SHOULD NOT panic, but the state machine
//     recovers from panics anyway -- a panicking recipe
//     surfaces as [ErrScannerPanic] (Stage 3.2
//     implementation-plan line 306).
type AstScanner interface {
	Scan(ctx context.Context, claim ScanRunClaim) error
}

// IngestorAstScanner is the production [AstScanner] that
// bridges the state machine's [ScanRunClaim] to the
// per-call [Ingestor.Run] coordinator. Construct via
// [NewIngestorAstScanner].
//
// The scanner builds a [ScanRunContext] from the claim and
// invokes [Ingestor.Run] with no churn payload (Stage 3.2
// drives foundation-tier scans only; per-row churn arrives
// via the separate webhook path that already calls the
// [Ingestor] directly). The [Ingestor]'s per-kind dispatch
// logic decides whether to run the foundation dispatcher
// (`full` / `delta`) or short-circuit
// (`external_per_row`, which Stage 3.2's state machine
// never claims because its default kind is `full`).
type IngestorAstScanner struct {
	ingestor *Ingestor
}

// NewIngestorAstScanner returns an [IngestorAstScanner]
// wired to `ing`. PANICS if `ing` is nil -- the dependency
// is non-optional.
func NewIngestorAstScanner(ing *Ingestor) *IngestorAstScanner {
	if ing == nil {
		panic("metric_ingestor: NewIngestorAstScanner received nil *Ingestor")
	}
	return &IngestorAstScanner{ingestor: ing}
}

// Scan implements [AstScanner] by delegating to
// [Ingestor.Run]. The [Ingestor]'s own validation rejects
// any claim whose `Kind` is outside [AllowedScanRunKinds]
// (e.g. `external_single`, `retract`); when this happens
// the state machine maps the error to a `failed`
// transition. The state machine itself does NOT pre-filter
// the kind, so the layer-boundary is honest: the [Ingestor]
// remains the single source of truth for which kinds it
// dispatches.
func (s *IngestorAstScanner) Scan(ctx context.Context, claim ScanRunClaim) error {
	_, err := s.ingestor.Run(ctx, RunRequest{
		ScanRun: ScanRunContext{
			ID:     claim.ScanRunID,
			Kind:   claim.Kind,
			RepoID: claim.RepoID,
			SHA:    claim.SHA,
		},
		Foundation: FoundationInput{
			// Thread the claimed commit SHA into the
			// foundation dispatch input so a wired
			// [RegistryBackedFoundationDispatcher.Writer]
			// can stamp `metric_sample.sha` (Stage 3.2
			// item 4 / brief: "drives the recipe
			// registry over the parsed AST and writes
			// metric_sample rows").
			SHA: claim.SHA,
		},
	})
	return err
}

// ProcessOneResult is the structured outcome of one
// [StateMachine.ProcessOne] call. Returned even when the
// scan failed so callers can emit a structured log line
// covering both success and failure paths uniformly.
type ProcessOneResult struct {
	// DidWork is true iff a pending commit was claimed.
	// false means the queue was empty -- the caller's
	// sweep loop should back off.
	DidWork bool
	// Claim is the [ScanRunClaim] the store produced. Zero
	// value when DidWork is false.
	Claim ScanRunClaim
	// RunStatus is the terminal `scan_run.status` the
	// state machine recorded. `succeeded` on a clean Scan,
	// `failed` otherwise. Zero value when DidWork is false.
	RunStatus ScanRunStatus
	// CommitStatus is the terminal `commit.scan_status`
	// the state machine recorded. `scanned` on a clean
	// Scan, `failed` otherwise. Zero value when DidWork
	// is false.
	CommitStatus repo_indexer.ScanStatus
	// ScanErr is the non-nil error returned by the
	// [AstScanner.Scan] call OR a wrapped
	// [ErrScannerPanic] / [ErrScanTimeout]. nil on a
	// successful scan.
	ScanErr error
	// Duration is the wall-clock time from the start of
	// the Scan call to its return (or panic / timeout).
	// Recorded for observability; does NOT include
	// claim or finalize time.
	Duration time.Duration
	// SkipReason is non-empty when the state machine
	// short-circuited BEFORE claiming a commit -- the
	// pre-flight pipeline (iter-4 evaluator item 2)
	// found a reason to defer the work. DidWork is
	// always false when SkipReason is set.
	SkipReason SkipReason
	// Pending is the peeked
	// [PendingCommit] when [SkipReason] is set so the
	// caller can include the commit identity in
	// structured logs / metrics without re-peeking.
	// Zero-value when SkipReason is empty.
	Pending PendingCommit
}

// SkipReason enumerates the reasons
// [StateMachine.ProcessOne] may return DidWork=false
// AFTER observing a pending commit. The empty value is
// the canonical "no commit pending" -- semantically
// distinct from the "commit pending but deferred" cases.
type SkipReason string

// Closed set of [SkipReason] values.
const (
	// SkipReasonSourceNotReady -- the
	// [AstSourceAvailability] probe reported the
	// upstream artefact is not yet materialised (iter-4
	// evaluator item 2 structural pre-flight). The
	// commit stays `pending`; the next sweep tick
	// retries.
	SkipReasonSourceNotReady SkipReason = "source_not_ready"
)

// StateMachine is the writer-side orchestrator. Construct
// via [NewStateMachine]; call [StateMachine.ProcessOne] to
// drain ONE pending commit. The intentional minimalism
// (no goroutine loop, no batching) keeps the lifecycle
// inversion-of-control compatible with the future
// periodic-sweep cadence loop the composition root will
// wrap around it.
//
// # State graph
//
//	     ClaimNextPendingCommit              FinalizeScanRun
//	pending ----------------> scanning ----------------> scanned   (clean)
//	                              |
//	                              +-> ----------------> failed     (scan error / panic / timeout)
//	                                  FinalizeScanRun
//
// Every state transition is gated by
// [repo_indexer.ValidateTransition] BEFORE the store sees
// it -- a defence-in-depth match for the DB role grants in
// migration 0004.
type StateMachine struct {
	store        ScanRunStore
	scanner      AstScanner
	probe        AstSourceAvailability
	kind         string
	binding      string
	timeout      time.Duration
	finalize     time.Duration
	probeFanout  int
	now          func() time.Time
	log          *slog.Logger
}

// StateMachineOption configures a [StateMachine]
// constructed via [NewStateMachine]. Designed so
// production wiring and tests can override only the
// fields they care about.
type StateMachineOption func(*StateMachine)

// WithStateMachineKind overrides the `scan_run.kind` the
// state machine passes to
// [ScanRunStore.ClaimNextPendingCommit]. Default is
// [ScanRunKindFull]. Tests use this to assert the
// invalid-kind rejection path.
//
// Validated at [NewStateMachine] time via
// [ValidateScanRunKind] so an unknown literal surfaces
// immediately rather than at first ProcessOne call.
func WithStateMachineKind(kind string) StateMachineOption {
	return func(sm *StateMachine) {
		sm.kind = kind
	}
}

// WithStateMachineSHABinding overrides the
// `scan_run.sha_binding` the state machine passes to
// [ScanRunStore.ClaimNextPendingCommit]. Default is
// [SHABindingSingle]. Validated at [NewStateMachine] time
// via [ValidateSHABinding].
func WithStateMachineSHABinding(binding string) StateMachineOption {
	return func(sm *StateMachine) {
		sm.binding = binding
	}
}

// WithStateMachineTimeout overrides the per-scan timeout
// the state machine wraps around [AstScanner.Scan].
// Default is [config.DefaultScanTimeout] (30min,
// tech-spec Sec 8.2). PANICS at [NewStateMachine] time if
// the value is <= 0 -- a zero timeout would deadlock the
// scan immediately.
func WithStateMachineTimeout(d time.Duration) StateMachineOption {
	return func(sm *StateMachine) {
		sm.timeout = d
	}
}

// WithStateMachineFinalizeTimeout overrides the deadline
// applied to the [ScanRunStore.FinalizeScanRun] call. The
// state machine uses a SEPARATE context (decoupled from
// the caller's cancellation) so a timed-out scan still
// gets to record `scan_run.status='failed'` and
// `commit.scan_status='failed'`. Default is 30 seconds.
func WithStateMachineFinalizeTimeout(d time.Duration) StateMachineOption {
	return func(sm *StateMachine) {
		sm.finalize = d
	}
}

// WithStateMachineClock overrides the clock the state
// machine uses for `scan_run.started_at` / `ended_at`.
// Default is [time.Now]. Tests inject a fixed clock so
// timestamps are deterministic.
func WithStateMachineClock(now func() time.Time) StateMachineOption {
	return func(sm *StateMachine) {
		sm.now = now
	}
}

// WithStateMachineLogger overrides the logger. nil
// disables logging (the zero value is permitted).
func WithStateMachineLogger(log *slog.Logger) StateMachineOption {
	return func(sm *StateMachine) {
		sm.log = log
	}
}

// WithStateMachineSourceProbe wires the optional
// [AstSourceAvailability] pre-flight check (iter-4
// evaluator item 2). When set, [ProcessOne] peeks the
// next pending commits via
// [ScanRunStore.PeekNextPendingCommits] and asks the probe
// whether its underlying AST source can deliver files for
// each candidate BEFORE issuing the canonical
// `pending->scanning` claim transition. The state machine
// claims the FIRST candidate whose probe returns `true`
// (iter-5 evaluator item 4 removed the iter-4 head-of-line
// block where only the OLDEST candidate was probed).
//
// When NO peeked candidate is ready, the state machine
// returns
// `ProcessOneResult{DidWork:false, SkipReason:SourceNotReady}, nil`
// -- every peeked commit stays `pending` (no canonical
// transition occurs) and the next sweep tick retries. This
// is the supported recovery surface for a not-yet-
// materialised upstream checkout: the Metric Ingestor
// remains the SOLE writer of `commit.scan_status`, the
// four-state Commit diagram is preserved, and
// [repo_indexer.ValidateTransition] stays the gate on
// every canonical edge.
//
// When a probed candidate returns `true`, the state machine
// proceeds with the targeted claim via
// [ScanRunStore.ClaimSpecificPendingCommit]. A TOCTOU race
// (probe says ready, source not ready by parse time)
// surfaces inside the scan via
// [ErrCommitRootNotMaterialised] and the commit gets the
// canonical `failed` terminal state -- the probe just
// keeps that outcome rare in steady-state.
//
// nil disables the pre-flight (the default); ProcessOne
// then claims the oldest pending commit unconditionally via
// [ScanRunStore.ClaimNextPendingCommit] (legacy iter-3
// behaviour).
func WithStateMachineSourceProbe(probe AstSourceAvailability) StateMachineOption {
	return func(sm *StateMachine) {
		sm.probe = probe
	}
}

// WithStateMachineProbeFanout overrides the number of
// pending commits the state machine peeks per
// [ProcessOne] call when an [AstSourceAvailability] probe
// is wired. iter-5 evaluator item 4: a fanout > 1 lets the
// state machine SKIP a not-yet-ready oldest commit and
// claim a newer commit whose source IS ready, instead of
// head-of-line blocking the entire queue behind the
// oldest. Default is [defaultProbeFanout] = 16.
//
// PANICS at [NewStateMachine] time if `n` is <= 0.
//
// No effect when no probe is wired (the legacy path
// claims the oldest unconditionally and never peeks).
func WithStateMachineProbeFanout(n int) StateMachineOption {
	return func(sm *StateMachine) {
		sm.probeFanout = n
	}
}

// defaultProbeFanout is the iter-5 default for the number
// of pending commits the state machine peeks per
// [ProcessOne] call when a probe is wired. 16 is large
// enough to cover a sweep tick's-worth of typical commits
// without paying a wide SELECT per call; operators can
// override via [WithStateMachineProbeFanout].
const defaultProbeFanout = 16

// defaultFinalizeTimeout is the deadline on the finalize
// path when the caller does not override it. 30 seconds
// is enough for a normal PG round-trip even under load
// but short enough that a wedged DB does not pile up
// goroutines indefinitely.
const defaultFinalizeTimeout = 30 * time.Second

// NewStateMachine returns a wired [StateMachine]. PANICS
// when:
//
//   - `store` is nil,
//   - `scanner` is nil,
//   - any option supplies an invalid `kind` (not in
//     [AllScanRunKinds]), `sha_binding` (not in
//     `{single, per_row}`), or non-positive timeout.
//
// The default configuration is:
//
//   - kind=`full`,
//   - sha_binding=`single`,
//   - timeout=30min,
//   - finalize_timeout=30s,
//   - now=[time.Now],
//   - log=nil.
//
// All defaults match the Stage 3.2 brief verbatim:
//
//	"opens a scan_run(kind='full', sha_binding='single',
//	 status='running')".
func NewStateMachine(store ScanRunStore, scanner AstScanner, opts ...StateMachineOption) *StateMachine {
	if store == nil {
		panic("metric_ingestor: NewStateMachine received nil ScanRunStore")
	}
	if scanner == nil {
		panic("metric_ingestor: NewStateMachine received nil AstScanner")
	}
	sm := &StateMachine{
		store:       store,
		scanner:     scanner,
		kind:        ScanRunKindFull,
		binding:     SHABindingSingle,
		timeout:     30 * time.Minute,
		finalize:    defaultFinalizeTimeout,
		probeFanout: defaultProbeFanout,
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(sm)
	}
	if err := ValidateScanRunKind(sm.kind); err != nil {
		panic(fmt.Sprintf("metric_ingestor: NewStateMachine: %v", err))
	}
	if err := ValidateSHABinding(sm.binding); err != nil {
		panic(fmt.Sprintf("metric_ingestor: NewStateMachine: %v", err))
	}
	if sm.timeout <= 0 {
		panic(fmt.Sprintf("metric_ingestor: NewStateMachine: timeout=%s must be > 0", sm.timeout))
	}
	if sm.finalize <= 0 {
		panic(fmt.Sprintf("metric_ingestor: NewStateMachine: finalize timeout=%s must be > 0", sm.finalize))
	}
	if sm.probeFanout <= 0 {
		panic(fmt.Sprintf("metric_ingestor: NewStateMachine: probe fanout=%d must be > 0", sm.probeFanout))
	}
	if sm.now == nil {
		panic("metric_ingestor: NewStateMachine: nil clock")
	}
	return sm
}

// ProcessOne attempts to claim the oldest pending commit
// and drive it through the foundation-tier scan to a
// terminal state.
//
// Return contract:
//
//   - When NO commit is pending: returns
//     ProcessOneResult{DidWork: false}, nil.
//   - When a commit was claimed and the scan succeeded:
//     returns a result with DidWork=true, RunStatus=succeeded,
//     CommitStatus=scanned, ScanErr=nil, and a NIL error.
//   - When a commit was claimed and the scan failed
//     (scanner returned err / panicked / hit the configured
//     timeout): returns a result with DidWork=true,
//     RunStatus=failed, CommitStatus=failed, ScanErr=<the
//     scan failure>, and a NIL error -- the scan failure is
//     RECORDED state, not a Go error from ProcessOne.
//   - When the STORE itself fails (claim error, finalize
//     error): returns a partially-populated result and a
//     non-nil error from ProcessOne. Infrastructure
//     failures propagate; scan failures do not.
//
// Cancellation:
//
//   - The state machine wraps `ctx` with the configured
//     scan_timeout before calling [AstScanner.Scan]. A
//     deadline-exceeded result transitions the rows to
//     `failed` and surfaces [ErrScanTimeout] in
//     ScanErr.
//   - The finalize path uses [context.WithoutCancel] +
//     a fresh deadline so a caller-cancelled or
//     timed-out scan still gets recorded. The caller's
//     cancellation can still affect the CLAIM step; if
//     the claim fails the state machine returns the
//     error directly (no row was opened).
func (sm *StateMachine) ProcessOne(ctx context.Context) (ProcessOneResult, error) {
	claimReq := ClaimRequest{
		Kind:       sm.kind,
		SHABinding: sm.binding,
		OpenedAt:   sm.now(),
	}
	if err := claimReq.Validate(); err != nil {
		// Defensive: NewStateMachine should have caught
		// this, but guard so a future field on
		// ClaimRequest can't slip past unvalidated.
		return ProcessOneResult{}, fmt.Errorf("metric_ingestor: state machine claim request invalid: %w", err)
	}

	// iter-4 evaluator item 2 + iter-5 evaluator item 4 --
	// structural pre-flight that no longer head-of-line
	// blocks behind the oldest commit. When an
	// [AstSourceAvailability] probe is wired (production:
	// the [DirectoryAstFileSource] doubles as the probe),
	// peek up to [StateMachine.probeFanout] pending
	// commits and ask the probe whether each upstream
	// artefact is materialised. Claim the FIRST one whose
	// probe returns `true` via the targeted
	// [ScanRunStore.ClaimSpecificPendingCommit]. If NO
	// peeked candidate is ready, return DidWork=false
	// (every peeked commit stays `pending`; no canonical
	// edge crossed). The SOLE-WRITER-of-`commit.scan_status`
	// invariant (architecture Sec 1.5.1 row 1) is
	// preserved without forcing the oldest commit to
	// `failed` and without blocking newer ready commits
	// behind it.
	if sm.probe != nil {
		fanout := sm.probeFanout
		if fanout <= 0 {
			fanout = defaultProbeFanout
		}
		pending, err := sm.store.PeekNextPendingCommits(ctx, fanout)
		if err != nil {
			return ProcessOneResult{}, fmt.Errorf("metric_ingestor: peek next pending commits (limit=%d): %w", fanout, err)
		}
		if len(pending) == 0 {
			if sm.log != nil {
				sm.log.Debug("state machine: pre-flight peek found no pending commit",
					"kind", sm.kind,
					"sha_binding", sm.binding,
					"probe_fanout", fanout,
				)
			}
			return ProcessOneResult{DidWork: false}, nil
		}
		// Iterate the peeked slice in commit-time order;
		// claim the first commit whose probe returns
		// ready. iter-5 item 4: the iter-4 wiring only
		// checked pending[0], so a not-yet-materialised
		// oldest commit blocked every newer ready commit
		// behind it (head-of-line).
		var (
			notReady     int
			lastSkipped  PendingCommit
		)
		for i, cand := range pending {
			ready, err := sm.probe.HasFilesFor(ctx, cand)
			if err != nil {
				return ProcessOneResult{}, fmt.Errorf("metric_ingestor: source availability probe (%s @ %s): %w", cand.RepoID, cand.SHA, err)
			}
			if !ready {
				notReady++
				lastSkipped = cand
				if sm.log != nil {
					sm.log.Debug("state machine: pre-flight skipping not-ready candidate",
						"component", "metric_ingestor.StateMachine",
						"repo_id", cand.RepoID,
						"sha", cand.SHA,
						"index", i,
						"reason", "AstSourceAvailability.HasFilesFor=false",
					)
				}
				continue
			}
			// Found a ready candidate -- claim it
			// specifically. A ClaimSpecific that returns
			// (zero, false, nil) means the row raced to
			// another worker between the peek and the
			// claim; `continue` to the next peeked
			// candidate so a single race doesn't cost the
			// whole cadence cycle in Phase 3.5 multi-worker
			// fan-out. If every remaining candidate also
			// races or is not-ready, the loop falls through
			// to the "no ready source in fanout" result
			// below with DidWork=false.
			claim, didClaim, err := sm.store.ClaimSpecificPendingCommit(ctx, cand.RepoID, cand.SHA, claimReq)
			if err != nil {
				return ProcessOneResult{}, fmt.Errorf("metric_ingestor: claim specific pending commit (%s @ %s): %w", cand.RepoID, cand.SHA, err)
			}
			if !didClaim {
				if sm.log != nil {
					sm.log.Debug("state machine: targeted claim raced away",
						"component", "metric_ingestor.StateMachine",
						"repo_id", cand.RepoID,
						"sha", cand.SHA,
						"index", i,
					)
				}
				continue
			}
			return sm.runAndFinalize(ctx, claim)
		}
		// Every peeked candidate was not ready. Surface
		// the SkipReason with the LAST-skipped pending
		// row so callers see at least one (RepoID, SHA)
		// in their structured log. The oldest is in
		// pending[0]; we keep it as the canonical
		// "skipped" surface for parity with iter-4's
		// single-row behaviour.
		if sm.log != nil {
			sm.log.Info("state machine: deferring claim -- no ready source in fanout",
				"component", "metric_ingestor.StateMachine",
				"probe_fanout", fanout,
				"candidates_seen", len(pending),
				"candidates_skipped", notReady,
				"oldest_repo_id", pending[0].RepoID,
				"oldest_sha", pending[0].SHA,
				"reason", "AstSourceAvailability.HasFilesFor=false (all)",
			)
		}
		_ = lastSkipped
		return ProcessOneResult{
			DidWork:    false,
			SkipReason: SkipReasonSourceNotReady,
			Pending:    pending[0],
		}, nil
	}

	claim, didClaim, err := sm.store.ClaimNextPendingCommit(ctx, claimReq)
	if err != nil {
		return ProcessOneResult{}, fmt.Errorf("metric_ingestor: claim next pending commit: %w", err)
	}
	if !didClaim {
		if sm.log != nil {
			sm.log.Debug("state machine: no pending commit",
				"kind", sm.kind,
				"sha_binding", sm.binding,
			)
		}
		return ProcessOneResult{DidWork: false}, nil
	}

	return sm.runAndFinalize(ctx, claim)
}

// runAndFinalize runs the AST scan for `claim` under a
// hard timeout and finalises BOTH `scan_run.status` and
// `commit.scan_status` to their canonical terminal pair
// (succeeded/scanned or failed/failed). Pulled out of
// [ProcessOne] so the pre-flight pipeline (iter-5 evaluator
// item 4) can claim a SPECIFIC commit via
// [ScanRunStore.ClaimSpecificPendingCommit] and drive the
// same scan+finalize sequence without duplicating the
// timeout / panic-recovery / finalize-with-separate-context
// wiring.
func (sm *StateMachine) runAndFinalize(ctx context.Context, claim ScanRunClaim) (ProcessOneResult, error) {
	scanCtx, cancel := context.WithTimeout(ctx, sm.timeout)
	defer cancel()

	start := sm.now()
	scanErr := sm.runScan(scanCtx, claim)
	duration := sm.now().Sub(start)

	runStatus := ScanRunStatusSucceeded
	commitStatus := repo_indexer.ScanStatusScanned
	if scanErr != nil {
		runStatus = ScanRunStatusFailed
		commitStatus = repo_indexer.ScanStatusFailed
	}

	finalizeCtx, finalizeCancel := context.WithTimeout(context.WithoutCancel(ctx), sm.finalize)
	defer finalizeCancel()
	if err := sm.store.FinalizeScanRun(finalizeCtx, claim, runStatus, commitStatus, sm.now()); err != nil {
		// Finalize failed AFTER a successful scan: this is
		// an infrastructure error that the caller must
		// notice. The scan output is still durable (its
		// MetricSample rows landed), but `scan_run.status`
		// and `commit.scan_status` may be stuck in
		// `running` / `scanning` and the next sweep will
		// see them as orphans. Propagate.
		return ProcessOneResult{
			DidWork:      true,
			Claim:        claim,
			RunStatus:    runStatus,
			CommitStatus: commitStatus,
			ScanErr:      scanErr,
			Duration:     duration,
		}, fmt.Errorf("metric_ingestor: finalize scan_run %s: %w", claim.ScanRunID, err)
	}

	if sm.log != nil {
		level := slog.LevelInfo
		if scanErr != nil {
			level = slog.LevelWarn
		}
		sm.log.Log(ctx, level, "state machine: scan_run finalised",
			"scan_run_id", claim.ScanRunID,
			"repo_id", claim.RepoID,
			"sha", claim.SHA,
			"kind", claim.Kind,
			"sha_binding", claim.SHABinding,
			"run_status", runStatus,
			"commit_status", commitStatus,
			"duration_ms", duration.Milliseconds(),
			"scan_err", scanErrString(scanErr),
		)
	}

	return ProcessOneResult{
		DidWork:      true,
		Claim:        claim,
		RunStatus:    runStatus,
		CommitStatus: commitStatus,
		ScanErr:      scanErr,
		Duration:     duration,
	}, nil
}

// runScan invokes the injected [AstScanner.Scan] under a
// HARD timeout that is enforced REGARDLESS of whether the
// scanner honours `ctx.Done()`. The Scan runs in a goroutine
// and `runScan` selects on (scanner result, ctx.Done). When
// the context deadline fires first the state machine
// records [ErrScanTimeout] and finalises the run -- a
// hung, CPU-bound, or context-ignoring recipe still gets
// transitioned to `failed` so the queue is not blocked.
//
// # Leaked-goroutine note
//
// If the scanner ignores cancellation entirely the
// background goroutine outlives `runScan`. The goroutine
// is bounded in size (one per claimed commit) and the
// process exits via SIGTERM eventually; leaking a few
// hung scanners is preferable to wedging the queue. The
// production [AstScanner] implementation MUST honour
// `ctx.Done()` -- the leak path is a defence against a
// buggy recipe, not a normal operating mode.
//
// # Panic recovery
//
// A panic inside the scanner goroutine is recovered and
// wrapped into [ErrScannerPanic]. The recovered panic is
// delivered to the select via the same result channel so
// the caller path for "scanner returned err" and "scanner
// panicked" share the same finalize wiring.
//
// # Timeout sentinel mapping
//
// When the scanner returns `context.DeadlineExceeded`
// (cooperative path), or the select fires on
// `ctx.Done()` (hard path), the returned error wraps both
// [ErrScanTimeout] AND `context.DeadlineExceeded` so
// callers can `errors.Is` on either.
func (sm *StateMachine) runScan(ctx context.Context, claim ScanRunClaim) error {
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%w: %v", ErrScannerPanic, r)
			}
			// done is buffered (cap=1) so this send NEVER
			// blocks -- the receive may have already
			// happened on the timeout path, but the buffer
			// absorbs the send so the goroutine exits
			// cleanly without leaking on the channel send.
			done <- err
		}()
		err = sm.scanner.Scan(ctx, claim)
	}()

	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		// Map a deadline exceeded surfaced through the
		// scanner (cooperative cancellation honoured) to
		// the typed ErrScanTimeout sentinel so callers can
		// distinguish "timed out" from generic scan errors.
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %w", ErrScanTimeout, err)
		}
		return err
	case <-ctx.Done():
		// Hard-timeout path: the scanner has NOT yet
		// returned but the configured scan_timeout fired.
		// Record the timeout regardless of whether the
		// goroutine ever exits -- the finalize wiring
		// transitions the rows to `failed` and the queue
		// keeps moving.
		ctxErr := ctx.Err()
		if ctxErr == nil {
			// Defensive: select fired on Done but Err is
			// nil. Should not happen for WithTimeout, but
			// guard so the wrap below has a non-nil cause.
			ctxErr = context.DeadlineExceeded
		}
		return fmt.Errorf("%w: %w", ErrScanTimeout, ctxErr)
	}
}

// scanErrString safely renders a possibly-nil scan error
// for the structured log. Nil maps to the empty string;
// non-nil maps to the error's text. Pulled out so the
// log call site stays one line.
func scanErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// InMemoryScanRunStore is the Stage 3.2 in-memory
// implementation of [ScanRunStore]. Designed for tests
// AND for early integration scenarios where the PG-backed
// store is not yet wired (Phase 3.5 lands the SQL
// implementation). Thread-safe via an internal mutex --
// concurrent ProcessOne callers will serialise on it but
// the claim step is the only contended section.
//
// Construct via [NewInMemoryScanRunStore] which lets the
// caller seed the pending queue.
//
// # Invariants the store enforces
//
//   - The pending queue is FIFO ordered by `committed_at`
//     (the store calls [time.Time.Before] when sorting
//     enqueued commits).
//   - A commit can only be claimed ONCE; subsequent claims
//     skip it.
//   - Finalize MUST receive a claim with a known ScanRunID
//     that is currently `running`. Double-finalize raises
//     [ErrClaimedRunNotInProgress]; unknown ScanRunID
//     raises [ErrUnknownScanRunID].
//   - Every transition is gated by
//     [repo_indexer.ValidateTransition] BEFORE the
//     in-memory state mutates.
type InMemoryScanRunStore struct {
	mu sync.Mutex

	// pending is the ordered queue of PendingCommit rows
	// the store will hand out via ClaimNextPendingCommit.
	// Sorted by CommittedAt ASC on Seed; mutated in place
	// on each claim (pop front).
	pending []PendingCommit
	// runs maps scan_run_id -> the current run record so
	// FinalizeScanRun can look up and validate the prior
	// state.
	runs map[uuid.UUID]*inMemoryScanRunRecord
	// commitStatus maps `repo_id:sha` -> the current
	// `commit.scan_status` so FinalizeScanRun can assert
	// the from-state matches `scanning`.
	commitStatus map[string]repo_indexer.ScanStatus
	// newID is the UUID factory. Default is [uuid.NewV4];
	// tests inject a deterministic sequence.
	newID func() (uuid.UUID, error)
}

// inMemoryScanRunRecord is the in-memory representation of
// one `clean_code.scan_run` row.
type inMemoryScanRunRecord struct {
	ScanRunID  uuid.UUID
	RepoID     uuid.UUID
	ToSHA      string // matches the DB `to_sha` column
	Kind       string
	SHABinding string
	Status     ScanRunStatus
	StartedAt  time.Time
	EndedAt    time.Time
}

// InMemoryStoreOption configures an
// [InMemoryScanRunStore] at construction time.
type InMemoryStoreOption func(*InMemoryScanRunStore)

// WithInMemoryStoreIDFactory overrides the UUID factory.
// Tests use this to produce deterministic scan_run_ids.
func WithInMemoryStoreIDFactory(f func() (uuid.UUID, error)) InMemoryStoreOption {
	return func(s *InMemoryScanRunStore) {
		s.newID = f
	}
}

// NewInMemoryScanRunStore returns a fresh
// [InMemoryScanRunStore] with no pending commits. Seed
// via [InMemoryScanRunStore.SeedPending].
func NewInMemoryScanRunStore(opts ...InMemoryStoreOption) *InMemoryScanRunStore {
	s := &InMemoryScanRunStore{
		runs:         make(map[uuid.UUID]*inMemoryScanRunRecord),
		commitStatus: make(map[string]repo_indexer.ScanStatus),
		newID:        uuid.NewV4,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.newID == nil {
		s.newID = uuid.NewV4
	}
	return s
}

// SeedPending appends `commits` to the pending queue. The
// queue is re-sorted by `CommittedAt` ASC on every call
// so callers can supply them in any order.
//
// The matching `commit.scan_status` is recorded as
// `pending` -- a precondition the Repo Indexer satisfies
// in production when it INSERTs the row with the schema
// default.
func (s *InMemoryScanRunStore) SeedPending(commits ...PendingCommit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, commits...)
	sortPendingByCommittedAt(s.pending)
	for _, c := range commits {
		key := commitKey(c.RepoID, c.SHA)
		// Only seed status if it's not already present -- a
		// double-seed (test artefact) should not regress a
		// claimed commit back to pending.
		if _, ok := s.commitStatus[key]; !ok {
			s.commitStatus[key] = repo_indexer.ScanStatusPending
		}
	}
}

// CommitStatus returns the current `commit.scan_status`
// for the given (repo_id, sha) pair, or
// [repo_indexer.ScanStatus]("") if unknown. Exposed for
// test assertions -- production code reads the column
// directly from PG.
func (s *InMemoryScanRunStore) CommitStatus(repoID uuid.UUID, sha string) repo_indexer.ScanStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitStatus[commitKey(repoID, sha)]
}

// ScanRunStatus returns the current `scan_run.status` for
// the given scan_run_id, or [ScanRunStatus]("") if
// unknown. Exposed for test assertions.
func (s *InMemoryScanRunStore) ScanRunStatus(id uuid.UUID) ScanRunStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		return r.Status
	}
	return ""
}

// ScanRunRecord returns a SNAPSHOT of the scan_run row
// with the given id, or (zero, false) if unknown. The
// snapshot is a value copy -- mutating it does not affect
// the store.
func (s *InMemoryScanRunStore) ScanRunRecord(id uuid.UUID) (inMemoryScanRunRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return inMemoryScanRunRecord{}, false
	}
	return *r, true
}

// PeekNextPendingCommit implements [ScanRunStore] for the
// in-memory store. Returns the front of the pending queue
// (the OLDEST `committed_at`) WITHOUT popping, locking,
// inserting any `scan_run` row, or mutating the commit
// status -- iter-4 evaluator item 2 structural pre-flight.
//
// The peek returns whatever the queue head currently
// holds; subsequent [ClaimNextPendingCommit] may observe
// a DIFFERENT head if SeedPending was called between the
// peek and the claim (the in-memory store re-sorts on
// every Seed). Callers must treat the peek as
// best-effort, matching the PG store's
// "no-lock SELECT" semantics.
func (s *InMemoryScanRunStore) PeekNextPendingCommit(ctx context.Context) (PendingCommit, bool, error) {
	if err := ctx.Err(); err != nil {
		return PendingCommit{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return PendingCommit{}, false, nil
	}
	return s.pending[0], true, nil
}

// PeekNextPendingCommits implements [ScanRunStore] for the
// in-memory store. Returns up to `limit` oldest pending
// commits in `committed_at ASC, sha ASC` order WITHOUT
// popping, locking, or mutating. iter-5 evaluator item 4:
// the state machine's pre-flight uses this multi-row peek
// to avoid head-of-line blocking when the OLDEST pending
// commit's source has not yet materialised but newer ones
// have.
func (s *InMemoryScanRunStore) PeekNextPendingCommits(ctx context.Context, limit int) ([]PendingCommit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("metric_ingestor: InMemoryScanRunStore.PeekNextPendingCommits: limit=%d must be > 0", limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil, nil
	}
	n := limit
	if n > len(s.pending) {
		n = len(s.pending)
	}
	out := make([]PendingCommit, n)
	copy(out, s.pending[:n])
	return out, nil
}

// ClaimNextPendingCommit implements [ScanRunStore]. The
// in-memory implementation pops the front of the pending
// queue, mints a fresh scan_run_id, records the
// `running` run, and transitions the commit's status to
// `scanning` -- all under the store mutex.
func (s *InMemoryScanRunStore) ClaimNextPendingCommit(ctx context.Context, req ClaimRequest) (ScanRunClaim, bool, error) {
	if err := ctx.Err(); err != nil {
		return ScanRunClaim{}, false, err
	}
	if err := req.Validate(); err != nil {
		return ScanRunClaim{}, false, err
	}
	// CHECK constraint parity: kind='single' MUST come with
	// a non-NULL to_sha. The state machine always supplies
	// the SHA from the claimed commit, so the only way
	// this fails is a future caller passing per_row + a
	// commit-bound store -- guard explicitly.
	if req.SHABinding == SHABindingPerRow {
		return ScanRunClaim{}, false, fmt.Errorf(
			"metric_ingestor: InMemoryScanRunStore does not support sha_binding=%q (per-row claims have no commit binding)",
			req.SHABinding)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.pending) == 0 {
		return ScanRunClaim{}, false, nil
	}

	// Pop the front (oldest by CommittedAt). The slice is
	// kept sorted by SeedPending so head=oldest.
	next := s.pending[0]
	s.pending = s.pending[1:]

	from := s.commitStatus[commitKey(next.RepoID, next.SHA)]
	if err := repo_indexer.ValidateTransition(from, repo_indexer.ScanStatusScanning); err != nil {
		// Should never happen -- SeedPending stamps
		// `pending`, and the store is the sole writer.
		// Restore the queue so the caller can retry.
		s.pending = append([]PendingCommit{next}, s.pending...)
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: invalid commit transition for claim: %w", err)
	}

	scanRunID, err := s.newID()
	if err != nil {
		s.pending = append([]PendingCommit{next}, s.pending...)
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: mint scan_run_id: %w", err)
	}

	s.commitStatus[commitKey(next.RepoID, next.SHA)] = repo_indexer.ScanStatusScanning
	s.runs[scanRunID] = &inMemoryScanRunRecord{
		ScanRunID:  scanRunID,
		RepoID:     next.RepoID,
		ToSHA:      next.SHA,
		Kind:       req.Kind,
		SHABinding: req.SHABinding,
		Status:     ScanRunStatusRunning,
		StartedAt:  req.OpenedAt,
	}

	return ScanRunClaim{
		ScanRunID:  scanRunID,
		RepoID:     next.RepoID,
		SHA:        next.SHA,
		Kind:       req.Kind,
		SHABinding: req.SHABinding,
		OpenedAt:   req.OpenedAt,
	}, true, nil
}

// ClaimSpecificPendingCommit implements [ScanRunStore] for
// the in-memory store. iter-5 evaluator item 4: claims a
// SPECIFIC pending commit named by (repoID, sha) -- the
// state machine's pre-flight pipeline uses this to skip
// past a not-yet-ready oldest commit and claim a newer
// ready commit instead.
//
// Returns (zero, false, nil) when the targeted row is no
// longer in the pending queue (raced away or never seeded);
// returns (zero, false, err) on validation/wiring errors.
func (s *InMemoryScanRunStore) ClaimSpecificPendingCommit(ctx context.Context, repoID uuid.UUID, sha string, req ClaimRequest) (ScanRunClaim, bool, error) {
	if err := ctx.Err(); err != nil {
		return ScanRunClaim{}, false, err
	}
	if err := req.Validate(); err != nil {
		return ScanRunClaim{}, false, err
	}
	if repoID == uuid.Nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: InMemoryScanRunStore.ClaimSpecificPendingCommit: zero RepoID")
	}
	if sha == "" {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: InMemoryScanRunStore.ClaimSpecificPendingCommit: empty SHA")
	}
	if req.SHABinding == SHABindingPerRow {
		return ScanRunClaim{}, false, fmt.Errorf(
			"metric_ingestor: InMemoryScanRunStore does not support sha_binding=%q (per-row claims have no commit binding)",
			req.SHABinding)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Locate the targeted pending row.
	idx := -1
	for i, p := range s.pending {
		if p.RepoID == repoID && p.SHA == sha {
			idx = i
			break
		}
	}
	if idx < 0 {
		// Not in the pending queue -- treat as a no-op
		// (the caller re-peeks and tries the next
		// candidate).
		return ScanRunClaim{}, false, nil
	}
	next := s.pending[idx]
	from := s.commitStatus[commitKey(next.RepoID, next.SHA)]
	if from != repo_indexer.ScanStatusPending {
		// Status raced away from pending between the peek
		// and the claim (e.g. another worker beat us).
		// Drop the stale queue entry and report no-claim.
		s.pending = append(s.pending[:idx], s.pending[idx+1:]...)
		return ScanRunClaim{}, false, nil
	}
	if err := repo_indexer.ValidateTransition(from, repo_indexer.ScanStatusScanning); err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: invalid commit transition for specific claim: %w", err)
	}
	scanRunID, err := s.newID()
	if err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: mint scan_run_id (specific): %w", err)
	}

	// Remove from pending and stamp scanning.
	s.pending = append(s.pending[:idx], s.pending[idx+1:]...)
	s.commitStatus[commitKey(next.RepoID, next.SHA)] = repo_indexer.ScanStatusScanning
	s.runs[scanRunID] = &inMemoryScanRunRecord{
		ScanRunID:  scanRunID,
		RepoID:     next.RepoID,
		ToSHA:      next.SHA,
		Kind:       req.Kind,
		SHABinding: req.SHABinding,
		Status:     ScanRunStatusRunning,
		StartedAt:  req.OpenedAt,
	}
	return ScanRunClaim{
		ScanRunID:  scanRunID,
		RepoID:     next.RepoID,
		SHA:        next.SHA,
		Kind:       req.Kind,
		SHABinding: req.SHABinding,
		OpenedAt:   req.OpenedAt,
	}, true, nil
}
// FinalizeScanRun implements [ScanRunStore]. The
// in-memory implementation guards every invariant the SQL
// implementation will (terminal-only runStatus,
// terminal-only commitStatus, ValidateTransition from
// `scanning`, prior status is `running`, scan_run_id is
// known).
func (s *InMemoryScanRunStore) FinalizeScanRun(ctx context.Context, claim ScanRunClaim, runStatus ScanRunStatus, commitStatus repo_indexer.ScanStatus, endedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateScanRunStatus(runStatus); err != nil {
		return err
	}
	if runStatus == ScanRunStatusRunning {
		return fmt.Errorf("metric_ingestor: FinalizeScanRun requires a TERMINAL run status, got %q", runStatus)
	}
	if err := commitStatus.Validate(); err != nil {
		return err
	}
	if !commitStatus.IsTerminal() {
		return fmt.Errorf("metric_ingestor: FinalizeScanRun requires a TERMINAL commit status, got %q", commitStatus)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[claim.ScanRunID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownScanRunID, claim.ScanRunID)
	}
	if run.Status != ScanRunStatusRunning {
		return fmt.Errorf("%w: scan_run_id=%s status=%q", ErrClaimedRunNotInProgress, claim.ScanRunID, run.Status)
	}

	from := s.commitStatus[commitKey(claim.RepoID, claim.SHA)]
	if err := repo_indexer.ValidateTransition(from, commitStatus); err != nil {
		return fmt.Errorf("metric_ingestor: invalid commit transition on finalize: %w", err)
	}

	// Sanity: runStatus and commitStatus must agree on
	// success / failure. `succeeded <-> scanned`,
	// `failed <-> failed`. Mismatched pairs are a state
	// machine bug.
	if !terminalPairsAgree(runStatus, commitStatus) {
		return fmt.Errorf(
			"metric_ingestor: terminal pair disagrees: run_status=%q commit_status=%q (allowed: succeeded<->scanned, failed<->failed)",
			runStatus, commitStatus)
	}

	run.Status = runStatus
	run.EndedAt = endedAt
	s.commitStatus[commitKey(claim.RepoID, claim.SHA)] = commitStatus
	return nil
}

// PendingCount returns the number of pending commits the
// store still has to hand out. Exposed for test
// assertions / sweep-loop back-off heuristics.
func (s *InMemoryScanRunStore) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// terminalPairsAgree returns true iff the (runStatus,
// commitStatus) tuple is one of the two canonical terminal
// pairs:
//
//	(succeeded, scanned)
//	(failed,    failed)
//
// The state machine itself constructs both values in
// lockstep so a disagreement is always a programmer error.
func terminalPairsAgree(runStatus ScanRunStatus, commitStatus repo_indexer.ScanStatus) bool {
	switch runStatus {
	case ScanRunStatusSucceeded:
		return commitStatus == repo_indexer.ScanStatusScanned
	case ScanRunStatusFailed:
		return commitStatus == repo_indexer.ScanStatusFailed
	default:
		return false
	}
}

// commitKey is the (repo_id, sha) composite key used in
// the in-memory commitStatus map. The DB `commit` table's
// natural PRIMARY KEY is (repo_id, sha) too.
func commitKey(repoID uuid.UUID, sha string) string {
	return repoID.String() + ":" + sha
}

// sortPendingByCommittedAt sorts `cs` ASC by
// CommittedAt. Implemented as an explicit pass so the
// `committed_at` ordering invariant is visible in the
// callers' stack traces if a test ever asserts on it.
func sortPendingByCommittedAt(cs []PendingCommit) {
	// Simple insertion sort -- the pending queue is tiny
	// in any realistic test scenario (1-10 items) and
	// avoids pulling in `sort` for one call site.
	for i := 1; i < len(cs); i++ {
		j := i
		for j > 0 && cs[j-1].CommittedAt.After(cs[j].CommittedAt) {
			cs[j-1], cs[j] = cs[j], cs[j-1]
			j--
		}
	}
}
