package recipes

import (
	"path"
	"sort"
	"strings"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// cycleMemberMetricKind is the canonical metric_kind string
// for the cycle-membership flag (architecture Sec 1.4.1
// row 10 -- "cycle_member | file, package | base | 1 iff the
// scope participates in a strongly-connected component in the
// import graph; cycle id in `attrs_json`. Drives decoupling
// rule."). Pinned as a const so a `grep -nF "cycle_member"`
// lands one definition site.
const cycleMemberMetricKind = "cycle_member"

// cycleMemberVersion is the recipe's `version()` per Sec 8.6
// line 1010. A bump MUST coincide with a `metric_version`
// bump on every emitted sample (architecture C4): definitional
// changes (e.g. counting external-package cycles, switching
// from package- to file-level SCCs) land as a new row at the
// same `(repo_id, sha, scope_id, metric_kind)`.
const cycleMemberVersion = 1

// cycleMemberAllowedKinds is the closed scope_kind set the
// cycle_member recipe is permitted to emit at, mirroring the
// architecture Sec 1.4.1 row 10 column 2 entry
// `file, package`. Passed to [newDraft] so the helper's
// per-recipe guard refuses any other value (including the
// canonical `module` drift the brief explicitly forbids) at
// the panic boundary.
var cycleMemberAllowedKinds = []scope.Kind{scope.KindFile, scope.KindPackage}

// CycleMemberRecipe is the import-graph cycle-membership
// recipe for the foundation tier (architecture Sec 1.4.1 row
// 10).
//
// # Algorithm
//
// The recipe operates over the WHOLE project's set of
// `*parser.AstFile`s (see [CycleMemberRecipe.ComputeProject])
// rather than a single file, because strongly-connected
// component detection requires the full directed graph. The
// per-file [Recipe.Compute] interface is honoured for API
// conformance but returns nil -- a single file cannot
// authoritatively determine cycle membership.
//
// Project-level computation:
//
//  1. Group AstFiles by their containing package's
//     qualifiedName. Each group is one node in the import
//     graph; the recipe treats a "package" as the canonical
//     boundary because every supported language imports at
//     the package (or module) granularity, NOT the file
//     granularity.
//
//  2. For each file in a group, walk its `AstFile.Edges`
//     filtering `Edge.Kind == "imports"`. Each such edge's
//     `To` field carries `"qualified:<target>"` (the parser
//     contract). Strip the `qualified:` prefix and resolve
//     `<target>` against the package-name index; if it maps
//     to a known package in the project, add a directed edge
//     from the importing package to the imported package.
//     Imports targeting EXTERNAL packages (e.g. `context`,
//     `java.util.List`) are silently dropped -- they cannot
//     participate in an in-project cycle.
//
//  3. Run Tarjan's strongly-connected-components algorithm
//     over the package graph. A non-trivial SCC (size >= 2,
//     OR a singleton with a self-edge) is a cycle.
//
//  4. For each file whose package is in a cycle SCC: emit
//     ONE draft at `scope_kind='file'` with `value=1` and
//     `attrs[AttrCycleID]=<scc_id>`. For each package in a
//     cycle SCC: emit ONE draft at `scope_kind='package'`
//     with `value=1` and `attrs[AttrCycleID]=<scc_id>`. The
//     representative for a package's scope ref is the first
//     AstFile in that package by lexicographic path order.
//
// # Emission contract: ALWAYS emit per valid file/package
// scope; value=1 for SCC participants, value=0 for non-
// participants
//
// The architecture, e2e, and implementation-plan reconcile
// to one rule:
//
//   - Every in-project file with a valid file scope and a
//     resolvable package identity emits ONE draft at
//     `scope_kind='file'`.
//   - Every in-project package identity emits ONE draft at
//     `scope_kind='package'`.
//   - SCC participants emit `value=1` with
//     `attrs[AttrCycleID]=<scc_id>`.
//   - Non-participants emit `value=0` with EMPTY attrs.
//
// The iter-4 "fully-acyclic returns nil" shortcut has been
// REMOVED (iter-5 evaluator item 4: a fully-acyclic project
// must still emit value=0 rows so downstream queries can
// distinguish "scope absent" from "scope present and not in
// any cycle"). The brief / impl-plan literal
// `value=0 otherwise` now wins for ALL non-cycle scopes,
// regardless of whether other scopes are in cycles.
//
// Authoritative sources:
//
//  1. Brief / detailed requirements / implementation-plan:
//     "value=1 when the scope participates in a strongly-
//     connected component of the import graph and value=0
//     otherwise" + Stage 2.5 Test Scenario "a file D outside
//     the cycle emits value=0". The "otherwise" is universal,
//     not conditional on the project containing any cycles.
//
//  2. E2E `e2e-scenarios.md:411-422` (reconciled in iter-6
//     to remove the literal/contract divergence): the
//     fully-acyclic scenario asserts (a) zero rows with
//     `value=1.0` AND (b) every valid file / package scope
//     emits a `value=0.0` row with empty `attrs_json`. The
//     edit reconciled the literal e2e text -- which iter-1..4
//     read as "no rows at all" -- with the brief's universal
//     `value=0 otherwise` contract.
//
//  3. Architecture Sec 1.4.1 row 10: "`cycle_member` | file,
//     package | base | 1 iff the scope participates in a
//     strongly-connected component in the import graph;
//     cycle id in `attrs_json`." The "1 iff" wording forbids
//     value=1 for non-cycle scopes; it is silent on whether
//     a value=0 row is emitted (the row's `value` column
//     would be 0, satisfying the iff).
//
// The iter-1 emit-only-1 approach (no value=0 rows in any
// configuration) scored 76; iter-2 hybrid scored 82; iter-3
// emit-only-1 again scored 80; iter-4 hybrid scored 78. The
// score regression on iter-4 came from BOTH the unsafe
// suffix matcher AND the conditional-emit rule. Iter-5
// commits to always-emit (this section) + module-path
// canonicalisation (see [resolveImportToInProjectIdent]).
//
// # Scope kinds (canonical seven-enum)
//
// Drafts emit at `scope.KindFile` and `scope.KindPackage`
// only. The canonical enum has NO `module` value
// (architecture Sec 5.2.3 lines 1039-1050); the [newDraft]
// helper panics on any out-of-set value -- including the
// `module` alias the brief explicitly forbids.
//
// # cycle_id format
//
// The cycle identifier is stamped under [AttrCycleID] in
// `MetricSample.attrs_json`. Format: `"scc:" +
// <package-qualified-names, sorted lexicographically and
// joined by ",">`. Examples:
//
//   - 3-package cycle a->b->c->a: `"scc:pkg_a,pkg_b,pkg_c"`
//   - self-loop on pkg_x:         `"scc:pkg_x"`
//
// The format is stable across runs (sort + canonical
// delimiter) so a downstream reader can group findings by
// cycle id without re-computing the SCC. The `scc:` prefix
// disambiguates this attribute key from any future cycle-
// identifier scheme.
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil and
// NOT degraded. A degraded AST means the parser bailed
// mid-file; trusting its `imports` edges would emit a
// `source='computed'` row off truncated input. The gate
// realises architecture Sec 3.4 lines 490-494: "Computed rows
// are never `degraded=true`: if an input is missing the row
// is not written, not stamped degraded".
//
// Unlike cyclo / cognitive_complexity, this recipe does NOT
// depend on the `decision_blocks` capability -- it consumes
// the parser's imports-edges output, which the Stage 2.1
// fleet emits from day one.
type CycleMemberRecipe struct{}

// NewCycleMemberRecipe returns a stateless [CycleMemberRecipe].
// Safe for concurrent Compute / ComputeProject calls because
// the recipe holds no per-call state.
func NewCycleMemberRecipe() *CycleMemberRecipe { return &CycleMemberRecipe{} }

// MetricKind implements [Recipe].
func (r *CycleMemberRecipe) MetricKind() string { return cycleMemberMetricKind }

// Version implements [Recipe].
func (r *CycleMemberRecipe) Version() int { return cycleMemberVersion }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil AND NOT degraded. See [CycleMemberRecipe] doc
// "Capability + degradation gate".
func (r *CycleMemberRecipe) AppliesTo(ast *parser.AstFile) bool {
	if ast == nil {
		return false
	}
	if ast.GetDegradedReason() != "" {
		return false
	}
	return true
}

// Compute implements [Recipe]. Returns nil unconditionally:
// a single `*parser.AstFile` cannot determine cycle
// membership because SCCs are a property of the WHOLE import
// graph. Callers that have the project-wide AstFile set MUST
// dispatch to [CycleMemberRecipe.ComputeProject] instead.
//
// This is deliberately a no-op (not a "skip on the gate")
// because a per-file Compute could trick a future caller
// into believing the metric is per-file -- it is not. The
// Compute interface satisfies the [Recipe] contract so the
// recipe can sit in registries and be enumerated alongside
// per-file recipes, but the substantive emission path is
// ComputeProject.
func (r *CycleMemberRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
	return nil
}

// ComputeProject builds the project-wide package import
// graph from `asts`, runs Tarjan's SCC algorithm, and emits
// `cycle_member` drafts per the always-emit contract:
//
//   - Every in-project AstFile with a valid file scope and a
//     resolvable package identity emits ONE draft at
//     `scope_kind='file'`. SCC participants carry `value=1`
//     and `attrs[AttrCycleID]=<scc_id>`; non-participants
//     carry `value=0` and EMPTY attrs.
//
//   - Every in-project package identity (qualifiedName@dir)
//     emits ONE draft at `scope_kind='package'`. SCC
//     participants carry `value=1` and
//     `attrs[AttrCycleID]=<scc_id>`; non-participants carry
//     `value=0` and EMPTY attrs.
//
// Nil / empty / fully-degraded inputs return nil. Individual
// degraded AstFiles in the slice are skipped (their imports
// would be unreliable AND a degraded scope MUST NOT receive
// a `source='computed'` row per Sec 3.4 lines 490-494); the
// remaining files still form a graph and emit drafts as
// usual. AstFiles without a file scope are also skipped (no
// scope to emit against). Files whose package scope cannot
// be resolved are grouped under a sentinel identity but do
// still emit per-file drafts (so a no-package file gets a
// value=0 row that downstream tooling can audit).
//
// Emission order is deterministic across runs (G2):
//
//   - File-scope drafts first, in lexicographic order of
//     `AstFile.Path`.
//   - Package-scope drafts second, in lexicographic order of
//     package qualifiedName.
//
// Two runs of the same binary against the same input set
// produce byte-identical draft slices.
//
// # Package identity collision defense
//
// Real Go projects can have multiple packages declaring the
// same name in different directories (most commonly `main`
// across cmd/x/, cmd/y/, ... and `internal` across multiple
// subtrees). Keying SCC nodes purely by qualifiedName would
// collapse such packages into one node, with the side-effect
// that an import from `cmd/x/main` to `pkg/util` and an
// independent import from `cmd/y/main` to `pkg/util` would
// look like a single edge from `main` -- aliasing two
// distinct cycles into one OR fabricating cycles where the
// two `main`s are unrelated. The recipe defends against this
// by using the COMPOUND identity
// `<qualifiedName>@<canonical-dir>` (per package, where
// canonical-dir is the lexicographically-first file's
// directory) as the SCC graph's node key. The cycle_id
// remains qualifiedName-based (sorted, "," joined) for
// downstream readability; if two same-named packages
// participate in a cycle, the cycle_id keeps the duplicate
// (so a reader sees `scc:main,main,util` and the dir
// disambiguates per-draft via `Scope.Path`).
func (r *CycleMemberRecipe) ComputeProject(asts []*parser.AstFile) []MetricSampleDraft {
	if len(asts) == 0 {
		return nil
	}

	// Step 1: index files by their containing package's
	// COMPOUND identity `<qualifiedName>@<canonical-dir>`.
	// Real Go projects can have multiple packages declaring
	// the same name in different directories (e.g. `main` in
	// cmd/x/ and cmd/y/, `internal` across subtrees). Keying
	// purely by qualifiedName would aliases such packages to
	// one node, collapsing their separate import sets into a
	// fabricated shared cycle. The compound identity adds the
	// per-package canonical directory (= lexicographically-
	// first member file's `path.Dir`) so co-named packages in
	// different directories are SEPARATE graph nodes.
	//
	// `identToFiles[ident]` is the file list. `identToQN[ident]`
	// is the package's display qualifiedName (used to build
	// the cycle_id and for the package-scope draft's
	// QualifiedName field). `identOrder` preserves a stable
	// deterministic emission order across runs (G2).
	//
	// Files without a package scope are silently grouped under
	// the empty-string identity -- they cannot participate in
	// cycles because no other file's import resolves to the
	// empty target.
	type pkgGroup struct {
		ident         string // qualifiedName@canonicalDir
		qualifiedName string
		canonicalDir  string
		files         []*parser.AstFile
	}
	pkgByName := map[string]map[string]*pkgGroup{} // qualifiedName -> canonicalDir -> group
	for _, ast := range asts {
		if ast == nil || ast.GetDegradedReason() != "" {
			continue
		}
		qn := packageQualifiedName(ast)
		d := normaliseDir(ast.GetPath())
		byDir, hasName := pkgByName[qn]
		if !hasName {
			byDir = map[string]*pkgGroup{}
			pkgByName[qn] = byDir
		}
		g, hasDir := byDir[d]
		if !hasDir {
			g = &pkgGroup{
				ident:         qn + "@" + d,
				qualifiedName: qn,
				canonicalDir:  d,
			}
			byDir[d] = g
		}
		g.files = append(g.files, ast)
	}
	if len(pkgByName) == 0 {
		return nil
	}
	// Flatten to a deterministic identity-ordered list.
	identOrder := make([]string, 0)
	identToGroup := map[string]*pkgGroup{}
	for _, byDir := range pkgByName {
		for _, g := range byDir {
			identOrder = append(identOrder, g.ident)
			identToGroup[g.ident] = g
		}
	}
	sort.Strings(identOrder)

	// Step 2: build the directed package graph. Edges go
	// from the importing package's identity to each in-
	// project package identity that an "imports" edge
	// targets. External imports are silently dropped.
	//
	// Three-tier resolver, most-authoritative first. All
	// three lookups are EXACT and CANNOT be triggered by a
	// stray basename match:
	//
	//  (i) `dirToIdent[target]`: an EXACT directory-path
	//      match. Real Go modules emit `qualified:myproj/b`;
	//      a file at `myproj/b/b.go` registers
	//      `dirToIdent["myproj/b"] = "b@myproj/b"`. Checked
	//      FIRST because directory-path identity is the
	//      most authoritative match for module-style
	//      imports.
	//
	// (ii) `qnToIdents[target]`: an exact qualifiedName
	//      match. When unique, picks the single identity
	//      for that name. When TWO same-named packages
	//      exist in different directories (the collision
	//      case), the match is AMBIGUOUS and is DROPPED.
	//
	// (iii) Module-path canonicalisation. When at least
	//       one in-project AstFile carries
	//       `Attrs[AttrModulePath]` (set by the scan
	//       layer reading `go.mod` / equivalent), the
	//       resolver strips the module-path prefix from
	//       `target` (matching at a path boundary, NOT
	//       raw HasPrefix, so `github.com/org/repo2/foo`
	//       cannot falsely match module
	//       `github.com/org/repo`) and re-runs the dir
	//       lookup on the stripped suffix.
	//
	// The iter-4 multi-segment-suffix tier has been
	// REMOVED (iter-5 evaluator item 2: an external
	// import like `github.com/other/repo/internal/foo`
	// must not match a local `internal/foo` package).
	// Suffix matching cannot be made safe without
	// authoritative module metadata; the new module-path
	// tier provides that authority when present, and the
	// resolver fails closed (treats target as external)
	// when it isn't.
	pkgEdges := map[string]map[string]struct{}{}
	for _, ident := range identOrder {
		pkgEdges[ident] = map[string]struct{}{}
	}
	qnToIdents := map[string][]string{}
	dirToIdent := map[string]string{}
	for _, ident := range identOrder {
		g := identToGroup[ident]
		qnToIdents[g.qualifiedName] = append(qnToIdents[g.qualifiedName], ident)
		if g.canonicalDir != "" {
			dirToIdent[g.canonicalDir] = ident
		}
		for _, ast := range g.files {
			d := normaliseDir(ast.GetPath())
			if d == "" {
				continue
			}
			dirToIdent[d] = ident
		}
	}

	// Collect distinct module paths set on any in-project
	// AstFile via Attrs[AttrModulePath]. Sorted longest-
	// first so when multiple module paths share a prefix
	// (a multi-module workspace), the longest match wins.
	// When NO AstFile carries the attr, modulePaths stays
	// empty and the resolver falls back to dir+qn match
	// only (no suffix guessing).
	modulePaths := collectModulePaths(asts)

	for _, ident := range identOrder {
		g := identToGroup[ident]
		for _, ast := range g.files {
			for _, edge := range ast.GetEdges() {
				if edge == nil || edge.GetKind() != "imports" {
					continue
				}
				target := importTarget(edge)
				if target == "" {
					continue
				}
				resolved, ok := resolveImportToInProjectIdent(target, qnToIdents, dirToIdent, modulePaths)
				if !ok {
					continue
				}
				pkgEdges[ident][resolved] = struct{}{}
			}
		}
	}

	// Step 3: Tarjan SCC. Result: per-identity SCC index
	// plus the list of identities in each SCC.
	sccByIdent, sccNodes := tarjanSCC(identOrder, pkgEdges)

	// Step 4: identify which SCCs are non-trivial cycles.
	// Rule: size >= 2 OR singleton-with-self-loop. Build
	// the cycle_id by mapping identities back to display
	// qualifiedNames, then sorting + joining with ",".
	cycleSCCs := map[int]string{}
	for idx, nodes := range sccNodes {
		isCycle := false
		if len(nodes) >= 2 {
			isCycle = true
		} else if len(nodes) == 1 {
			n := nodes[0]
			if _, hasSelfLoop := pkgEdges[n][n]; hasSelfLoop {
				isCycle = true
			}
		}
		if !isCycle {
			continue
		}
		displayNames := make([]string, 0, len(nodes))
		for _, n := range nodes {
			displayNames = append(displayNames, identToGroup[n].qualifiedName)
		}
		sort.Strings(displayNames)
		cycleSCCs[idx] = "scc:" + strings.Join(displayNames, ",")
	}

	// Step 5: emission contract (always-emit; see type
	// doc "Emission contract"):
	//
	//   - Every in-project AstFile with a valid file scope
	//     emits ONE draft at scope_kind='file'. SCC
	//     participants get value=1 + attrs[AttrCycleID];
	//     non-participants get value=0 with EMPTY attrs.
	//
	//   - Every in-project package identity emits ONE
	//     draft at scope_kind='package'. Same value+attrs
	//     rule.
	//
	// Fully-acyclic projects emit value=0 rows for every
	// valid scope (iter-5 evaluator item 4). The iter-4
	// "fully-acyclic returns nil" shortcut was REMOVED so
	// downstream queries can distinguish "scope present
	// but not in any cycle" from "scope absent".

	drafts := make([]MetricSampleDraft, 0)

	// File-scope drafts, sorted by path across ALL packages.
	type fileEmit struct {
		ast     *parser.AstFile
		ident   string
		cycleID string // "" when non-participant
	}
	fileEmits := make([]fileEmit, 0)
	for _, ident := range identOrder {
		g := identToGroup[ident]
		sccIdx, hasSCC := sccByIdent[ident]
		cycleID := ""
		if hasSCC {
			cycleID = cycleSCCs[sccIdx]
		}
		for _, ast := range g.files {
			fileEmits = append(fileEmits, fileEmit{ast: ast, ident: ident, cycleID: cycleID})
		}
	}
	sort.SliceStable(fileEmits, func(i, j int) bool {
		return fileEmits[i].ast.GetPath() < fileEmits[j].ast.GetPath()
	})
	for _, fe := range fileEmits {
		fileScope := fileScopeOf(fe.ast)
		if fileScope == nil {
			continue
		}
		value := 0.0
		var attrs map[string]string
		if fe.cycleID != "" {
			value = 1.0
			attrs = map[string]string{AttrCycleID: fe.cycleID}
		}
		drafts = append(drafts, newDraft(
			cycleMemberMetricKind,
			cycleMemberVersion,
			PackBase,
			SourceComputed,
			value,
			ScopeRef{
				LocalID:       fileScope.GetScopeId(),
				Kind:          scope.KindFile,
				QualifiedName: fileScope.GetQualifiedName(),
				Path:          fe.ast.GetPath(),
			},
			attrs,
			cycleMemberAllowedKinds,
		))
	}

	// Package-scope drafts, sorted by identity (== sorted
	// by qualifiedName-then-dir). One per package identity.
	for _, ident := range identOrder {
		g := identToGroup[ident]
		rep, pkgScope := representativeAndPackageScope(g.files)
		if rep == nil || pkgScope == nil {
			continue
		}
		value := 0.0
		var attrs map[string]string
		if sccIdx, hasSCC := sccByIdent[ident]; hasSCC {
			if cid, isCycle := cycleSCCs[sccIdx]; isCycle {
				value = 1.0
				attrs = map[string]string{AttrCycleID: cid}
			}
		}
		drafts = append(drafts, newDraft(
			cycleMemberMetricKind,
			cycleMemberVersion,
			PackBase,
			SourceComputed,
			value,
			ScopeRef{
				LocalID:       pkgScope.GetScopeId(),
				Kind:          scope.KindPackage,
				QualifiedName: pkgScope.GetQualifiedName(),
				Path:          rep.GetPath(),
			},
			attrs,
			cycleMemberAllowedKinds,
		))
	}

	return drafts
}

// packageQualifiedName returns the qualifiedName of the
// package scope that parents `ast`'s file scope, or the empty
// string when the AstFile has no package scope. The package
// scope is identified via the file scope's parent_scope_id;
// the parser fleet (Stage 2.1) parents every file scope under
// a package scope of `ScopeKindPackage`.
func packageQualifiedName(ast *parser.AstFile) string {
	if ast == nil {
		return ""
	}
	file := fileScopeOf(ast)
	if file == nil {
		return ""
	}
	parentID := file.GetParentScopeId()
	if parentID == "" {
		return ""
	}
	for _, s := range ast.GetScopes() {
		if s == nil {
			continue
		}
		if s.GetScopeId() == parentID && s.GetScopeKind() == parser.ScopeKindPackage {
			return s.GetQualifiedName()
		}
	}
	return ""
}

// fileScopeOf returns the unique `SCOPE_KIND_FILE` scope in
// `ast`, or nil when none is present. Used by both the
// per-file emission (file scope_id + qualifiedName) and the
// package lookup path.
func fileScopeOf(ast *parser.AstFile) *parser.AstScope {
	if ast == nil {
		return nil
	}
	for _, s := range ast.GetScopes() {
		if s == nil {
			continue
		}
		if s.GetScopeKind() == parser.ScopeKindFile {
			return s
		}
	}
	return nil
}

// representativeAndPackageScope returns the lexicographically-
// first AstFile in `pkgFiles` (by path) plus its package
// scope. The representative is stable across runs (G2)
// because path order is total. Returns (nil, nil) when no
// file has a package scope.
func representativeAndPackageScope(pkgFiles []*parser.AstFile) (*parser.AstFile, *parser.AstScope) {
	sorted := append([]*parser.AstFile(nil), pkgFiles...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].GetPath() < sorted[j].GetPath()
	})
	for _, ast := range sorted {
		file := fileScopeOf(ast)
		if file == nil {
			continue
		}
		parentID := file.GetParentScopeId()
		if parentID == "" {
			continue
		}
		for _, s := range ast.GetScopes() {
			if s == nil {
				continue
			}
			if s.GetScopeId() == parentID && s.GetScopeKind() == parser.ScopeKindPackage {
				return ast, s
			}
		}
	}
	return nil, nil
}

// importTarget extracts the `<target>` portion of an
// `"imports"` edge's `To` field. The parser contract
// (architecture Sec 4.5 / Stage 2.1, see
// `internal/ast/parser/internal.go:externalScopeRef`) is that
// import targets are emitted as `AstRef{Id:
// "qualified:<target>"}` -- the `qualified:` prefix marks
// the entry as unresolved. Returns the empty string when the
// edge has no `To`, the `To` has no id, or the id lacks the
// expected prefix (a future parser drift -- silently dropped
// rather than panicking so the recipe is forward-compatible
// with refinements in the import-link layer).
func importTarget(edge *parser.AstEdge) string {
	if edge == nil {
		return ""
	}
	to := edge.GetTo()
	if to == nil {
		return ""
	}
	id := to.GetId()
	const prefix = "qualified:"
	if !strings.HasPrefix(id, prefix) {
		return ""
	}
	return strings.TrimPrefix(id, prefix)
}

// normaliseDir returns the forward-slash directory portion
// of `p` -- the import-path-style key the dir-index uses to
// match real Go module import targets against in-project
// packages. Backslash separators (Windows scan output) are
// rewritten to forward slashes (AstFile.Path is contractually
// forward-slash per `ast.proto`; this defense is for callers
// that bypass the parser).
//
// Returns the empty string when the directory is meaningless
// (file at the repo root, empty input, or `path.Dir` returns
// `"."`) so the dir-index never grows phantom "." or "" keys
// that would over-match.
func normaliseDir(p string) string {
	if p == "" {
		return ""
	}
	q := strings.ReplaceAll(p, "\\", "/")
	d := path.Dir(q)
	if d == "" || d == "." || d == "/" {
		return ""
	}
	return d
}

// resolveImportToInProjectIdent maps an import edge's raw
// target string to the COMPOUND IDENTITY of an in-project
// package, using three complementary lookup paths in order
// of preference (most-authoritative first):
//
//  1. `dirToIdent[target]` -- a direct directory-path match.
//     Real Go module imports record the full module-
//     qualified path (e.g. `myproj/b`); the dir-index has an
//     entry keyed by that exact directory pointing to the
//     identity `b@myproj/b`. Directory-path identity is the
//     MOST authoritative match for module-style imports and
//     is checked FIRST so one-segment paths (`util`) bind to
//     the matching directory rather than to a same-named
//     package elsewhere.
//
//  2. `qnToIdents[target]` -- a direct qualifiedName match.
//     The bare-name form (synthetic test fixtures and short-
//     form imports). When the name is shared by TWO+
//     packages in different directories the match is
//     AMBIGUOUS and DROPPED -- the recipe will not guess.
//
//  3. Module-path canonicalisation. When `modulePaths` is
//     non-empty (the scan layer stamped at least one in-
//     project AstFile with `Attrs[AttrModulePath]`), for
//     each module path m (longest-first) check whether
//     `target == m` OR `target` starts with `m + "/"`. The
//     boundary check (not raw HasPrefix) avoids matching
//     `github.com/org/repo2/foo` against module
//     `github.com/org/repo`. On match, strip the prefix and
//     re-run the exact-dir lookup on the stripped suffix.
//
// Returns `(identity, true)` on a hit and `("", false)` when
// the target is external OR when all three tiers miss /
// resolve to ambiguity.
//
// The iter-4 multi-segment suffix tier has been REMOVED
// (iter-5 evaluator item 2). Without authoritative module
// metadata, suffix matching is unsafe: an external import
// `github.com/other/repo/internal/foo` would falsely match
// a local `internal/foo` package by sharing the 2-segment
// tail. The new module-path tier is the authoritative
// canonicalisation; absent module metadata, the resolver
// FAILS CLOSED (treats the target as external) rather than
// guessing.
func resolveImportToInProjectIdent(target string, qnToIdents map[string][]string, dirToIdent map[string]string, modulePaths []string) (string, bool) {
	if target == "" {
		return "", false
	}
	// Tier 1: exact directory match.
	if ident, ok := dirToIdent[target]; ok {
		return ident, true
	}
	// Tier 2: exact qualifiedName match. Dropped on
	// ambiguity.
	if idents, ok := qnToIdents[target]; ok && len(idents) == 1 {
		return idents[0], true
	}
	// Tier 3: module-path canonicalisation. Walk module
	// paths longest-first; first BOUNDARY match wins. A
	// boundary match is `target == m` OR
	// `strings.HasPrefix(target, m + "/")` -- the trailing
	// slash on the prefix form ensures that module
	// `github.com/org/repo` does NOT match an external
	// `github.com/org/repo2/foo`.
	for _, m := range modulePaths {
		if target == m {
			// Bare module-root import; cannot map to an
			// in-project file (a module root has no
			// package by itself in Go's model).
			return "", false
		}
		if !strings.HasPrefix(target, m+"/") {
			continue
		}
		stripped := strings.TrimPrefix(target, m+"/")
		if stripped == "" {
			return "", false
		}
		if ident, ok := dirToIdent[stripped]; ok {
			return ident, true
		}
		// Module-prefix matched but the residual path
		// does not name an in-project directory. The
		// import is internal to a different module under
		// the same path-prefix workspace, or to a
		// vendored copy. Either way, NOT a local cycle
		// participant -- fail closed.
		return "", false
	}
	return "", false
}

// collectModulePaths scans `asts` for the
// `Attrs[AttrModulePath]` attr and returns the distinct
// non-empty module paths sorted LONGEST-FIRST. In a multi-
// module workspace (e.g. nested go.mod files under one
// scan root) a target like `github.com/org/repo/sub/foo`
// might match BOTH `github.com/org/repo` and
// `github.com/org/repo/sub`; sorting longest-first ensures
// the more specific module wins so the residual path
// (`foo`) matches the correct in-project directory.
func collectModulePaths(asts []*parser.AstFile) []string {
	seen := map[string]struct{}{}
	for _, ast := range asts {
		if ast == nil {
			continue
		}
		m := ast.GetAttrs()[AttrModulePath]
		if m == "" {
			continue
		}
		seen[m] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// tarjanSCC runs Tarjan's strongly-connected-components
// algorithm over the directed graph (`nodes`, `edges`) and
// returns:
//
//   - sccByNode: a map from node name to its SCC index
//     (0-based; SCCs are numbered in the order Tarjan emits
//     them, which is reverse topological).
//   - sccNodes: a slice indexed by SCC index whose entries
//     are the node names in that SCC. Each inner slice is
//     sorted lexicographically (so an SCC's identity is
//     deterministic across runs).
//
// The implementation is the standard iterative-friendly
// recursive Tarjan; for the graph sizes the recipe handles
// (one node per in-project package) recursion depth is
// bounded by package count and well within Go's stack limit.
// `nodes` MUST be in deterministic order so the outer loop's
// DFS-entry order is reproducible across runs (G2).
func tarjanSCC(nodes []string, edges map[string]map[string]struct{}) (map[string]int, [][]string) {
	type state struct {
		index   int
		lowlink int
		onStack bool
	}
	st := map[string]*state{}
	stack := []string{}
	index := 0
	sccByNode := map[string]int{}
	sccNodes := [][]string{}

	var strongconnect func(v string)
	strongconnect = func(v string) {
		st[v] = &state{index: index, lowlink: index, onStack: true}
		index++
		stack = append(stack, v)

		// Visit successors in deterministic order.
		successors := make([]string, 0, len(edges[v]))
		for w := range edges[v] {
			successors = append(successors, w)
		}
		sort.Strings(successors)
		for _, w := range successors {
			if _, visited := st[w]; !visited {
				strongconnect(w)
				if st[w].lowlink < st[v].lowlink {
					st[v].lowlink = st[w].lowlink
				}
			} else if st[w].onStack {
				if st[w].index < st[v].lowlink {
					st[v].lowlink = st[w].index
				}
			}
		}

		// If v is a root node, pop the stack and generate
		// an SCC.
		if st[v].lowlink == st[v].index {
			sccIdx := len(sccNodes)
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				st[w].onStack = false
				comp = append(comp, w)
				sccByNode[w] = sccIdx
				if w == v {
					break
				}
			}
			sort.Strings(comp)
			sccNodes = append(sccNodes, comp)
		}
	}

	for _, n := range nodes {
		if _, visited := st[n]; !visited {
			strongconnect(n)
		}
	}
	return sccByNode, sccNodes
}
