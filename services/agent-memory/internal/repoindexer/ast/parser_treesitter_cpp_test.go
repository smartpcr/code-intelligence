//go:build cgo

package ast

import (
	"sort"
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

// classNames is a small helper for error messages -- it
// keeps test failure output readable without bloating each
// assertion with formatting boilerplate.
func classNames(in []ClassDecl) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, c.QualifiedName)
	}
	return out
}

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

// TestCppFixture_EmitsExpectedNodeAndEdgeSet pins the Stage
// 3.5 (Cpp-fixture-test) acceptance contract for the C++
// tree-sitter parser per
// docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/
// implementation-plan.md Stage 3.5. The fixture is the
// canonical snippet the spec spells out verbatim, exercising
// the four grammar surfaces the C++ stages must cover:
//
//   - two `class_specifier` declarations (`Base`, `Greeter`)
//     with single-base public inheritance -- so the parser
//     proves it emits two ClassDecl entries, records
//     `Greeter.Extends == ["Base"]` (the parser-level input
//     the dispatcher uses to mint the `extends` edge), and
//     records `Greeter.LangMeta["base_access"]["Base"] ==
//     "public"` (the descriptive LangMeta the dispatcher
//     folds into `attrs_json["base_access"]`),
//   - three method-bearing declarations (`Base.identify`
//     inline in the class body, `Greeter.greet` inline in
//     the class body, and the free function `log_global`) --
//     so the parser proves it emits three MethodDecls with
//     the dotted `<EnclosingClass>.<name>` QualifiedName
//     convention for members and the bare name for free
//     functions, the latter with `EnclosingClass == ""`
//     (the parser-level signal the dispatcher uses to mint
//     the `file -> method` contains edge instead of
//     `class -> method`),
//   - one bare-identifier call `log_global()` inside
//     `Greeter.greet` -- so the parser records it in
//     `greet.Calls` (the dispatcher's Pass 2b same-file
//     callee index resolves the bare name to the
//     `log_global` method node and mints the single
//     `static_calls` edge the spec pins), and
//   - one receiver-qualified call `this->identify()` --
//     so the parser records it in `greet.ReceiverCalls`
//     (the dispatcher's Pass 2b receiver-qualified path
//     attempts the lookup against
//     `methodNodeID["Greeter.identify"]`; because
//     `identify` is declared only on `Base` (a different
//     class in the same file), the lookup misses and the
//     edge is correctly DROPPED per architecture invariant
//     A4 -- prefer missing edges over wrong ones. The
//     verbatim callee name "identify" persists on the
//     dispatcher's `attrs_json["calls_raw"]` for the future
//     cross-file resolver to retry; the parser-level proxy
//     for that persistence contract is that ReceiverCalls
//     captures the bare name verbatim, which is the only
//     input the dispatcher ever sees).
//
// The test exercises the parser DIRECTLY via `parser.Parse`,
// mirroring TestCFixture_EmitsExpectedNodeAndEdgeSet in
// parser_treesitter_c_test.go (the C analog the spec calls
// out as the immediate sibling stage). The dispatcher-level
// `calls_raw` persistence and the edge-drop on the
// receiver-qualified miss are exercised by the dispatcher
// pipeline's own tests (dispatcher_pass2bd_test.go and the
// canonical_dispatcher-gated EmitFile tests); this file's
// scope is the parser-level INPUT contract that those
// dispatcher tests downstream consume. The "edge" and
// "node" terminology in the spec maps to ParseResult fields
// the dispatcher reads:
//
//   - "contains edge"      -> file-level decl (ClassDecl, or
//     MethodDecl with EnclosingClass
//     == ""); the dispatcher mints
//     one `contains` edge per
//     file-level decl in Pass 1
//   - "extends edge"       -> ClassDecl.Extends entry; the
//     dispatcher mints one `extends`
//     edge per Extends entry in
//     Pass 2a after class resolution
//   - "static_calls edge"  -> MethodDecl.Calls (bare-name
//     entries that the dispatcher's
//     Pass 2b resolver stitches
//     against the same-file callee
//     index)
//   - "imports edge"       -> ParseResult.Imports entry with
//     a non-relative Module
//     specifier (`isRelativeImport`
//     drops `./`-prefixed entries)
//
// The fixture deliberately puts the `log_global()` call
// inside `Greeter.greet` BEFORE the `log_global` free
// function is declared. tree-sitter does not enforce C++
// name-resolution order, and the dispatcher's same-file
// callee index is built AFTER the walk completes, so
// declaration order does not affect static_calls
// resolution. The spec fixture relies on this property; a
// regression that started resolving calls during the walk
// (rather than via the post-walk callee index) would fail
// this test by failing to resolve `log_global` from
// `Greeter.greet`.
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

	// ----- Classes (2 class nodes: Base, Greeter) -----
	// The spec pins exactly 2 ClassDecl entries. Both are
	// `class_specifier` nodes at translation-unit scope.
	if got := len(res.Classes); got != 2 {
		t.Errorf("expected exactly 2 class nodes; got %d (%v)",
			got, classNames(res.Classes))
	}
	classByName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		classByName[c.QualifiedName] = c
	}
	base, ok := classByName["Base"]
	if !ok {
		t.Fatalf("class %q missing from emitted set; got %v",
			"Base", classNames(res.Classes))
	}
	if base.Kind != "class" {
		t.Errorf("Base.Kind = %q; want %q", base.Kind, "class")
	}
	if len(base.Extends) != 0 {
		t.Errorf("Base.Extends should be empty (no base classes); got %v",
			base.Extends)
	}

	greeter, ok := classByName["Greeter"]
	if !ok {
		t.Fatalf("class %q missing from emitted set; got %v",
			"Greeter", classNames(res.Classes))
	}
	if greeter.Kind != "class" {
		t.Errorf("Greeter.Kind = %q; want %q", greeter.Kind, "class")
	}

	// ----- Extends (1 extends edge: Greeter -> Base) -----
	// The spec pins exactly 1 extends edge. At the parser
	// level the dispatcher mints one `extends` edge per
	// ClassDecl.Extends entry; pinning the single-element
	// slice with the exact base name is the parser-level
	// equivalent of "1 extends edge (Greeter -> Base)".
	wantExtends := []string{"Base"}
	if !stringSlicesEqualOrdered(greeter.Extends, wantExtends) {
		t.Errorf("Greeter.Extends: want %v, got %v",
			wantExtends, greeter.Extends)
	}

	// ----- Methods (3 method nodes) -----
	// The spec pins exactly 3 MethodDecl entries:
	// `Base.identify` and `Greeter.greet` (member methods
	// defined inline in their class bodies, with the dotted
	// `<EnclosingClass>.<name>` QualifiedName convention)
	// and `log_global` (a free function with
	// EnclosingClass == "" -- the parser-level signal the
	// dispatcher uses to mint a `file -> method` contains
	// edge instead of `class -> method`).
	if got := len(res.Methods); got != 3 {
		t.Errorf("expected exactly 3 method nodes; got %d (%v)",
			got, methodNames(res.Methods))
	}
	methodByName := map[string]MethodDecl{}
	for _, m := range res.Methods {
		methodByName[m.QualifiedName] = m
	}
	identify, ok := methodByName["Base.identify"]
	if !ok {
		t.Fatalf("method %q missing; got %v", "Base.identify",
			methodNames(res.Methods))
	}
	if identify.EnclosingClass != "Base" {
		t.Errorf("Base.identify.EnclosingClass = %q; want %q",
			identify.EnclosingClass, "Base")
	}
	greet, ok := methodByName["Greeter.greet"]
	if !ok {
		t.Fatalf("method %q missing; got %v", "Greeter.greet",
			methodNames(res.Methods))
	}
	if greet.EnclosingClass != "Greeter" {
		t.Errorf("Greeter.greet.EnclosingClass = %q; want %q",
			greet.EnclosingClass, "Greeter")
	}
	logGlobal, ok := methodByName["log_global"]
	if !ok {
		t.Fatalf("method %q missing; got %v", "log_global",
			methodNames(res.Methods))
	}
	if logGlobal.EnclosingClass != "" {
		t.Errorf("log_global.EnclosingClass = %q; want %q (free function)",
			logGlobal.EnclosingClass, "")
	}

	// ----- static_calls edge (Greeter.greet -> log_global) -----
	// The spec pins exactly 1 static_calls edge. The C++
	// walker records bare-identifier callees in
	// `MethodDecl.Calls`; the dispatcher's Pass 2b resolver
	// then matches each against the same-file callee index
	// to mint the edge. The fixture has exactly one bare
	// call inside Greeter.greet (`log_global()`), so
	// `greet.Calls` must contain exactly that one entry.
	// `this->identify()` is receiver-qualified and MUST go
	// to ReceiverCalls (see below), NOT into Calls -- a
	// walker that mis-routed it into Calls would produce a
	// second `static_calls` edge (false positive against
	// `Base.identify` after Pass 2b's free-function
	// fallback) and break the "exactly 1" pin.
	if !containsString(greet.Calls, "log_global") {
		t.Errorf("Greeter.greet.Calls should contain %q; got %v",
			"log_global", greet.Calls)
	}
	if got := len(greet.Calls); got != 1 {
		t.Errorf("expected exactly 1 entry in Greeter.greet.Calls "+
			"(only %q is a bare-identifier call; %q is receiver-qualified "+
			"and belongs in ReceiverCalls); got %d (%v)",
			"log_global", "this->identify", got, greet.Calls)
	}

	// ----- Receiver-qualified call captured verbatim -----
	// `this->identify()` is the receiver-qualified call the
	// spec uses to exercise the Pass 2b drop-on-miss path.
	// The parser-level contract is that ReceiverCalls
	// captures the bare callee name "identify" verbatim --
	// that is the ONLY input the dispatcher ever sees. The
	// dispatcher then:
	//
	//   1. attempts the lookup at
	//      `methodNodeID["Greeter.identify"]` (the
	//      ReceiverCalls entry scoped to the method's
	//      EnclosingClass),
	//   2. misses, because `identify` is declared on `Base`
	//      not `Greeter`, and the spec's A4 invariant
	//      forbids the dispatcher from walking the class
	//      hierarchy at this stage (cross-class resolution
	//      is the future Pass 2c work),
	//   3. drops the edge but persists the verbatim
	//      callee name in `attrs_json["calls_raw"]` so the
	//      future cross-file resolver can retry.
	//
	// This test cannot directly assert step 3 (it's a
	// dispatcher concern -- the `calls_raw` attrs key is
	// stamped by dispatcher code, not the parser); the
	// parser-level proxy is that the verbatim "identify"
	// name reaches ReceiverCalls intact. A walker that
	// dropped the receiver-qualified call entirely (or
	// rewrote `this->identify` into something other than
	// the bare callee name) would BLOCK the dispatcher's
	// persistence path -- there would be no verbatim name
	// for calls_raw to carry. Pin both presence and exact
	// cardinality so a regression that double-emits the
	// call (e.g. into both Calls and ReceiverCalls) is
	// also caught.
	if !containsString(greet.ReceiverCalls, "identify") {
		t.Errorf("Greeter.greet.ReceiverCalls should contain %q "+
			"(the bare callee name from `this->identify()`; the "+
			"dispatcher's Pass 2b drops the edge against "+
			"methodNodeID[%q] because identify is declared on Base "+
			"not Greeter, but persists the verbatim name on "+
			"attrs_json[calls_raw]); got %v",
			"identify", "Greeter.identify", greet.ReceiverCalls)
	}
	if got := len(greet.ReceiverCalls); got != 1 {
		t.Errorf("expected exactly 1 entry in Greeter.greet.ReceiverCalls "+
			"(only `this->identify()` is receiver-qualified in the fixture); "+
			"got %d (%v)", got, greet.ReceiverCalls)
	}

	// Negative: log_global is a leaf and Base.identify is
	// an empty body. Pin so a walker that hallucinates
	// self-references or leaks the parent function's call
	// set is caught.
	if got := len(logGlobal.Calls); got != 0 {
		t.Errorf("expected 0 entries in log_global.Calls (no inner calls); got %d (%v)",
			got, logGlobal.Calls)
	}
	if got := len(identify.Calls); got != 0 {
		t.Errorf("expected 0 entries in Base.identify.Calls (empty body); got %d (%v)",
			got, identify.Calls)
	}

	// ----- imports edge (file -> `string` package; ./base.h dropped) -----
	// The spec's fixture includes `<string>` (system
	// include) and `"base.h"` (local include). The
	// dispatcher's `isRelativeImport` filter drops entries
	// whose Module starts with `.` or `/`, so the parser
	// MUST emit the local include with a leading `./`
	// prefix (mirroring the C parser's contract) so the
	// dispatcher suppresses it. The system include keeps
	// its bracketed path verbatim (`string` -- no leading
	// `.` or `/`), so `isRelativeImport` returns false and
	// the dispatcher mints exactly one `imports` edge.
	mods := importModules(res.Imports)
	if !containsString(mods, "string") {
		t.Errorf("import %q missing; got %v", "string", mods)
	}
	if containsString(mods, "base.h") {
		t.Errorf("import %q must NOT appear as bare path (would produce "+
			"an imports edge); the parser must emit it as %q so "+
			"isRelativeImport drops it. got %v",
			"base.h", "./base.h", mods)
	}
	if !containsString(mods, "./base.h") {
		t.Errorf("import %q missing (the parser must prefix local includes "+
			"with `./` so the dispatcher drops them); got %v",
			"./base.h", mods)
	}

	// ----- base_access sub-test -----
	// The spec calls out a sub-test for
	// `LangMeta["base_access"]={"Base":"public"}` on the
	// Greeter class node's `attrs_json`. The dispatcher
	// folds `ClassDecl.LangMeta["base_access"]` straight
	// into the class node's `attrs_json["base_access"]`
	// via mergeLangMeta (architecture Section 4.4.2), so
	// the parser-level assertion on LangMeta is the
	// upstream of that persisted attr. Kept as a named
	// sub-test (per spec wording) so failure output makes
	// the spec mapping obvious.
	t.Run("base_access metadata", func(t *testing.T) {
		if greeter.LangMeta == nil {
			t.Fatalf("Greeter.LangMeta should be populated with base_access; got nil")
		}
		access, ok := greeter.LangMeta["base_access"].(map[string]string)
		if !ok {
			t.Fatalf("Greeter.LangMeta[base_access] should be map[string]string; got %T (%+v)",
				greeter.LangMeta["base_access"], greeter.LangMeta["base_access"])
		}
		if access["Base"] != "public" {
			t.Errorf("Greeter.LangMeta[base_access][Base] = %q; want %q "+
				"(the fixture writes `class Greeter : public Base` -- the "+
				"explicit `public` keyword must survive into LangMeta and "+
				"from there into attrs_json on the class node)",
				access["Base"], "public")
		}
	})

	// ----- dedupe sub-test -----
	// The spec calls out a sub-test for the declaration-
	// plus-definition dedupe rule: `class Foo { void
	// bar(); }; void Foo::bar() { log_global(); }` must
	// produce exactly one `Foo.bar` MethodDecl whose
	// `Calls` contains `log_global`. The parser's
	// `qualified_identifier` handling MUST recognise
	// `Foo::bar` as the out-of-line definition of the
	// in-class declaration `Foo::bar()` and replace (not
	// duplicate) the declaration entry -- the same dedupe
	// contract as the in-file forward-declaration rule
	// already pinned by TestTreeSitterCppParser_-
	// ForwardDeclarationDeduped, but extended to the
	// method-decl-vs-definition case the spec calls out
	// explicitly here.
	t.Run("dedupe collapses declaration plus definition", func(t *testing.T) {
		const dedupeSrc = `
class Foo {
public:
  void bar();
};

void Foo::bar() {
  log_global();
}

void log_global() {}
`
		res, err := parser.Parse("src/dedupe.cpp", []byte(dedupeSrc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		// Cardinality pin: counting by QualifiedName via
		// methodNames so a walker that emitted both the
		// declaration entry AND the definition entry (2
		// `Foo.bar`s) fails here visibly. Map lookup alone
		// would silently take whichever entry was inserted
		// last and mask the duplicate.
		fooBarCount := 0
		for _, m := range res.Methods {
			if m.QualifiedName == "Foo.bar" {
				fooBarCount++
			}
		}
		if fooBarCount != 1 {
			t.Fatalf("expected exactly 1 Foo.bar method node after "+
				"declaration+definition dedupe; got %d (%v)",
				fooBarCount, methodNames(res.Methods))
		}

		methodByName := map[string]MethodDecl{}
		for _, m := range res.Methods {
			methodByName[m.QualifiedName] = m
		}
		fooBar := methodByName["Foo.bar"]
		if fooBar.EnclosingClass != "Foo" {
			t.Errorf("Foo.bar.EnclosingClass = %q; want %q",
				fooBar.EnclosingClass, "Foo")
		}
		// The dedupe survivor MUST be the definition (the
		// one carrying the call to log_global), not the
		// prototype-only declaration. A walker that kept
		// the declaration and discarded the definition
		// would leave Calls empty -- the dispatcher would
		// then mint zero static_calls edges and the spec's
		// "static_calls edge targets log_global" assertion
		// would fail at the dispatcher level. Catch it at
		// the parser level so the failure localises here.
		if !containsString(fooBar.Calls, "log_global") {
			t.Errorf("Foo.bar.Calls should contain %q after dedupe "+
				"(the definition is the dedupe survivor; the "+
				"declaration carries no body and no calls); got %v",
				"log_global", fooBar.Calls)
		}
	})
}
