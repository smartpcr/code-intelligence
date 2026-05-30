// -----------------------------------------------------------------------
// <copyright file="loader.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package devpolicy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/microsoft/cleancode-service/internal/policy/steward"
)

// Loader is the dev-mode policy loader the CLI invokes once per
// `cleanc analyze` run to turn YAML rule pack files into the
// canonical steward row shapes the rule engine's InMemoryStore consumes.
type Loader interface {
	Load(ctx context.Context, src LoaderSource) (Bundle, error)
}

// LoaderSource selects the input source the Loader reads YAML
// rule pack files from.
type LoaderSource struct {
	// UseEmbedded selects the embedded rule pack embed.FS.
	UseEmbedded bool

	// DirPath is the directory the Loader walks for YAML files
	// when UseEmbedded is false.
	DirPath string
}

// Bundle is the in-memory output of Loader.Load.
type Bundle struct {
	PolicyVersion steward.PolicyVersion
	Rules         []steward.Rule
	RulePacks     []steward.RulePack
}

// ErrMissingPolicyDir is returned when DirPath is empty and UseEmbedded is false.
var ErrMissingPolicyDir = errors.New("devpolicy: LoaderSource.DirPath must be non-empty when UseEmbedded is false")

// ErrDevModeUnavailable is the sentinel returned by the prod-tagged Loader.
var ErrDevModeUnavailable = errors.New("dev-mode policy bypass not available in prod build")

// FS resolves the fs.FS the Loader walks for the active source.
func (s LoaderSource) FS() (fs.FS, error) {
	if s.UseEmbedded {
		if embeddedRulePacks == nil {
			return nil, errors.New("devpolicy: embedded rule packs not available in this build")
		}
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

// rulePackYAML is the on-disk shape of a rule pack YAML file.
type rulePackYAML struct {
	PackID  string     `yaml:"pack_id"`
	Version string     `yaml:"version"`
	Rules   []ruleYAML `yaml:"rules"`
}

type ruleYAML struct {
	RuleID            string `yaml:"rule_id"`
	MetricKind        string `yaml:"metric_kind"`
	ScopeKind         string `yaml:"scope_kind"`
	PredicateDSL      string `yaml:"predicate_dsl"`
	Description       string `yaml:"description"`
	SuggestedRefactor string `yaml:"suggested_refactor"`
}

// SynthesisePolicyVersion produces a deterministic PolicyVersion
// from the loaded rule set. The PolicyVersionID is a SHA-256 hex
// digest over the sorted rule IDs, guaranteeing byte-for-byte
// identical output for identical inputs.
func SynthesisePolicyVersion(rules []steward.Rule) steward.PolicyVersion {
	ids := make([]string, len(rules))
	for i, r := range rules {
		ids[i] = r.RuleID
	}
	sort.Strings(ids)
	h := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return steward.PolicyVersion{
		PolicyVersionID: fmt.Sprintf("%x", h),
	}
}