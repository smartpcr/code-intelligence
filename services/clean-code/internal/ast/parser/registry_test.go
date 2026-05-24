package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fixturePath returns the absolute path of a fixture file
// relative to the parser package's testdata directory.
func fixturePath(t *testing.T, sub ...string) string {
	t.Helper()
	parts := append([]string{"testdata"}, sub...)
	abs, err := filepath.Abs(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("filepath.Abs(%v): %v", parts, err)
	}
	return abs
}

// readFixture loads the bytes of a fixture file under testdata/.
func readFixture(t *testing.T, sub ...string) []byte {
	t.Helper()
	content, err := os.ReadFile(fixturePath(t, sub...))
	if err != nil {
		t.Fatalf("read fixture %v: %v", sub, err)
	}
	return content
}

// TestRegistry_SupportsV1FourLanguages exercises scenario
// `parser-supports-v1-four-languages` from implementation-plan
// Stage 2.1. For each v1 language fixture the registry returns
// a Parser; Parse yields an AstFile whose Language matches and
// whose Scopes contain at least one entry (the file scope).
func TestRegistry_SupportsV1FourLanguages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		lang    string
		fixture []string
		path    string
	}{
		{LanguageGo, []string{"go", "sample.go"}, "sample.go"},
		{LanguagePython, []string{"python", "sample.py"}, "sample.py"},
		{LanguageTypeScript, []string{"typescript", "sample.ts"}, "sample.ts"},
		{LanguageJava, []string{"java", "Sample.java"}, "Sample.java"},
	}
	r := DefaultRegistry()
	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			t.Parallel()
			content := readFixture(t, tc.fixture...)
			p, err := r.For(tc.lang)
			if err != nil {
				t.Fatalf("registry.For(%q): %v", tc.lang, err)
			}
			if p.Language() != tc.lang {
				t.Fatalf("Parser.Language()=%q; want %q", p.Language(), tc.lang)
			}
			out, err := p.Parse(context.Background(), tc.path, content)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.path, err)
			}
			if out == nil {
				t.Fatalf("Parse(%q) returned nil *AstFile", tc.path)
			}
			if out.GetLanguage() != tc.lang {
				t.Errorf("AstFile.language=%q; want %q", out.GetLanguage(), tc.lang)
			}
			if len(out.GetScopes()) == 0 {
				t.Errorf("AstFile.scopes is empty; want at least one (the file scope)")
			}
			if out.GetContentSha256() == "" {
				t.Errorf("AstFile.content_sha256 is empty; want hex SHA-256 of the fixture bytes")
			}
			if out.GetParserVersion() == "" {
				t.Errorf("AstFile.parser_version is empty; want %q", ParserVersion)
			}
			// Spot-check the file scope is first and has the
			// canonical SCOPE_KIND_FILE discriminator.
			if got := out.GetScopes()[0].GetScopeKind(); got != ScopeKindFile {
				t.Errorf("first scope kind = %v; want SCOPE_KIND_FILE", got)
			}
		})
	}
}

// TestRegistry_RejectsPostV1Language exercises the registry's
// v1 pin guard. Attempting to register a language outside the
// SupportedLanguages set (e.g. C#) MUST return
// ErrUnsupportedLanguage per tech-spec Sec 8.6.
func TestRegistry_RejectsPostV1Language(t *testing.T) {
	t.Parallel()
	cases := []string{"csharp", "cs", "rust", "ruby", "kotlin", ""}
	r := NewRegistry()
	for _, lang := range cases {
		lang := lang
		t.Run(lang, func(t *testing.T) {
			t.Parallel()
			err := r.Register(lang, func() Parser { return &goParser{} })
			if err == nil {
				t.Fatalf("Register(%q) returned nil; want ErrUnsupportedLanguage", lang)
			}
			if !errorIs(err, ErrUnsupportedLanguage) {
				t.Fatalf("Register(%q) returned %v; want ErrUnsupportedLanguage", lang, err)
			}
		})
	}
}

// TestRegistry_RejectsNilFactory asserts the registry refuses
// nil factories so a wiring typo surfaces at registration time.
func TestRegistry_RejectsNilFactory(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(LanguageGo, nil); err == nil {
		t.Fatalf("Register(%q, nil) returned nil; want non-nil factory error", LanguageGo)
	}
}

// TestRegistry_RejectsDoubleRegistration asserts duplicate
// registration fails so a typo'd `init()` reads loudly.
func TestRegistry_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(LanguageGo, func() Parser { return &goParser{} }); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(LanguageGo, func() Parser { return &goParser{} }); err == nil {
		t.Fatalf("second Register returned nil; want already-registered error")
	}
}

// TestDefaultRegistry_HasAllFourLanguages asserts the
// process-wide registry is pre-populated with the four
// v1-pinned languages via `init()`.
func TestDefaultRegistry_HasAllFourLanguages(t *testing.T) {
	t.Parallel()
	r := DefaultRegistry()
	got := r.Languages()
	want := map[string]bool{
		LanguageGo: true, LanguagePython: true,
		LanguageTypeScript: true, LanguageJava: true,
	}
	if len(got) != len(want) {
		t.Fatalf("Languages() returned %v; want exactly %v", got, sortedKeys(want))
	}
	for _, l := range got {
		if !want[l] {
			t.Fatalf("Languages() returned unexpected language %q; want subset of %v", l, sortedKeys(want))
		}
	}
}

// TestRegistry_ParseDispatchesByPath exercises the convenience
// Parse method that routes via DetectLanguage.
func TestRegistry_ParseDispatchesByPath(t *testing.T) {
	t.Parallel()
	content := readFixture(t, "go", "sample.go")
	out, err := DefaultRegistry().Parse(context.Background(), "sample.go", content)
	if err != nil {
		t.Fatalf("Registry.Parse: %v", err)
	}
	if out.GetLanguage() != LanguageGo {
		t.Fatalf("AstFile.language=%q; want %q", out.GetLanguage(), LanguageGo)
	}
}

// TestRegistry_ParseRejectsUnknownPath asserts a file with an
// unsniffable extension surfaces ErrUnsupportedLanguage.
func TestRegistry_ParseRejectsUnknownPath(t *testing.T) {
	t.Parallel()
	_, err := DefaultRegistry().Parse(context.Background(), "data.bin", []byte("BIN"))
	if err == nil {
		t.Fatalf("Registry.Parse returned nil err; want ErrUnsupportedLanguage")
	}
	if !errorIs(err, ErrUnsupportedLanguage) {
		t.Fatalf("Registry.Parse returned %v; want ErrUnsupportedLanguage", err)
	}
}

// errorIs is a small re-implementation of errors.Is that
// avoids the import-cycle awkwardness of importing "errors" in
// test-helper code; matches by chain equality.
func errorIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Use the parser package's own DefaultRegistry().Languages
	// behaviour as ground truth; sort.Strings is shadowed by
	// this helper to keep the test file import-light.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
