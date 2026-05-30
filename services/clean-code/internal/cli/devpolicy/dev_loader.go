// -----------------------------------------------------------------------
// <copyright file="dev_loader.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build !prod

package devpolicy

import (
	"context"
	"fmt"
	"io/fs"
	"strings"

	"github.com/microsoft/cleancode-service/internal/policy/steward"
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