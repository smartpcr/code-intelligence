//go:build canonical_dispatcher

// Fixture-driven Python parser test pipes through the full V2
// Dispatcher.EmitFile to assert node/edge counts. Depends on
// `newFakeWriter` / `makeEvent` helpers defined in
// dispatcher_test.go (also gated). Re-enables when the Stage
// 3.2 dispatcher landing workstream wires emission.
package ast

import (
	"context"
	"testing"
)

// TestPythonFixture_EmitsExpectedNodeAndEdgeSet pins the
// Stage 3.2 acceptance requirement "Add a fixture-driven
// parser test that asserts ... a known Python file ...
// produce[s] the expected Node + Edge set." The fixture
// exercises:
//   - one base class and one derived class declared in the
//     same file (so `extends` is same-file resolvable; Python
//     multiple inheritance via base list)
//   - two methods on the derived class
//   - one free function declared at file scope
//   - one method that calls a free function declared in the
//     same file (so `static_calls` is same-file resolvable)
//   - one `from X import ...` statement (recorded as Import,
//     not edge in v1)
func TestPythonFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	const src = `from utils import helper_external

class Base:
    def identify(self):
        return "base"

class HelloWorld(Base):
    def __init__(self, prefix):
        self.prefix = prefix

    def greet(self, name):
        return format_greeting(self.prefix, name)

def format_greeting(prefix, name):
    return prefix + " " + name
`
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	if _, err := d.EmitFile(context.Background(), makeEvent("svc/hello.py", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	classes := fw.nodesOf("class")
	classNames := signatureSimpleNames(classes)
	for _, want := range []string{"Base", "HelloWorld"} {
		if !classNames[want] {
			t.Errorf("class %q missing from emitted set: got %v", want, classNames)
		}
	}
	if len(classes) != 2 {
		t.Errorf("expected 2 class nodes; got %d (%v)", len(classes), classNames)
	}

	methods := fw.nodesOf("method")
	methodNames := signatureSimpleNames(methods)
	for _, want := range []string{
		"Base.identify",
		"HelloWorld.__init__",
		"HelloWorld.greet",
		"format_greeting",
	} {
		if !methodNames[want] {
			t.Errorf("method %q missing: got %v", want, methodNames)
		}
	}
	if len(methods) != 4 {
		t.Errorf("expected 4 method nodes; got %d (%v)", len(methods), methodNames)
	}

	// Edges.
	// 2 file->class + 3 class->method + 1 file->method = 6 contains edges
	if got := len(fw.edgesOf("contains")); got != 6 {
		t.Errorf("expected 6 contains edges; got %d", got)
	}
	if got := len(fw.edgesOf("extends")); got != 1 {
		t.Errorf("expected 1 extends edge (HelloWorld->Base); got %d", got)
	}
	if got := len(fw.edgesOf("static_calls")); got != 1 {
		t.Errorf("expected 1 static_calls edge (greet->format_greeting); got %d", got)
	}
	// Python has no `implements`; v1 emits zero.
	if got := len(fw.edgesOf("implements")); got != 0 {
		t.Errorf("expected 0 implements edges in python; got %d", got)
	}
}

// TestPythonParser_HandlesDecorators ensures `@decorator`
// lines preceding a class / def do not prevent the
// declaration from being extracted.
func TestPythonParser_HandlesDecorators(t *testing.T) {
	const src = `@some_decorator
class Foo:
    @other.decorator
    def bar(self):
        return 1
`
	parser := NewPythonParser()
	res, err := parser.Parse("x.py", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Classes) != 1 || res.Classes[0].QualifiedName != "Foo" {
		t.Fatalf("expected class Foo; got %+v", res.Classes)
	}
	if len(res.Methods) != 1 || res.Methods[0].QualifiedName != "Foo.bar" {
		t.Fatalf("expected method Foo.bar; got %+v", res.Methods)
	}
}

// TestPythonParser_IgnoresTripleQuotedDocstrings ensures the
// triple-quote masker is doing its job.
func TestPythonParser_IgnoresTripleQuotedDocstrings(t *testing.T) {
	const src = `"""
class Phantom:
    def bar(self): pass
"""
class Real:
    pass
`
	parser := NewPythonParser()
	res, _ := parser.Parse("x.py", []byte(src))
	if len(res.Classes) != 1 || res.Classes[0].QualifiedName != "Real" {
		t.Fatalf("expected only class Real; got %+v", res.Classes)
	}
}

// TestPythonParser_HandlesEmptyFile guards degenerate input.
func TestPythonParser_HandlesEmptyFile(t *testing.T) {
	parser := NewPythonParser()
	res, err := parser.Parse("x.py", []byte(""))
	if err != nil {
		t.Fatalf("Parse empty: %v", err)
	}
	if len(res.Classes) != 0 || len(res.Methods) != 0 {
		t.Fatalf("expected empty parse result; got %+v", res)
	}
}

// TestPythonParser_ImportStatementShapes verifies both
// `import X` and `from X import a, b` are captured.
func TestPythonParser_ImportStatementShapes(t *testing.T) {
	const src = `import os
import sys as system
from collections import OrderedDict, namedtuple
`
	parser := NewPythonParser()
	res, _ := parser.Parse("x.py", []byte(src))
	if len(res.Imports) != 3 {
		t.Fatalf("expected 3 imports; got %d (%+v)", len(res.Imports), res.Imports)
	}
}
