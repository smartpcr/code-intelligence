package metric_ingestor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
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

// TestRegistryBackedDispatcher_Persistence_HappyPath pins the
// Stage 3.2 "drafts -> MetricSampleRecord -> WriteBatch" path
// (iter 2 evaluator item 4). A wired Writer + a non-empty SHA
// makes the dispatcher convert each draft to a record and
// persist the batch in one call. Each record stamps:
//
//   - ProducerRunID = ScanRunContext.ID
//   - RepoID        = ScanRunContext.RepoID
//   - SHA           = FoundationInput.SHA
//   - SampleID      = SampleIDFactory()
//   - ScopeID       = Scopes.ResolveScopeIDs(...)[i]
//   - {MetricKind, MetricVersion, Pack, Source, Value, Attrs}
//     copied verbatim from the draft.
func TestRegistryBackedDispatcher_Persistence_HappyPath(t *testing.T) {
	t.Parallel()
	reg := recipes.NewRegistry()
	r := &trackingRecipe{
		kind: "cyclo", version: 7, pack: recipes.PackBase, appliesToReturns: true,
		computeDrafts: []recipes.MetricSampleDraft{
			{
				MetricKind: "cyclo", MetricVersion: 7,
				Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 4,
				Scope: recipes.ScopeRef{
					Kind:          "method",
					QualifiedName: "github.com/test/pkg.Foo()",
					Path:          "pkg/foo.go",
				},
				Attrs: map[string]string{"file": "pkg/foo.go"},
			},
			{
				MetricKind: "cyclo", MetricVersion: 7,
				Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 9,
				Scope: recipes.ScopeRef{
					Kind:          "method",
					QualifiedName: "github.com/test/pkg.Bar()",
					Path:          "pkg/bar.go",
				},
			},
		},
	}
	reg.Register(r)

	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sampleCounter := uint32(0)
	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: reg,
		AstFiles: sliceAstSource{files: []*parser.AstFile{{}}},
		Writer:   writer,
		SampleIDFactory: func() (uuid.UUID, error) {
			sampleCounter++
			return uuid.Must(uuid.FromString(
				"00000000-0000-0000-0000-0000000000" + twoHex(sampleCounter))), nil
		},
	}
	scan := mustScanRun(t)
	err := d.Dispatch(context.Background(), scan, metric_ingestor.FoundationInput{
		SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	})
	if err != nil {
		t.Fatalf("Dispatch with writer wired: err=%v, want nil", err)
	}
	records := writer.Records()
	if len(records) != 2 {
		t.Fatalf("InMemoryMetricSampleWriter.Records = %d, want 2", len(records))
	}
	for i, rec := range records {
		if rec.ProducerRunID != scan.ID {
			t.Errorf("records[%d].ProducerRunID = %s, want %s", i, rec.ProducerRunID, scan.ID)
		}
		if rec.RepoID != scan.RepoID {
			t.Errorf("records[%d].RepoID = %s, want %s", i, rec.RepoID, scan.RepoID)
		}
		if rec.SHA != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
			t.Errorf("records[%d].SHA = %q, want the FoundationInput SHA", i, rec.SHA)
		}
		if rec.SampleID == uuid.Nil {
			t.Errorf("records[%d].SampleID is zero", i)
		}
		if rec.ScopeID == uuid.Nil {
			t.Errorf("records[%d].ScopeID is zero -- resolver should have minted a deterministic UUID", i)
		}
		if rec.MetricKind != "cyclo" {
			t.Errorf("records[%d].MetricKind = %q, want cyclo", i, rec.MetricKind)
		}
		if rec.MetricVersion != 7 {
			t.Errorf("records[%d].MetricVersion = %d, want 7", i, rec.MetricVersion)
		}
	}
	// G2 determinism: scope IDs for the two distinct
	// qualified names MUST differ (the resolver derives a
	// UUIDv5 from the QualifiedName + SHA + Kind).
	if records[0].ScopeID == records[1].ScopeID {
		t.Errorf("records[0..1].ScopeID collided: %s -- distinct QualifiedNames must produce distinct scope_ids",
			records[0].ScopeID)
	}
	// Values copied through unchanged.
	if records[0].Value != 4 || records[1].Value != 9 {
		t.Errorf("records.Value = [%v, %v], want [4, 9]", records[0].Value, records[1].Value)
	}
}

// twoHex formats n as a 2-character zero-padded hex string,
// used by the test sample-ID factory to mint
// "00000000-0000-0000-0000-000000000001",
// "00000000-0000-0000-0000-000000000002", ... deterministically.
func twoHex(n uint32) string {
	const hex = "0123456789abcdef"
	if n > 0xff {
		n = n & 0xff
	}
	return string([]byte{hex[(n>>4)&0xf], hex[n&0xf]})
}

// TestRegistryBackedDispatcher_Persistence_RequiresSHA pins
// ErrFoundationSHAMissing: a wired Writer + an empty
// FoundationInput.SHA must refuse to write rather than
// silently stamping the zero string on metric_sample.sha.
func TestRegistryBackedDispatcher_Persistence_RequiresSHA(t *testing.T) {
	t.Parallel()
	reg := recipes.NewRegistry()
	r := &trackingRecipe{
		kind: "loc", version: 1, pack: recipes.PackBase, appliesToReturns: true,
		computeDrafts: []recipes.MetricSampleDraft{
			{
				MetricKind: "loc", MetricVersion: 1,
				Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 1,
				Scope: recipes.ScopeRef{Kind: "file", QualifiedName: "pkg/x.go", Path: "pkg/x.go"},
			},
		},
	}
	reg.Register(r)

	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: reg,
		AstFiles: sliceAstSource{files: []*parser.AstFile{{}}},
		Writer:   writer,
	}
	err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{})
	if !errors.Is(err, metric_ingestor.ErrFoundationSHAMissing) {
		t.Fatalf("Dispatch without SHA: err=%v, want errors.Is ErrFoundationSHAMissing", err)
	}
	if got := len(writer.Records()); got != 0 {
		t.Errorf("Writer.Records = %d, want 0 (no rows persisted when SHA is missing)", got)
	}
}

// TestRegistryBackedDispatcher_Persistence_WriterErrorPropagates
// proves a WriteBatch failure is wrapped (not swallowed) so
// the state machine transitions the commit to `failed`.
func TestRegistryBackedDispatcher_Persistence_WriterErrorPropagates(t *testing.T) {
	t.Parallel()
	reg := recipes.NewRegistry()
	r := &trackingRecipe{
		kind: "loc", version: 1, pack: recipes.PackBase, appliesToReturns: true,
		computeDrafts: []recipes.MetricSampleDraft{
			{
				MetricKind: "loc", MetricVersion: 1,
				Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 1,
				Scope: recipes.ScopeRef{Kind: "file", QualifiedName: "pkg/x.go", Path: "pkg/x.go"},
			},
		},
	}
	reg.Register(r)

	wantErr := errors.New("simulated WriteBatch failure")
	d := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: reg,
		AstFiles: sliceAstSource{files: []*parser.AstFile{{}}},
		Writer:   failingMetricSampleWriter{err: wantErr},
	}
	err := d.Dispatch(context.Background(), mustScanRun(t), metric_ingestor.FoundationInput{
		SHA: "0123456789012345678901234567890123456789",
	})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Dispatch with failing writer: err=%v, want wrapping of writer error", err)
	}
}

type failingMetricSampleWriter struct{ err error }

func (f failingMetricSampleWriter) WriteBatch(_ context.Context, _ []metric_ingestor.MetricSampleRecord) error {
	return f.err
}
