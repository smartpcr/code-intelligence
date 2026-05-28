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
//
// Methods, free functions, calls, and includes are out of
// scope here; sibling stages own them. The walker still
// returns `ParseResult{Classes, Methods, Imports}` so the
// dispatcher's two-pass insert protocol is satisfied -- the
// `Methods` and `Imports` slices are simply empty for now.

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
)

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
	seenClasses map[string]int // qualifiedName -> index in w.classes
}

// walkTop dispatches the immediate children of a
// translation_unit. Each child is treated as if it were at
// the global namespace scope: no `container` / `namespace`
// prefix, no inherited template parameters.
func (w *cppWalker) walkTop(root *sitter.Node) {
	if w.seenClasses == nil {
		w.seenClasses = map[string]int{}
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
		// passes through.
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
	if idx, dup := w.seenClasses[qualified]; dup {
		prev := w.classes[idx]
		prevHadBody := prev.EndLine > prev.StartLine
		if hasBody && !prevHadBody {
			w.classes[idx] = cls
		}
		// Whether or not we overwrote, fall through to walk
		// the body so nested classes still surface.
		if hasBody {
			w.walkClassBody(body, qualified, namespace)
		}
		return
	}
	w.seenClasses[qualified] = len(w.classes)
	w.classes = append(w.classes, cls)

	if hasBody {
		w.walkClassBody(body, qualified, namespace)
	}
}

// walkClassBody walks the field_declaration_list looking for
// nested class / struct definitions and template-wrapped
// nested classes. Member methods and fields are NOT emitted
// here -- the C++ methods workstream owns that surface; the
// brief for this stage is class-only.
//
// `qualified` is the enclosing class's QualifiedName, used
// as the `outer` prefix for any nested class emitted here.
// `namespace` is forwarded UNCHANGED -- a nested class
// lives in the same namespace as its enclosing class, so
// `LangMeta["namespace"]` should carry the enclosing
// namespace, not `enclosingClass.qualified`.
//
// Preprocessor wrappers inside the class body
// (`preproc_ifdef`, `preproc_if`, `preproc_else`,
// `preproc_elif`, `preproc_elifdef`) are descended through
// so nested classes inside `#ifdef PLATFORM` /
// `#ifndef GUARD` regions still surface; the grammar uses
// the same string names for the in-field-declaration-list
// variants of these nodes (see parser.c
// `sym_preproc_ifdef_in_field_declaration_list` ->
// `"preproc_ifdef"`).
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
