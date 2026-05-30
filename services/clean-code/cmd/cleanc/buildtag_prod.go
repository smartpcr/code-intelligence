//go:build prod

package main

// This file ships only in `-tags prod` builds.
//
// Pairs with `buildtag_default.go` (which carries
// `//go:build !prod`). See that file's doc comment for the
// full build-tag matrix.
//
// In a prod build:
//   - `buildTag` is set to `"prod"` so the version line carries
//     the literal `build-tag=prod` substring (e2e regex
//     `^cleanc \d+\.\d+\.\d+ \(build-tag=(|prod)\) ...`).
//   - `flags.DefaultDevMode` (in `internal/cli/flags/devmode_prod.go`)
//     flips to false so `cleanc analyze .` refuses to run against
//     unsigned rule packs unless the operator explicitly opts in
//     with `--dev-mode` (architecture.md Sec 1.6,
//     "policy-signing-required"). The constant lives in the
//     `flags` package -- this file no longer owns a sibling
//     `defaultDevMode` (resolves iter-4 evaluator item 6).

const (
	buildTag = "prod"
)
