// -----------------------------------------------------------------------
// <copyright file="markdown_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package report_test

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// TestMarkdown_SatisfiesRenderer is a compile-time + runtime
// assertion that *report.Markdown is assignable to the
// architecture Sec 5.7 [report.Renderer] surface. The
// compile-time half is the var-decl below; the runtime half
// invokes Render against a representative artifact so a
// silently-broken Render signature (wrong receiver shape,
// wrong arg names) cannot land green.
func TestMarkdown_SatisfiesRenderer(t *testing.T) {
	var r report.Renderer = report.Markdown{}
	var buf bytes.Buffer
	if err := r.Render(context.Background(), report.RunArtifact{}, &buf); err != nil {
		t.Fatalf("Render(zero artifact) returned %v; want nil", err)
	}
	if buf.Len() == 0 {
		t.Fatalf("Render(zero artifact) wrote 0 bytes; want a header + verdict block")
	}
}

// TestMarkdown_HeaderEchoesArchitectureRows asserts the
// architecture Sec 3.7.1 step 1 rows -- repo path, head SHA,
// policy id + version, active parser fleet, dark-metric
// summary count -- all appear in the rendered output for a
// populated artifact, in the architecture order, with the
// architecture-mandated labels.
func TestMarkdown_HeaderEchoesArchitectureRows(t *testing.T) {
	policyID := uuid.Must(uuid.NewV4())
	art := report.RunArtifact{
		Context: repocontext.RepoContext{
			RootPath: "/repos/example",
			HeadSHA:  "deadbeef",
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: policyID,
			Name:            "cleanc-dev-policy",
			RefactorWeights: steward.RefactorWeights{
				EffortModelVersion: "fallback-2026.05",
			},
		},
		DarkMetrics: []orchestrator.DarkMetric{
			{MetricKind: "cyclo", Language: "go"},
			{MetricKind: "lcom4", Language: "java"},
		},
		Verdict: rule_engine.EvaluationVerdict{Verdict: rule_engine.VerdictWarn},
	}

	got := renderToString(t, report.Markdown{}, art)

	wantSubstrings := []string{
		"# Clean-code report",
		"- **Repo path:** /repos/example",
		"- **Head SHA:** deadbeef",
		"- **Policy:** policy_id=" + policyID.String() + " version=fallback-2026.05",
		"- **Active parser fleet:** " + strings.Join(parser.DefaultRegistry().Languages(), ", "),
		"- **Dark metrics:** 2",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Errorf("Render output missing %q\n---\n%s\n---", want, got)
		}
	}

	// Architecture ORDER: repo path < head sha < policy <
	// active parser fleet < dark metrics. A future refactor
	// that reshuffles the bullets must coordinate with
	// architecture Sec 3.7.1.
	order := []string{
		"Repo path:", "Head SHA:", "Policy:",
		"Active parser fleet:", "Dark metrics:",
	}
	last := -1
	for _, key := range order {
		idx := strings.Index(got, key)
		if idx < 0 {
			t.Fatalf("header row %q not present in output:\n%s", key, got)
		}
		if idx < last {
			t.Errorf("header row %q out of architecture-pinned order (idx=%d, last=%d)\n%s",
				key, idx, last, got)
		}
		last = idx
	}
}

// TestMarkdown_VerdictBlockStrictlyEchoesArtifact asserts the
// verdict line is a STRICT echo of `RunArtifact.Verdict.Verdict`
// for any input -- canonical or not -- per the iter-1 review's
// "the renderer must not rewrite to unknown" finding. The
// closed canonical set is enforced by the engine, NOT by the
// renderer; a non-canonical value reaching the report is an
// engine bug the operator must see verbatim.
func TestMarkdown_VerdictBlockStrictlyEchoesArtifact(t *testing.T) {
	cases := []struct {
		name string
		v    rule_engine.Verdict
		want string
	}{
		{"pass", rule_engine.VerdictPass, "Verdict: pass"},
		{"warn", rule_engine.VerdictWarn, "Verdict: warn"},
		{"block", rule_engine.VerdictBlock, "Verdict: block"},
		// Non-canonical input: renderer MUST echo verbatim,
		// not rewrite to "unknown". This pins the iter-1 fix.
		{"non-canonical-echoed-verbatim", rule_engine.Verdict("fail"), "Verdict: fail"},
		// Empty: renderer MUST emit the bare "Verdict: " line
		// with no substitution.
		{"empty-echoed-verbatim", rule_engine.Verdict(""), "Verdict: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			art := report.RunArtifact{
				Verdict: rule_engine.EvaluationVerdict{Verdict: tc.v},
			}
			got := renderToString(t, report.Markdown{}, art)
			if !strings.Contains(got, tc.want) {
				t.Errorf("Render did not emit %q; output:\n%s", tc.want, got)
			}
			// The renderer MUST NOT substitute a placeholder
			// like "unknown" for any input -- pins the iter-2
			// structural fix against the iter-1 rewrite.
			if strings.Contains(got, "Verdict: unknown") && tc.v != rule_engine.Verdict("unknown") {
				t.Errorf("Render substituted 'unknown' for verdict %q; want strict echo:\n%s",
					string(tc.v), got)
			}
		})
	}
}

// TestMarkdown_DarkMetricsCount_ZeroEmitsZero asserts the
// header's dark-metric summary count cell echoes the exact
// `len(art.DarkMetrics)` value, including zero. Architecture
// Sec 3.7.1 step 1 mandates the count appear; a renderer
// that omitted the row for zero would defeat the
// "operator always sees the dark-metric posture" contract.
func TestMarkdown_DarkMetricsCount_ZeroEmitsZero(t *testing.T) {
	got := renderToString(t, report.Markdown{}, report.RunArtifact{})
	if !strings.Contains(got, "- **Dark metrics:** 0") {
		t.Errorf("Render of zero-dark-metric artifact did not emit %q; output:\n%s",
			"- **Dark metrics:** 0", got)
	}
}

// TestMarkdown_ActiveParserFleet_UsesDefaultRegistry asserts
// the active-parser-fleet row is sourced strictly from
// [parser.DefaultRegistry] -- the architecture Sec 3.7.1
// step 1 contract -- and includes every v1-pinned language.
// The renderer exposes NO override surface for this row (the
// iter-1 evaluator flagged the prior `Markdown.Parsers` field
// as a contract leak); the registry is the single source.
func TestMarkdown_ActiveParserFleet_UsesDefaultRegistry(t *testing.T) {
	got := renderToString(t, report.Markdown{}, report.RunArtifact{})
	for _, lang := range parser.DefaultRegistry().Languages() {
		if !strings.Contains(got, lang) {
			t.Errorf("Render did not list default-registry language %q; output:\n%s", lang, got)
		}
	}
}

// TestMarkdown_StructIsEmpty pins the iter-2 structural fix:
// the workstream brief calls for `type Markdown struct{}`
// verbatim, with no per-instance overrides. The iter-1
// `Parsers` field allowed callers to render a non-canonical
// parser fleet; removing it restores the architecture
// contract. This reflect-based check refuses to compile a
// future field addition without a coordinated brief update.
func TestMarkdown_StructIsEmpty(t *testing.T) {
	rt := reflect.TypeOf(report.Markdown{})
	if rt.NumField() != 0 {
		t.Errorf("report.Markdown has %d field(s); the workstream brief pins `type Markdown struct{}` (no fields). Fields: %s",
			rt.NumField(), fieldNames(rt))
	}
}

func fieldNames(rt reflect.Type) []string {
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		out = append(out, rt.Field(i).Name)
	}
	return out
}

// TestMarkdown_RejectsNilWriter asserts the renderer surfaces
// a clear error for a nil writer rather than panicking on the
// downstream bufio.NewWriter call. Composition-root callers
// pass the user-supplied `--out <path>` file handle directly
// here; a nil-writer panic would mask the operator's typo as
// a stack trace.
func TestMarkdown_RejectsNilWriter(t *testing.T) {
	if err := (report.Markdown{}).Render(context.Background(), report.RunArtifact{}, nil); err == nil {
		t.Fatal("Render(..., nil writer) returned nil error; want non-nil")
	}
}

// renderToString invokes m.Render(ctx, art, &buf) and returns
// the resulting string, failing the test on any render error.
// Centralised so each test reads as "set up artifact -> assert
// rendered substring" without re-doing the buffer dance.
func renderToString(t *testing.T, m report.Markdown, art report.RunArtifact) string {
	t.Helper()
	var buf bytes.Buffer
	if err := m.Render(context.Background(), art, &buf); err != nil {
		t.Fatalf("Render returned %v; want nil", err)
	}
	return buf.String()
}
