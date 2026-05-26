package metric_ingestor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/coverage"
)

// ErrCoverageSHAMismatch is returned by [CoverageSweep.Run]
// when the parent [ScanRunContext.SHA] disagrees with the
// uploaded [coverage.Payload.SHA]. The
// `external_single`/`single`-binding contract pins ONE SHA
// per upload (architecture Sec 6.4 lines 1364-1366; tech-
// spec Sec 4.11 lines 429-431); a publisher that stamps a
// different SHA on the body than on the scan_run is a wire-
// protocol violation. The two values arrive on independent
// channels (ScanRun SHA via the webhook's ExtractMetadata
// envelope, payload SHA via the parsed XML) so this guard
// is the load-bearing single-SHA invariant check at the
// writer-side seam.
var ErrCoverageSHAMismatch = errors.New("metric_ingestor: ScanRunContext.SHA does not match coverage.Payload.SHA (single-SHA binding violated)")

// `scan_run.kind` literals the [CoverageSweep] accepts. A
// coverage upload carries ONE `sha` per call (tech-spec Sec
// 4.11 line 429-431, architecture Sec 6.4 lines 1364-1366),
// so the only canonical kind here is `external_single` --
// `external_per_row` is incompatible with single-SHA
// coverage payloads, and `full` / `delta` are
// foundation-recipe scans that do NOT take a coverage XML
// body. The closed set is DELIBERATELY narrower than
// [allowedScanRunKinds] (which is the ChurnSweep's accepted
// set) so a mis-wired router cannot accidentally drive
// coverage through a `full` scan.
var coverageAllowedScanRunKinds = []string{ScanRunKindExternalSingle}

// CoverageAllowedScanRunKinds returns a fresh slice of the
// [ScanRun.kind] literals the [CoverageSweep] accepts.
// Currently just `{external_single}`. Returned as a new
// slice each call so a caller cannot mutate the closed set.
func CoverageAllowedScanRunKinds() []string {
	out := make([]string, len(coverageAllowedScanRunKinds))
	copy(out, coverageAllowedScanRunKinds)
	return out
}

// CoverageSweepResult summarises a single
// [CoverageSweep.Run] call's outcome. Designed for the
// composition root's INFO-level log line and for the
// verb-handler's Detail envelope.
type CoverageSweepResult struct {
	// SamplesWritten is the count of records appended by
	// [MetricSampleWriter.WriteBatch] -- one per
	// hydrator-emitted row (which is one per file-level
	// (line_ratio | branch_ratio) pair).
	SamplesWritten int
	// RowsHydrated is the count of
	// [coverage.HydratedCoverageRow]s the hydrator emitted
	// (BEFORE the writer call). Equals SamplesWritten on
	// success; differs only if a future refactor adds
	// post-hydration filtering.
	RowsHydrated int
	// SkippedUnboundScopeCount is the count of files in the
	// Cobertura payload that the [coverage.ScopeResolver]
	// could not bind to a durable scope_id (per
	// `coverage_skipped_unbound_scope` counter -- iter-1
	// evaluator item 4). The sweep's INFO log line + the
	// router's response detail envelope both surface this so
	// an operator can spot a publisher whose paths drift
	// from the AST adapter's scope layout.
	SkippedUnboundScopeCount int
}

// CoverageSweep is the writer-side orchestrator for one
// `ingest.coverage` Cobertura upload (one webhook delivery
// -> one external_single ScanRun). Construct via
// [NewCoverageSweep]; the sweep is stateless past its
// dependencies so a single instance handles every batch
// the service receives.
//
// # Why a separate type from ChurnSweep
//
// ChurnSweep dispatches a recipe-versioned MATERIALISER
// over hydrated rows; its inputs and outputs span the
// `modification_count_in_window` aggregation pipeline.
// CoverageSweep is a STRAIGHT-THROUGH path: the
// [coverage.Hydrator] already produces per-file ratio
// values (line + branch) ready for direct persistence, so
// the sweep's job is purely (a) validate the parent
// ScanRun, (b) drive the hydrator, (c) shape the rows for
// the writer. Sharing a type with ChurnSweep would force
// either a materialiser-shaped no-op or a `kind`-switch
// inside ChurnSweep.Run; both would couple two unrelated
// pipelines through a leaky abstraction.
type CoverageSweep struct {
	hydrator *coverage.Hydrator
	writer   MetricSampleWriter
	// newUUID mints the [MetricSampleRecord.SampleID] for
	// each emitted row. Defaults to `uuid.NewV7` (the same
	// time-ordered PK strategy ChurnSweep uses). Tests
	// inject a deterministic generator.
	newUUID func() (uuid.UUID, error)
}

// NewCoverageSweep returns a [CoverageSweep] wired with the
// provided dependencies. PANICS on any nil argument -- the
// composition root is the only legitimate caller, and a nil
// here is always a wiring bug.
func NewCoverageSweep(h *coverage.Hydrator, w MetricSampleWriter) *CoverageSweep {
	return newCoverageSweepWithUUID(h, w, uuid.NewV7)
}

func newCoverageSweepWithUUID(
	h *coverage.Hydrator,
	w MetricSampleWriter,
	newUUID func() (uuid.UUID, error),
) *CoverageSweep {
	if h == nil {
		panic("metric_ingestor: NewCoverageSweep received nil *coverage.Hydrator")
	}
	if w == nil {
		panic("metric_ingestor: NewCoverageSweep received nil MetricSampleWriter")
	}
	if newUUID == nil {
		panic("metric_ingestor: newUUID is nil")
	}
	return &CoverageSweep{
		hydrator: h,
		writer:   w,
		newUUID:  newUUID,
	}
}

// validateCoverageScanRun is the closed-set guard for the
// CoverageSweep's accepted scan_run kinds. Decoupled from
// [ScanRunContext.Validate] (which accepts the ChurnSweep's
// allow-list) so the structural-coupling of churn and
// coverage at the kind level is explicit.
func validateCoverageScanRun(c ScanRunContext) error {
	if c.ID == uuid.Nil {
		return ErrZeroScanRunID
	}
	if c.RepoID == uuid.Nil {
		return ErrZeroRepoID
	}
	for _, k := range coverageAllowedScanRunKinds {
		if c.Kind == k {
			return nil
		}
	}
	return fmt.Errorf("%w: got %q (coverage allowed: %v)",
		ErrInvalidScanRunKind, c.Kind, coverageAllowedScanRunKinds)
}

// Run drives one coverage payload through the writer
// pipeline:
//
//  1. Validate [ScanRunContext]: non-zero
//     [ScanRunContext.ID] / [ScanRunContext.RepoID] AND
//     [ScanRunContext.Kind] == `external_single` (the
//     only kind compatible with the single-SHA coverage
//     semantics).
//  2. Assert [coverage.Payload.RepoID] matches the
//     ScanRun's RepoID (the writer-ownership invariant is
//     per-repo).
//  3. Hydrate the payload to
//     [coverage.HydratedCoverageRow]s (resolves every
//     file_path to a durable scope_id; un-bound files are
//     counted into the result's SkippedUnboundScopeCount).
//  4. Shape each hydrated row into a
//     [MetricSampleRecord] (mint a fresh sample_id, stamp
//     ProducerRunID = scanRun.ID, SHA = payload.SHA -- all
//     rows in a coverage batch share the same SHA).
//  5. Call [MetricSampleWriter.WriteBatch] with the full
//     record slice (atomic write).
//
// On any error, Run returns ([CoverageSweepResult]{}, error)
// and the writer has NOT been called. The all-or-nothing
// contract is critical: a partial write would leave the
// active-row index in a half-state.
func (s *CoverageSweep) Run(ctx context.Context, scanRun ScanRunContext, payload *coverage.Payload) (CoverageSweepResult, error) {
	if err := validateCoverageScanRun(scanRun); err != nil {
		return CoverageSweepResult{}, err
	}
	if payload == nil {
		return CoverageSweepResult{}, errors.New("metric_ingestor: coverage payload is nil")
	}
	if scanRun.RepoID != payload.RepoID {
		return CoverageSweepResult{}, fmt.Errorf("%w: scan_run.repo_id=%s payload.repo_id=%s",
			ErrRepoIDMismatch, scanRun.RepoID, payload.RepoID)
	}
	// Single-SHA binding (iter-3 evaluator item 4):
	// `external_single` carries ONE SHA per upload and the
	// parent ScanRun is opened with that SHA before this
	// sweep runs. The webhook splits the SHA channel across
	// scan_run.to_sha (from ExtractMetadata) and
	// payload.SHA (from the parsed XML body) -- a mismatch
	// here means the publisher's two sources disagree and
	// the writer MUST NOT proceed: either the publisher
	// posted the body under the wrong scan_run sha, or the
	// metadata extraction lost the body's actual sha. Both
	// paths violate the single-SHA invariant the row keys
	// are stamped against, so fail-closed before the
	// writer call.
	if scanRun.SHA != payload.SHA {
		return CoverageSweepResult{}, fmt.Errorf("%w: scan_run.sha=%s payload.sha=%s",
			ErrCoverageSHAMismatch, scanRun.SHA, payload.SHA)
	}

	hydrateResult, err := s.hydrator.Hydrate(ctx, payload, scanRun.ID)
	if err != nil {
		return CoverageSweepResult{}, err
	}

	// Build the sample-record slice via the hydrator-owned
	// converter so the cross-package shim (MetricSampleSeed)
	// is the SINGLE site that knows both shapes.
	raws, convErr := hydrateResult.ToMetricSampleRecords(payload.RepoID, s.newUUID, func(seed coverage.MetricSampleSeed) any {
		return MetricSampleRecord{
			SampleID:      seed.SampleID,
			RepoID:        seed.RepoID,
			SHA:           seed.SHA,
			ScopeID:       seed.ScopeID,
			MetricKind:    seed.MetricKind,
			MetricVersion: seed.MetricVersion,
			Pack:          seed.Pack,
			Source:        seed.Source,
			Value:         seed.Value,
			// Coverage rows carry no per-row attrs at v1
			// (the metric_kind already encodes the
			// line-vs-branch distinction; the file_path is
			// recoverable via the scope_id join). A nil
			// map is the canonical "no attrs" sentinel the
			// PGMetricSampleWriter persists as JSON `{}`.
			Attrs:         nil,
			ProducerRunID: seed.ProducerRunID,
		}
	})
	if convErr != nil {
		return CoverageSweepResult{}, convErr
	}

	records := make([]MetricSampleRecord, len(raws))
	for i, raw := range raws {
		records[i] = raw.(MetricSampleRecord)
	}

	if len(records) > 0 {
		if err := s.writer.WriteBatch(ctx, records); err != nil {
			return CoverageSweepResult{}, fmt.Errorf("%w: %v", ErrWriterFailure, err)
		}
	}

	return CoverageSweepResult{
		SamplesWritten:           len(records),
		RowsHydrated:             len(hydrateResult.Rows),
		SkippedUnboundScopeCount: hydrateResult.SkippedUnboundScopeCount,
	}, nil
}

// EnsureCoverageSkipLoggerAttached wires `logger` into the
// hydrator if non-nil. Idempotent. Used by the composition
// root after the [CoverageSweep] is constructed; the
// hydrator's optional logger surfaces ONE structured INFO
// line per skipped file so an operator can spot publisher
// drift via the `coverage_skipped_unbound_scope` event.
//
// Returns the [CoverageSweep] for chaining. Calling with a
// nil logger is a no-op (returns the receiver unchanged).
func (s *CoverageSweep) EnsureCoverageSkipLoggerAttached(logger *slog.Logger) *CoverageSweep {
	if logger != nil && s != nil {
		s.hydrator = s.hydrator.WithSkipLogger(logger)
	}
	return s
}
