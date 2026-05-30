//go:build !prod

package main

// This file ships in dev / no-tag builds.
//
// Pairs with `buildtag_prod.go` (which carries `//go:build prod`).
// The Go toolchain picks exactly one of the two files when
// compiling the cleanc binary, so the constants below are
// always defined exactly once.
//
// Contract (tech-spec.md Sec 8.9, "Build-tag matrix"):
//   - dev / no-tag build  -> `buildTag = ""`,    `defaultDevMode = true`
//   - `-tags prod` build  -> `buildTag = "prod"`, `defaultDevMode = false`
//
// The dev build defaults `--dev-mode` to true so developers
// can run `cleanc analyze .` against a local checkout without
// having to bring up the full signing infrastructure
// (architecture.md Sec 1.6, "policy-signing-required").

const (
	// buildTag is the empty string in dev / no-tag builds.
	// The version header renders this verbatim as
	// `build-tag=` (note the empty value after the equals
	// sign), which the e2e regex
	// `^cleanc \d+\.\d+\.\d+ \(build-tag=(|prod)\) \(parsers=[^)]+\) \(rule-packs=[^)]+\)$`
	// explicitly accepts via the alternation `(|prod)`.
	buildTag = ""
	// defaultDevMode pins the `--dev-mode` flag default to
	// true in dev / no-tag builds.
	defaultDevMode = true
)
