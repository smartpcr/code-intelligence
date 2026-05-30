// Package rulepacks owns the canonical, repo-rooted set of YAML
// rule pack files (`solid/*.yaml`, `decoupling/*.yaml`) and
// exposes them as an `embed.FS` baked into every binary.
//
// # Why a sibling package
//
// `//go:embed` patterns may not contain `..` and may not reference
// files outside the package directory tree (Go spec, see
// `embed/embed.go` in the standard library). Embedding the canonical
// solid + decoupling YAML set therefore requires a Go file that
// physically lives at the directory ROOT of those subtrees -- i.e.
// `services/clean-code/policy/rulepacks/`. The existing
// `services/clean-code/policy/rulepacks/solid` and
// `.../decoupling` Go packages each carry their own family-scoped
// `//go:embed *.yaml` directive (`solid.embeddedRulepacks` /
// `decoupling.embeddedRulepacks`) which they consume internally
// for the Steward bootstrap path; this package is the SECOND,
// independent embed surface that exposes BOTH families together
// in a single `embed.FS` so the CLI's `internal/cli/devpolicy`
// loader can treat them uniformly without depending on the
// Steward-bootstrap-shaped per-family loaders.
//
// # Why both surfaces coexist
//
// The per-family loaders in `solid/` and `decoupling/` parse YAML
// into [steward.PublishRulepackRequest] shapes for the production
// composition root's `policy.publish_rulepack` ingest path. The CLI
// has no Steward, so it cannot reuse those loaders' shape; it
// needs raw `fs.FS`-shaped access to decode YAML into in-memory
// `steward.RulePack` / `steward.Rule` rows and synthesise an
// unsigned `steward.PolicyVersion` (architecture Sec 3.8, 5.8).
// [EmbeddedFS] is exactly that raw surface.
//
// # Anchors
//
//   - architecture Sec 1.3 row `cli-policy-distribution`
//   - architecture Sec 3.8 ("L8 -- Dev-mode Policy Loader")
//   - tech-spec Sec 8.4 ("Rule-pack distribution")
package rulepacks

import "embed"

// EmbeddedFS is the read-only `embed.FS` view of every YAML rule
// pack the CLI ships with -- the union of the `solid/` and
// `decoupling/` families. The Go embed directive can only reach
// files under the directory of the declaring source file, which
// is why this file lives at the rulepacks root (NOT inside one
// of the family subdirectories) and uses the relative glob
// patterns `solid/*.yaml` / `decoupling/*.yaml`.
//
// # Determinism
//
// The set is fixed at compile time. Adding a new family means
// (a) creating `services/clean-code/policy/rulepacks/<family>/`
// with one or more `*.yaml` files and (b) extending the
// `//go:embed` directive below to include `<family>/*.yaml`.
// Iterating the FS at runtime (e.g. `fs.WalkDir`) is the only
// supported enumeration path; callers MUST NOT hard-code the
// filename list.
//
//go:embed solid/*.yaml decoupling/*.yaml
var EmbeddedFS embed.FS
