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
// dotted prefix per namespace keyword.
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
	got := map[string]bool{}
	for _, c := range res.Classes {
		got[c.QualifiedName] = true
	}
	for _, want := range []string{"alpha.Outer", "alpha.beta.Inner", "gamma.delta.Bridge"} {
		if !got[want] {
			t.Errorf("missing class %q; got %v", want, classNames(res.Classes))
		}
	}
}

// TestTreeSitterCppParser_NestedClassInsideClass asserts a
// class declared inside another class body inherits the
// outer's QualifiedName as a dotted prefix -- consistent
// with the namespace-prefix rule (and with the TS walker's
// nested-class behaviour the brief tells us to mirror).
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
	got := map[string]string{}
	for _, c := range res.Classes {
		got[c.QualifiedName] = c.Kind
	}
	if got["ns.Outer"] != "class" {
		t.Errorf("ns.Outer: want class, got %q (full=%v)", got["ns.Outer"], got)
	}
	if got["ns.Outer.Inner"] != "class" {
		t.Errorf("ns.Outer.Inner: want class, got %q (full=%v)", got["ns.Outer.Inner"], got)
	}
	if got["ns.Outer.Tag"] != "struct" {
		t.Errorf("ns.Outer.Tag: want struct, got %q (full=%v)", got["ns.Outer.Tag"], got)
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
