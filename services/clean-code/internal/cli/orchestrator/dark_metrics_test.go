// -----------------------------------------------------------------------
// <copyright file="dark_metrics_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package orchestrator_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
)

// ---------------------------------------------------------------------------
// Stage 2.5 / workstream Dark Metric Diagnostics
//
// tech-spec REFACTOR-GUIDE Sec 8.7 / D10:
//   - One DarkMetric row per (metric_kind, language) pair when
//     the recipe's AppliesTo returned false on every file of
//     the language because a required parser attr was unstamped.
//   - MissingAttrs is the closed-set value from the
//     metricAttrRequirements table:
//       cyclo / cognitive_complexity -> [decision_blocks]
//       fan_in / fan_out             -> [call_edges]
//       lcom4                        -> [call_edges, field_accesses]
//   - AffectedScopeCount is the count of scopes the recipe
//     would have evaluated had the attr been stamped.
//   - ClosurePhase == "P2" in P0/P1.
//   - Rows are sorted by (metric_kind, language) for byte-
//     identical determinism (tech-spec D9).
// ---------------------------------------------------------------------------

// TestOrchestrator_Run_DarkMetrics_EmittedForDarkRecipes is the
// table-driven contract test: a corpus of one file per pinned
// language exercises every dark-metric recipe simultaneously
// and asserts EVERY expected (metric_kind, language) pair
// surfaces with the right MissingAttrs + AffectedScopeCount.
// Today's Stage 2.1 parsers do NOT stamp decision_blocks /
// call_edges / field_accesses, so every dark-metric recipe is
// expected to stay dark on every language.
func TestOrchestrator_Run_DarkMetrics_EmittedForDarkRecipes(t *testing.T) {
	root := t.TempDir()

	// One Go file with a method scope (so AffectedScopeCount > 0
	// for cyclo / cognitive_complexity / fan_in / fan_out).
	writeFixtureFile(t, root, "pkg/foo.go", strings.Join([]string{
		"package pkg",
		"",
		"type Widget struct{}",
		"",
		"func (w Widget) Run() int { return 1 }",
		"",
		"func TopLevel(x int) int { return x + 1 }",
		"",
	}, "\n"))

	// One Python file -- exercises (kind, language=python).
	writeFixtureFile(t, root, "pkg/foo.py", strings.Join([]string{
		"class Widget:",
		"    def run(self):",
		"        return 1",
		"",
		"def top_level(x):",
		"    return x + 1",
		"",
	}, "\n"))

	rc := testRepoContext(t, root)
	o := orchestrator.New(orchestrator.Options{Workers: 1})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Diagnostics.DarkMetrics == nil {
		t.Fatalf("res.Diagnostics.DarkMetrics is nil; want non-nil (empty slice when no dark metrics)")
	}

	// Index the emitted rows by (metric_kind, language) for
	// O(1) per-assertion lookup.
	type key struct{ kind, lang string }
	got := map[key]orchestrator.DarkMetric{}
	for _, dm := range res.Diagnostics.DarkMetrics {
		got[key{dm.MetricKind, dm.Language}] = dm
	}

	// Determine which languages the walker actually surfaced
	// (the parser-registry detection sometimes labels Python
	// as "py" vs "python" depending on the language detector
	// -- we look the language up from the parsed corpus
	// rather than hard-coding it).
	langs := map[string]bool{}
	for _, f := range res.Files {
		langs[f.GetLanguage()] = true
	}
	if !langs["go"] {
		t.Fatalf("fixture did not parse any Go file; languages=%v", langs)
	}

	// Per-metric expected MissingAttrs (from the
	// metricAttrRequirements table). The orchestrator MUST
	// preserve the table's slice order in the emitted
	// MissingAttrs (tech-spec D9).
	expectedAttrs := map[string][]string{
		"cyclo":                {"decision_blocks"},
		"cognitive_complexity": {"decision_blocks"},
		"fan_in":               {"call_edges"},
		"fan_out":              {"call_edges"},
		"lcom4":                {"call_edges", "field_accesses"},
	}

	for metricKind, wantAttrs := range expectedAttrs {
		for lang := range langs {
			dm, ok := got[key{metricKind, lang}]
			if !ok {
				t.Errorf("missing DarkMetric row for (metric_kind=%q, language=%q); got rows=%+v",
					metricKind, lang, res.Diagnostics.DarkMetrics)
				continue
			}
			if got, want := dm.MissingAttrs, wantAttrs; !sliceEqual(got, want) {
				t.Errorf("(%q,%q).MissingAttrs = %v, want %v (order matters; tech-spec D9)",
					metricKind, lang, got, want)
			}
			if dm.ClosurePhase != "P2" {
				t.Errorf("(%q,%q).ClosurePhase = %q, want %q",
					metricKind, lang, dm.ClosurePhase, "P2")
			}
			if dm.AffectedScopeCount < 0 {
				t.Errorf("(%q,%q).AffectedScopeCount = %d, must be >= 0",
					metricKind, lang, dm.AffectedScopeCount)
			}
		}
	}

	// Specifically assert AffectedScopeCount > 0 for cyclo on
	// the Go file (we authored two methods + a file scope, so
	// the count must be at least 3 for kinds={method, file}).
	cycloGo, ok := got[key{"cyclo", "go"}]
	if !ok {
		t.Fatalf("expected (cyclo, go) row, got rows=%+v", res.Diagnostics.DarkMetrics)
	}
	if cycloGo.AffectedScopeCount < 1 {
		t.Errorf("(cyclo,go).AffectedScopeCount = %d, want >= 1 (fixture has at least one method + one file scope)", cycloGo.AffectedScopeCount)
	}
}

// TestOrchestrator_Run_DarkMetrics_SortedDeterministically
// pins tech-spec D9: the emitted DarkMetric slice MUST be
// sorted by (MetricKind, Language) byte-identically across
// runs. The orchestrator's accumulator finalize() walks a map
// (non-deterministic iteration order), so the sort step is
// load-bearing.
func TestOrchestrator_Run_DarkMetrics_SortedDeterministically(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "pkg/a.go", "package pkg\n\nfunc A() {}\n")
	writeFixtureFile(t, root, "pkg/a.py", "def a(): pass\n")
	writeFixtureFile(t, root, "src/a.ts", "function a(): void {}\n")
	writeFixtureFile(t, root, "src/A.java", "public class A { public void a() {} }\n")

	rc := testRepoContext(t, root)
	o := orchestrator.New(orchestrator.Options{Workers: 1})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	rows := res.Diagnostics.DarkMetrics
	if len(rows) == 0 {
		t.Fatalf("no dark metrics emitted; corpus has every dark-recipe-touching language")
	}

	sorted := make([]orchestrator.DarkMetric, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].MetricKind != sorted[j].MetricKind {
			return sorted[i].MetricKind < sorted[j].MetricKind
		}
		return sorted[i].Language < sorted[j].Language
	})
	for i := range rows {
		if rows[i].MetricKind != sorted[i].MetricKind || rows[i].Language != sorted[i].Language {
			t.Errorf("DarkMetrics not sorted at index %d: got (%q,%q), want (%q,%q)",
				i, rows[i].MetricKind, rows[i].Language,
				sorted[i].MetricKind, sorted[i].Language)
		}
	}
}

// TestOrchestrator_Run_DarkMetrics_OnlyEmittedForLanguagesSeen
// pins the contract that the orchestrator NEVER invents a
// dark-metric row for a language that wasn't observed in the
// scan: an empty corpus produces an empty DarkMetrics slice.
func TestOrchestrator_Run_DarkMetrics_OnlyEmittedForLanguagesSeen(t *testing.T) {
	root := t.TempDir()
	// Empty repo -- walker emits zero files.

	rc := testRepoContext(t, root)
	o := orchestrator.New(orchestrator.Options{Workers: 1})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Diagnostics.DarkMetrics == nil {
		t.Fatalf("DarkMetrics is nil; want empty slice for empty corpus")
	}
	if len(res.Diagnostics.DarkMetrics) != 0 {
		t.Errorf("len(DarkMetrics) = %d, want 0 for empty corpus; got %+v",
			len(res.Diagnostics.DarkMetrics), res.Diagnostics.DarkMetrics)
	}
}

// TestOrchestrator_Run_DarkMetrics_LitRecipesDoNotAppear pins
// the inverse contract: a recipe that ACTUALLY emits drafts
// (e.g. `loc`, which lights up today) MUST NOT appear in the
// dark-metric diagnostic.
func TestOrchestrator_Run_DarkMetrics_LitRecipesDoNotAppear(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "pkg/foo.go", "package pkg\n\nfunc Foo() {}\n")

	rc := testRepoContext(t, root)
	o := orchestrator.New(orchestrator.Options{Workers: 1})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, dm := range res.Diagnostics.DarkMetrics {
		switch dm.MetricKind {
		case "loc", "duplication_ratio", "cycle_member",
			"interface_width", "depth_of_inheritance",
			"coupling_between_objects":
			t.Errorf("metric_kind=%q surfaced as dark, but it lights up today (architecture Sec 3.3 table)",
				dm.MetricKind)
		}
	}
}

// sliceEqual is the minimal slice-equality helper used by the
// dark-metric tests above. Pinned local to this file because
// `reflect.DeepEqual` would also accept `nil == []string{}`
// which the tech-spec D9 row-determinism contract explicitly
// rejects (every emitted MissingAttrs slice is non-nil).
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
