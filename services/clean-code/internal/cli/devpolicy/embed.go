// Package devpolicy is the CLI's dev-mode policy loader: it reads
// YAML rule packs (embedded by default, filesystem via
// `--policy <path>` when the dev build allows it) and synthesises
// the canonical `steward.PolicyVersion` / `steward.Rule` /
// `steward.RulePack` row shapes the rule engine consumes.
//
// # Two responsibilities
//
//  1. Source resolution: choose between the binary-baked embed.FS
//     (this file) and a filesystem directory the operator points
//     `--policy` at. Both must be addressable through the same
//     `io/fs.FS` interface so the loader's downstream YAML walker
//     never has to branch on source kind.
//  2. Policy synthesis: decode each `*.yaml` into a
//     `steward.RulePack` plus per-entry `steward.Rule` rows, then
//     bundle them into a `steward.PolicyVersion` whose
//     `Signature == nil` so the rule engine's `InMemoryStore`
//     accepts it without ever invoking
//     [steward.Steward.VerifyPolicyVersionSignature]. This is the
//     STRUCTURAL signature bypass pinned by architecture Sec 3.8.
//
// # Why this file (`embed.go`) carries no build tag
//
// The `embed.FS` alias is intentionally visible in EVERY
// compilation mode (`-tags prod` AND the default no-tag dev
// build). A follow-up workstream (implementation-plan
// `docs/stories/code-intelligence-REFACTOR-GUIDE/implementation-plan.md`
// Stage 1.4 lines 99-100) will add two build-tag-gated
// synthesiser files in this same `devpolicy` package:
//
//   - `unsigned_dev.go` (`//go:build !prod`) -- will READ from
//     `embeddedRulePacks` to produce an unsigned
//     `steward.PolicyVersion` (architecture Sec 3.8 structural
//     bypass).
//   - `unsigned_prod.go` (`//go:build prod`) -- will IGNORE
//     `embeddedRulePacks` and return an `ErrDevModeUnavailable`
//     sentinel (to be introduced by the same follow-up).
//
// Declaring `embeddedRulePacks` here (no build tag) lets both
// future synthesisers reference the same canonical name without
// re-importing the rulepacks package from inside their
// build-tag-gated files. The embed surface itself contains no
// bypass logic and is therefore safe to ship in every build.
package devpolicy

import (
	"io/fs"

	"github.com/smartpcr/code-intelligence/services/clean-code/policy/rulepacks"
)

// embeddedRulePacks is the package-internal alias for the canonical
// rule pack `embed.FS` declared by the `rulepacks` package
// ([rulepacks.EmbeddedFS]). It is typed as the broader
// [io/fs.FS] interface so the Loader's YAML walker can accept
// either this embedded source or an `os.DirFS(path)` override
// uniformly.
//
// The variable is unexported on purpose: callers route through
// the [Loader] interface defined in loader.go, never reaching
// into the embedded byte tree directly.
var embeddedRulePacks fs.FS = rulepacks.EmbeddedFS
