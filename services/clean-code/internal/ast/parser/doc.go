// Package parser is the canonical AST adapter front end for the
// clean-code service (architecture Sec 4.5, tech-spec Sec 4.5 /
// Sec 8.6). Recipes consume `*astv1.AstFile`; they never reach
// for the original source bytes. Per-language parsers implement
// the `Parser` interface and are registered with the
// `Registry`; the registry refuses to register a language
// outside the v1 pin (Go + Python + TypeScript + Java -- the
// org's top-4 by repo count per tech-spec Sec 8.6 lines
// 1005-1016). Post-v1 languages are recipe-pack additions per
// Sec 8.6 and must publish via a future schema-version bump,
// not by relaxing the registry guard here.
//
// # Parser backends
//
// Two build-tagged code paths satisfy the `Parser` interface.
// Exactly one is active per build:
//
//   - `//go:build cgo` (the DEFAULT in CI and the v1 supported
//     path). Go / Python / TypeScript / Java are ALL powered by
//     `github.com/smacker/go-tree-sitter` wrapping the upstream
//     `tree-sitter-go`, `tree-sitter-python`,
//     `tree-sitter-typescript`, and `tree-sitter-java`
//     grammars. These produce a full concrete syntax tree; the
//     walker in `tree_sitter_<lang>.go` emits the canonical
//     scope / symbol / edge shape and stamps
//     `AstFile.degraded_reason` if `root.HasError()` reports
//     parse-error nodes.
//   - `//go:build !cgo` (the lexer fallback). Python /
//     TypeScript / Java are powered by handwritten line-based
//     parsers in `python.go`, `typescript.go`, `java.go`. Go
//     keeps the standard-library `go/parser` adapter (also in
//     `go.go`) because it is full-fidelity and CGO-free. The
//     fallback exists for cross-compile sandboxes and CI hosts
//     without a C compiler; `make test` exercises both paths
//     (see the Makefile `test-cgo` / `test-nocgo` targets).
//
// Tests under both build tags assert the same canonical
// invariants (scope kinds, parent relationships, imports +
// extends + implements edges) so swapping CGO on/off does
// not change the set of recipes that pass on a given fixture.
// See `docs/stories/code-intelligence-CLEAN-CODE/tech-spec.md`
// Sec 9.14 for the grammar-version-drift posture this design
// supports.
package parser
