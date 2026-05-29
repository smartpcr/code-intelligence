//go:build cgo

package ast

import (
	"sort"
	"testing"
)

// TestTreeSitterCSharpParser_LanguageAndExtensions verifies the
// placeholder parser advertises the canonical C# language id
// and the `.cs` extension from the workstream brief §4
// "Register extensions". The sibling stage workstream
// `stage-4.1-csharptreesitterparser-implementation` will
// replace the stub Parse() with a real walker, but the
// (Language, Extensions) contract MUST stay stable through
// that swap because the dispatcher routes files by extension
// (lower-case) at registration time -- a drift here would
// silently mis-route real `.cs` source files.
func TestTreeSitterCSharpParser_LanguageAndExtensions(t *testing.T) {
	p := NewTreeSitterCSharpParser()
	if got := p.Language(); got != "csharp" {
		t.Fatalf("Language: want csharp, got %q", got)
	}
	want := []string{".cs"}
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

// TestTreeSitterCSharpParser_StubParseEmpty asserts that the
// placeholder Parse() returns an empty ParseResult (no error)
// for valid C# input. The sibling stage workstream
// `stage-4.1-csharptreesitterparser-implementation` REPLACES
// this test in-place with a fixture-driven walker test that
// asserts emitted Classes / Methods / Imports per the story
// brief §1 ("C#: classes/interfaces/structs, methods,
// inheritance/interfaces, using directives"). Until then, this
// test guards against accidental panics in the stub and pins
// the empty-return contract so a half-implemented sibling-stage
// walker doesn't silently regress the result shape.
func TestTreeSitterCSharpParser_StubParseEmpty(t *testing.T) {
	const src = `
using System;
using System.Collections.Generic;

namespace Acme.Widgets
{
    public interface IWidget
    {
        int Id { get; }
        void Run();
    }

    public abstract class WidgetBase : IWidget
    {
        public abstract int Id { get; }
        public abstract void Run();
    }

    public sealed class Gadget : WidgetBase
    {
        public override int Id => 42;

        public override void Run()
        {
            Console.WriteLine($"running {Id}");
        }
    }
}
`
	p := NewTreeSitterCSharpParser()
	res, err := p.Parse("src/Acme/Widgets/Gadget.cs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(res.Classes); got != 0 {
		t.Errorf("stub should return empty Classes; got %d (%+v)", got, res.Classes)
	}
	if got := len(res.Methods); got != 0 {
		t.Errorf("stub should return empty Methods; got %d (%+v)", got, res.Methods)
	}
	if got := len(res.Imports); got != 0 {
		t.Errorf("stub should return empty Imports; got %d (%+v)", got, res.Imports)
	}
}

// TestTreeSitterCSharpParser_StubParseStruct confirms the stub
// also handles `struct` declarations without error. Once the
// sibling stage lands the real walker, this test should be
// extended to assert that struct declarations become
// ClassDecls and using directives become Imports.
func TestTreeSitterCSharpParser_StubParseStruct(t *testing.T) {
	const src = `
namespace Acme.Geometry
{
    public struct Point
    {
        public int X;
        public int Y;

        public Point(int x, int y)
        {
            X = x;
            Y = y;
        }
    }
}
`
	p := NewTreeSitterCSharpParser()
	res, err := p.Parse("src/Acme/Geometry/Point.cs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(res.Classes); got != 0 {
		t.Errorf("stub should return empty Classes; got %d", got)
	}
}
