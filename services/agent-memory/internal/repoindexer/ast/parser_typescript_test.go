// Fixture-driven TypeScript parser test pipes through the full
// V2 Dispatcher.EmitFile to assert node/edge counts. Depends on
// `newFakeWriter` / `makeEvent` helpers in dispatcher_test.go
// (also un-gated in iter 6). Defines `signatureSimpleNames`
// which is used by parser_python_test.go.
package ast

import (
	"context"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet pins the
// Stage 3.2 acceptance requirement "Add a fixture-driven parser
// test that asserts a known TypeScript / JavaScript file ...
// produce[s] the expected Node + Edge set." The fixture is a
// small, deliberately-canonical TS file that exercises:
//   - one class with a base class declared in the same file
//     (so `extends` is same-file resolvable)
//   - one interface declared in the same file (so `implements`
//     is same-file resolvable)
//   - two methods on the class -- one constructor and one
//     regular method; the regular method calls a free
//     function declared in the same file (so `static_calls`
//     is same-file resolvable)
//   - one free function declared at file scope
//   - one relative import statement (dropped, NOT
//     materialised as an edge; relative-vs-external
//     classification is dispatcher-level. External-module
//     coverage lives in
//     `TestDispatcher_EmitsImportsEdgesForExternalModules`.)
func TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	const src = `
import { something } from "./util";

interface Greeter {
  greet(name: string): string;
}

class Base {
  identify() {
    return "base";
  }
}

class HelloWorld extends Base implements Greeter {
  constructor(prefix: string) {
    this.prefix = prefix;
  }

  greet(name: string): string {
    return formatGreeting(this.prefix, name);
  }
}

function formatGreeting(prefix: string, name: string): string {
  return prefix + " " + name;
}
`
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	if _, err := d.EmitFile(context.Background(), makeEvent("src/hello.ts", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	classes := fw.nodesOf("class")
	classNames := signatureSimpleNames(classes)
	wantClasses := map[string]bool{"Greeter": true, "Base": true, "HelloWorld": true}
	for want := range wantClasses {
		if !classNames[want] {
			t.Errorf("class %q missing from emitted set: got %v", want, classNames)
		}
	}
	if len(classes) != 3 {
		t.Errorf("expected 3 class/interface nodes; got %d (%v)", len(classes), classNames)
	}

	methods := fw.nodesOf("method")
	methodNames := signatureSimpleNames(methods)
	for _, want := range []string{"Greeter.greet", "Base.identify", "HelloWorld.constructor", "HelloWorld.greet", "formatGreeting"} {
		if !methodNames[want] {
			t.Errorf("method %q missing: got %v", want, methodNames)
		}
	}
	// 5 methods: Greeter.greet (interface decl, no body), Base.identify,
	// HelloWorld.constructor, HelloWorld.greet, formatGreeting (free fn).
	if len(methods) != 5 {
		t.Errorf("expected 5 method nodes (incl. interface + free fn); got %d (%v)",
			len(methods), methodNames)
	}

	// Edges.
	contains := fw.edgesOf("contains")
	// 3 file->class + 4 class->method (Greeter.greet, Base.identify,
	// HelloWorld.constructor, HelloWorld.greet) + 1 file->method
	// (formatGreeting) = 8 contains edges.
	if len(contains) != 8 {
		t.Errorf("expected 8 contains edges; got %d", len(contains))
	}
	if len(fw.edgesOf("extends")) != 1 {
		t.Errorf("expected 1 extends edge (HelloWorld->Base); got %d",
			len(fw.edgesOf("extends")))
	}
	if len(fw.edgesOf("implements")) != 1 {
		t.Errorf("expected 1 implements edge (HelloWorld->Greeter); got %d",
			len(fw.edgesOf("implements")))
	}
	staticCalls := fw.edgesOf("static_calls")
	if len(staticCalls) != 1 {
		t.Errorf("expected 1 static_calls edge (greet->formatGreeting); got %d",
			len(staticCalls))
	}
	// The fixture's only import (`./util`) is relative, so
	// no `imports` edges are emitted -- the dispatcher
	// drops relative imports pending the cross-file
	// resolver story (a non-relative import would
	// materialise a synthetic external-package Node + an
	// `imports` edge per
	// `TestDispatcher_EmitsImportsEdgesForExternalModules`).
	if got := len(fw.edgesOf("imports")); got != 0 {
		t.Errorf("expected 0 imports edges (only import is relative); got %d", got)
	}
}

// TestTypeScriptParser_AcceptsModifiersAndAsync exercises the
// scanner against modifier-heavy declarations.
func TestTypeScriptParser_AcceptsModifiersAndAsync(t *testing.T) {
	const src = `
export default class Service {
  public async load(): Promise<void> {
    await helper();
  }
  static helper() {
    return 1;
  }
}
`
	parser := NewTypeScriptParser()
	res, err := parser.Parse("src/svc.ts", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Classes) != 1 || res.Classes[0].QualifiedName != "Service" {
		t.Fatalf("expected exactly class Service; got %+v", res.Classes)
	}
	names := map[string]MethodDecl{}
	for _, m := range res.Methods {
		names[m.QualifiedName] = m
	}
	if _, ok := names["Service.load"]; !ok {
		t.Errorf("missing Service.load; got %v", keys(names))
	}
	if _, ok := names["Service.helper"]; !ok {
		t.Errorf("missing Service.helper; got %v", keys(names))
	}
}

// TestTypeScriptParser_HandlesEmptyFile guards against the
// scanner crashing on degenerate input -- malformed / empty
// files are common in monorepos and must not abort an ingest.
func TestTypeScriptParser_HandlesEmptyFile(t *testing.T) {
	parser := NewTypeScriptParser()
	res, err := parser.Parse("src/empty.ts", []byte(""))
	if err != nil {
		t.Fatalf("Parse empty: %v", err)
	}
	if len(res.Classes) != 0 || len(res.Methods) != 0 {
		t.Fatalf("expected empty parse result; got %+v", res)
	}
}

// TestTypeScriptParser_IgnoresCommentedDeclarations ensures
// the string/comment masker is doing its job; without it, a
// `class Foo {` inside a `/* ... */` block would produce a
// phantom class.
func TestTypeScriptParser_IgnoresCommentedDeclarations(t *testing.T) {
	const src = `
/* class Phantom {} */
// class AlsoPhantom {}
class Real {}
`
	parser := NewTypeScriptParser()
	res, _ := parser.Parse("src/c.ts", []byte(src))
	if len(res.Classes) != 1 || res.Classes[0].QualifiedName != "Real" {
		t.Fatalf("expected only class Real; got %+v", res.Classes)
	}
}

// --- helpers ---

func signatureSimpleNames(nodes []graphwriter.NodeInput) map[string]bool {
	out := map[string]bool{}
	for _, n := range nodes {
		out[lastSegmentAfterHash(n.CanonicalSignature)] = true
	}
	return out
}

// lastSegmentAfterHash returns the substring after the final
// `#` in s, stripped of any trailing `(...)` (so a method
// signature `<url>::method::<path>#Foo.bar(int)` becomes
// `Foo.bar`).
func lastSegmentAfterHash(s string) string {
	idx := strings.LastIndexByte(s, '#')
	if idx < 0 {
		return s
	}
	tail := s[idx+1:]
	if i := strings.IndexByte(tail, '('); i >= 0 {
		tail = tail[:i]
	}
	return tail
}

func keys(m map[string]MethodDecl) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
