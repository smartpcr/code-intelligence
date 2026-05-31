package devpolicy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// Loader is the dev-mode policy loader the CLI invokes once per
// `cleanc analyze` run to turn YAML rule pack files into the
// canonical `steward.*` row shapes the rule engine's
// `InMemoryStore` consumes (architecture Sec 3.8, Sec 5.8).
//
// This file declares the contract ONLY; no concrete
// implementation ships in this iteration. A follow-up
// workstream (implementation-plan Stage 1.4 items 97-102) will
// add two build-tag-gated synthesiser files in this same
// `devpolicy` package:
//
//   - `unsigned_dev.go` (`//go:build !prod`) -- will construct
//     an unsigned `steward.PolicyVersion` per architecture
//     Sec 3.8's "STRUCTURAL signature bypass".
//   - `unsigned_prod.go` (`//go:build prod`) -- will return an
//     `ErrDevModeUnavailable` sentinel (introduced by the same
//     follow-up) without producing any `PolicyVersion`, so the
//     bypass cannot be smuggled into a production binary at
//     compile time.
//
// Declaring the interface up front lets the future CLI
// orchestrator and its tests program against the canonical
// shape regardless of which file is in the active build.
type Loader interface {
	// Load reads YAML rule pack files from the source described
	// by src and returns a fully-populated [Bundle]. The
	// concrete implementation is added by a follow-up workstream
	// (implementation-plan Stage 1.4 items 97-102); the
	// contract declared here will be honoured as follows:
	//
	//   - YAML decode / validation errors will surface unchanged
	//     from the underlying decoder so the operator sees the
	//     offending filename and line.
	//   - When the active build does not permit the unsigned
	//     bypass (a `-tags prod` build), Load will return an
	//     `ErrDevModeUnavailable` sentinel (to be introduced by
	//     the same follow-up) without reading any bytes.
	//
	// The returned `Bundle.PolicyVersion.Signature` will always
	// be nil in the dev build (architecture Sec 3.8 / tech-spec
	// C6); the rule engine's InMemoryStore accepts the unsigned
	// row without invoking the Steward verifier.
	Load(ctx context.Context, src LoaderSource) (Bundle, error)
}

// LoaderSource selects the input source the Loader reads YAML
// rule pack files from.
//
// When UseEmbedded is true, the Loader walks the canonical
// `embed.FS` baked into the binary at compile time via
// `services/clean-code/policy/rulepacks/embedded_fs.go`. When
// UseEmbedded is false, the Loader walks `os.DirFS(DirPath)` --
// this is the `--policy <path>` override path; it is permitted
// in the no-tag dev build and will be FORBIDDEN in a `-tags
// prod` build (the prod synthesiser added by the Stage 1.4
// follow-up will return its `ErrDevModeUnavailable` sentinel
// before any filesystem access). See architecture Sec 7.2 and
// tech-spec Sec 8.4 for the override-vs-embed precedence rules.
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

	// Thresholds is the set of [steward.Threshold] rows the
	// dev-mode loader seeds so any rule whose `predicate_dsl`
	// uses the `threshold('<uuid>')` atom can compile cleanly
	// at engine `RunBatch` time. Without this, the dev loader
	// would silently break the `decoupling.*` rule pack family
	// (every decoupling predicate references a threshold UUID
	// per `policy/rulepacks/decoupling/coupling.yaml`); the
	// engine's predicate compiler rejects unknown threshold
	// ids with `ErrPredicateCompile` (engine.go:407-410).
	//
	// The slice is the canonical
	// [decoupling.ListCanonicalThresholds] set in dev builds;
	// the SOLID family ships zero threshold dependencies (its
	// predicates use literal numeric cut-offs) so this slice
	// is only populated when the decoupling family is loaded.
	Thresholds []steward.Threshold
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
// filesystem sources, so the future build-tag-gated synthesisers
// (`unsigned_dev.go` / `unsigned_prod.go`, to be added by the
// Stage 1.4 follow-up workstream) will stay decoupled from the
// source-kind switch: they will call `src.FS()` and walk the
// returned FS uniformly.
//
// For the filesystem source, FS eagerly stats s.DirPath and
// verifies it is a directory before returning the os.DirFS
// handle. This surfaces an operator typo in `--policy <path>`
// here -- as a clear "policy dir: ... no such file or
// directory" / "is not a directory" diagnostic -- rather than
// as an opaque walk/read error later inside the loader.
func (s LoaderSource) FS() (fs.FS, error) {
	if s.UseEmbedded {
		return embeddedRulePacks, nil
	}
	if s.DirPath == "" {
		return nil, ErrMissingPolicyDir
	}
	info, err := os.Stat(s.DirPath)
	if err != nil {
		return nil, fmt.Errorf("devpolicy: policy dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("devpolicy: policy dir: %s is not a directory", s.DirPath)
	}
	return os.DirFS(s.DirPath), nil
}
