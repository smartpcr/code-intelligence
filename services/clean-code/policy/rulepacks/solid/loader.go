// Package solid owns the canonical YAML payloads for the
// Stage 5.5 SOLID family of rule_packs (`solid.srp`,
// `solid.ocp`, `solid.lsp`, `solid.isp`, `solid.dip`) AND the
// loader+bootstrap functions the composition root
// (`cmd/clean-coded/main.go::run()`, gated on `signer != nil`)
// invokes at startup to ingest them via `policy.publish_rulepack`.
//
// # Why a loader lives here
//
// The implementation-plan Stage 5.5 brief (lines 511-518) lists
// the item "Each rulepack is signed and ingested via
// `policy.publish_rulepack` at startup if absent". The IRC
// verb (`policy.publish_rulepack`) lives in the Stage 5.2
// steward; this package supplies the YAML payloads PARSED
// into the steward's request shape so the bootstrap call site
// is one line in `cmd/clean-coded/main.go`:
//
//	_, err := solid.Bootstrap(ctx, stew)
//
// (see [Bootstrap] -- it loads each YAML via [LoadAll] and
// invokes `steward.PublishRulepack` for each pack. Unlike the
// Stage 5.6 decoupling family, the SOLID family does NOT
// reference any `clean_code.threshold` rows -- every numeric
// cut-off is a literal in the predicate text -- so this
// package ships no Threshold seeding step.)
//
// We embed the five `*.yaml` files via `go:embed` so the
// binary carries them in any deployment (including the
// scaffold-mode container that has no host-filesystem access
// to `policy/rulepacks/solid/`).
//
// # Why a local YAML-tagged type
//
// `gopkg.in/yaml.v3` lowercases struct field names by default
// (`PackID` -> `packid`), and [steward.PublishRulepackRequest]
// carries only `json:"..."` tags. Rather than mutating the
// steward package to gain YAML tags it does not otherwise need
// (it is a Go-API shape, not a YAML shape), we declare
// [LoadedRulepack] / [LoadedRuleSpec] here with explicit yaml
// tags and supply a [LoadedRulepack.ToPublishRulepackRequest]
// converter that the bootstrap calls before invoking
// `steward.PublishRulepack`. (The same pattern is used by the
// Stage 5.6 decoupling rulepack package; we intentionally
// duplicate the shape here rather than couple the two packages
// through a shared helper, since the YAML schemas are
// independent and a future drift in one family must not
// silently affect the other.)
//
// # pack_id naming
//
// Each YAML file is its own `rule_pack` row. The pack_id
// follows the canonical `<family>.<subname>` convention pinned
// by the `clean_code.rule_pack.pack_id` column comment in
// migration `0003_policy_audit_refactor.up.sql` line 200:
// "(e.g. `solid.srp`, `solid.dip`, `decoupling.cycles`,
// `base.complexity`)". The `solid` family groups the five
// SOLID sub-packs WITHOUT colliding on the
// `(pack_id, version)` primary key.
package solid

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

//go:embed *.yaml
var embeddedRulepacks embed.FS

// FamilyPrefix is the literal pack_id prefix every file in
// this package MUST use. Pinned here so callers (and the
// Stage 5.5 conformance tests) can grep for the single
// source of truth.
const FamilyPrefix = "solid."

// LoadedRuleSpec mirrors [steward.RuleSpec] with explicit
// yaml tags. The fields are identical -- we keep them in
// sync via the [LoadedRulepack.ToPublishRulepackRequest]
// converter.
//
// `severity_default` is decoded as a plain string and
// validated at conversion time so a typo in the YAML
// (e.g. `severityDefault`) surfaces as a load-time error
// rather than a silent zero value.
type LoadedRuleSpec struct {
	RuleID          string `yaml:"rule_id"`
	Version         int    `yaml:"version"`
	PredicateDSL    string `yaml:"predicate_dsl"`
	SeverityDefault string `yaml:"severity_default"`
	DescriptionMD   string `yaml:"description_md"`
}

// LoadedRulepack mirrors [steward.PublishRulepackRequest]
// with explicit yaml tags. One YAML file decodes to exactly
// one LoadedRulepack. The [LoadedRulepack.Filename] field is
// populated by [LoadAll] so downstream error reporting can
// name the offending file.
type LoadedRulepack struct {
	// Filename is the base name of the YAML file inside the
	// embed.FS (e.g. `srp.yaml`). NOT serialised in the
	// yaml document; set by [LoadAll] at decode time.
	Filename string `yaml:"-"`

	PackID        string           `yaml:"pack_id"`
	Version       int              `yaml:"version"`
	DisplayName   string           `yaml:"display_name"`
	DescriptionMD string           `yaml:"description_md"`
	Rules         []LoadedRuleSpec `yaml:"rules"`
}

// ToPublishRulepackRequest converts a LoadedRulepack into
// the canonical steward request shape used by
// `policy.publish_rulepack`. The conversion also validates
// each rule's `severity_default` against the
// [steward.Severity] closed set so an invalid severity
// surfaces at bootstrap time (a Steward.PublishRulepack call
// would otherwise fail later with a less specific error).
func (l LoadedRulepack) ToPublishRulepackRequest() (steward.PublishRulepackRequest, error) {
	rules := make([]steward.RuleSpec, 0, len(l.Rules))
	for i, r := range l.Rules {
		sev := steward.Severity(r.SeverityDefault)
		if !sev.IsValid() {
			return steward.PublishRulepackRequest{}, fmt.Errorf(
				"solid: %s rules[%d] rule_id=%s: severity_default=%q is not in {info, warn, block}",
				l.Filename, i, r.RuleID, r.SeverityDefault)
		}
		rules = append(rules, steward.RuleSpec{
			RuleID:          r.RuleID,
			Version:         r.Version,
			PredicateDSL:    r.PredicateDSL,
			SeverityDefault: sev,
			DescriptionMD:   r.DescriptionMD,
		})
	}
	return steward.PublishRulepackRequest{
		PackID:        l.PackID,
		Version:       l.Version,
		DisplayName:   l.DisplayName,
		DescriptionMD: l.DescriptionMD,
		Rules:         rules,
	}, nil
}

// LoadAll reads every `*.yaml` file embedded under this
// package, decodes it into a [LoadedRulepack], and returns
// the list in deterministic filename order.
//
// The decode is strict (`KnownFields(true)`): an unknown
// top-level key like `severityDefault` (typo for
// `severity_default`) returns an error rather than
// silently zero-filling the field. Same for unknown rule
// fields.
//
// Cross-file invariants enforced here:
//   - every pack_id starts with [FamilyPrefix] (so the
//     bootstrap knows it is allowed to ingest THIS file from
//     THIS package -- a future drift dropping a decoupling.*
//     pack into this directory is caught at load time);
//   - no two files share the same `(pack_id, version)` pair
//     (PK violation guard);
//   - no two rules across all files share the same
//     `(rule_id, version)` pair (Rule.PK guard).
//
// The bootstrap caller passes each entry to
// `steward.PublishRulepack`, swallowing
// [steward.ErrDuplicateRulePack] as the "already
// bootstrapped" idempotent outcome.
func LoadAll() ([]LoadedRulepack, error) {
	entries, err := fs.ReadDir(embeddedRulepacks, ".")
	if err != nil {
		return nil, fmt.Errorf("solid: read embed.FS: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".yaml") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]LoadedRulepack, 0, len(names))
	for _, name := range names {
		pack, err := loadOne(name)
		if err != nil {
			return nil, err
		}
		out = append(out, pack)
	}
	if err := validateCrossFileInvariants(out); err != nil {
		return nil, err
	}
	return out, nil
}

// loadOne reads + strict-decodes one YAML file from the
// embed.FS. Sets [LoadedRulepack.Filename] to `name` so
// caller-facing errors can name the offending file.
func loadOne(name string) (LoadedRulepack, error) {
	raw, err := embeddedRulepacks.ReadFile(name)
	if err != nil {
		return LoadedRulepack{}, fmt.Errorf("solid: read %s: %w", name, err)
	}
	var pack LoadedRulepack
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&pack); err != nil {
		return LoadedRulepack{}, fmt.Errorf("solid: decode %s: %w", name, err)
	}
	pack.Filename = name
	if err := validatePack(pack); err != nil {
		return LoadedRulepack{}, err
	}
	return pack, nil
}

// validatePack enforces the per-file invariants the Steward
// would otherwise enforce later -- catching them here lets
// `LoadAll` fail at bootstrap with a filename-qualified
// message instead of a generic steward error.
func validatePack(p LoadedRulepack) error {
	if !strings.HasPrefix(p.PackID, FamilyPrefix) {
		return fmt.Errorf(
			"solid: %s: pack_id=%q does not start with family prefix %q",
			p.Filename, p.PackID, FamilyPrefix)
	}
	if p.PackID == strings.TrimSuffix(FamilyPrefix, ".") {
		return fmt.Errorf(
			"solid: %s: pack_id=%q is only the family prefix; expected %q+<subname>",
			p.Filename, p.PackID, FamilyPrefix)
	}
	if p.Version <= 0 {
		return fmt.Errorf("solid: %s: version=%d must be > 0", p.Filename, p.Version)
	}
	if strings.TrimSpace(p.DisplayName) == "" {
		return fmt.Errorf("solid: %s: display_name must be non-empty", p.Filename)
	}
	if len(p.Rules) == 0 {
		return fmt.Errorf("solid: %s: rules is empty -- a rulepack must define at least one rule", p.Filename)
	}
	for i, r := range p.Rules {
		if strings.TrimSpace(r.RuleID) == "" {
			return fmt.Errorf("solid: %s: rules[%d]: rule_id must be non-empty", p.Filename, i)
		}
		if r.Version <= 0 {
			return fmt.Errorf("solid: %s: rules[%d] rule_id=%s: version=%d must be > 0", p.Filename, i, r.RuleID, r.Version)
		}
		if strings.TrimSpace(r.PredicateDSL) == "" {
			return fmt.Errorf("solid: %s: rules[%d] rule_id=%s: predicate_dsl must be non-empty", p.Filename, i, r.RuleID)
		}
	}
	return nil
}

// validateCrossFileInvariants pins the two cross-file
// uniqueness invariants described on [LoadAll].
func validateCrossFileInvariants(packs []LoadedRulepack) error {
	packKey := make(map[string]string) // "<pack_id>@v<version>" -> filename
	ruleKey := make(map[string]string) // "<rule_id>@v<version>" -> filename
	for _, p := range packs {
		pk := fmt.Sprintf("%s@v%d", p.PackID, p.Version)
		if first, dup := packKey[pk]; dup {
			return fmt.Errorf(
				"solid: (pack_id=%s, version=%d) declared in both %s and %s -- violates rule_pack PRIMARY KEY (pack_id, version)",
				p.PackID, p.Version, first, p.Filename)
		}
		packKey[pk] = p.Filename
		for _, r := range p.Rules {
			rk := fmt.Sprintf("%s@v%d", r.RuleID, r.Version)
			if first, dup := ruleKey[rk]; dup {
				return fmt.Errorf(
					"solid: (rule_id=%s, version=%d) declared in both %s and %s -- violates rule PRIMARY KEY (rule_id, version)",
					r.RuleID, r.Version, first, p.Filename)
			}
			ruleKey[rk] = p.Filename
		}
	}
	return nil
}
