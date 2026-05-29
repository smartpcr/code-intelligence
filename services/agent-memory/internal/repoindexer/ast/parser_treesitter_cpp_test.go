//go:build cgo

package ast

import (
	"sort"
	"strings"
	"testing"
)

// TestTreeSitterCppParser_LanguageAndExtensions verifies the
// parser advertises the canonical C++ language id and the
// extension set the workstream brief enumerates. The
// dispatcher routes files by extension, so any drift here
// silently re-routes (or fails to route) real source files.
func TestTreeSitterCppParser_LanguageAndExtensions(t *testing.T) {
	p := NewTreeSitterCppParser()
	if got := p.Language(); got != "cpp" {
		t.Fatalf("Language: want cpp, got %q", got)
	}
	want := []string{".cc", ".cpp", ".cxx", ".c++", ".hpp", ".hh", ".hxx", ".h++"}
	got := append([]string(nil), p.Extensions()...)
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if len(got) != len(sortedWant) {
		t.Fatalf("Extensions: want %v, got %v", sortedWant, got)
	}
	for i := range got {
		if got[i] != sortedWant[i] {
			t.Fatalf("Extensions[%d]: want %q, got %q (full want=%v got=%v)",
				i, sortedWant[i], got[i], sortedWant, got)
		}
	}
}

// TestTreeSitterCppParser_PlainClassAndStruct asserts that
// the walker emits one ClassDecl per `class` / `struct`
// keyword with the correct Kind, QualifiedName (no
// namespace prefix at file scope), and no LangMeta when
// there are no bases and no template params.
func TestTreeSitterCppParser_PlainClassAndStruct(t *testing.T) {
	const src = `
class Greeter {
public:
  void hello();
};

struct Point {
  int x;
  int y;
};
`
	p := NewTreeSitterCppParser()
	res, err := p.Parse("src/plain.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Classes) != 2 {
		t.Fatalf("want 2 classes; got %d (%+v)", len(res.Classes), res.Classes)
	}
	byName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		byName[c.QualifiedName] = c
	}
	if g, ok := byName["Greeter"]; !ok || g.Kind != "class" {
		t.Errorf("Greeter: ok=%v kind=%q (full=%+v)", ok, g.Kind, g)
	} else if g.LangMeta != nil {
		t.Errorf("Greeter should have nil LangMeta when no bases/templates; got %+v", g.LangMeta)
	}
	if pt, ok := byName["Point"]; !ok || pt.Kind != "struct" {
		t.Errorf("Point: ok=%v kind=%q (full=%+v)", ok, pt.Kind, pt)
	}
}

// TestTreeSitterCppParser_BaseClassesWithAccess covers the
// multi-base inheritance case the brief specifies: each base
// is captured into Extends, and LangMeta["base_access"]
// records the access specifier per base (with the language
// default applied when the keyword is omitted).
func TestTreeSitterCppParser_BaseClassesWithAccess(t *testing.T) {
	const src = `
class Base1 {};
class Base2 {};
class Base3 {};

class Derived : public Base1, private Base2, Base3 {
public:
  Derived();
};

struct SDerived : Base1, protected Base2 {
};
`
	p := NewTreeSitterCppParser()
	res, err := p.Parse("src/inherit.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		byName[c.QualifiedName] = c
	}

	derived, ok := byName["Derived"]
	if !ok {
		t.Fatalf("missing Derived; got %v", classNames(res.Classes))
	}
	wantExtends := []string{"Base1", "Base2", "Base3"}
	if !stringSlicesEqualOrdered(derived.Extends, wantExtends) {
		t.Errorf("Derived.Extends: want %v, got %v", wantExtends, derived.Extends)
	}
	if derived.LangMeta == nil {
		t.Fatalf("Derived.LangMeta should be populated; got nil")
	}
	access, ok := derived.LangMeta["base_access"].(map[string]string)
	if !ok {
		t.Fatalf("Derived.LangMeta[base_access] should be map[string]string; got %T (%+v)",
			derived.LangMeta["base_access"], derived.LangMeta["base_access"])
	}
	if access["Base1"] != "public" {
		t.Errorf("Base1 access: want public, got %q", access["Base1"])
	}
	if access["Base2"] != "private" {
		t.Errorf("Base2 access: want private, got %q", access["Base2"])
	}
	// Base3 has no access specifier; default for a class is private.
	if access["Base3"] != "private" {
		t.Errorf("Base3 access: want private (class default), got %q", access["Base3"])
	}

	sderived, ok := byName["SDerived"]
	if !ok {
		t.Fatalf("missing SDerived; got %v", classNames(res.Classes))
	}
	saccess, ok := sderived.LangMeta["base_access"].(map[string]string)
	if !ok {
		t.Fatalf("SDerived.LangMeta[base_access] should be map[string]string; got %T",
			sderived.LangMeta["base_access"])
	}
	// Base1 has no access specifier; default for a struct is public.
	if saccess["Base1"] != "public" {
		t.Errorf("SDerived Base1 access: want public (struct default), got %q", saccess["Base1"])
	}
	if saccess["Base2"] != "protected" {
		t.Errorf("SDerived Base2 access: want protected, got %q", saccess["Base2"])
	}
}

// TestTreeSitterCppParser_QualifiedAndTemplatedBases asserts
// the base-name normalization rules: namespaced bases lose
// the `::` separator (replaced with `.`) and template
// arguments are dropped so the QualifiedName conventions
// match what the dispatcher's same-file resolver expects.
func TestTreeSitterCppParser_QualifiedAndTemplatedBases(t *testing.T) {
	const src = `
namespace ns {
  class Base {};
}

template<typename T>
class Container {};

class Derived : public ns::Base, public Container<int> {
};
`
	p := NewTreeSitterCppParser()
	res, err := p.Parse("src/qbase.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		byName[c.QualifiedName] = c
	}
	d, ok := byName["Derived"]
	if !ok {
		t.Fatalf("missing Derived; got %v", classNames(res.Classes))
	}
	wantExtends := []string{"ns.Base", "Container"}
	if !stringSlicesEqualOrdered(d.Extends, wantExtends) {
		t.Errorf("Derived.Extends: want %v, got %v", wantExtends, d.Extends)
	}
	access, _ := d.LangMeta["base_access"].(map[string]string)
	if access["ns.Base"] != "public" {
		t.Errorf("base_access[ns.Base]: want public, got %q (full=%+v)", access["ns.Base"], access)
	}
	if access["Container"] != "public" {
		t.Errorf("base_access[Container]: want public, got %q (full=%+v)", access["Container"], access)
	}
}

// TestTreeSitterCppParser_NamespaceQualifiedNames covers the
// container-accumulating walk required by the brief: classes
// declared inside one or more `namespace` blocks gain a
// dotted prefix per namespace keyword. The test also asserts
// that each class records the same prefix in
// `LangMeta["namespace"]` per tech-spec §5.2 line 444
// ("LangMeta keys: namespace (string), ...") -- a discrete
// attr separate from the QualifiedName so downstream
// consumers can route on it without re-parsing.
func TestTreeSitterCppParser_NamespaceQualifiedNames(t *testing.T) {
	const src = `
namespace alpha {
  class Outer {};

  namespace beta {
    class Inner {};
  }
}

namespace gamma::delta {
  class Bridge {};
}
`
	p := NewTreeSitterCppParser()
	res, err := p.Parse("src/ns.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		byName[c.QualifiedName] = c
	}
	wantNamespace := map[string]string{
		"alpha.Outer":        "alpha",
		"alpha.beta.Inner":   "alpha.beta",
		"gamma.delta.Bridge": "gamma.delta",
	}
	for qn, wantNS := range wantNamespace {
		c, ok := byName[qn]
		if !ok {
			t.Errorf("missing class %q; got %v", qn, classNames(res.Classes))
			continue
		}
		if c.LangMeta == nil {
			t.Errorf("%s.LangMeta should be populated with namespace=%q; got nil", qn, wantNS)
			continue
		}
		gotNS, _ := c.LangMeta["namespace"].(string)
		if gotNS != wantNS {
			t.Errorf("%s.LangMeta[namespace]: want %q, got %q (full=%+v)",
				qn, wantNS, gotNS, c.LangMeta)
		}
	}
}

// TestTreeSitterCppParser_NestedClassInsideClass asserts a
// class declared inside another class body inherits the
// outer's QualifiedName as a dotted prefix -- consistent
// with the namespace-prefix rule (and with the TS walker's
// nested-class behaviour the brief tells us to mirror).
//
// The test also asserts that nested classes record
// `LangMeta["namespace"]` as the ENCLOSING NAMESPACE
// (not the dotted enclosing-class chain): per tech-spec
// §5.2 the `namespace` LangMeta attr describes the C++
// namespace, not the qualified parent-name, so a class
// declared inside `ns::Outer` must record `namespace=="ns"`.
func TestTreeSitterCppParser_NestedClassInsideClass(t *testing.T) {
	const src = `
namespace ns {
  class Outer {
  public:
    class Inner {};
    struct Tag {};
  };
}
`
	p := NewTreeSitterCppParser()
	res, err := p.Parse("src/nested.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		byName[c.QualifiedName] = c
	}
	if c := byName["ns.Outer"]; c.Kind != "class" {
		t.Errorf("ns.Outer: want class, got %q (full=%v)", c.Kind, byName)
	}
	if c := byName["ns.Outer.Inner"]; c.Kind != "class" {
		t.Errorf("ns.Outer.Inner: want class, got %q (full=%v)", c.Kind, byName)
	}
	if c := byName["ns.Outer.Tag"]; c.Kind != "struct" {
		t.Errorf("ns.Outer.Tag: want struct, got %q (full=%v)", c.Kind, byName)
	}
	// All three classes live in namespace "ns" -- even the
	// nested Inner/Tag, since the LangMeta["namespace"]
	// attr describes the enclosing C++ namespace, not the
	// dotted chain of enclosing classes.
	for _, qn := range []string{"ns.Outer", "ns.Outer.Inner", "ns.Outer.Tag"} {
		c, ok := byName[qn]
		if !ok {
			t.Errorf("missing %q", qn)
			continue
		}
		if c.LangMeta == nil {
			t.Errorf("%s.LangMeta should carry namespace=ns; got nil", qn)
			continue
		}
		if ns, _ := c.LangMeta["namespace"].(string); ns != "ns" {
			t.Errorf("%s.LangMeta[namespace]: want %q, got %q (full=%+v)",
				qn, "ns", ns, c.LangMeta)
		}
	}
}

// TestTreeSitterCppParser_TemplateParams covers the
// LangMeta["template_params"] population for templated
// classes -- both single-parameter and multi-parameter
// (including non-type params) cases, and the negative case
// where a non-templated class leaves the key out.
func TestTreeSitterCppParser_TemplateParams(t *testing.T) {
	const src = `
template<typename T>
class Holder {
};

template<class K, class V, int N>
class Cache {
};

class Plain {};
`
	p := NewTreeSitterCppParser()
	res, err := p.Parse("src/tpl.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		byName[c.QualifiedName] = c
	}

	holder, ok := byName["Holder"]
	if !ok {
		t.Fatalf("missing Holder; got %v", classNames(res.Classes))
	}
	if holder.LangMeta == nil {
		t.Fatalf("Holder.LangMeta should be populated for templated class; got nil")
	}
	tp, ok := holder.LangMeta["template_params"].([]string)
	if !ok {
		t.Fatalf("Holder.LangMeta[template_params] should be []string; got %T (%+v)",
			holder.LangMeta["template_params"], holder.LangMeta["template_params"])
	}
	if !stringSlicesEqualOrdered(tp, []string{"T"}) {
		t.Errorf("Holder template_params: want [T], got %v", tp)
	}

	cache, ok := byName["Cache"]
	if !ok {
		t.Fatalf("missing Cache; got %v", classNames(res.Classes))
	}
	ctp, _ := cache.LangMeta["template_params"].([]string)
	if !stringSlicesEqualOrdered(ctp, []string{"K", "V", "N"}) {
		t.Errorf("Cache template_params: want [K V N], got %v", ctp)
	}

	plain, ok := byName["Plain"]
	if !ok {
		t.Fatalf("missing Plain")
	}
	if plain.LangMeta != nil {
		// A non-templated class with no bases must NOT
		// populate LangMeta -- the descriptive-not-
		// identifying invariant (parser.go C12) requires
		// the field stay nil when there's nothing to say.
		if _, hasTP := plain.LangMeta["template_params"]; hasTP {
			t.Errorf("Plain should have no template_params; got %+v", plain.LangMeta)
		}
	}
}

// TestTreeSitterCppParser_ForwardDeclarationDeduped asserts
// that a `class Foo;` forward declaration is replaced (not
// duplicated) when the full definition arrives later in the
// same translation unit.
func TestTreeSitterCppParser_ForwardDeclarationDeduped(t *testing.T) {
	const src = `
class Foo;

class Foo {
public:
  void hello();
};
`
	p := NewTreeSitterCppParser()
	res, err := p.Parse("src/fwd.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	count := 0
	for _, c := range res.Classes {
		if c.QualifiedName == "Foo" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("Foo should appear exactly once after forward-decl + def; got %d (full=%+v)",
			count, res.Classes)
	}
}

// TestTreeSitterCppParser_LineNumbers asserts the StartLine /
// EndLine fields are 1-based and frame the actual span of
// the class_specifier (not just the keyword line). Downstream
// span resolution depends on these offsets being accurate.
func TestTreeSitterCppParser_LineNumbers(t *testing.T) {
	const src = "// line1\n" +
		"\n" +
		"class Greeter {\n" +
		"public:\n" +
		"  void hello();\n" +
		"};\n"
	p := NewTreeSitterCppParser()
	res, err := p.Parse("src/lines.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Classes) != 1 {
		t.Fatalf("want 1 class, got %d", len(res.Classes))
	}
	g := res.Classes[0]
	if g.StartLine != 3 {
		t.Errorf("Greeter.StartLine: want 3, got %d", g.StartLine)
	}
	if g.EndLine != 6 {
		t.Errorf("Greeter.EndLine: want 6, got %d", g.EndLine)
	}
}

// TestTreeSitterCppParser_PreprocWrappers exercises the
// `default:` recursive-descent branch in `visit()` and the
// preproc switch arm in `walkClassBody()`. Header files
// almost always wrap their contents in include guards
// (`#ifndef FOO_H` / `#define FOO_H` / ... / `#endif`),
// and platform code routinely uses `#ifdef WINDOWS` / `#else`
// to switch between alternate class definitions; neither
// case can be silently dropped or the dispatcher would
// produce empty graphs for every header in the repo.
//
// tree-sitter-cpp does NOT evaluate the preprocessor -- it
// parses BOTH branches of an `#ifdef ... #else ... #endif`
// into the AST, so both class definitions in that case are
// expected to surface.
func TestTreeSitterCppParser_PreprocWrappers(t *testing.T) {
	t.Run("include guard", func(t *testing.T) {
		const src = `
#ifndef GUARDED_H
#define GUARDED_H

class GuardedClass {
public:
  void hello();
};

struct GuardedStruct {
  int x;
};

#endif
`
		p := NewTreeSitterCppParser()
		res, err := p.Parse("src/guarded.hpp", []byte(src))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		byName := map[string]ClassDecl{}
		for _, c := range res.Classes {
			byName[c.QualifiedName] = c
		}
		if c, ok := byName["GuardedClass"]; !ok || c.Kind != "class" {
			t.Errorf("GuardedClass should surface from inside #ifndef include guard; got %v",
				classNames(res.Classes))
		}
		if c, ok := byName["GuardedStruct"]; !ok || c.Kind != "struct" {
			t.Errorf("GuardedStruct should surface from inside #ifndef include guard; got %v",
				classNames(res.Classes))
		}
	})

	t.Run("ifdef else both branches", func(t *testing.T) {
		const src = `
#ifdef PLATFORM_WINDOWS
class PlatformImpl {
public:
  void winApi();
};
#else
class PlatformImpl {
public:
  void posixApi();
};
#endif

#ifdef HAS_ALPHA
class AlphaOnly {};
#endif

#ifdef HAS_BETA
class BetaOnly {};
#else
class BetaFallback {};
#endif
`
		p := NewTreeSitterCppParser()
		res, err := p.Parse("src/platform.cpp", []byte(src))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		got := map[string]int{}
		for _, c := range res.Classes {
			got[c.QualifiedName]++
		}
		// tree-sitter does not evaluate the preprocessor;
		// both branches of `#ifdef PLATFORM_WINDOWS / #else`
		// parse and yield the same QualifiedName. The
		// forward-decl dedupe in handleClass keeps just one
		// entry (whichever variant wins is fine -- the
		// dispatcher only needs a single ClassDecl per
		// QualifiedName).
		if got["PlatformImpl"] == 0 {
			t.Errorf("PlatformImpl should surface from at least one branch of #ifdef/#else; got %v",
				classNames(res.Classes))
		}
		if got["AlphaOnly"] == 0 {
			t.Errorf("AlphaOnly should surface from #ifdef-only block; got %v",
				classNames(res.Classes))
		}
		if got["BetaOnly"] == 0 {
			t.Errorf("BetaOnly should surface from #ifdef branch; got %v",
				classNames(res.Classes))
		}
		if got["BetaFallback"] == 0 {
			t.Errorf("BetaFallback should surface from #else branch; got %v",
				classNames(res.Classes))
		}
	})

	t.Run("preproc inside class body", func(t *testing.T) {
		const src = `
class Outer {
public:
  class AlwaysInner {};
#ifdef ENABLE_FEATURE
  class FeatureInner {};
#endif
#ifndef DISABLE_OPTIONAL
  struct OptionalTag {};
#endif
};
`
		p := NewTreeSitterCppParser()
		res, err := p.Parse("src/inner.cpp", []byte(src))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		got := map[string]string{}
		for _, c := range res.Classes {
			got[c.QualifiedName] = c.Kind
		}
		if got["Outer"] != "class" {
			t.Errorf("Outer: want class, got %q (full=%v)", got["Outer"], got)
		}
		if got["Outer.AlwaysInner"] != "class" {
			t.Errorf("Outer.AlwaysInner: want class, got %q (full=%v)", got["Outer.AlwaysInner"], got)
		}
		if got["Outer.FeatureInner"] != "class" {
			t.Errorf("Outer.FeatureInner should surface from #ifdef inside class body; got %q (full=%v)",
				got["Outer.FeatureInner"], got)
		}
		if got["Outer.OptionalTag"] != "struct" {
			t.Errorf("Outer.OptionalTag should surface from #ifndef inside class body; got %q (full=%v)",
				got["Outer.OptionalTag"], got)
		}
	})

	t.Run("preproc inside namespace", func(t *testing.T) {
		const src = `
namespace ns {
#ifndef GUARD_H
#define GUARD_H
  class Bar {};
#endif
}
`
		p := NewTreeSitterCppParser()
		res, err := p.Parse("src/nspreproc.hpp", []byte(src))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		byName := map[string]ClassDecl{}
		for _, c := range res.Classes {
			byName[c.QualifiedName] = c
		}
		bar, ok := byName["ns.Bar"]
		if !ok {
			t.Fatalf("ns.Bar should surface from #ifndef inside namespace body; got %v",
				classNames(res.Classes))
		}
		if bar.LangMeta == nil {
			t.Fatalf("ns.Bar.LangMeta should be populated with namespace=ns; got nil")
		}
		if got, _ := bar.LangMeta["namespace"].(string); got != "ns" {
			t.Errorf("ns.Bar.LangMeta[namespace]: want %q, got %q (full=%+v)",
				"ns", got, bar.LangMeta)
		}
	})
}

// TestTreeSitterCppParser_FunctionLocalClassesSkipped is the
// policy test the iter-2 evaluator asked for: confirms that
// classes declared inside a function body (free function or
// namespaced free function) do NOT surface as namespace-
// scope `ClassDecl`s. Without the explicit
// `function_definition` / `lambda_expression` /
// `compound_statement` skip cases in `visit()`, the default
// recursive descent (added in iter 2 to handle preprocessor
// wrappers) would walk into function bodies and produce
// bogus entries like `ns.Local` for any local class.
//
// The sibling C++ methods workstream owns function-body
// extraction; this stage's `ClassDecl` list is
// type-declarations-only. Both the `MustEmit` assertions
// (top-level types still surface) AND the `MustNotEmit`
// assertions (function-local types are filtered out) are
// load-bearing -- the fix must keep the wanted classes while
// suppressing the unwanted ones.
func TestTreeSitterCppParser_FunctionLocalClassesSkipped(t *testing.T) {
	const src = `
namespace ns {

class Visible {
public:
  void api();
};

void freeFunction() {
  class LocalInFree {};
  struct TagInFree {};
}

class Outer {
public:
  void method() {
    class LocalInMethod {};
  }
};

}

void globalFreeFn() {
  class GlobalLocal {};
}
`
	p := NewTreeSitterCppParser()
	res, err := p.Parse("src/local.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]bool{}
	for _, c := range res.Classes {
		got[c.QualifiedName] = true
	}

	mustEmit := []string{"ns.Visible", "ns.Outer"}
	for _, qn := range mustEmit {
		if !got[qn] {
			t.Errorf("missing expected class %q; got %v", qn, classNames(res.Classes))
		}
	}

	// The forbidden set: function-local classes (free fn,
	// method body, namespaced free fn, and a global free
	// fn for good measure). Each should be filtered by the
	// function-body skip in visit(). If any appears, the
	// dispatcher would route a bogus extends/contains edge
	// against a namespace-qualified name that no other
	// translation unit can resolve.
	mustNotEmit := []string{
		"ns.LocalInFree",
		"ns.TagInFree",
		"ns.LocalInMethod",
		"ns.Outer.LocalInMethod",
		"LocalInFree",
		"TagInFree",
		"LocalInMethod",
		"GlobalLocal",
		"ns.GlobalLocal",
	}
	for _, qn := range mustNotEmit {
		if got[qn] {
			t.Errorf("function-local class %q should NOT surface as a namespace-scope ClassDecl; full list=%v",
				qn, classNames(res.Classes))
		}
	}
}

// classNames helper lives in parser_powershell_test.go (no
// build tag) so it is always available; redeclaring it here
// would duplicate-define under `//go:build cgo`.

// stringSlicesEqualOrdered returns true when a and b contain
// the same strings in the same order. Used to assert
// source-order invariants the brief requires (e.g. Extends
// preserves declaration order).
func stringSlicesEqualOrdered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTreeSitterCppParser_RegisteredInDefaultParsers verifies
// the parser is reachable through `defaultParsers()` (the
// factory parsers_cgo.go::defaultParsers wires into the
// dispatcher). Without this assertion, the parser could be
// implemented but silently unreachable -- the
// implemented-but-unreachable failure mode previously flagged
// by the rubber-duck review for the C tree-sitter parser. This
// test is the C++ twin of
// TestTreeSitterCParser_RegisteredInDefaultParsers and pins
// the `parsers_cgo.go` registration line so a future edit
// that drops `NewTreeSitterCppParser()` from
// `defaultParsers()` fails loudly here instead of silently
// regressing `.cc` / `.cpp` / `.cxx` / `.c++` / `.hpp` /
// `.hh` / `.hxx` / `.h++` ingestion to "no parser registered
// for extension" skips.
//
// The test runs on the CGO build path only (the file's
// `//go:build cgo` tag); the CGO=off counterpart in
// parsers_nocgo.go intentionally does NOT register C++ (per
// architecture Section 3 -- ".c / .cpp / .cs / .go / .rs are
// SKIPPED by the dispatcher" under CGO=off), so a parallel
// CGO=off assertion would contradict the documented
// "tree-sitter-backed only" stance.
//
// `.h` is intentionally NOT in the want-set: it is claimed by
// the C parser per the pinned `dot-h-extension-routing` rule
// (tech-spec Section 8 R6) and parser_treesitter_cpp.go:51-58
// explicitly documents that the C++ parser does not register
// `.h` because the dispatcher's `buildExtMap` overwrite rule
// would otherwise non-deterministically re-route `.h` files
// when registration order drifts. See parsers_cgo.go for the
// documented registration order rationale.
func TestTreeSitterCppParser_RegisteredInDefaultParsers(t *testing.T) {
	parsers := defaultParsers()
	want := map[string]bool{
		".cc":  false,
		".cpp": false,
		".cxx": false,
		".c++": false,
		".hpp": false,
		".hh":  false,
		".hxx": false,
		".h++": false,
	}
	for _, p := range parsers {
		if p.Language() != "cpp" {
			continue
		}
		for _, ext := range p.Extensions() {
			if _, ok := want[ext]; ok {
				want[ext] = true
			}
		}
	}
	for ext, found := range want {
		if !found {
			t.Errorf("defaultParsers() does not register the C++ parser for %q; saw %d parsers", ext, len(parsers))
		}
	}
}

// TestDefaultParsers_CBeforeCpp pins the registration order
// documented in parsers_cgo.go so a future edit that swaps
// the C and C++ entries (or moves either past the other)
// fails loudly here. The order matters because:
//
//   - `buildExtMap` (dispatcher.go) iterates `defaultParsers()`
//     in order and LATER entries overwrite EARLIER ones for
//     any shared extension. Today `.h` is claimed only by C,
//     but a future revision of parser_treesitter_cpp.go that
//     accidentally adds `.h` to its Extensions would silently
//     re-route `.h` files to the C++ grammar.
//
//   - The cross-language dispatcher test
//     `TestDispatcher_DotHRoutesToC_EvenWithCppHint`
//     (implementation-plan.md line 444, added in a later
//     workstream) relies on the C parser being registered.
//     Pinning the order here keeps that later test
//     deterministic across any edits to this slice.
//
// The check is "position of C < position of C++" rather than
// "exact slice equality" so this test does not break every
// time a new language (Go, Rust, ...) is appended.
func TestDefaultParsers_CBeforeCpp(t *testing.T) {
	parsers := defaultParsers()
	cIdx, cppIdx := -1, -1
	for i, p := range parsers {
		switch p.Language() {
		case "c":
			if cIdx == -1 {
				cIdx = i
			}
		case "cpp":
			if cppIdx == -1 {
				cppIdx = i
			}
		}
	}
	if cIdx == -1 {
		t.Fatalf("defaultParsers() does not include the C parser; saw %d parsers", len(parsers))
	}
	if cppIdx == -1 {
		t.Fatalf("defaultParsers() does not include the C++ parser; saw %d parsers", len(parsers))
	}
	if cIdx >= cppIdx {
		t.Errorf("defaultParsers() ordering: C at index %d must come BEFORE C++ at index %d (see parsers_cgo.go documented order)", cIdx, cppIdx)
	}
}

// TestCppFixture_EmitsExpectedNodeAndEdgeSet pins the
// Stage 3.5 (Cpp-fixture-test) acceptance contract for the C++
// tree-sitter parser. The fixture is a small, deliberately
// canonical C++ file that exercises every grammar surface the
// workstream brief calls out:
//
//   - `#include <string>` -- a system header include (angle-
//     bracket form). The dispatcher later materialises this as
//     a synthetic external-package node + a `file -> string`
//     `imports` edge.
//   - `#include "base.h"` -- a quoted (relative) header include.
//     The parser records it as `./base.h` (relative-include
//     marker per implementation-plan.md Stage 3.2 step 206);
//     the dispatcher then DROPS it from edge emission per the
//     relative-include filter (mirrors the C-fixture-test
//     "relative include dropped" scenario in Stage 3.4).
//   - `class Base { public: void identify() {} }` -- a single
//     class containing one in-line method definition. The
//     parser emits `Base` (ClassDecl) and `Base.identify`
//     (MethodDecl with EnclosingClass=Base).
//   - `class Greeter : public Base { ... }` -- a derived class
//     extending `Base` via a `public` access specifier. The
//     parser populates `Greeter.Extends=["Base"]` and
//     `Greeter.LangMeta["base_access"]["Base"]=="public"`.
//   - `Greeter.greet()` body -- contains TWO call sites:
//   - `log_global()` -- a bare-name call to a same-file free
//     function (declared AFTER greet's body, so the parser
//     cannot rely on declaration order; the dispatcher's
//     Pass 2a same-file callee index is built before Pass 2b
//     resolution runs). Lands in
//     `Greeter.greet.Calls=["log_global"]`.
//   - `this->identify()` -- a receiver-qualified call. At the
//     parser level this lands in
//     `Greeter.greet.ReceiverCalls=["identify"]` (the
//     dispatcher persists the union of Calls + ReceiverCalls
//     on `attrs_json["calls_raw"]` for the future cross-file
//     resolver). The dispatcher's Pass 2b receiver-qualified
//     resolver then looks for
//     `methodNodeID["Greeter.identify"]`, MISSES because
//     `identify` is declared only on `Base` in this fixture
//     (different class, even though same file), and CORRECTLY
//     DROPS the edge per A4 ("memory store prefers missing
//     edges over wrong ones"). The brief therefore pins
//     "exactly 1 static_calls edge" -- the `log_global` one
//     survives Pass 2b; the `identify` one is dropped. At the
//     parser level we enforce the COMPOSITION that produces
//     that one-edge outcome: Calls has exactly ["log_global"]
//     (the surviving edge's source) AND ReceiverCalls has
//     exactly ["identify"] (the dropped edge's verbatim name,
//     preserved on calls_raw for the future cross-file
//     resolver).
//   - `void log_global() {}` -- a free function declared at
//     file scope, so the parser emits `MethodDecl{
//     QualifiedName: "log_global", EnclosingClass: ""}`.
//
// This test runs at the PARSER level (//go:build cgo,
// mirroring TestCSharpFixture_EmitsExpectedNodeAndEdgeSet in
// parser_treesitter_csharp_test.go) rather than at the
// dispatcher level (//go:build canonical_dispatcher, as used
// by TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet). The
// brief's "edge" terminology maps to ParseResult fields the
// dispatcher consumes:
//
//   - "extends edge"           -> ClassDecl.Extends
//   - "static_calls edge"      -> MethodDecl.Calls (bare-name
//     entries the dispatcher's Pass 2a/2b resolver stitches
//     against the same-file callee index) -- in this fixture
//     exactly ONE entry, `log_global`, survives Pass 2b.
//   - "calls_raw" attr         -> persisted from the union of
//     MethodDecl.Calls + ReceiverCalls (dispatcher-level
//     concern; the parser supplies the verbatim slices that
//     fold into the attr). Hard-asserted here on
//     Greeter.greet.{Calls, ReceiverCalls} -- if either slice
//     drops an entry, the corresponding calls_raw attr loses
//     it too.
//   - "imports edge"           -> ParseResult.Imports (the
//     dispatcher mints the synthetic package node + the
//     `imports` edge in Pass 1 / 2a; relative includes are
//     dropped from edge emission via the `./`-prefix marker
//     `handleInclude` writes for quoted forms).
//
// All subtests assert HARD CONTRACTS. There are no
// skip-guards: the parser surfaces required by the brief
// (classes, extends, base_access, methods, calls, imports)
// are all populated by parser_treesitter_cpp.go and any
// regression that empties one of those surfaces fails the
// corresponding subtest loudly.
func TestCppFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	const src = `#include <string>
#include "base.h"

class Base {
public:
    void identify() {}
};

class Greeter : public Base {
public:
    void greet() {
        this->identify();
        log_global();
    }
};

void log_global() {}
`
	parser := NewTreeSitterCppParser()
	if got := parser.Language(); got != "cpp" {
		t.Fatalf("Language() = %q; want %q", got, "cpp")
	}

	res, err := parser.Parse("src/hello.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// ----- Classes (supported today) -----
	// The brief pins exactly 2 class nodes: `Base` and
	// `Greeter`. Both are declared at file scope with no
	// enclosing namespace, so the QualifiedName is just the
	// simple name. Suffix-tolerant lookups guard against a
	// future walker that adds a synthetic file-scope or
	// translation-unit prefix.
	t.Run("class nodes", func(t *testing.T) {
		for _, want := range []string{"Base", "Greeter"} {
			if _, ok := findCppFixtureClass(res.Classes, want); !ok {
				t.Errorf("class %q missing from emitted set; got %v",
					want, classNames(res.Classes))
			}
		}
		if got := len(res.Classes); got != 2 {
			t.Errorf("expected 2 class nodes (Base, Greeter); got %d (%v)",
				got, classNames(res.Classes))
		}
		// Kind: both should be `"class"` (not `"struct"`).
		// A walker that emits `Base` with Kind="struct" from
		// the C++ `class_specifier` would silently mis-route
		// the dispatcher's struct-vs-class downstream policy.
		for _, want := range []string{"Base", "Greeter"} {
			c, ok := findCppFixtureClass(res.Classes, want)
			if !ok {
				continue
			}
			if c.Kind != "class" {
				t.Errorf("%s.Kind = %q; want %q", want, c.Kind, "class")
			}
		}
	})

	// ----- Extends edge (Greeter -> Base) (supported today) -----
	// `class Greeter : public Base` populates Greeter.Extends
	// with a single entry, `Base`. The dispatcher's same-file
	// resolver matches the entry against the Base ClassDecl
	// and emits one `extends` edge.
	t.Run("extends edge Greeter to Base", func(t *testing.T) {
		greeter, ok := findCppFixtureClass(res.Classes, "Greeter")
		if !ok {
			t.Fatalf("Greeter class missing; got %v", classNames(res.Classes))
		}
		if !cppFixtureHasSuffix(greeter.Extends, "Base") {
			t.Errorf("Greeter.Extends should contain Base; got %v", greeter.Extends)
		}
		if got := len(greeter.Extends); got != 1 {
			t.Errorf("expected exactly 1 extends entry on Greeter (-> Base); got %d (%v)",
				got, greeter.Extends)
		}
	})

	// ----- LangMeta["base_access"]["Base"] == "public" (supported today) -----
	// Stage 3.5 implementation-plan step 253 calls out this
	// attr explicitly. The C++ access specifier (`public` in
	// `: public Base`) feeds the LangMeta["base_access"] map
	// per parser_treesitter_cpp.go::collectCppBaseClasses;
	// downstream consumers (graph queries, policy rules)
	// route on the access specifier without re-parsing the
	// raw extends list.
	t.Run("base_access attrs", func(t *testing.T) {
		greeter, ok := findCppFixtureClass(res.Classes, "Greeter")
		if !ok {
			t.Fatalf("Greeter class missing; got %v", classNames(res.Classes))
		}
		if greeter.LangMeta == nil {
			t.Fatalf("Greeter.LangMeta should be populated with base_access; got nil")
		}
		access, ok := greeter.LangMeta["base_access"].(map[string]string)
		if !ok {
			t.Fatalf("Greeter.LangMeta[base_access] should be map[string]string; got %T (%+v)",
				greeter.LangMeta["base_access"], greeter.LangMeta["base_access"])
		}
		if access["Base"] != "public" {
			t.Errorf("base_access[Base]: want %q, got %q (full=%+v)",
				"public", access["Base"], access)
		}
	})

	// ----- Method nodes -----
	// The brief pins exactly 3 method nodes:
	//   - `Base.identify`   (in-class method definition,
	//                        EnclosingClass=Base)
	//   - `Greeter.greet`   (in-class method definition,
	//                        EnclosingClass=Greeter)
	//   - `log_global`      (file-scope free function,
	//                        EnclosingClass="")
	//
	// Hard contract: an empty Methods slice fails the
	// subtest. A 4th method (e.g. accidentally picking up
	// the in-class method declaration twice, or routing a
	// nested local class function through the walker) also
	// fails. The walker's correctness on the
	// `field_declaration` (bodyless `void api();` in
	// `TestTreeSitterCppParser_FunctionLocalClassesSkipped`)
	// is exercised indirectly here: a walker that emitted
	// methods for bodyless declarations would inflate the
	// count and fail the `!= 3` check on this fixture
	// (which has no bodyless declarations).
	t.Run("method nodes", func(t *testing.T) {
		for _, want := range []string{
			"Base.identify",
			"Greeter.greet",
			"log_global",
		} {
			if _, ok := findCppFixtureMethod(res.Methods, want); !ok {
				t.Errorf("method %q missing; got %v",
					want, methodNames(res.Methods))
			}
		}
		if got := len(res.Methods); got != 3 {
			t.Errorf("expected exactly 3 method nodes (Base.identify, Greeter.greet, log_global); got %d (%v)",
				got, methodNames(res.Methods))
		}
		// Verify EnclosingClass routing -- this is what the
		// dispatcher's Pass 1b uses to pick the method's
		// parent node when wiring the `contains` edge.
		expectEnclosing := map[string]string{
			"Base.identify": "Base",
			"Greeter.greet": "Greeter",
			"log_global":    "",
		}
		for qn, wantEnc := range expectEnclosing {
			m, ok := findCppFixtureMethod(res.Methods, qn)
			if !ok {
				continue
			}
			if m.EnclosingClass != wantEnc {
				t.Errorf("%s.EnclosingClass = %q; want %q", qn, m.EnclosingClass, wantEnc)
			}
		}
	})

	// ----- static_calls slice + receiver-call slice (Pass 2b drop semantics) -----
	// Greeter.greet's body has TWO call sites:
	//   - `log_global()`         -> Calls=["log_global"]
	//   - `this->identify()`     -> ReceiverCalls=["identify"]
	//
	// Downstream the dispatcher's Pass 2b receiver-qualified
	// resolver looks for `methodNodeID["Greeter.identify"]`.
	// In this fixture `identify` is declared only on `Base`
	// (a DIFFERENT class even though same file), so the
	// lookup MISSES and the edge is CORRECTLY DROPPED per
	// A4 ("memory store prefers missing edges over wrong
	// ones"). The brief therefore pins exactly 1 surviving
	// `static_calls` edge: Greeter.greet -> log_global. The
	// verbatim `identify` name still persists on
	// `attrs_json["calls_raw"]` (the dispatcher persists
	// `Calls ∪ ReceiverCalls` there per parser.go
	// MethodDecl.Calls / ReceiverCalls semantics) so the
	// future cross-file resolver can stitch it up.
	//
	// At the parser level we hard-enforce the COMPOSITION
	// that produces the one-edge outcome:
	//   - Calls         == ["log_global"]   (exact, len 1)
	//   - ReceiverCalls == ["identify"]     (exact, len 1)
	//
	// A walker that mis-routed `this->identify()` into
	// `Calls` (e.g. by greedily extracting "the first
	// identifier under the function field") would inflate
	// Calls to ["identify", "log_global"] and the wrong
	// edge `greet -> identify` would survive Pass 2b --
	// this subtest catches that regression on the
	// cardinality check.
	t.Run("static_calls and receiver calls", func(t *testing.T) {
		greet, ok := findCppFixtureMethod(res.Methods, "Greeter.greet")
		if !ok {
			t.Fatalf("Greeter.greet method missing; got %v", methodNames(res.Methods))
		}
		// Bare-name call to log_global -- the one that
		// survives Pass 2b and becomes the single
		// static_calls edge per the brief.
		if !cppFixtureHasSuffix(greet.Calls, "log_global") {
			t.Errorf("Greeter.greet.Calls should contain log_global; got %v", greet.Calls)
		}
		if got := len(greet.Calls); got != 1 {
			t.Errorf("expected exactly 1 entry in Greeter.greet.Calls (log_global only -- this->identify() must NOT route here, or Pass 2b would emit the wrong edge); got %d (%v)",
				got, greet.Calls)
		}
		// Receiver-qualified call `this->identify()` --
		// captured for future cross-file resolution (the
		// dispatcher persists this on attrs_json["calls_raw"]
		// alongside Calls) but dropped by Pass 2b because
		// Greeter itself does not declare `identify`.
		if !cppFixtureHasSuffix(greet.ReceiverCalls, "identify") {
			t.Errorf("Greeter.greet.ReceiverCalls should contain identify (verbatim name persists on attrs_json[calls_raw] for the future cross-file resolver); got %v",
				greet.ReceiverCalls)
		}
		if got := len(greet.ReceiverCalls); got != 1 {
			t.Errorf("expected exactly 1 entry in Greeter.greet.ReceiverCalls (identify); got %d (%v)",
				got, greet.ReceiverCalls)
		}
		// Cross-check that the other methods (Base.identify
		// and log_global) have empty Calls/ReceiverCalls --
		// their bodies are empty, so any non-empty slice
		// here would signal walker drift (e.g. accidentally
		// inheriting calls from a sibling method).
		for _, qn := range []string{"Base.identify", "log_global"} {
			m, ok := findCppFixtureMethod(res.Methods, qn)
			if !ok {
				continue
			}
			if len(m.Calls) != 0 {
				t.Errorf("%s.Calls should be empty (body is `{}`); got %v", qn, m.Calls)
			}
			if len(m.ReceiverCalls) != 0 {
				t.Errorf("%s.ReceiverCalls should be empty (body is `{}`); got %v", qn, m.ReceiverCalls)
			}
		}
	})

	// ----- Imports -----
	// The fixture declares two `#include`s:
	//   - `#include <string>`   -- angle-bracket / system
	//                              header. The parser records
	//                              `Module="string"`. The
	//                              dispatcher materialises
	//                              this as the one surviving
	//                              `imports` edge.
	//   - `#include "base.h"`   -- quoted / local header. Per
	//                              implementation-plan.md
	//                              Stage 3.2 step 206
	//                              ("preproc_include ... \"...\"
	//                              -> {Module:\"./\"+<path>}")
	//                              the parser prefixes the
	//                              module with `./` so the
	//                              dispatcher's
	//                              `isRelativeImport` filter
	//                              drops it before edge
	//                              emission. The Import record
	//                              itself still lands in
	//                              ParseResult.Imports.
	//
	// Hard contract: exactly 2 imports, with the exact
	// Module strings above. A regression that drops the `./`
	// prefix on the quoted form would silently turn it into
	// an external-package edge -- catch that here.
	t.Run("imports", func(t *testing.T) {
		if got := len(res.Imports); got != 2 {
			t.Errorf("expected exactly 2 imports (<string>, \"base.h\"); got %d (%v)",
				got, importModules(res.Imports))
		}
		imports := importModules(res.Imports)
		if !containsString(imports, "string") {
			t.Errorf("expected system include <string> recorded as Module=\"string\"; got %v", imports)
		}
		if !containsString(imports, "./base.h") {
			t.Errorf("expected quoted include \"base.h\" recorded as Module=\"./base.h\" (relative-include marker per Stage 3.2 step 206); got %v", imports)
		}
	})
}

// findCppFixtureClass returns the ClassDecl whose
// QualifiedName equals want OR ends with "." + want. The
// fixture declares classes at file scope (no enclosing
// namespace), so an exact match is the expected path today;
// the suffix-tolerant fallback guards against a future walker
// that adds a synthetic translation-unit or file prefix.
//
// Distinct from the file's existing `classNames` helper (a
// formatter for error messages) and from
// findCSharpClass / findGoClass in sibling //go:build cgo
// test files (which embed the language-specific naming
// convention in the helper name). The `cppFixture` prefix
// keeps this fixture-specific lookup separable from the
// other suffix-tolerant helpers in the package -- and from
// any future repo-wide simple-name lookup that may want a
// stricter contract.
func findCppFixtureClass(classes []ClassDecl, want string) (ClassDecl, bool) {
	for _, c := range classes {
		if c.QualifiedName == want || strings.HasSuffix(c.QualifiedName, "."+want) {
			return c, true
		}
	}
	return ClassDecl{}, false
}

// findCppFixtureMethod returns the MethodDecl whose
// QualifiedName equals want OR ends with "." + want. `want`
// is expected to be in `Class.Method` form (e.g.
// `Greeter.greet`) or a bare free-function name (e.g.
// `log_global`); the suffix match accepts a future
// namespace-qualified emission such as `ns.Greeter.greet`.
func findCppFixtureMethod(methods []MethodDecl, want string) (MethodDecl, bool) {
	for _, m := range methods {
		if m.QualifiedName == want || strings.HasSuffix(m.QualifiedName, "."+want) {
			return m, true
		}
	}
	return MethodDecl{}, false
}

// cppFixtureHasSuffix reports whether s contains an element
// that equals want or ends with "." + want. Used to assert
// extends / Calls / ReceiverCalls entries tolerantly of any
// namespace prefix the walker may attach.
func cppFixtureHasSuffix(s []string, want string) bool {
	for _, v := range s {
		if v == want || strings.HasSuffix(v, "."+want) {
			return true
		}
	}
	return false
}