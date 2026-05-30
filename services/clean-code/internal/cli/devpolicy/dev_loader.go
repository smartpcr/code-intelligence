// -----------------------------------------------------------------------
// <copyright file="dev_loader.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// This file is a STALE COPY of the dev-build [Loader]
// implementation. The canonical `//go:build !prod` loader is in
// `unsigned_dev.go`, added by the CLI Binary Skeleton workstream
// (commit 94e3f11) as the Stage 1.1 shell. This file's
// pre-skeleton implementation:
//
//   - Redeclares `devLoader`, `NewLoader`, and `Load` (all
//     declared in `unsigned_dev.go` under the same `!prod` tag).
//   - References an undefined `rulePackYAML` type and an
//     undefined `SynthesisePolicyVersion` helper.
//   - References `steward.Rule.MetricKind` /
//     `steward.Rule.ScopeKind` / `steward.Rule.Description` /
//     `steward.Rule.SuggestedRefactor` fields that do not exist
//     on the current `steward.Rule` struct (see
//     `internal/policy/steward/types.go:151-156`).
//
// The Stage 1.4 implementation-plan follow-up workstream is
// expected to replace `unsigned_dev.go`'s stub body with a
// real YAML decoder, at which point this file should be
// deleted outright. Until then, gating with the
// `legacy_superseded` build tag (NEVER set) keeps the file
// on disk while excluding it from every compilation, so the
// duplicates + undefined symbols do not break the default
// `go build ./...` invocation.
//
// See `banner.go` for the full rationale behind the
// `legacy_superseded` build tag convention.

//go:build legacy_superseded

package devpolicy

import (
	"context"
	"fmt"
	"io/fs"
	"strings"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"gopkg.in/yaml.v3"
)

type devLoader struct{}

// NewLoader returns the dev-mode Loader that reads and parses
// YAML rule packs into an unsigned in-memory PolicyVersion.
func NewLoader() Loader {
	return &devLoader{}
}

func (l *devLoader) Load(_ context.Context, src LoaderSource) (Bundle, error) {
	fsys, err := src.FS()
	if err != nil {
		return Bundle{}, err
	}

	var bundle Bundle

	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			return nil
		}

		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", path, readErr)
		}

		var pack rulePackYAML
		if yamlErr := yaml.Unmarshal(data, &pack); yamlErr != nil {
			return fmt.Errorf("parsing %s: %w", path, yamlErr)
		}

		bundle.RulePacks = append(bundle.RulePacks, steward.RulePack{
			PackID:  pack.PackID,
			Version: pack.Version,
		})

		for _, r := range pack.Rules {
			bundle.Rules = append(bundle.Rules, steward.Rule{
				RuleID:            r.RuleID,
				PackID:            pack.PackID,
				MetricKind:        r.MetricKind,
				ScopeKind:         r.ScopeKind,
				PredicateDSL:      r.PredicateDSL,
				Description:       r.Description,
				SuggestedRefactor: r.SuggestedRefactor,
			})
		}

		return nil
	})
	if err != nil {
		return Bundle{}, err
	}

	bundle.PolicyVersion = SynthesisePolicyVersion(bundle.Rules)

	return bundle, nil
}