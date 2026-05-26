package churn

// Stage 4.4 (ingest churn verb feeds materialiser,
// implementation-plan.md lines 410-425).
//
// payload.go holds the staging-table row shape ([ChurnEvent])
// and the [BuildChurnEvents] helper that maps a validated
// [Payload] -> []ChurnEvent for the [Ingester] to write into
// `clean_code.churn_event`. The Payload wire-shape itself
// lives in churn.go (kept where the foundation Stage 2.6
// inline path consumes it); this file owns ONLY the
// staging-table-bound types so the Stage 4.4 staging adapter
// is self-contained when read alone.

import (
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"
)

// ErrZeroScanRunID is returned when [BuildChurnEvents] (or the
// [Ingester] orchestration that calls it) is invoked with a
// zero ScanRunID. The Router opens the scan_run BEFORE
// dispatching to the verb, so a zero value at this boundary
// is always a wiring bug -- surface it as a structured error
// so the operator log names the boundary that failed instead
// of a stray FK violation at INSERT time.
var ErrZeroScanRunID = errors.New("churn: scan_run_id is the zero UUID (the Router must open the scan_run before invoking the verb)")

// ErrZeroNow is returned when [BuildChurnEvents] is invoked
// with a zero `now` clock reading. The staging-row
// `created_at` column needs a real timestamp so operators can
// correlate a staged row with the webhook delivery audit
// trail; a zero value is always a wiring bug.
var ErrZeroNow = errors.New("churn: now() returned the zero time")

// ErrUUIDMintFailed wraps the underlying error from the
// caller-supplied UUID minter. Surfaced as a sentinel so the
// HTTP handler stage can map the failure mode without parsing
// the inner error's free-form text.
var ErrUUIDMintFailed = errors.New("churn: minting churn_event_id failed")

// ChurnEvent is the in-process serialised form of one
// `clean_code.churn_event` row (canonical schema in
// `services/clean-code/internal/db/schema/clean_code/churn_event.sql`
// and the AUTHORITATIVE migration at
// `services/clean-code/migrations/0010_churn_event.up.sql`).
// Field names mirror the SQL columns 1:1 so a future
// PG-backed [ChurnEventWriter] can INSERT directly from this
// shape without an adapter struct.
//
// # Why per-row identity is stamped here (not at INSERT)
//
// [ChurnEvent.ChurnEventID] is minted at hydrate time (NOT
// left to the DB default) so the [Ingester] can return a
// caller-visible row-id slice and so a future end-to-end
// audit trace can correlate a webhook delivery to its
// staged rows without a follow-up SELECT.
type ChurnEvent struct {
	// ChurnEventID is the per-row PK. Minted by the
	// caller-supplied UUID generator at hydrate time; the
	// production path uses [uuid.NewV7] (time-ordered, so the
	// PK index is correlated with insert order).
	ChurnEventID uuid.UUID
	// ScanRunID is the parent ScanRun the Router opened with
	// (verb='churn', kind='external_per_row',
	// sha_binding='per_row', to_sha=NULL, payload_hash=...).
	// The FK on `churn_event.scan_run_id` is the writer-
	// ownership chain anchor.
	ScanRunID uuid.UUID
	// RepoID is the parent repo. Duplicated from
	// `scan_run.repo_id` so the materialiser's per-repo read
	// path is index-friendly without a join.
	RepoID uuid.UUID
	// SHA is the 40-char commit SHA from the
	// corresponding [PayloadRow.SHA] (per-row binding,
	// architecture Sec 4.4 line 781).
	SHA string
	// FilePath is the repo-relative path of the touched file.
	// The materialiser resolves `(repo_id, file_path)` to a
	// `scope_id` at read time via the `scope_binding` table;
	// the Stage 4.4 verb does NOT do scope resolution at
	// stage time.
	FilePath string
	// ModifiedAt is the commit's modification timestamp in
	// UTC (architecture Sec 4.4 / tech-spec Sec 4.11). The
	// materialiser's window math (`now - window_days*24h`)
	// is the only consumer.
	ModifiedAt time.Time
	// Author is the commit author identity (reserved for
	// `knowledge_index`; ignored by the
	// `modification_count_in_window` materialiser).
	// Optional -- the [Payload] wire schema marks it
	// `json:"author,omitempty"`.
	Author string
	// PayloadRowIndex is the zero-based row index inside the
	// source payload. Persisted with the row so the
	// materialiser (and ops debug tools) can reconstruct the
	// original payload order, and so the unique constraint
	// `churn_event_scan_run_row_uniq` on (scan_run_id,
	// payload_row_index) catches duplicate inserts inside the
	// same scan_run claim.
	PayloadRowIndex int
	// CreatedAt is the stage-time clock reading. Pinned to
	// the `now` function the [Ingester] holds so test fakes
	// can produce deterministic rows.
	CreatedAt time.Time
}

// BuildChurnEvents maps a validated [Payload] into the
// `clean_code.churn_event` row shape the [Ingester] writes.
// The returned slice has the SAME length as `payload.Rows`
// and preserves source order; [ChurnEvent.PayloadRowIndex]
// stamps the position so callers can recover the original
// order even after the materialiser sorts on its read.
//
// # Validation
//
// BuildChurnEvents calls [Payload.Validate] internally so
// callers cannot smuggle a malformed payload past it. The
// validation errors wrap the same sentinels the
// HTTP-handler stage already maps to 400 responses (see
// `internal/ingest/webhook/churn_verb.go` ClassifyError).
//
// On the FIRST malformed row the function returns
// (nil, error). Partial output is NEVER returned -- the
// staging insert is all-or-nothing per the writer-ownership
// invariant.
//
// # SHA normalisation
//
// `BuildChurnEvents` copies [PayloadRow.SHA] verbatim; the
// `churn_event_sha_40_hex` CHECK in the migration accepts
// either casing. A future Stage MAY lower-case the SHA
// before stage so the materialiser's per-row dedupe is
// case-stable; for v1 the application-layer
// [shaRegex] permits both and the Stage 2.6 materialiser's
// per-row dedupe groups on the verbatim string. Two payloads
// that disagree on SHA casing therefore stage as DIFFERENT
// rows but the materialiser's dedupe will still count them
// as one commit IFF the casing matches across both rows --
// production publishers consistently emit lowercase, so this
// is a documented edge case rather than a latent bug.
func BuildChurnEvents(
	scanRunID uuid.UUID,
	payload *Payload,
	now time.Time,
	newUUID func() (uuid.UUID, error),
) ([]ChurnEvent, error) {
	if err := payload.Validate(); err != nil {
		return nil, err
	}
	if scanRunID == uuid.Nil {
		return nil, ErrZeroScanRunID
	}
	if now.IsZero() {
		return nil, ErrZeroNow
	}
	if newUUID == nil {
		return nil, errors.New("churn: BuildChurnEvents received nil newUUID minter")
	}
	out := make([]ChurnEvent, 0, len(payload.Rows))
	createdAt := now.UTC()
	for i := range payload.Rows {
		r := &payload.Rows[i]
		id, err := newUUID()
		if err != nil {
			return nil, fmt.Errorf("%w (rows[%d]): %v", ErrUUIDMintFailed, i, err)
		}
		out = append(out, ChurnEvent{
			ChurnEventID:    id,
			ScanRunID:       scanRunID,
			RepoID:          payload.RepoID,
			SHA:             r.SHA,
			FilePath:        r.FilePath,
			ModifiedAt:      r.ModifiedAt.UTC(),
			Author:          r.Author,
			PayloadRowIndex: i,
			CreatedAt:       createdAt,
		})
	}
	return out, nil
}
