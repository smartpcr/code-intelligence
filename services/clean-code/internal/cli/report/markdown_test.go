// -----------------------------------------------------------------------
// <copyright file="markdown_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package report_test

import (
	"bytes"
	"context"
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
			Name:            "solid+decoupling v1",
		},
		DarkMetrics: []orchestrator.DarkMetric{
			{MetricKind: "cyclo", Language: "go"},
			{MetricKind: "lcom4", Language: "java"},
		},
		Verdict: rule_engine.EvaluationVerdict{Verdict: rule_engine.VerdictWarn},
	}

	got := renderToString(t, report.Markdown{Parsers: parser.DefaultRegistry()}, art)

	wantSubstrings := []string{
		"# Clean-code report",
		"- **Repo path:** /repos/example",
		"- **Head SHA:** deadbeef",
		"- **Policy:** " + policyID.String() + " name=solid+decoupling v1",
		"- **Active parser fleet:** go, java, python, typescript",
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

// TestMarkdown_VerdictBlockEchoesEngineVerdict asserts the
// single-line verdict block per architecture Sec 3.7.1 step 2
// echoes `RunArtifact.Verdict.Verdict` verbatim for each
// canonical label.
func TestMarkdown_VerdictBlockEchoesEngineVerdict(t *testing.T) {
	cases := []struct {
		name string
		v    rule_engine.Verdict
		want string
	}{
		{"pass", rule_engine.VerdictPass, "Verdict: pass"},
		{"warn", rule_engine.VerdictWarn, "Verdict: warn"},
		{"block", rule_engine.VerdictBlock, "Verdict: block"},
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
		})
	}
}

// TestMarkdown_VerdictBlockHandlesUnstampedVerdict asserts
// the renderer never drops the Verdict line when the engine
// has not stamped a canonical verdict yet (the zero value).
// The architecture pins the closed set `{pass, warn, block}`
// (Sec 5.4.3); the renderer is the operator's only window
// into a degraded short-circuit, so an empty Verdict line
// would silently elide that signal.
func TestMarkdown_VerdictBlockHandlesUnstampedVerdict(t *testing.T) {
	got := renderToString(t, report.Markdown{}, report.RunArtifact{})
	if !strings.Contains(got, "Verdict: unknown") {
		t.Errorf("Render of zero-verdict artifact did not emit %q; output:\n%s",
			"Verdict: unknown", got)
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
// the active-parser-fleet row defaults to
// [parser.DefaultRegistry] when [Markdown.Parsers] is nil --
// the architecture Sec 3.7.1 step 1 contract -- and includes
// every v1-pinned language.
func TestMarkdown_ActiveParserFleet_UsesDefaultRegistry(t *testing.T) {
	got := renderToString(t, report.Markdown{}, report.RunArtifact{})
	for _, lang := range parser.DefaultRegistry().Languages() {
		if !strings.Contains(got, lang) {
			t.Errorf("Render did not list default-registry language %q; output:\n%s", lang, got)
		}
	}
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
