// -----------------------------------------------------------------------
// <copyright file="unsigned_dev.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build !prod

// This file is the !prod (dev) build's [Loader] implementation.
// It is mutually exclusive with `unsigned_prod.go`; only one
// of the two is consumed per build. Anything BOTH files need
// to agree on lives in `bypass.go` (no build tag).
//
// # Scope
//
// This file ships the dev-build loader: a concrete
// implementation of the [Loader] interface whose `Load` method
// validates the [LoaderSource], walks the chosen YAML source
// (embedded `embed.FS` or `--policy <dir>` override), decodes
// each `*.yaml` file with `gopkg.in/yaml.v3` strict mode, and
// returns a fully-populated [Bundle] whose
// `PolicyVersion.PolicyVersionID` is a deterministic UUID-v5
// over the sorted rule-id set (tech-spec C11). Threshold rows
// are seeded from `decoupling.ListCanonicalThresholds()` when
// the decoupling pack is present.
//
// The [Loader] surface is identical across both build tags so
// the `cmd/cleanc` dispatcher can call `devpolicy.NewLoader()`
// without a per-build-tag branch. An operator who forgets
// `--policy <path>` still gets [ErrMissingPolicyDir] at THIS
// layer, not later inside an empty walk.
//
// # Why the package-doc note in `embed.go` still applies
//
// `embed.go` already documents that `unsigned_dev.go` READS
// from `embeddedRulePacks` to produce an unsigned
// `steward.PolicyVersion` (architecture Sec 3.8 STRUCTURAL
// bypass). This file honours that documentation by SHIPPING
// the real decoder under the `//go:build !prod` tag.

package devpolicy

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"gopkg.in/yaml.v3"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/policy/rulepacks/decoupling"
)

// devLoader is the !prod build's concrete [Loader]
// implementation. It carries no state: every Load call is
// self-contained (the [LoaderSource] supplies the input
// kind and any directory path).
//
// The type is unexported on purpose: callers route through
// the [Loader] interface that [NewLoader] returns, never
// reaching into the concrete value directly. This keeps the
// dev / prod swap a SHAPE swap rather than a TYPE swap.
type devLoader struct{}

// NewLoader returns the active build's [Loader].
//
// In the !prod (dev) build (this file), it returns a
// [devLoader] whose Load walks the resolved [LoaderSource]
// FS, decodes every `*.yaml` rule pack into the canonical
// [steward.RulePack] / [steward.Rule] row shapes, and
// synthesises an unsigned [steward.PolicyVersion] that
// composes the loaded rule + threshold catalogue.
//
// In the prod build (`unsigned_prod.go`), it returns a
// loader whose Load ALWAYS returns
// [ErrDevModeUnavailable] regardless of input.
//
// The function name and signature are IDENTICAL across
// both files so the `cmd/cleanc` dispatcher can call
// `devpolicy.NewLoader()` without a per-build-tag import.
func NewLoader() Loader { return devLoader{} }

// devPolicyNamespace is the UUID-v5 namespace seed used to
// derive deterministic ids for the synthesised
// [steward.PolicyVersion]. Pinning a CLI-specific namespace
// keeps the dev-bypass ids from ever colliding with a
// production policy id, even if the same rule_id set were
// republished through `policy.publish` (architecture
// Sec 3.8 STRUCTURAL bypass: dev-mode rows must be
// distinguishable from signed rows at the row level).
var devPolicyNamespace = uuid.NewV5(uuid.NamespaceURL, "cleanc.devpolicy/policy_version")

// devLoaderClock is the wall-clock the dev loader stamps
// onto the synthesised `CreatedAt` fields. Held as a
// package var so tests can pin it; the production code path
// uses [time.Now] directly via the default.
var devLoaderClock = func() time.Time { return time.Now().UTC() }

// loadedRulepack is the YAML decode target -- one document
// per `*.yaml` file. The shape mirrors the per-family
// `LoadedRulepack` structs in `policy/rulepacks/{solid,
// decoupling}/loader.go` so the dev loader stays in sync
// with the canonical Steward bootstrap path; an unknown
// field (typo, drift) surfaces as a strict-decode error.
type loadedRulepack struct {
	Filename      string           `yaml:"-"`
	PackID        string           `yaml:"pack_id"`
	Version       int              `yaml:"version"`
	DisplayName   string           `yaml:"display_name"`
	DescriptionMD string           `yaml:"description_md"`
	Rules         []loadedRuleSpec `yaml:"rules"`
}

type loadedRuleSpec struct {
	RuleID          string `yaml:"rule_id"`
	Version         int    `yaml:"version"`
	PredicateDSL    string `yaml:"predicate_dsl"`
	SeverityDefault string `yaml:"severity_default"`
	DescriptionMD   string `yaml:"description_md"`
}

// Load walks src.FS() for `*.yaml` files, strict-decodes
// each one into a [loadedRulepack], converts the result
// into canonical [steward.*] row shapes, and synthesises an
// unsigned [steward.PolicyVersion] that composes the loaded
// rule + threshold catalogue.
//
// Threshold rows: the decoupling family's predicates use
// `threshold('<uuid>')` atoms (see
// `policy/rulepacks/decoupling/coupling.yaml`). The dev
// loader seeds the canonical four-row threshold set from
// [decoupling.ListCanonicalThresholds] so the engine's
// predicate compiler can bind every reference without
// hitting `ErrPredicateCompile`. SOLID predicates use
// literal numeric cut-offs and need no threshold seeding.
//
// Determinism: the [steward.PolicyVersion.PolicyVersionID]
// is a UUID-v5 over the loaded rule ids; re-running the
// loader against the same FS yields byte-identical ids
// (tech-spec C11).
func (devLoader) Load(ctx context.Context, src LoaderSource) (Bundle, error) {
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	root, err := src.FS()
	if err != nil {
		return Bundle{}, err
	}

	packs, err := walkAndDecodeRulepacks(root)
	if err != nil {
		return Bundle{}, err
	}
	if len(packs) == 0 {
		return Bundle{}, fmt.Errorf("devpolicy: no *.yaml rule pack files found under loader source")
	}

	now := devLoaderClock()
	bundle := Bundle{}
	hasDecoupling := false
	for _, p := range packs {
		bundle.RulePacks = append(bundle.RulePacks, steward.RulePack{
			PackID:        p.PackID,
			Version:       p.Version,
			DisplayName:   p.DisplayName,
			DescriptionMD: p.DescriptionMD,
			CreatedAt:     now,
		})
		if strings.HasPrefix(p.PackID, "decoupling.") {
			hasDecoupling = true
		}
		for _, r := range p.Rules {
			sev := steward.Severity(r.SeverityDefault)
			if !sev.IsValid() {
				return Bundle{}, fmt.Errorf("devpolicy: %s rule_id=%s: severity_default=%q is not in {info, warn, block}",
					p.Filename, r.RuleID, r.SeverityDefault)
			}
			bundle.Rules = append(bundle.Rules, steward.Rule{
				RuleID:          r.RuleID,
				Version:         r.Version,
				PackID:          p.PackID,
				PredicateDSL:    r.PredicateDSL,
				SeverityDefault: sev,
				DescriptionMD:   r.DescriptionMD,
				CreatedAt:       now,
			})
		}
	}

	// Seed the canonical threshold catalogue ONLY when at
	// least one decoupling pack is loaded; SOLID-only sources
	// (e.g. an operator-supplied `--policy <dir>` that
	// excludes the decoupling family) skip the seeding
	// entirely so the synthesised policy version's
	// `ThresholdRefs` stays minimal.
	if hasDecoupling {
		bundle.Thresholds = decoupling.ListCanonicalThresholds()
	}

	// Synthesise the PolicyVersion. RuleRefs / ThresholdRefs
	// must cover every loaded row because the engine reads
	// `pv.RuleRefs` / `pv.ThresholdRefs` to enumerate which
	// rules to evaluate (engine.go:507, 533).
	ruleRefs := make([]steward.RuleRef, 0, len(bundle.Rules))
	ruleIDs := make([]string, 0, len(bundle.Rules))
	for _, r := range bundle.Rules {
		ruleRefs = append(ruleRefs, steward.RuleRef{RuleID: r.RuleID, Version: r.Version})
		ruleIDs = append(ruleIDs, fmt.Sprintf("%s@%d", r.RuleID, r.Version))
	}
	thresholdRefs := make([]steward.ThresholdRef, 0, len(bundle.Thresholds))
	for _, t := range bundle.Thresholds {
		thresholdRefs = append(thresholdRefs, steward.ThresholdRef{ThresholdID: t.ThresholdID})
	}

	// Deterministic policy_version_id: UUID-v5 over the sorted
	// rule-id set so two runs over the same packs yield byte-
	// identical ids (tech-spec C11).
	sort.Strings(ruleIDs)
	pvID := uuid.NewV5(devPolicyNamespace, strings.Join(ruleIDs, ","))

	bundle.PolicyVersion = steward.PolicyVersion{
		PolicyVersionID: pvID,
		Name:            "cleanc.devpolicy",
		RuleRefs:        ruleRefs,
		ThresholdRefs:   thresholdRefs,
		RefactorWeights: defaultRefactorWeights(),
		Signature:       nil, // architecture Sec 3.8 STRUCTURAL bypass: unsigned in dev
		CreatedAt:       now,
	}

	return bundle, nil
}

// walkAndDecodeRulepacks reads every `*.yaml` file under
// root (recursively), strict-decodes each into a
// [loadedRulepack], and returns the results in
// deterministic filename order. The traversal honours
// arbitrary nesting so both the embedded
// `solid/*.yaml + decoupling/*.yaml` shape and an operator's
// flat `--policy <dir>` shape work uniformly.
func walkAndDecodeRulepacks(root fs.FS) ([]loadedRulepack, error) {
	type entry struct {
		path string
	}
	var found []entry
	err := fs.WalkDir(root, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".yaml") {
			return nil
		}
		found = append(found, entry{path: path})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("devpolicy: walk rule pack FS: %w", err)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].path < found[j].path })

	out := make([]loadedRulepack, 0, len(found))
	seenPack := make(map[string]struct{}, len(found))
	seenRule := make(map[string]struct{})
	for _, e := range found {
		raw, err := fs.ReadFile(root, e.path)
		if err != nil {
			return nil, fmt.Errorf("devpolicy: read %s: %w", e.path, err)
		}
		var pack loadedRulepack
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&pack); err != nil {
			return nil, fmt.Errorf("devpolicy: decode %s: %w", e.path, err)
		}
		pack.Filename = e.path
		if pack.PackID == "" || pack.Version == 0 {
			return nil, fmt.Errorf("devpolicy: %s: pack_id and version are required (got %q, %d)",
				e.path, pack.PackID, pack.Version)
		}
		key := fmt.Sprintf("%s@%d", pack.PackID, pack.Version)
		if _, dup := seenPack[key]; dup {
			return nil, fmt.Errorf("devpolicy: duplicate rule pack (pack_id, version) = (%s, %d) in %s",
				pack.PackID, pack.Version, e.path)
		}
		seenPack[key] = struct{}{}
		for _, r := range pack.Rules {
			rkey := fmt.Sprintf("%s@%d", r.RuleID, r.Version)
			if _, dup := seenRule[rkey]; dup {
				return nil, fmt.Errorf("devpolicy: duplicate rule (rule_id, version) = (%s, %d) in %s",
					r.RuleID, r.Version, e.path)
			}
			seenRule[rkey] = struct{}{}
		}
		out = append(out, pack)
	}
	return out, nil
}

// defaultRefactorWeights returns the v1 dev-mode default
// composite-score weights the synthesised
// [steward.PolicyVersion] carries. The values mirror the
// architecture Sec 3.9 defaults the production loader uses
// when a rule pack omits the `refactor_weights` block.
func defaultRefactorWeights() steward.RefactorWeights {
	return steward.RefactorWeights{
		Alpha:              1.0,
		Beta:               1.0,
		Gamma:              1.0,
		Delta:              1.0,
		EffortModelVersion: "dev",
		WindowDays:         90,
		TopN:               20,
	}
}
