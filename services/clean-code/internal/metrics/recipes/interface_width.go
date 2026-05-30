package recipes

import (
	"strings"
	"unicode"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// interfaceWidthMetricKind is the canonical metric_kind
// string for the type's-method-count (Chidamber & Kemerer
// "Weighted Methods per Class" variant) -- architecture Sec
// 1.4.1 row 8 -- "interface_width | class, interface | solid
// | Method count of a class/interface's exposed surface.
// Drives ISP rule". Pinned as a const so a
// `grep -nF "interface_width"` lands one definition site.
// NOT `class_width`, `method_count`, or `wmc` -- the
// closed-set spelling is exactly `interface_width`.
const interfaceWidthMetricKind = "interface_width"

// interfaceWidthVersion is the recipe's `version()` per Sec
// 8.6 line 1010. A bump MUST coincide with a `metric_version`
// bump on emitted samples (architecture C4): definitional
// changes land as a new row at the same `(repo_id, sha,
// scope_id, metric_kind)`.
//
// v1 emits the count of PUBLIC direct method children of a
// class or interface, per architecture Sec 1.4.1 row 8
// ("Method count of a class/interface's exposed surface").
// Public-vs-non-public is decided by [isPublicMethod] using
// per-language conventions because the Stage 2.1 parser fleet
// does not yet expose a single uniform `visibility` attr
// across all four languages.
const interfaceWidthVersion = 1

// interfaceWidthAllowedKinds is the closed scope_kind set
// this recipe is permitted to emit at, mirroring architecture
// Sec 1.4.1 row 8 column 2 entry `class, interface`. Passed
// to [newDraft] so the per-recipe panic guard refuses any
// other value -- particularly the architectural drift
// candidates `module` (NOT in the canonical 7-enum), `repo`,
// or `method` (interface_width is a CONTAINER metric; method-
// scope rows would be nonsense).
var interfaceWidthAllowedKinds = []scope.Kind{scope.KindClass, scope.KindInterface}

// InterfaceWidthRecipe is the type-method-count recipe for the
// foundation tier (architecture Sec 1.4.1 row 8). It computes
// the size of a class or interface's exposed method surface
// -- the primary input to the ISP (Interface Segregation
// Principle) rule pack.
//
// # Algorithm (per-file)
//
// For each `SCOPE_KIND_CLASS` or `SCOPE_KIND_INTERFACE` scope
// T in the AST, the recipe emits ONE draft whose `value` is
// the count of PUBLIC `SCOPE_KIND_METHOD` scopes whose
// `parent_scope_id` equals T's scope id. Closures nested
// inside a method body are NOT counted because their
// `parent_scope_id` is the enclosing method, not T.
//
// "Public" is decided by [isPublicMethod] using per-language
// conventions, mirroring how each parser stamps the surface:
//
//   - Go: a method whose first rune is upper-case is exported
//     (Go's standard `ast.IsExported` rule). Lower-case (or
//     non-letter) names are unexported.
//   - Java: the parser stamps `attrs["public"]="true"` on the
//     method when the source had an explicit `public`
//     modifier (`java.go::javaMethodAttrs`); interface
//     members are implicitly public, marked via
//     `attrs["interface_member"]="true"`. Absence of both
//     means package-private / private / protected, so NOT
//     public.
//   - TypeScript: the parser stamps `attrs["private"]` /
//     `attrs["protected"]` for explicit modifiers
//     (`typescript.go::tsMethodAttrs`); TypeScript defaults
//     to `public`, so the absence of those markers means
//     public.
//   - Python: PEP-8 names starting with `_` are private by
//     convention; names starting with `__` (dunder methods
//     like `__init__`) ARE part of the public surface and
//     are counted.
//   - Any unknown language: default to public (the parser
//     fleet is a closed v1 set per
//     `parser.SupportedLanguages`; an unknown language here
//     is a future addition that has not yet had its
//     visibility convention codified, and over-counting
//     surfaces as an INFLATED ISP signal that a reviewer can
//     spot, whereas under-counting would silently mask a
//     real ISP risk).
//
// # Scope kinds (canonical seven-enum)
//
// Drafts emit at `scope.KindClass` and `scope.KindInterface`
// only. The [newDraft] helper panics on any other value;
// the per-recipe `allowedKinds` slice [interfaceWidthAllowedKinds]
// refuses `method`, `file`, `package`, `repo`, and `block`.
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil and
// NOT degraded. Unlike the call-graph and field-access
// metrics, this recipe needs NO capability attr -- it walks
// the scope tree only, which every Stage 2.1 parser populates
// from day one. This places `interface_width` in the "lit"
// foundation-tier set (architecture Sec 3.3 lit-metric table)
// alongside `loc`, `cycle_member`, and `depth_of_inheritance`.
//
// The non-degraded check realises Sec 3.4 lines 490-494: a
// degraded AST means the parser bailed mid-file, and a
// truncated scope tree would silently undercount methods.
// Better to skip emission than land a misleading row.
type InterfaceWidthRecipe struct{}

// NewInterfaceWidthRecipe returns a stateless
// [InterfaceWidthRecipe]. Safe for concurrent Compute calls.
func NewInterfaceWidthRecipe() *InterfaceWidthRecipe { return &InterfaceWidthRecipe{} }

// MetricKind implements [Recipe].
func (r *InterfaceWidthRecipe) MetricKind() string { return interfaceWidthMetricKind }

// Version implements [Recipe].
func (r *InterfaceWidthRecipe) Version() int { return interfaceWidthVersion }

// Pack implements [Recipe]. interface_width is in the `solid`
// pack (architecture Sec 1.4.1 row 8 -- "Drives ISP rule").
func (r *InterfaceWidthRecipe) Pack() Pack { return PackSolid }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil AND NOT degraded. No capability gate -- the scope
// tree is universally populated.
func (r *InterfaceWidthRecipe) AppliesTo(ast *parser.AstFile) bool {
	if ast == nil {
		return false
	}
	if ast.GetDegradedReason() != "" {
		return false
	}
	return true
}

// Compute implements [Recipe]. Emits one draft per class /
// interface scope, in source order, whose value is the count
// of PUBLIC direct method children. An AST with no class or
// interface scopes returns nil.
//
// The caller MUST gate on [InterfaceWidthRecipe.AppliesTo]
// before invoking Compute; the recipe ALSO honours the
// degradation gate as defence-in-depth.
func (r *InterfaceWidthRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
	if ast == nil {
		return nil
	}
	if ast.GetDegradedReason() != "" {
		return nil
	}
	idx := buildIndex(ast)

	classes := idx.scopesByKind(parser.ScopeKindClass)
	interfaces := idx.scopesByKind(parser.ScopeKindInterface)
	if len(classes)+len(interfaces) == 0 {
		return nil
	}

	language := ast.GetLanguage()
	drafts := make([]MetricSampleDraft, 0, len(classes)+len(interfaces))
	for _, c := range classes {
		drafts = append(drafts, r.emit(ast, c, scope.KindClass, idx, language))
	}
	for _, ifc := range interfaces {
		drafts = append(drafts, r.emit(ast, ifc, scope.KindInterface, idx, language))
	}
	return drafts
}

// emit builds one draft for a class/interface scope T whose
// value is the count of PUBLIC direct method children
// (children whose scope_kind is method AND whose
// parent_scope_id is T's scope id AND whose visibility
// resolves to public via [isPublicMethod]). Closures nested
// deeper than one level (i.e. methods whose nearest enclosing
// scope is another method, not T) are NOT counted because
// their direct parent is the method scope, not T.
func (r *InterfaceWidthRecipe) emit(ast *parser.AstFile, t *parser.AstScope, kind scope.Kind, idx scopeIndex, language string) MetricSampleDraft {
	count := 0
	for _, child := range idx.children[t.GetScopeId()] {
		if child == nil {
			continue
		}
		if child.GetScopeKind() != parser.ScopeKindMethod {
			continue
		}
		if !isPublicMethod(language, child) {
			continue
		}
		count++
	}
	return newDraft(
		interfaceWidthMetricKind,
		interfaceWidthVersion,
		PackSolid,
		SourceComputed,
		float64(count),
		ScopeRef{
			LocalID:       t.GetScopeId(),
			Kind:          kind,
			QualifiedName: t.GetQualifiedName(),
			Path:          ast.GetPath(),
		},
		nil,
		interfaceWidthAllowedKinds,
	)
}

// isPublicMethod returns whether the given method scope is
// part of the type's PUBLIC surface, per architecture Sec
// 1.4.1 row 8 ("Method count of a class/interface's exposed
// surface"). The check is language-specific because the
// Stage 2.1 parser fleet stamps visibility differently per
// language:
//
//   - parser.LanguageGo: Go has no `visibility` attr; the
//     language convention is "first rune is upper-case iff
//     exported" (Go's `ast.IsExported` rule). A non-ASCII
//     upper-case rune (e.g. capital Greek) also counts; the
//     check uses the Unicode property, matching `go/ast`.
//   - parser.LanguageJava: the parser stamps
//     `attrs["public"]="true"` when the source had an
//     explicit `public` modifier
//     (`internal/ast/parser/java.go::javaMethodAttrs`).
//     Interface members are implicitly public, marked via
//     `attrs["interface_member"]="true"`
//     (`java.go:248`). Absence of both means
//     package-private / private / protected -- NOT public.
//   - parser.LanguageTypeScript: the parser stamps
//     `attrs["private"]` / `attrs["protected"]` for explicit
//     modifiers
//     (`internal/ast/parser/typescript.go::tsMethodAttrs`).
//     TypeScript defaults to `public`, so absence of those
//     markers means public.
//   - parser.LanguagePython: PEP-8 names starting with `_`
//     are private by convention; names starting with `__`
//     (dunder methods such as `__init__`) ARE part of the
//     public surface and are counted.
//   - Any other / unknown language: default to public. The
//     parser fleet is a closed v1 set
//     (`parser.SupportedLanguages`), so this branch is
//     reached only by a hypothetical future language that
//     has not yet had its visibility convention codified.
//     Over-counting surfaces as an INFLATED ISP signal a
//     reviewer can spot; under-counting would silently mask
//     a real ISP risk.
func isPublicMethod(language string, method *parser.AstScope) bool {
	if method == nil {
		return false
	}
	switch language {
	case parser.LanguageGo:
		return isGoExportedName(method.GetName())
	case parser.LanguageJava:
		attrs := method.GetAttrs()
		if attrs["public"] == "true" {
			return true
		}
		if attrs["interface_member"] == "true" {
			return true
		}
		return false
	case parser.LanguageTypeScript:
		attrs := method.GetAttrs()
		if attrs["private"] == "true" {
			return false
		}
		if attrs["protected"] == "true" {
			return false
		}
		return true
	case parser.LanguagePython:
		return isPythonPublicName(method.GetName())
	default:
		return true
	}
}

// isGoExportedName mirrors Go's `ast.IsExported`: a name is
// exported iff its first rune is an upper-case letter. An
// empty name or one starting with a non-letter rune (digit,
// underscore) is NOT exported.
func isGoExportedName(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}

// isPythonPublicName implements the PEP-8 visibility
// convention: a name is private iff it starts with `_` BUT a
// dunder name (double underscore on BOTH sides, e.g.
// `__init__`, `__str__`, `__eq__`) is part of the public
// surface -- it is the language's special-method protocol and
// the canonical public hook into a class. Names with a double
// leading underscore but no trailing `__` (e.g.
// `__private_method`) trigger Python's name-mangling and are
// private.
func isPythonPublicName(name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__") && len(name) > 4 {
		return true
	}
	if strings.HasPrefix(name, "_") {
		return false
	}
	return true
}
