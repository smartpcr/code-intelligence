// Package defects is the shape + validation adapter for the
// `ingest.defects` external webhook payload (architecture
// Sec 6.4, tech-spec Sec 4.11 row 4). Stage 4.5 implements
// the v1 "store-only at the ScanRun boundary" pin -- the
// webhook parses the body, validates its shape, and DISCARDS
// it after the scan_run idempotency claim records the
// payload_hash. NO `metric_sample` row is written by this
// verb in v1.
//
// # v1 pin (tech-spec Sec 4.11 row 4, tech-spec Sec 10A pin,
// implementation-plan Stage 4.5)
//
// The Metric Ingestor accepts the payload, persists a
// `scan_run(kind='external_per_row', sha_binding='per_row',
// to_sha=NULL, payload_hash=sha256(body))` row for
// idempotency, marks it `succeeded` on parse OK, and emits
// no further state. The defect rows themselves are NOT
// persisted (architecture Sec 5.7 `ScanRun` carries
// `payload_hash` only and no payload-body / backlog field,
// per tech-spec Sec 4.11 row 4).
//
// # Why no MetricSample row in v1
//
// The architecture metric catalogue (Sec 1.4.1 + Sec 1.4.2)
// names no defect-derived foundation `metric_kind` -- the
// augmentcode-referenced `change_failure_rate` / `mttr` /
// `lead_time` kinds are v2 candidates (Sec 5.8), not v1
// catalogue entries. `MetricSample.metric_kind` is NOT NULL
// per arch Sec 5.2.1 + Sec 8.7 DDL; writing a row would
// require either an invented metric_kind or a NULL
// violation. Both are forbidden.
//
// # Future migration path (v2 follow-on, NOT this stage)
//
// A v2 stage (a) extends the `ScanRun`-or-sibling schema
// upstream to hold the payload body, (b) adds a
// defect-driven foundation `metric_kind` -- likely
// candidates: per-file `defect_count`, per-scope
// `severity_weighted_defect_density` -- as a derived metric
// in a later story (`defect_density`), and (c) materialises
// `MetricSample` rows from re-ingested payloads at that
// point. The v1 pin owner is tech-spec Sec 4.11.
//
// # What this package owns
//
//  1. The canonical Go-side struct shape of the payload --
//     [Payload], [PayloadRow] -- the webhook handler
//     unmarshals CI POST bodies into. The wire-format field
//     names are pinned by tech-spec Sec 4.11 row 4:
//     `{repo_id, file_path, sha, defect_id, severity}` (rows
//     under `rows`).
//
//  2. The shape validator [Payload.Validate] that surfaces
//     each malformed-field case as a sentinel error so the
//     HTTP handler layer ([github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/webhook.DefectsVerbHandler])
//     can map them to structured 400 / 422 responses
//     without parsing free-form error text.
//
// This package does NOT depend on the writer layer
// (`metric_ingestor`) because Stage 4.5 writes no
// `metric_sample` rows. The verb's only side-effect is the
// scan_run idempotency row owned by the webhook Router.
package defects

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gofrs/uuid"
)

// ScanRunKindExternalPerRow is the canonical `scan_run.kind`
// the `ingest.defects` verb opens its idempotency row at
// (tech-spec Sec 4.11 row 4, e2e-scenarios.md line 688). The
// literal is intentionally string-equal to
// `metric_ingestor.ScanRunKindExternalPerRow` and to
// `churn.ScanRunKindExternalPerRow`; a build-time canon-guard
// (the closed-set assertion in
// `webhook.canonicalScanRunKindForVerb`) catches drift.
const ScanRunKindExternalPerRow = "external_per_row"

// Validation errors. Surfaced as wrapped errors (NOT panics)
// because a malformed payload at the webhook boundary is a
// caller-induced runtime fault, not a writer-layer bug --
// `errors.Is` lets the HTTP handler stage map them to
// structured 400 / 422 responses without parsing strings.
var (
	// ErrEmptyRepoID is returned when [Payload.RepoID] is
	// the zero UUID. Legitimate clean-code rows reference
	// a `repo.repo_id` minted via `gen_random_uuid()` which
	// never returns zero, so the zero value always indicates
	// an uninitialised caller value.
	ErrEmptyRepoID = errors.New("defects: payload RepoID is the zero UUID")
	// ErrEmptyRows is returned when [Payload.Rows] is empty
	// or nil. A defects payload with no rows is a no-op at
	// the parser level; surface it so the operator can fix
	// the publisher (a JIRA export that filtered everything
	// out is a publisher-side bug, not a no-op success
	// case).
	ErrEmptyRows = errors.New("defects: payload Rows is empty")
	// ErrEmptySHA is returned when a [PayloadRow.SHA] is the
	// empty string. The per-row SHA contract (tech-spec
	// Sec 4.11 row 4, `sha_binding='per_row'`) requires
	// every row to carry its own commit identity.
	ErrEmptySHA = errors.New("defects: payload row has empty SHA")
	// ErrInvalidSHA is returned when a [PayloadRow.SHA] is
	// non-empty but does not match the canonical 40-character
	// hex commit-SHA shape. The pattern `^[0-9a-fA-F]{40}$`
	// accepts both lowercase (Git's default) and uppercase
	// hex; whitespace-padded, truncated, or non-hex strings
	// are REJECTED. Mirrors the `churn.ErrInvalidSHA`
	// invariant from Stage 4.4 so two verbs sharing
	// `external_per_row` enforce the same SHA shape.
	ErrInvalidSHA = errors.New("defects: payload row SHA is not a 40-character hex commit SHA")
	// ErrEmptyFilePath is returned when a [PayloadRow.FilePath]
	// is the empty string (or whitespace-only). The wire
	// contract pins `file_path` as the repo-relative path
	// of the defective file; an empty value would silently
	// drop the row's locus of defect.
	ErrEmptyFilePath = errors.New("defects: payload row has empty FilePath")
	// ErrEmptyDefectID is returned when a [PayloadRow.DefectID]
	// is the empty string. The wire contract pins
	// `defect_id` as the upstream tracker's stable
	// identifier (e.g. `JIRA-1234`); an empty value defeats
	// the publisher's traceability invariant.
	ErrEmptyDefectID = errors.New("defects: payload row has empty DefectID")
	// ErrEmptySeverity is returned when a [PayloadRow.Severity]
	// is the empty string. The wire contract pins `severity`
	// as the upstream tracker's severity literal (e.g.
	// `critical`, `major`, `minor`); v1 does not pin a
	// closed enum here (the tracker's enum is a per-deployment
	// concern documented in the operator runbook) but
	// rejects empty values so a publisher cannot accidentally
	// drop the field. The future defect-derived metric_kind
	// will lift the value into a typed bucket.
	ErrEmptySeverity = errors.New("defects: payload row has empty Severity")
)

// Payload is the canonical in-process form of an
// `ingest.defects` POST body (tech-spec Sec 4.11 row 4). The
// wire-format is `application/json` and JSON tags match the
// CI-side publisher contract pinned in tech-spec Sec 8.5
// row 4.
//
// JSON tags are populated so the [github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/webhook.DefectsVerbHandler]
// can `json.Decoder.Decode(...DisallowUnknownFields())`
// straight into this struct without an intermediate DTO.
type Payload struct {
	// RepoID is the `clean_code.repo.repo_id` the payload
	// targets. The webhook handler resolves the caller's
	// per-source signing key (architecture Sec 3.12) and
	// trusts the body-supplied RepoID as the parent for the
	// idempotency scan_run row.
	RepoID uuid.UUID `json:"repo_id"`
	// Rows is the per-defect record set. Each row carries
	// its own SHA (per-row binding, tech-spec Sec 4.11 row
	// 4); two rows with different SHAs for the same
	// (repo, file_path) are legitimate (the same file has
	// distinct defects across commits).
	Rows []PayloadRow `json:"rows"`
}

// PayloadRow is one defect record: "commit `sha` introduced
// defect `defect_id` (severity `severity`) on file
// `file_path`".
//
// # v1 store-only invariant
//
// The defect rows themselves are NOT persisted in v1
// (tech-spec Sec 4.11 row 4: "the defect payload body
// itself is NOT persisted"). The webhook validates the
// shape, records the parent scan_run's `payload_hash`, and
// DISCARDS the row set. A future defect-derived
// `metric_kind` (likely candidates: per-file `defect_count`,
// per-scope `severity_weighted_defect_density`) will lift
// these rows into the foundation-tier catalogue; that v2
// migration is owned by a later story and is out of scope
// here.
type PayloadRow struct {
	// SHA is the 40-character commit SHA the defect was
	// observed at (per-row binding, tech-spec Sec 4.11 row
	// 4). Validation is strict-hex (40 case-insensitive
	// hex characters) so a malformed value cannot flow
	// downstream when v2 lifts these rows into the catalogue.
	SHA string `json:"sha"`
	// FilePath is the repo-relative path of the defective
	// file (e.g. `services/clean-code/internal/foo.go`).
	// MUST be non-empty. The wire contract does NOT pin a
	// path normalisation (forward-slash vs back-slash); v1
	// preserves the publisher's exact bytes because we
	// discard the row anyway. v2 SHOULD normalise to
	// forward-slash before materialising the
	// defect-derived metric_kind.
	FilePath string `json:"file_path"`
	// DefectID is the upstream tracker's stable identifier
	// for the defect (e.g. `JIRA-1234`, `GH-5678`,
	// `SNOW-INC0042`). The format is per-deployment; v1
	// validates only non-emptiness.
	DefectID string `json:"defect_id"`
	// Severity is the upstream tracker's severity literal
	// (e.g. `critical`, `major`, `minor`; `S0`, `S1`, `S2`;
	// `blocker`, `high`, `low`). The format is
	// per-deployment; v1 validates only non-emptiness. A
	// future v2 stage will canonicalise into a closed enum
	// when the defect-derived metric_kind lands.
	Severity string `json:"severity"`
}

// Validate returns nil iff the payload satisfies every
// structural contract the v1 store-only verb requires.
// Validation errors are wrapped on the corresponding
// sentinel (`errors.Is(err, ErrEmptyRepoID)` etc.) so the
// HTTP handler stage can map each to a structured response
// without parsing strings.
//
// # Why so strict for a verb that discards the body
//
// Two reasons:
//
//  1. **Forward-compatibility.** When v2 lifts these rows
//     into the catalogue, every field will be load-bearing.
//     Rejecting malformed rows in v1 means the v2 migration
//     does not have to scrub historical noise out of the
//     in-flight publishers; the wire contract is strict
//     from day one.
//  2. **Scan-run idempotency hygiene.** The webhook opens a
//     durable `scan_run` row with `payload_hash=sha256(body)`.
//     A malformed body that the parser accepts would burn a
//     scan_run slot (the row exists, finalised `failed`) and
//     the publisher would never recover that hash. Rejecting
//     malformed bodies BEFORE the scan_run open keeps the
//     idempotency table clean.
func (p *Payload) Validate() error {
	if p == nil {
		return errors.New("defects: payload is nil")
	}
	if p.RepoID == uuid.Nil {
		return ErrEmptyRepoID
	}
	if len(p.Rows) == 0 {
		return ErrEmptyRows
	}
	for i := range p.Rows {
		if err := validateRow(&p.Rows[i]); err != nil {
			return fmt.Errorf("rows[%d]: %w", i, err)
		}
	}
	return nil
}

// shaRegex is the strict canonical pattern for a commit SHA:
// exactly 40 hexadecimal characters, no leading/trailing
// whitespace, case-insensitive. Mirrors the churn package's
// `shaRegex` -- the two MUST remain in sync; a future
// canonicalisation should hoist this into a shared package.
var shaRegex = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// validateRow checks a single [PayloadRow] for structural
// validity. Empty / whitespace-only string fields and
// malformed (non-40-hex) SHAs are rejected; the row is
// otherwise opaque to the v1 verb (we never read the
// severity or defect_id values downstream).
func validateRow(r *PayloadRow) error {
	if strings.TrimSpace(r.SHA) == "" {
		return ErrEmptySHA
	}
	if !shaRegex.MatchString(r.SHA) {
		return fmt.Errorf("%w (got %q)", ErrInvalidSHA, r.SHA)
	}
	if strings.TrimSpace(r.FilePath) == "" {
		return ErrEmptyFilePath
	}
	if strings.TrimSpace(r.DefectID) == "" {
		return ErrEmptyDefectID
	}
	if strings.TrimSpace(r.Severity) == "" {
		return ErrEmptySeverity
	}
	return nil
}
