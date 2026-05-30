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
//     flips to false, and the prod policy loader sentinel in
//     `internal/cli/devpolicy/unsigned_prod.go`
//     (`prodSentinelLoader.Load`) returns
//     `devpolicy.ErrDevModeUnavailable` UNCONDITIONALLY. Passing
//     `--dev-mode` on the command line CANNOT enable an unsigned
//     rule-pack bypass in a prod build -- the refusal is
//     compile-time and absolute (architecture.md Sec 1.6,
//     "policy-signing-required"; Sec 7.2, prod posture). The
//     `--dev-mode` flag remains accepted by the parser so that
//     dev/prod operators can share scripts, but the prod loader
//     ignores it.
//   - The constant lives in the `flags` package -- this file no
//     longer owns a sibling `defaultDevMode` (resolves iter-4
//     evaluator item 6).

const (
	buildTag = "prod"
)
