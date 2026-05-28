package ast

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// fakeStaticParser is a LanguageParser whose Parse method
// returns a caller-supplied ParseResult / error verbatim,
// without inspecting the source bytes. Used by the
// dispatcher Pass 2b / Pass 2d unit tests to drive specific
// receiver-collision, alias-resolution, and trait-override
// scenarios that the real per-language parsers cannot
// produce on demand.
type fakeStaticParser struct {
	lang       string
	extensions []string
	result     ParseResult
	err        error
}

func (f fakeStaticParser) Language() string     { return f.lang }
func (f fakeStaticParser) Extensions() []string { return f.extensions }
func (f fakeStaticParser) Parse(_ string, _ []byte) (ParseResult, error) {
	return f.result, f.err
}

// TestDispatcher_ErrParserUnavailable_LogsSkip pins
// tech-spec.md §7 row `TestDispatcher_ErrParserUnavailable_LogsSkip`
// and `dispatcher.go::EmitFile` lines 196-215: a parser that
// returns `ErrParserUnavailable` (typically because a
// required runtime tool like `pwsh` is missing on PATH)
// MUST be treated as a SKIP, not a parse failure -- the
// dispatcher logs `ast.dispatch.skip{reason=<slug>}` at Info
// level and returns `EmitResult{}, nil` so the surrounding
// worker keeps draining its queue.
//
// The wrapped reason slug (`reason=pwsh_not_available` per
// the convention documented on `parser.go::ErrParserUnavailable`)
// surfaces on the structured log. A wrapper omitting the
// slug must fall back to `reason="runtime_unavailable"`
// (covered by the no-slug sub-test).
func TestDispatcher_ErrParserUnavailable_LogsSkip(t *testing.T) {
	cases := []struct {
		name       string
		wrapErr    error
		wantReason string
	}{
		{
			name:       "with reason slug",
			wrapErr:    fmt.Errorf("powershell: %w (reason=pwsh_not_available)", ErrParserUnavailable),
			wantReason: "pwsh_not_available",
		},
		{
			name:       "without reason slug falls back",
			wrapErr:    fmt.Errorf("powershell: %w", ErrParserUnavailable),
			wantReason: "runtime_unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fw := newFakeWriter()
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			parser := fakeStaticParser{
				lang:       "powershell",
				extensions: []string{".ps1"},
				err:        tc.wrapErr,
			}
			d := NewDispatcher(fw, WithParsers(parser), WithLogger(logger))

			got, err := d.EmitFile(context.Background(), makeEvent("scripts/run.ps1", "Write-Host hi"))
			if err != nil {
				t.Fatalf("EmitFile returned error; want nil: %v", err)
			}
			if len(got.TouchedNodes) != 0 {
				t.Errorf("TouchedNodes = %d; want 0 (skip branch must emit nothing)", len(got.TouchedNodes))
			}
			if len(fw.nodes) != 0 {
				t.Errorf("inserted nodes = %d; want 0 on skip", len(fw.nodes))
			}
			if len(fw.edges) != 0 {
				t.Errorf("inserted edges = %d; want 0 on skip", len(fw.edges))
			}

			out := buf.String()
			if !strings.Contains(out, "ast.dispatch.skip") {
				t.Errorf("log missing ast.dispatch.skip; got:\n%s", out)
			}
			if !strings.Contains(out, "reason="+tc.wantReason) {
				t.Errorf("log missing reason=%s; got:\n%s", tc.wantReason, out)
			}
			if !strings.Contains(out, "language=powershell") {
				t.Errorf("log missing language=powershell; got:\n%s", out)
			}
			if strings.Contains(out, "ast.parse.error") {
				t.Errorf("log contains ast.parse.error; the sentinel branch must NOT log a parse error. got:\n%s", out)
			}
			// Info-level skip is operationally meaningful per
			// the workstream brief; verify the level token is
			// present so a future regression that silently
			// downgrades to Debug is caught.
			if !strings.Contains(out, "level=INFO") {
				t.Errorf("log missing INFO level on skip; got:\n%s", out)
			}
		})
	}
}

// TestDispatcher_GoMultimapDropsOnReceiverCollision pins
// tech-spec.md §7 row `TestDispatcher_GoMultimapDropsOnReceiverCollision`
// and the A5 drop-on-ambiguity rule in `dispatcher.go::emit`
// Pass 2b (lines 612-690). When a Go file declares BOTH a
// value-receiver and a pointer-receiver method whose simple
// name collides -- `func (r Foo) Bar()` AND
// `func (r *Foo) Bar()` -- a sibling method's `r.Bar()`
// call resolves through the receiver-index multimap into
// TWO candidate nodes (Foo.Bar and *Foo.Bar). The A5 rule
// drops the callee per the same "wrong edge is worse than
// missing edge" principle the bare-name resolver uses.
func TestDispatcher_GoMultimapDropsOnReceiverCollision(t *testing.T) {
	fw := newFakeWriter()
	parser := fakeStaticParser{
		lang:       "go",
		extensions: []string{".go"},
		result: ParseResult{
			Classes: []ClassDecl{
				{QualifiedName: "Foo", Kind: "struct"},
			},
			Methods: []MethodDecl{
				// Value-receiver Bar -- QualifiedName has no
				// `*` prefix; ReceiverAliases nil per the v1
				// Go parser contract (value receivers don't
				// need an alias because their primary key
				// already matches the multimap formula).
				{QualifiedName: "Foo.Bar", EnclosingClass: "Foo"},
				// Pointer-receiver Bar -- the operator-pinned
				// `*` prefix lives in QualifiedName, while the
				// `Foo.Bar` alias mirrors the formula a
				// receiver-qualified caller would generate
				// (architecture Section 4.5.1).
				{
					QualifiedName:   "*Foo.Bar",
					EnclosingClass:  "Foo",
					ReceiverAliases: []string{"Foo.Bar"},
				},
				// A third method that calls Bar via receiver
				// qualification; its only role here is to
				// trigger Pass 2b's receiver-index lookup so
				// the assertion below has something to drop.
				{
					QualifiedName:  "Foo.caller",
					EnclosingClass: "Foo",
					ReceiverCalls:  []string{"Bar"},
				},
			},
		},
	}
	d := NewDispatcher(fw, WithParsers(parser))
	if _, err := d.EmitFile(context.Background(), makeEvent("src/foo.go", "// fake")); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	// Sanity: the fake parser must have driven the full
	// Pass 1 insert protocol -- otherwise the static_calls
	// assertion below would pass vacuously.
	if n := len(fw.nodesOf("class")); n != 1 {
		t.Fatalf("class nodes = %d; want 1 (fake parser was not selected)", n)
	}
	if n := len(fw.nodesOf("method")); n != 3 {
		t.Fatalf("method nodes = %d; want 3 (Foo.Bar value, *Foo.Bar pointer, Foo.caller)", n)
	}

	// The collision MUST drop the receiver-qualified edge.
	if calls := fw.edgesOf("static_calls"); len(calls) != 0 {
		t.Errorf("static_calls edges = %d; want 0 (A5 drop on receiverIndex size > 1); edges=%+v",
			len(calls), calls)
	}
}

// TestDispatcher_GoMultimapResolvesPointerReceiverAlone pins
// tech-spec.md §7 row `TestDispatcher_GoMultimapResolvesPointerReceiverAlone`:
// when ONLY a pointer-receiver method exists (`func (r *Foo) Bar()`),
// a sibling method's `r.Bar()` MUST resolve to the pointer
// receiver's node via the `ReceiverAliases` entry that maps
// the canonical `Foo.Bar` multimap key onto the
// `*Foo.Bar` node id. The receiver-index dedup helper in
// `dispatcher.go::emit` keeps the multimap entry size at 1
// even though the primary key (`Foo.`+simpleName(`*Foo.Bar`)
// = `Foo.Bar`) and the alias key (`Foo.Bar`) collide on the
// same node id; the resulting `len(ids) == 1` lookup emits
// exactly one edge.
func TestDispatcher_GoMultimapResolvesPointerReceiverAlone(t *testing.T) {
	fw := newFakeWriter()
	parser := fakeStaticParser{
		lang:       "go",
		extensions: []string{".go"},
		result: ParseResult{
			Classes: []ClassDecl{
				{QualifiedName: "Foo", Kind: "struct"},
			},
			Methods: []MethodDecl{
				// Pointer-receiver Bar -- the only Bar on Foo.
				// Without the ReceiverAliases entry there is
				// no key the receiver-qualified caller could
				// match against, because the caller resolves
				// against `<EnclosingClass>.<callee>` which
				// produces `Foo.Bar`, not `*Foo.Bar`.
				{
					QualifiedName:   "*Foo.Bar",
					EnclosingClass:  "Foo",
					ReceiverAliases: []string{"Foo.Bar"},
				},
				{
					QualifiedName:  "Foo.caller",
					EnclosingClass: "Foo",
					ReceiverCalls:  []string{"Bar"},
				},
			},
		},
	}
	d := NewDispatcher(fw, WithParsers(parser))
	if _, err := d.EmitFile(context.Background(), makeEvent("src/foo.go", "// fake")); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	if n := len(fw.nodesOf("class")); n != 1 {
		t.Fatalf("class nodes = %d; want 1 (fake parser was not selected)", n)
	}
	methods := fw.nodesOf("method")
	if len(methods) != 2 {
		t.Fatalf("method nodes = %d; want 2 (*Foo.Bar pointer, Foo.caller)", len(methods))
	}

	// The order of inserts is class(Foo)=node-0,
	// method(*Foo.Bar)=node-1, method(Foo.caller)=node-2.
	// Pass 2b must emit a single edge node-2 -> node-1.
	calls := fw.edgesOf("static_calls")
	if len(calls) != 1 {
		t.Fatalf("static_calls edges = %d; want 1 (pointer alias must resolve); edges=%+v",
			len(calls), calls)
	}
	if calls[0].SrcNodeID != "node-2" || calls[0].DstNodeID != "node-1" {
		t.Errorf("edge = %s -> %s; want node-2 -> node-1 (Foo.caller -> *Foo.Bar)",
			calls[0].SrcNodeID, calls[0].DstNodeID)
	}
}

// TestDispatcher_Rust_TraitOverrides_SameFile pins
// tech-spec.md §7 row `Rust same-file override edge` /
// §8 risk R4 and `dispatcher.go::emit` Pass 2d (lines
// 753-803): a Rust impl method whose `LangMeta["trait"]`
// names a trait whose default-bodied method lives in the
// SAME file MUST emit an `overrides` edge from the impl
// method node to the trait default-method node. Cross-
// file pairs are dropped per A4 -- the trait identity
// persists on `LangMeta["trait"]` for the future
// cross-file resolver.
func TestDispatcher_Rust_TraitOverrides_SameFile(t *testing.T) {
	fw := newFakeWriter()
	parser := fakeStaticParser{
		lang:       "rust",
		extensions: []string{".rs"},
		result: ParseResult{
			Classes: []ClassDecl{
				// The trait carries the default method. Its
				// QualifiedName is the trait name; the trait
				// method below uses `Greeter.greet` as its
				// QualifiedName so methodNodeID is keyed by
				// the same string Pass 2d's `dstKey` builds
				// (`traitName + "." + simpleName(impl)`).
				{QualifiedName: "Greeter", Kind: "trait"},
				{QualifiedName: "MyType", Kind: "struct"},
			},
			Methods: []MethodDecl{
				// Trait default body. No LangMeta["trait"]
				// because this IS the trait's own method,
				// not an impl override.
				{QualifiedName: "Greeter.greet", EnclosingClass: "Greeter"},
				// Impl override. LangMeta["trait"] points at
				// the trait whose default this method shadows.
				{
					QualifiedName:  "MyType.greet",
					EnclosingClass: "MyType",
					LangMeta:       map[string]any{"trait": "Greeter"},
				},
			},
		},
	}
	d := NewDispatcher(fw, WithParsers(parser))
	if _, err := d.EmitFile(context.Background(), makeEvent("src/lib.rs", "// fake")); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	if n := len(fw.nodesOf("class")); n != 2 {
		t.Fatalf("class nodes = %d; want 2 (Greeter trait, MyType struct)", n)
	}
	if n := len(fw.nodesOf("method")); n != 2 {
		t.Fatalf("method nodes = %d; want 2 (Greeter.greet, MyType.greet)", n)
	}

	// Pass 1 insert order: class(Greeter)=node-0,
	// class(MyType)=node-1, method(Greeter.greet)=node-2,
	// method(MyType.greet)=node-3. Pass 2d must emit a
	// single overrides edge node-3 -> node-2.
	overrides := fw.edgesOf("overrides")
	if len(overrides) != 1 {
		t.Fatalf("overrides edges = %d; want 1; edges=%+v", len(overrides), overrides)
	}
	if overrides[0].SrcNodeID != "node-3" || overrides[0].DstNodeID != "node-2" {
		t.Errorf("overrides edge = %s -> %s; want node-3 -> node-2 (MyType.greet -> Greeter.greet)",
			overrides[0].SrcNodeID, overrides[0].DstNodeID)
	}
}
