// -----------------------------------------------------------------------
// <copyright file="orchestrator.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"runtime"
	"sort"
	"sync"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/walk"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// SyntheticRepoURLPrefix is the per-repo URL surrogate prefix
// the CLI orchestrator stamps into canonical signatures when
// minting durable scope IDs. The literal is identical to the
// production ingest path's
// `metric_ingestor.syntheticRepoStampPrefix`
// (`services/clean-code/internal/metric_ingestor/canonical_signature.go`
// line 68): both layers concatenate
// `"clean-code-repo:" + repoID.String()` to seed
// `scope.BuildRepo` / `scope.BuildPackage` / etc. so a CLI-
// side scope binding has the same canonical signature -- and
// therefore the same UUID per [scopebinding.MintScopeID] --
// as the production row for the same logical scope.
//
// The literal is duplicated rather than imported because
// REFACTOR-GUIDE `tech-spec.md` C2 forbids `internal/cli/*`
// from importing `internal/metric_ingestor/*` (the SQL-tainted
// production layer); a coordinated edit (here + the metric
// ingestor) is the only acceptable way to ever change the
// prefix, and both call sites are pinned by tests
// (`pg_scope_binding_resolver_test.go:137` on the production
// side; the orchestrator suite asserts the same prefix here).
const SyntheticRepoURLPrefix = "clean-code-repo:"

// Skip reason constants the orchestrator stamps on
// [walk.WalkSkip]s it emits in addition to the walker-side
// reasons in [walk.SkipReasonDirectory] etc. Kept as named
// constants (NOT inline strings) so the report writer and the
// e2e contract can grep a single source of truth.
const (
	// SkipReasonParserError is emitted when the parser
	// registry returns a non-nil error for a per-file
	// parse (e.g. unsupported language survives the
	// walker's extension filter; producer returned the
	// degraded sentinel without recovering bytes).
	SkipReasonParserError = "parser_error"

	// SkipReasonParserPanic is emitted when a per-file
	// parse PANICS. The orchestrator's per-job
	// `defer recover()` swallows the panic, emits this
	// skip, logs the recovered value with full structured
	// fields, and continues to the next file. The CLI
	// binary's exit code is `70` only when the panic
	// happens OUTSIDE a per-file parse (tech-spec Sec
	// 8.6); per-file panics are non-fatal.
	SkipReasonParserPanic = "parser_panic"

	// SkipReasonScopeBindingError is emitted when a
	// scope's canonical signature cannot be derived
	// (empty qualifiedName, NUL byte in a field, etc).
	// The scope is dropped from the [scopebinding.Table]
	// but the file's other scopes and recipes still run.
	SkipReasonScopeBindingError = "scope_binding_error"
)

// Options controls Orchestrator construction. The zero value
// is usable: any nil hook is filled with the production
// default in [New], so `New(Options{})` returns a fully
// wired orchestrator backed by [walk.NewDefaultWalker],
// [parser.DefaultRegistry], [recipes.DefaultRegistry], a
// fresh [scopebinding.NewTable], and a `GOMAXPROCS` worker
// pool.
type Options struct {
	// Walker is the L1 walker producing the (files, skips,
	// errs) channel triple. Defaults to a fresh
	// [walk.NewDefaultWalker] when nil.
	Walker walk.Walker

	// Parsers is the per-language parser registry. Defaults
	// to [parser.DefaultRegistry] when nil. Tests use
	// [parser.NewRegistry] to inject panicking / fake
	// parsers without leaking into the process-wide
	// default.
	Parsers *parser.Registry

	// Recipes is the per-file recipe registry. Defaults to
	// [recipes.DefaultRegistry] when nil.
	Recipes *recipes.Registry

	// ProjectRecipes is the project-level recipe registry.
	// Defaults to [recipes.DefaultProjectRegistry] when
	// nil.
	ProjectRecipes *recipes.ProjectRegistry

	// ScopeBindings is the [scopebinding.Table] populated
	// as a side-effect of [Orchestrator.Run]. Caller-
	// supplied so the same table can be threaded into
	// Stage 2.3's `rule_engine.Sample` rewrites and Stage
	// 4's prompt emitter. Defaults to a fresh
	// [scopebinding.NewTable] when nil.
	ScopeBindings *scopebinding.Table

	// Workers is the parse worker pool size. Defaults to
	// `runtime.GOMAXPROCS(0)` (clamped to >= 1) when
	// zero or negative. Tech-spec Sec 8.8 pins the
	// `GOMAXPROCS` default; tests pass `Workers: 1` for
	// deterministic emission orders independent of
	// host CPU count.
	Workers int

	// Logger is the structured logger the orchestrator
	// emits parser-error / parser-panic / scope-binding-
	// error diagnostics into. Defaults to a sink that
	// writes to [io.Discard] when nil so unit tests stay
	// quiet; the CLI composition root injects the
	// `cleanc`-shaped logger.
	Logger *slog.Logger
}

// Orchestrator drives the Stage 2.2 parse + recipe pipeline.
// Construct via [New]; the public surface is the single
// [Orchestrator.Run] method.
type Orchestrator struct {
	walker        walk.Walker
	parsers       *parser.Registry
	recipeReg     *recipes.Registry
	projectRecReg *recipes.ProjectRegistry
	scopeBindings *scopebinding.Table
	workers       int
	logger        *slog.Logger
}

// New returns an Orchestrator with the supplied options;
// any nil hook is filled with the production default
// documented on [Options].
func New(opts Options) *Orchestrator {
	o := &Orchestrator{
		walker:        opts.Walker,
		parsers:       opts.Parsers,
		recipeReg:     opts.Recipes,
		projectRecReg: opts.ProjectRecipes,
		scopeBindings: opts.ScopeBindings,
		workers:       opts.Workers,
		logger:        opts.Logger,
	}
	if o.walker == nil {
		o.walker = walk.NewDefaultWalker()
	}
	if o.parsers == nil {
		o.parsers = parser.DefaultRegistry()
	}
	if o.recipeReg == nil {
		o.recipeReg = recipes.DefaultRegistry()
	}
	if o.projectRecReg == nil {
		o.projectRecReg = recipes.DefaultProjectRegistry()
	}
	if o.scopeBindings == nil {
		o.scopeBindings = scopebinding.NewTable()
	}
	if o.workers <= 0 {
		o.workers = runtime.GOMAXPROCS(0)
		if o.workers <= 0 {
			o.workers = 1
		}
	}
	if o.logger == nil {
		o.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return o
}

// ScopeBindings returns the [scopebinding.Table] the
// orchestrator populates during [Orchestrator.Run]. Exposed
// so Stage 2.3 can resolve `MetricSampleDraft.Scope.LocalID`
// to a durable `scope_id` via the same table the orchestrator
// just stamped.
func (o *Orchestrator) ScopeBindings() *scopebinding.Table { return o.scopeBindings }

// ScopeBindingKey identifies a parser-side intra-file scope
// placeholder by `(path, local_id)`. The orchestrator builds
// a [Result.ScopeIDs] map from this key to the durable scope
// ID so Stage 2.3 can resolve a draft's
// `(Scope.Path, Scope.LocalID)` to the
// `rule_engine.Sample.ScopeID` without re-walking the AST.
type ScopeBindingKey struct {
	Path    string
	LocalID string
}

// Result is the orchestrator's full output for one
// [Orchestrator.Run] invocation.
type Result struct {
	// Files is the parsed `*AstFile` corpus, sorted by
	// `Path` for determinism. Empty when the walker
	// emitted no files.
	Files []*parser.AstFile

	// Drafts is the concatenated per-file + project-level
	// `MetricSampleDraft` slice, sorted by
	// `(metric_kind, path, scope_kind, local_id, metric_version)`.
	Drafts []recipes.MetricSampleDraft

	// Skips is the deduped, sorted list of skip rows from
	// BOTH the walker (directory_skip / gitignore / size_cap
	// / unsupported_language / symlink* / read_error /
	// empty) and the orchestrator (parser_error /
	// parser_panic / scope_binding_error). Sorted via
	// [walk.Skipped] for byte-identical determinism.
	Skips []walk.WalkSkip

	// ScopeIDs maps each `(path, local_id)` placeholder to
	// the durable UUID minted via
	// [scopebinding.MintScopeID]. Empty entries are
	// excluded (a scope whose canonical signature could
	// not be derived is reported as a [walk.WalkSkip] with
	// reason [SkipReasonScopeBindingError] and does NOT
	// appear here).
	ScopeIDs map[ScopeBindingKey]uuid.UUID

	// Diagnostics carries the run's dark-metric (Stage 2.5)
	// and future diagnostic rows. Always non-nil:
	// [Diagnostics.DarkMetrics] is `[]DarkMetric{}` when
	// every recipe lit up. The CLI's `--diagnostics` JSON
	// sidecar serialises this struct directly and the
	// report writer's diagnostics section reads it
	// verbatim. Per tech-spec REFACTOR-GUIDE Sec 8.7 the
	// orchestrator is the sole producer; downstream layers
	// MUST NOT add or rewrite rows after Run returns.
	Diagnostics Diagnostics
}

// Run executes the Stage 2.2 pipeline against `rootPath`:
//
//  1. Start the walker.
//  2. Spawn `o.workers` parse goroutines + a single dispatch
//     goroutine + a single skip-drain goroutine.
//  3. Each parse goroutine wraps its work in a per-job
//     `defer recover()` so a panic on file X does not kill
//     the worker; the panic is converted to a
//     [walk.WalkSkip]`{Reason: SkipReasonParserPanic}`.
//  4. Sort the parsed `*AstFile` corpus by `Path` and stamp
//     `Attrs[AttrModulePath]` from `repoCtx.ModulePath`
//     (deferred to recipes which gate on the attr).
//  5. Mint a durable scope ID for every emitted
//     [parser.AstScope] and insert one [scopebinding.ScopeBinding]
//     per scope into the configured table.
//  6. Run every per-file [recipes.Recipe] (gated by
//     `AppliesTo`) over the corpus, then every
//     [recipes.ProjectRecipe] over the full slice.
//  7. Sort drafts by
//     `(metric_kind, path, scope_kind, local_id, metric_version)`
//     for byte-identical determinism and return.
//
// Run returns [walk.ErrRootNotFound] verbatim when the
// walker reports the root is missing (the CLI maps that to
// exit code 2). Any other walker error is wrapped and
// returned. Per-file parse errors and per-file parse panics
// are captured as `WalkSkip` rows; Run does NOT fail on
// them.
//
// `repoCtx.RepoID` MUST be non-zero and `repoCtx.HeadSHA`
// MUST be non-empty. The CLI's `internal/cli/repocontext`
// package guarantees both by construction; Run guards them
// here so a wiring bug surfaces loudly.
func (o *Orchestrator) Run(ctx context.Context, repoCtx repocontext.RepoContext, rootPath string) (*Result, error) {
	if repoCtx.RepoID == uuid.Nil {
		return nil, fmt.Errorf("orchestrator: repoCtx.RepoID must not be uuid.Nil")
	}
	if repoCtx.HeadSHA == "" {
		return nil, fmt.Errorf("orchestrator: repoCtx.HeadSHA must not be empty")
	}

	result := &Result{
		ScopeIDs:    map[ScopeBindingKey]uuid.UUID{},
		Diagnostics: Diagnostics{DarkMetrics: []DarkMetric{}},
	}

	filesCh, skipsCh, errsCh := o.walker.Walk(ctx, rootPath)

	var skipMu sync.Mutex
	appendSkip := func(s walk.WalkSkip) {
		skipMu.Lock()
		result.Skips = append(result.Skips, s)
		skipMu.Unlock()
	}

	var drainWg sync.WaitGroup
	drainWg.Add(1)
	go func() {
		defer drainWg.Done()
		for s := range skipsCh {
			appendSkip(s)
		}
	}()

	jobs := make(chan walk.WalkedFile, o.workers)
	astCh := make(chan *parser.AstFile, o.workers)

	var workersWg sync.WaitGroup
	for i := 0; i < o.workers; i++ {
		workersWg.Add(1)
		go func() {
			defer workersWg.Done()
			for wf := range jobs {
				o.parseOne(ctx, wf, repoCtx, astCh, appendSkip)
			}
		}()
	}

	var dispatchWg sync.WaitGroup
	dispatchWg.Add(1)
	go func() {
		defer dispatchWg.Done()
		defer close(jobs)
		for wf := range filesCh {
			select {
			case jobs <- wf:
			case <-ctx.Done():
				// Drain remaining file events so the
				// walker can close cleanly; the
				// per-job recovery still applies on
				// the workers, but no NEW jobs are
				// queued under cancellation.
				for range filesCh {
				}
				return
			}
		}
	}()

	go func() {
		workersWg.Wait()
		close(astCh)
	}()

	var asts []*parser.AstFile
	for ast := range astCh {
		asts = append(asts, ast)
	}

	dispatchWg.Wait()
	drainWg.Wait()

	var firstFatal error
	for err := range errsCh {
		if errors.Is(err, walk.ErrRootNotFound) {
			return nil, err
		}
		if firstFatal == nil {
			firstFatal = err
		}
	}
	if firstFatal != nil {
		return nil, fmt.Errorf("orchestrator: walker error: %w", firstFatal)
	}

	sort.SliceStable(asts, func(i, j int) bool {
		return asts[i].GetPath() < asts[j].GetPath()
	})
	result.Files = asts

	for _, ast := range asts {
		o.mintBindings(ast, repoCtx, result, appendSkip)
	}

	dark := newDarkMetricAccumulator()
	var drafts []recipes.MetricSampleDraft
	for _, ast := range asts {
		for _, recipe := range o.recipeReg.Recipes() {
			if !recipe.AppliesTo(ast) {
				// Stage 2.5: attribute the no-op to a
				// missing parser-attr capability when
				// the recipe's metric_kind is in the
				// [metricAttrRequirements] table.
				// Recipes whose MetricKind is NOT in
				// the table (e.g. `loc`, which gates
				// only on degraded-not-set) are
				// silently dropped by `observe`'s
				// [metricAttrIndex] lookup.
				//
				// The degraded-AST guard below is
				// load-bearing: every gated recipe's
				// `AppliesTo` checks its parser-attr
				// capability FIRST and the degraded
				// gate SECOND (see
				// `cyclo.AppliesTo` etc. in
				// `internal/metrics/recipes`). Today's
				// fleet stamps no decision_blocks /
				// call_edges / field_accesses, so the
				// degraded branch is unreachable and
				// the only path into this `if` body
				// is the capability gap. Once a
				// future parser starts stamping a
				// capability on a file that is ALSO
				// degraded, `AppliesTo` would return
				// false at the degraded gate and the
				// file would otherwise reach
				// `observe` -- falsely inflating
				// `AffectedScopeCount` for a row
				// whose cause is degradation, not a
				// dark metric. Degraded files surface
				// via the skip-row path (parser-
				// returned degraded sentinel /
				// [SkipReasonParserError]); the dark-
				// metric taxonomy (tech-spec Sec 8.7
				// line 1000) covers ONLY unstamped
				// parser-attr capabilities.
				if ast.GetDegradedReason() != "" {
					continue
				}
				dark.observe(recipe.MetricKind(), ast)
				continue
			}
			drafts = append(drafts, recipe.Compute(ast)...)
		}
	}
	result.Diagnostics = dark.finalize()

	for _, projectRecipe := range o.projectRecReg.All() {
		drafts = append(drafts, projectRecipe.ComputeProject(asts)...)
	}

	sortDrafts(drafts)
	result.Drafts = drafts

	result.Skips = walk.Skipped(result.Skips)

	return result, nil
}

// parseOne is one worker's per-file body. The deferred
// `recover()` is intentionally PER-JOB (inside the for-range
// loop in [Run]) rather than per-worker so a panicking file
// does not silently exit the worker goroutine and starve the
// pool; the recovered panic surfaces as
// [SkipReasonParserPanic] and the worker continues to the
// next file.
func (o *Orchestrator) parseOne(
	ctx context.Context,
	wf walk.WalkedFile,
	repoCtx repocontext.RepoContext,
	astCh chan<- *parser.AstFile,
	appendSkip func(walk.WalkSkip),
) {
	defer func() {
		if r := recover(); r != nil {
			appendSkip(walk.WalkSkip{Path: wf.RepoRelPath, Reason: SkipReasonParserPanic})
			o.logger.Error("orchestrator: parser panic (per-file recover)",
				slog.String("path", wf.RepoRelPath),
				slog.String("language", wf.Language),
				slog.Any("recover", r),
			)
		}
	}()

	ast, err := o.parsers.Parse(ctx, wf.RepoRelPath, wf.Content)
	if err != nil {
		appendSkip(walk.WalkSkip{Path: wf.RepoRelPath, Reason: SkipReasonParserError})
		o.logger.Warn("orchestrator: parser error",
			slog.String("path", wf.RepoRelPath),
			slog.String("language", wf.Language),
			slog.String("error", err.Error()),
		)
		return
	}
	if ast == nil {
		appendSkip(walk.WalkSkip{Path: wf.RepoRelPath, Reason: SkipReasonParserError})
		o.logger.Warn("orchestrator: parser returned nil AstFile",
			slog.String("path", wf.RepoRelPath),
			slog.String("language", wf.Language),
		)
		return
	}

	if ast.Attrs == nil {
		ast.Attrs = map[string]string{}
	}
	if repoCtx.ModulePath != "" {
		if _, exists := ast.Attrs[recipes.AttrModulePath]; !exists {
			ast.Attrs[recipes.AttrModulePath] = repoCtx.ModulePath
		}
	}
	if _, exists := ast.Attrs[recipes.AttrSourceBytes]; !exists {
		ast.Attrs[recipes.AttrSourceBytes] = string(wf.Content)
	}

	select {
	case astCh <- ast:
	case <-ctx.Done():
	}
}

// mintBindings walks every [parser.AstScope] on an
// `*AstFile`, derives its canonical signature, mints a
// durable scope ID via [scopebinding.MintScopeID], and
// inserts the resulting [scopebinding.ScopeBinding] into the
// configured table. Block scopes are skipped: their
// canonical signature requires the enclosing method's
// signature plus the block's ordinal+kind, neither of which
// is reliably populated on the proto today, and the Stage
// 2.2 / 2.3 recipe fleet does not emit at `scope_kind=block`.
//
// Package scopes that share a `(repoID, qualifiedName)` (two
// `.go` files in the same package, for instance) mint the
// same canonical signature -- by construction, they
// represent the SAME logical scope. The orchestrator records
// the binding from the FIRST file in path-sorted order; a
// subsequent insert for the same `ScopeID` is silently
// idempotent in [scopebinding.Table] (last-write-wins per
// `TestTable_LastWriteWins`) but the `(path, local_id) ->
// ScopeID` record sticks to the first observation. Both
// observations resolve to the same `ScopeID`, so callers
// looking up the package's binding by ScopeID always find
// it.
func (o *Orchestrator) mintBindings(
	ast *parser.AstFile,
	repoCtx repocontext.RepoContext,
	result *Result,
	appendSkip func(walk.WalkSkip),
) {
	repoURL := SyntheticRepoURLPrefix + repoCtx.RepoID.String()
	relPath := ast.GetPath()
	language := ast.GetLanguage()

	for _, sc := range ast.GetScopes() {
		kind := protoScopeKindToScopeKind(sc.GetScopeKind())
		if kind == "" {
			// SCOPE_KIND_UNSPECIFIED is a producer bug;
			// log it once but do not surface a per-file
			// skip (the file itself is fine).
			o.logger.Warn("orchestrator: scope with SCOPE_KIND_UNSPECIFIED dropped",
				slog.String("path", relPath),
				slog.String("local_id", sc.GetScopeId()),
			)
			continue
		}
		if kind == scope.KindBlock {
			// Block signatures need the enclosing
			// method's canonical signature and a 0-based
			// ordinal -- neither is reliably wired on
			// today's proto. Stage 2.2 recipes do not
			// emit at `scope_kind=block`, so silently
			// drop the binding rather than emit a
			// degraded one.
			continue
		}

		canonSig, err := buildCanonicalSignature(repoURL, relPath, kind, sc)
		if err != nil {
			appendSkip(walk.WalkSkip{Path: relPath, Reason: SkipReasonScopeBindingError})
			o.logger.Warn("orchestrator: skip scope binding (signature build failed)",
				slog.String("path", relPath),
				slog.String("scope_kind", string(kind)),
				slog.String("local_id", sc.GetScopeId()),
				slog.String("error", err.Error()),
			)
			continue
		}

		scopeID, err := scopebinding.TryMintScopeID(repoCtx.RepoID, string(kind), canonSig, repoCtx.HeadSHA)
		if err != nil {
			appendSkip(walk.WalkSkip{Path: relPath, Reason: SkipReasonScopeBindingError})
			o.logger.Warn("orchestrator: skip scope binding (mint failed)",
				slog.String("path", relPath),
				slog.String("scope_kind", string(kind)),
				slog.String("error", err.Error()),
			)
			continue
		}

		var startLine, endLine int
		if rg := sc.GetRange(); rg != nil {
			startLine = int(rg.GetStartLine())
			endLine = int(rg.GetEndLine())
		}

		if err := o.scopeBindings.Insert(scopebinding.ScopeBinding{
			ScopeID:   scopeID,
			ScopeKind: string(kind),
			FilePath:  relPath,
			StartLine: startLine,
			EndLine:   endLine,
			Signature: canonSig,
			Language:  language,
		}); err != nil {
			// Insert can only fail on ErrZeroScopeID, which
			// TryMintScopeID already ruled out. Log and
			// move on rather than swallow silently.
			o.logger.Warn("orchestrator: scope binding insert failed",
				slog.String("path", relPath),
				slog.String("scope_kind", string(kind)),
				slog.String("error", err.Error()),
			)
			continue
		}

		key := ScopeBindingKey{Path: relPath, LocalID: sc.GetScopeId()}
		if _, exists := result.ScopeIDs[key]; !exists {
			result.ScopeIDs[key] = scopeID
		}
	}
}

// protoScopeKindToScopeKind translates the parser-side proto
// enum [parser.ScopeKind] into the canonical [scope.Kind]
// string. Returns the empty Kind for `SCOPE_KIND_UNSPECIFIED`
// so callers can drop the scope without panicking.
func protoScopeKindToScopeKind(k parser.ScopeKind) scope.Kind {
	switch k {
	case parser.ScopeKindRepo:
		return scope.KindRepo
	case parser.ScopeKindPackage:
		return scope.KindPackage
	case parser.ScopeKindFile:
		return scope.KindFile
	case parser.ScopeKindClass:
		return scope.KindClass
	case parser.ScopeKindInterface:
		return scope.KindInterface
	case parser.ScopeKindMethod:
		return scope.KindMethod
	case parser.ScopeKindBlock:
		return scope.KindBlock
	default:
		return ""
	}
}

// buildCanonicalSignature dispatches to the per-kind
// [scope.BuildRepo] / [scope.BuildPackage] / etc. seam,
// passing the parser-side `qualifiedName` (falling back to
// `name` when `qualifiedName` is empty) and `parameters`.
//
// For `scope.KindPackage`, the `dir` argument is derived
// from the file's repo-relative path via [path.Dir]; for a
// top-level file (`baz.go`) the directory is `"."` which
// [scope.BuildPackage] accepts as a non-empty input (the
// `BuildPackage` guard only rejects empty strings).
func buildCanonicalSignature(repoURL, relPath string, kind scope.Kind, sc *parser.AstScope) (string, error) {
	qname := sc.GetQualifiedName()
	if qname == "" {
		qname = sc.GetName()
	}
	switch kind {
	case scope.KindRepo:
		return scope.BuildRepo(repoURL)
	case scope.KindPackage:
		dir := path.Dir(relPath)
		if dir == "" {
			dir = "."
		}
		return scope.BuildPackage(repoURL, dir)
	case scope.KindFile:
		return scope.BuildFile(repoURL, relPath)
	case scope.KindClass:
		return scope.BuildClass(repoURL, relPath, qname)
	case scope.KindInterface:
		return scope.BuildInterface(repoURL, relPath, qname)
	case scope.KindMethod:
		return scope.BuildMethod(repoURL, relPath, qname, sc.GetParameters())
	default:
		return "", fmt.Errorf("orchestrator: unsupported scope kind %q for canonical signature build", string(kind))
	}
}

// sortDrafts orders the merged per-file + project draft
// slice deterministically. The four-tuple
// `(MetricKind, Scope.Path, Scope.Kind, Scope.LocalID)` is a
// total order across the recipes Stage 2.2 dispatches: every
// per-file recipe emits at most one draft per
// `(MetricKind, Scope.LocalID)` within a single AstFile, so
// the `(MetricKind, Path, Kind, LocalID)` prefix breaks ties.
// MetricVersion is appended as a defence-in-depth tie-breaker
// in case a future recipe emits multiple versions for the
// same scope (a Sec 8.6 "definitional change" rollout).
//
// `sort.SliceStable` preserves the per-recipe emission order
// for any ties the four-tuple does not break, so a recipe
// that emits multiple drafts at the same scope (today: none;
// reserved for project-level recipes that emit per-package
// rows) keeps its declared order.
func sortDrafts(drafts []recipes.MetricSampleDraft) {
	sort.SliceStable(drafts, func(i, j int) bool {
		a, b := drafts[i], drafts[j]
		if a.MetricKind != b.MetricKind {
			return a.MetricKind < b.MetricKind
		}
		if a.Scope.Path != b.Scope.Path {
			return a.Scope.Path < b.Scope.Path
		}
		if a.Scope.Kind != b.Scope.Kind {
			return string(a.Scope.Kind) < string(b.Scope.Kind)
		}
		if a.Scope.LocalID != b.Scope.LocalID {
			return a.Scope.LocalID < b.Scope.LocalID
		}
		return a.MetricVersion < b.MetricVersion
	})
}
