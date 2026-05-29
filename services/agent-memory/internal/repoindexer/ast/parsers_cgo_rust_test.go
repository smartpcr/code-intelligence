//go:build cgo

package ast

import (
	"context"
	"testing"
)

// TestDefaultParsers_CGORegistersRust pins the CGO build's
// parser set contract: `defaultParsers()` must include a
// LanguageParser whose Language()=="rust" and whose
// Extensions() includes ".rs". This is the test the
// dispatcher relies on to route `.rs` files to the
// tree-sitter Rust parser at runtime
// (implementation-plan.md §5.2 "Stage 5.2 dispatcher
// routing" line 354).
func TestDefaultParsers_CGORegistersRust(t *testing.T) {
	parsers := defaultParsers()
	if len(parsers) == 0 {
		t.Fatalf("defaultParsers() returned empty under cgo build; expected at least Rust")
	}
	var rust LanguageParser
	for _, p := range parsers {
		if p.Language() == "rust" {
			rust = p
			break
		}
	}
	if rust == nil {
		got := make([]string, len(parsers))
		for i, p := range parsers {
			got[i] = p.Language()
		}
		t.Fatalf("defaultParsers() under cgo did not register Language()=\"rust\"; got %v", got)
	}
	hasRs := false
	for _, ext := range rust.Extensions() {
		if ext == ".rs" {
			hasRs = true
			break
		}
	}
	if !hasRs {
		t.Errorf("rust parser Extensions() = %v; expected to include \".rs\"", rust.Extensions())
	}
}

// TestDispatcher_RoutesRsToRustParser_CGO is the routing-
// integration counterpart to TestDefaultParsers_CGORegistersRust.
// It constructs a real `NewDispatcher(fw)` and calls `EmitFile`
// against a `.rs` file with a minimal real Rust source. The
// assertions verify the routing decision END-TO-END (not just
// the parser-set contract): the file MUST be parsed, MUST
// emit at least one class / method node, AND every emitted
// node MUST carry `attrs_json["language"]="rust"`.
//
// This addresses evaluator iter-3 finding #3: the previous
// CGO routing test only scanned `defaultParsers()` for an
// extension match; an `extMap`-building bug (e.g. lowercase
// inconsistency or a `nil` parser slipping through
// `WithParsers`) could pass the slice scan and still drop
// `.rs` files at the dispatcher's `selectParser` step. The
// EmitFile-driven check catches that regression.
func TestDispatcher_RoutesRsToRustParser_CGO(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	const src = `pub struct Greeter {
    prefix: String,
}

pub fn make_greeter() -> Greeter {
    Greeter { prefix: String::new() }
}
`
	if _, err := d.EmitFile(context.Background(), makeEvent("src/hello.rs", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	classes := fw.nodesOf("class")
	if len(classes) != 1 {
		t.Fatalf("class nodes for .rs = %d; want 1 (Greeter); the dispatcher did NOT route .rs to the Rust parser",
			len(classes))
	}
	if got := attrString(t, classes[0].AttrsJSON, "language"); got != "rust" {
		t.Errorf("class attrs.language = %q; want rust (the wrong parser was selected for .rs)", got)
	}
	methods := fw.nodesOf("method")
	if len(methods) < 1 {
		t.Fatalf("method nodes for .rs = %d; want at least 1 (make_greeter free fn)", len(methods))
	}
	for _, m := range methods {
		if got := attrString(t, m.AttrsJSON, "language"); got != "rust" {
			t.Errorf("method %s attrs.language = %q; want rust",
				m.CanonicalSignature, got)
		}
	}
}
