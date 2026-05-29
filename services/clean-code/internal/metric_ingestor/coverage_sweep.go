package metric_ingestor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/coverage"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// ErrCoveragePackageRollupResolverMismatch is returned by
// [CoverageSweep.Run] when the optional
// [CoverageSweep.packageScopeResolver] returns a different
// number of scope_ids than the package-rollup batch
// requested. This is a [FoundationScopeResolver] contract
// violation -- the interface doc pins the return slice
// length to the input batch length -- and surfaces as a
// distinct sentinel so an operator can tell "resolver
// dropped rows" apart from "package rollup itself failed".
var ErrCoveragePackageRollupResolverMismatch = errors.New("metric_ingestor: CoverageSweep package-rollup: scope resolver returned a slice of the wrong length (FoundationScopeResolver contract violation)")

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

// ErrEmptyScanRunSHA is returned by [CoverageSweep.Run]
// when [ScanRunContext.SHA] is the empty string. The
// `external_single` scan_run is opened with the body's SHA
// stamped on `scan_run.to_sha` BEFORE this sweep runs; an
// empty value here means the webhook's ExtractMetadata
// step never persisted a SHA on the parent scan_run.
//
// This is a DEDICATED sentinel (rather than letting the
// emptiness fall through into the
// [ErrCoverageSHAMismatch] branch on line 209) so an
// operator can distinguish "publisher stamped two
// disagreeing SHAs" -- a wire-protocol violation that
// points at the upstream publisher -- from "the scan_run
// was opened without a SHA at all" -- a webhook /
// scan-run-store bug that points at our own ingress path.
// The mismatch error's format string would otherwise read
// `scan_run.sha= payload.sha=<40 hex>`, which buries the
// root cause behind the misleading "the two channels
// disagree" framing.
//
// The payload-side SHA is enforced non-empty AND 40-char
// hex by `coverage.ParseXML` (`shaRegex.MatchString`)
// before this sweep ever sees it, so a dedicated
// `ErrEmptyPayloadSHA` is unnecessary -- the parser
// rejects that case at the ingress boundary.
var ErrEmptyScanRunSHA = errors.New("metric_ingestor: ScanRunContext.SHA is empty (external_single scan_run was opened without a to_sha)")

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
	// packageScopeResolver is OPTIONAL. When non-nil,
	// [CoverageSweep.Run] computes a per-package weighted
	// rollup of the same payload and APPENDS one
	// [MetricSampleRecord] per (package, metric_kind)
	// cohort to the file-level slice BEFORE the single
	// atomic [MetricSampleWriter.WriteBatch] call. When
	// nil, the sweep behaves exactly as it did before iter
	// 8 (file-scope rows only) -- existing tests that
	// construct CoverageSweep without the option continue
	// to work.
	//
	// Iter 8: this seam closes the file -> package rollup
	// production gap the cross-repo happy-path e2e
	// (`test/e2e/cross_repo_happy_path/`) previously
	// bridged with a test-side SQL shim. The composer is
	// the production replacement: it derives package values
	// from the payload's CARDINALITY-WEIGHTED line/branch
	// counts (NOT an average of per-file ratios), uses the
	// canonical [BuildCanonicalSignatureForRefURL] /
	// [storage.ScopeBindingWriter] path to mint package
	// `scope_binding` rows, and writes the resulting
	// metric_sample rows via the same writer the file rows
	// use -- so the rollup inherits the writer's
	// transaction, ProducerRunID guard, post-finalize
	// fence and active-row UPSERT semantics for free.
	//
	// See [coverage_package_rollup.go] for the pure
	// grouping/weighting helper and
	// [FoundationScopeResolver] for the resolver contract.
	packageScopeResolver FoundationScopeResolver
}

// CoverageSweepOption configures a [CoverageSweep] built
// via [NewCoverageSweep]. Iter 8 adds the first option
// ([WithCoveragePackageRollupResolver]); pre-iter-8 call
// sites that omit the variadic continue to compile and
// behave exactly as before.
type CoverageSweepOption func(*CoverageSweep)

// WithCoveragePackageRollupResolver wires a
// [FoundationScopeResolver] into the [CoverageSweep] so
// that every [CoverageSweep.Run] call ALSO emits one
// [MetricSampleRecord] per (package, metric_kind) cohort
// alongside the per-file rows. The resolver is called
// once with a `[]recipes.ScopeRef{Kind: scope.KindPackage,
// Path: <pkgDir>}` batch; on success the returned
// `scope_id`s parallel the input and the sweep appends the
// rollup records to the same [MetricSampleWriter.WriteBatch]
// call as the file rows.
//
// Passing a nil resolver is a NO-OP -- the option leaves
// the sweep in its file-only mode. Call sites that want to
// disable the rollup explicitly should simply omit the
// option.
//
// Iter 8 wires this from the composition root
// (`internal/composition/ingest_router.go` and
// `cmd/clean-code-metric-ingestor/main.go`) using the
// existing [PGScopeBindingResolver] -- the same canonical
// `scope_binding` writer the test_balance and foundation
// verbs already use -- so the rollup writes go through ONE
// shared seam, not a coverage-specific composer.
func WithCoveragePackageRollupResolver(r FoundationScopeResolver) CoverageSweepOption {
	return func(s *CoverageSweep) {
		if r != nil {
			s.packageScopeResolver = r
		}
	}
}

// NewCoverageSweep returns a [CoverageSweep] wired with the
// provided dependencies. PANICS on any nil argument -- the
// composition root is the only legitimate caller, and a nil
// here is always a wiring bug.
//
// Iter 8 adds the variadic `opts` slot for
// [CoverageSweepOption] values (e.g.
// [WithCoveragePackageRollupResolver]). The variadic is
// SOURCE-BACKWARD-COMPATIBLE -- pre-iter-8 callers
// (`NewCoverageSweep(h, w)`) continue to compile and
// behave exactly as before. Existing unit tests that
// construct the sweep without options exercise the
// file-only path; composition-root callers wire the rollup
// resolver explicitly.
func NewCoverageSweep(h *coverage.Hydrator, w MetricSampleWriter, opts ...CoverageSweepOption) *CoverageSweep {
	return newCoverageSweepWithUUID(h, w, uuid.NewV7, opts...)
}

func newCoverageSweepWithUUID(
	h *coverage.Hydrator,
	w MetricSampleWriter,
	newUUID func() (uuid.UUID, error),
	opts ...CoverageSweepOption,
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
	s := &CoverageSweep{
		hydrator: h,
		writer:   w,
		newUUID:  newUUID,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// validateCoverageScanRun is the closed-set guard for the
// CoverageSweep's accepted scan_run kinds. Decoupled from
// [ScanRunContext.Validate] (which accepts the ChurnSweep's
// allow-list) so the structural-coupling of churn and
// coverage at the kind level is explicit.
//
// The SHA non-empty guard is INTENTIONALLY at this layer
// (not on the shared [ScanRunContext.Validate]): only the
// single-SHA-binding pipelines (`external_single`) require
// a non-empty `to_sha` on the scan_run -- the
// foundation-recipe `full` / `delta` scans do NOT, so
// pushing the check up would over-constrain ChurnSweep.
// Running BEFORE the [ErrCoverageSHAMismatch] check in
// Run keeps the "missing SHA" and "disagreeing SHA"
// failure modes distinguishable.
func validateCoverageScanRun(c ScanRunContext) error {
	if c.ID == uuid.Nil {
		return ErrZeroScanRunID
	}
	if c.RepoID == uuid.Nil {
		return ErrZeroRepoID
	}
	if c.SHA == "" {
		return ErrEmptyScanRunSHA
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
//     [ScanRunContext.ID] / [ScanRunContext.RepoID],
//     non-empty [ScanRunContext.SHA], AND
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
	//
	// NOTE: scanRun.SHA is guaranteed non-empty here --
	// validateCoverageScanRun above returns
	// [ErrEmptyScanRunSHA] for the empty case so the
	// "missing SHA" vs "disagreeing SHAs" failure modes
	// surface as DISTINCT sentinels to the operator.
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

	// Iter 8 -- file -> package weighted rollup. Runs ONLY
	// when the composition root wired a
	// [FoundationScopeResolver] via
	// [WithCoveragePackageRollupResolver]; existing call
	// sites (unit tests, scaffolds) that omit the option
	// behave exactly as before.
	//
	// The rollup is intentionally APPENDED to the file
	// record slice (one shared [MetricSampleWriter.WriteBatch]
	// call) so:
	//
	//   - both file and package rows share the same
	//     [MetricSampleRecord.ProducerRunID] (the
	//     [PGMetricSampleWriter] batch guard enforces this);
	//   - the writer's post-finalize fence runs ONCE for
	//     the combined batch;
	//   - a rollup failure aborts before the file rows
	//     persist, preserving the all-or-nothing contract
	//     the file-only path already documents on lines
	//     223-225.
	if s.packageScopeResolver != nil && len(payload.Files) > 0 {
		packageRecords, err := s.buildPackageRollupRecords(ctx, payload, scanRun)
		if err != nil {
			return CoverageSweepResult{}, err
		}
		records = append(records, packageRecords...)
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

// buildPackageRollupRecords groups the [coverage.Payload]
// files by package directory, computes the cardinality-
// weighted line/branch ratio per package via
// [rollUpCoveragePackages], resolves each package's durable
// `scope_binding.scope_id` via the wired
// [FoundationScopeResolver], and shapes the result into a
// [MetricSampleRecord] slice ready to APPEND to the file
// records before [MetricSampleWriter.WriteBatch].
//
// # Scope resolution
//
// The resolver call is the SAME canonical seam the
// foundation dispatcher and the test_balance writer use --
// [PGScopeBindingResolver] in production -- so the package
// `scope_binding` rows land via the same advisory-lock
// natural-key path that the file rows already rely on.
// `BuildCanonicalSignatureForRefURL` switches on
// `ref.Kind == scope.KindPackage` and emits
// `<repoURL>::pkg::<pkgDir>` via [scope.BuildPackage].
//
// # ProducerRunID
//
// Every emitted record's ProducerRunID equals
// `scanRun.ID`, matching the file records' ProducerRunID
// so the [PGMetricSampleWriter] mixed-batch guard accepts
// the combined slice.
//
// # Empty rollup
//
// Returns `(nil, nil)` when [rollUpCoveragePackages]
// produces zero cohorts (e.g. every file had
// `LinesValid == 0` AND `BranchesValid == 0`). The caller
// treats this as a clean no-op append.
func (s *CoverageSweep) buildPackageRollupRecords(ctx context.Context, payload *coverage.Payload, scanRun ScanRunContext) ([]MetricSampleRecord, error) {
	rollups := rollUpCoveragePackages(payload)
	if len(rollups) == 0 {
		return nil, nil
	}

	// Build the dedup'd package-ref batch. Two rollup
	// rows for the same package (line + branch) MUST share
	// the same scope_id, so we resolve each package
	// directory ONCE and zip back by package path.
	pkgIndex := map[string]int{}
	pkgRefs := make([]recipes.ScopeRef, 0, len(rollups))
	for i := range rollups {
		pkg := rollups[i].PackagePath
		if _, seen := pkgIndex[pkg]; seen {
			continue
		}
		pkgIndex[pkg] = len(pkgRefs)
		pkgRefs = append(pkgRefs, recipes.ScopeRef{
			Kind: scope.KindPackage,
			// Path is the per-package directory the
			// [BuildCanonicalSignatureForRefURL] dispatch
			// (canonical_signature.go:165-170) requires
			// for [scope.KindPackage]; the helper threads
			// it into [scope.BuildPackage] which renders
			// `<repoURL>::pkg::<pkgDir>`.
			Path: pkg,
			// QualifiedName is unused for package refs
			// (only file/class/interface/method need
			// it); the resolver's per-ref validation
			// skips the non-empty check when
			// `ref.Kind == scope.KindPackage`.
			QualifiedName: pkg,
		})
	}

	scopeIDs, err := s.packageScopeResolver.ResolveScopeIDs(ctx, payload.RepoID, pkgRefs, payload.SHA)
	if err != nil {
		return nil, fmt.Errorf("metric_ingestor: CoverageSweep package-rollup ResolveScopeIDs (repo=%s sha=%s): %w", payload.RepoID, payload.SHA, err)
	}
	if len(scopeIDs) != len(pkgRefs) {
		return nil, fmt.Errorf("%w: requested %d refs, got %d ids", ErrCoveragePackageRollupResolverMismatch, len(pkgRefs), len(scopeIDs))
	}

	out := make([]MetricSampleRecord, 0, len(rollups))
	for i := range rollups {
		row := &rollups[i]
		idx, ok := pkgIndex[row.PackagePath]
		if !ok {
			// Defensive: the package was just inserted
			// into `pkgIndex` above. A miss here is a
			// programmer bug, not a runtime condition.
			return nil, fmt.Errorf("metric_ingestor: CoverageSweep package-rollup: pkg %q missing from index after dedup (programmer bug)", row.PackagePath)
		}
		scopeID := scopeIDs[idx]
		if scopeID == uuid.Nil {
			return nil, fmt.Errorf("metric_ingestor: CoverageSweep package-rollup: resolver returned the zero UUID for pkg %q (repo=%s sha=%s)", row.PackagePath, payload.RepoID, payload.SHA)
		}
		sampleID, err := s.newUUID()
		if err != nil {
			return nil, fmt.Errorf("metric_ingestor: CoverageSweep package-rollup: mint sample_id for pkg=%q kind=%s: %w", row.PackagePath, row.MetricKind, err)
		}
		out = append(out, MetricSampleRecord{
			SampleID:      sampleID,
			RepoID:        payload.RepoID,
			SHA:           payload.SHA,
			ScopeID:       scopeID,
			MetricKind:    row.MetricKind,
			MetricVersion: coverage.MetricVersion,
			Pack:          recipes.PackIngested,
			// SourceDerived flags the row as a derived
			// rollup rather than a raw ingest -- the
			// aggregator's read filters do not
			// distinguish today, but the column makes
			// the provenance visible to operators
			// reading metric_sample directly.
			Source:        recipes.SourceDerived,
			Value:         row.Value,
			Attrs:         nil,
			ProducerRunID: scanRun.ID,
		})
	}
	return out, nil
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
