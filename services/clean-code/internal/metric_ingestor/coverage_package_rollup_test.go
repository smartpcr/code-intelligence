package metric_ingestor

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/coverage"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestRollUpCoveragePackages_WeightedNotAverage pins the
// load-bearing semantic the iter-8 production fix replaced:
// package coverage is the cardinality-weighted ratio
// `sum(covered) / sum(valid)`, NOT the unweighted average
// of per-file ratios.
//
// Two files in the same package:
//
//   - a.py: 1 line covered out of 1 line valid -> 1.00
//   - b.py: 0 lines covered out of 99 lines valid -> 0.00
//
// Unweighted AVG would emit 0.50 (the iter-2..iter-7 test
// shim used AVG semantics and was scored as a shim). The
// correct weighted ratio is `(1+0) / (1+99) = 0.01` -- a
// 1-line file MUST NOT weigh the same as a 99-line file.
// Drifting back to AVG would silently corrupt cross-repo
// percentiles by re-introducing a 50x error on this shape.
func TestRollUpCoveragePackages_WeightedNotAverage(t *testing.T) {
	payload := &coverage.Payload{
		RepoID: uuid.Must(uuid.NewV7()),
		SHA:    "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Files: []coverage.FileCoverage{
			{FilePath: "pkg/a.py", LinesCovered: 1, LinesValid: 1},
			{FilePath: "pkg/b.py", LinesCovered: 0, LinesValid: 99},
		},
	}
	got := rollUpCoveragePackages(payload)
	if len(got) != 1 {
		t.Fatalf("want 1 rollup row (line ratio); got %d: %+v", len(got), got)
	}
	row := got[0]
	if row.PackagePath != "pkg" {
		t.Errorf("PackagePath = %q, want %q", row.PackagePath, "pkg")
	}
	if row.MetricKind != coverage.MetricKindCoverageLineRatio {
		t.Errorf("MetricKind = %q, want %q", row.MetricKind, coverage.MetricKindCoverageLineRatio)
	}
	want := 0.01
	if math.Abs(row.Value-want) > 1e-12 {
		t.Errorf("Value = %.6f, want weighted %.6f (AVG-of-ratios would be 0.50 -- the iter-7 shim's wrong semantic)", row.Value, want)
	}
}

// TestRollUpCoveragePackages_MultiPackageGroupingDeterministic
// pins both the per-package grouping (files grouped by
// `path.Dir`) and the deterministic emission order (sorted
// by PackagePath then MetricKind ascending). A drift in
// either property would corrupt the writer batch shape on
// re-runs.
func TestRollUpCoveragePackages_MultiPackageGroupingDeterministic(t *testing.T) {
	payload := &coverage.Payload{
		RepoID: uuid.Must(uuid.NewV7()),
		SHA:    "1234567890abcdef1234567890abcdef12345678",
		Files: []coverage.FileCoverage{
			// pkgZ goes first in the input slice but should
			// emit LAST after sort.
			{FilePath: "pkgZ/x.py", LinesCovered: 4, LinesValid: 10, BranchesCovered: 1, BranchesValid: 2},
			{FilePath: "pkgA/x.py", LinesCovered: 1, LinesValid: 10},
			{FilePath: "pkgA/y.py", LinesCovered: 9, LinesValid: 10},
		},
	}
	got := rollUpCoveragePackages(payload)
	// pkgA: line only (no branches) -> 1 row
	// pkgZ: line + branch -> 2 rows
	if len(got) != 3 {
		t.Fatalf("want 3 rollup rows; got %d: %+v", len(got), got)
	}
	// Determinism: pkgA before pkgZ; within pkgZ, line
	// before branch (the helper emits line first by
	// construction).
	if got[0].PackagePath != "pkgA" || got[0].MetricKind != coverage.MetricKindCoverageLineRatio {
		t.Errorf("rows[0] = %+v, want pkgA/line", got[0])
	}
	if got[1].PackagePath != "pkgZ" || got[1].MetricKind != coverage.MetricKindCoverageLineRatio {
		t.Errorf("rows[1] = %+v, want pkgZ/line", got[1])
	}
	if got[2].PackagePath != "pkgZ" || got[2].MetricKind != coverage.MetricKindCoverageBranchRatio {
		t.Errorf("rows[2] = %+v, want pkgZ/branch", got[2])
	}
	// Weighted values: pkgA line = (1+9)/(10+10) = 0.50
	if math.Abs(got[0].Value-0.50) > 1e-12 {
		t.Errorf("pkgA line value = %.6f, want 0.50", got[0].Value)
	}
}

// TestRollUpCoveragePackages_SkipsZeroDenominator pins the
// `LinesValid == 0` / `BranchesValid == 0` suppression rule.
// A package whose every file has zero-denominator emissions
// must produce NO rollup row -- producing `NaN` or
// silently-substituted `0.0` would corrupt the aggregator's
// cohort.
func TestRollUpCoveragePackages_SkipsZeroDenominator(t *testing.T) {
	payload := &coverage.Payload{
		RepoID: uuid.Must(uuid.NewV7()),
		SHA:    "00000000000000000000000000000000deadbeef",
		Files: []coverage.FileCoverage{
			{FilePath: "empty/a.py", LinesCovered: 0, LinesValid: 0, BranchesCovered: 0, BranchesValid: 0},
		},
	}
	got := rollUpCoveragePackages(payload)
	if len(got) != 0 {
		t.Errorf("want 0 rollup rows for zero-denominator package; got %d: %+v", len(got), got)
	}
}

// TestRollUpCoveragePackages_EmptyAndNilPayload pins the
// nil/empty payload contract -- the helper returns nil so
// the [CoverageSweep.Run] caller's `if rollups == nil`
// shortcut is correct.
func TestRollUpCoveragePackages_EmptyAndNilPayload(t *testing.T) {
	if got := rollUpCoveragePackages(nil); got != nil {
		t.Errorf("nil payload: got %+v, want nil", got)
	}
	if got := rollUpCoveragePackages(&coverage.Payload{}); got != nil {
		t.Errorf("empty Files: got %+v, want nil", got)
	}
}

// stubFoundationScopeResolver is a deterministic test double
// for the [FoundationScopeResolver] seam the iter-8
// CoverageSweep package-rollup uses. Records the most-recent
// call for assertion and returns mint-ordered UUIDs.
type stubFoundationScopeResolver struct {
	gotRepoID uuid.UUID
	gotSHA    string
	gotRefs   []recipes.ScopeRef
	returnErr error
}

func (r *stubFoundationScopeResolver) ResolveScopeIDs(_ context.Context, repoID uuid.UUID, refs []recipes.ScopeRef, sha string) ([]uuid.UUID, error) {
	r.gotRepoID = repoID
	r.gotSHA = sha
	r.gotRefs = append(r.gotRefs, refs...)
	if r.returnErr != nil {
		return nil, r.returnErr
	}
	out := make([]uuid.UUID, len(refs))
	for i := range refs {
		out[i] = uuid.Must(uuid.NewV7())
	}
	return out, nil
}

// batchCountingWriter wraps [InMemoryMetricSampleWriter] to
// also count how many WriteBatch calls were made -- the
// shared-batch invariant the iter-8 rollup integration
// relies on cannot be observed via `Records()` alone
// (which flattens across calls).
type batchCountingWriter struct {
	inner    *InMemoryMetricSampleWriter
	batches  [][]MetricSampleRecord
}

func newBatchCountingWriter() *batchCountingWriter {
	return &batchCountingWriter{inner: NewInMemoryMetricSampleWriter()}
}

func (w *batchCountingWriter) WriteBatch(ctx context.Context, records []MetricSampleRecord) error {
	if err := w.inner.WriteBatch(ctx, records); err != nil {
		return err
	}
	cp := make([]MetricSampleRecord, len(records))
	copy(cp, records)
	w.batches = append(w.batches, cp)
	return nil
}

func (w *batchCountingWriter) Batches() [][]MetricSampleRecord { return w.batches }

// TestCoverageSweep_PackageRollupAppendsToWriteBatch pins
// the load-bearing wiring: when the iter-8
// [WithCoveragePackageRollupResolver] option is set, every
// [CoverageSweep.Run] call APPENDS one package-scope record
// per (package, metric_kind) cohort to the SAME
// [MetricSampleWriter.WriteBatch] call as the file rows --
// preserving the all-or-nothing transaction contract.
func TestCoverageSweep_PackageRollupAppendsToWriteBatch(t *testing.T) {
	repoID := uuid.Must(uuid.NewV7())
	scanRunID := uuid.Must(uuid.NewV7())
	sha := "abcdef0123456789abcdef0123456789abcdef01"

	// File-scope mapping for the hydrator. Two files in
	// one package; the rollup should produce ONE package
	// row.
	res := coverage.NewMapScopeResolver()
	res.Add(repoID, "pkg/a.py", uuid.Must(uuid.NewV7()), recipes.ScopeRef{
		Kind:          scope.KindFile,
		Path:          "pkg/a.py",
		QualifiedName: "pkg/a.py",
	})
	res.Add(repoID, "pkg/b.py", uuid.Must(uuid.NewV7()), recipes.ScopeRef{
		Kind:          scope.KindFile,
		Path:          "pkg/b.py",
		QualifiedName: "pkg/b.py",
	})
	hyd := coverage.NewHydrator(res)
	stubResolver := &stubFoundationScopeResolver{}
	writer := newBatchCountingWriter()
	sweep := NewCoverageSweep(hyd, writer, WithCoveragePackageRollupResolver(stubResolver))

	payload := &coverage.Payload{
		RepoID: repoID,
		SHA:    sha,
		Files: []coverage.FileCoverage{
			{FilePath: "pkg/a.py", LinesCovered: 2, LinesValid: 10},
			{FilePath: "pkg/b.py", LinesCovered: 6, LinesValid: 10},
		},
	}
	scanCtx := ScanRunContext{
		ID:     scanRunID,
		RepoID: repoID,
		SHA:    sha,
		Kind:   ScanRunKindExternalSingle,
	}

	got, err := sweep.Run(context.Background(), scanCtx, payload)
	if err != nil {
		t.Fatalf("CoverageSweep.Run: %v", err)
	}

	// 2 file rows + 1 package row.
	if got.SamplesWritten != 3 {
		t.Errorf("SamplesWritten = %d, want 3 (2 file + 1 package)", got.SamplesWritten)
	}
	if got.RowsHydrated != 2 {
		t.Errorf("RowsHydrated = %d, want 2", got.RowsHydrated)
	}

	// The resolver received exactly one batch with one
	// KindPackage ref for "pkg".
	if len(stubResolver.gotRefs) != 1 {
		t.Fatalf("stub resolver received %d refs; want 1: %+v", len(stubResolver.gotRefs), stubResolver.gotRefs)
	}
	if stubResolver.gotRefs[0].Kind != scope.KindPackage {
		t.Errorf("resolver ref.Kind = %q, want %q", stubResolver.gotRefs[0].Kind, scope.KindPackage)
	}
	if stubResolver.gotRefs[0].Path != "pkg" {
		t.Errorf("resolver ref.Path = %q, want %q", stubResolver.gotRefs[0].Path, "pkg")
	}
	if stubResolver.gotRepoID != repoID || stubResolver.gotSHA != sha {
		t.Errorf("resolver got (repoID=%s, sha=%q); want (%s, %q)", stubResolver.gotRepoID, stubResolver.gotSHA, repoID, sha)
	}

	// The writer received one combined batch.
	if len(writer.Batches()) != 1 {
		t.Fatalf("writer received %d batches; want 1 (file + package in ONE WriteBatch): %+v", len(writer.Batches()), writer.Batches())
	}
	batch := writer.Batches()[0]
	if len(batch) != 3 {
		t.Errorf("batch length = %d, want 3", len(batch))
	}
	// Every record shares the ProducerRunID (writer
	// guard requirement).
	for i, rec := range batch {
		if rec.ProducerRunID != scanRunID {
			t.Errorf("batch[%d].ProducerRunID = %s, want %s", i, rec.ProducerRunID, scanRunID)
		}
	}
}

// TestCoverageSweep_NoResolverNoRollup pins the
// backward-compatible behaviour: when the iter-8 option is
// NOT wired (the default), CoverageSweep.Run behaves
// exactly as before -- file rows only, no package rollup
// pass, no resolver dependency required.
func TestCoverageSweep_NoResolverNoRollup(t *testing.T) {
	repoID := uuid.Must(uuid.NewV7())
	scanRunID := uuid.Must(uuid.NewV7())
	sha := "1111111111111111111111111111111111111111"

	res := coverage.NewMapScopeResolver()
	res.Add(repoID, "pkg/a.py", uuid.Must(uuid.NewV7()), recipes.ScopeRef{
		Kind:          scope.KindFile,
		Path:          "pkg/a.py",
		QualifiedName: "pkg/a.py",
	})
	hyd := coverage.NewHydrator(res)
	writer := newBatchCountingWriter()
	// Note: NO WithCoveragePackageRollupResolver option.
	sweep := NewCoverageSweep(hyd, writer)

	payload := &coverage.Payload{
		RepoID: repoID,
		SHA:    sha,
		Files: []coverage.FileCoverage{
			{FilePath: "pkg/a.py", LinesCovered: 5, LinesValid: 10},
		},
	}
	scanCtx := ScanRunContext{
		ID:     scanRunID,
		RepoID: repoID,
		SHA:    sha,
		Kind:   ScanRunKindExternalSingle,
	}
	got, err := sweep.Run(context.Background(), scanCtx, payload)
	if err != nil {
		t.Fatalf("CoverageSweep.Run: %v", err)
	}
	// Only the file row; no package rollup.
	if got.SamplesWritten != 1 {
		t.Errorf("SamplesWritten = %d, want 1 (file only, no rollup wired)", got.SamplesWritten)
	}
}

// TestCoverageSweep_RollupResolverFailureAborts pins the
// all-or-nothing contract: a package-rollup resolver
// failure MUST abort BEFORE the writer is called so the
// file rows do not land without their package companions.
func TestCoverageSweep_RollupResolverFailureAborts(t *testing.T) {
	repoID := uuid.Must(uuid.NewV7())
	scanRunID := uuid.Must(uuid.NewV7())
	sha := "2222222222222222222222222222222222222222"

	res := coverage.NewMapScopeResolver()
	res.Add(repoID, "pkg/a.py", uuid.Must(uuid.NewV7()), recipes.ScopeRef{
		Kind:          scope.KindFile,
		Path:          "pkg/a.py",
		QualifiedName: "pkg/a.py",
	})
	hyd := coverage.NewHydrator(res)
	stubResolver := &stubFoundationScopeResolver{returnErr: errors.New("simulated resolver outage")}
	writer := newBatchCountingWriter()
	sweep := NewCoverageSweep(hyd, writer, WithCoveragePackageRollupResolver(stubResolver))

	payload := &coverage.Payload{
		RepoID: repoID,
		SHA:    sha,
		Files: []coverage.FileCoverage{
			{FilePath: "pkg/a.py", LinesCovered: 5, LinesValid: 10},
		},
	}
	scanCtx := ScanRunContext{
		ID:     scanRunID,
		RepoID: repoID,
		SHA:    sha,
		Kind:   ScanRunKindExternalSingle,
	}
	_, err := sweep.Run(context.Background(), scanCtx, payload)
	if err == nil {
		t.Fatalf("CoverageSweep.Run: want error from resolver outage, got nil")
	}
	// Writer MUST NOT have been called.
	if len(writer.Batches()) != 0 {
		t.Errorf("writer received %d batches; want 0 (rollup failure must abort before file writes): %+v", len(writer.Batches()), writer.Batches())
	}
}
