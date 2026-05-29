package metric_ingestor

import (
	"path"
	"sort"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/coverage"
)

// coveragePackageRollup is the writer-ready summary of a
// single (package, metric_kind) cohort that the production
// file-to-package rollup composer emits ALONGSIDE the per-
// file [coverage.HydratedCoverageRow] slice.
//
// # Why this lives in the metric_ingestor package
//
// The rollup wraps THREE existing seams (the
// [coverage.Payload] for the raw counts, the
// [FoundationScopeResolver] for the package
// `scope_binding.scope_id`, and the
// [MetricSampleWriter] for the persisted row); only the
// metric_ingestor package sees all three at once, and the
// natural composition point is inside
// [CoverageSweep.Run]. Keeping the helper in this package
// (rather than alongside [coverage.Hydrator]) avoids an
// import-direction inversion -- the coverage package
// already exposes its parser/hydrator types here, the
// reverse direction (coverage -> metric_ingestor) is
// blocked by [coverage_sweep.go]'s existing imports.
//
// # Weighted line-rate semantics
//
// Package-level line rate is NOT the average of per-file
// rates. The mathematically correct value is the
// CARDINALITY-WEIGHTED ratio
// `sum(file.LinesCovered) / sum(file.LinesValid)` (and
// analogously for branches): a 1-line file MUST NOT weigh
// the same as a 1000-line file. We have the raw
// `LinesCovered` and `LinesValid` columns on every
// [coverage.FileCoverage] (architecture Sec 6.4) so we
// compute the weighted ratio from the payload directly
// rather than re-deriving it from the persisted ratios
// (which would have lost the denominator).
//
// # Determinism
//
// The returned slice is sorted by `(PackagePath,
// MetricKind)` ascending so a re-run on the same payload
// emits a byte-identical batch. The
// [PGMetricSampleWriter]'s ON CONFLICT path already
// idempotent-handles re-emission, but a deterministic
// ordering keeps the operator's WAL diff readable.
type coveragePackageRollup struct {
	// PackagePath is the repo-relative package directory,
	// computed as `path.Dir(file.FilePath)`. Files at the
	// repo root return `"."` from `path.Dir`; we keep that
	// literal so the package-scope `canonical_signature`
	// for a root-level file is distinguishable from a
	// missing-path bug.
	PackagePath string
	// MetricKind is one of
	// [coverage.MetricKindCoverageLineRatio] or
	// [coverage.MetricKindCoverageBranchRatio].
	MetricKind string
	// Value is the weighted ratio in `[0, 1]`:
	// `sum(covered) / sum(valid)` summed over every
	// [coverage.FileCoverage] under the package.
	Value float64
}

// rollUpCoveragePackages groups the [coverage.Payload.Files]
// slice by `path.Dir(filePath)` and emits one
// [coveragePackageRollup] per (package, metric_kind) cohort
// with the cardinality-weighted ratio.
//
// Files with `LinesValid == 0` are EXCLUDED from the line
// rollup's numerator AND denominator -- the per-file
// hydrator already suppresses the line row for such files
// (cobertura.go:747-760), so the rollup follows the same
// suppression rule. Same for `BranchesValid == 0`.
//
// A package whose every file has `LinesValid == 0` emits
// NO line rollup row. Same for branches. This is
// intentional: a zero-denominator "weighted ratio" is
// undefined; producing `0/0 = NaN` (or worse, a
// silently-substituted `0.0`) would smuggle a bogus value
// into the aggregator's cohort and the cross-repo
// percentile would be wrong by more than the test's
// p90/p99 noise.
//
// The returned slice is sorted by `(PackagePath,
// MetricKind)` ascending for determinism (G2). An empty
// or nil payload returns `nil`.
func rollUpCoveragePackages(payload *coverage.Payload) []coveragePackageRollup {
	if payload == nil || len(payload.Files) == 0 {
		return nil
	}

	type pkgAgg struct {
		linesCovered    int
		linesValid      int
		branchesCovered int
		branchesValid   int
	}
	byPkg := map[string]*pkgAgg{}
	for i := range payload.Files {
		f := &payload.Files[i]
		pkg := path.Dir(f.FilePath)
		agg, ok := byPkg[pkg]
		if !ok {
			agg = &pkgAgg{}
			byPkg[pkg] = agg
		}
		agg.linesCovered += f.LinesCovered
		agg.linesValid += f.LinesValid
		agg.branchesCovered += f.BranchesCovered
		agg.branchesValid += f.BranchesValid
	}

	out := make([]coveragePackageRollup, 0, 2*len(byPkg))
	pkgs := make([]string, 0, len(byPkg))
	for pkg := range byPkg {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		agg := byPkg[pkg]
		if agg.linesValid > 0 {
			out = append(out, coveragePackageRollup{
				PackagePath: pkg,
				MetricKind:  coverage.MetricKindCoverageLineRatio,
				Value:       float64(agg.linesCovered) / float64(agg.linesValid),
			})
		}
		if agg.branchesValid > 0 {
			out = append(out, coveragePackageRollup{
				PackagePath: pkg,
				MetricKind:  coverage.MetricKindCoverageBranchRatio,
				Value:       float64(agg.branchesCovered) / float64(agg.branchesValid),
			})
		}
	}
	return out
}
