package devpolicy

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// Loader is the dev-mode policy loader the CLI invokes once per
// `cleanc analyze` run to turn YAML rule pack files into the
// canonical `steward.*` row shapes the rule engine's
// `InMemoryStore` consumes (architecture Sec 3.8, Sec 5.8).
//
// Implementations are build-tag-gated: the no-tag dev build
// constructs an unsigned `steward.PolicyVersion` (architecture
// Sec 3.8 "STRUCTURAL signature bypass"); a `-tags prod` build
// returns [ErrDevModeUnavailable] without producing any
// `PolicyVersion` so the bypass cannot be smuggled into a
// production binary at compile time. Both implementations live in
// sibling `unsigned_dev.go` / `unsigned_prod.go` files (Stage 1.4
// follow-up); the interface is declared here so callers (the CLI
// orchestrator and its tests) can program against the canonical
// shape regardless of which file is in the active build.
type Loader interface {
	// Load reads YAML rule pack files from the source described
	// by src and returns a fully-populated [Bundle]. Errors:
	//
	//   - YAML decode / validation errors surface unchanged from
	//     the underlying decoder so the operator sees the
	//     offending filename and line.
	//   - When the active build does not permit the unsigned
	//     bypass (a `-tags prod` build), the prod
	//     implementation returns [ErrDevModeUnavailable] without
	//     reading any bytes.
	//
	// The returned [Bundle.PolicyVersion.Signature] is always
	// nil in the dev build (architecture Sec 3.8 / tech-spec
	// C6); the rule engine's InMemoryStore accepts the
	// unsigned row without invoking the Steward verifier.
	Load(ctx context.Context, src LoaderSource) (Bundle, error)
}

// LoaderSource selects the input source the Loader reads YAML
// rule pack files from.
//
// When UseEmbedded is true, the Loader walks the canonical
// `embed.FS` baked into the binary at compile time via
// `services/clean-code/policy/rulepacks/embedded_fs.go`. When
// UseEmbedded is false, the Loader walks `os.DirFS(DirPath)` --
// this is the `--policy <path>` override path; it is permitted in
// the no-tag dev build and FORBIDDEN in a `-tags prod` build (the
// prod implementation returns [ErrDevModeUnavailable] before any
// filesystem access). See architecture Sec 7.2 and tech-spec
// Sec 8.4 for the override-vs-embed precedence rules.
type LoaderSource struct {
	// UseEmbedded selects the embedded rule pack `embed.FS`.
	// When true, [LoaderSource.DirPath] is ignored.
	UseEmbedded bool

	// DirPath is the absolute or repo-relative directory the
	// Loader walks for YAML files when UseEmbedded is false. It
	// is required (non-empty) in that mode -- the Loader
	// returns [ErrMissingPolicyDir] otherwise. The directory
	// must contain one or more `.yaml` files in the canonical
	// rule pack shape (matching the embedded `solid/*.yaml` /
	// `decoupling/*.yaml` set).
	DirPath string
}

// Bundle is the in-memory output of [Loader.Load]: the canonical
// `steward.*` row shapes the rule engine consumes (architecture
// Sec 5.8 `DevPolicy -> Steward shapes`).
//
// Every field is the unmodified `steward.*` type so the CLI
// orchestrator can hand Bundle.Rules and Bundle.RulePacks
// directly to `rule_engine.InMemoryStore.InsertRule` /
// `InsertRulePack`, and Bundle.PolicyVersion directly to
// `InsertPolicyVersion`, without a per-field translation step.
type Bundle struct {
	// PolicyVersion is the synthesised [steward.PolicyVersion].
	// Its Signature is nil in the dev build (the engine's
	// InMemoryStore does not enforce signatures); its
	// PolicyVersionID is a stable UUID-v5 over the loaded rule
	// id set so two runs over the same packs yield byte-for-byte
	// identical ids (impl-plan Stage 1.4 / tech-spec C11).
	PolicyVersion steward.PolicyVersion

	// Rules is one [steward.Rule] per entry in any loaded
	// YAML file's `rules:` array. PackID on each rule resolves
	// to the parent [Bundle.RulePacks] entry.
	Rules []steward.Rule

	// RulePacks is one [steward.RulePack] per loaded YAML
	// file. The composite primary key `(PackID, Version)` is
	// enforced cross-file by the loader before this slice is
	// returned (mirrors the per-family loaders in
	// `services/clean-code/policy/rulepacks/{solid,decoupling}`).
	RulePacks []steward.RulePack
}

// ErrMissingPolicyDir is returned by [LoaderSource.FS] when the
// caller asked for a filesystem source (UseEmbedded == false) but
// did not provide a [LoaderSource.DirPath]. The CLI surfaces this
// as an "operator forgot `--policy <path>`" diagnostic, not a
// bug, so the error string is operator-facing.
var ErrMissingPolicyDir = errors.New("devpolicy: LoaderSource.DirPath must be non-empty when UseEmbedded is false")

// FS resolves the [io/fs.FS] the Loader walks for the active
// source. When UseEmbedded is true, the returned FS is the
// canonical binary-baked rule pack `embed.FS`
// (declared in `embed.go`); when false, it is
// `os.DirFS(s.DirPath)` -- the `--policy <path>` override.
//
// This is the SINGLE choice point between the embedded and
// filesystem sources, so the build-tag-gated synthesisers
// (`unsigned_dev.go` / `unsigned_prod.go`, Stage 1.4 follow-up)
// stay decoupled from the source-kind switch; they call
// `src.FS()` and walk the returned FS uniformly.
func (s LoaderSource) FS() (fs.FS, error) {
	if s.UseEmbedded {
		return embeddedRulePacks, nil
	}
	if s.DirPath == "" {
		return nil, ErrMissingPolicyDir
	}
	return os.DirFS(s.DirPath), nil
}
