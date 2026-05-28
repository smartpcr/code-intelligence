//go:build !cgo

package ast

import (
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
