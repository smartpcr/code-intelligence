//go:build cgo

package ast

import (
	"sort"
	"testing"
)

// TestTreeSitterCParser_LanguageAndExtensions verifies the
// placeholder parser advertises the canonical C language id
// and the `.c` / `.h` extension set from the workstream brief
// §4 "Register extensions". The sibling stage workstream
// `stage-3.1-ctreesitterparser-implementation` will replace
// the stub Parse() with a real walker, but the
// (Language, Extensions) contract MUST stay stable through
// that swap because the dispatcher routes files by extension
// (lower-case) at registration time -- a drift here would
// silently mis-route real `.c` / `.h` source files.
func TestTreeSitterCParser_LanguageAndExtensions(t *testing.T) {
	p := NewTreeSitterCParser()
	if got := p.Language(); got != "c" {
		t.Fatalf("Language: want c, got %q", got)
	}
	want := []string{".c", ".h"}
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

// TestTreeSitterCParser_StubParseEmpty asserts that the
// placeholder Parse() returns an empty ParseResult (no error)
// for valid C input. The sibling stage workstream
// `stage-3.1-ctreesitterparser-implementation` REPLACES this
// test in-place with a fixture-driven walker test that
// asserts emitted Classes / Methods / Imports per the story
// brief §1 ("C: functions, structs, includes, function
// calls"). Until then, this test guards against accidental
// panics in the stub and pins the empty-return contract so a
// half-implemented sibling-stage walker doesn't silently
// regress the result shape.
func TestTreeSitterCParser_StubParseEmpty(t *testing.T) {
	const src = `
#include <stdio.h>

int add(int a, int b) {
  return a + b;
}

int main(void) {
  printf("%d\n", add(2, 3));
  return 0;
}
`
	p := NewTreeSitterCParser()
	res, err := p.Parse("src/main.c", []byte(src))
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

// TestTreeSitterCParser_StubParseHeader confirms the stub
// also handles `.h` header input without error. Once the
// sibling stage lands the real walker, this test should be
// extended to assert that `#include` directives become
// Imports and that struct declarations become ClassDecls.
func TestTreeSitterCParser_StubParseHeader(t *testing.T) {
	const src = `
#ifndef WIDGET_H
#define WIDGET_H

struct Widget {
  int id;
  const char *name;
};

void widget_init(struct Widget *w, int id, const char *name);

#endif
`
	p := NewTreeSitterCParser()
	res, err := p.Parse("include/widget.h", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(res.Classes); got != 0 {
		t.Errorf("stub should return empty Classes; got %d", got)
	}
}
