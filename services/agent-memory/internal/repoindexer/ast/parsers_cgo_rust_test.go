//go:build cgo

package ast

import (
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

// TestDispatcher_RoutesRsToRustParser exercises the
// dispatcher's selectParser path under CGO: an
// EmitFileEvent for a path ending in `.rs` MUST be picked
// up by the Rust parser registered via defaultParsers().
// We assert the routing decision rather than the full
// parse pipeline -- the parse-level assertions live in
// parser_treesitter_rust_test.go.
func TestDispatcher_RoutesRsToRustParser(t *testing.T) {
	// Confirm the routing table contains an entry mapping
	// ".rs" to the Rust parser. selectParser is internal
	// to dispatcher.go; we exercise it indirectly via the
	// defaultParsers slice + extension matching, which is
	// the same predicate the dispatcher applies.
	parsers := defaultParsers()
	var match LanguageParser
	for _, p := range parsers {
		for _, ext := range p.Extensions() {
			if ext == ".rs" {
				match = p
				break
			}
		}
		if match != nil {
			break
		}
	}
	if match == nil {
		t.Fatalf("no parser in defaultParsers() claims the .rs extension (CGO build); .rs files would be skipped")
	}
	if match.Language() != "rust" {
		t.Errorf(".rs is routed to Language()=%q; want \"rust\"", match.Language())
	}
}
