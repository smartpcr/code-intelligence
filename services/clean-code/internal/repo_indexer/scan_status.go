// Package repo_indexer is the Catalog / Lifecycle writer-side
// service that owns the `clean_code.commit` and
// `clean_code.repo_event(kind='registered')` rows for every
// new SHA the service observes. Consumed by Git webhook
// deliveries AND by the CLI rescan trigger; both call
// [Indexer.OnNewSHA] which threads through a single
// [CatalogWriter] seam.
//
// # Writer-ownership invariants (architecture G1 / Sec 1.5.1 row 1)
//
// The Repo Indexer is the SOLE application writer that
// INSERTs new `clean_code.commit` rows (Management is the
// formal G1 writer of `Commit`; it DELEGATES row creation to
// this package per architecture Sec 3.3 / Sec 9 row "Worker
// -- Repo Indexer"). The Repo Indexer:
//
//  1. INSERTs `commit` rows naming ONLY the columns
//     `(repo_id, sha, parent_sha, committed_at)` -- the
//     `scan_status` column is OMITTED from the INSERT column
//     list so the schema-level `DEFAULT 'pending'`
//     (migration `0001_catalog_lifecycle.up.sql:229`) supplies
//     the initial value without any application writer naming
//     the column.
//  2. NEVER UPDATEs `commit.scan_status`; transitions
//     (`pending -> scanning -> scanned | failed`) are the
//     Metric Ingestor's exclusive responsibility per
//     architecture Sec 1.5.1 row 1.
//  3. NEVER writes any other column on `commit` after row
//     creation -- the row is append-once.
//  4. Appends EXACTLY ONE `repo_event(kind='registered')` row
//     per `repo_id` (idempotent across re-deliveries). The
//     past-tense kind literal `registered` (NOT `register`)
//     is pinned by architecture Sec 5.1.4 lines 877-884 and
//     by the database enum `clean_code.repo_event_kind`.
//
// # Why `ScanStatus` lives here
//
// The Stage 3.1 implementation-plan step pins the type to
// this package ("Add a Go enum `ScanStatus` with only the
// four canonical values"). The Repo Indexer does NOT call
// the transition guard itself -- it is the package that
// OWNS the canonical lifecycle surface, and the Metric
// Ingestor (Stage 3.2) imports
// [repo_indexer.ScanStatus] / [repo_indexer.CanTransition]
// when it transitions rows. A future refactor MAY extract
// the type to a shared `internal/lifecycle/` package if a
// third writer ever joins; until then a one-package home
// keeps the import graph flat.
package repo_indexer

import (
	"errors"
	"fmt"
)

// ScanStatus is the canonical Go-side enum for
// `clean_code.commit.scan_status`. The four allowed values
// (`pending`, `scanning`, `scanned`, `failed`) match the
// PostgreSQL enum `clean_code.commit_scan_status`
// (migration `0001_catalog_lifecycle.up.sql:87-92`) per
// architecture Sec 5.1.2 line 864. Values like `complete`,
// `superseded`, `orphaned`, or `queued` are NEVER members
// of this set -- they appear only as inputs to the
// [ScanStatus.Validate] rejection tests.
//
// The type is a string alias (NOT iota) so:
//
//   - JSON marshalling round-trips against the canonical
//     wire literal without a custom encoder.
//   - A `psql` read of `scan_status` and the Go-side string
//     are byte-identical (no enum-to-name lookup needed).
//   - `errors.Is`-style equality checks are trivial.
//
// The closed set is enforced at runtime via
// [ScanStatus.Validate]; Go cannot prevent
// `ScanStatus("garbage")` from being constructed, so every
// boundary where a `ScanStatus` enters the process MUST
// call [ScanStatus.Validate] before persisting (the
// downstream DB enum is a second safety net but a
// pre-database guard surfaces the bad value in a
// structured error rather than a Postgres SQLSTATE).
type ScanStatus string

// The four canonical values. The string literals MUST
// remain byte-equal to the labels in
// `clean_code.commit_scan_status` (migration 0001) and to
// the architecture Sec 5.1.2 line 864 list. Tests pin this
// invariant.
const (
	// ScanStatusPending is the initial state for a freshly
	// INSERTed `commit` row. Supplied by the schema-level
	// DB DEFAULT (`scan_status enum NOT NULL DEFAULT
	// 'pending'`) so NO application writer ever names the
	// column on INSERT (architecture Sec 3.3 / Sec 5.1.2).
	ScanStatusPending ScanStatus = "pending"
	// ScanStatusScanning is the in-flight state. Set by the
	// Metric Ingestor when it picks up the pending row and
	// opens its `ScanRun(kind='full', ...)` (architecture
	// Sec 4.1 step 2).
	ScanStatusScanning ScanStatus = "scanning"
	// ScanStatusScanned is the terminal success state. Set
	// by the Metric Ingestor after the foundation-tier
	// scan completes and every `metric_sample` row lands
	// (architecture Sec 4.1 step 6).
	ScanStatusScanned ScanStatus = "scanned"
	// ScanStatusFailed is the terminal failure state. Set
	// by the Metric Ingestor on any scan error, including
	// the Stage 3.2 timeout path (`scan_timeout=30min` per
	// tech-spec Sec 8.2).
	ScanStatusFailed ScanStatus = "failed"
)

// allScanStatuses is the closed package-private set. Tests
// and [AllScanStatuses] both consume it; the closed-set
// guard in [ScanStatus.Validate] iterates the slice so a
// single edit-point covers every membership check.
var allScanStatuses = []ScanStatus{
	ScanStatusPending,
	ScanStatusScanning,
	ScanStatusScanned,
	ScanStatusFailed,
}

// Sentinel errors. Surfaced as wrapped errors so callers can
// `errors.Is(err, ErrInvalidScanStatus)` to map to
// structured responses without parsing text.
var (
	// ErrInvalidScanStatus is returned by
	// [ScanStatus.Validate] when the value is not one of
	// the four canonical members. The wrap carries the
	// offending literal so the writer can log "got 'x',
	// allowed [pending scanning scanned failed]".
	ErrInvalidScanStatus = errors.New("repo_indexer: ScanStatus is not in AllScanStatuses")
	// ErrInvalidScanStatusTransition is returned by
	// [ValidateTransition] when a `from -> to` pair is not
	// on the canonical diagram. The architectural diagram
	// is:
	//
	//   pending  -> scanning
	//   scanning -> scanned
	//   scanning -> failed
	//
	// Any other pair (including same-state, terminal->any,
	// pending->terminal) is REJECTED.
	ErrInvalidScanStatusTransition = errors.New("repo_indexer: ScanStatus transition is not on the canonical diagram (pending->scanning->scanned|failed)")
)

// AllScanStatuses returns a fresh slice of the four
// canonical [ScanStatus] values in their declared order
// (`pending`, `scanning`, `scanned`, `failed`). Returned as
// a new slice each call so a caller mutating it cannot
// leak back into the package-private closed set.
//
// The closed-set property is the public contract: tests
// assert `AllScanStatuses()` is EXACTLY this set, with no
// `complete`, `superseded`, `orphaned`, or `queued`
// member. New values may only be added by a coordinated
// migration update (the PG enum) AND a code change here
// AND a tech-spec / architecture canon revision -- the
// triple-touch is the deliberate friction that protects
// the C22 closed-set invariant.
func AllScanStatuses() []ScanStatus {
	out := make([]ScanStatus, len(allScanStatuses))
	copy(out, allScanStatuses)
	return out
}

// String implements [fmt.Stringer]. Returns the canonical
// wire literal verbatim so a `%s`-formatted log line
// matches the `psql` representation byte-for-byte.
func (s ScanStatus) String() string {
	return string(s)
}

// Validate returns nil iff `s` is one of the four
// canonical members and a wrapped [ErrInvalidScanStatus]
// otherwise. The wrap carries the offending literal for
// the writer's structured log line.
//
// Every boundary that admits a [ScanStatus] from
// untrusted bytes (JSON decode, gRPC request, DB row
// scan in a future Phase 3.2 implementation) MUST call
// this before persisting -- relying solely on the
// downstream PG enum would surface bad values as opaque
// `SQLSTATE 22P02` errors instead of structured 400s.
func (s ScanStatus) Validate() error {
	for _, ok := range allScanStatuses {
		if s == ok {
			return nil
		}
	}
	return fmt.Errorf("%w: got %q (allowed: %v)", ErrInvalidScanStatus, string(s), allScanStatuses)
}

// IsTerminal returns true iff `s` is one of the two
// end-states (`scanned`, `failed`). Used by future
// observability surfaces (e.g. "% of commits stuck in
// non-terminal states"); not relied upon by the
// transition guard.
func (s ScanStatus) IsTerminal() bool {
	return s == ScanStatusScanned || s == ScanStatusFailed
}

// CanTransition returns true iff `from -> to` is on the
// canonical [ScanStatus] transition diagram. The diagram
// is the four-edge graph:
//
//	pending  -> scanning   (Metric Ingestor picks up the row)
//	scanning -> scanned    (foundation scan + samples landed)
//	scanning -> failed     (foundation scan errored / timed out)
//
// Every other (from, to) pair returns false. In
// particular:
//
//   - Same-state edges (`pending -> pending`, etc.) are
//     REJECTED -- a no-op UPDATE is a bug at this layer
//     and should be caught by the transition guard, not
//     silently absorbed.
//   - Terminal -> any (`scanned -> *`, `failed -> *`) is
//     REJECTED -- once a row is scanned or failed it
//     stays put; a rescan opens a NEW commit row only when
//     the SHA changes, otherwise the operator's
//     `mgmt.rescan` verb opens a fresh `scan_run` and the
//     `commit.scan_status` value is unaffected.
//   - `pending -> scanned`, `pending -> failed`,
//     `pending -> pending` are all REJECTED -- the scan
//     MUST visit `scanning` to record the in-flight state.
//
// Used by the Metric Ingestor (Stage 3.2) before each
// UPDATE; the Repo Indexer itself never calls this -- it
// only INSERTs new `commit` rows.
func CanTransition(from, to ScanStatus) bool {
	switch from {
	case ScanStatusPending:
		return to == ScanStatusScanning
	case ScanStatusScanning:
		return to == ScanStatusScanned || to == ScanStatusFailed
	case ScanStatusScanned, ScanStatusFailed:
		return false
	default:
		return false
	}
}

// ValidateTransition is the checked variant of
// [CanTransition]: returns nil iff the edge is on the
// diagram, otherwise wraps [ErrInvalidScanStatusTransition]
// with the offending pair. The wrap is structured so
// callers can `errors.Is` without parsing text.
//
// The function ALSO validates the membership of both
// arguments first ([ScanStatus.Validate]) so an unknown
// `from` or `to` surfaces as [ErrInvalidScanStatus]
// rather than masquerading as a transition error.
func ValidateTransition(from, to ScanStatus) error {
	if err := from.Validate(); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if err := to.Validate(); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidScanStatusTransition, from, to)
	}
	return nil
}
