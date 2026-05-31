// -----------------------------------------------------------------------
// <copyright file="json_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package report_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// TestJSON_SatisfiesRenderer is the compile-time + runtime
// assertion that *report.JSON is assignable to the
// architecture Sec 5.7 [report.Renderer] surface.
func TestJSON_SatisfiesRenderer(t *testing.T) {
	var r report.Renderer = report.JSON{}
	var buf bytes.Buffer
	if err := r.Render(context.Background(), report.RunArtifact{}, &buf); err != nil {
		t.Fatalf("Render(zero artifact) returned %v; want nil", err)
	}
	if buf.Len() == 0 {
		t.Fatalf("Render(zero artifact) wrote 0 bytes; want a JSON document")
	}
}

// TestJSON_StructIsEmpty pins the workstream brief contract:
// `type JSON struct{}` -- no fields, no constructor knobs.
// A future field addition signals contract drift and must
// be paired with a brief amendment.
func TestJSON_StructIsEmpty(t *testing.T) {
	rt := reflect.TypeOf(report.JSON{})
	if rt.NumField() != 0 {
		t.Errorf("report.JSON has %d field(s); the workstream brief pins `type JSON struct{}` (no fields).",
			rt.NumField())
	}
}

// TestJSON_RoundTripsRunArtifact is the central one-for-one
// contract: a fully-populated RunArtifact survives a
// Render -> json.Unmarshal round trip into the same
// RunArtifact type without field loss. Pins the brief's
// "do NOT collapse fields or add new ones so downstream
// consumers can json.Unmarshal into the same struct" rule.
func TestJSON_RoundTripsRunArtifact(t *testing.T) {
	policyID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	runID := uuid.Must(uuid.NewV4())
	verdictID := uuid.Must(uuid.NewV4())
	findingID := uuid.Must(uuid.NewV4())

	want := report.RunArtifact{
		Context: repocontext.RepoContext{
			RootPath:   "/repos/example",
			RepoID:     repoID,
			HeadSHA:    "deadbeef",
			ModulePath: "example.com/repo",
			IsGitRepo:  true,
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: policyID,
			Name:            "cleanc-dev-policy",
			RefactorWeights: steward.RefactorWeights{
				EffortModelVersion: "fallback-2026.05",
			},
		},
		Files: []report.WalkedFileSummary{
			{Path: "a.go", Language: "go", SizeBytes: 42, ParseStatus: "parsed"},
		},
		DarkMetrics: []orchestrator.DarkMetric{
			{MetricKind: "cyclo", Language: "go"},
		},
		Run: rule_engine.EvaluationRun{
			EvaluationRunID: runID,
			RepoID:          repoID,
			SHA:             "deadbeef",
			PolicyVersionID: policyID,
		},
		Verdict: rule_engine.EvaluationVerdict{
			VerdictID:       verdictID,
			EvaluationRunID: runID,
			Verdict:         rule_engine.VerdictWarn,
		},
		Findings: []rule_engine.Finding{
			{
				FindingID:       findingID,
				EvaluationRunID: runID,
				RepoID:          repoID,
				RuleID:          "solid.srp.lcom4",
				RuleVersion:     1,
				PolicyVersionID: policyID,
				ExplanationMD:   "Suggested refactor: split <Class> & extract.",
			},
		},
	}

	var buf bytes.Buffer
	if err := (report.JSON{}).Render(context.Background(), want, &buf); err != nil {
		t.Fatalf("Render returned %v; want nil", err)
	}

	var got report.RunArtifact
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal of rendered bytes failed: %v\n---\n%s\n---", err, buf.String())
	}

	if !reflect.DeepEqual(want, got) {
		t.Errorf("round-trip mismatch:\nwant: %#v\n got: %#v", want, got)
	}
}

// TestJSON_UsesTwoSpaceIndent pins the encoding contract: the
// brief says SetIndent("", "  ") so the artifact stays
// diff-friendly in PR review. A change to the indent shape
// shifts every byte of every existing fixture; this catches
// the regression on the first failing test.
func TestJSON_UsesTwoSpaceIndent(t *testing.T) {
	art := report.RunArtifact{
		Context: repocontext.RepoContext{RootPath: "/x"},
	}
	var buf bytes.Buffer
	if err := (report.JSON{}).Render(context.Background(), art, &buf); err != nil {
		t.Fatalf("Render returned %v", err)
	}
	out := buf.String()
	// Top-level keys must sit at EXACTLY two spaces of
	// indent. A regression to tab indent or to a different
	// step size shifts every byte of every existing fixture.
	if !strings.HasPrefix(out, "{\n  \"") {
		end := len(out)
		if end > 40 {
			end = 40
		}
		t.Errorf("expected output to begin with `{\\n  \"`; got first 40 bytes %q", out[:end])
	}
	// Tabs must never appear -- json.Encoder.SetIndent("", "  ")
	// uses spaces; a tab anywhere in the output signals the
	// indent argument drifted.
	if strings.Contains(out, "\t") {
		t.Errorf("output contains a tab; SetIndent must use spaces only:\n%s", out)
	}
}

// TestJSON_DoesNotEscapeHTML pins SetEscapeHTML(false). With
// the default `encoding/json` behaviour, characters `<`, `>`,
// `&` would escape to `\u003c`, `\u003e`, `\u0026` and the
// Markdown snippets in Rule.DescriptionMD / Finding.ExplanationMD
// would become unreadable in raw text diffs.
func TestJSON_DoesNotEscapeHTML(t *testing.T) {
	art := report.RunArtifact{
		Findings: []rule_engine.Finding{
			{ExplanationMD: "split <Class> & extract"},
		},
	}
	var buf bytes.Buffer
	if err := (report.JSON{}).Render(context.Background(), art, &buf); err != nil {
		t.Fatalf("Render returned %v", err)
	}
	out := buf.String()
	for _, raw := range []string{"<Class>", "&"} {
		if !strings.Contains(out, raw) {
			t.Errorf("expected raw %q in output; got escaped form. output:\n%s", raw, out)
		}
	}
	for _, esc := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(out, esc) {
			t.Errorf("output contains HTML-escaped sequence %q; want raw. output:\n%s", esc, out)
		}
	}
}

// TestJSON_UUIDsAreCanonicalRFC4122 asserts that every UUID
// in the rendered document is the canonical RFC 4122
// lowercase hex-dashed string (gofrs/uuid's MarshalText
// path). A regression that swapped MarshalText for a byte
// array form would emit a 16-element JSON array per UUID
// and break every downstream consumer.
func TestJSON_UUIDsAreCanonicalRFC4122(t *testing.T) {
	policyID := uuid.Must(uuid.NewV4())
	art := report.RunArtifact{
		Policy: steward.PolicyVersion{PolicyVersionID: policyID},
	}
	var buf bytes.Buffer
	if err := (report.JSON{}).Render(context.Background(), art, &buf); err != nil {
		t.Fatalf("Render returned %v", err)
	}
	wantQuoted := `"` + policyID.String() + `"`
	if !strings.Contains(buf.String(), wantQuoted) {
		t.Errorf("expected canonical RFC 4122 UUID %s in output; got:\n%s",
			wantQuoted, buf.String())
	}
}

// TestJSON_TerminatedByNewline pins json.Encoder.Encode's
// documented trailing-newline contract -- keeps the artifact
// POSIX-compliant (terminated by a newline) so editors and
// `diff` do not flag a "no newline at end of file" marker.
func TestJSON_TerminatedByNewline(t *testing.T) {
	var buf bytes.Buffer
	if err := (report.JSON{}).Render(context.Background(), report.RunArtifact{}, &buf); err != nil {
		t.Fatalf("Render returned %v", err)
	}
	out := buf.Bytes()
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Errorf("expected output to end with a newline; got last byte %q (full: %q)",
			out[len(out)-1], string(out))
	}
}

// TestJSON_HonoursContextCancellation asserts a cancelled
// ctx short-circuits before any bytes are written so a
// downstream consumer either sees the full document or no
// document at all (no partial / corrupt JSON file).
func TestJSON_HonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	err := (report.JSON{}).Render(ctx, report.RunArtifact{}, &buf)
	if err == nil {
		t.Fatal("Render(cancelled ctx, ...) returned nil error; want non-nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected error to wrap context.Canceled; got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no bytes written on cancelled ctx; wrote %d bytes: %q",
			buf.Len(), buf.String())
	}
}

// TestJSON_WriterErrorSurfaced asserts a writer that fails
// surfaces the underlying error verbatim (wrapped) so the
// composition root's `--findings <path>` flag surface can
// report the offending path. Pins the iter-1 markdown
// renderer's "surface I/O failures, do not swallow them"
// contract on the JSON sidecar too.
func TestJSON_WriterErrorSurfaced(t *testing.T) {
	w := &failingWriter{err: errors.New("disk full")}
	err := (report.JSON{}).Render(context.Background(), report.RunArtifact{}, w)
	if err == nil {
		t.Fatal("Render returned nil error on a failing writer; want non-nil")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("expected underlying error to surface; got %v", err)
	}
}

type failingWriter struct{ err error }

func (f *failingWriter) Write(p []byte) (int, error) { return 0, f.err }
