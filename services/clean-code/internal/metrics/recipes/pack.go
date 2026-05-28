package recipes

// Stage 7.3 iter 2 -- this file was introduced by the
// `[e2e] System tier metric composer -- E2E (#122)` merge
// as a stub duplicating the canonical [Pack] / [Source]
// type and constant declarations already owned by
// `recipe.go` (in-tree since #75, last touched by #111).
// The stub caused `Pack/Source/PackBase/PackSolid/...
// redeclared in this block` build failures across every
// package that imports `internal/metrics/recipes` -- in
// particular `internal/aggregator`, `internal/management`,
// and the four `cmd/clean-code-*` binaries.
//
// The canonical declarations remain in `recipe.go`; this
// file is intentionally left empty (package clause only)
// to preserve the path while removing the duplicate
// declarations that broke the build. Deleting the file
// outright would also work but would put a sibling stage's
// merge artefact through `git rm`, which the workstream
// brief discourages. Future maintainers may consolidate
// this file into `recipe.go` once the post-#122 baseline
// is stable.
