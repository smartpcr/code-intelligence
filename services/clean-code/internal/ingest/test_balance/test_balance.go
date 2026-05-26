// Package test_balance is the writer-side adapter for the
// `ingest.test_balance` external webhook payload (architecture
// Sec 1.4.2 / Sec 6.4 line 1395 / lines 1367-1368, tech-spec
// Sec 4.1.1 / Sec 4.11 lines 429-432 / Sec 8.5 lines 987-992,
// implementation-plan Stage 4.3).
//
// What this package owns:
//
//  1. The canonical Go shape ([Payload], [PayloadRow])
//     the webhook handler unmarshals POST bodies into. The
//     wire shape is the bare JSON row-array
//     `[{scope_id, attempt_count, pass_count}, ...]` per
//     `e2e-scenarios.md:648` and
//     `implementation-plan.md:396,400`. Iter-1 incorrectly
//     wrapped this in an envelope `{repo_id, sha, rows}`;
//     iter-2 corrects to the documented bare-array shape
//     and moves `(repo_id, sha)` onto the HTTP request
//     headers ([webhook.RepoIDHeader], [webhook.SHAHeader]).
//
//  2. The [Writer] that turns a validated [Payload] +
//     [ScanRunContext] into [metric_ingestor.MetricSampleRecord]
//     writes -- one `pass_first_try_ratio` sample per
//     [PayloadRow] -- and persists them through a
//     [metric_ingestor.MetricSampleWriter] in a single atomic
//     batch. The Writer ALSO upserts a `scope_binding` row
//     per emitted record via a [ScopeResolver] dependency
//     (Stage 4.3 evaluator iter-1 #3 fix), so the
//     `metric_sample.scope_id REFERENCES scope_binding(scope_id)`
//     FK (migration `0002_measurement.up.sql:266-268`) is
//     satisfied at insert time.
//
// # Why not route through [metric_ingestor.Ingestor]
//
// The shared [metric_ingestor.Ingestor.Run] coordinator only
// accepts `kind in {full, delta, external_per_row}` (see
// `metric_ingestor.AllowedScanRunKinds`); test_balance is
// `kind='external_single'` (one SHA per call, architecture Sec
// 6.4 lines 1367-1368). The semantics diverge: the test_balance
// pipeline does NOT run a materialiser -- the publisher already
// computed the ratios upstream; the writer only stamps each row
// as a `pass_first_try_ratio` sample. A separate [Writer] keeps
// the Ingestor's churn-sweep invariants intact while exposing
// the [metric_ingestor.MetricSampleWriter] seam unchanged.
//
// # Single-SHA contract (architecture Sec 6.4)
//
// Every row in the payload shares the parent
// [metric_ingestor.ScanRunContext.SHA] (one SHA per call,
// supplied via the [webhook.SHAHeader] HTTP header). The
// Router opens a single
// `scan_run(kind='external_single', sha_binding='single',
// to_sha=ScanRunContext.SHA)` row and the Writer stamps every
// emitted `metric_sample.sha` with the SAME value. This is the
// OPPOSITE of `ingest.churn` (`external_per_row`) where each
// emitted row carries its own SHA.
//
// # Scope identity for opaque publisher scope_ids
//
// The publisher sends an opaque `scope_id` string (e.g.
// `"S1"`, a test-suite name) -- it does NOT know about the
// AST taxonomy (file, class, method, ...). The Writer maps
// every publisher-supplied scope_id to
// [scope.KindFile] with a NAMESPACED canonical signature
// path component (`.ingested/test_balance/<escaped scope_id>`)
// so:
//
//   - The `scope_binding` natural-key lookup is stable
//     across calls (same publisher scope_id => same
//     scope_binding row).
//   - The namespace prefix prevents a publisher's opaque
//     scope_id from accidentally colliding with a real
//     repo-relative file path written by the AST adapter
//     (e.g. the publisher sends `"internal/foo.go"`
//     hoping to alias the file scope -- iter-1 would have
//     mapped this onto the SAME scope_id as the AST's
//     `internal/foo.go` file scope; iter-2 stamps it as
//     `.ingested/test_balance/internal/foo.go` which is a
//     distinct natural-key bucket).
//
// # Skip-on-zero, clamp-to-[0,1]
//
// Rows with `attempt_count == 0` are SKIPPED (no MetricSample)
// to avoid the NaN that `0/0` would produce -- the publisher
// is expected to omit untested scopes, but a defensive skip
// here keeps a buggy publisher from poisoning the metric.
// Negative counts are REJECTED (validation error). When
// `pass_count > attempt_count` the ratio is CLAMPED to 1.0
// rather than rejected so a publisher with an over-count bug
// still produces a valid `[0,1]`-bounded sample (this matches
// the e2e scenario name `ratio-clamped-zero-to-one`).
package test_balance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// MetricKind is the closed-set `metric_kind` literal every
// emitted `metric_sample` carries (architecture Sec 1.4.2 row 2,
// tech-spec Sec 4.1.1).
const MetricKind = "pass_first_try_ratio"

// MetricVersion is the schema version stamped onto every
// emitted sample. Pinned at `1` (v1 of the recipe definition)
// per the convention shared with the foundation-tier recipes.
const MetricVersion = 1

// ScanRunKindExternalSingle is the canonical `scan_run.kind`
// literal the Router opens for every test_balance call
// (architecture Sec 6.4 lines 1367-1368, tech-spec Sec 4.11
// lines 429-432). Re-exported from
// [metric_ingestor.ScanRunKindExternalSingle] so callers in
// this package do not have to import the metric_ingestor
// package just for the literal.
const ScanRunKindExternalSingle = metric_ingestor.ScanRunKindExternalSingle

// SHABindingSingle is the canonical `scan_run.sha_binding`
// literal paired with [ScanRunKindExternalSingle]. Pinned at
// the package boundary so a future drift in the SHA-binding
// model surfaces as a compile error against this constant,
// not as a silent FK mismatch.
const SHABindingSingle = metric_ingestor.SHABindingSingle

// ScopePathNamespace is the path-prefix segment the Writer
// stamps onto every publisher-supplied `scope_id` before
// minting the [storage.ScopeBindingCandidate.CanonicalSignature].
// Pinned at the package boundary so:
//
//   - The natural-key lookup `(repo_id, scope_kind=file,
//     canonical_signature)` in `clean_code.scope_binding`
//     bucket-separates ingested-pack test_balance scopes
//     from the AST adapter's `KindFile` scopes (which use
//     real repo-relative paths like `internal/foo.go`).
//
//   - A publisher posting `"internal/foo.go"` as a
//     test_balance scope_id maps to
//     `.ingested/test_balance/internal/foo.go` (a distinct
//     scope_binding row) rather than aliasing onto the AST
//     adapter's file scope.
//
// The literal prefix starts with a leading dot so the
// in-namespace tokens are NOT valid repo-relative paths on
// any filesystem the AST adapter walks (no AST-emitted
// `KindFile.Path` begins with `.`).
const ScopePathNamespace = ".ingested/test_balance/"

// Validation errors. Surfaced as wrapped errors (NOT panics)
// because a malformed payload at the webhook boundary is a
// caller-induced runtime fault; `errors.Is` lets the verb
// handler stage map each to a structured `400` response.
var (
	// ErrEmptyRows is returned when the payload row-array
	// is empty or nil. A test_balance request with no rows
	// would be a no-op; surface it so the operator can fix
	// the publisher.
	ErrEmptyRows = errors.New("test_balance: payload Rows is empty")
	// ErrEmptyScopeID is returned when a [PayloadRow.ScopeID]
	// is the empty string after trimming. The Writer cannot
	// mint a durable scope_id from an empty identifier.
	ErrEmptyScopeID = errors.New("test_balance: payload row has empty ScopeID")
	// ErrNegativeAttemptCount is returned when a
	// [PayloadRow.AttemptCount] is < 0. Negative attempts
	// are nonsensical -- the test runner could not have
	// "tried -1 times".
	ErrNegativeAttemptCount = errors.New("test_balance: payload row has negative AttemptCount")
	// ErrNegativePassCount is returned when a
	// [PayloadRow.PassCount] is < 0. Negative passes are
	// nonsensical.
	ErrNegativePassCount = errors.New("test_balance: payload row has negative PassCount")
	// ErrScopeResolutionFailed wraps any error the writer's
	// [ScopeResolver.ResolveScopeIDs] returns. Distinct
	// sentinel so the verb-handler classifier can map it to
	// `500 SCOPE_RESOLUTION_FAILED` (the writer cannot
	// distinguish a transient DB failure from a producer-
	// induced one at this seam).
	ErrScopeResolutionFailed = errors.New("test_balance: scope_id resolution failed")
)

// Payload is the canonical in-process form of an
// `ingest.test_balance` POST body. The wire format is the
// bare JSON row-array
// `[{"scope_id":...,"attempt_count":...,"pass_count":...}, ...]`
// per `e2e-scenarios.md:648` and
// `implementation-plan.md:396,400`.
//
// The verb handler reads `(repo_id, sha)` from request
// HEADERS ([webhook.RepoIDHeader], [webhook.SHAHeader])
// rather than the body (architecture Sec 6.4 line 1395:
// `ingest.test_balance(repo_id, sha, payload)`); the Router
// folds both header values into `payload_hash` so
// idempotency is unique per `(verb, repo, sha, body)`.
//
// JSON encodes / decodes as a bare array (not an object)
// because the underlying type is a slice.
type Payload []PayloadRow

// PayloadRow is one test_balance record: "scope `scope_id`
// was attempted `attempt_count` times and passed
// `pass_count` of them at the parent request's SHA". The
// publisher (CI / test-runner adapter) is responsible for
// the upstream aggregation; the writer does NOT recompute
// from raw test events.
type PayloadRow struct {
	// ScopeID is the publisher-supplied opaque string
	// identifier of the scope (e.g. `"S1"`, a test-suite
	// name, a file path). The Writer maps the string to a
	// durable `scope_binding.scope_id` UUID via the
	// natural-key lookup
	// `(repo_id, scope_kind='file',
	//  canonical_signature=BuildFile(repo_url,
	//                                ScopePathNamespace + scope_id))`
	// -- two POSTs with the SAME `(repo_id, scope_id)`
	// yield the SAME `metric_sample.scope_id` (active-row
	// dedupe relies on this stability).
	ScopeID string `json:"scope_id"`
	// AttemptCount is the count of test attempts at the
	// parent request's SHA. Rows with `attempt_count == 0`
	// are SKIPPED by the writer (no sample emitted) to
	// avoid the NaN from `0/0`. Negative values are
	// REJECTED with [ErrNegativeAttemptCount].
	AttemptCount int `json:"attempt_count"`
	// PassCount is the count of attempts that passed on
	// the first try (the recipe is "pass on first try").
	// Negative values are REJECTED with
	// [ErrNegativePassCount]; values exceeding
	// `attempt_count` are clamped (the ratio caps at 1.0)
	// rather than rejected, so a publisher with an
	// over-count bug still produces a valid `[0,1]`-bounded
	// sample.
	PassCount int `json:"pass_count"`
}

// Validate returns nil iff the payload satisfies every
// structural contract the writer depends on. Errors are
// wrapped on the package sentinels (`errors.Is(err,
// ErrEmptyRows)` etc.) so the verb-handler stage maps each
// to a structured `400` response without parsing strings.
//
// Skip-on-zero attempts: rows with `attempt_count == 0`
// are NOT rejected here -- the [Writer.Run] method skips
// them silently. Validate only refuses NEGATIVE counts
// (which can never represent a real measurement).
func (p Payload) Validate() error {
	if len(p) == 0 {
		return ErrEmptyRows
	}
	for i := range p {
		if err := validateRow(&p[i]); err != nil {
			return fmt.Errorf("rows[%d]: %w", i, err)
		}
	}
	return nil
}

func validateRow(r *PayloadRow) error {
	if strings.TrimSpace(r.ScopeID) == "" {
		return ErrEmptyScopeID
	}
	if r.AttemptCount < 0 {
		return fmt.Errorf("%w (got %d)", ErrNegativeAttemptCount, r.AttemptCount)
	}
	if r.PassCount < 0 {
		return fmt.Errorf("%w (got %d)", ErrNegativePassCount, r.PassCount)
	}
	return nil
}

// ScopeResolver is the seam the [Writer] uses to translate
// publisher-supplied opaque scope_id strings into durable
// `clean_code.scope_binding.scope_id` UUIDs. Implementations:
//
//   - Production:
//     [metric_ingestor.PGScopeBindingResolver] -- upserts
//     `scope_binding` rows via [storage.ScopeBindingWriter]
//     under the canonical natural key, satisfying the
//     `metric_sample.scope_id REFERENCES scope_binding(scope_id)`
//     FK at first write (migration
//     `0002_measurement.up.sql:266-268`).
//
//   - In-memory / scaffold:
//     [metric_ingestor.DefaultFoundationScopeResolver] -- mints
//     a deterministic UUIDv5 in process without persisting,
//     suitable for unit tests and the in-memory writer.
//
// The interface is intentionally identical to
// [metric_ingestor.FoundationScopeResolver] so the same
// production resolver wired for the foundation-tier dispatch
// path can be reused here verbatim.
type ScopeResolver interface {
	// ResolveScopeIDs returns the durable
	// `clean_code.scope_binding.scope_id` UUIDs for the
	// given `refs` slice. `ids[i]` corresponds to
	// `refs[i]`. A length mismatch from the resolver is a
	// contract violation the writer surfaces to the caller
	// (it would corrupt the records-to-refs zip).
	ResolveScopeIDs(ctx context.Context, repoID uuid.UUID, refs []recipes.ScopeRef, sha string) ([]uuid.UUID, error)
}

// Result summarises a single [Writer.Run] call's outcome.
// Designed for the verb handler's response envelope and for
// the tests' assertions.
type Result struct {
	// SamplesWritten is the count of records appended by
	// [metric_ingestor.MetricSampleWriter.WriteBatch] --
	// one per row whose `attempt_count > 0`.
	SamplesWritten int
	// RowsSkipped is the count of payload rows skipped
	// because `attempt_count == 0` (no sample emitted).
	// The webhook handler exposes the count in its response
	// detail so the publisher can detect a degraded batch
	// without parsing logs.
	RowsSkipped int
}

// Writer is the test_balance writer-side orchestrator. One
// [Writer.Run] call corresponds to one webhook delivery -->
// one durable [metric_ingestor.ScanRunContext] -->
// `SamplesWritten` rows in `metric_sample`. The writer is
// stateless past its dependencies and is safe for concurrent
// use iff the underlying [metric_ingestor.MetricSampleWriter]
// and [ScopeResolver] are.
type Writer struct {
	writer  metric_ingestor.MetricSampleWriter
	scopes  ScopeResolver
	newUUID func() (uuid.UUID, error)
}

// NewWriter returns a [Writer] bound to `w` and `scopes`.
// PANICS when `w` is nil OR `scopes` is nil -- the
// composition root is the only legitimate caller and a nil
// dependency is always a wiring bug that should fail loudly
// at startup. The rubber-duck audit (iter-2) called out a
// nil-resolver fallback as a vector for silently
// reintroducing the production FK gap; iter-2 closes that by
// requiring a non-nil resolver. Tests inject
// [metric_ingestor.DefaultFoundationScopeResolver]{} as the
// explicit in-process scaffold resolver.
//
// SampleID generation defaults to [uuid.NewV7]; tests can
// override via [NewWriterWithUUID].
func NewWriter(w metric_ingestor.MetricSampleWriter, scopes ScopeResolver) *Writer {
	return NewWriterWithUUID(w, scopes, uuid.NewV7)
}

// NewWriterWithUUID is the test-only constructor that lets a
// caller inject a deterministic UUID generator for
// [metric_ingestor.MetricSampleRecord.SampleID] minting.
// PANICS on a nil writer, nil scopes, or nil newUUID.
func NewWriterWithUUID(w metric_ingestor.MetricSampleWriter, scopes ScopeResolver, newUUID func() (uuid.UUID, error)) *Writer {
	if w == nil {
		panic("test_balance: NewWriter received nil MetricSampleWriter")
	}
	if scopes == nil {
		panic("test_balance: NewWriter received nil ScopeResolver")
	}
	if newUUID == nil {
		panic("test_balance: NewWriter received nil newUUID generator")
	}
	return &Writer{writer: w, scopes: scopes, newUUID: newUUID}
}

// Run validates the supplied [metric_ingestor.ScanRunContext]
// and bare-array `rows` and persists one [MetricSampleRecord]
// per row whose `attempt_count > 0`. Validation surface:
//
//   - `scanRun.ID` must be non-zero.
//   - `scanRun.Kind` must equal [ScanRunKindExternalSingle].
//   - `scanRun.RepoID` must be non-zero.
//   - `scanRun.SHA` must be non-empty (the verb-handler stage
//     pre-validates the 40-char hex shape so the writer does
//     not re-impose the regex here).
//   - `rows` must be non-empty and each row must pass
//     [validateRow].
//
// On success returns [Result] with the emit/skip counters.
// The write is ATOMIC: a single
// [metric_ingestor.MetricSampleWriter.WriteBatch] call
// carries every emitted record. The scope_binding upsert
// runs in a SEPARATE [ScopeResolver.ResolveScopeIDs] call
// BEFORE the metric_sample write so the
// `metric_sample.scope_id` FK is satisfied at INSERT time.
// On writer failure the returned error wraps
// [metric_ingestor.ErrWriterFailure] so the verb-handler
// classifier maps it to `500 WRITER_FAILURE`. On scope-
// resolver failure the error wraps
// [ErrScopeResolutionFailed].
func (w *Writer) Run(ctx context.Context, scanRun metric_ingestor.ScanRunContext, rows Payload) (Result, error) {
	if err := w.validateScanRun(scanRun); err != nil {
		return Result{}, err
	}
	if err := rows.Validate(); err != nil {
		return Result{}, err
	}

	// First pass: build the (kept-rows, refs) tuple. We
	// resolve scope_ids in ONE batch so the
	// [storage.ScopeBindingWriter]'s advisory-lock + UPSERT
	// path is amortised across every emitted row -- per
	// the iter-3 dispatcher precedent.
	kept := make([]int, 0, len(rows))
	refs := make([]recipes.ScopeRef, 0, len(rows))
	skipped := 0
	for i := range rows {
		row := &rows[i]
		if row.AttemptCount == 0 {
			skipped++
			continue
		}
		kept = append(kept, i)
		refs = append(refs, scopeRefForRow(row))
	}

	if len(refs) == 0 {
		return Result{
			SamplesWritten: 0,
			RowsSkipped:    skipped,
		}, nil
	}

	scopeIDs, scopeErr := w.scopes.ResolveScopeIDs(ctx, scanRun.RepoID, refs, scanRun.SHA)
	if scopeErr != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrScopeResolutionFailed, scopeErr)
	}
	if len(scopeIDs) != len(refs) {
		return Result{}, fmt.Errorf("%w: resolver returned %d ids for %d refs (length mismatch)",
			ErrScopeResolutionFailed, len(scopeIDs), len(refs))
	}

	records := make([]metric_ingestor.MetricSampleRecord, 0, len(refs))
	for outIdx, rowIdx := range kept {
		row := &rows[rowIdx]
		ratio := float64(row.PassCount) / float64(row.AttemptCount)
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		sampleID, err := w.newUUID()
		if err != nil {
			return Result{}, fmt.Errorf("test_balance: SampleID generation failed at rows[%d]: %w", rowIdx, err)
		}
		records = append(records, metric_ingestor.MetricSampleRecord{
			SampleID:      sampleID,
			RepoID:        scanRun.RepoID,
			SHA:           scanRun.SHA,
			ScopeID:       scopeIDs[outIdx],
			MetricKind:    MetricKind,
			MetricVersion: MetricVersion,
			Pack:          recipes.PackIngested,
			Source:        recipes.SourceIngested,
			Value:         ratio,
			ProducerRunID: scanRun.ID,
		})
	}

	if len(records) > 0 {
		if err := w.writer.WriteBatch(ctx, records); err != nil {
			return Result{}, fmt.Errorf("%w: %v", metric_ingestor.ErrWriterFailure, err)
		}
	}

	return Result{
		SamplesWritten: len(records),
		RowsSkipped:    skipped,
	}, nil
}

// scopeRefForRow builds the [recipes.ScopeRef] the writer
// hands to [ScopeResolver.ResolveScopeIDs] for a single
// [PayloadRow]. Pinned semantics:
//
//   - `Kind = scope.KindFile`. The publisher's scope_id is
//     opaque to us; we treat each scope as a
//     `KindFile`-shaped natural-key bucket because
//     `BuildFile` is the only existing `scope.Build*` helper
//     that accepts a free-form path-shaped token (vs
//     `BuildMethod` which requires Params).
//
//   - `QualifiedName = Path = LocalID = ScopePathNamespace + scope_id`.
//     The path namespace prefix isolates ingested-pack
//     test_balance scopes from real AST-derived file scopes
//     in the SAME repo's `scope_binding` table.
//     `QualifiedName` and `Path` are both non-empty so the
//     [PGScopeBindingResolver]'s "QualifiedName must be
//     non-empty (except for KindRepo)" check passes.
//     `LocalID` is non-empty so the dispatcher's
//     "LocalID populated" defensive guard passes (the
//     recipes' panic in `buildMetricSampleDraft` would not
//     trigger here because we are NOT in the recipe path,
//     but iter-2 still populates LocalID for shape
//     uniformity).
//
// The publisher's scope_id is TRIMMED of leading/trailing
// whitespace before namespacing; an empty scope_id has
// already failed [validateRow] by the time we get here.
func scopeRefForRow(row *PayloadRow) recipes.ScopeRef {
	id := strings.TrimSpace(row.ScopeID)
	path := ScopePathNamespace + id
	return recipes.ScopeRef{
		Kind:          scope.KindFile,
		QualifiedName: path,
		Path:          path,
		LocalID:       path,
	}
}

// validateScanRun mirrors the validation surface
// [metric_ingestor.ScanRunContext.Validate] enforces for the
// churn-sweep BUT against the `external_single` kind --
// the shared validator's [AllowedScanRunKinds] set REJECTS
// `external_single` (sweep semantics require per-row SHAs),
// so test_balance owns its own validator.
func (w *Writer) validateScanRun(c metric_ingestor.ScanRunContext) error {
	if c.ID == uuid.Nil {
		return fmt.Errorf("test_balance: ScanRunContext.ID is the zero UUID")
	}
	if c.RepoID == uuid.Nil {
		return metric_ingestor.ErrZeroRepoID
	}
	if c.Kind != ScanRunKindExternalSingle {
		return fmt.Errorf("%w: got %q (test_balance requires %q)",
			metric_ingestor.ErrInvalidScanRunKind, c.Kind, ScanRunKindExternalSingle)
	}
	if strings.TrimSpace(c.SHA) == "" {
		return fmt.Errorf("test_balance: ScanRunContext.SHA is empty (external_single requires one SHA per call)")
	}
	return nil
}
