// Package solid holds the test that pins the Stage 5.5
// rulepack files at `policy/rulepacks/solid/*.yaml` against
// the canonical schema, the canonical metric_kind closed
// set, and the [LoadAll] embed loader contract.
//
// The package is colocated with the YAML files (`srp.yaml`,
// `ocp.yaml`, `lsp.yaml`, `isp.yaml`, `dip.yaml`) and the
// [loader.go] production code; the YAMLs are loaded both at
// runtime via `//go:embed` and statically asserted on disk so
// a future drift is caught at build time rather than at
// startup.
package solid

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
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

// rulepackFixtures enumerates the five files this stage adds.
// Pinned here so a future file added to the embed.FS without a
// fixture entry is caught by
// [TestRulepacksOnDisk_LiveWhereWeExpect] and the loader
// count test.
var rulepackFixtures = []string{
	"dip.yaml",
	"isp.yaml",
	"lsp.yaml",
	"ocp.yaml",
	"srp.yaml",
}

// expectedPackIDs pins the canonical pack_id closed set
// declared by the five Stage 5.5 YAML files. The keys
// double as fixture filenames; the values are the canonical
// `<family>.<subname>` pack_ids per migration
// `0003_policy_audit_refactor.up.sql` line 200.
var expectedPackIDs = map[string]string{
	"srp.yaml": "solid.srp",
	"ocp.yaml": "solid.ocp",
	"lsp.yaml": "solid.lsp",
	"isp.yaml": "solid.isp",
	"dip.yaml": "solid.dip",
}

// expectedRuleCounts pins the rule_count per file. A change
// here means the canonical SOLID rule catalogue has drifted
// and the architecture / implementation-plan references need
// updating in lockstep.
var expectedRuleCounts = map[string]int{
	"srp.yaml": 2, // lcom4_high + interface_width_high
	"ocp.yaml": 2, // fan_in_high + modification_count_high
	"lsp.yaml": 2, // depth_of_inheritance_high + override_violation
	"isp.yaml": 1, // interface_width_high
	"dip.yaml": 2, // fan_out_high + coupling_between_objects_high
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
// surfaces EXACTLY the five Stage 5.5 fixture files (no
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
// its expected `solid.<subname>` value from
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
// to the `solid.` family prefix. Belt-and-braces against the
// loader's own [validatePack] check -- if a future drift
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
// non-empty display_name, version > 0, expected rule count
// per file. The loader's [validatePack] already enforces the
// first two; the rule count is the canonical Stage 5.5
// catalogue (5 packs, 9 rules total -- iter-2 added the
// `solid.lsp.override_violation` rule to consume the
// override-method-signature signal from Stage 2.4).
func TestLoadAll_PackShape(t *testing.T) {
	t.Parallel()
	packs := loadAllOrFail(t)
	totalRules := 0
	for _, p := range packs {
		if p.Version <= 0 {
			t.Errorf("%s: version = %d, want > 0", p.Filename, p.Version)
		}
		if strings.TrimSpace(p.DisplayName) == "" {
			t.Errorf("%s: display_name is empty", p.Filename)
		}
		want, ok := expectedRuleCounts[p.Filename]
		if !ok {
			t.Errorf("%s: filename has no expected rule_count entry", p.Filename)
			continue
		}
		if len(p.Rules) != want {
			t.Errorf("%s: rules len = %d; want %d", p.Filename, len(p.Rules), want)
		}
		totalRules += len(p.Rules)
	}
	const wantTotal = 9
	if totalRules != wantTotal {
		t.Errorf("total Stage 5.5 SOLID rules = %d; want %d", totalRules, wantTotal)
	}
}

// TestRulepacks_PredicatesParse is the implementation-plan
// Stage 5.5 test scenario `solid-rulepacks-load`: "Given the
// five SOLID rulepack files, When the Steward loads them,
// Then `pack='solid'` rule_packs exist with parsed
// predicates."
//
// "Parsed predicates" means: every rule's `predicate_dsl`
// text compiles cleanly under the Stage 5.4 [dsl.Parse]
// grammar. SOLID v1 uses pure literal comparisons (no
// `threshold('<uuid>')` atoms), so Parse is sufficient --
// no resolver is required.
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

// TestRulepacks_PredicatesCompile pins the stronger
// acceptance: every rule's predicate also COMPILES (parse +
// bind with a nil resolver, since v1 uses no
// `threshold('<uuid>')` atoms). [dsl.Compile] surfaces type
// errors the bare Parse would let through.
func TestRulepacks_PredicatesCompile(t *testing.T) {
	t.Parallel()
	for _, p := range loadAllOrFail(t) {
		p := p
		t.Run(p.Filename, func(t *testing.T) {
			t.Parallel()
			for _, r := range p.Rules {
				if _, err := dsl.Compile(r.PredicateDSL, nil); err != nil {
					t.Errorf("%s rule_id=%s: dsl.Compile(%q): %v",
						p.Filename, r.RuleID, r.PredicateDSL, err)
				}
			}
		})
	}
}

// TestRulepacks_OnlyCanonicalMetricKinds is the
// implementation-plan Stage 5.5 test scenario
// `solid-rulepacks-only-canonical-kinds`: every metric_kind
// the SOLID rulepacks reference is in the canonical closed
// set declared by `dsl.CanonicalMetricKinds`.
//
// Mechanics: for every rule's predicate, parse the DSL and
// walk the AST gathering string literals on the RHS of a
// `metric_kind == '<lit>'` (or LHS-reversed) comparison.
// Assert each gathered literal is a member of
// [dsl.CanonicalMetricKinds].
//
// In v1 the DSL parser's own canon-guard ([dsl.canonGuard])
// also enforces the metric_kind literal closed set at Parse
// time; this test is belt-and-braces over that, plus it pins
// that EVERY rule has at least one metric_kind reference --
// a rule that referenced no metric_kind would be either a
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
				kinds := collectMetricKindLiterals(node)
				if len(kinds) == 0 {
					t.Errorf("%s rule_id=%s: predicate references no metric_kind literal; the SOLID rules MUST scope to a canonical metric_kind",
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

// TestRulepacks_HaveScopeKindBinding pins that every SOLID
// rule predicate carries an explicit `scope_kind == '<lit>'`
// comparison. The architecture Sec 1.4.1 notes several
// SOLID metric_kinds live at multiple scopes (e.g.
// `interface_width` on both class AND interface; `fan_in` /
// `fan_out` on method, class, file); without a scope_kind
// binding a SRP rule would accidentally fire on an
// interface-scoped sample and a DIP rule would fire on a
// method-scoped one. The scope_kind binding is the explicit
// contract the implementation-plan Stage 5.5 brief assumes.
func TestRulepacks_HaveScopeKindBinding(t *testing.T) {
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
				scopes := collectScopeKindLiterals(node)
				if len(scopes) == 0 {
					t.Errorf("%s rule_id=%s: predicate has no scope_kind binding; every SOLID rule MUST pin a scope_kind to disambiguate samples (architecture Sec 1.4.1)",
						p.Filename, r.RuleID)
					continue
				}
				for _, s := range scopes {
					if !dsl.IsCanonicalScopeKind(s) {
						t.Errorf("%s rule_id=%s: scope_kind=%q is not in dsl.CanonicalScopeKinds",
							p.Filename, r.RuleID, s)
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

// TestOCPRules_UsesFanInAndModificationCount is the
// implementation-plan Stage 5.5 test scenario
// `ocp-uses-fan-in` (iter-1 evaluator item 10 canon-guard):
// the OCP rulepack inputs MUST be exactly
// `{fan_in, modification_count_in_window}` -- NOT
// `depth_of_inheritance`. The architecture Sec 3.5.1.b
// lines 504-513 and tech-spec Sec 4.3 lines 341-351 pin
// this signal pair as the canonical OCP smell ("a class
// that is widely depended-on AND frequently edited").
//
// Without this guard a future drift might swap in
// `depth_of_inheritance` (the legacy OCP indicator) which
// is the LSP signal in this catalogue.
func TestOCPRules_UsesFanInAndModificationCount(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "ocp.yaml")
	wantInputs := map[string]bool{
		"fan_in":                       false,
		"modification_count_in_window": false,
	}
	forbidden := map[string]struct{}{
		"depth_of_inheritance": {},
	}
	for _, r := range pack.Rules {
		node, err := dsl.Parse(r.PredicateDSL)
		if err != nil {
			t.Fatalf("rule_id=%s: dsl.Parse: %v", r.RuleID, err)
		}
		kinds := collectMetricKindLiterals(node)
		for _, k := range kinds {
			if _, banned := forbidden[k]; banned {
				t.Errorf("ocp.yaml rule_id=%s: metric_kind=%q is forbidden for OCP (architecture Sec 3.5.1.b: OCP smells are fan_in + modification_count_in_window, NOT depth_of_inheritance)",
					r.RuleID, k)
			}
			if _, want := wantInputs[k]; want {
				wantInputs[k] = true
			}
		}
	}
	for kind, covered := range wantInputs {
		if !covered {
			t.Errorf("ocp.yaml: metric_kind %q is not covered by any rule (the canonical OCP input pair is {fan_in, modification_count_in_window})",
				kind)
		}
	}
}

// TestSRPRules_UsesLcom4AndInterfaceWidth pins the
// implementation-plan Stage 5.5 brief "SRP rule per
// architecture Sec 1.3 consuming `metric_kind IN
// ('lcom4','interface_width')`". The srp.yaml file covers
// exactly that two-element closed set.
func TestSRPRules_UsesLcom4AndInterfaceWidth(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "srp.yaml")
	expected := map[string]bool{
		"lcom4":           false,
		"interface_width": false,
	}
	for _, r := range pack.Rules {
		node, err := dsl.Parse(r.PredicateDSL)
		if err != nil {
			t.Fatalf("rule_id=%s: dsl.Parse: %v", r.RuleID, err)
		}
		for _, k := range collectMetricKindLiterals(node) {
			if _, want := expected[k]; want {
				expected[k] = true
			}
		}
	}
	for kind, covered := range expected {
		if !covered {
			t.Errorf("srp.yaml: metric_kind %q is not covered by any rule (the canonical SRP input pair is {lcom4, interface_width})",
				kind)
		}
	}
}

// TestLSPRules_UseDITAndOverrideViolation pins the
// implementation-plan Stage 5.5 brief "LSP rule consuming
// `metric_kind='depth_of_inheritance'` plus override-method-
// signature checks emitted by Stage 2.4" (line 514). The
// LSP rulepack MUST carry exactly TWO rules whose
// metric_kinds, in aggregate, equal the canonical pair
// `{depth_of_inheritance, lsp_violation}`. Architecture Sec
// 3.5.1.c lines 514-518 frames the override signal as a
// boolean attribute on `MetricSample.attrs_json`; the v1
// DSL has no attribute accessor, so the signal is encoded
// in-band as the `lsp_violation` 0/1-indicator metric_kind
// (canonicalised under `dsl.CanonicalMetricKinds` with the
// same precedent as `cycle_member`). The test pins both
// rule IDs by name so a future accidental rename / drop
// surfaces here rather than silently passing the
// metric_kind-only check.
func TestLSPRules_UseDITAndOverrideViolation(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "lsp.yaml")
	wantRuleIDs := map[string]string{
		"solid.lsp.depth_of_inheritance_high": "depth_of_inheritance",
		"solid.lsp.override_violation":        "lsp_violation",
	}
	sawRule := make(map[string]bool, len(wantRuleIDs))
	sawKind := map[string]bool{
		"depth_of_inheritance": false,
		"lsp_violation":        false,
	}
	for _, r := range pack.Rules {
		node, err := dsl.Parse(r.PredicateDSL)
		if err != nil {
			t.Fatalf("rule_id=%s: dsl.Parse: %v", r.RuleID, err)
		}
		wantKind, isExpectedRule := wantRuleIDs[r.RuleID]
		if !isExpectedRule {
			expected := make([]string, 0, len(wantRuleIDs))
			for id := range wantRuleIDs {
				expected = append(expected, id)
			}
			sort.Strings(expected)
			t.Errorf("lsp.yaml: unexpected rule_id=%q (canonical set: %v)",
				r.RuleID, expected)
			continue
		}
		sawRule[r.RuleID] = true
		kinds := collectMetricKindLiterals(node)
		if len(kinds) != 1 || kinds[0] != wantKind {
			t.Errorf("lsp.yaml rule_id=%s: metric_kinds=%v; want exactly [%q]",
				r.RuleID, kinds, wantKind)
		}
		for _, k := range kinds {
			if _, want := sawKind[k]; want {
				sawKind[k] = true
			}
		}
	}
	for ruleID := range wantRuleIDs {
		if !sawRule[ruleID] {
			t.Errorf("lsp.yaml: rule_id=%q missing -- canonical LSP rule set is {solid.lsp.depth_of_inheritance_high, solid.lsp.override_violation}",
				ruleID)
		}
	}
	for kind, ok := range sawKind {
		if !ok {
			t.Errorf("lsp.yaml: metric_kind %q not covered by any rule (canonical LSP inputs: {depth_of_inheritance, lsp_violation})",
				kind)
		}
	}
}

// TestISPRule_UsesInterfaceWidthOnInterfaceScope pins the
// scope split between SRP and ISP. SRP fires
// `interface_width` on `class` scope; ISP fires it on
// `interface` scope. Without the split they would emit
// identical findings on a shared sample, which the
// implementation-plan Stage 5.5 brief explicitly
// disallows.
func TestISPRule_UsesInterfaceWidthOnInterfaceScope(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "isp.yaml")
	if len(pack.Rules) != 1 {
		t.Fatalf("isp.yaml: expected exactly one rule, got %d", len(pack.Rules))
	}
	rule := pack.Rules[0]
	node, err := dsl.Parse(rule.PredicateDSL)
	if err != nil {
		t.Fatalf("rule_id=%s: dsl.Parse: %v", rule.RuleID, err)
	}
	metricKinds := collectMetricKindLiterals(node)
	if !contains(metricKinds, "interface_width") {
		t.Errorf("isp.yaml rule_id=%s: missing `interface_width` metric_kind literal; got %v",
			rule.RuleID, metricKinds)
	}
	scopeKinds := collectScopeKindLiterals(node)
	if !contains(scopeKinds, "interface") {
		t.Errorf("isp.yaml rule_id=%s: missing `interface` scope_kind binding; got %v -- ISP must fire on interface scope (vs SRP on class scope)",
			rule.RuleID, scopeKinds)
	}
}

// TestDIPRules_UsesFanOutAndCBO pins the canonical DIP input
// pair (architecture Sec 3.5.1.c): high concrete fan_out +
// high Coupling Between Objects. Both must scope to `class`
// to avoid colliding with the SRP / OCP scope bindings.
func TestDIPRules_UsesFanOutAndCBO(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "dip.yaml")
	expected := map[string]bool{
		"fan_out":                  false,
		"coupling_between_objects": false,
	}
	for _, r := range pack.Rules {
		node, err := dsl.Parse(r.PredicateDSL)
		if err != nil {
			t.Fatalf("rule_id=%s: dsl.Parse: %v", r.RuleID, err)
		}
		for _, k := range collectMetricKindLiterals(node) {
			if _, want := expected[k]; want {
				expected[k] = true
			}
		}
	}
	for kind, covered := range expected {
		if !covered {
			t.Errorf("dip.yaml: metric_kind %q is not covered by any rule (the canonical DIP input pair is {fan_out, coupling_between_objects})",
				kind)
		}
	}
}

// TestSRPRule_FiresOnHighLcom4 exercises the canonical
// srp.lcom4_high rule end-to-end: compile the literal
// predicate and eval against a synthetic class-scoped
// `lcom4=12` sample. Boundary cases pin the `>= 10` cut-off.
func TestSRPRule_FiresOnHighLcom4(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "srp.yaml")
	rule := findRule(t, pack, "solid.srp.lcom4_high")
	pred, err := dsl.Compile(rule.PredicateDSL, nil)
	if err != nil {
		t.Fatalf("dsl.Compile(%q): %v", rule.PredicateDSL, err)
	}
	cases := []struct {
		name      string
		scopeKind string
		value     float64
		wantOK    bool
	}{
		{"class_below_threshold_does_not_fire", "class", 5, false},
		{"class_just_below_threshold_does_not_fire", "class", 9.999, false},
		{"class_at_threshold_fires", "class", 10, true},
		{"class_above_threshold_fires", "class", 50, true},
		{"interface_scope_does_not_fire_even_if_value_high", "interface", 100, false},
		{"method_scope_does_not_fire_even_if_value_high", "method", 100, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := dsl.Sample{
				ScopeKind:     c.scopeKind,
				MetricKind:    "lcom4",
				MetricVersion: 1,
				Value:         c.value,
				HasValue:      true,
				Pack:          "solid",
				Source:        "computed",
			}
			ok, err := pred.Eval(s)
			if err != nil {
				t.Fatalf("Eval(scope=%s, value=%v): %v", c.scopeKind, c.value, err)
			}
			if ok != c.wantOK {
				t.Errorf("Eval(scope=%s, value=%v) = %v; want %v",
					c.scopeKind, c.value, ok, c.wantOK)
			}
		})
	}
}

// TestOCPRule_FiresOnHighFanIn exercises the canonical
// solid.ocp.fan_in_high rule end-to-end on the canonical
// class-scoped fan_in input.
func TestOCPRule_FiresOnHighFanIn(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "ocp.yaml")
	rule := findRule(t, pack, "solid.ocp.fan_in_high")
	pred, err := dsl.Compile(rule.PredicateDSL, nil)
	if err != nil {
		t.Fatalf("dsl.Compile(%q): %v", rule.PredicateDSL, err)
	}
	cases := []struct {
		name   string
		value  float64
		wantOK bool
	}{
		{"below_threshold_does_not_fire", 15, false},
		{"at_threshold_fires", 20, true},
		{"above_threshold_fires", 100, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := dsl.Sample{
				ScopeKind:     "class",
				MetricKind:    "fan_in",
				MetricVersion: 1,
				Value:         c.value,
				HasValue:      true,
				Pack:          "solid",
				Source:        "computed",
			}
			ok, err := pred.Eval(s)
			if err != nil {
				t.Fatalf("Eval(value=%v): %v", c.value, err)
			}
			if ok != c.wantOK {
				t.Errorf("Eval(value=%v) = %v; want %v", c.value, ok, c.wantOK)
			}
		})
	}
}

// TestLSPRule_FiresOnDeepInheritance exercises the canonical
// solid.lsp.depth_of_inheritance_high rule end-to-end on the
// canonical class-scoped DIT input.
func TestLSPRule_FiresOnDeepInheritance(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "lsp.yaml")
	rule := findRule(t, pack, "solid.lsp.depth_of_inheritance_high")
	pred, err := dsl.Compile(rule.PredicateDSL, nil)
	if err != nil {
		t.Fatalf("dsl.Compile(%q): %v", rule.PredicateDSL, err)
	}
	cases := []struct {
		name   string
		value  float64
		wantOK bool
	}{
		{"shallow_does_not_fire", 1, false},
		{"just_below_threshold_does_not_fire", 3, false},
		{"at_threshold_fires", 4, true},
		{"deep_fires", 10, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := dsl.Sample{
				ScopeKind:     "class",
				MetricKind:    "depth_of_inheritance",
				MetricVersion: 1,
				Value:         c.value,
				HasValue:      true,
				Pack:          "solid",
				Source:        "computed",
			}
			ok, err := pred.Eval(s)
			if err != nil {
				t.Fatalf("Eval(value=%v): %v", c.value, err)
			}
			if ok != c.wantOK {
				t.Errorf("Eval(value=%v) = %v; want %v", c.value, ok, c.wantOK)
			}
		})
	}
}

// TestLSPRule_FiresOnOverrideViolation exercises the
// canonical solid.lsp.override_violation rule end-to-end:
// compile the literal predicate and eval against a synthetic
// method-scoped `lsp_violation` indicator sample. The metric
// is a 0/1 indicator (Stage 2.4 emits value=1 when the
// override fails the Liskov substitution contract), so the
// `> 0` boundary fires on value=1 but not on value=0.
// Method-scope binding is checked by feeding non-method
// scopes that must NOT fire.
func TestLSPRule_FiresOnOverrideViolation(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "lsp.yaml")
	rule := findRule(t, pack, "solid.lsp.override_violation")
	pred, err := dsl.Compile(rule.PredicateDSL, nil)
	if err != nil {
		t.Fatalf("dsl.Compile(%q): %v", rule.PredicateDSL, err)
	}
	cases := []struct {
		name      string
		scopeKind string
		value     float64
		wantOK    bool
	}{
		{"method_no_violation_does_not_fire", "method", 0, false},
		{"method_violation_fires", "method", 1, true},
		{"method_fractional_violation_fires", "method", 0.5, true},
		{"class_scope_does_not_fire_even_on_violation", "class", 1, false},
		{"file_scope_does_not_fire_even_on_violation", "file", 1, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := dsl.Sample{
				ScopeKind:     c.scopeKind,
				MetricKind:    "lsp_violation",
				MetricVersion: 1,
				Value:         c.value,
				HasValue:      true,
				Pack:          "solid",
				Source:        "computed",
			}
			ok, err := pred.Eval(s)
			if err != nil {
				t.Fatalf("Eval(scope=%s, value=%v): %v", c.scopeKind, c.value, err)
			}
			if ok != c.wantOK {
				t.Errorf("Eval(scope=%s, value=%v) = %v; want %v",
					c.scopeKind, c.value, ok, c.wantOK)
			}
		})
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
			// Sanity: every converted severity is valid.
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
// the source tree still does too.
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

// findRule returns the LoadedRuleSpec in `pack` whose
// RuleID matches `id`. Fatalfs if not found.
func findRule(t *testing.T, pack LoadedRulepack, id string) LoadedRuleSpec {
	t.Helper()
	for _, r := range pack.Rules {
		if r.RuleID == id {
			return r
		}
	}
	t.Fatalf("findRule(%q): not in pack %s", id, pack.Filename)
	return LoadedRuleSpec{}
}

// contains reports whether s is an element of xs.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
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
