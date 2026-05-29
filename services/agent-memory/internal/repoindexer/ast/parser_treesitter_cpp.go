//go:build cgo

// Package ast — tree-sitter C++ parser.
//
// This file implements the LanguageParser interface for C++ on
// top of the smacker/go-tree-sitter `cpp` grammar binding. It
// is the C++ analogue of parser_treesitter.go's TypeScript and
// Python walkers, and is gated on `//go:build cgo` for the
// same reason: the grammar links against C-compiled
// tree-sitter runtime objects. The CGO=0 path (the portable
// `make test` lane) does not pull this file in and therefore
// does not register a C++ parser; that's intentional and
// documented at the dispatcher's registration site
// (parsers_cgo.go / parsers_nocgo.go) -- the registration
// belongs to a separate stage of the AST-PARSER-FOR-ADDIT
// story.
//
// Scope for this stage (per the workstream brief):
//
//   - Walk `translation_unit`; recurse into
//     `namespace_definition` accumulating the namespace path
//     via a `container` parameter that mirrors the TS walker's
//     `outer` argument.
//   - Emit `ClassDecl` for `class_specifier` /
//     `struct_specifier`, with
//     `QualifiedName=<namespace+"."+name>`.
//   - Capture `base_class_clause` entries into `Extends` and
//     populate `LangMeta["base_access"][baseName]=<access>`
//     and `LangMeta["template_params"]=[...]` for templated
//     classes.
//   - Emit `MethodDecl` for every `function_definition` --
//     in-class inline methods (EnclosingClass=<className>),
//     out-of-line definitions (`void Foo::bar() {}` ->
//     EnclosingClass=Foo extracted from the qualified
//     declarator), and free functions
//     (EnclosingClass=""). Per-method `Calls` /
//     `ReceiverCalls` slices are populated from
//     `call_expression` walks inside the body so the
//     dispatcher can resolve them into `static_calls` edges
//     (bare-name) and Pass 2b receiver-qualified edges
//     (`this->foo()` -> ReceiverCalls). Bodyless method
//     declarations (`void api();` inside a class) are NOT
//     emitted here -- they live in `field_declaration` and
//     are out of scope for this stage.
//   - Emit `Import` for every `preproc_include` at
//     translation-unit or namespace scope: system headers
//     (`#include <string>`) drop the angle brackets and
//     surface as `Module: "string"`; quoted local headers
//     (`#include "base.h"`) are normalized with a leading
//     `./` so the dispatcher's relative-import filter drops
//     them (`Module: "./base.h"`).
//
// Function-body walks are LIMITED to call_expression
// extraction -- they intentionally do NOT recurse through
// `class_specifier` / `struct_specifier` nodes that may
// appear inside a function body. Local types are an
// implementation detail of the body and must not leak as
// namespace-scope ClassDecls (the existing
// `TestTreeSitterCppParser_FunctionLocalClassesSkipped`
// test pins this invariant).

package ast

import (
	"context"
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/cpp"
)

// NewTreeSitterCppParser returns a LanguageParser backed by
// the upstream tree-sitter `cpp` grammar. The returned
// instance claims `.cc`, `.cpp`, `.cxx`, `.c++`, `.hpp`,
// `.hh`, `.hxx`, and `.h++` -- the canonical extension set
// for C++ source and header files. Plain `.h` is NOT claimed
// because tree-sitter-cpp accepts the C subset and the
// project's `parser_treesitter_c` workstream owns the C
// header extensions; routing `.h` through the cpp grammar
// here would produce duplicate registration the dispatcher
// rejects.
func NewTreeSitterCppParser() LanguageParser { return cppTreeSitterParser{} }

type cppTreeSitterParser struct{}

func (cppTreeSitterParser) Language() string { return "cpp" }

func (cppTreeSitterParser) Extensions() []string {
	return []string{".cc", ".cpp", ".cxx", ".c++", ".hpp", ".hh", ".hxx", ".h++"}
}

func (cppTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, cpp.GetLanguage())
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter cpp parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	w := cppWalker{src: src}
	w.walkTop(root)
	return ParseResult{
		Classes: w.classes,
		Methods: w.methods,
		Imports: w.imports,
	}, nil
}

// Grammar node-type constants. Centralising the literal
// strings keeps a single edit site if the upstream grammar
// renames a node (the C++ grammar has been stable for years
// but the discipline matches parser_treesitter.go).
const (
	cppNodeNamespaceDefinition       = "namespace_definition"
	cppNodeClassSpecifier            = "class_specifier"
	cppNodeStructSpecifier           = "struct_specifier"
	cppNodeBaseClassClause           = "base_class_clause"
	cppNodeAccessSpecifier           = "access_specifier"
	cppNodeTypeIdentifier            = "type_identifier"
	cppNodeQualifiedIdentifier       = "qualified_identifier"
	cppNodeQualifiedTypeIdentifier   = "qualified_type_identifier"
	cppNodeTemplateType              = "template_type"
	cppNodeDependentTypeIdentifier   = "dependent_type_identifier"
	cppNodeTemplateDeclaration       = "template_declaration"
	cppNodeTemplateParameterList     = "template_parameter_list"
	cppNodeTypeParameterDecl         = "type_parameter_declaration"
	cppNodeVariadicTypeParamDecl     = "variadic_type_parameter_declaration"
	cppNodeOptionalTypeParamDecl     = "optional_type_parameter_declaration"
	cppNodeTemplateTemplateParamDecl = "template_template_parameter_declaration"
	cppNodeParameterDecl             = "parameter_declaration"
	cppNodeOptionalParameterDecl     = "optional_parameter_declaration"
	cppNodeVariadicParameterDecl     = "variadic_parameter_declaration"
	cppNodeDeclarationList           = "declaration_list"
	cppNodeFieldDeclarationList      = "field_declaration_list"
	cppNodeFieldDeclaration          = "field_declaration"
	cppNodeDeclaration               = "declaration"
	cppNodeLinkageSpecification      = "linkage_specification"
	cppNodeIdentifier                = "identifier"
	cppNodeNamespaceIdentifier       = "namespace_identifier"
	// Function-body-bearing nodes. The walker handles
	// `function_definition` by emitting a MethodDecl and
	// then STOPPING recursion into the body, so any
	// function-local class declarations (e.g.
	// `void foo() { class Local {}; }`) do NOT surface as
	// top-level class entries -- they're an implementation
	// detail of the function body and would mislead
	// downstream graph consumers if they appeared as
	// namespace-scope ClassDecls alongside namespace-
	// scope classes. `lambda_expression` and bare
	// `compound_statement` also stop recursion (they're
	// not in scope for v1 method extraction). Only
	// `call_expression` lookups via `cppWalkCalls` ever
	// touch nodes inside a body, and that lookup never
	// invokes `handleClass`. This is the explicit guard
	// against the failure mode the iter-2 evaluator called
	// out: without this, the visit() default branch would
	// recursively walk every node type, eventually
	// reaching a function body's compound_statement and
	// emitting locals with namespace-only qualified names.
	cppNodeFunctionDefinition = "function_definition"
	cppNodeLambdaExpression   = "lambda_expression"
	cppNodeCompoundStatement  = "compound_statement"
	// Preprocessor wrapper node types. tree-sitter-cpp
	// emits the same string name whether the node lives
	// at translation-unit scope or inside a
	// field_declaration_list (see parser.c
	// `sym_preproc_*_in_field_declaration_list` aliases ->
	// the unqualified display name), so a single set of
	// constants drives both the visit() default-branch
	// recursion and the walkClassBody preproc dispatch.
	cppNodePreprocIf      = "preproc_if"
	cppNodePreprocIfdef   = "preproc_ifdef"
	cppNodePreprocElse    = "preproc_else"
	cppNodePreprocElif    = "preproc_elif"
	cppNodePreprocElifdef = "preproc_elifdef"
	// Function/call/include nodes added for the Stage 3.5
	// methods/imports/calls extraction surface.
	cppNodePreprocInclude      = "preproc_include"
	cppNodeSystemLibString     = "system_lib_string"
	cppNodeStringLiteral       = "string_literal"
	cppNodeFunctionDeclarator  = "function_declarator"
	cppNodePointerDeclarator   = "pointer_declarator"
	cppNodeReferenceDeclarator = "reference_declarator"
	cppNodeParenthesizedDecl   = "parenthesized_declarator"
	cppNodeFieldIdentifier     = "field_identifier"
	cppNodeDestructorName      = "destructor_name"
	cppNodeOperatorName        = "operator_name"
	cppNodeCallExpression      = "call_expression"
	cppNodeFieldExpression     = "field_expression"
	cppNodeThisExpression      = "this"
)

// cppSeenClass tracks an already-emitted ClassDecl for
// dedupe purposes. `hasBody` records the AST-based
// determination (from `cppFindBody`) made at the time the
// entry was inserted -- NOT a heuristic derived from line
// numbers. A single-line definition like `class Foo {};`
// has `EndLine == StartLine` but still has a body, so any
// reasoning about whether the previous entry was a forward
// declaration must use this flag rather than re-deriving it
// from the stored ClassDecl's line range.
type cppSeenClass struct {
	idx     int
	hasBody bool
}

type cppWalker struct {
	src     []byte
	classes []ClassDecl
	methods []MethodDecl
	imports []Import
	// seenClasses dedupes a class definition against any
	// preceding forward declaration. The grammar emits
	// `class Foo;` as a `declaration` wrapping a body-less
	// `class_specifier` and the subsequent `class Foo { ...
	// };` as a top-level `class_specifier` with body. We
	// skip the body-less form when an entry already exists,
	// or accept it on its own when no definition follows --
	// the dispatcher tolerates a single ClassDecl per
	// QualifiedName and a forward decl alone still carries
	// useful location info.
	seenClasses map[string]cppSeenClass // qualifiedName -> entry
}

// walkTop dispatches the immediate children of a
// translation_unit. Each child is treated as if it were at
// the global namespace scope: no `container` / `namespace`
// prefix, no inherited template parameters.
func (w *cppWalker) walkTop(root *sitter.Node) {
	if w.seenClasses == nil {
		w.seenClasses = map[string]cppSeenClass{}
	}
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.visit(root.NamedChild(int(i)), "", "", nil)
	}
}

// visit dispatches a node encountered at file / namespace /
// linkage-specification scope. `container` is the dotted
// namespace path accumulated so far (the C++ analogue of the
// TypeScript walker's `outer` argument). `namespace` is the
// same path with any inner-class segments removed -- at
// namespace scope they're identical, but the parameter is
// threaded separately so the walker can populate
// `LangMeta["namespace"]` with JUST the namespace prefix
// when emitting a class (per tech-spec §5.2 "LangMeta keys:
// namespace (string), ...").  `templateParams` is non-nil
// only when the parent of n was a `template_declaration` --
// in that case the params attach to the wrapped class /
// struct via LangMeta.
//
// Unrecognised node types fall through to the `default`
// branch where we recurse into the named children, mirroring
// the TypeScript walker's pattern (parser_treesitter.go
// `visitTopLevel` default branch). That ensures classes
// nested inside preprocessor wrappers -- `preproc_ifdef` /
// `preproc_if` / `preproc_else` / `preproc_elif`, the
// node types tree-sitter-cpp uses for `#ifndef HEADER_H`
// include guards and `#ifdef PLATFORM_X` conditional
// compilation blocks -- and inside `declaration` /
// `attributed_declaration` / `static_assert_declaration`
// wrappers still surface as `ClassDecl`s, instead of being
// silently dropped because the walker only knew about the
// closed set of node types it special-cases.
//
// Body-bearing function nodes (`function_definition`,
// `lambda_expression`, and bare `compound_statement`) are
// explicitly handled: `function_definition` emits a
// MethodDecl (via handleFunctionDefinition) and stops
// recursive descent so local class declarations inside the
// body do NOT surface as namespace-scope ClassDecls;
// `lambda_expression` and bare `compound_statement` simply
// stop the recursion for the same reason (their bodies are
// not in scope for v1 method extraction). The
// `TestTreeSitterCppParser_FunctionLocalClassesSkipped`
// test pins this no-leak invariant.
//
// `preproc_include` at translation-unit / namespace scope
// emits an Import via handleInclude (system headers stripped
// of angle brackets, quoted headers normalized with a
// leading `./` so the dispatcher's relative-include filter
// drops them).
func (w *cppWalker) visit(n *sitter.Node, container, namespace string, templateParams []string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case cppNodeNamespaceDefinition:
		w.handleNamespace(n, container, namespace)
	case cppNodeClassSpecifier, cppNodeStructSpecifier:
		w.handleClass(n, container, namespace, templateParams)
	case cppNodeTemplateDeclaration:
		w.handleTemplateDeclaration(n, container, namespace)
	case cppNodeLinkageSpecification:
		// `extern "C" { ... }` wraps a declaration_list
		// whose children should be visited at the enclosing
		// scope -- the linkage modifier does not contribute
		// a name segment to the QualifiedName.
		body := cppFindBody(n)
		if body != nil {
			for i := uint32(0); i < body.NamedChildCount(); i++ {
				w.visit(body.NamedChild(int(i)), container, namespace, nil)
			}
		}
	case cppNodeFunctionDefinition:
		// Free function (or out-of-line method definition
		// like `void Foo::bar() {}` -- the qualified
		// declarator carries the enclosing class name and
		// `cppExtractMethodIdentifier` recovers it). Emit a
		// MethodDecl and walk the body for calls. Crucially,
		// do NOT call `visit` on the body: that would route
		// any inner `class_specifier` through `handleClass`
		// and surface local types as namespace-scope
		// ClassDecls (the failure mode pinned by
		// `TestTreeSitterCppParser_FunctionLocalClassesSkipped`).
		// `cppWalkCalls` only inspects `call_expression`
		// nodes -- local types are silently walked over.
		w.handleFunctionDefinition(n, "", namespace)
		return
	case cppNodeLambdaExpression,
		cppNodeCompoundStatement:
		// Policy: do NOT descend into lambda bodies or bare
		// compound statements either. Same rationale as
		// function_definition above -- local class declarations
		// inside these scopes must not surface as namespace-
		// scope `ClassDecl`s. Calls inside lambda bodies are
		// not in scope for v1 (no enclosing MethodDecl yet);
		// future work can add a `walkCppCalls(lambda.body)`
		// pass that attributes the calls to the enclosing
		// method's slice.
		return
	case cppNodePreprocInclude:
		// `#include <string>` or `#include "base.h"` at
		// namespace / translation-unit scope. The
		// preprocessor sees these before the C++ parser,
		// but tree-sitter-cpp surfaces them as
		// `preproc_include` siblings of the post-preproc
		// declarations so the AST captures the include
		// statement's source location for downstream graph
		// edges.
		w.handleInclude(n)
		return
	default:
		// Generic recursive descent for every other node
		// type -- includes preproc wrappers
		// (`preproc_ifdef`, `preproc_if`, `preproc_else`,
		// `preproc_elif`, `preproc_elifdef`),
		// `declaration` (which wraps `class Foo;` forward
		// declarations), `attributed_declaration`, and
		// anything else the grammar may evolve to wrap a
		// class/namespace in. The recursive walk is bounded
		// because the only emission sites are the explicit
		// switch cases above; an unknown wrapper merely
		// passes through. Function-body-bearing nodes are
		// stopped by the explicit case above so this
		// recursion can NEVER reach a local class.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			w.visit(n.NamedChild(int(i)), container, namespace, templateParams)
		}
	}
}

// handleNamespace extracts the namespace name (one segment
// per `namespace` keyword, plus any `a::b` nested syntax),
// extends BOTH the dotted `container` path AND the parallel
// `namespace` path (they advance together at namespace
// scope), and walks the namespace body. Anonymous namespaces
// (`namespace { }`) are walked with both paths unchanged --
// their members appear at the enclosing scope as far as the
// QualifiedName is concerned. That trades the (rare)
// possibility of a name collision with a global declaration
// against the simpler invariant "every QualifiedName comes
// from source-visible identifiers".
func (w *cppWalker) handleNamespace(n *sitter.Node, outer, namespace string) {
	qualified := outer
	ns := namespace
	if nameNode := n.ChildByFieldName("name"); nameNode != nil {
		// `namespace foo` -> nameNode is a single
		// namespace_identifier with content "foo".
		// `namespace a::b` -> nameNode is a
		// nested_namespace_specifier whose raw content is
		// "a::b". Either way, splitting on `::` and joining
		// with `.` gives one dotted segment per `namespace`
		// keyword -- matching the brief's
		// `QualifiedName=<namespace+"."+name>` rule and the
		// tech-spec §5.2 `LangMeta["namespace"]` key.
		raw := strings.TrimSpace(nameNode.Content(w.src))
		for _, seg := range strings.Split(raw, "::") {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			if qualified == "" {
				qualified = seg
			} else {
				qualified = qualified + "." + seg
			}
			if ns == "" {
				ns = seg
			} else {
				ns = ns + "." + seg
			}
		}
	}
	body := cppFindBody(n)
	if body != nil {
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			w.visit(body.NamedChild(int(i)), qualified, ns, nil)
		}
	}
}

// handleTemplateDeclaration extracts type parameter names
// from a `template<...>` clause and dispatches to the wrapped
// declaration with those params attached. The wrapped node
// can be a class/struct specifier (the common case for this
// stage), another nested template_declaration (e.g.
// `template<class T> template<class U> ...`), or a function
// (out of scope here -- visit() simply ignores it).
//
// `template_declaration` does not contribute a namespace
// segment, so both `container` and `namespace` are forwarded
// to the wrapped declaration unchanged.
func (w *cppWalker) handleTemplateDeclaration(n *sitter.Node, container, namespace string) {
	var params []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c.Type() == cppNodeTemplateParameterList {
			params = collectCppTemplateParams(c, w.src)
			break
		}
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c.Type() == cppNodeTemplateParameterList {
			continue
		}
		w.visit(c, container, namespace, params)
	}
}

// handleClass emits a ClassDecl for one class_specifier /
// struct_specifier. Forward declarations (no body) are
// emitted once but yield to a later full definition that
// arrives with the same QualifiedName -- the seenClasses
// dedupe table prefers the entry with a body.
//
// `outer` is the dotted prefix for the QualifiedName (it
// includes both namespace path AND any enclosing-class
// path for nested classes). `namespace` is JUST the
// namespace prefix -- it stays stable as the walker
// descends through nested class bodies so that
// `LangMeta["namespace"]` carries the namespace the class
// was declared in, not the dotted chain of enclosing
// classes (tech-spec §5.2 "LangMeta keys: namespace
// (string), ...").
func (w *cppWalker) handleClass(n *sitter.Node, outer, namespace string, templateParams []string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		// Anonymous struct/class (e.g. `typedef struct { ...
		// } X;`) -- there's no QualifiedName to register and
		// the dispatcher can't resolve such a node, so skip.
		return
	}
	name := strings.TrimSpace(nameNode.Content(w.src))
	if name == "" {
		return
	}
	// The name field for a templated specialization
	// (`class Foo<int>`) is a `template_type` whose content
	// is `Foo<int>`. Strip any `<...>` tail so the
	// QualifiedName stays stable across the primary template
	// and its specializations -- otherwise the dispatcher
	// would treat them as unrelated classes.
	name = cppStripTemplateArgs(name)
	// Likewise, `class ns::Foo { ... }` (out-of-namespace
	// definition inside a different scope) presents a
	// qualified_identifier name. Flatten `::` to `.` so the
	// QualifiedName still matches the same `ns.Foo` that an
	// in-namespace declaration would produce.
	name = strings.ReplaceAll(name, "::", ".")

	qualified := name
	if outer != "" {
		qualified = outer + "." + name
	}
	kind := "class"
	if n.Type() == cppNodeStructSpecifier {
		kind = "struct"
	}

	body := cppFindBody(n)
	hasBody := body != nil

	cls := ClassDecl{
		QualifiedName: qualified,
		Kind:          kind,
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
	}

	baseAccess := map[string]string{}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(int(i))
		if child.Type() == cppNodeBaseClassClause {
			cls.Extends = append(cls.Extends,
				collectCppBaseClasses(child, w.src, kind, baseAccess)...)
		}
	}

	// LangMeta is populated when there is anything per-
	// language to record: a non-empty namespace prefix, one
	// or more base-class access entries, or one or more
	// template parameters. A class with none of these (a
	// plain top-level `class Foo {};` with no bases and no
	// `template<>`) leaves the field nil -- the descriptive-
	// not-identifying invariant (parser.go C12) requires the
	// field stay nil when there's nothing to say.
	if namespace != "" || len(baseAccess) > 0 || len(templateParams) > 0 {
		cls.LangMeta = map[string]any{}
		if namespace != "" {
			// Per tech-spec §5.2 line 444 ("LangMeta keys:
			// namespace (string), ...") the namespace
			// prefix is also persisted as a discrete
			// LangMeta value so downstream consumers can
			// route on it without re-parsing the
			// QualifiedName -- the brief still requires the
			// namespace to be in the QualifiedName
			// (<namespace+"."+name>), so this is an
			// ADDITIVE attr, not a replacement.
			cls.LangMeta["namespace"] = namespace
		}
		if len(baseAccess) > 0 {
			cls.LangMeta["base_access"] = baseAccess
		}
		if len(templateParams) > 0 {
			cls.LangMeta["template_params"] = templateParams
		}
	}

	// Dedupe: a forward declaration that ran first leaves
	// an entry in seenClasses; the full definition (with
	// body) wins and overwrites it. A second forward decl
	// after the full def is ignored. A standalone forward
	// decl (no later body) is kept.
	//
	// Both sides of the comparison use the AST-derived
	// `hasBody` flag (the previous one stashed in
	// seenClasses at insertion time) rather than deriving
	// it from line numbers -- a single-line definition like
	// `class Foo {};` has `EndLine == StartLine` and would
	// be misidentified as bodyless by a line-range
	// heuristic.
	if prev, dup := w.seenClasses[qualified]; dup {
		if hasBody && !prev.hasBody {
			w.classes[prev.idx] = cls
			w.seenClasses[qualified] = cppSeenClass{idx: prev.idx, hasBody: true}
		}
		// Whether or not we overwrote, fall through to walk
		// the body so nested classes still surface.
		if hasBody {
			w.walkClassBody(body, qualified, namespace)
		}
		return
	}
	w.seenClasses[qualified] = cppSeenClass{idx: len(w.classes), hasBody: hasBody}
	w.classes = append(w.classes, cls)

	if hasBody {
		w.walkClassBody(body, qualified, namespace)
	}
}

// walkClassBody walks a class / struct body
// (`field_declaration_list`) and emits everything this stage
// owns at the body-internal scope:
//
//   - Nested `class_specifier` / `struct_specifier` declarations
//     (the original v1 responsibility) -- forwarded to
//     `handleClass` with the enclosing class's QualifiedName as
//     the `outer` prefix.
//   - Template-wrapped nested classes
//     (`template_declaration` -> `class_specifier`) -- routed
//     through `handleTemplateDeclaration` so the body's
//     template parameters land on `LangMeta["template_params"]`.
//   - `field_declaration`-wrapped forward declarations
//     (`class Inner;`) -- one-level descent so the forward decl
//     still produces a `ClassDecl` even without an inline body.
//   - **In-class inline method definitions**
//     (`function_definition` inside the class body, e.g.
//     `class Foo { void bar() {} }`) -- emitted via
//     `handleFunctionDefinition` with the enclosing class's
//     QualifiedName forwarded as `enclosingClass`. The resulting
//     MethodDecl carries `EnclosingClass = "<qualified>"` and
//     `QualifiedName = "<qualified>.<methodName>"`, which is
//     what dispatcher Pass 1b uses to wire the `contains` edge
//     from class node to method node, and what Pass 2b's
//     receiver-qualified `this->foo()` resolver indexes against.
//   - Header-internal `#include` directives (`preproc_include`
//     inside a class body -- rare but legal in injection-style
//     headers) -- surfaced at the file level via
//     `handleInclude`, identical treatment to a top-level
//     include.
//
// `qualified` is the enclosing class's QualifiedName, used
// as the `outer` prefix for any nested class emitted here AND
// as the `enclosingClass` arg for inline methods. `namespace`
// is forwarded UNCHANGED -- a nested class / method lives in
// the same namespace as its enclosing class, so
// `LangMeta["namespace"]` should carry the enclosing
// namespace, not `enclosingClass.qualified`.
//
// Preprocessor wrappers inside the class body
// (`preproc_ifdef`, `preproc_if`, `preproc_else`,
// `preproc_elif`, `preproc_elifdef`) are descended through
// so members declared inside `#ifdef PLATFORM` /
// `#ifndef GUARD` regions still surface; the grammar uses
// the same string names for the in-field-declaration-list
// variants of these nodes (see parser.c
// `sym_preproc_ifdef_in_field_declaration_list` ->
// `"preproc_ifdef"`).
//
// Bodyless member declarations (`field_declaration` of the
// `void api();` shape, i.e. a method DECLARATION without a
// body) are intentionally NOT emitted as MethodDecls --
// `function_definition` is the only path. This keeps
// in-header forward declarations from inflating the
// MethodDecl count on translation units that also see the
// implementation, and is what the
// `TestTreeSitterCppParser_FunctionLocalClassesSkipped`
// invariant relies on.
func (w *cppWalker) walkClassBody(body *sitter.Node, qualified, namespace string) {
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		member := body.NamedChild(int(i))
		switch member.Type() {
		case cppNodeClassSpecifier, cppNodeStructSpecifier:
			w.handleClass(member, qualified, namespace, nil)
		case cppNodeTemplateDeclaration:
			w.handleTemplateDeclaration(member, qualified, namespace)
		case cppNodeFieldDeclaration:
			// A nested class declared without inline body
			// (e.g. `class Inner;`) lives inside a
			// field_declaration wrapper -- descend one
			// level so the forward decl still produces a
			// ClassDecl.
			for j := uint32(0); j < member.NamedChildCount(); j++ {
				c := member.NamedChild(int(j))
				if c.Type() == cppNodeClassSpecifier || c.Type() == cppNodeStructSpecifier {
					w.handleClass(c, qualified, namespace, nil)
				}
			}
		case cppNodeFunctionDefinition:
			// In-class inline method definition (`class
			// Foo { void bar() {} }`). The enclosing
			// class's QualifiedName becomes the method's
			// EnclosingClass; the method's QualifiedName
			// is `<EnclosingClass>.<name>`.
			w.handleFunctionDefinition(member, qualified, namespace)
		case cppNodePreprocInclude:
			// A `#include` inside a class body is rare but
			// legal (header injection patterns). Surface
			// it at the file level just like a top-level
			// include.
			w.handleInclude(member)
		case cppNodePreprocIf,
			cppNodePreprocIfdef,
			cppNodePreprocElse,
			cppNodePreprocElif,
			cppNodePreprocElifdef:
			// Preproc wrapper inside a class body -- the
			// children are themselves field-declaration-
			// list-shaped, so we recurse with the same
			// (qualified, namespace) pair.
			w.walkClassBody(member, qualified, namespace)
		}
	}
}

// collectCppBaseClasses walks one base_class_clause node and
// returns the ordered list of normalized base names. Each
// base's access specifier (explicit or language default) is
// recorded in baseAccess.
//
// The grammar lays out a base_class_clause as a flat sequence
// of named children -- per ancestor an optional
// `access_specifier`, an optional `virtual_specifier`, and
// exactly one type-reference node (`type_identifier`,
// `qualified_identifier`, `qualified_type_identifier`,
// `template_type`, or `dependent_type_identifier`). Commas
// between bases appear as anonymous children and are
// invisible to the named-child walk -- we instead reset the
// accumulated access to the language default each time we
// commit a base.
//
// Base names are normalized to match `QualifiedName`
// conventions: `::` -> `.`, template arguments stripped,
// leading `.` (from a top-level `::Foo`) removed. That keeps
// the dispatcher's same-file `extends` resolver from
// failing on `class D : public ns::Base<T>` purely because
// of cosmetic differences in how the parser captured the
// name vs how the target class registered its qualified
// name.
func collectCppBaseClasses(clause *sitter.Node, src []byte, kind string, baseAccess map[string]string) []string {
	var bases []string
	defaultAccess := "private"
	if kind == "struct" {
		defaultAccess = "public"
	}
	currentAccess := defaultAccess
	for i := uint32(0); i < clause.NamedChildCount(); i++ {
		c := clause.NamedChild(int(i))
		switch c.Type() {
		case cppNodeAccessSpecifier:
			// `public:` inside a class body carries a
			// trailing colon, but in a base_class_clause
			// the keyword stands alone; strip defensively.
			currentAccess = strings.TrimSuffix(strings.TrimSpace(c.Content(src)), ":")
		case cppNodeTypeIdentifier,
			cppNodeQualifiedIdentifier,
			cppNodeQualifiedTypeIdentifier,
			cppNodeTemplateType,
			cppNodeDependentTypeIdentifier:
			rawName := strings.TrimSpace(c.Content(src))
			name := cppNormalizeBaseName(rawName)
			if name == "" {
				continue
			}
			bases = append(bases, name)
			baseAccess[name] = currentAccess
			// Reset for the next ancestor; a missing
			// access on that ancestor means the language
			// default applies (private for class, public
			// for struct).
			currentAccess = defaultAccess
		}
	}
	return bases
}

// cppNormalizeBaseName converts a raw base-class name as
// captured by tree-sitter into the QualifiedName-compatible
// form the dispatcher can match against same-file class
// registrations:
//
//   - `ns::Base`        -> `ns.Base`
//   - `ns::Base<T>`     -> `ns.Base`     (template args dropped)
//   - `::Base`          -> `Base`        (leading `::` from
//     a top-level qualifier is structural noise here)
//   - `typename T::U`   -> `T.U`         (dependent type;
//     `typename` keyword stripped)
//
// The function is defensive about whitespace and stops at
// the first `<` so nested template arguments (`A<B<C>>`)
// don't confuse the strip.
func cppNormalizeBaseName(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "typename ") {
		s = strings.TrimSpace(s[len("typename "):])
	}
	s = cppStripTemplateArgs(s)
	s = strings.TrimPrefix(s, "::")
	s = strings.ReplaceAll(s, "::", ".")
	return s
}

// cppStripTemplateArgs returns s with the first `<` and
// everything after it removed. Tree-sitter captures a
// `template_type` as `<base>` text followed by the
// `<args>` -- we want just the base name for QualifiedName
// purposes. Safe on names that contain no `<`.
func cppStripTemplateArgs(s string) string {
	if idx := strings.IndexByte(s, '<'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}

// collectCppTemplateParams returns the declared names of the
// type and non-type parameters in a `template_parameter_list`.
// Source order is preserved so the metadata reflects the
// declaration. Unnamed parameters (`template<typename>`,
// rare but legal) are skipped rather than producing an empty
// string -- empty entries would surface as bogus identifiers
// downstream.
//
// Default values (`template<class T = Foo>`) are intentionally
// not included: only the parameter's own declared name
// contributes.
func collectCppTemplateParams(list *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < list.NamedChildCount(); i++ {
		p := list.NamedChild(int(i))
		switch p.Type() {
		case cppNodeTypeParameterDecl,
			cppNodeVariadicTypeParamDecl,
			cppNodeOptionalTypeParamDecl,
			cppNodeTemplateTemplateParamDecl:
			// Type parameter -- the declared name is the
			// first type_identifier descendant (after the
			// `typename` / `class` keyword token, which
			// is an anonymous child invisible to a named-
			// child walk).
			if name := cppFirstIdent(p, src); name != "" {
				out = append(out, name)
			}
		case cppNodeParameterDecl,
			cppNodeOptionalParameterDecl,
			cppNodeVariadicParameterDecl:
			// Non-type parameter (`int N`, `size_t S`)
			// -- the declarator field carries the
			// identifier.
			if d := p.ChildByFieldName("declarator"); d != nil {
				if name := cppFirstIdent(d, src); name != "" {
					out = append(out, name)
				}
			} else if name := cppFirstIdent(p, src); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// cppFirstIdent returns the text of the first `identifier`
// or `type_identifier` descendant of n in document order, or
// the empty string if no such descendant exists. Used for
// pulling parameter names out of declarator subtrees without
// hard-coding the grammar's exact wrapper layout.
func cppFirstIdent(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	if t := n.Type(); t == cppNodeIdentifier || t == cppNodeTypeIdentifier {
		return strings.TrimSpace(n.Content(src))
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if s := cppFirstIdent(n.NamedChild(int(i)), src); s != "" {
			return s
		}
	}
	return ""
}

// cppFindBody returns n's `body` field child, falling back
// to the first `declaration_list` / `field_declaration_list`
// named child if the field lookup misses. The grammar exposes
// `body:` on class_specifier, struct_specifier,
// namespace_definition, and linkage_specification, so the
// field lookup usually succeeds -- the fallback covers
// grammar variants the binding may have evolved away from.
func cppFindBody(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	if b := n.ChildByFieldName("body"); b != nil {
		return b
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		switch c.Type() {
		case cppNodeDeclarationList, cppNodeFieldDeclarationList:
			return c
		}
	}
	return nil
}

// handleFunctionDefinition emits a MethodDecl for one
// `function_definition` node. `enclosingClass` is the
// QualifiedName of the surrounding class (empty string for
// free functions at namespace / file scope). `namespace` is
// the dotted namespace prefix; it is currently unused at the
// MethodDecl level (kept for future LangMeta["namespace"]
// attribution) but the parameter is wired so the call sites
// don't need to special-case "where am I" knowledge.
//
// The function handles three shapes:
//
//   - Inline method definition (`class Foo { void bar() {} }`):
//     `enclosingClass="Foo"`; declarator unwraps to a bare
//     `field_identifier` / `identifier` -> name="bar"; final
//     QualifiedName="Foo.bar", EnclosingClass="Foo".
//   - Out-of-line method definition (`void Foo::bar() {}` at
//     namespace scope): `enclosingClass=""` from the visit()
//     caller, but the declarator unwraps to a
//     `qualified_identifier` whose scope reveals "Foo" and
//     name "bar" -> final EnclosingClass="Foo",
//     QualifiedName="Foo.bar". This lets the dispatcher
//     resolve same-file calls against the inline-defined
//     class.
//   - Free function (`void log_global() {}`):
//     `enclosingClass=""` and the declarator unwraps to an
//     `identifier` -> name="log_global"; final
//     EnclosingClass="" and QualifiedName="log_global".
//
// Body-relative line/byte ranges are recorded so the
// dispatcher's `SubdivideMethod` can emit Block boundaries
// in file-relative coordinates (parser.go MethodDecl
// BodyStartLine / BodyEndLine / BodyStartByte / BodyEndByte
// docs). The body source is also captured verbatim into
// `BodySource` so the embedding-publish path has content
// to vectorise (per dispatcher.go §9.6a publish hook).
//
// Calls and ReceiverCalls are populated from
// `cppWalkCalls(body)`; both lists are deduped in insertion
// order, matching the contract in parser.go MethodDecl.Calls
// ("Order is the source order of the first occurrence of
// each call target; duplicates are removed").
func (w *cppWalker) handleFunctionDefinition(n *sitter.Node, enclosingClass, namespace string) {
	_ = namespace // reserved for future LangMeta["namespace"] attribution.
	if n == nil {
		return
	}
	declRoot := n.ChildByFieldName("declarator")
	if declRoot == nil {
		return
	}
	extractedClass, methodName := cppExtractMethodIdentifier(declRoot, w.src)
	if methodName == "" {
		return
	}
	finalEnclosing := enclosingClass
	if extractedClass != "" {
		// Out-of-line definition (`void Foo::bar() {}`) --
		// the declarator's qualifier wins over whatever
		// scope the visit caller passed in. The caller at
		// namespace scope passes enclosingClass="", so this
		// is the only way EnclosingClass gets populated for
		// out-of-line definitions.
		finalEnclosing = extractedClass
	}
	qn := methodName
	if finalEnclosing != "" {
		qn = finalEnclosing + "." + methodName
	}
	body := n.ChildByFieldName("body")
	if body == nil {
		// Defensive fallback: scan for the first
		// compound_statement child if the field lookup
		// misses (grammar variants).
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c.Type() == cppNodeCompoundStatement {
				body = c
				break
			}
		}
	}
	calls, receiverCalls := cppWalkCalls(body, w.src)
	method := MethodDecl{
		QualifiedName:  qn,
		EnclosingClass: finalEnclosing,
		ParamSignature: cppExtractParamSignature(declRoot, w.src),
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
		Calls:          calls,
		ReceiverCalls:  receiverCalls,
	}
	if body != nil {
		method.BodyStartLine = int(body.StartPoint().Row) + 1
		method.BodyEndLine = int(body.EndPoint().Row) + 1
		method.BodyStartByte = int(body.StartByte())
		method.BodyEndByte = int(body.EndByte())
		if method.BodyEndByte > method.BodyStartByte && method.BodyEndByte <= len(w.src) {
			method.BodySource = string(w.src[method.BodyStartByte:method.BodyEndByte])
		}
	}
	w.methods = append(w.methods, method)
}

// cppExtractMethodIdentifier walks a declarator subtree and
// returns (extractedClass, methodName). The walker unwraps
// the common declarator wrappers (pointer / reference /
// parenthesized / function_declarator) and then matches the
// final name-bearing node:
//
//   - `identifier` / `field_identifier` -> ("", name)
//   - `qualified_identifier`            -> (scope, name)
//   - `destructor_name`                 -> ("", "~name")
//   - `operator_name`                   -> ("", raw text)
//
// Anything else returns ("", "") and the caller skips the
// declaration. Tree-sitter-cpp wraps the declarator inside a
// `function_declarator` whose own `declarator` field carries
// the name-bearing node; the unwrap chain below handles that
// indirection generically.
func cppExtractMethodIdentifier(decl *sitter.Node, src []byte) (string, string) {
	if decl == nil {
		return "", ""
	}
	cur := decl
	// Unwrap up to a few levels of declarator wrappers. The
	// bound prevents pathological infinite loops if a future
	// grammar revision introduces a cycle (it shouldn't, but
	// the bound is cheap insurance).
	for i := 0; i < 6; i++ {
		switch cur.Type() {
		case cppNodeFunctionDeclarator,
			cppNodePointerDeclarator,
			cppNodeReferenceDeclarator,
			cppNodeParenthesizedDecl:
			inner := cur.ChildByFieldName("declarator")
			if inner == nil {
				return "", ""
			}
			cur = inner
			continue
		}
		break
	}
	switch cur.Type() {
	case cppNodeIdentifier, cppNodeFieldIdentifier:
		return "", strings.TrimSpace(cur.Content(src))
	case cppNodeQualifiedIdentifier:
		// `Foo::bar` -> scope="Foo", name="bar".
		// `ns::Foo::bar` -> scope chain "ns::Foo" -> "ns.Foo".
		// The `name` child may itself be a
		// qualified_identifier; recurse via this helper to
		// flatten the chain.
		nameNode := cur.ChildByFieldName("name")
		scopeNode := cur.ChildByFieldName("scope")
		var scope, name string
		if nameNode != nil {
			subScope, subName := cppExtractMethodIdentifier(nameNode, src)
			if subScope != "" {
				if scope == "" {
					scope = subScope
				} else {
					scope = scope + "." + subScope
				}
			}
			name = subName
		}
		if scopeNode != nil {
			raw := strings.TrimSpace(scopeNode.Content(src))
			raw = strings.ReplaceAll(raw, "::", ".")
			raw = strings.TrimSuffix(raw, ".")
			if raw != "" {
				if scope == "" {
					scope = raw
				} else {
					scope = raw + "." + scope
				}
			}
		}
		return scope, name
	case cppNodeDestructorName, cppNodeOperatorName:
		return "", strings.TrimSpace(cur.Content(src))
	}
	return "", ""
}

// cppExtractParamSignature returns the raw parameter list
// text from a function_declarator (or any declarator wrapping
// one). The `(...)` wrapping is trimmed so the result
// matches the existing parsers' convention -- consumers that
// need the parenthesised form can re-add them.
func cppExtractParamSignature(decl *sitter.Node, src []byte) string {
	if decl == nil {
		return ""
	}
	cur := decl
	for i := 0; i < 6; i++ {
		if cur.Type() == cppNodeFunctionDeclarator {
			break
		}
		inner := cur.ChildByFieldName("declarator")
		if inner == nil {
			return ""
		}
		cur = inner
	}
	if cur.Type() != cppNodeFunctionDeclarator {
		return ""
	}
	params := cur.ChildByFieldName("parameters")
	if params == nil {
		return ""
	}
	return trimParens(strings.TrimSpace(params.Content(src)))
}

// cppWalkCalls visits every `call_expression` descendant of
// `body` and returns the per-method (Calls, ReceiverCalls)
// pair the dispatcher consumes for static_calls edge
// emission. Both slices are deduped in insertion order so
// `helper(); helper();` produces a single entry per
// parser.go MethodDecl.Calls / ReceiverCalls semantics.
//
// Classification rules:
//
//   - `function` field is an `identifier`  -> bare call,
//     appended to Calls.
//   - `function` field is a `field_expression` whose
//     `argument` is the `this` keyword AND whose `field` is
//     a `field_identifier` / `identifier` -> receiver-
//     qualified call, the field's text appended to
//     ReceiverCalls. We accept BOTH `argument.Type() ==
//     "this"` and `Content(src) == "this"` to be robust
//     against grammar revisions that re-shape the `this`
//     expression.
//
// All other call shapes (selector-style `obj.foo()`,
// pointer-arrow on a non-`this` operand, template
// instantiations of free functions, std-qualified calls
// like `std::move(x)`) are intentionally NOT routed
// anywhere -- they're out of scope for this v1 surface and
// would require cross-file resolution to emit accurate
// edges.
//
// IMPORTANT: bare-call classification ONLY accepts a direct
// `identifier` as the `function` field. We MUST NOT recurse
// to find "the first identifier under function", because
// that would mis-route `this->identify()` (a field_expression
// whose deepest identifier is `identify`) into the bare-call
// slice and the dispatcher would emit a wrong edge.
func cppWalkCalls(body *sitter.Node, src []byte) (calls, receiverCalls []string) {
	if body == nil {
		return nil, nil
	}
	seenCalls := map[string]struct{}{}
	seenRecv := map[string]struct{}{}
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != cppNodeCallExpression {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil {
			return true
		}
		switch fn.Type() {
		case cppNodeIdentifier:
			name := strings.TrimSpace(fn.Content(src))
			if name == "" {
				return true
			}
			if _, dup := seenCalls[name]; dup {
				return true
			}
			seenCalls[name] = struct{}{}
			calls = append(calls, name)
		case cppNodeFieldExpression:
			arg := fn.ChildByFieldName("argument")
			field := fn.ChildByFieldName("field")
			if arg == nil || field == nil {
				return true
			}
			// Detect `this` defensively: the upstream
			// grammar emits a dedicated `this` node type
			// for the keyword, but a future revision may
			// wrap it differently -- the content fallback
			// catches that case without breaking the
			// existing-shape happy path.
			if arg.Type() != cppNodeThisExpression &&
				strings.TrimSpace(arg.Content(src)) != "this" {
				return true
			}
			if field.Type() != cppNodeFieldIdentifier && field.Type() != cppNodeIdentifier {
				return true
			}
			name := strings.TrimSpace(field.Content(src))
			if name == "" {
				return true
			}
			if _, dup := seenRecv[name]; dup {
				return true
			}
			seenRecv[name] = struct{}{}
			receiverCalls = append(receiverCalls, name)
		}
		return true
	})
	return calls, receiverCalls
}

// handleInclude emits an Import for one `preproc_include`
// node. The grammar exposes the include path via the `path`
// field; we fall back to scanning for a `system_lib_string`
// or `string_literal` child if the field lookup misses
// (grammar variants).
//
// Normalization:
//
//   - `system_lib_string` (`<string>`)   -> Module="string"
//     (angle brackets stripped).
//   - `string_literal`    (`"base.h"`)   -> Module="./base.h"
//     (quotes stripped; `./` prepended unless the path is
//     already relative-marked or absolute). The leading
//     `./` is the marker the dispatcher's relative-include
//     filter (`isRelativeImport`) keys on so local headers
//     don't materialise external-package nodes -- see
//     implementation-plan.md Stage 3.2 step 206.
//
// Includes whose path cannot be decoded (empty or
// non-string node) are silently dropped: the parser
// contract requires malformed entries to be omitted rather
// than surfaced as bogus edges.
func (w *cppWalker) handleInclude(n *sitter.Node) {
	if n == nil {
		return
	}
	pathNode := n.ChildByFieldName("path")
	if pathNode == nil {
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c.Type() == cppNodeSystemLibString || c.Type() == cppNodeStringLiteral {
				pathNode = c
				break
			}
		}
	}
	if pathNode == nil {
		return
	}
	raw := strings.TrimSpace(pathNode.Content(w.src))
	if raw == "" {
		return
	}
	var module string
	switch pathNode.Type() {
	case cppNodeSystemLibString:
		// `<foo/bar.h>` -> `foo/bar.h`.
		s := strings.TrimPrefix(raw, "<")
		s = strings.TrimSuffix(s, ">")
		module = strings.TrimSpace(s)
	case cppNodeStringLiteral:
		// `"foo.h"` -> `./foo.h` (relative-include marker).
		s := strings.TrimPrefix(raw, "\"")
		s = strings.TrimSuffix(s, "\"")
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		switch {
		case strings.HasPrefix(s, "./"),
			strings.HasPrefix(s, "../"),
			strings.HasPrefix(s, "/"):
			module = s
		default:
			module = "./" + s
		}
	default:
		// Unknown path child shape -- preserve the raw
		// captured text so the dispatcher at least logs a
		// meaningful module name rather than dropping the
		// include silently.
		module = raw
	}
	if module == "" {
		return
	}
	w.imports = append(w.imports, Import{
		Module: module,
		Line:   int(n.StartPoint().Row) + 1,
	})
}
