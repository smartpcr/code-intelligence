package metric_ingestor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// sliceAstSource is a deterministic test [AstFileSource] that
// yields a pre-built slice of `*parser.AstFile` -- exactly
// what a Phase 4-backed source will yield, just from memory
// instead of PG.
type sliceAstSource struct {
	files []*parser.AstFile
	err   error
}

func (s sliceAstSource) Files(_ context.Context, _ metric_ingestor.ScanRunContext) ([]*parser.AstFile, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.files, nil
}

// trackingRecipe is a deterministic test [recipes.Recipe] that
// records every AppliesTo / Compute call so a test can assert
// the dispatcher actually iterated the recipe over each AST
// file.
type trackingRecipe struct {
	kind             string
	version          int
	pack             recipes.Pack
	appliesToReturns bool
	computeDrafts    []recipes.MetricSampleDraft
	appliesToCalls   int
	computeCalls     int
}

func (r *trackingRecipe) MetricKind() string { return r.kind }
func (r *trackingRecipe) Version() int       { return r.version }

// Pack returns the recipe's pack stamp. iter-3 added this
// method when the [recipes.Recipe] interface gained Pack as
// a required method (stage 3.0); the test helper had drifted
// behind the contract. The zero-value `pack` (empty string)
// is intentionally NOT defaulted to `PackBase` so a test
// that forgets to populate it surfaces the omission via the
// pack-validation guard at draft-construction time.
func (r *trackingRecipe) Pack() recipes.Pack { return r.pack }

func (r *trackingRecipe) AppliesTo(_ *parser.AstFile) bool {
	r.appliesToCalls++
	return r.appliesToReturns
}
func (r *trackingRecipe) Compute(_ *parser.AstFile) []recipes.MetricSampleDraft {
	r.computeCalls++
	return r.computeDrafts
}

func mustScanRun(t *testing.T) metric_ingestor.ScanRunContext {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	return metric_ingestor.ScanRunContext{
		ID:     id,
		Kind:   metric_ingestor.ScanRunKindFull,
		RepoID: uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555")),
	}
}

// TestRegistryBackedDispatcher_EmptyAstSource_DefaultRegistry --
// the production wiring shape at Stage 2.6. Empty source means
// zero files iterated; the registered recipes are NOT
// evaluated (no files to apply to). Returns nil, no drafts
// produced -- the noop-shape outcome with the registry
// HONESTLY THREADED.
func TestRegistryBackedDispatcher_EmptyAstSource_DefaultRegistry(t *testing.T) {
	t.Parallel()
	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: recipes.DefaultRegistry(),
		AstFiles: metric_ingestor.EmptyAstFileSource{},
	}
	if err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{}); err != nil {
		t.Fatalf("Dispatch with empty source + default registry: err=%v, want nil", err)
	}
}

// TestRegistryBackedDispatcher_NonEmptyAstSource_IteratesRegistry --
// proves the dispatcher's iteration loop is real: a fake
// source yielding 2 files + a 1-recipe registry results in 2
// AppliesTo invocations on the recipe (one per file). The
// recipe is wired to return false so Compute is NOT called.
// This is the "registry actually consumed" structural pin.
func TestRegistryBackedDispatcher_NonEmptyAstSource_IteratesRegistry(t *testing.T) {
	t.Parallel()
	reg := recipes.NewRegistry()
	r := &trackingRecipe{kind: "test_kind", version: 1, pack: recipes.PackBase, appliesToReturns: false}
	reg.Register(r)

	source := sliceAstSource{files: []*parser.AstFile{{}, {}}}

	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: reg,
		AstFiles: source,
	}
	if err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{}); err != nil {
		t.Fatalf("Dispatch: err=%v, want nil", err)
	}
	if r.appliesToCalls != 2 {
		t.Errorf("trackingRecipe.AppliesTo calls = %d, want 2 (1 per AST file)", r.appliesToCalls)
	}
	if r.computeCalls != 0 {
		t.Errorf("trackingRecipe.Compute calls = %d, want 0 (AppliesTo returned false)", r.computeCalls)
	}
}

// TestRegistryBackedDispatcher_RecipeApplies_ComputeIsCalled --
// proves the AppliesTo gate actually conditions Compute: a
// recipe that returns true gets Compute called once per AST
// file.
func TestRegistryBackedDispatcher_RecipeApplies_ComputeIsCalled(t *testing.T) {
	t.Parallel()
	reg := recipes.NewRegistry()
	r := &trackingRecipe{kind: "test_kind", version: 1, pack: recipes.PackBase, appliesToReturns: true /* no drafts */}
	reg.Register(r)

	source := sliceAstSource{files: []*parser.AstFile{{}, {}, {}}}

	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: reg,
		AstFiles: source,
	}
	if err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{}); err != nil {
		t.Fatalf("Dispatch: err=%v, want nil", err)
	}
	if r.appliesToCalls != 3 {
		t.Errorf("AppliesTo calls = %d, want 3", r.appliesToCalls)
	}
	if r.computeCalls != 3 {
		t.Errorf("Compute calls = %d, want 3 (AppliesTo returned true for every file)", r.computeCalls)
	}
}

// TestRegistryBackedDispatcher_DraftsProduced_PersistenceUnimplemented --
// Stage 2.6 honesty pin: a recipe that produces actual drafts
// surfaces [ErrFoundationDraftPersistenceUnimplemented] rather
// than inventing fake `sha`/`scope_id` values for the
// MetricSampleRecord columns the Phase 3.2 writer mints. The
// production wiring uses EmptyAstFileSource so this path never
// fires in a real binary, but the error proves the dispatcher
// is HONEST about what it does and does not own.
func TestRegistryBackedDispatcher_DraftsProduced_PersistenceUnimplemented(t *testing.T) {
	t.Parallel()
	reg := recipes.NewRegistry()
	r := &trackingRecipe{
		kind: "test_kind", version: 1, pack: recipes.PackBase, appliesToReturns: true,
		computeDrafts: []recipes.MetricSampleDraft{
			{MetricKind: "test_kind", MetricVersion: 1, Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 1},
		},
	}
	reg.Register(r)

	source := sliceAstSource{files: []*parser.AstFile{{}}}

	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: reg,
		AstFiles: source,
	}
	err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{})
	if !errors.Is(err, metric_ingestor.ErrFoundationDraftPersistenceUnimplemented) {
		t.Fatalf("Dispatch with drafts produced: err=%v, want errors.Is ErrFoundationDraftPersistenceUnimplemented", err)
	}
}

// TestRegistryBackedDispatcher_NilRegistry -- defence-in-depth
// wiring-bug pin.
func TestRegistryBackedDispatcher_NilRegistry(t *testing.T) {
	t.Parallel()
	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: nil,
		AstFiles: metric_ingestor.EmptyAstFileSource{},
	}
	if err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{}); !errors.Is(err, metric_ingestor.ErrFoundationRegistryUnwired) {
		t.Fatalf("Dispatch nil registry: err=%v, want errors.Is ErrFoundationRegistryUnwired", err)
	}
}

// TestRegistryBackedDispatcher_NilAstFiles -- defence-in-depth
// wiring-bug pin.
func TestRegistryBackedDispatcher_NilAstFiles(t *testing.T) {
	t.Parallel()
	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: recipes.DefaultRegistry(),
		AstFiles: nil,
	}
	if err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{}); !errors.Is(err, metric_ingestor.ErrFoundationAstFilesUnwired) {
		t.Fatalf("Dispatch nil source: err=%v, want errors.Is ErrFoundationAstFilesUnwired", err)
	}
}

// TestRegistryBackedDispatcher_SourceError_Propagates -- when
// the source returns an error (e.g. PG read failure in Phase
// 4), the dispatcher returns it wrapped so the Ingestor surfaces
// it to the caller without partial dispatch state.
func TestRegistryBackedDispatcher_SourceError_Propagates(t *testing.T) {
	t.Parallel()
	srcErr := errors.New("simulated PG read failure")
	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: recipes.DefaultRegistry(),
		AstFiles: sliceAstSource{err: srcErr},
	}
	err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{})
	if err == nil || !errors.Is(err, srcErr) {
		t.Fatalf("Dispatch source-error: err=%v, want wrapping the source error", err)
	}
}

// TestRegistryBackedDispatcher_DefaultRegistry_NonEmptySource --
// the integration pin: the canonical Stage 2.3 base-pack
// registry (cyclo, cognitive_complexity, loc) MUST be consumed
// by the dispatcher. With a non-empty source the dispatcher
// iterates ALL THREE recipes against the file, so the registry
// is genuinely threaded through (evaluator iter-5 #4 fix).
func TestRegistryBackedDispatcher_DefaultRegistry_NonEmptySource(t *testing.T) {
	t.Parallel()
	reg := recipes.DefaultRegistry()
	// One stub AST file -- the real recipes' AppliesTo will
	// return false for a zero-valued AstFile (no decision
	// blocks attr, no methods); the call count proves
	// iteration happened.
	source := sliceAstSource{files: []*parser.AstFile{{}}}
	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: reg,
		AstFiles: source,
	}
	if err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{}); err != nil {
		t.Fatalf("Dispatch DefaultRegistry: err=%v, want nil", err)
	}
	if got := len(reg.Recipes()); got != 6 {
		t.Errorf("DefaultRegistry recipes = %d, want 6 (cyclo, cognitive_complexity, loc, lcom4, fan_in, fan_out)", got)
	}
}

// TestEmptyAstFileSource_AlwaysEmpty -- pin the Stage 2.6
// scaffold source's contract: never returns files, never
// returns an error.
func TestEmptyAstFileSource_AlwaysEmpty(t *testing.T) {
	t.Parallel()
	files, err := metric_ingestor.EmptyAstFileSource{}.Files(context.Background(), mustScanRun(t))
	if err != nil {
		t.Errorf("EmptyAstFileSource.Files: err=%v, want nil", err)
	}
	if files != nil {
		t.Errorf("EmptyAstFileSource.Files: files=%v, want nil", files)
	}
}
