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
