//go:build cgo

package ast

import (
	"context"
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
	// Inheritance / interface partition (tech-spec Section 5.3):
	// IGreeter is a same-file `interface_declaration`, so the
	// Greeter base-list entry must land in Implements (NOT
	// Extends). The verbatim list is always retained on
	// LangMeta["base_raw"] for the future cross-file
	// resolver to re-partition.
	hasIGreeterImpl := false
	for _, base := range greeter.Implements {
		if base == "IGreeter" {
			hasIGreeterImpl = true
		}
	}
	if !hasIGreeterImpl {
		t.Errorf("Greeter.Implements = %v; want [IGreeter] (same-file interface routes to Implements per tech-spec 5.3)", greeter.Implements)
	}
	if len(greeter.Extends) != 0 {
		t.Errorf("Greeter.Extends = %v; want empty (IGreeter is a same-file interface, not a class base)", greeter.Extends)
	}
	if raw, _ := greeter.LangMeta["base_raw"].([]string); len(raw) != 1 || raw[0] != "IGreeter" {
		t.Errorf("Greeter.LangMeta[base_raw] = %v; want [IGreeter]", greeter.LangMeta["base_raw"])
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

// findCSharpClassByName returns a pointer to the ClassDecl
// whose QualifiedName matches `name`, or nil.
func findCSharpClassByName(cs []ClassDecl, name string) *ClassDecl {
	for i := range cs {
		if cs[i].QualifiedName == name {
			return &cs[i]
		}
	}
	return nil
}

// equalCSharpStrSlice treats nil and []string{} as equal.
func equalCSharpStrSlice(a, b []string) bool {
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

// TestCSharpFixture_EmitsExpectedNodeAndEdgeSet exercises the
// Stage 4.3 implementation-plan fixture (line 307) through the
// production `Dispatcher.EmitFile` pipeline. The fixture
// declares one interface (IGreeter), one base class (Base),
// and one derived class (HelloWorld : Base, IGreeter) inside
// namespace `Demo` with the HelloWorld.Greet method calling
// HelloWorld.FormatGreeting plus a `using System;` directive
// — the exact shape the implementation-plan calls out as the
// C# acceptance scenario for the Stage 4.3 dispatcher.
//
// Driving the real dispatcher (rather than the simulator
// `simulateCSharpDispatcherPass` previously used while the
// dispatcher was a no-op stub) ensures the fixture exercises
// the same two-pass insert protocol production callers run
// through `worker.go::runFull`. The simulator helper is kept
// alive by `TestCSharpFixture_DispatcherHarnessDropsUnresolvedAndAmbiguous`
// which pins the conservative-drop rules in isolation.
func TestCSharpFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	const src = `using System;

namespace Demo
{
    interface IGreeter { string Greet(string name); }

    class Base { public string Identify() => "base"; }

    class HelloWorld : Base, IGreeter
    {
        private string prefix = "hi";

        public string Greet(string name)
        {
            return FormatGreeting(this.prefix, name);
        }

        private static string FormatGreeting(string prefix, string name) => prefix + name;
    }
}
`
	const relPath = "src/HelloWorld.cs"

	// --- Parser-level shape pre-checks (what Pass 1 consumes). ---

	parser := NewTreeSitterCSharpParser()
	res, err := parser.Parse(relPath, []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	classByName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		classByName[c.QualifiedName] = c
	}
	for _, want := range []string{"IGreeter", "Base", "HelloWorld"} {
		if _, ok := classByName[want]; !ok {
			t.Errorf("missing class/interface %q; got %v", want, csharpClassKeys(classByName))
		}
	}
	if len(res.Classes) != 3 {
		t.Errorf("class node count = %d; want 3 (got %v)", len(res.Classes), csharpClassKeys(classByName))
	}

	wantMethods := []string{
		"IGreeter.Greet",
		"Base.Identify",
		"HelloWorld.Greet",
		"HelloWorld.FormatGreeting",
	}
	methodByName := map[string]MethodDecl{}
	for _, m := range res.Methods {
		methodByName[m.QualifiedName] = m
	}
	for _, want := range wantMethods {
		if _, ok := methodByName[want]; !ok {
			t.Errorf("missing method %q; got %v", want, csharpMethodKeys(methodByName))
		}
	}
	if len(res.Methods) != 4 {
		t.Errorf("method node count = %d; want 4 (got %v)", len(res.Methods), csharpMethodKeys(methodByName))
	}

	hw, ok := classByName["HelloWorld"]
	if !ok {
		t.Fatalf("HelloWorld class missing; cannot verify edges")
	}
	rawBases, _ := hw.LangMeta["base_raw"].([]string)
	if !equalCSharpStrSlice(rawBases, []string{"Base", "IGreeter"}) {
		t.Errorf("HelloWorld.LangMeta[base_raw] = %v; want [Base IGreeter]", hw.LangMeta["base_raw"])
	}

	// --- Production dispatcher emission assertions. ---

	fw := newFakeWriter()
	d := NewDispatcher(fw, WithParsers(NewTreeSitterCSharpParser()))
	if _, err := d.EmitFile(context.Background(), makeEvent(relPath, src)); err != nil {
		t.Fatalf("Dispatcher.EmitFile: %v", err)
	}

	// Pass 1a: 3 class/interface nodes.
	classNodes := fw.nodesOf("class")
	if len(classNodes) != 3 {
		t.Errorf("class nodes = %d; want 3 (IGreeter, Base, HelloWorld)", len(classNodes))
	}
	classSigs := signatureSimpleNames(classNodes)
	for _, want := range []string{"IGreeter", "Base", "HelloWorld"} {
		if !classSigs[want] {
			t.Errorf("class node %q missing; got %v", want, classSigs)
		}
	}

	// Pass 1b: 4 method nodes.
	methodNodes := fw.nodesOf("method")
	if len(methodNodes) != 4 {
		t.Errorf("method nodes = %d; want 4", len(methodNodes))
	}
	methodSigs := signatureSimpleNames(methodNodes)
	for _, want := range wantMethods {
		if !methodSigs[want] {
			t.Errorf("method node %q missing; got %v", want, methodSigs)
		}
	}

	// Resolve NodeIDs through the fake writer's
	// signature-indexed registry so the tuple assertions
	// below do not depend on the global insertion order
	// (Pass-0 imports may insert a package node before any
	// class, which would shift simple `node-N` predictions).
	hwID := fw.nodeIDBySimpleSig("class", "HelloWorld")
	baseID := fw.nodeIDBySimpleSig("class", "Base")
	igreeterID := fw.nodeIDBySimpleSig("class", "IGreeter")
	greetID := fw.nodeIDBySimpleSig("method", "HelloWorld.Greet")
	fmtID := fw.nodeIDBySimpleSig("method", "HelloWorld.FormatGreeting")
	if hwID == "" || baseID == "" || igreeterID == "" || greetID == "" || fmtID == "" {
		t.Fatalf("missing NodeID(s): hw=%q base=%q igreeter=%q greet=%q fmt=%q",
			hwID, baseID, igreeterID, greetID, fmtID)
	}

	// Pass 2a: exactly one extends tuple HelloWorld -> Base.
	extends := fw.edgesOf("extends")
	if len(extends) != 1 {
		t.Fatalf("extends edges = %d; want 1; edges=%+v", len(extends), extends)
	}
	if extends[0].SrcNodeID != hwID || extends[0].DstNodeID != baseID {
		t.Errorf("extends tuple = %s->%s; want %s->%s (HelloWorld->Base)",
			extends[0].SrcNodeID, extends[0].DstNodeID, hwID, baseID)
	}

	// Pass 2a: exactly one implements tuple HelloWorld -> IGreeter.
	implements := fw.edgesOf("implements")
	if len(implements) != 1 {
		t.Fatalf("implements edges = %d; want 1; edges=%+v", len(implements), implements)
	}
	if implements[0].SrcNodeID != hwID || implements[0].DstNodeID != igreeterID {
		t.Errorf("implements tuple = %s->%s; want %s->%s (HelloWorld->IGreeter)",
			implements[0].SrcNodeID, implements[0].DstNodeID, hwID, igreeterID)
	}

	// Pass 2b: exactly one static_calls tuple
	// HelloWorld.Greet -> HelloWorld.FormatGreeting.
	staticCalls := fw.edgesOf("static_calls")
	if len(staticCalls) != 1 {
		t.Fatalf("static_calls edges = %d; want 1 (HelloWorld.Greet -> HelloWorld.FormatGreeting); edges=%+v",
			len(staticCalls), staticCalls)
	}
	if staticCalls[0].SrcNodeID != greetID || staticCalls[0].DstNodeID != fmtID {
		t.Errorf("static_calls tuple = %s->%s; want %s->%s (Greet->FormatGreeting)",
			staticCalls[0].SrcNodeID, staticCalls[0].DstNodeID, greetID, fmtID)
	}

	// Pass 0: exactly one imports tuple file -> System package.
	importEdges := fw.edgesOf("imports")
	if len(importEdges) != 1 {
		t.Fatalf("imports edges = %d; want 1 (file -> System); edges=%+v", len(importEdges), importEdges)
	}
	pkgNodes := fw.nodesOf("package")
	if len(pkgNodes) != 1 {
		t.Fatalf("package nodes = %d; want 1 (synthetic external System)", len(pkgNodes))
	}
	systemID := fw.nodeIDBySimpleSig("package", "System")
	if systemID == "" {
		// External-package canonical signatures use a flat
		// module name (no `#` segment); fall back to a
		// direct nodes-of scan for the System node id.
		systemID = fw.idBySig[pkgNodes[0].CanonicalSignature]
	}
	if importEdges[0].DstNodeID != systemID {
		t.Errorf("imports tuple dst = %s; want %s (System package)",
			importEdges[0].DstNodeID, systemID)
	}
	if !strings.Contains(pkgNodes[0].CanonicalSignature, "System") {
		t.Errorf("package node signature = %q; want to contain \"System\"",
			pkgNodes[0].CanonicalSignature)
	}

	// Pass 1a/1b contains edges: 3 file->class + 4 parent->method = 7.
	if got := len(fw.edgesOf("contains")); got != 7 {
		t.Errorf("contains edges = %d; want 7 (3 file->class + 4 class->method)", got)
	}
	// Tuple-pin two representative parent->method contains
	// edges so a swapped enclosing-class lookup would fail:
	//   HelloWorld -> HelloWorld.Greet
	//   HelloWorld -> HelloWorld.FormatGreeting
	containsHWGreet := false
	containsHWFmt := false
	for _, e := range fw.edgesOf("contains") {
		if e.SrcNodeID == hwID && e.DstNodeID == greetID {
			containsHWGreet = true
		}
		if e.SrcNodeID == hwID && e.DstNodeID == fmtID {
			containsHWFmt = true
		}
	}
	if !containsHWGreet {
		t.Errorf("missing contains edge HelloWorld->HelloWorld.Greet (%s->%s)", hwID, greetID)
	}
	if !containsHWFmt {
		t.Errorf("missing contains edge HelloWorld->HelloWorld.FormatGreeting (%s->%s)", hwID, fmtID)
	}

	// Sanity: the C# parser emits no Block subdivision for this
	// fixture (every method body is <= 80 logical lines).
	if got := len(fw.nodesOf("block")); got != 0 {
		t.Errorf("block nodes = %d; want 0 (every method body is <= 80 logical lines)", got)
	}
}

// csharpDispatcherEdge models one typed graph edge the dispatcher
// would emit for a parser-produced fact. The fields mirror the
// production Edge struct in dispatcher.go (Kind, Source, Target).
type csharpDispatcherEdge struct {
	Kind, Source, Target string
}

// csharpDispatcherEdgeBag is the test-side fake writer. It just
// records every edge the simulated dispatcher emits so the test
// can assert on the resulting set.
type csharpDispatcherEdgeBag struct {
	edges []csharpDispatcherEdge
}

func (b *csharpDispatcherEdgeBag) add(e csharpDispatcherEdge) { b.edges = append(b.edges, e) }

func (b *csharpDispatcherEdgeBag) has(want csharpDispatcherEdge) bool {
	for _, e := range b.edges {
		if e == want {
			return true
		}
	}
	return false
}

func (b *csharpDispatcherEdgeBag) countKind(kind string) int {
	n := 0
	for _, e := range b.edges {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// simulateCSharpDispatcherPass models the dispatcher's Pass 2a
// edge emission for a C# ParseResult, per tech-spec Section 5.3
// ("the dispatcher's Pass 2a iterates `c.Extends` / `c.Implements`
// unchanged ... drops the edge per C4 when `classNodeID[entry]`
// misses"). The simulated dispatcher emits, for one file:
//
//   - file -> class `contains` per ClassDecl.
//   - class -> method `contains` per MethodDecl with a non-empty
//     EnclosingClass.
//   - class -> baseclass `extends` per ClassDecl.Extends entry
//     that resolves to a same-file `class_declaration` (kind ==
//     "class") -- cross-file or non-class targets are dropped
//     per the dispatcher's C4 unknown-target rule.
//   - class -> interface `implements` per ClassDecl.Implements
//     entry that resolves to a same-file `interface_declaration`
//     (kind == "interface") -- cross-file or non-interface
//     targets are dropped.
//   - method -> method `static_calls` per MethodDecl.Calls entry
//     that resolves UNAMBIGUOUSLY (one match) to a same-class
//     method via the bare-name multimap rule (dispatcher A5:
//     ambiguous bare names are dropped, not mis-resolved).
//   - file -> module `imports` per Import.
//
// The unresolved-target drop rule is the same shape `parser.go`
// documents on `ClassDecl.Extends` ("the dispatcher resolves each
// one against the same file's declared classes and emits an
// `extends` edge per resolved entry, dropping unresolved bare
// names").
func simulateCSharpDispatcherPass(res ParseResult, relPath string) *csharpDispatcherEdgeBag {
	bag := &csharpDispatcherEdgeBag{}

	// Same-file class-kind index for extends / implements
	// resolution (mirrors `dispatcher.go::classNodeID` lookups,
	// keyed by simple QualifiedName).
	classKind := map[string]string{}
	for _, c := range res.Classes {
		classKind[c.QualifiedName] = c.Kind
	}

	// Same-class method short-name multimap for static_calls
	// resolution (mirrors `dispatcher.go::buildCalleeIndex` /
	// `resolveBareCalls`, the A5 collision-drop rule).
	methodsByClass := map[string]map[string]int{}
	for _, m := range res.Methods {
		if m.EnclosingClass == "" {
			continue
		}
		short := m.QualifiedName
		if dot := strings.LastIndex(short, "."); dot >= 0 {
			short = short[dot+1:]
		}
		if _, ok := methodsByClass[m.EnclosingClass]; !ok {
			methodsByClass[m.EnclosingClass] = map[string]int{}
		}
		methodsByClass[m.EnclosingClass][short]++
	}

	// Classes: contains + extends + implements.
	for _, c := range res.Classes {
		bag.add(csharpDispatcherEdge{Kind: "contains", Source: relPath, Target: c.QualifiedName})
		for _, base := range c.Extends {
			if classKind[base] == "class" {
				bag.add(csharpDispatcherEdge{Kind: "extends", Source: c.QualifiedName, Target: base})
			}
		}
		for _, iface := range c.Implements {
			if classKind[iface] == "interface" {
				bag.add(csharpDispatcherEdge{Kind: "implements", Source: c.QualifiedName, Target: iface})
			}
		}
	}

	// Methods: contains + static_calls.
	for _, m := range res.Methods {
		if m.EnclosingClass != "" {
			bag.add(csharpDispatcherEdge{Kind: "contains", Source: m.EnclosingClass, Target: m.QualifiedName})
		}
		if m.EnclosingClass == "" {
			continue
		}
		siblings := methodsByClass[m.EnclosingClass]
		for _, callee := range m.Calls {
			if siblings[callee] == 1 {
				bag.add(csharpDispatcherEdge{
					Kind:   "static_calls",
					Source: m.QualifiedName,
					Target: m.EnclosingClass + "." + callee,
				})
			}
		}
	}

	// Imports: file -> module.
	for _, imp := range res.Imports {
		if imp.Module == "" {
			continue
		}
		bag.add(csharpDispatcherEdge{Kind: "imports", Source: relPath, Target: imp.Module})
	}

	return bag
}

// TestCSharpFixture_DispatcherHarnessDropsUnresolvedAndAmbiguous
// exercises the conservative-drop rules embedded in
// `simulateCSharpDispatcherPass`: extends / implements targets
// that do not resolve to a same-file declaration of the matching
// kind are dropped (the dispatcher's C4 unknown-target rule);
// static_calls callees whose bare name resolves to more than one
// same-class method are dropped (the dispatcher's A5 ambiguity
// rule). These drops match the production dispatcher's
// edge-emission contract per tech-spec §5.3 and prevent false-
// positive edges from showing up in the graph.
func TestCSharpFixture_DispatcherHarnessDropsUnresolvedAndAmbiguous(t *testing.T) {
	// `OnlyHere` extends `MissingBase` (cross-file, no same-file
	// declaration -> extends edge MUST be dropped) and
	// implements `IMissing` (cross-file -> implements edge MUST
	// be dropped). `Run` calls `Helper`, but the class declares
	// TWO `Helper` overloads -> the static_calls edge MUST be
	// dropped per A5.
	res := ParseResult{
		Classes: []ClassDecl{
			{
				QualifiedName: "OnlyHere",
				Kind:          "class",
				Extends:       []string{"MissingBase"},
				Implements:    []string{"IMissing"},
			},
		},
		Methods: []MethodDecl{
			{QualifiedName: "OnlyHere.Run", EnclosingClass: "OnlyHere", Calls: []string{"Helper"}},
			{QualifiedName: "OnlyHere.Helper", EnclosingClass: "OnlyHere"},
			{QualifiedName: "OnlyHere.Helper", EnclosingClass: "OnlyHere"},
		},
		Imports: []Import{{Module: "System"}},
	}

	bag := simulateCSharpDispatcherPass(res, "src/OnlyHere.cs")

	if got := bag.countKind("extends"); got != 0 {
		t.Errorf("extends edges = %d; want 0 (cross-file MissingBase must be dropped per C4); edges=%v", got, bag.edges)
	}
	if got := bag.countKind("implements"); got != 0 {
		t.Errorf("implements edges = %d; want 0 (cross-file IMissing must be dropped per C4); edges=%v", got, bag.edges)
	}
	if got := bag.countKind("static_calls"); got != 0 {
		t.Errorf("static_calls edges = %d; want 0 (ambiguous Helper overloads must be dropped per A5); edges=%v", got, bag.edges)
	}

	// Sanity: imports + contains are unaffected by the drop rules.
	if got, want := bag.countKind("imports"), 1; got != want {
		t.Errorf("imports edges = %d; want %d; edges=%v", got, want, bag.edges)
	}
	if got, want := bag.countKind("contains"), 1+3; got != want {
		t.Errorf("contains edges = %d; want %d (1 file->class + 3 class->method); edges=%v", got, want, bag.edges)
	}
}

// TestCSharpFixture_BaseListPartitionsByLocalKind covers the
// six rows of tech-spec Section 5.3's base-list decision
// matrix. Each sub-case fixes a declaring kind + base-list
// shape and asserts the parser's two-pass walker partitions
// the entries into Extends / Implements per the table.
// `LangMeta["base_raw"]` always equals the verbatim
// source-order list.
func TestCSharpFixture_BaseListPartitionsByLocalKind(t *testing.T) {
	type matrixCase struct {
		name           string
		src            string
		targetClass    string
		wantExtends    []string
		wantImplements []string
		wantBaseRaw    []string
	}
	cases := []matrixCase{
		{
			// Row 1: same-file class base.
			name:           "class extends same-file class",
			src:            `class Bar {} class Foo : Bar {}`,
			targetClass:    "Foo",
			wantExtends:    []string{"Bar"},
			wantImplements: nil,
			wantBaseRaw:    []string{"Bar"},
		},
		{
			// Row 2: same-file interface base.
			name:           "class implements same-file interface",
			src:            `interface IFoo {} class Foo : IFoo {}`,
			targetClass:    "Foo",
			wantExtends:    nil,
			wantImplements: []string{"IFoo"},
			wantBaseRaw:    []string{"IFoo"},
		},
		{
			// Row 3: mixed same-file (class + interface).
			name:           "class mixed same-file",
			src:            `class Bar {} interface IBaz {} class Foo : Bar, IBaz {}`,
			targetClass:    "Foo",
			wantExtends:    []string{"Bar"},
			wantImplements: []string{"IBaz"},
			wantBaseRaw:    []string{"Bar", "IBaz"},
		},
		{
			// Row 4: interface-only same-file.
			name:           "class interface-only same-file",
			src:            `interface IBaz {} interface IQux {} class Foo : IBaz, IQux {}`,
			targetClass:    "Foo",
			wantExtends:    nil,
			wantImplements: []string{"IBaz", "IQux"},
			wantBaseRaw:    []string{"IBaz", "IQux"},
		},
		{
			// Row 5: cross-file unresolved at position 0.
			// Per tech-spec 5.3, defaults to Extends because
			// C# permits at most one base class and position 0
			// is most likely the cross-file base.
			name:           "class cross-file unresolved at pos 0",
			src:            `class Foo : Bar {}`,
			targetClass:    "Foo",
			wantExtends:    []string{"Bar"},
			wantImplements: nil,
			wantBaseRaw:    []string{"Bar"},
		},
		{
			// Row 6: mixed cross-file class + same-file interface.
			name:           "class mixed cross-file + same-file interface",
			src:            `interface IBaz {} class Foo : Bar, IBaz {}`,
			targetClass:    "Foo",
			wantExtends:    []string{"Bar"},
			wantImplements: []string{"IBaz"},
			wantBaseRaw:    []string{"Bar", "IBaz"},
		},
	}
	parser := NewTreeSitterCSharpParser()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := parser.Parse("src/p.cs", []byte(tc.src))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			target := findCSharpClassByName(res.Classes, tc.targetClass)
			if target == nil {
				t.Fatalf("missing target class %q; got %v", tc.targetClass, res.Classes)
			}
			if !equalCSharpStrSlice(target.Extends, tc.wantExtends) {
				t.Errorf("%s.Extends = %v; want %v", tc.targetClass, target.Extends, tc.wantExtends)
			}
			if !equalCSharpStrSlice(target.Implements, tc.wantImplements) {
				t.Errorf("%s.Implements = %v; want %v", tc.targetClass, target.Implements, tc.wantImplements)
			}
			raw, _ := target.LangMeta["base_raw"].([]string)
			if !equalCSharpStrSlice(raw, tc.wantBaseRaw) {
				t.Errorf("%s.LangMeta[base_raw] = %v; want %v", tc.targetClass, raw, tc.wantBaseRaw)
			}
		})
	}
}

// TestCSharpFixture_BaseListPartitionsForNonClassDeclaringKinds
// covers the partition rules for `interface` and `struct` /
// `record` declaring kinds (tech-spec Section 5.3, second and
// third bullets of the "two-pass walker contract"). The
// interface row sends every base to Extends (super-interfaces);
// the struct / record rows send every base to Implements.
func TestCSharpFixture_BaseListPartitionsForNonClassDeclaringKinds(t *testing.T) {
	parser := NewTreeSitterCSharpParser()

	t.Run("interface declaring routes base list to Extends", func(t *testing.T) {
		res, err := parser.Parse("src/iface.cs", []byte(`interface IBase {} interface IExt : IBase {}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		iExt := findCSharpClassByName(res.Classes, "IExt")
		if iExt == nil {
			t.Fatalf("missing IExt; got %v", res.Classes)
		}
		if !equalCSharpStrSlice(iExt.Extends, []string{"IBase"}) {
			t.Errorf("IExt.Extends = %v; want [IBase]", iExt.Extends)
		}
		if len(iExt.Implements) != 0 {
			t.Errorf("IExt.Implements = %v; want empty (super-interfaces route to Extends)", iExt.Implements)
		}
		raw, _ := iExt.LangMeta["base_raw"].([]string)
		if !equalCSharpStrSlice(raw, []string{"IBase"}) {
			t.Errorf("IExt.LangMeta[base_raw] = %v; want [IBase]", iExt.LangMeta["base_raw"])
		}
	})

	t.Run("struct declaring routes base list to Implements", func(t *testing.T) {
		res, err := parser.Parse("src/strct.cs", []byte(`interface IFoo {} struct S : IFoo {}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		s := findCSharpClassByName(res.Classes, "S")
		if s == nil {
			t.Fatalf("missing S; got %v", res.Classes)
		}
		if len(s.Extends) != 0 {
			t.Errorf("S.Extends = %v; want empty (struct has no base class in C#)", s.Extends)
		}
		if !equalCSharpStrSlice(s.Implements, []string{"IFoo"}) {
			t.Errorf("S.Implements = %v; want [IFoo]", s.Implements)
		}
	})

	t.Run("record declaring routes base list to Implements", func(t *testing.T) {
		res, err := parser.Parse("src/rec.cs", []byte(`interface IFoo {} record R(int X) : IFoo;`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		r := findCSharpClassByName(res.Classes, "R")
		if r == nil {
			t.Fatalf("missing R; got %v", res.Classes)
		}
		if len(r.Extends) != 0 {
			t.Errorf("R.Extends = %v; want empty (record has no base class via base-list interface convention in v1)", r.Extends)
		}
		if !equalCSharpStrSlice(r.Implements, []string{"IFoo"}) {
			t.Errorf("R.Implements = %v; want [IFoo]", r.Implements)
		}
	})
}

// TestCSharpFixture_PartialFlagAndNamespace asserts that a
// `partial class Foo` fragment inside `namespace Demo` populates
// both `LangMeta["namespace"]=="Demo"` and
// `LangMeta["partial"]==true` (the well-known C# LangMeta
// keys per tech-spec line 487-488).
func TestCSharpFixture_PartialFlagAndNamespace(t *testing.T) {
	res, err := NewTreeSitterCSharpParser().Parse("src/p.cs", []byte(`namespace Demo { partial class Foo {} }`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	foo := findCSharpClassByName(res.Classes, "Foo")
	if foo == nil {
		t.Fatalf("missing Foo; got %v", res.Classes)
	}
	if ns, _ := foo.LangMeta["namespace"].(string); ns != "Demo" {
		t.Errorf("Foo.LangMeta[namespace] = %v; want Demo", foo.LangMeta["namespace"])
	}
	if partial, _ := foo.LangMeta["partial"].(bool); !partial {
		t.Errorf("Foo.LangMeta[partial] = %v; want true", foo.LangMeta["partial"])
	}
}

// TestCSharpFixture_ReceiverCallDoesNotEmitSpuriousMemberAccess
// pins the tech-spec Section 5.3 "Member access" rule: a
// `this.<name>` access is recorded as a MemberAccess ONLY
// when it sits OUTSIDE any invocation_expression. The
// receiver call form `this.Method()` must produce a
// ReceiverCall but NOT a MemberAccess on the enclosing type.
// Without this rule the parser would emit a spurious read
// edge from the method to the enclosing type for every
// receiver-qualified call.
func TestCSharpFixture_ReceiverCallDoesNotEmitSpuriousMemberAccess(t *testing.T) {
	const src = `class C {
    void Caller()
    {
        this.Helper();
        var x = this.Field;
    }
    void Helper() {}
    int Field;
}`
	res, err := NewTreeSitterCSharpParser().Parse("src/c.cs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var caller *MethodDecl
	for i := range res.Methods {
		if res.Methods[i].QualifiedName == "C.Caller" {
			caller = &res.Methods[i]
			break
		}
	}
	if caller == nil {
		t.Fatalf("missing C.Caller; got %v", res.Methods)
	}
	// this.Helper() -> ReceiverCalls, NOT MemberAccesses.
	sawHelperCall := false
	for _, c := range caller.ReceiverCalls {
		if c == "Helper" {
			sawHelperCall = true
		}
	}
	if !sawHelperCall {
		t.Errorf("C.Caller.ReceiverCalls should include Helper; got %v", caller.ReceiverCalls)
	}
	for _, ma := range caller.MemberAccesses {
		if ma.Name == "Helper" {
			t.Errorf("C.Caller.MemberAccesses must NOT include Helper (it's a receiver call, not a member access); got %v", caller.MemberAccesses)
		}
	}
	// this.Field (outside any invocation) -> MemberAccesses.
	sawFieldAccess := false
	for _, ma := range caller.MemberAccesses {
		if ma.Name == "Field" && !ma.IsWrite {
			sawFieldAccess = true
		}
	}
	if !sawFieldAccess {
		t.Errorf("C.Caller.MemberAccesses should include Field (read); got %v", caller.MemberAccesses)
	}
}

// TestTreeSitterCSharpParser_RegisteredInDefaultParsers verifies
// the parser is reachable through `defaultParsers()` (the
// factory parsers_cgo.go::defaultParsers wires into the
// dispatcher). Without this assertion, the parser could be
// implemented but silently unreachable -- the
// implemented-but-unreachable failure mode previously flagged
// by the rubber-duck review for the C tree-sitter parser. This
// test is the C# twin of TestTreeSitterCParser_RegisteredInDefaultParsers
// and pins the `parsers_cgo.go` registration line so a future
// edit that drops `NewTreeSitterCSharpParser()` from
// `defaultParsers()` fails loudly here instead of silently
// regressing `.cs` / `.csx` ingestion to "no parser registered
// for extension" skips.
//
// The test runs on the CGO build path only (the file's `//go:build cgo`
// tag); the CGO=off counterpart in parsers_nocgo.go intentionally
// does NOT register C# (per the workstream brief and the
// parsers_nocgo.go doc comment), so a parallel CGO=off assertion
// would contradict the documented "no scanner-backed fallback in
// v1" stance.
func TestTreeSitterCSharpParser_RegisteredInDefaultParsers(t *testing.T) {
	parsers := defaultParsers()
	want := map[string]bool{".cs": false, ".csx": false}
	for _, p := range parsers {
		if p.Language() != "csharp" {
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
			t.Errorf("defaultParsers() does not register the C# parser for %q; saw %d parsers", ext, len(parsers))
		}
	}
}
