package scope_test

import (
	"errors"
	"testing"

	"forge/services/clean-code/internal/ast/scope"
)

// TestNormalizeSignature_AgentMemoryParity asserts our
// [scope.NormalizeSignature] produces the same output as
// agent-memory's `NormalizeSignature` for every example pinned
// in the agent-memory whitespace doc comments. If this drifts,
// the cross-service `agent_memory_node_id` link in linked-mode
// silently breaks because the two services would compute
// different canonical signatures for the same input.
func TestNormalizeSignature_AgentMemoryParity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Per agent-memory whitespace.go: `Map<K, V>` and
		// `Map < K , V >` and `Map<K,V>` MUST collapse to the
		// same normalised string.
		{"generic with spaces", "Map<K, V>", "Map<K,V>"},
		{"generic with extra spaces", "Map < K , V >", "Map<K,V>"},
		{"generic no spaces", "Map<K,V>", "Map<K,V>"},
		// Line comments stripped (note: end-of-line `\n` required
		// for the line-comment scanner to find a terminator).
		{"line comment go", "int // count\n", "int"},
		{"line comment python", "int # count\n", "int"},
		// Block comments collapsed to a single space then
		// stripped around adjacent punctuation.
		{"block comment", "int /* note */, string", "int,string"},
		// Whitespace runs collapse.
		{"tabs and newlines", "int\t\nstring", "int string"},
		// Trim leading/trailing.
		{"trim", "   int  ", "int"},
		// Multibyte whitespace (NBSP, ideographic space).
		{"multibyte ws", "int\u00A0\u3000string", "int string"},
		// Empty stays empty.
		{"empty", "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scope.NormalizeSignature(tc.in)
			if got != tc.want {
				t.Errorf("NormalizeSignature(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildRepo asserts the repo-kind signature recipe matches
// agent-memory's: simply the repo URL.
func TestBuildRepo(t *testing.T) {
	t.Parallel()
	got, err := scope.BuildRepo("github.com/acme/repo")
	if err != nil {
		t.Fatalf("BuildRepo: %v", err)
	}
	if want := "github.com/acme/repo"; got != want {
		t.Errorf("BuildRepo = %q, want %q", got, want)
	}
	if _, err := scope.BuildRepo(""); !errors.Is(err, scope.ErrEmptyField) {
		t.Errorf("BuildRepo(\"\") expected ErrEmptyField, got %v", err)
	}
	if _, err := scope.BuildRepo("github.com/x\x00y"); !errors.Is(err, scope.ErrEmbeddedNUL) {
		t.Errorf("BuildRepo with NUL expected ErrEmbeddedNUL, got %v", err)
	}
}

// TestBuildPackage asserts the package-kind signature recipe
// `<repoURL>::pkg::<dir>` and that the dir is NOT normalised
// (paths can legitimately contain `,` `:` `;` etc. and the
// punctuation stripper would mangle them).
func TestBuildPackage(t *testing.T) {
	t.Parallel()
	got, err := scope.BuildPackage("github.com/acme/repo", "internal/storage")
	if err != nil {
		t.Fatalf("BuildPackage: %v", err)
	}
	if want := "github.com/acme/repo::pkg::internal/storage"; got != want {
		t.Errorf("BuildPackage = %q, want %q", got, want)
	}
	// Dir with leading/trailing whitespace MUST be preserved
	// verbatim (paths are authoritative, not normalised). This
	// is intentionally different from class/method qualifiedName.
	got, err = scope.BuildPackage("github.com/acme/repo", " a/b ")
	if err != nil {
		t.Fatalf("BuildPackage with whitespace: %v", err)
	}
	if want := "github.com/acme/repo::pkg:: a/b "; got != want {
		t.Errorf("BuildPackage with whitespace = %q, want %q (paths NOT normalised)", got, want)
	}
	// Empty dir is invalid (an empty dir would mean the repo
	// root, which is the repo-kind scope, not the package-kind
	// scope).
	if _, err := scope.BuildPackage("github.com/acme/repo", ""); !errors.Is(err, scope.ErrEmptyField) {
		t.Errorf("BuildPackage with empty dir expected ErrEmptyField, got %v", err)
	}
}

// TestBuildFile asserts the file-kind signature recipe
// `<repoURL>::file::<relPath>`.
func TestBuildFile(t *testing.T) {
	t.Parallel()
	got, err := scope.BuildFile("github.com/acme/repo", "internal/storage/migrate.go")
	if err != nil {
		t.Fatalf("BuildFile: %v", err)
	}
	if want := "github.com/acme/repo::file::internal/storage/migrate.go"; got != want {
		t.Errorf("BuildFile = %q, want %q", got, want)
	}
}

// TestBuildClass asserts the class-kind signature recipe
// `<repoURL>::class::<relPath>#<NormalizeSignature(qualifiedName)>`.
// In particular, qualifiedName IS run through the normaliser
// (so a generic-type formatter commit produces a stable
// signature).
func TestBuildClass(t *testing.T) {
	t.Parallel()
	got, err := scope.BuildClass("github.com/acme/repo", "src/Foo.go", "pkg.Foo")
	if err != nil {
		t.Fatalf("BuildClass: %v", err)
	}
	if want := "github.com/acme/repo::class::src/Foo.go#pkg.Foo"; got != want {
		t.Errorf("BuildClass = %q, want %q", got, want)
	}
	// Generic-param whitespace normalises away.
	got, err = scope.BuildClass("github.com/acme/repo", "src/Foo.go", "pkg.Map < K , V >")
	if err != nil {
		t.Fatalf("BuildClass with generic: %v", err)
	}
	if want := "github.com/acme/repo::class::src/Foo.go#pkg.Map<K,V>"; got != want {
		t.Errorf("BuildClass with generic = %q, want %q", got, want)
	}
}

// TestBuildInterface asserts the interface-kind signature
// recipe uses the `::class::` discriminator (NOT `::interface::`)
// to remain byte-identical to agent-memory's `classSignature`,
// which folds Class and Interface nodes into one signature
// shape. Linked-mode `agent_memory_node_id` resolution depends
// on this parity; the `scope_kind` discrimination still happens
// in the `scope_id` derivation pre-image (so two ScopeBindings
// for the same qualifiedName -- one class, one interface -- get
// the SAME canonical_signature string but DIFFERENT scope_ids).
func TestBuildInterface(t *testing.T) {
	t.Parallel()
	got, err := scope.BuildInterface("github.com/acme/repo", "src/Reader.go", "pkg.Reader")
	if err != nil {
		t.Fatalf("BuildInterface: %v", err)
	}
	// Same as class -- agent-memory parity.
	if want := "github.com/acme/repo::class::src/Reader.go#pkg.Reader"; got != want {
		t.Errorf("BuildInterface = %q, want %q", got, want)
	}
	// A class and an interface with the same relPath +
	// qualifiedName MUST produce the SAME canonical signature
	// (agent-memory parity). Differentiation is done at the
	// scope_id layer via scope_kind in the pre-image, NOT at
	// the canonical_signature layer.
	cls, _ := scope.BuildClass("github.com/acme/repo", "src/Reader.go", "pkg.Reader")
	if cls != got {
		t.Errorf("class and interface produced DIFFERENT canonical signatures (%q vs %q); "+
			"agent-memory parity requires them to match -- the scope_id pre-image carries the kind discriminator", cls, got)
	}
}

// TestBuildMethod asserts the method-kind signature recipe
// `<repoURL>::method::<relPath>#<NormalizeSignature(qualifiedName)>(<NormalizeSignature(joinedParams)>)`.
// The brief example `pkg.Foo#bar(int)` is realised here.
func TestBuildMethod(t *testing.T) {
	t.Parallel()
	// The brief example `pkg.Foo#bar(int)` is the RECIPE OUTPUT
	// where `#` is the recipe-level separator between `<relPath>`
	// and the normalised qualifiedName. The qualifiedName input
	// itself MUST NOT contain `#` because [NormalizeSignature]
	// strips `#...\n` runs as Python-style line comments
	// (agent-memory parity); a producer that passes
	// `pkg.Foo#bar` would see `pkg.Foo` round-trip. The Java /
	// Go / Python parser layer emits dot-separated qualified
	// names (`pkg.Foo.bar`), so this is not a practical
	// constraint.
	got, err := scope.BuildMethod("github.com/acme/repo", "src/foo.go", "pkg.Foo.bar", []string{"int"})
	if err != nil {
		t.Fatalf("BuildMethod: %v", err)
	}
	if want := "github.com/acme/repo::method::src/foo.go#pkg.Foo.bar(int)"; got != want {
		t.Errorf("BuildMethod = %q, want %q", got, want)
	}
	// Multiple params joined with `,` then normalised.
	got, err = scope.BuildMethod("github.com/acme/repo", "src/foo.go", "pkg.Foo.bar", []string{"int", "*Bar"})
	if err != nil {
		t.Fatalf("BuildMethod multi: %v", err)
	}
	if want := "github.com/acme/repo::method::src/foo.go#pkg.Foo.bar(int,*Bar)"; got != want {
		t.Errorf("BuildMethod multi = %q, want %q", got, want)
	}
	// No-arg method: empty params yields an empty `()` group.
	got, err = scope.BuildMethod("github.com/acme/repo", "src/foo.go", "pkg.Foo.bar", nil)
	if err != nil {
		t.Fatalf("BuildMethod nil params: %v", err)
	}
	if want := "github.com/acme/repo::method::src/foo.go#pkg.Foo.bar()"; got != want {
		t.Errorf("BuildMethod no-arg = %q, want %q", got, want)
	}
	// Whitespace-noise inside params normalises away (stability
	// against formatter-only commits).
	got, err = scope.BuildMethod("github.com/acme/repo", "src/foo.go", "pkg.Foo.bar", []string{"int", " *Bar "})
	if err != nil {
		t.Fatalf("BuildMethod whitespace params: %v", err)
	}
	if want := "github.com/acme/repo::method::src/foo.go#pkg.Foo.bar(int,*Bar)"; got != want {
		t.Errorf("BuildMethod whitespace params = %q, want %q", got, want)
	}
	// NUL inside a param is rejected to preserve the framing
	// invariant in DeriveScopeID's pre-image.
	if _, err := scope.BuildMethod("github.com/acme/repo", "src/foo.go", "pkg.Foo.bar", []string{"int", "be\x00ef"}); !errors.Is(err, scope.ErrEmbeddedNUL) {
		t.Errorf("BuildMethod NUL param expected ErrEmbeddedNUL, got %v", err)
	}
}

// TestBuildBlock asserts the block-kind signature recipe
// `<methodSig>#block_<ordinal>_<kind>` per agent-memory's
// `blockSignature`. Ordinals are 0-based (agent-memory parity).
func TestBuildBlock(t *testing.T) {
	t.Parallel()
	methodSig := "github.com/acme/repo::method::src/foo.go#pkg.Foo.bar(int)"
	// 0-based ordinal: the FIRST block is `#block_0_<kind>`,
	// NOT `#block_1_...`. Matches agent-memory's
	// `Block.Ordinal` doc ("0-based position") and the
	// `blockSignature` emission shape.
	got, err := scope.BuildBlock(methodSig, 0, "entry")
	if err != nil {
		t.Fatalf("BuildBlock ordinal=0: %v", err)
	}
	want := "github.com/acme/repo::method::src/foo.go#pkg.Foo.bar(int)#block_0_entry"
	if got != want {
		t.Errorf("BuildBlock ordinal=0 = %q, want %q", got, want)
	}
	// Second block is `#block_1_<kind>`.
	got, err = scope.BuildBlock(methodSig, 1, "exit")
	if err != nil {
		t.Fatalf("BuildBlock ordinal=1: %v", err)
	}
	want = "github.com/acme/repo::method::src/foo.go#pkg.Foo.bar(int)#block_1_exit"
	if got != want {
		t.Errorf("BuildBlock ordinal=1 = %q, want %q", got, want)
	}
	// Negative ordinal is a parser bug.
	if _, err := scope.BuildBlock(methodSig, -1, "entry"); err == nil {
		t.Error("BuildBlock with ordinal=-1 expected error, got nil")
	}
	// Empty kind / methodSig rejected.
	if _, err := scope.BuildBlock(methodSig, 0, ""); !errors.Is(err, scope.ErrEmptyField) {
		t.Errorf("BuildBlock empty kind expected ErrEmptyField, got %v", err)
	}
	if _, err := scope.BuildBlock("", 0, "entry"); !errors.Is(err, scope.ErrEmptyField) {
		t.Errorf("BuildBlock empty methodSig expected ErrEmptyField, got %v", err)
	}
}

// TestBuilders_RejectNUL gives every per-kind builder the NUL-
// byte rejection check the [DeriveScopeID] pre-image relies on.
func TestBuilders_RejectNUL(t *testing.T) {
	t.Parallel()
	bad := "x\x00y"
	checks := []struct {
		name string
		fn   func() (string, error)
	}{
		{"BuildRepo", func() (string, error) { return scope.BuildRepo(bad) }},
		{"BuildPackage repoURL", func() (string, error) { return scope.BuildPackage(bad, "d") }},
		{"BuildPackage dir", func() (string, error) { return scope.BuildPackage("u", bad) }},
		{"BuildFile repoURL", func() (string, error) { return scope.BuildFile(bad, "f") }},
		{"BuildFile relPath", func() (string, error) { return scope.BuildFile("u", bad) }},
		{"BuildClass repoURL", func() (string, error) { return scope.BuildClass(bad, "p", "q") }},
		{"BuildClass relPath", func() (string, error) { return scope.BuildClass("u", bad, "q") }},
		{"BuildClass qn", func() (string, error) { return scope.BuildClass("u", "p", bad) }},
		{"BuildInterface repoURL", func() (string, error) { return scope.BuildInterface(bad, "p", "q") }},
		{"BuildMethod relPath", func() (string, error) { return scope.BuildMethod("u", bad, "q", nil) }},
		{"BuildBlock methodSig", func() (string, error) { return scope.BuildBlock(bad, 0, "k") }},
	}
	for _, c := range checks {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.fn()
			if !errors.Is(err, scope.ErrEmbeddedNUL) {
				t.Errorf("%s with NUL: expected ErrEmbeddedNUL, got %v", c.name, err)
			}
		})
	}
}
