package churn

// Stage 4.4 (ingest churn verb feeds materialiser,
// implementation-plan.md lines 410-425).
//
// scan_run.go holds the value types and validators the
// [Ingester] uses to receive a pre-opened scan_run handle
// from the webhook Router. The Router (in
// `internal/ingest/webhook`) is the layer that actually
// claims the durable `clean_code.scan_run` row -- it computes
// `payload_hash = sha256(body)` and atomically claims the
// row via the partial unique index on (verb, payload_hash)
// from migration 0009 BEFORE dispatching to the verb
// handler.
//
// This package owns the CANONICAL CONSTANTS (verb, kind,
// sha_binding, to_sha) the Router stamps on the scan_run
// row when the verb is `ingest.churn`, plus the
// [ScanRunHandle] value type that the [Ingester] consumes
// and the [ValidateScanRunHandle] guard that PANICS if a
// caller tries to drive the [Ingester] under a non-canonical
// scan_run shape (so a regression in the Router wiring
// surfaces as a loud bug at the verb boundary, not a
// silently mis-attributed `churn_event` row).
//
// # Why the Ingester does NOT open scan_run itself
//
// An earlier iter-1 design had the [Ingester] open its own
// scan_run via a `ScanRunOpener` interface. The rubber-duck
// design review flagged this as a parallel-architecture
// bug: the Router ALREADY opens scan_run (with
// payload_hash idempotency) before any verb handler runs.
// A second opener inside the verb would race the Router's
// claim, produce duplicate `scan_run` rows under the same
// `payload_hash` (or, worse, claim a DIFFERENT scan_run_id
// than the one the Router cached), and confuse the (verb,
// payload_hash) idempotency anchor that migration 0009's
// partial unique index relies on. The structural fix: the
// [Ingester] CONSUMES a pre-opened handle from the Router;
// the Router remains the SOLE opener.

import (
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"
)

// Canonical scan_run metadata for the `ingest.churn` verb.
// Pinned here so a `grep -nF "ingest.churn"` over the verb
// tree finds the one definition site and a regression that
// drifts any of these literals surfaces as a build break.
//
// Per the verb-to-kind matrix in e2e-scenarios.md lines
// 684-688, `ingest.churn` MUST open its scan_run with:
//
//   - Verb       = "churn"             (URL path segment)
//   - Kind       = "external_per_row"
//   - SHABinding = "per_row"
//   - ToSHA      = ""                  (NULL at the DB layer)
//
// The Router stamps these on the `clean_code.scan_run` row
// (migration 0001's scan_run_sha_binding_consistent CHECK
// enforces sha_binding<->to_sha; migration 0009's
// scan_run_verb_payload_hash_consistent CHECK enforces
// verb<->payload_hash).
const (
	// Verb is the URL path segment that routes a POST to the
	// `ingest.churn` handler.
	Verb = "churn"
	// Kind is the canonical `scan_run.kind` enum literal the
	// Router writes for a churn delivery.
	Kind = "external_per_row"
	// SHABinding is the canonical `scan_run.sha_binding`
	// enum literal. `per_row` because each staged
	// `clean_code.churn_event` row carries its own SHA
	// (architecture Sec 4.4 line 781) -- there is no single
	// representative SHA for the whole run. The Stage 4.4
	// verb writes ZERO `metric_sample` rows directly; the
	// `modification_count_in_window` materialiser is the sole
	// writer of that metric_kind on a later pass.
	SHABinding = "per_row"
)

// ErrScanRunHandleZeroID is returned when [ValidateScanRunHandle]
// receives a zero scan_run_id. The Router opens the scan_run
// before dispatch; a zero value is always a wiring bug.
var ErrScanRunHandleZeroID = errors.New("churn: ScanRunHandle.ScanRunID is the zero UUID")

// ErrScanRunHandleZeroRepoID is returned when
// [ValidateScanRunHandle] receives a zero repo_id. The repo
// FK on `churn_event.repo_id` would fail at INSERT time
// anyway -- surface it earlier so the operator log names
// the boundary that failed.
var ErrScanRunHandleZeroRepoID = errors.New("churn: ScanRunHandle.RepoID is the zero UUID")

// ErrScanRunHandleWrongVerb is returned when
// [ValidateScanRunHandle] receives a non-`churn` verb. The
// [Ingester] services exactly one verb; a different value
// is a wiring bug -- the Router routes per-verb.
var ErrScanRunHandleWrongVerb = errors.New("churn: ScanRunHandle.Verb must be \"churn\"")

// ErrScanRunHandleWrongKind is returned when
// [ValidateScanRunHandle] receives a non-`external_per_row`
// kind. The verb-to-kind matrix is pinned in e2e-scenarios.md
// lines 684-688; the per-row-SHA semantics of the
// `modification_count_in_window` materialiser would corrupt
// active-row state under any other kind.
var ErrScanRunHandleWrongKind = errors.New("churn: ScanRunHandle.Kind must be \"external_per_row\"")

// ErrScanRunHandleWrongSHABinding is returned when
// [ValidateScanRunHandle] receives a non-`per_row`
// sha_binding. The migration 0001
// scan_run_sha_binding_consistent CHECK would reject the
// scan_run INSERT anyway; surface earlier with a structured
// error.
var ErrScanRunHandleWrongSHABinding = errors.New("churn: ScanRunHandle.SHABinding must be \"per_row\"")

// ErrScanRunHandleToSHANotEmpty is returned when
// [ValidateScanRunHandle] receives a non-empty ToSHA. The
// `scan_run_sha_binding_consistent` CHECK (migration 0001)
// rejects (sha_binding='per_row', to_sha IS NOT NULL).
var ErrScanRunHandleToSHANotEmpty = errors.New("churn: ScanRunHandle.ToSHA must be empty (per_row binding -> NULL to_sha)")

// ErrScanRunHandleZeroOpenedAt is returned when
// [ValidateScanRunHandle] receives a zero OpenedAt. The
// Router stamps `time.Now()` on every open; a zero value is
// always a wiring bug.
var ErrScanRunHandleZeroOpenedAt = errors.New("churn: ScanRunHandle.OpenedAt is the zero time")

// ScanRunHandle is the pre-opened scan_run context the
// [Ingester] writes events under. Constructed by the
// webhook Router AFTER it claims the durable
// `clean_code.scan_run` row via
// `ScanRunRepository.OpenExternal` (Stage 4.1
// implementation-plan); the verb handler threads the result
// here.
//
// The handle is a value type -- pass by copy. Fields mirror
// the subset of `scan_run` columns the staging path needs;
// `payload_hash` is intentionally NOT exposed here because
// the [Ingester] does NOT re-validate idempotency (the
// Router already did, against the durable partial unique
// index from migration 0009).
type ScanRunHandle struct {
	// ScanRunID is the freshly-claimed (or replay-resolved)
	// `clean_code.scan_run.scan_run_id` UUID. Stamped on
	// every emitted [ChurnEvent.ScanRunID].
	ScanRunID uuid.UUID
	// RepoID is the parent repo's `clean_code.repo.repo_id`.
	// MUST match [Payload.RepoID] -- the [Ingester]
	// cross-checks and refuses to stage when they disagree.
	RepoID uuid.UUID
	// Verb is the URL-path segment (`churn`). The
	// [Ingester] PANICs / errors when this is anything
	// else; the const exists so a refactor that adds a new
	// verb cannot accidentally route through the churn
	// Ingester.
	Verb string
	// Kind is the `scan_run.kind` literal (`external_per_row`).
	Kind string
	// SHABinding is the `scan_run.sha_binding` literal
	// (`per_row`).
	SHABinding string
	// ToSHA is the `scan_run.to_sha` value -- empty string
	// (mapped to SQL NULL) because per_row leaves to_sha
	// NULL per architecture Sec 4.4 line 781.
	ToSHA string
	// OpenedAt is the scan_run's `started_at` -- the
	// Router's `time.Now()` at open time. The [Ingester]
	// stamps [ChurnEvent.CreatedAt] from its own clock
	// (separately) but threads OpenedAt here so audit logs
	// can correlate.
	OpenedAt time.Time
}

// ValidateScanRunHandle returns nil iff the handle satisfies
// every canonical-shape invariant the staging path depends
// on. The checks pin the verb-to-kind matrix from
// e2e-scenarios.md lines 684-688 and the sha-binding<->to_sha
// invariant from migration 0001 at the verb boundary, so a
// regression in the Router wiring fails LOUDLY here rather
// than silently writing a mis-attributed `churn_event` row.
//
// Returns wrapped sentinel errors so a future test (or a
// future Router refactor) can branch on `errors.Is(err,
// ErrScanRunHandleWrongVerb)` etc. without parsing free-form
// text.
//
// Validation order (cheapest first):
//
//  1. ScanRunID != zero
//  2. RepoID != zero
//  3. OpenedAt != zero
//  4. Verb == "churn"
//  5. Kind == "external_per_row"
//  6. SHABinding == "per_row"
//  7. ToSHA == ""
//
// All seven checks must pass; the first failure short-
// circuits with the matching sentinel.
func ValidateScanRunHandle(h ScanRunHandle) error {
	if h.ScanRunID == uuid.Nil {
		return ErrScanRunHandleZeroID
	}
	if h.RepoID == uuid.Nil {
		return ErrScanRunHandleZeroRepoID
	}
	if h.OpenedAt.IsZero() {
		return ErrScanRunHandleZeroOpenedAt
	}
	if h.Verb != Verb {
		return fmt.Errorf("%w (got %q)", ErrScanRunHandleWrongVerb, h.Verb)
	}
	if h.Kind != Kind {
		return fmt.Errorf("%w (got %q)", ErrScanRunHandleWrongKind, h.Kind)
	}
	if h.SHABinding != SHABinding {
		return fmt.Errorf("%w (got %q)", ErrScanRunHandleWrongSHABinding, h.SHABinding)
	}
	if h.ToSHA != "" {
		return fmt.Errorf("%w (got %q)", ErrScanRunHandleToSHANotEmpty, h.ToSHA)
	}
	return nil
}
