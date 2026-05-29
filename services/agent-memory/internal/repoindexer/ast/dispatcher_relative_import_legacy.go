//go:build !canonical_dispatcher

package ast

// isRelativeImport is the dispatcher-level filter that
// decides whether an `Import` whose `Module` starts with
// `./`, `../`, `/`, or is empty should be SUPPRESSED from
// the imports-edge emission (relative / in-repo file
// references do not mint external `package` nodes -- see
// architecture Section 6.1 dot-source / local-include row).
//
// The CANONICAL implementation lives in a
// `//go:build canonical_dispatcher`-gated dispatcher source
// file that ships with the full dispatcher landing (commit
// `876a128` on the `phase-c-and-cpp-parsers-stage-cpp-fixture
// -test` worktree; not yet merged into `feature/memory`).
// This file's `!canonical_dispatcher` build tag deliberately
// disappears the moment the canonical file lands, so the
// canonical-build path has exactly one definition.
//
// Until then the test surface depends on this helper being
// package-accessible: `parser_powershell_test.go` (no build
// tag) uses it to assert `dot.Module` ("./helpers.ps1") is
// detected as relative so the dispatcher will drop the
// import edge. The implementation MUST match the canonical
// version byte-for-byte to keep behaviour stable when the
// tag switches over.
//
// This file lands as part of the PowerShell register-cgo
// stage (story code-intelligence:AST-PARSER-FOR-ADDIT,
// phase powershell-parser) because PR #173's
// "additive surfaces" cycle that introduced the PowerShell
// test that calls `isRelativeImport` did not also publish
// the helper into a non-canonical file, so the package did
// not compile under default (CGO=0) builds.
func isRelativeImport(module string) bool {
	if module == "" {
		return true
	}
	if module[0] == '.' || module[0] == '/' {
		return true
	}
	return false
}
