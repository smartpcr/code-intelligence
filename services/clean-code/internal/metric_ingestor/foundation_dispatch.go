package metric_ingestor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
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
// produces draft rows AND no [MetricSampleWriter] is wired.
// Before Stage 3.2 the entire foundation tier surfaced this
// error whenever drafts were produced because the
// transaction-aware writer did not yet exist; Stage 3.2 lands
// the writer and the dispatcher now persists drafts when
// [RegistryBackedFoundationDispatcher.Writer] is non-nil. The
// sentinel is RETAINED (not removed) so callers that
// deliberately wire a dispatcher without a writer
// (e.g. iteration-logic tests, configuration-validation tests)
// keep the "you forgot the writer" surface they pin on.
var ErrFoundationDraftPersistenceUnimplemented = errors.New("metric_ingestor: foundation-tier draft persistence requires RegistryBackedFoundationDispatcher.Writer to be wired")

// ErrFoundationSHAMissing is returned when a wired Writer
// would persist a draft but [FoundationInput.SHA] is empty.
// Every persisted [MetricSampleRecord] MUST stamp the
// claimed commit's SHA on its row (architecture Sec 5.2.1 --
// `MetricSample.sha` is part of the active-row natural key);
// silently writing the zero string would smuggle a bogus row
// into the active index.
var ErrFoundationSHAMissing = errors.New("metric_ingestor: FoundationInput.SHA is required when a Writer is wired")

// FoundationScopeResolver translates a batch of recipe-emitted
// [recipes.ScopeRef] values (intra-file `local:N` placeholders +
// canonical signature seeds) into the durable
// `scope_binding.scope_id` UUIDs the [MetricSampleRecord]
// requires. The seam exists because foundation drafts carry
// only the parser's intra-file placeholder; minting the
// durable UUID belongs to the Ingestor, not the recipe (G1:
// recipes never mint UUIDs).
//
// # Batched contract (iter-3 evaluator item 3/4)
//
// The interface accepts a slice of refs rather than a single
// ref so production resolvers can amortise a single PG
// transaction (advisory lock + SELECT + INSERT) across all
// scopes a scan emits. The returned slice MUST be parallel to
// the input slice: `ids[i]` is the resolved `scope_id` for
// `refs[i]`. A length mismatch is a contract violation; the
// dispatcher treats `len(ids) != len(refs)` as a fatal error.
//
// # Production resolver
//
// The production resolver is [PGScopeBindingResolver]
// (`pg_scope_binding_resolver.go`), which delegates to
// [storage.ScopeBindingWriter.Write]. That writer takes a
// pg_advisory_xact_lock per repo, looks up each candidate by
// natural key, reuses the persisted `first_seen_sha` when a
// row already exists, and INSERTs fresh rows otherwise. G2
// stability ("the same logical scope produces the same
// scope_id across SHAs") is supplied BY the writer's
// natural-key lookup -- the resolver itself is a thin
// adapter.
//
// # Scaffold / in-memory resolver
//
// [DefaultFoundationScopeResolver] is the scaffold-mode
// resolver used when no `*sql.DB` is wired. It calls
// [scope.DeriveScopeID] directly with the scan SHA as
// `first_seen_sha`. G2 stability is provided only WITHIN a
// single SHA (re-running the dispatcher at the same SHA
// emits the same scope_id); cross-SHA stability requires the
// PG resolver.
type FoundationScopeResolver interface {
	// ResolveScopeIDs returns the durable
	// `scope_binding.scope_id` UUIDs for the given batch of
	// refs against `repoID` and `sha`. The returned slice
	// MUST be parallel to `refs` (same length, same order);
	// callers zip back to drafts by index. Implementations
	// SHOULD batch a single PG round-trip rather than N
	// independent calls.
	ResolveScopeIDs(ctx context.Context, repoID uuid.UUID, refs []recipes.ScopeRef, sha string) ([]uuid.UUID, error)
}

// DefaultFoundationScopeResolver is the scaffold-mode
// [FoundationScopeResolver]. It calls
// [BuildCanonicalSignatureForRefURL] (with a
// [SyntheticRepoURLLookup]-supplied stamp) to build the
// canonical signature (per-kind dispatch via `scope.Build*`)
// and then [scope.DeriveScopeID](repoID, ref.Kind,
// signature, sha) to mint the scope_id. The scan SHA is
// used as `first_seen_sha`, which is sufficient for tests
// and in-memory deployments but does NOT provide cross-SHA
// G2 stability. Production deployments wire
// [PGScopeBindingResolver] which delegates to the writer's
// natural-key lookup for persistent G2 stability AND uses
// [PGRepoURLLookup] for real-URL parity (iter-5 evaluator
// item 2).
//
// iter-4 evaluator item 1: builds canonical signatures via
// the same [BuildCanonicalSignatureForRefURL] helper as the
// PG resolver so the canonical signature shape is byte-
// identical regardless of mode (the URL stamp differs --
// scaffold uses synthetic; PG-mode reads
// `clean_code.repo.repo_url` via [PGRepoURLLookup]).
type DefaultFoundationScopeResolver struct {
	// URLs supplies the per-repo URL stamp the signature
	// helper uses. nil falls back to [SyntheticRepoURLLookup]
	// (the iter-4 behaviour), so existing tests that pin
	// the `clean-code-repo:<repoID>` literal continue to
	// pass.
	URLs RepoURLLookup
}

// ResolveScopeIDs implements [FoundationScopeResolver].
func (d DefaultFoundationScopeResolver) ResolveScopeIDs(ctx context.Context, repoID uuid.UUID, refs []recipes.ScopeRef, sha string) ([]uuid.UUID, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	urls := d.URLs
	if urls == nil {
		urls = SyntheticRepoURLLookup{}
	}
	repoURL, err := urls.LookupRepoURL(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("metric_ingestor: DefaultFoundationScopeResolver.LookupRepoURL (repo_id=%s): %w", repoID, err)
	}
	ids := make([]uuid.UUID, len(refs))
	for i, ref := range refs {
		if ref.QualifiedName == "" && ref.Kind != scope.KindRepo {
			return nil, fmt.Errorf("metric_ingestor: ScopeRef.QualifiedName is empty for refs[%d] kind=%q", i, ref.Kind)
		}
		sig, sigErr := BuildCanonicalSignatureForRefURL(repoURL, ref)
		if sigErr != nil {
			return nil, fmt.Errorf("metric_ingestor: DefaultFoundationScopeResolver.BuildCanonicalSignatureForRefURL refs[%d]: %w", i, sigErr)
		}
		id, err := scope.DeriveScopeID(repoID, ref.Kind, sig, sha)
		if err != nil {
			return nil, fmt.Errorf("metric_ingestor: derive scope_id for refs[%d] kind=%q name=%q sha=%q: %w",
				i, ref.Kind, ref.QualifiedName, sha, err)
		}
		ids[i] = id
	}
	return ids, nil
}

// AstFileSource is the seam between a foundation-tier
// dispatcher and the AST parser fleet (Phase 4). The interface
// is intentionally minimal: one call yields every
// `*parser.AstFile` the dispatcher should iterate for a given
// [ScanRunContext]. A future streaming implementation can wrap
// a slice over `iter.Seq2`, but the slice form is canonical at
// Stage 2.6 because the AST parser fleet itself does not yet
// expose a streaming reader.
//
// # Stage 3.2 wiring
//
// The composition root in `cmd/clean-coded/main.go` wires
// [DirectoryAstFileSource] when `cfg.AstScanRoot` is set
// (Stage 3.2 brief item 3 -- "drives the recipe registry over
// the parsed AST"). When `cfg.AstScanRoot` is unset the root
// falls back to [EmptyAstFileSource] so a deployment without
// a checkout-resolver upstream still boots.
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
// (nil, nil) -- a deployment with no on-disk checkout
// upstream falls back to this so the dispatcher still
// completes cleanly with zero drafts.
//
// Phase 4 (`stage-ast-adapter-and-foundation-tier-compute`)
// replaces this scaffold with the parser-fleet adapter that
// loads from `clean_code.ast_file`. Stage 3.2 also ships
// [DirectoryAstFileSource] which walks a local directory --
// it's a real source for environments where the checkout
// resolver materialises working trees on disk.
type EmptyAstFileSource struct{}

// Files implements [AstFileSource] by returning (nil, nil).
func (EmptyAstFileSource) Files(_ context.Context, _ ScanRunContext) ([]*parser.AstFile, error) {
	return nil, nil
}

// RegistryBackedFoundationDispatcher is the production
// [FoundationRecipeDispatcher] wired by the composition root.
// It iterates the recipes registry over every AST file the
// source yields. With a wired [Writer] + [Scopes] resolver,
// produced drafts are converted to [MetricSampleRecord]s and
// persisted via [MetricSampleWriter.WriteBatch] in one batch
// per dispatch call.
//
// # Dispatch loop
//
// For each `*parser.AstFile` the [AstFiles] source yields,
// the dispatcher iterates every [recipes.Recipe] the
// [Registry] has registered. A recipe's [recipes.Recipe.Compute]
// is invoked iff its [recipes.Recipe.AppliesTo] returns true
// -- mirroring the architecture Sec 8.6 line 1008 contract.
// Produced drafts are accumulated and, if a Writer is wired,
// persisted in one batch at the end of the dispatch call.
//
// # Iteration honesty
//
// When the source yields zero files the dispatcher reports
// `ast_files_seen=0, recipes_evaluated=0, drafts_produced=0,
// drafts_persisted=0` via [Logger]. Tests that exercise a
// non-empty fake source prove the iteration logic works --
// the registry is consumed for every yielded file, AppliesTo
// is called per (file, recipe) pair, and drafts are counted
// and persisted (when a writer is wired).
//
// # Writer wiring
//
// The Stage 3.2 composition root wires
// [Writer] = [PGMetricSampleWriter] (when `cfg.PostgresURL`
// is set) or [InMemoryMetricSampleWriter] (scaffold mode).
// Tests that exercise the iteration logic without a real
// writer wire `Writer = nil` -- in that mode, a dispatch
// call that PRODUCES drafts returns
// [ErrFoundationDraftPersistenceUnimplemented] (an explicit
// wiring-bug signal).
//
// # ProducerRunID minting
//
// The [Writer] receives [MetricSampleRecord.ProducerRunID]
// set to [ScanRunContext.ID] (the active ScanRun) and
// [MetricSampleRecord.SampleID] minted via
// [SampleIDFactory] (default: [uuid.NewV4]). Tests can
// inject a deterministic factory via [SampleIDFactory] to
// pin the IDs.
type RegistryBackedFoundationDispatcher struct {
	// Registry is the foundation-tier recipe registry. The
	// composition root constructs it via
	// [recipes.DefaultRegistryWithLog]; tests MAY pass any
	// non-nil [recipes.Registry] -- the dispatcher only
	// consults [recipes.Registry.Recipes].
	Registry *recipes.Registry
	// AstFiles is the per-ScanRun source of `*parser.AstFile`s
	// the dispatcher iterates. Production:
	// [DirectoryAstFileSource] (or [EmptyAstFileSource] when
	// the deployment has no on-disk checkouts). Tests: a
	// slice-backed fake.
	AstFiles AstFileSource
	// Writer persists every produced draft via
	// [MetricSampleWriter.WriteBatch]. MAY be nil -- in nil
	// mode the dispatcher still iterates the registry and
	// counts drafts, but a dispatch call that produced any
	// drafts returns [ErrFoundationDraftPersistenceUnimplemented].
	Writer MetricSampleWriter
	// Scopes resolves recipe-emitted [recipes.ScopeRef] values
	// to durable `scope_binding.scope_id` UUIDs. MAY be nil;
	// the dispatcher falls back to
	// [DefaultFoundationScopeResolver] (which derives the UUID
	// via [scope.DeriveScopeID] using the scan SHA).
	Scopes FoundationScopeResolver
	// SampleIDFactory mints each persisted record's `sample_id`.
	// MAY be nil; falls back to [uuid.NewV4]. Tests inject a
	// deterministic factory to pin IDs.
	SampleIDFactory func() (uuid.UUID, error)
	// Logger receives ONE structured INFO line per Dispatch
	// call summarising the dispatch counts. MAY be nil.
	Logger *slog.Logger
}

// Dispatch implements [FoundationRecipeDispatcher]. See the
// type-level docstring for the loop + persistence contract.
func (d *RegistryBackedFoundationDispatcher) Dispatch(ctx context.Context, scanRun ScanRunContext, input FoundationInput) error {
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
		drafts           []recipes.MetricSampleDraft
	)
	for _, ast := range files {
		for _, r := range recipesByKind {
			recipesEvaluated++
			if !r.AppliesTo(ast) {
				continue
			}
			drafts = append(drafts, r.Compute(ast)...)
		}
	}

	draftsProduced := len(drafts)
	draftsPersisted := 0

	defer func() {
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
				"drafts_persisted", draftsPersisted,
				"persistence_wired", d.Writer != nil,
			)
		}
	}()

	if draftsProduced == 0 {
		return nil
	}

	if d.Writer == nil {
		// Iteration-logic tests pin this surface to prove
		// "you forgot to wire a writer" is loud, not silent.
		return fmt.Errorf("%w: drafts_produced=%d, ast_files_seen=%d",
			ErrFoundationDraftPersistenceUnimplemented, draftsProduced, astFilesSeen)
	}
	if input.SHA == "" {
		return fmt.Errorf("%w: drafts_produced=%d", ErrFoundationSHAMissing, draftsProduced)
	}

	scopes := d.Scopes
	if scopes == nil {
		scopes = DefaultFoundationScopeResolver{}
	}
	sampleFactory := d.SampleIDFactory
	if sampleFactory == nil {
		sampleFactory = uuid.NewV4
	}

	// iter-3 evaluator items 3+4: resolve all scope_ids
	// in ONE batched call so [PGScopeBindingResolver] can
	// amortise a single advisory-locked transaction across
	// the whole batch (rather than N round-trips). The
	// returned slice MUST be parallel to refs; we treat a
	// length mismatch as a fatal contract violation.
	refs := make([]recipes.ScopeRef, len(drafts))
	for i, draft := range drafts {
		refs[i] = draft.Scope
	}
	scopeIDs, scopeErr := scopes.ResolveScopeIDs(ctx, scanRun.RepoID, refs, input.SHA)
	if scopeErr != nil {
		return fmt.Errorf("metric_ingestor: dispatch resolve scope_ids: %w", scopeErr)
	}
	if len(scopeIDs) != len(refs) {
		return fmt.Errorf("metric_ingestor: dispatch resolve scope_ids: got %d ids for %d refs (resolver contract violation)",
			len(scopeIDs), len(refs))
	}

	records := make([]MetricSampleRecord, 0, len(drafts))
	for i, draft := range drafts {
		sampleID, idErr := sampleFactory()
		if idErr != nil {
			return fmt.Errorf("metric_ingestor: dispatch draft[%d] mint sample_id: %w", i, idErr)
		}
		records = append(records, MetricSampleRecord{
			SampleID:      sampleID,
			RepoID:        scanRun.RepoID,
			SHA:           input.SHA,
			ScopeID:       scopeIDs[i],
			MetricKind:    draft.MetricKind,
			MetricVersion: draft.MetricVersion,
			Pack:          draft.Pack,
			Source:        draft.Source,
			Value:         draft.Value,
			Attrs:         draft.Attrs,
			ProducerRunID: scanRun.ID,
		})
	}

	if err := d.Writer.WriteBatch(ctx, records); err != nil {
		return fmt.Errorf("metric_ingestor: dispatch WriteBatch: %w", err)
	}
	draftsPersisted = len(records)
	return nil
}
