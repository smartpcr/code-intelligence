//go:build !cgo

package ast

import (
	"context"
	"testing"
)

// TestDefaultParsers_NoCGOOmitsRust pins the documented
// no-CGO gap: when the binary is built with CGO_ENABLED=0
// (the portable Windows `make test` path), the Rust parser
// is NOT registered. Files ending in `.rs` fall through the
// dispatcher without producing class/method nodes -- this
// is intentional and is the behavior the no-CGO doc note
// in parsers_nocgo.go promises (implementation-plan.md
// §5.2 line 355 "no-CGO skip").
//
// Adding a scanner-fallback Rust parser would require a
// separate workstream (Stage 5.4 was descoped per the
// merged plan); until then, the contract is "no Rust
// parser under CGO=0".
func TestDefaultParsers_NoCGOOmitsRust(t *testing.T) {
	parsers := defaultParsers()
	for _, p := range parsers {
		if p.Language() == "rust" {
			t.Errorf("defaultParsers() under !cgo unexpectedly registered Language()=\"rust\"; the no-CGO build must NOT include the tree-sitter Rust parser")
		}
		for _, ext := range p.Extensions() {
			if ext == ".rs" {
				t.Errorf("defaultParsers() under !cgo unexpectedly claims the .rs extension via parser %q; this would silently route Rust files to a non-Rust parser", p.Language())
			}
		}
	}
}

// TestDispatcher_NoCGOSkipsRsFilesWithoutWritingNodes is the
// EmitFile-driven companion to TestDefaultParsers_NoCGOOmitsRust.
// It pins evaluator iter-3 finding #4: the previous no-CGO
// test only scanned `defaultParsers()` for absence, which
// would not catch a regression where a fallback parser was
// added and silently claimed `.rs` (or where the dispatcher
// routed `.rs` through its `LanguageHints` path despite no
// registered extension). This test exercises the dispatcher's
// full `EmitFile` path and asserts the contract documented in
// parsers_nocgo.go: a `.rs` file under CGO=0 produces ZERO
// nodes, ZERO edges, and ZERO TouchedNodes -- a clean
// no-parser skip (dispatcher.go::EmitFile lines 185-189).
func TestDispatcher_NoCGOSkipsRsFilesWithoutWritingNodes(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	const src = `pub struct Greeter {
    prefix: String,
}

pub fn make_greeter() -> Greeter {
    Greeter { prefix: String::new() }
}
`
	res, err := d.EmitFile(context.Background(), makeEvent("src/hello.rs", src))
	if err != nil {
		t.Fatalf("EmitFile: %v; want nil (the no-parser skip must NOT surface as an error)", err)
	}
	if len(res.TouchedNodes) != 0 {
		t.Errorf("TouchedNodes = %d; want 0 (no-CGO must skip .rs without ensuring nodes)", len(res.TouchedNodes))
	}
	if n := len(fw.nodes); n != 0 {
		t.Errorf("InsertNode call count = %d; want 0 (.rs must not produce any node under CGO=0); nodes=%+v",
			n, fw.nodes)
	}
	if n := len(fw.edges); n != 0 {
		t.Errorf("InsertEdge call count = %d; want 0 (.rs must not produce any edge under CGO=0); edges=%+v",
			n, fw.edges)
	}
}

// TestDispatcher_NoCGORustLanguageHintDoesNotRoute pins the
// matching defence on the LanguageHints path: even when an
// event explicitly carries `language_hints=["rust"]`, the
// no-CGO build has no parser whose Language()=="rust", so
// the hint resolution loop in `selectParser` must miss and
// fall through. Without this guard a future refactor could
// add a `rust` language alias to `normalizeHints` and
// silently start routing `.rs` files to a TypeScript or
// Python parser via the hint fallback.
func TestDispatcher_NoCGORustLanguageHintDoesNotRoute(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	ev := makeEvent("oddball.unknown", "pub struct Foo;")
	ev.LanguageHints = []string{"rust"}
	if _, err := d.EmitFile(context.Background(), ev); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	if n := len(fw.nodes); n != 0 {
		t.Errorf("InsertNode call count = %d; want 0 (rust hint must NOT route under CGO=0); nodes=%+v",
			n, fw.nodes)
	}
}
