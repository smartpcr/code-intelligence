//go:build cgo

package ast

import (
	"strings"
	"testing"
)

// TestTreeSitterCSharpParser_ParsesFixture asserts that the
// C# tree-sitter implementation extracts classes, methods,
// using directives, namespace metadata, and `partial`
// modifiers from a representative fixture covering the v1
// extraction scope (one namespace, one class, one method, one
// free-ish constructor, one using directive, one inheritance
// edge, one this-qualified call).
func TestTreeSitterCSharpParser_ParsesFixture(t *testing.T) {
	const src = `
using System;
using static System.Math;
using F = System.IO.File;

namespace Acme.Greetings
{
    public interface IGreeter
    {
    }

    public partial class Greeter : IGreeter
    {
        private string _prefix;

        public Greeter(string prefix)
        {
            _prefix = prefix;
        }

        public string Greet(string name)
        {
            var formatted = FormatGreeting(this.prefix, name);
            this.lastGreeted = name;
            return formatted;
        }

        private string FormatGreeting(string prefix, string name)
        {
            return prefix + " " + name;
        }
    }
}
`
	parser := NewTreeSitterCSharpParser()
	if got := parser.Language(); got != "csharp" {
		t.Fatalf("Language: got %q, want csharp", got)
	}
	if got := parser.Extensions(); len(got) != 2 || got[0] != ".cs" || got[1] != ".csx" {
		t.Fatalf("Extensions: got %v, want [.cs .csx]", got)
	}

	res, err := parser.Parse("src/Greeter.cs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Classes: IGreeter + Greeter, both carry the namespace
	// on LangMeta and Greeter is flagged `partial`.
	classByName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		classByName[c.QualifiedName] = c
	}
	greeter, ok := classByName["Greeter"]
	if !ok {
		t.Fatalf("missing class Greeter; got %+v", classByName)
	}
	if greeter.Kind != "class" {
		t.Errorf("Greeter.Kind = %q; want class", greeter.Kind)
	}
	if greeter.LangMeta == nil {
		t.Fatalf("Greeter.LangMeta is nil; want namespace + partial")
	}
	if ns, _ := greeter.LangMeta["namespace"].(string); ns != "Acme.Greetings" {
		t.Errorf("Greeter.LangMeta[namespace] = %v; want Acme.Greetings", greeter.LangMeta["namespace"])
	}
	if partial, _ := greeter.LangMeta["partial"].(bool); !partial {
		t.Errorf("Greeter.LangMeta[partial] = %v; want true", greeter.LangMeta["partial"])
	}
	// Inheritance edge: Greeter : IGreeter must populate Extends.
	hasIGreeter := false
	for _, base := range greeter.Extends {
		if base == "IGreeter" {
			hasIGreeter = true
		}
	}
	if !hasIGreeter {
		t.Errorf("Greeter.Extends = %v; want IGreeter present", greeter.Extends)
	}

	iface, ok := classByName["IGreeter"]
	if !ok {
		t.Fatalf("missing interface IGreeter; got %+v", classByName)
	}
	if iface.Kind != "interface" {
		t.Errorf("IGreeter.Kind = %q; want interface", iface.Kind)
	}

	// Methods: ctor + Greet + FormatGreeting.
	methodByName := map[string]MethodDecl{}
	for _, m := range res.Methods {
		methodByName[m.QualifiedName] = m
	}
	for _, want := range []string{"Greeter.Greeter", "Greeter.Greet", "Greeter.FormatGreeting"} {
		if _, ok := methodByName[want]; !ok {
			t.Errorf("missing method %q; got %v", want, csharpMethodKeys(methodByName))
		}
	}

	// Greet's body must NOT start/end with raw `{`/`}` (brace-
	// strip parity with the TS walker so CountLogicalLines
	// behaves the same across languages).
	greet := methodByName["Greeter.Greet"]
	if strings.HasPrefix(greet.BodySource, "{") {
		t.Errorf("Greet.BodySource still starts with `{`: %q", greet.BodySource)
	}
	if strings.HasSuffix(strings.TrimRight(greet.BodySource, " \t\n\r"), "}") {
		t.Errorf("Greet.BodySource still ends with `}`: %q", greet.BodySource)
	}
	if greet.EnclosingClass != "Greeter" {
		t.Errorf("Greet.EnclosingClass = %q; want Greeter", greet.EnclosingClass)
	}
	// Bare call resolution: FormatGreeting should appear as a
	// bare call from Greet.
	gotBare := map[string]bool{}
	for _, c := range greet.Calls {
		gotBare[c] = true
	}
	if !gotBare["FormatGreeting"] {
		t.Errorf("Greet should call FormatGreeting; got %v", greet.Calls)
	}

	// `this.prefix` / `this.lastGreeted` must surface as
	// receiver-qualified accesses with the write classification
	// matching the assignment shape.
	memberByName := map[string]MemberAccess{}
	for _, ma := range greet.MemberAccesses {
		memberByName[ma.Name] = ma
	}
	if _, ok := memberByName["prefix"]; !ok {
		t.Errorf("Greet should read this.prefix; got %v", greet.MemberAccesses)
	}
	if ma, ok := memberByName["lastGreeted"]; !ok || !ma.IsWrite {
		t.Errorf("Greet should write this.lastGreeted; got %+v", greet.MemberAccesses)
	}

	// Modifiers: Greet is `public`.
	gotMods := map[string]bool{}
	for _, m := range greet.Modifiers {
		gotMods[m] = true
	}
	if !gotMods["public"] {
		t.Errorf("Greet.Modifiers should include public; got %v", greet.Modifiers)
	}

	// Method LangMeta should also carry the namespace.
	if ns, _ := greet.LangMeta["namespace"].(string); ns != "Acme.Greetings" {
		t.Errorf("Greet.LangMeta[namespace] = %v; want Acme.Greetings", greet.LangMeta["namespace"])
	}

	// Imports: System, System.Math (static), System.IO.File (alias=F).
	impByModule := map[string]Import{}
	for _, imp := range res.Imports {
		impByModule[imp.Module] = imp
	}
	if _, ok := impByModule["System"]; !ok {
		t.Errorf("missing `using System;` import; got %+v", res.Imports)
	}
	if stat, ok := impByModule["System.Math"]; !ok {
		t.Errorf("missing `using static System.Math;` import; got %+v", res.Imports)
	} else if isStat, _ := stat.LangMeta["is_static"].(bool); !isStat {
		t.Errorf("`using static System.Math` should set LangMeta[is_static]=true; got %+v", stat)
	}
	if al, ok := impByModule["System.IO.File"]; !ok {
		t.Errorf("missing aliased `using F = System.IO.File;`; got %+v", res.Imports)
	} else if al.Alias != "F" {
		t.Errorf("`using F = System.IO.File` Alias = %q; want F", al.Alias)
	}
}

// TestTreeSitterCSharpParser_FileScopedNamespace covers the
// `namespace Foo;` (file-scoped) form added in C# 10. The
// walker must propagate the namespace onto subsequent type
// declarations without descending into a `declaration_list`
// body.
func TestTreeSitterCSharpParser_FileScopedNamespace(t *testing.T) {
	const src = `
namespace Acme.Models;

public record Person(string Name);

public struct Vec
{
    public int X;
    public int Y;
}

public enum Color
{
    Red,
    Green,
}
`
	parser := NewTreeSitterCSharpParser()
	res, err := parser.Parse("src/Models.cs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		byName[c.QualifiedName] = c
	}
	wantKinds := map[string]string{
		"Person": "record",
		"Vec":    "struct",
		"Color":  "enum",
	}
	for name, wantKind := range wantKinds {
		cls, ok := byName[name]
		if !ok {
			t.Errorf("missing type %q; got %v", name, csharpClassKeys(byName))
			continue
		}
		if cls.Kind != wantKind {
			t.Errorf("%s.Kind = %q; want %q", name, cls.Kind, wantKind)
		}
		if cls.LangMeta == nil {
			t.Errorf("%s.LangMeta nil; want namespace=Acme.Models", name)
			continue
		}
		if ns, _ := cls.LangMeta["namespace"].(string); ns != "Acme.Models" {
			t.Errorf("%s.LangMeta[namespace] = %v; want Acme.Models", name, cls.LangMeta["namespace"])
		}
	}
}

// TestTreeSitterCSharpParser_NestedNamespacesAccumulate
// verifies that the walker dotted-joins an inner
// `namespace_declaration` onto an outer one rather than
// shadowing the outer scope.
func TestTreeSitterCSharpParser_NestedNamespacesAccumulate(t *testing.T) {
	const src = `
namespace Outer
{
    namespace Inner
    {
        public class Thing {}
    }
}
`
	parser := NewTreeSitterCSharpParser()
	res, err := parser.Parse("src/Thing.cs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Classes) != 1 || res.Classes[0].QualifiedName != "Thing" {
		t.Fatalf("expected single class Thing; got %+v", res.Classes)
	}
	if ns, _ := res.Classes[0].LangMeta["namespace"].(string); ns != "Outer.Inner" {
		t.Errorf("Thing.LangMeta[namespace] = %v; want Outer.Inner", res.Classes[0].LangMeta["namespace"])
	}
}

// csharpMethodKeys / csharpClassKeys return sorted key sets
// for compact failure messages.
func csharpMethodKeys(m map[string]MethodDecl) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func csharpClassKeys(m map[string]ClassDecl) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
