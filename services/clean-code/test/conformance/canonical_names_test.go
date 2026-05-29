// Stage 10.3 canonical-names conformance test.
//
// Walks every source-of-truth location named in the
// implementation-plan (Stage 10.3 step 2,
// `docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md`
// line 871) -- `policy/rulepacks/**/*.yaml`,
// `internal/metrics/recipes/**`, and
// `internal/aggregator/system_tier.go` -- and asserts every
// `metric_kind` literal observed in those locations is a
// member of the closed canonical set declared by
// architecture Sec 1.4.1 + Sec 1.4.2 + tech-spec Sec 4.1.1.
//
// The canonical set is pinned LITERALLY in this file (see
// [canonicalMetricKinds]) as the spec-anchored truth.
// `internal/policy/dsl.CanonicalMetricKinds` is the runtime
// canon-guard the DSL parser consults; this test asserts the
// two sets agree, so any drift in either direction (an
// architecture edit that fails to update the dsl map, OR a
// dsl-side edit that smuggles a non-architectural name into
// the runtime set) is caught here at test time before the
// PR lands. The iter-1 evaluator findings 3, 4, 7 are the
// historical regressions this test guards against.
//
// # Why we extract metric_kinds physically rather than
// trust the canonical slices the production packages export
//
// The brief is "read every metric_kind reference" in the
// three locations -- not "trust the exported canonical
// slice". A stray non-canonical string literal living in
// `system_tier.go` next to the canonical slice would be a
// regression the slice-only check could not catch. We
// therefore parse each Go source file with `go/ast` and walk
// every `const ... = "..."` that ends in `MetricKind` (the
// canonical naming pattern recipes use) OR that starts with
// `SystemMetric` (the canonical pattern in `system_tier.go`),
// and harvest the value. The same logic also asserts the
// exported canonical slices match the harvested literals so
// a slice-only refactor cannot diverge.
//
// # Threshold-bound metric_kinds (yaml indirect references)
//
// Several decoupling rulepack predicates use
// `threshold('<uuid>')` atoms instead of the direct
// `metric_kind == 'xxx'` form (see
// `policy/rulepacks/decoupling/{coupling,duplication}.yaml`).
// The metric_kind binding lives on the referenced Threshold
// row, not in the predicate text. We resolve those UUIDs via
// `decoupling.ListCanonicalThresholds()` (the in-package
// source of truth) and surface each row's `.MetricKind` as
// an observed reference. A threshold UUID referenced in YAML
// but missing from the canonical list is a fatal test error.
package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/policy/rulepacks/decoupling"
)

// canonicalMetricKinds is the SPEC-ANCHORED closed set of
// every `metric_kind` literal the v1 clean-code surface may
// reference. Each entry is annotated with the architecture
// or tech-spec line it mirrors, so a doc edit reviewer can
// cross-check this map without leaving the file.
//
// Edits to this map MUST be paired with an edit to:
//
//   - `docs/stories/code-intelligence-CLEAN-CODE/architecture.md`
//     Sec 1.4.1 / 1.4.2 (foundation + system tiers), OR
//   - `docs/stories/code-intelligence-CLEAN-CODE/tech-spec.md`
//     Sec 4.1.1 (ingested-pack tier), AND
//   - `internal/policy/dsl.CanonicalMetricKinds`
//     (the runtime closed-set guard).
//
// The [TestCanonicalMetricKinds_DSLAgrees] sub-test pins the
// triple-update invariant: it asserts the dsl map equals
// this map element-for-element. A doc edit that bumps one
// without the other surfaces here.
//
// # Tier totals (v1)
//
//   - Foundation tier (`pack='base'`):   6 kinds
//     (cyclo, cognitive_complexity, loc, cycle_member,
//      duplication_ratio, modification_count_in_window).
//   - Foundation tier (`pack='solid'`):  7 kinds
//     (lcom4, fan_in, fan_out, depth_of_inheritance,
//      interface_width, coupling_between_objects,
//      lsp_violation).
//   - System tier (`pack='system'`):     7 kinds
//     (xrepo_dep_depth, arch_debt_ratio, velocity_trend,
//      arch_fitness, blast_radius, xservice_test_reliability,
//      knowledge_index).
//   - Ingested tier (`pack='ingested'`): 3 kinds
//     (coverage_line_ratio, coverage_branch_ratio,
//      pass_first_try_ratio).
//
// Total: 23. Any edit that changes this count MUST also
// update the corresponding architecture row and the dsl
// map; the test fails loudly otherwise.
var canonicalMetricKinds = map[string]struct{}{
	// Foundation tier, pack='base' (architecture Sec 1.4.1).
	"cyclo":                        {}, // row 1
	"cognitive_complexity":         {}, // row 2
	"loc":                          {}, // row 3
	"cycle_member":                 {}, // row 10
	"duplication_ratio":            {}, // row 11
	"modification_count_in_window": {}, // row 12

	// Foundation tier, pack='solid' (architecture Sec 1.4.1).
	"lcom4":                    {}, // row 4
	"fan_in":                   {}, // row 5
	"fan_out":                  {}, // row 6
	"depth_of_inheritance":     {}, // row 7
	"interface_width":          {}, // row 8
	"coupling_between_objects": {}, // row 9
	"lsp_violation":            {}, // row 13 -- the LSP override 0/1 indicator

	// System tier, pack='system' (architecture Sec 1.4.2).
	"xrepo_dep_depth":           {}, // row 1
	"arch_debt_ratio":           {}, // row 2
	"velocity_trend":            {}, // row 3
	"arch_fitness":              {}, // row 4
	"blast_radius":              {}, // row 5
	"xservice_test_reliability": {}, // row 6
	"knowledge_index":           {}, // row 7

	// Ingested tier, pack='ingested' (tech-spec Sec 4.1.1
	// table at lines 302-304). The v1 set is exactly these
	// three -- legacy aliases `coverage_line` / `coverage_branch`
	// are NEVER canonical (iter-1 evaluator item 7 anti-pattern).
	"coverage_line_ratio":   {},
	"coverage_branch_ratio": {},
	"pass_first_try_ratio":  {},
}

// metricKindEqualsLiteralRE matches the direct DSL form
// `metric_kind == 'xxx'` inside a predicate string. Used
// when scanning `predicate_dsl` YAML values to harvest
// the metric_kind literal each rule's predicate names.
//
// The regex tolerates either single or double quotes, any
// amount of inter-token whitespace, and the equality
// operators the DSL grammar accepts (`==` only -- the
// grammar rejects `=` per the parser's tokeniser).
var metricKindEqualsLiteralRE = regexp.MustCompile(
	`metric_kind\s*==\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]`)

// thresholdAtomRE matches the indirect DSL form
// `threshold('<uuid>')` inside a predicate string. The
// matched UUID is resolved via
// [decoupling.ListCanonicalThresholds] to recover the
// underlying metric_kind binding.
var thresholdAtomRE = regexp.MustCompile(
	`threshold\(\s*['"]([0-9a-fA-F-]{36})['"]\s*\)`)

// serviceRoot resolves to the absolute path of the
// `services/clean-code/` directory (the Go module root)
// regardless of the cwd `go test` is invoked from. Tests
// run with cwd set to the package directory, so we walk
// two levels UP from this file
// (`services/clean-code/test/conformance/`).
//
// The function uses `runtime.Caller(0)` to anchor to THIS
// source file's absolute path; that anchor survives module
// vendoring, cross-platform path separators, and
// CI-runner working-directory drift.
func serviceRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed -- the test binary lost its source-file metadata; rebuild without `-trimpath`")
	}
	// thisFile = services/clean-code/test/conformance/canonical_names_test.go
	// service root  = services/clean-code/
	dir := filepath.Dir(thisFile)            // .../test/conformance
	dir = filepath.Dir(filepath.Dir(dir))    // .../services/clean-code
	return dir
}

// collectYAMLMetricKinds walks every YAML rulepack under
// `policy/rulepacks/**` and returns the set of metric_kind
// literals their `predicate_dsl` fields reference (direct
// `metric_kind == 'xxx'` form), unioned with the set of
// metric_kind bindings the YAML's `threshold('<uuid>')`
// atoms resolve to via
// [decoupling.ListCanonicalThresholds].
//
// Each observed name is paired with a short provenance
// string -- "<pack_id>/<rule_id>" -- so a non-canonical
// finding can be traced to the exact YAML rule.
func collectYAMLMetricKinds(t *testing.T, root string) map[string][]string {
	t.Helper()
	rulepacksDir := filepath.Join(root, "policy", "rulepacks")
	observed := make(map[string][]string)

	// Build a UUID -> metric_kind index from the canonical
	// decoupling threshold seed (the only YAML rulepacks
	// that use `threshold('<uuid>')` atoms today). Future
	// rulepack families that add their own seeds MUST
	// register a sibling helper here; we surface an
	// unresolved UUID as a fatal test error so the gap
	// cannot be silent.
	thresholdIndex := make(map[string]string)
	for _, th := range decoupling.ListCanonicalThresholds() {
		thresholdIndex[th.ThresholdID.String()] = th.MetricKind
	}

	err := filepath.WalkDir(rulepacksDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".yaml") &&
			!strings.HasSuffix(strings.ToLower(d.Name()), ".yml") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read rulepack %s: %v", path, readErr)
		}
		var doc rulepackYAML
		if uErr := yaml.Unmarshal(raw, &doc); uErr != nil {
			t.Fatalf("unmarshal rulepack %s: %v", path, uErr)
		}
		for _, r := range doc.Rules {
			provenance := doc.PackID + "/" + r.RuleID
			for _, m := range metricKindEqualsLiteralRE.FindAllStringSubmatch(r.PredicateDSL, -1) {
				kind := m[1]
				observed[kind] = append(observed[kind], provenance)
			}
			for _, m := range thresholdAtomRE.FindAllStringSubmatch(r.PredicateDSL, -1) {
				uuid := m[1]
				kind, ok := thresholdIndex[uuid]
				if !ok {
					t.Fatalf("rulepack %s rule %s references threshold UUID %s that is not in decoupling.ListCanonicalThresholds(); extend the canonical seed or fix the YAML",
						path, r.RuleID, uuid)
				}
				observed[kind] = append(observed[kind], provenance+" via threshold("+uuid+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", rulepacksDir, err)
	}
	if len(observed) == 0 {
		t.Fatalf("no metric_kind references found under %s -- path-resolution bug or empty rulepack tree", rulepacksDir)
	}
	return observed
}

// rulepackYAML is the subset of the published-rulepack YAML
// shape this test needs. Field names match the YAML keys
// verbatim so a future schema extension (e.g. nested
// `metadata`) does not change the harvest path.
type rulepackYAML struct {
	PackID string `yaml:"pack_id"`
	Rules  []struct {
		RuleID       string `yaml:"rule_id"`
		PredicateDSL string `yaml:"predicate_dsl"`
	} `yaml:"rules"`
}

// collectRecipeMetricKinds parses every non-test .go file
// under `internal/metrics/recipes/**` with `go/ast` and
// harvests every package-level constant whose name ends in
// `MetricKind` and whose value is a string literal. The
// returned map carries the file path so a non-canonical
// reference is traceable to a specific source location.
//
// AST parsing (rather than regex) catches edge cases the
// rubber-duck flagged: a constant declared in a parenthesised
// `const ( ... )` group, a constant with a non-string literal
// (which would slip past `"%s" = "%s"` regex), or a constant
// spread across two lines via line-continuation. The brief
// says "read every reference"; AST is the only way to
// enumerate definitions exhaustively.
func collectRecipeMetricKinds(t *testing.T, root string) map[string][]string {
	t.Helper()
	recipesDir := filepath.Join(root, "internal", "metrics", "recipes")
	observed := make(map[string][]string)

	err := filepath.WalkDir(recipesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		harvestGoStringConsts(t, path, func(name, value string) {
			if !strings.HasSuffix(name, "MetricKind") {
				return
			}
			observed[value] = append(observed[value], path+":"+name)
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", recipesDir, err)
	}
	if len(observed) == 0 {
		t.Fatalf("no *MetricKind constants found under %s -- path-resolution bug or empty recipes tree", recipesDir)
	}
	return observed
}

// collectSystemTierMetricKinds parses
// `internal/aggregator/system_tier.go` and harvests every
// package-level string constant whose name starts with
// `SystemMetric`. The harvested set is asserted to equal
// the package's exported
// `aggregator.CanonicalSystemTierMetricKinds` slice (so a
// future refactor that drops a literal from the slice but
// leaves the constant declared is caught).
func collectSystemTierMetricKinds(t *testing.T, root string) map[string][]string {
	t.Helper()
	systemTierFile := filepath.Join(root, "internal", "aggregator", "system_tier.go")
	observed := make(map[string][]string)
	harvestGoStringConsts(t, systemTierFile, func(name, value string) {
		if !strings.HasPrefix(name, "SystemMetric") {
			return
		}
		// SystemMetricVersion is the int-typed metric_version
		// stamp, not a metric_kind string. We filter by value
		// type via the harvester's string-only contract (only
		// string literals are reported), so a stray int const
		// like `SystemMetricVersion = 1` never reaches here.
		observed[value] = append(observed[value], systemTierFile+":"+name)
	})
	if len(observed) == 0 {
		t.Fatalf("no SystemMetric* string constants found in %s -- path-resolution bug or refactor broke the naming convention", systemTierFile)
	}
	return observed
}

// harvestGoStringConsts parses `path` with `go/ast` and
// invokes `visit(name, value)` once for every package-level
// `const NAME = "literal"` declaration whose value is a
// string [ast.BasicLit]. Used by both the recipes walker
// and the system_tier walker so the parsing logic is
// shared.
func harvestGoStringConsts(t *testing.T, path string, visit func(name, value string)) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unq, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote string const %s in %s: %v", name.Name, path, err)
				}
				visit(name.Name, unq)
			}
		}
	}
}

// TestCanonicalMetricKinds_SourceReferencesAreSubset is the
// headline assertion: every metric_kind literal the v1
// service references in YAML rulepacks, Go recipes, or the
// system_tier composer MUST be a member of the closed
// canonical set (architecture Sec 1.4 + tech-spec Sec 4.1.1).
//
// A non-canonical hit fails with a diagnostic that prints
// the offending literal, the source location(s) it appears
// at, AND the canonical superset -- so the reviewer sees
// immediately whether the fix is to rename the literal or
// to extend the canonical set after a corresponding doc
// edit.
func TestCanonicalMetricKinds_SourceReferencesAreSubset(t *testing.T) {
	root := serviceRoot(t)

	allObserved := make(map[string][]string)
	for k, provs := range collectYAMLMetricKinds(t, root) {
		allObserved[k] = append(allObserved[k], provs...)
	}
	for k, provs := range collectRecipeMetricKinds(t, root) {
		allObserved[k] = append(allObserved[k], provs...)
	}
	for k, provs := range collectSystemTierMetricKinds(t, root) {
		allObserved[k] = append(allObserved[k], provs...)
	}

	var violations []string
	keys := make([]string, 0, len(allObserved))
	for k := range allObserved {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := canonicalMetricKinds[k]; ok {
			continue
		}
		provs := allObserved[k]
		sort.Strings(provs)
		violations = append(violations,
			fmt.Sprintf("  - %q at:\n      %s", k, strings.Join(provs, "\n      ")))
	}
	if len(violations) > 0 {
		canonical := make([]string, 0, len(canonicalMetricKinds))
		for k := range canonicalMetricKinds {
			canonical = append(canonical, k)
		}
		sort.Strings(canonical)
		t.Fatalf(
			"non-canonical metric_kind reference(s) found in source:\n%s\n\n"+
				"Canonical set (architecture Sec 1.4 + tech-spec Sec 4.1.1):\n  %s\n\n"+
				"To fix EITHER rename the literal to a canonical name OR extend the canonical set in this file AFTER a corresponding edit to architecture Sec 1.4 / tech-spec Sec 4.1.1 AND `internal/policy/dsl.CanonicalMetricKinds`.",
			strings.Join(violations, "\n"),
			strings.Join(canonical, ", "),
		)
	}
}

// TestCanonicalMetricKinds_DSLAgrees asserts the runtime
// canon-guard set (`dsl.CanonicalMetricKinds`) equals this
// test's spec-anchored superset element-for-element.
//
// This is the SECOND half of the drift guard: the subset
// assertion above catches a non-canonical name landing in
// SOURCE (a YAML / recipe / system_tier literal). This
// equality assertion catches a non-canonical name landing
// in the runtime parser's closed-set table -- which would
// allow the parser to silently accept a name that does not
// exist in the architecture. The two together pin the
// triple (architecture <-> dsl <-> source).
func TestCanonicalMetricKinds_DSLAgrees(t *testing.T) {
	dslSet := make(map[string]struct{}, len(dsl.ListCanonicalMetricKinds()))
	for _, k := range dsl.ListCanonicalMetricKinds() {
		dslSet[k] = struct{}{}
	}
	var missingFromDSL, missingFromLocal []string
	for k := range canonicalMetricKinds {
		if _, ok := dslSet[k]; !ok {
			missingFromDSL = append(missingFromDSL, k)
		}
	}
	for k := range dslSet {
		if _, ok := canonicalMetricKinds[k]; !ok {
			missingFromLocal = append(missingFromLocal, k)
		}
	}
	sort.Strings(missingFromDSL)
	sort.Strings(missingFromLocal)
	if len(missingFromDSL) == 0 && len(missingFromLocal) == 0 {
		return
	}
	t.Fatalf(
		"dsl.CanonicalMetricKinds and this test's spec-anchored set have drifted:\n"+
			"  Present here, missing in dsl: %v\n"+
			"  Present in dsl, missing here: %v\n\n"+
			"Both lists MUST agree -- the spec-anchored set in this file mirrors architecture Sec 1.4 + tech-spec Sec 4.1.1, and `dsl.CanonicalMetricKinds` is the parser's runtime closed-set guard. A drift means either (a) the architecture was edited without updating the dsl map, or (b) the dsl map was edited without an architecture review. Resolve by editing BOTH this file AND the dsl map to match the architecture rows.",
		missingFromDSL, missingFromLocal,
	)
}

// TestCanonicalSystemTierMetricKinds_SliceMatchesLiterals
// asserts the exported
// `aggregator.CanonicalSystemTierMetricKinds` slice equals
// the set harvested from `system_tier.go`'s SystemMetric*
// string constants. This is the symmetry guard the
// rubber-duck flagged: a future refactor that drops a name
// from the canonical slice but forgets to delete the const
// (or vice versa) is caught here.
func TestCanonicalSystemTierMetricKinds_SliceMatchesLiterals(t *testing.T) {
	root := serviceRoot(t)
	observed := collectSystemTierMetricKinds(t, root)
	exported := make(map[string]struct{}, len(aggregator.CanonicalSystemTierMetricKinds))
	for _, k := range aggregator.CanonicalSystemTierMetricKinds {
		exported[k] = struct{}{}
	}
	for k := range observed {
		if _, ok := exported[k]; !ok {
			t.Errorf("SystemMetric* literal %q in system_tier.go is NOT a member of aggregator.CanonicalSystemTierMetricKinds; either add it to the canonical slice or remove the stray const", k)
		}
	}
	for k := range exported {
		if _, ok := observed[k]; !ok {
			t.Errorf("aggregator.CanonicalSystemTierMetricKinds lists %q but no SystemMetric* const in system_tier.go declares it; either add the const or shrink the canonical slice", k)
		}
	}
}
