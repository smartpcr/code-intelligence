package ast

// signature_helpers_test.go exposes the canonical-signature
// formulae used by dispatcher.go as cheap helpers the
// per-language dispatcher tests can call to mint expected node
// ids. Without these helpers each test would inline the
// `fmt.Sprintf("%s::class::%s#%s", ...)` pattern, which means
// a single formula change has to be chased across every
// dispatcher test file and silently mismatches when one is
// missed. The helpers MUST match dispatcher.go byte-for-byte:
//
//   - class signature: `<repoURL>::class::<relPath>#<qualName>`
//     (see dispatcher.go ~line 440).
//   - method signature: `<repoURL>::method::<relPath>#<qualName>(<params>)`
//     (see dispatcher.go ~line 499); the `methodSignature`
//     helper lives in whitespace_test.go to keep the
//     whitespace-stability tests local.
//   - external package signature:
//     `<repoURL>::package::<module>` (see dispatcher.go ~line
//     403); no `ext::` infix, no NormalizeSignature wrapping
//     because the dispatcher does not normalise the inputs
//     either.
//
// This file is build-tag agnostic so dispatcher tests gated on
// `//go:build cgo` (parser_treesitter_c_dispatcher_test.go,
// parser_treesitter_rust_dispatcher_test.go) and any future
// non-cgo dispatcher test share the same helpers.

import "fmt"

func classSignature(repoURL, relPath, qualifiedName string) string {
	return fmt.Sprintf("%s::class::%s#%s", repoURL, relPath, qualifiedName)
}

func externalPackageSignature(repoURL, module string) string {
	return fmt.Sprintf("%s::package::%s", repoURL, module)
}
