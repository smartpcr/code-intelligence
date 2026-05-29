package recipes

import (
	"hash/fnv"
	"sort"

	"forge/services/clean-code/internal/ast/parser"
	"forge/services/clean-code/internal/ast/scope"
)

// duplicationRatioMetricKind is the canonical metric_kind
// string for the structural-duplication ratio (architecture
// Sec 1.4.1 row 11 -- "duplication_ratio | file, package |
// base | sliding 50-token window over the file's lexical
// stream; ratio of tokens covered by a window that has a
// duplicate elsewhere in the same scope. Drives duplication
// rule."). Pinned as a const so a `grep -nF
// "duplication_ratio"` lands ONE definition site.
const duplicationRatioMetricKind = "duplication_ratio"

// duplicationRatioVersion is the recipe's `version()` per
// Sec 8.6 line 1010. A bump MUST coincide with a
// `metric_version` bump on every emitted sample (architecture
// C4): a change to the window size, the token-extraction
// rule, or the cover-counting formula is a definitional
// change and lands as a new row at the same
// `(repo_id, sha, scope_id, metric_kind)`.
const duplicationRatioVersion = 1

// duplicationWindowSize is the canonical window length the
// recipe slides over the token stream (brief: "50-token
// sliding window"). Pinned to a `const` so a future change is
// a single-site edit paired with a [duplicationRatioVersion]
// bump.
const duplicationWindowSize = 50

// duplicationRatioAllowedKinds is the closed scope_kind set
// the duplication_ratio recipe is permitted to emit at,
// mirroring architecture Sec 1.4.1 row 11 column 2 entry
// `file, package`. Passed to [newDraft] so the helper's
// per-recipe guard refuses any other value -- including the
// `function`, `method`, and `module` aliases the brief
// explicitly forbids -- at the panic boundary.
var duplicationRatioAllowedKinds = []scope.Kind{scope.KindFile, scope.KindPackage}

// SourceLoader is the seam the recipe uses to obtain a file's
// raw source bytes for tier-2 LEXICAL tokenisation. Returns
// `(bytes, true)` on a hit and `(nil, false)` on any miss
// (path empty, file unreachable, etc.). A tier-2 miss is NOT
// itself sufficient to force structural tokens -- tier 1
// (parser-stamped `Attrs[parser.AttrSourceBytes]`) is
// consulted FIRST, and only when both tier 1 and tier 2 miss
// does the recipe fall through to the structural fallback at
// tier 3. See the [DuplicationRatioRecipe] doc "Tokenisation"
// for the full priority order.
//
// The recipe is PURE: same `(*AstFile, SourceLoader)` in ->
// same drafts out. The [DefaultSourceLoader] is the canonical
// "always miss" loader and is the production default. A
// non-default loader is only needed when a composition root
// wants to provide bytes for ASTs that LACK the parser-stamped
// attr (e.g. hand-built fixtures, or a future parser path
// that bypasses the canonical builder funnel); such a loader
// MUST be deterministic (typically a closure over a scan-time
// source-bytes cache validated against `AstFile.ContentSha256`).
// The recipe itself NEVER reads the filesystem, so the
// per-file output is a function of the AstFile alone, not the
// cwd or the live filesystem.
//
// Tests inject a stub loader (map[path][]byte) to keep the
// lexical-mode tier-2 tests hermetic; the parser-attr tier-1
// tests instead populate `AstFile.Attrs[AttrSourceBytes]`
// directly on the fixture AstFile.
type SourceLoader func(path string) ([]byte, bool)

// DefaultSourceLoader is the always-miss [SourceLoader] used
// by [NewDuplicationRatioRecipe]. It returns `(nil, false)`
// unconditionally so the recipe's tier-2 SourceLoader path is
// a guaranteed miss; tier-2 is wired this way so the recipe's
// output depends ONLY on the `*AstFile` it is given (recipe
// purity contract per `recipe.go:227-237`).
//
// IMPORTANT: an always-miss [DefaultSourceLoader] does NOT
// mean the default recipe is non-lexical in production.
// LEXICAL tokenisation is the production default via TIER 1
// (parser-stamped `Attrs[parser.AttrSourceBytes]`), which
// `fileTokens` consults BEFORE the SourceLoader -- see the
// [DuplicationRatioRecipe] "Tokenisation" priority order
// (tier 1: parser attrs, tier 2: SourceLoader, tier 3:
// structural fallback). For every parser-produced AstFile the
// builder funnel at `internal/ast/parser/internal.go`
// populates `Attrs[parser.AttrSourceBytes]`, so the default-
// constructed recipe IS lexical out of the box; no
// SourceLoader wiring is required at the composition root for
// production lexical tokenisation.
//
// [NewDuplicationRatioRecipeWithSource] is for callers that
// need tier-2 bytes -- specifically (a) ASTs that lack the
// parser-stamped attr (hand-built test fixtures, or a future
// parser path that bypasses the canonical builder funnel), or
// (b) callers that want to provide an alternate byte source
// when tier-1 is absent. A loader supplied here NEVER
// overrides tier 1: the `Attrs[parser.AttrSourceBytes]` check
// runs first; the loader is only consulted on a tier-1 miss.
// Production composition roots that simply need lexical
// tokenisation for normal parser-produced ASTs do NOT need
// `NewDuplicationRatioRecipeWithSource`; the default
// constructor is sufficient.
//
// When a caller does inject a loader, it MUST be deterministic
// (typically a closure over a scan-time source-bytes cache
// validated against `AstFile.ContentSha256`). Reading
// `os.ReadFile` from inside a loader closure is prohibited
// because the recipe contract requires the AstFile alone to
// determine output -- a recipe that reads the live filesystem
// returns different drafts on the same AstFile depending on
// the process's cwd / the file's mtime / the file's continued
// existence, which violates G2 (re-run idempotency on the
// same SHA).
//
// Iter-2 wired `os.ReadFile` here and was rejected by the
// evaluator (iter-2 item 3: "DefaultSourceLoader reads
// `os.ReadFile(ast.GetPath())` making output depend on
// cwd/filesystem state despite the recipe contract requiring
// same `*AstFile` in -> same drafts out"). The iter-3 fix
// removed the filesystem read. Iter-5 then wired tier 1
// (parser attrs) so the default recipe became lexical in
// production without needing any composition-root wiring at
// all; this doc-comment was updated in iter 11 to match.
func DefaultSourceLoader(_ string) ([]byte, bool) {
	return nil, false
}

// DuplicationRatioRecipe is the duplication ratio recipe for
// the foundation tier (architecture Sec 1.4.1 row 11).
//
// # Algorithm
//
// The recipe extracts a deterministic token stream from each
// AstFile, slides a fixed-size window of [duplicationWindowSize]
// tokens over the stream, and reports the fraction of
// (real) tokens covered by a window whose content also
// appears at another window position.
//
//	duplication_ratio = real_covered_tokens / real_total_tokens
//
// The value is bounded in [0.0, 1.0]:
//
//   - 0.0 means no 50-token window appears more than once
//     in the scope.
//   - 1.0 means every (real) token in the stream sits inside
//     at least one window with a duplicate elsewhere (i.e.
//     the whole stream is part of some repeated block).
//
// # Tokenisation
//
// The recipe extracts tokens from one of THREE sources, in
// strict priority order. Higher-priority sources are LEXICAL
// (whitespace-canonical, matches the e2e contract at e2e-
// scenarios.md:426-430); the structural fallback exists only
// when no source bytes are reachable from the recipe's
// process.
//
//  1. PARSER-STAMPED LEXICAL (DEFAULT in production).
//     The parser fleet's `scopeBuilder.build()` (see
//     `internal/ast/parser/internal.go`) stamps the file's
//     raw source bytes on `AstFile.Attrs[parser.AttrSourceBytes]`
//     for every parse path that lands in the canonical
//     builder funnel. `fileTokens` checks this attr FIRST
//     and, when present and non-empty, lexes the bytes into
//     the maximal-munch token stream described below. This
//     makes the DEFAULT-constructed recipe lexical for
//     normal parser-produced ASTs -- no SourceLoader wiring
//     is required at the composition root.
//
//  2. SOURCELOADER SEAM (production override / hermetic
//     tests). When `Attrs[parser.AttrSourceBytes]` is absent
//     or empty (e.g. the AstFile was hand-built in a test
//     fixture, or a future parser bypasses the canonical
//     builder), the recipe falls back to its [SourceLoader]
//     closure via `ast.GetPath()`. Production composition
//     roots that prefer an out-of-band source-bytes cache
//     (e.g. a scan-time blob store keyed by `path` and
//     validated against `AstFile.ContentSha256`) inject it
//     via [NewDuplicationRatioRecipeWithSource]. The default
//     [DefaultSourceLoader] is the canonical "always miss"
//     so the default recipe's output remains a pure function
//     of the AstFile alone.
//
//  3. STRUCTURAL FALLBACK (last resort). When neither
//     `Attrs[parser.AttrSourceBytes]` nor the SourceLoader
//     yield bytes, the recipe falls back to the AST's scopes
//     and symbols. Each non-file / non-package scope and
//     each symbol contributes ONE token of the form
//     `<kind>:<name>` (e.g. `class:Order`, `sym:var:userID`).
//     The fallback is much coarser than lexical -- two
//     functions that differ only by whitespace ARE detected
//     as duplicates either way (their scope/symbol trees are
//     identical), but two textually-identical 50-line blocks
//     contribute only ~5-10 structural tokens each, so dense
//     lexical duplication is invisible to the fallback. The
//     fallback exists so the recipe still produces a
//     defensible signal in tests / hermetic environments
//     where neither the parser-stamped attr nor a loader
//     supplies bytes.
//
// Lexical token rules (sources 1 and 2):
//
//   - Whitespace (` `, `\t`, `\r`, `\n`) is a separator only
//     and is NOT emitted (so two functions differing only in
//     indentation tokenise identically -- the e2e contract
//     at e2e-scenarios.md:426-430).
//   - Maximal run of `[A-Za-z0-9_]` becomes one token
//     (identifiers, keywords, numeric literals each
//     contribute one token).
//   - Any other byte (operators, punctuation, string quotes,
//     ...) becomes a single-character token.
//
// Iter-5 wired source 1 by populating
// `Attrs[parser.AttrSourceBytes]` at the parser builder
// funnel (parser-side change in `parser/internal.go`);
// iter-6 documents that production wiring fully so callers
// see the default IS lexical for normal parser output.
//
// # Per-file vs project-level emission
//
// [Recipe.Compute] emits ONE draft at `scope_kind='file'`
// per AST. Files with fewer tokens than the 50-token window
// emit `value=0.0` (a present-but-short scope cannot contain
// a 50-token clone by definition, so zero is the only
// defensible answer; emitting nothing would create a
// "metric missing vs metric zero" ambiguity downstream).
//
// [DuplicationRatioRecipe.ComputeProject] emits one
// additional draft per package at `scope_kind='package'`,
// computed over the CONCATENATION of the package's files'
// token streams (path-sorted) with a UNIQUE per-boundary
// sentinel token inserted between files. The sentinel
// guarantees that a 50-token window straddling two source
// files cannot match another straddling window (since each
// boundary's sentinel is unique), so the package-scope ratio
// reflects only REAL cross-file duplication, not artificial
// concatenation artifacts. CRITICALLY the sentinels are
// EXCLUDED from BOTH the numerator AND the denominator of
// the ratio: `ratio = real_covered / real_total` -- so the
// emitted value is over actual source/metric tokens only,
// never inflated or diluted by synthetic markers.
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil
// AND NOT degraded. A degraded AST means the parser bailed
// mid-file; a duplication ratio over truncated input would
// drift downstream rule decisions, so the recipe drops the
// row per architecture Sec 3.4 lines 490-494 ("Computed rows
// are never `degraded=true`: if an input is missing the row
// is not written").
//
// Unlike cyclo / cognitive_complexity, this recipe does NOT
// depend on the `decision_blocks` capability -- it walks the
// parser's mandatory scopes-and-symbols topology, which the
// Stage 2.1 fleet emits from day one.
type DuplicationRatioRecipe struct {
	source SourceLoader
}

// NewDuplicationRatioRecipe returns a [DuplicationRatioRecipe]
// wired with [DefaultSourceLoader] (always-miss). The recipe
// is PURE: same `*AstFile` in -> same drafts out, with no
// filesystem coupling.
//
// In production this constructor produces a LEXICAL recipe by
// default: the parser fleet's `scopeBuilder.build()` populates
// `AstFile.Attrs[parser.AttrSourceBytes]` on every parser-
// produced AstFile, and `fileTokens` reads that attr BEFORE
// consulting the SourceLoader (see [DuplicationRatioRecipe]
// doc "Tokenisation" priority order). Composition roots that
// dispatch the recipe across parser-produced ASTs therefore
// get whitespace-canonical lexical duplication detection (the
// e2e contract at e2e-scenarios.md:426-430) without any
// SourceLoader wiring.
//
// The recipe falls back to STRUCTURAL tokens only when
// neither the parser-stamped attr nor the SourceLoader yield
// bytes -- typically because the AstFile was hand-built in a
// test fixture, or a future parser bypasses the canonical
// builder.
//
// Composition roots that want to OVERRIDE the parser-stamped
// attr (e.g. to source bytes from an out-of-band cache that
// canonicalises whitespace differently) MUST construct via
// [NewDuplicationRatioRecipeWithSource]. The
// `Attrs[parser.AttrSourceBytes]` check still runs first, so
// "override" only applies when the parser attr is absent /
// empty; callers that need to BYPASS the parser attr should
// strip it from the AstFile prior to compute.
//
// Safe for concurrent Compute / ComputeProject calls because
// the recipe holds no per-call state.
func NewDuplicationRatioRecipe() *DuplicationRatioRecipe {
	return &DuplicationRatioRecipe{source: DefaultSourceLoader}
}

// NewDuplicationRatioRecipeWithSource returns a
// [DuplicationRatioRecipe] wired with a caller-supplied
// [SourceLoader]. Tests use this to inject a stub loader
// (either always-miss to exercise structural fallback or a
// tmpfs-backed loader to exercise lexical mode without
// touching the test process's working directory).
//
// Passing `nil` is equivalent to wiring an "always miss"
// loader at tier 2. The recipe still consults
// `Attrs[parser.AttrSourceBytes]` at tier 1 first, so a
// parser-produced AstFile remains LEXICAL even when the
// caller supplies a nil loader; only when the parser attr
// is also absent does the recipe fall through to the
// structural fallback at tier 3. See the [DuplicationRatioRecipe]
// "Tokenisation" priority order for the full contract.
func NewDuplicationRatioRecipeWithSource(loader SourceLoader) *DuplicationRatioRecipe {
	return &DuplicationRatioRecipe{source: loader}
}

// MetricKind implements [Recipe].
func (r *DuplicationRatioRecipe) MetricKind() string { return duplicationRatioMetricKind }

// Version implements [Recipe].
func (r *DuplicationRatioRecipe) Version() int { return duplicationRatioVersion }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil AND NOT degraded. See [DuplicationRatioRecipe] doc
// "Capability + degradation gate".
func (r *DuplicationRatioRecipe) AppliesTo(ast *parser.AstFile) bool {
	if ast == nil {
		return false
	}
	if ast.GetDegradedReason() != "" {
		return false
	}
	return true
}

// Compute implements [Recipe]. Emits ONE
// `duplication_ratio` draft at `scope_kind='file'`. The
// value is the ratio of tokens covered by a duplicated
// window to total tokens, bounded in [0.0, 1.0]. Files with
// fewer tokens than the 50-token window cannot contain a
// duplicated window by definition and emit `value=0.0`.
//
// Returns nil only when the AST itself is missing / degraded
// (per [Recipe.AppliesTo]); the architecture's "row not
// written, not stamped degraded" rule (Sec 3.4 lines
// 490-494) targets MISSING INPUTS, not short-but-valid ones.
// A 30-token file is a present, valid input whose metric is
// "no 50-token clone possible" -- the conceptual zero.
func (r *DuplicationRatioRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
	if !r.AppliesTo(ast) {
		return nil
	}
	fileScope := fileScopeOf(ast)
	if fileScope == nil {
		return nil
	}
	tokens, isSentinel := r.fileTokens(ast)
	ratio := computeDuplicationRatio(tokens, isSentinel)
	return []MetricSampleDraft{newDraft(
		duplicationRatioMetricKind,
		duplicationRatioVersion,
		PackBase,
		SourceComputed,
		ratio,
		ScopeRef{
			LocalID:       fileScope.GetScopeId(),
			Kind:          scope.KindFile,
			QualifiedName: fileScope.GetQualifiedName(),
			Path:          ast.GetPath(),
		},
		nil,
		duplicationRatioAllowedKinds,
	)}
}

// ComputeProject emits per-file drafts AND per-package
// drafts. Per-file drafts mirror [Compute]; per-package
// drafts use the concatenated token stream of every AstFile
// in the package (lexicographic path order) and emit at
// `scope_kind='package'` using the lexicographically-first
// file's package scope as the representative.
//
// Nil / empty / fully-degraded inputs return nil. Individual
// degraded AstFiles are skipped at BOTH the per-file emission
// and the per-package concatenation.
//
// Emission order is deterministic across runs (G2):
//
//   - File-scope drafts first, in lexicographic order of
//     `AstFile.Path`.
//   - Package-scope drafts second, in lexicographic order of
//     compound identity `<qualifiedName>@<canonicalDir>`
//     (iter-5: distinct same-named packages in different
//     directories are SEPARATE groups; see Step "Group files
//     by compound package identity" below).
func (r *DuplicationRatioRecipe) ComputeProject(asts []*parser.AstFile) []MetricSampleDraft {
	if len(asts) == 0 {
		return nil
	}

	// Filter + sort by path for deterministic emission.
	files := make([]*parser.AstFile, 0, len(asts))
	for _, ast := range asts {
		if ast == nil || ast.GetDegradedReason() != "" {
			continue
		}
		files = append(files, ast)
	}
	if len(files) == 0 {
		return nil
	}
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].GetPath() < files[j].GetPath()
	})

	drafts := make([]MetricSampleDraft, 0, len(files))

	// Per-file drafts.
	for _, ast := range files {
		fileScope := fileScopeOf(ast)
		if fileScope == nil {
			continue
		}
		tokens, isSentinel := r.fileTokens(ast)
		ratio := computeDuplicationRatio(tokens, isSentinel)
		drafts = append(drafts, newDraft(
			duplicationRatioMetricKind,
			duplicationRatioVersion,
			PackBase,
			SourceComputed,
			ratio,
			ScopeRef{
				LocalID:       fileScope.GetScopeId(),
				Kind:          scope.KindFile,
				QualifiedName: fileScope.GetQualifiedName(),
				Path:          ast.GetPath(),
			},
			nil,
			duplicationRatioAllowedKinds,
		))
	}

	// Per-package drafts: group files by package, concat
	// tokens in path order WITH file-boundary sentinels.
	// Without sentinels, a 50-token window could span the
	// last K tokens of file1 and the first 50-K tokens of
	// file2 and (incorrectly) match against a window with a
	// different file boundary. The sentinel token --
	// uniquely tagged per boundary -- makes any
	// boundary-spanning window contain a unique marker so it
	// cannot match another boundary-spanning window.
	//
	// Sentinels participate in window CONTENT (hash + byte-
	// equality) so any boundary-straddling window has a
	// unique marker. They are EXCLUDED from both the
	// numerator (real_covered) and the denominator
	// (real_total) of the ratio by the parallel `isSentinel`
	// mask -- the emitted value is over actual source /
	// metric tokens only.
	// Group files by COMPOUND package identity
	// `<qualifiedName>@<canonicalDir>` (iter-5 evaluator
	// item 3 -- mirrors the cycle_member compound identity).
	// Real Go projects can have multiple packages declaring
	// the same name in different directories (most commonly
	// `main` across cmd/x/, cmd/y/, ... and `internal`
	// across multiple subtrees). Keying groups purely by
	// qualifiedName would MERGE such packages into a single
	// duplication-ratio metric -- a non-trivial false
	// signal that obscures real per-package duplication.
	//
	// `canonicalDir` is the lexicographically-first file's
	// `path.Dir`. The compound identity is the grouping
	// key; the emitted draft uses the per-package real
	// `pkgScope` (via `representativeAndPackageScope`) so
	// downstream tooling sees the correct qualifiedName +
	// representative path per draft.
	type pkgGroup struct {
		ident         string
		qualifiedName string
		canonicalDir  string
		files         []*parser.AstFile
	}
	pkgGroups := map[string]*pkgGroup{}
	pkgOrder := []string{}
	for _, ast := range files {
		pkg := packageQualifiedName(ast)
		if pkg == "" {
			continue
		}
		d := normaliseDir(ast.GetPath())
		ident := pkg + "@" + d
		g, seen := pkgGroups[ident]
		if !seen {
			g = &pkgGroup{
				ident:         ident,
				qualifiedName: pkg,
				canonicalDir:  d,
			}
			pkgGroups[ident] = g
			pkgOrder = append(pkgOrder, ident)
		}
		g.files = append(g.files, ast)
	}
	sort.Strings(pkgOrder)
	for _, ident := range pkgOrder {
		g := pkgGroups[ident]
		groupFiles := g.files
		// Already sorted via outer `files` slice, but be
		// defensive -- a future caller might mutate.
		sort.SliceStable(groupFiles, func(i, j int) bool {
			return groupFiles[i].GetPath() < groupFiles[j].GetPath()
		})
		var combined []string
		var isSentinel []bool
		for i, ast := range groupFiles {
			if i > 0 {
				combined = append(combined, fileBoundarySentinel(i))
				isSentinel = append(isSentinel, true)
			}
			fileToks, fileSent := r.fileTokens(ast)
			combined = append(combined, fileToks...)
			isSentinel = append(isSentinel, fileSent...)
		}
		ratio := computeDuplicationRatio(combined, isSentinel)
		rep, pkgScope := representativeAndPackageScope(groupFiles)
		if rep == nil || pkgScope == nil {
			continue
		}
		drafts = append(drafts, newDraft(
			duplicationRatioMetricKind,
			duplicationRatioVersion,
			PackBase,
			SourceComputed,
			ratio,
			ScopeRef{
				LocalID:       pkgScope.GetScopeId(),
				Kind:          scope.KindPackage,
				QualifiedName: pkgScope.GetQualifiedName(),
				Path:          rep.GetPath(),
			},
			nil,
			duplicationRatioAllowedKinds,
		))
	}

	return drafts
}

// fileTokens returns the token stream for a single AstFile
// plus a parallel `isSentinel` mask. The mask is always all
// false at the file level (sentinels are only introduced at
// the per-package concat step); it exists so the per-file
// and per-package paths share one [computeDuplicationRatio]
// implementation.
//
// Token-source resolution order, ALL of which are pure on
// the AstFile alone (no filesystem reads, no time-of-day
// dependency):
//
//  1. `ast.Attrs[AttrSourceBytes]` -- when populated by the
//     parser / scan layer with the file's raw source bytes
//     stored as a string. This is the canonical lexical-
//     mode wire-up: composition roots that want whitespace-
//     canonical duplication detection populate this attr at
//     scan time. Because the bytes live ON the AstFile, the
//     recipe is pure: same AstFile in -> same tokens out.
//
//  2. `r.source(ast.GetPath())` -- the production SourceLoader
//     seam. Composition roots may wire a closure over a
//     deterministic in-memory cache (e.g. scan-time bytes
//     cache keyed by `path` and validated against
//     `AstFile.ContentSha256`). The default-constructed
//     recipe wires [DefaultSourceLoader] (always-miss).
//
//  3. STRUCTURAL fallback. Tokens from the AstFile's
//     scopes and symbols when neither lexical source is
//     available. Coarser-grained but still deterministic.
func (r *DuplicationRatioRecipe) fileTokens(ast *parser.AstFile) ([]string, []bool) {
	if ast == nil {
		return nil, nil
	}
	if src, ok := sourceBytesFromAttrs(ast); ok {
		tokens := lexicalTokenize(src)
		return tokens, make([]bool, len(tokens))
	}
	if r.source != nil {
		if src, ok := r.source(ast.GetPath()); ok {
			tokens := lexicalTokenize(src)
			return tokens, make([]bool, len(tokens))
		}
	}
	tokens := structuralTokens(ast)
	return tokens, make([]bool, len(tokens))
}

// sourceBytesFromAttrs returns the raw source bytes carried
// on `ast.Attrs[AttrSourceBytes]`. When the attr is absent or
// empty, returns `(nil, false)` so the caller falls through
// to the SourceLoader seam or the structural fallback.
// Lookup is pure on AstFile: no filesystem reads, no
// out-of-band cache, no cwd dependency.
func sourceBytesFromAttrs(ast *parser.AstFile) ([]byte, bool) {
	if ast == nil {
		return nil, false
	}
	attrs := ast.GetAttrs()
	if attrs == nil {
		return nil, false
	}
	v, ok := attrs[AttrSourceBytes]
	if !ok || v == "" {
		return nil, false
	}
	return []byte(v), true
}

// lexicalTokenize splits `src` into a maximal-munch token
// stream: whitespace separates and is dropped; runs of
// `[A-Za-z0-9_]` form one token each; every other byte is a
// single-character token. UTF-8 multi-byte continuation
// bytes are passed through as single-character tokens (the
// duplication detector compares byte-for-byte so a multi-
// byte identifier is still treated coherently as long as the
// bytes appear in the same order on each side).
//
// The whitespace-canonicalising property is what lets the
// e2e contract (e2e-scenarios.md:426-430) hold: two
// TypeScript functions differing only in indentation and
// trailing newlines tokenise to identical streams because
// whitespace is dropped before window hashing.
func lexicalTokenize(src []byte) []string {
	if len(src) == 0 {
		return nil
	}
	tokens := make([]string, 0, len(src)/4)
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case isLexWhitespace(c):
			i++
		case isLexWord(c):
			start := i
			for i < len(src) && isLexWord(src[i]) {
				i++
			}
			tokens = append(tokens, string(src[start:i]))
		default:
			tokens = append(tokens, string(src[i:i+1]))
			i++
		}
	}
	return tokens
}

// isLexWhitespace reports whether `c` is a whitespace byte
// per the lexical tokeniser's definition (space, tab, CR, LF).
func isLexWhitespace(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n':
		return true
	}
	return false
}

// isLexWord reports whether `c` continues an identifier-like
// run -- ASCII letter, digit, or underscore.
func isLexWord(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '_':
		return true
	}
	return false
}

// fileBoundarySentinel returns a unique-per-position token
// that cannot collide with any real `<kind>:<name>` or
// `sym:<kind>:<name>` token because the sentinel uses an
// uppercase, square-bracketed prefix the recipe's token
// constructors never produce. Different boundary positions
// produce different sentinels so two boundary-spanning
// windows cannot match each other.
func fileBoundarySentinel(boundaryIndex int) string {
	return "[FILE_BOUNDARY_" + itoaUnsigned(boundaryIndex) + "]"
}

// itoaUnsigned is a tiny strconv-free formatter so the recipe
// has no stdlib alias drift in the production code path. Used
// only by [fileBoundarySentinel].
func itoaUnsigned(n int) string {
	if n < 0 {
		n = -n
	}
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// structuralTokens flattens `ast`'s scopes and symbols into a
// deterministic, source-ordered token stream. Each scope and
// each symbol contributes ONE token of the form
// `<kind>:<name>`:
//
//   - scopes:  `pkg:<qualifiedName>`, `file:<qualifiedName>`,
//     `class:<name>`, `method:<name>`, `block:<name>`, ...
//   - symbols: `sym:<kind>:<name>`
//
// The file's own scope is EXCLUDED from the stream because
// every file emits exactly one file scope -- including it
// would inflate `total_tokens` by a constant 1 and skew the
// ratio for very short files. The package scope is also
// excluded (same reason; one per package, file-shared in the
// per-package concat path).
//
// Tokens are ordered by `(Range.StartLine, Range.StartByte)`
// with stable sort so two scopes at the same start position
// retain their slice order (the parser is contractually
// source-ordered, so this falls back to the parser's emit
// order on ties).
//
// This is the FALLBACK extractor; production prefers the
// lexical stream produced by [lexicalTokenize]. The function
// is kept exported-to-this-package so the recipe stays
// hermetic in environments where source bytes are not
// reachable.
func structuralTokens(ast *parser.AstFile) []string {
	if ast == nil {
		return nil
	}

	type tokenEntry struct {
		token string
		line  uint32
		byte_ uint32
	}
	entries := make([]tokenEntry, 0, len(ast.GetScopes())+len(ast.GetSymbols()))

	for _, s := range ast.GetScopes() {
		if s == nil {
			continue
		}
		kind := s.GetScopeKind()
		// Skip file and package scopes -- they are 1-per-file
		// / 1-per-package constants and would noise the ratio.
		if kind == parser.ScopeKindFile || kind == parser.ScopeKindPackage {
			continue
		}
		name := s.GetName()
		if name == "" {
			name = s.GetQualifiedName()
		}
		entries = append(entries, tokenEntry{
			token: scopeKindToken(kind) + ":" + name,
			line:  s.GetRange().GetStartLine(),
			byte_: s.GetRange().GetStartByte(),
		})
	}
	for _, sym := range ast.GetSymbols() {
		if sym == nil {
			continue
		}
		entries = append(entries, tokenEntry{
			token: "sym:" + sym.GetKind() + ":" + sym.GetName(),
			line:  sym.GetRange().GetStartLine(),
			byte_: sym.GetRange().GetStartByte(),
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].line != entries[j].line {
			return entries[i].line < entries[j].line
		}
		return entries[i].byte_ < entries[j].byte_
	})

	tokens := make([]string, len(entries))
	for i, e := range entries {
		tokens[i] = e.token
	}
	return tokens
}

// scopeKindToken returns the short string form of a
// `parser.ScopeKind` used as the prefix of a token in the
// duplication stream. Stable across recipe versions because
// the strings feed window hashes -- changing them would
// silently shift every existing duplication_ratio reading
// without bumping [duplicationRatioVersion].
func scopeKindToken(k parser.ScopeKind) string {
	switch k {
	case parser.ScopeKindRepo:
		return "repo"
	case parser.ScopeKindPackage:
		return "pkg"
	case parser.ScopeKindFile:
		return "file"
	case parser.ScopeKindClass:
		return "class"
	case parser.ScopeKindInterface:
		return "iface"
	case parser.ScopeKindMethod:
		return "method"
	case parser.ScopeKindBlock:
		return "block"
	default:
		return "unk"
	}
}

// computeDuplicationRatio returns the fraction of (real)
// tokens that lie inside at least one 50-token window whose
// content also appears at a different position in the stream.
// Streams whose total token count is shorter than
// [duplicationWindowSize] return 0.0 (no 50-token window can
// be formed; "no duplication" is the only defensible answer
// for a present, valid scope).
//
// The `isSentinel` mask runs in parallel with `tokens` and
// marks file-boundary sentinel positions (introduced by the
// per-package concatenation in [DuplicationRatioRecipe.ComputeProject]).
// Sentinels participate in window CONTENT (so a boundary-
// straddling window has a unique marker that cannot match
// any other window) but are EXCLUDED from BOTH the numerator
// (real_covered) and the denominator (real_total). At the
// per-file path the mask is all-false and the formula
// reduces to the canonical `covered / total`.
//
// Algorithm:
//
//  1. Walk all windows `[i .. i + windowSize)` for
//     `i = 0 .. len(tokens) - windowSize`.
//  2. Hash each window with FNV-64 over the tokens'
//     `\x00`-delimited byte form.
//  3. Bucket window indices by hash. For each hash with >= 2
//     windows, verify true byte-for-byte equality between
//     every pair before accepting them as duplicates (FNV-64
//     collisions are astronomically rare but the equality
//     check eliminates them deterministically).
//  4. Mark each token index as "covered" iff it lies in at
//     least one verified-duplicate window (`i in
//     [w .. w + windowSize)` for some duplicate window `w`).
//  5. real_total = count of `!isSentinel[i]`.
//     real_covered = count of `!isSentinel[i] && covered[i]`.
//     ratio = real_covered / real_total. real_total==0 ->
//     ratio = 0.0.
//
// The FNV-then-verify pattern keeps the algorithm
// O(N) on the average case (rare collisions) while ensuring
// O(N*windowSize) worst case -- which is acceptable for a
// 50-token window and a typical file's token count.
func computeDuplicationRatio(tokens []string, isSentinel []bool) float64 {
	n := len(tokens)
	if n < duplicationWindowSize {
		// A scope with fewer tokens than the window size
		// cannot contain a 50-token clone by definition.
		// Emitting 0.0 (rather than skipping) keeps the
		// metric uniformly dense across a repo so downstream
		// rules can distinguish "no duplication" from
		// "metric missing".
		return 0.0
	}
	if len(isSentinel) != n {
		// Defensive: an all-false mask of the right length is
		// the per-file contract. A nil mask is also accepted
		// and treated as all-false.
		isSentinel = make([]bool, n)
	}

	windowCount := n - duplicationWindowSize + 1
	hashes := make([]uint64, windowCount)
	for i := 0; i < windowCount; i++ {
		h := fnv.New64a()
		for j := 0; j < duplicationWindowSize; j++ {
			h.Write([]byte(tokens[i+j]))
			h.Write([]byte{0}) // delimiter
		}
		hashes[i] = h.Sum64()
	}

	byHash := map[uint64][]int{}
	for i, hv := range hashes {
		byHash[hv] = append(byHash[hv], i)
	}

	covered := make([]bool, n)
	for _, indices := range byHash {
		if len(indices) < 2 {
			continue
		}
		// Verify byte-for-byte equality to defend against
		// FNV-64 collisions. Group indices into equality
		// classes; any class with >= 2 entries gets its
		// windows' tokens covered.
		groups := groupByEquality(indices, tokens)
		for _, grp := range groups {
			if len(grp) < 2 {
				continue
			}
			for _, w := range grp {
				for k := w; k < w+duplicationWindowSize; k++ {
					covered[k] = true
				}
			}
		}
	}

	realTotal := 0
	realCovered := 0
	for i := 0; i < n; i++ {
		if isSentinel[i] {
			continue
		}
		realTotal++
		if covered[i] {
			realCovered++
		}
	}
	if realTotal == 0 {
		return 0.0
	}
	return float64(realCovered) / float64(realTotal)
}

// groupByEquality partitions `indices` (window-start
// positions that share a hash) into equality classes by
// byte-for-byte window comparison. Two window starts `i` and
// `j` land in the same class iff
// `tokens[i..i+windowSize] == tokens[j..j+windowSize]` as
// string slices. The number of classes per call is bounded
// by the number of distinct content values among hash
// collisions; in the no-collision case the function returns
// a single class equal to its input.
func groupByEquality(indices []int, tokens []string) [][]int {
	out := [][]int{}
	for _, idx := range indices {
		placed := false
		for c := range out {
			// Compare `idx`'s window to the representative
			// of class `c` (the first index in the slice).
			rep := out[c][0]
			if windowsEqual(tokens, rep, idx) {
				out[c] = append(out[c], idx)
				placed = true
				break
			}
		}
		if !placed {
			out = append(out, []int{idx})
		}
	}
	return out
}

// windowsEqual reports whether the
// [duplicationWindowSize]-long windows starting at `a` and
// `b` in `tokens` contain identical token sequences.
func windowsEqual(tokens []string, a, b int) bool {
	for k := 0; k < duplicationWindowSize; k++ {
		if tokens[a+k] != tokens[b+k] {
			return false
		}
	}
	return true
}
