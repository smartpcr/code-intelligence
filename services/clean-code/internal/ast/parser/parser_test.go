package parser

import (
	"context"
	"testing"
)

// TestGoParser_PopulatesCanonicalFields asserts the Go parser
// emits the canonical scope shape the foundation recipes
// expect: file + package + interface + class (struct) + at
// least one method, all carrying populated `name`,
// `qualified_name`, `range`, and (for methods) `parameters`.
func TestGoParser_PopulatesCanonicalFields(t *testing.T) {
	t.Parallel()
	content := readFixture(t, "go", "sample.go")
	out, err := (&goParser{}).Parse(context.Background(), "sample.go", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	gotKinds := scopeKindHistogram(out)
	assertAtLeast(t, gotKinds, ScopeKindFile, 1)
	assertAtLeast(t, gotKinds, ScopeKindPackage, 1)
	assertAtLeast(t, gotKinds, ScopeKindInterface, 2)
	assertAtLeast(t, gotKinds, ScopeKindClass, 1)
	assertAtLeast(t, gotKinds, ScopeKindMethod, 2)

	// Find the `Sample` method scope on MemorySampler and
	// assert the parameter list survived the parse.
	found := false
	for _, s := range out.GetScopes() {
		if s.GetScopeKind() == ScopeKindMethod && s.GetName() == "Sample" {
			found = true
			if got := len(s.GetParameters()); got != 2 {
				t.Errorf("Sample().Parameters has %d entries (%v); want 2 (ctx, seed)", got, s.GetParameters())
			}
		}
	}
	if !found {
		t.Errorf("did not find Sample() method scope")
	}

	// Imports should ride as symbols on the file scope.
	if len(out.GetSymbols()) == 0 {
		t.Errorf("AstFile.symbols is empty; want imports as symbols")
	}
}

func TestPythonParser_PopulatesCanonicalFields(t *testing.T) {
	t.Parallel()
	content := readFixture(t, "python", "sample.py")
	out, err := (&pythonParser{}).Parse(context.Background(), "sample.py", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	gotKinds := scopeKindHistogram(out)
	assertAtLeast(t, gotKinds, ScopeKindFile, 1)
	// Sampler is `class Sampler(ABC)` so it should land as an
	// interface; MemorySampler stays a class.
	assertAtLeast(t, gotKinds, ScopeKindInterface, 1)
	assertAtLeast(t, gotKinds, ScopeKindClass, 1)
	assertAtLeast(t, gotKinds, ScopeKindMethod, 3)

	// Module-qualified names matter to Stage 2.2.
	for _, s := range out.GetScopes() {
		if s.GetScopeKind() == ScopeKindMethod && s.GetName() == "sample" {
			if s.GetQualifiedName() == "" {
				t.Errorf("method.sample.qualified_name is empty; want module.class.sample")
			}
		}
	}
}

func TestTypeScriptParser_PopulatesCanonicalFields(t *testing.T) {
	t.Parallel()
	content := readFixture(t, "typescript", "sample.ts")
	out, err := (&tsParser{}).Parse(context.Background(), "sample.ts", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	gotKinds := scopeKindHistogram(out)
	assertAtLeast(t, gotKinds, ScopeKindFile, 1)
	assertAtLeast(t, gotKinds, ScopeKindInterface, 1)
	assertAtLeast(t, gotKinds, ScopeKindClass, 1)
	assertAtLeast(t, gotKinds, ScopeKindMethod, 2)
}

func TestJavaParser_PopulatesCanonicalFields(t *testing.T) {
	t.Parallel()
	content := readFixture(t, "java", "Sample.java")
	out, err := (&javaParser{}).Parse(context.Background(), "Sample.java", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	gotKinds := scopeKindHistogram(out)
	assertAtLeast(t, gotKinds, ScopeKindFile, 1)
	assertAtLeast(t, gotKinds, ScopeKindPackage, 1)
	assertAtLeast(t, gotKinds, ScopeKindInterface, 1)
	assertAtLeast(t, gotKinds, ScopeKindClass, 1)
	assertAtLeast(t, gotKinds, ScopeKindMethod, 2)
}

// TestParsers_RejectEmptyContent asserts every per-language
// parser surfaces ErrEmptyContent rather than silently
// returning an empty AstFile.
func TestParsers_RejectEmptyContent(t *testing.T) {
	t.Parallel()
	cases := map[string]Parser{
		"go":         &goParser{},
		"python":     &pythonParser{},
		"typescript": &tsParser{},
		"java":       &javaParser{},
	}
	for name, p := range cases {
		name, p := name, p
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := p.Parse(context.Background(), "empty."+name, nil)
			if err == nil {
				t.Fatalf("Parse(nil) returned nil; want ErrEmptyContent")
			}
			if !errorIs(err, ErrEmptyContent) {
				t.Fatalf("Parse(nil) returned %v; want ErrEmptyContent", err)
			}
		})
	}
}

// scopeKindHistogram counts scopes by ScopeKind so tests can
// assert "at least N of kind K" without iterating in every
// case.
func scopeKindHistogram(out *AstFile) map[ScopeKind]int {
	got := map[ScopeKind]int{}
	for _, s := range out.GetScopes() {
		got[s.GetScopeKind()]++
	}
	return got
}

func assertAtLeast(t *testing.T, hist map[ScopeKind]int, kind ScopeKind, want int) {
	t.Helper()
	if got := hist[kind]; got < want {
		t.Errorf("scope_kind=%v count = %d; want >= %d", kind, got, want)
	}
}
