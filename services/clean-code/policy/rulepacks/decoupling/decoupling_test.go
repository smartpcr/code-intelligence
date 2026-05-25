// Package decoupling holds the test that pins the Stage 5.6
// rulepack files at `policy/rulepacks/decoupling/*.yaml`
// against the canonical schema, the canonical metric_kind
// closed set, and the [LoadAll] embed loader contract.
//
// The package is colocated with the YAML files (`cycles.yaml`,
// `coupling.yaml`, `duplication.yaml`) and the [loader.go]
// production code; the YAMLs are loaded both at runtime via
// `//go:embed` and statically asserted on disk so a future
// drift is caught at build time rather than at startup.
package decoupling

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// canonicalSeverities mirrors the closed set declared in
// `steward.Severity`. Repeated here so the test does not
// depend on importing the steward package's UNEXPORTED set
// (the steward exposes [steward.Severity.IsValid] but the
// repeated literal here is the source of truth for "what
// the YAML files are allowed to carry").
var canonicalSeverities = map[string]struct{}{
	"info":  {},
	"warn":  {},
	"block": {},
}

// rulepackFixtures enumerates the three files this stage adds.
// Pinned here so a future file added to the embed.FS without a
// fixture entry is caught by
// [TestRulepacksOnDisk_LiveWhereWeExpect] and the loader
// count test.
var rulepackFixtures = []string{
	"coupling.yaml",
	"cycles.yaml",
	"duplication.yaml",
}

// expectedPackIDs pins the canonical pack_id closed set
// declared by the three Stage 5.6 YAML files. The keys
// double as fixture filenames; the values are the canonical
// `<family>.<subname>` pack_ids per migration
// `0003_policy_audit_refactor.up.sql` line 200.
var expectedPackIDs = map[string]string{
	"cycles.yaml":      "decoupling.cycles",
	"coupling.yaml":    "decoupling.coupling",
	"duplication.yaml": "decoupling.duplication",
}

// loadAllOrFail wraps [LoadAll] with a Fatalf on error so
// every per-test helper avoids the same err-check boilerplate.
func loadAllOrFail(t *testing.T) []LoadedRulepack {
	t.Helper()
	packs, err := LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	return packs
}

// TestLoadAll_ReturnsExpectedFiles pins that the embed.FS
// surfaces EXACTLY the three Stage 5.6 fixture files (no
// more, no fewer). A regression here means either a file was
// deleted (-> the stage is no longer complete) or a new
// file was added without updating [rulepackFixtures] /
// [expectedPackIDs] -- both worth a failure.
func TestLoadAll_ReturnsExpectedFiles(t *testing.T) {
	t.Parallel()
	packs := loadAllOrFail(t)
	got := make([]string, len(packs))
	for i, p := range packs {
		got[i] = p.Filename
	}
	want := append([]string(nil), rulepackFixtures...)
	sort.Strings(want)
	// LoadAll guarantees deterministic order; the fixture
	// slice is also sorted to keep the comparison stable.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadAll filenames = %v; want %v", got, want)
	}
}

// TestLoadAll_PackIDsAreCanonical pins each file's pack_id to
// its expected `decoupling.<subname>` value from
// [expectedPackIDs].
func TestLoadAll_PackIDsAreCanonical(t *testing.T) {
	t.Parallel()
	packs := loadAllOrFail(t)
	for _, p := range packs {
		want, ok := expectedPackIDs[p.Filename]
		if !ok {
			t.Errorf("%s: filename has no expected pack_id entry", p.Filename)
			continue
		}
		if p.PackID != want {
			t.Errorf("%s: pack_id = %q; want %q", p.Filename, p.PackID, want)
		}
	}
}

// TestLoadAll_FamilyPrefix pins every loaded file's pack_id
// to the `decoupling.` family prefix. Belt-and-braces against
// the loader's own [validatePack] check -- if a future drift
// weakens [validatePack], this test still fails.
func TestLoadAll_FamilyPrefix(t *testing.T) {
	t.Parallel()
	packs := loadAllOrFail(t)
	for _, p := range packs {
		if !strings.HasPrefix(p.PackID, FamilyPrefix) {
			t.Errorf("%s: pack_id=%q does not start with family prefix %q",
				p.Filename, p.PackID, FamilyPrefix)
		}
		if p.PackID == strings.TrimSuffix(FamilyPrefix, ".") {
			t.Errorf("%s: pack_id=%q is the literal family name; expected %q+<subname>",
				p.Filename, p.PackID, FamilyPrefix)
		}
	}
}

// TestLoadAll_PackShape pins the canonical row shape:
// non-empty display_name, version > 0, at least one rule per
// file. The loader's [validatePack] already enforces these;
// duplicated here so a future weakening surfaces in tests.
func TestLoadAll_PackShape(t *testing.T) {
	t.Parallel()
	packs := loadAllOrFail(t)
	for _, p := range packs {
		if p.Version <= 0 {
			t.Errorf("%s: version = %d, want > 0", p.Filename, p.Version)
		}
		if strings.TrimSpace(p.DisplayName) == "" {
			t.Errorf("%s: display_name is empty", p.Filename)
		}
		if len(p.Rules) == 0 {
			t.Errorf("%s: rules is empty -- a rulepack must define at least one rule", p.Filename)
		}
	}
}

// TestRulepacks_PredicatesParse is the implementation-plan
// Stage 5.6 test scenario `decoupling-loads`: "Given the
// three decoupling rulepack files, When the Steward loads
// them, Then `pack='decoupling'` rule_packs exist with parsed
// predicates."
//
// "Parsed predicates" means: every rule's `predicate_dsl`
// text compiles cleanly under the Stage 5.4 [dsl.Parse]
// grammar -- including the `threshold('<uuid>')` atoms used
// by coupling.yaml and duplication.yaml in v1. We do NOT
// resolve the threshold UUIDs here (that would require the
// canonical [Resolver] -- exercised by
// [TestRulepacks_OnlyCanonicalMetricKinds] and by the
// bootstrap e2e tests in `bootstrap_test.go`). At Parse
// time the `threshold('<uuid>')` atoms are syntactically
// valid; binding to the catalogue is a separate phase
// performed at Compile time by [dsl.Compile].
func TestRulepacks_PredicatesParse(t *testing.T) {
	t.Parallel()
	for _, p := range loadAllOrFail(t) {
		p := p
		t.Run(p.Filename, func(t *testing.T) {
			t.Parallel()
			for _, r := range p.Rules {
				if _, err := dsl.Parse(r.PredicateDSL); err != nil {
					t.Errorf("%s rule_id=%s: dsl.Parse(%q): %v",
						p.Filename, r.RuleID, r.PredicateDSL, err)
				}
			}
		})
	}
}

// TestRulepacks_OnlyCanonicalMetricKinds is the
// implementation-plan Stage 5.6 brief "Add a test asserting
// each predicate references only canonical metric_kinds".
//
// Mechanics: for every rule's predicate, parse the DSL and
// walk the AST gathering string literals that appear as the
// RHS of a `metric_kind == '<lit>'` (or LHS-reversed)
// comparison AND every metric_kind that a
// `threshold('<uuid>')` atom resolves to via [Resolver].
// Assert each gathered literal is a member of
// [dsl.CanonicalMetricKinds].
//
// In v1 the DSL parser's own canon-guard ([dsl.canonGuard])
// also enforces the metric_kind literal closed set at Parse
// time, and [steward.InsertThreshold] enforces the canonical
// set for Threshold rows; this test is belt-and-braces over
// both, plus it pins that EVERY rule has at least one
// metric_kind reference (whether literal or threshold-bound)
// -- a rule that referenced no metric_kind would be either a
// bug or a pure dead-code predicate.
func TestRulepacks_OnlyCanonicalMetricKinds(t *testing.T) {
	t.Parallel()
	for _, p := range loadAllOrFail(t) {
		p := p
		t.Run(p.Filename, func(t *testing.T) {
			t.Parallel()
			for _, r := range p.Rules {
				node, err := dsl.Parse(r.PredicateDSL)
				if err != nil {
					t.Fatalf("%s rule_id=%s: dsl.Parse: %v",
						p.Filename, r.RuleID, err)
				}
				kinds, err := collectAllMetricKinds(node)
				if err != nil {
					t.Fatalf("%s rule_id=%s: collectAllMetricKinds: %v",
						p.Filename, r.RuleID, err)
				}
				if len(kinds) == 0 {
					t.Errorf("%s rule_id=%s: predicate references no metric_kind (neither literal nor threshold-bound); the decoupling rules MUST scope to a canonical metric_kind",
						p.Filename, r.RuleID)
					continue
				}
				for _, k := range kinds {
					if !dsl.IsCanonicalMetricKind(k) {
						t.Errorf("%s rule_id=%s: metric_kind=%q is not in dsl.CanonicalMetricKinds",
							p.Filename, r.RuleID, k)
					}
				}
			}
		})
	}
}

// TestRulepacks_SeveritiesAreCanonical pins the
// `severity_default` field on every rule to the canonical
// `{info, warn, block}` closed set declared in
// `steward.Severity`.
func TestRulepacks_SeveritiesAreCanonical(t *testing.T) {
	t.Parallel()
	for _, p := range loadAllOrFail(t) {
		for _, r := range p.Rules {
			if _, ok := canonicalSeverities[r.SeverityDefault]; !ok {
				t.Errorf("%s rule_id=%s: severity_default=%q not in {info, warn, block}",
					p.Filename, r.RuleID, r.SeverityDefault)
			}
		}
	}
}

// TestRulepacks_NoDuplicateRuleIDs guards against a copy-
// paste drift where the same `(rule_id, version)` pair lands
// in two different rulepack files (which would be rejected by
// `steward.Store.InsertRulePackAndRules` at startup, but we
// surface it as a static test failure instead of a runtime
// crash). [LoadAll] also enforces this via
// [validateCrossFileInvariants]; duplicated here for
// defence in depth.
func TestRulepacks_NoDuplicateRuleIDs(t *testing.T) {
	t.Parallel()
	seen := make(map[string]string) // key -> filename of first occurrence
	for _, p := range loadAllOrFail(t) {
		for _, r := range p.Rules {
			key := r.RuleID + "@v" + itoa(r.Version)
			if first, dup := seen[key]; dup {
				t.Errorf("rule_id %q version=%d appears in both %s and %s",
					r.RuleID, r.Version, first, p.Filename)
				continue
			}
			seen[key] = p.Filename
		}
	}
}

// TestRulepacks_NoDuplicatePackIDs guards against two files
// declaring the same `(pack_id, version)` pair which would
// violate the `rule_pack` PRIMARY KEY in migration 0003.
// [LoadAll] also enforces this via
// [validateCrossFileInvariants]; duplicated here as a static
// guard.
func TestRulepacks_NoDuplicatePackIDs(t *testing.T) {
	t.Parallel()
	seen := make(map[string]string) // key -> filename of first occurrence
	for _, p := range loadAllOrFail(t) {
		key := p.PackID + "@v" + itoa(p.Version)
		if first, dup := seen[key]; dup {
			t.Errorf("(pack_id=%s, version=%d) declared in both %s and %s",
				p.PackID, p.Version, first, p.Filename)
			continue
		}
		seen[key] = p.Filename
	}
}

// TestCyclesRule_FiresOnCycleMember is the
// implementation-plan Stage 5.6 test scenario
// `cycles-rule-fires-on-cycle-member`:
//
//	Given a `metric_sample(metric_kind='cycle_member',
//	value=1)`, When the predicate evaluates, Then it returns
//	true (would trigger a Finding).
//
// We compile the rule's `predicate_dsl` end-to-end (no
// resolver needed -- v1 cycles.yaml uses pure literal
// comparisons) and eval it against a hand-built sample that
// matches the scenario's Given.
func TestCyclesRule_FiresOnCycleMember(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "cycles.yaml")
	if len(pack.Rules) != 1 {
		t.Fatalf("cycles.yaml: expected exactly one rule, got %d", len(pack.Rules))
	}
	rule := pack.Rules[0]
	pred, err := dsl.Compile(rule.PredicateDSL, nil)
	if err != nil {
		t.Fatalf("dsl.Compile(%q): %v", rule.PredicateDSL, err)
	}
	sample := dsl.Sample{
		ScopeKind:     "package",
		MetricKind:    "cycle_member",
		MetricVersion: 1,
		Value:         1,
		HasValue:      true,
		Pack:          "base",
		Source:        "computed",
	}
	ok, err := pred.Eval(sample)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !ok {
		t.Errorf("cycles.yaml rule did NOT fire on a cycle_member=1 sample; predicate=%q",
			rule.PredicateDSL)
	}
	// Negative control: cycle_member=0 must NOT fire.
	sample.Value = 0
	ok, err = pred.Eval(sample)
	if err != nil {
		t.Fatalf("Eval (value=0): %v", err)
	}
	if ok {
		t.Errorf("cycles.yaml rule unexpectedly fired on cycle_member=0; predicate=%q",
			rule.PredicateDSL)
	}
	// Sanity: must be severity=block per the brief ("block
	// when any module in a watched scope is in a cycle").
	if rule.SeverityDefault != "block" {
		t.Errorf("cycles.yaml rule severity=%q; want %q per implementation-plan Stage 5.6 line 533",
			rule.SeverityDefault, "block")
	}
}

// TestCouplingRules_CoverThreeMetricKinds pins the
// implementation-plan Stage 5.6 brief "decoupling rule
// consuming `metric_kind IN ('fan_in','fan_out',
// 'coupling_between_objects')`" -- the coupling.yaml file
// covers exactly that three-element closed set, sourced
// from the threshold-bound metric_kind on each rule (the
// v1 predicates are pure `threshold('<uuid>')` atoms, no
// `metric_kind == '<lit>'` literal).
func TestCouplingRules_CoverThreeMetricKinds(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "coupling.yaml")
	expected := map[string]bool{
		"fan_in":                   false,
		"fan_out":                  false,
		"coupling_between_objects": false,
	}
	for _, r := range pack.Rules {
		node, err := dsl.Parse(r.PredicateDSL)
		if err != nil {
			t.Fatalf("rule_id=%s: dsl.Parse: %v", r.RuleID, err)
		}
		kinds, err := collectAllMetricKinds(node)
		if err != nil {
			t.Fatalf("rule_id=%s: collectAllMetricKinds: %v", r.RuleID, err)
		}
		for _, k := range kinds {
			if _, want := expected[k]; want {
				expected[k] = true
			}
		}
	}
	for kind, covered := range expected {
		if !covered {
			t.Errorf("coupling.yaml: metric_kind %q is not covered by any rule (the three-element closed set is fan_in, fan_out, coupling_between_objects)",
				kind)
		}
	}
}

// TestDuplicationRule_UsesDuplicationRatio pins the
// implementation-plan Stage 5.6 brief "decoupling rule
// consuming `metric_kind='duplication_ratio'`". The v1
// predicate is a pure `threshold('<uuid>')` atom; the
// metric_kind is sourced from the threshold-bound resolver.
func TestDuplicationRule_UsesDuplicationRatio(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "duplication.yaml")
	var kinds []string
	for _, r := range pack.Rules {
		node, err := dsl.Parse(r.PredicateDSL)
		if err != nil {
			t.Fatalf("rule_id=%s: dsl.Parse: %v", r.RuleID, err)
		}
		k, err := collectAllMetricKinds(node)
		if err != nil {
			t.Fatalf("rule_id=%s: collectAllMetricKinds: %v", r.RuleID, err)
		}
		kinds = append(kinds, k...)
	}
	if len(kinds) == 0 {
		t.Fatalf("duplication.yaml: no metric_kind found (neither literal nor threshold-bound)")
	}
	for _, k := range kinds {
		if k != "duplication_ratio" {
			t.Errorf("duplication.yaml: metric_kind=%q; the duplication rulepack must reference only `duplication_ratio`",
				k)
		}
	}
}

// TestLoadedRulepack_ToPublishRulepackRequest_PreservesFields
// pins that the [LoadedRulepack.ToPublishRulepackRequest]
// converter loses no fields. A future drift that drops e.g.
// `description_md` would slip silently into the Steward
// otherwise -- this test makes the round-trip explicit.
func TestLoadedRulepack_ToPublishRulepackRequest_PreservesFields(t *testing.T) {
	t.Parallel()
	for _, p := range loadAllOrFail(t) {
		p := p
		t.Run(p.Filename, func(t *testing.T) {
			t.Parallel()
			req, err := p.ToPublishRulepackRequest()
			if err != nil {
				t.Fatalf("ToPublishRulepackRequest: %v", err)
			}
			if req.PackID != p.PackID {
				t.Errorf("pack_id: got %q, want %q", req.PackID, p.PackID)
			}
			if req.Version != p.Version {
				t.Errorf("version: got %d, want %d", req.Version, p.Version)
			}
			if req.DisplayName != p.DisplayName {
				t.Errorf("display_name: got %q, want %q", req.DisplayName, p.DisplayName)
			}
			if req.DescriptionMD != p.DescriptionMD {
				t.Errorf("description_md: got %q, want %q", req.DescriptionMD, p.DescriptionMD)
			}
			if len(req.Rules) != len(p.Rules) {
				t.Fatalf("rules len: got %d, want %d", len(req.Rules), len(p.Rules))
			}
			for i, want := range p.Rules {
				got := req.Rules[i]
				if got.RuleID != want.RuleID {
					t.Errorf("rules[%d].rule_id: got %q, want %q", i, got.RuleID, want.RuleID)
				}
				if got.Version != want.Version {
					t.Errorf("rules[%d].version: got %d, want %d", i, got.Version, want.Version)
				}
				if got.PredicateDSL != want.PredicateDSL {
					t.Errorf("rules[%d].predicate_dsl: got %q, want %q", i, got.PredicateDSL, want.PredicateDSL)
				}
				if string(got.SeverityDefault) != want.SeverityDefault {
					t.Errorf("rules[%d].severity_default: got %q, want %q", i, got.SeverityDefault, want.SeverityDefault)
				}
				if got.DescriptionMD != want.DescriptionMD {
					t.Errorf("rules[%d].description_md: got %q, want %q", i, got.DescriptionMD, want.DescriptionMD)
				}
			}
			// Sanity: the converted request type-asserts the
			// severity into a `steward.Severity` whose
			// IsValid() must be true (the converter would
			// have errored otherwise -- this is double-check).
			for i, r := range req.Rules {
				if !r.SeverityDefault.IsValid() {
					t.Errorf("rules[%d]: steward.Severity %q reports IsValid()=false",
						i, r.SeverityDefault)
				}
			}
			// Concrete type assertion -- compiler-checked
			// elsewhere but explicit here so a future
			// signature drift on PublishRulepackRequest
			// surfaces as a build failure tied to this test.
			var _ steward.PublishRulepackRequest = req
		})
	}
}

// TestRulepacksOnDisk_LiveWhereWeExpect is a sanity check
// that the YAMLs are colocated with this test file. The
// embed.FS proves the binary carries them; this test proves
// the source tree still does too (so a `go generate` that
// re-emits the files works, and so a future operator-mode
// reload can read from disk if needed).
func TestRulepacksOnDisk_LiveWhereWeExpect(t *testing.T) {
	t.Parallel()
	for _, name := range rulepackFixtures {
		abs, err := filepath.Abs(name)
		if err != nil {
			t.Errorf("filepath.Abs(%s): %v", name, err)
			continue
		}
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("rulepack file %s missing on disk: %v", abs, err)
		}
	}
}

// findPackByFilename returns the LoadedRulepack whose
// Filename matches `name`. Fatalfs if not found.
func findPackByFilename(t *testing.T, name string) LoadedRulepack {
	t.Helper()
	for _, p := range loadAllOrFail(t) {
		if p.Filename == name {
			return p
		}
	}
	t.Fatalf("findPackByFilename(%q): not in LoadAll() result", name)
	return LoadedRulepack{}
}

// itoa is a local int -> string helper that avoids pulling
// strconv (and the associated import block churn) for a
// single call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
