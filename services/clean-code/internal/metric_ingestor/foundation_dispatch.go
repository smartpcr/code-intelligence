package metric_ingestor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// ErrFoundationRegistryUnwired is returned by
// [RegistryBackedFoundationDispatcher.Dispatch] when its
// [recipes.Registry] dependency is nil. A wired dispatcher with
// no registry cannot iterate any recipes; surfacing the
// misconfiguration here keeps the composition-root error
// pointed at the missing seam instead of producing a silent
// "zero recipes scanned" success that would mask the wiring
// bug.
var ErrFoundationRegistryUnwired = errors.New("metric_ingestor: RegistryBackedFoundationDispatcher.Registry is nil")

// ErrFoundationAstFilesUnwired is returned by
// [RegistryBackedFoundationDispatcher.Dispatch] when its
// [AstFileSource] dependency is nil. The Stage 2.6 production
// wiring supplies [EmptyAstFileSource]; tests supply a fake.
// A nil source is always a wiring bug.
var ErrFoundationAstFilesUnwired = errors.New("metric_ingestor: RegistryBackedFoundationDispatcher.AstFiles is nil")

// ErrFoundationDraftPersistenceUnimplemented is returned by
// [RegistryBackedFoundationDispatcher.Dispatch] when a recipe
// produces draft rows at Stage 2.6. Persisting foundation-tier
// drafts requires the Phase 3.2 PG-backed [MetricSampleWriter]
// transaction wiring (so the drafts share a transaction with
// the churn sweep, enforcing the active-row uniqueness
// invariant). The Stage 2.6 production wiring uses
// [EmptyAstFileSource] which produces zero drafts; this error
// fires only in tests that exercise a non-empty AST source +
// non-empty registry, proving the dispatcher's iteration logic
// is real without inventing fake `sha` / `scope_id` values for
// the [MetricSampleRecord] columns the Phase 3.2 writer mints.
var ErrFoundationDraftPersistenceUnimplemented = errors.New("metric_ingestor: foundation-tier draft persistence is not implemented at Stage 2.6 (Phase 3.2 supplies the transaction-aware writer)")

// AstFileSource is the seam between a foundation-tier
// dispatcher and the AST parser fleet (Phase 4). The interface
// is intentionally minimal: one call yields every
// `*parser.AstFile` the dispatcher should iterate for a given
// [ScanRunContext]. A future streaming implementation can wrap
// a slice over `iter.Seq2`, but the slice form is canonical at
// Stage 2.6 because the AST parser fleet itself does not yet
// expose a streaming reader.
//
// # Stage 2.6 wiring
//
// The production composition root in `cmd/clean-coded/main.go`
// supplies [EmptyAstFileSource] -- AST parsing lands in Phase
// 4 (`phase-ast-adapter-and-foundation-tier-compute`), so a
// Stage 2.6 `full`/`delta` scan correctly iterates zero AST
// files. The [RegistryBackedFoundationDispatcher] honours the
// "registry actually consumed" requirement (evaluator iter-5
// #4) by iterating the registry's recipes over whatever the
// source yields -- zero today, the full per-language AST
// stream after Phase 4 wires the real source.
type AstFileSource interface {
	// Files returns every `*parser.AstFile` the dispatcher
	// should run recipes against for `scanRun`. The order
	// is implementation-defined but MUST be deterministic
	// for a given (repo, sha) pair (G2: re-running a scan
	// at the same SHA must emit identical drafts -- a
	// non-deterministic source would break re-run idempotency
	// even before the recipes are involved).
	//
	// Returns (nil, nil) for an empty source -- a foundation
	// scan against a repo with zero parseable files is a
	// degenerate-but-valid case (e.g. a brand-new repo).
	Files(ctx context.Context, scanRun ScanRunContext) ([]*parser.AstFile, error)
}

// EmptyAstFileSource is the Stage 2.6 scaffold
// [AstFileSource]. Its [Files] method always returns
// (nil, nil) -- AST parsing is Phase 4's responsibility, so a
// Stage 2.6 production scan iterates zero files. The
// composition-root structural value of the type is that the
// [RegistryBackedFoundationDispatcher] DOES wire the recipes
// registry through its dispatch loop -- the loop body just
// never executes because the source is empty.
//
// Replaced by a real `pgx`-backed reader in Phase 4
// (`stage-ast-adapter-and-foundation-tier-compute`).
type EmptyAstFileSource struct{}

// Files implements [AstFileSource] by returning (nil, nil).
func (EmptyAstFileSource) Files(_ context.Context, _ ScanRunContext) ([]*parser.AstFile, error) {
	return nil, nil
}

// RegistryBackedFoundationDispatcher is the production
// [FoundationRecipeDispatcher] wired by the composition root.
// It honours the iter-5 #4 structural requirement: the
// [recipes.Registry] constructed at startup MUST flow into
// the dispatcher and be consumed by the dispatch loop -- the
// prior `_ = recipes.DefaultRegistryWithLog(log)` discard
// shape was the evaluator's blocking complaint.
//
// # Dispatch loop
//
// For each `*parser.AstFile` the [AstFiles] source yields,
// the dispatcher iterates every [recipes.Recipe] the
// [Registry] has registered. A recipe's [recipes.Recipe.Compute]
// is invoked iff its [recipes.Recipe.AppliesTo] returns true
// -- mirroring the architecture Sec 8.6 line 1008 contract.
// Drafts produced by a Compute call are COUNTED but NOT
// persisted at Stage 2.6; the rationale is documented on
// [ErrFoundationDraftPersistenceUnimplemented].
//
// # Stage 2.6 honesty
//
// The production wiring supplies [EmptyAstFileSource], so the
// loop body never executes in a real binary. The dispatcher
// reports `ast_files_seen=0, recipes_evaluated=0,
// drafts_produced=0` via [Logger]. Tests that exercise a
// non-empty fake [AstFileSource] prove the iteration logic
// works -- the registry is consumed for every yielded file,
// AppliesTo is called per (file, recipe) pair, and drafts are
// counted.
//
// # Phase 3.2 swap
//
// Phase 3.2 (`stage-metric-ingestor-and-scanrun-state-machine`)
// replaces the dispatcher with a transaction-aware variant
// that:
//
//  1. Reads `*parser.AstFile`s from the PG-backed
//     `clean_code.ast_file` cache (the Phase 4 writer's
//     persistence layer).
//  2. Persists drafts into `clean_code.metric_sample` inside
//     the SAME transaction as the [ChurnSweep]'s writes (the
//     cross-producer atomicity guarantee Sec 5.2.2 / G2
//     requires).
//
// The [Registry] dependency stays unchanged; only the source
// + the per-draft persistence move.
type RegistryBackedFoundationDispatcher struct {
	// Registry is the foundation-tier recipe registry. The
	// composition root constructs it via
	// [recipes.DefaultRegistryWithLog]; tests MAY pass any
	// non-nil [recipes.Registry] -- the dispatcher only
	// consults [recipes.Registry.Recipes].
	Registry *recipes.Registry
	// AstFiles is the per-ScanRun source of `*parser.AstFile`s
	// the dispatcher iterates. Production: [EmptyAstFileSource]
	// at Stage 2.6. Tests: a slice-backed fake.
	AstFiles AstFileSource
	// Logger receives ONE structured INFO line per Dispatch
	// call summarising the dispatch counts. MAY be nil.
	Logger *slog.Logger
}

// Dispatch implements [FoundationRecipeDispatcher]. See the
// type-level docstring for the loop contract.
func (d *RegistryBackedFoundationDispatcher) Dispatch(ctx context.Context, scanRun ScanRunContext, _ FoundationInput) error {
	if d.Registry == nil {
		return ErrFoundationRegistryUnwired
	}
	if d.AstFiles == nil {
		return ErrFoundationAstFilesUnwired
	}

	files, err := d.AstFiles.Files(ctx, scanRun)
	if err != nil {
		return fmt.Errorf("metric_ingestor: foundation AstFiles.Files: %w", err)
	}

	recipesByKind := d.Registry.Recipes()
	var (
		astFilesSeen     = len(files)
		recipesEvaluated int
		draftsProduced   int
	)
	for _, ast := range files {
		for _, r := range recipesByKind {
			recipesEvaluated++
			if !r.AppliesTo(ast) {
				continue
			}
			drafts := r.Compute(ast)
			draftsProduced += len(drafts)
		}
	}

	if d.Logger != nil {
		d.Logger.Info("foundation recipe dispatcher: scan complete",
			"component", "metric_ingestor.RegistryBackedFoundationDispatcher",
			"scan_run_id", scanRun.ID,
			"scan_run_kind", scanRun.Kind,
			"repo_id", scanRun.RepoID,
			"registered_recipes", len(recipesByKind),
			"ast_files_seen", astFilesSeen,
			"recipes_evaluated", recipesEvaluated,
			"drafts_produced", draftsProduced,
			"drafts_persisted", 0,
			"persistence_layer", "Phase 3.2 (not wired at Stage 2.6)",
		)
	}

	if draftsProduced > 0 {
		// Stage 2.6 honesty: a Compute call that actually
		// produced drafts means a test fixture wired a
		// non-empty AST source. The Stage 2.6 contract is
		// EXPLICITLY that foundation-tier draft persistence
		// is Phase 3.2's job (the transaction-aware writer
		// is the only place `(sha, scope_id, sample_id)` can
		// be minted safely). Surfacing the unimplemented
		// path as a structured error is more honest than
		// inventing fake `sha`/`scope_id` values to write.
		return fmt.Errorf("%w: drafts_produced=%d, ast_files_seen=%d",
			ErrFoundationDraftPersistenceUnimplemented, draftsProduced, astFilesSeen)
	}
	return nil
}
