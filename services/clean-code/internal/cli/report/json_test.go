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
		SchemaVersion: report.SchemaVersionCurrent,
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

// TestJSON_RenderFromBytes_RoundTrip verifies the
// Stage 3.4 happy path: a [RunArtifact] rendered by
// [JSON.Render] and then fed through
// [JSON.RenderFromBytes] produces a non-empty markdown
// document that mentions the artifact's RootPath +
// Verdict tokens (proof that the unmarshal->markdown
// pipeline executed end-to-end).
func TestJSON_RenderFromBytes_RoundTrip(t *testing.T) {
	art := report.RunArtifact{
		SchemaVersion: report.SchemaVersionCurrent,
		Context:       repocontext.RepoContext{RootPath: "/repos/example"},
		Verdict:       rule_engine.EvaluationVerdict{Verdict: rule_engine.VerdictPass},
	}
	var jsonBuf bytes.Buffer
	if err := (report.JSON{}).Render(context.Background(), art, &jsonBuf); err != nil {
		t.Fatalf("Render returned %v", err)
	}
	var mdBuf bytes.Buffer
	if err := (report.JSON{}).RenderFromBytes(jsonBuf.Bytes(), &mdBuf); err != nil {
		t.Fatalf("RenderFromBytes returned %v", err)
	}
	md := mdBuf.String()
	for _, want := range []string{"/repos/example", "Verdict:"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, md)
		}
	}
}

// TestJSON_RenderFromBytes_SchemaMismatch pins the
// Stage 3.4 refusal contract: a findings.json whose
// schemaVersion does not match [SchemaVersionCurrent]
// returns a typed [*SchemaVersionMismatchError] carrying
// BOTH versions verbatim. The composition root in
// `cmd/cleanc/main.go` uses [errors.As] on this type to
// map to [flags.ExitUsage] (64) with a clear stderr line.
func TestJSON_RenderFromBytes_SchemaMismatch(t *testing.T) {
	const staleVersion = "v0.0.0"
	body := []byte(`{"schemaVersion":"` + staleVersion + `","Context":{"RootPath":"/x"}}`)

	var buf bytes.Buffer
	err := (report.JSON{}).RenderFromBytes(body, &buf)
	if err == nil {
		t.Fatal("RenderFromBytes(stale schema) returned nil error; want non-nil")
	}
	var smErr *report.SchemaVersionMismatchError
	if !errors.As(err, &smErr) {
		t.Fatalf("expected error to wrap *SchemaVersionMismatchError; got %T: %v",
			err, err)
	}
	if smErr.Got != staleVersion {
		t.Errorf("Got = %q, want %q", smErr.Got, staleVersion)
	}
	if smErr.Want != report.SchemaVersionCurrent {
		t.Errorf("Want = %q, want %q", smErr.Want, report.SchemaVersionCurrent)
	}
	// Defensive: no bytes written when refusal fires.
	if buf.Len() != 0 {
		t.Errorf("expected no bytes written on schema mismatch; wrote %d bytes",
			buf.Len())
	}
	// The error string itself must mention both versions
	// so log lines / crash dumps surface the skew even if
	// the dispatcher's stderr line is lost.
	for _, want := range []string{staleVersion, report.SchemaVersionCurrent} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error string missing %q: %v", want, err)
		}
	}
}

// TestJSON_RenderFromBytes_EmptySchemaIsMismatch asserts
// that a findings.json missing the `schemaVersion` tag
// entirely is treated as a mismatch (Got == "") -- the
// [JSON.Render] path auto-stamps the current version on
// every emitted document, so a missing tag necessarily
// means the bytes did not come from any released cleanc
// build.
func TestJSON_RenderFromBytes_EmptySchemaIsMismatch(t *testing.T) {
	var buf bytes.Buffer
	err := (report.JSON{}).RenderFromBytes([]byte(`{"Context":{"RootPath":"/x"}}`), &buf)
	var smErr *report.SchemaVersionMismatchError
	if !errors.As(err, &smErr) {
		t.Fatalf("expected *SchemaVersionMismatchError; got %T: %v", err, err)
	}
	if smErr.Got != "" {
		t.Errorf("Got = %q, want empty string", smErr.Got)
	}
}

// TestJSON_RenderFromBytes_InvalidJSON asserts a parse
// failure is surfaced verbatim (wrapped) so the
// composition root's stderr line points at the offending
// path rather than a generic "render failed".
func TestJSON_RenderFromBytes_InvalidJSON(t *testing.T) {
	var buf bytes.Buffer
	err := (report.JSON{}).RenderFromBytes([]byte("{not json"), &buf)
	if err == nil {
		t.Fatal("RenderFromBytes(garbage) returned nil error; want non-nil")
	}
	if !strings.Contains(err.Error(), "unmarshal previous findings") {
		t.Errorf("expected error to mention unmarshal step; got %v", err)
	}
}

// TestJSON_RenderFromBytes_RejectsNilWriter pins the
// nil-writer guard parity with [JSON.Render] -- a CLI
// caller that mis-wires a nil writer gets a clear error
// rather than a panic from the markdown renderer.
func TestJSON_RenderFromBytes_RejectsNilWriter(t *testing.T) {
	err := (report.JSON{}).RenderFromBytes([]byte(`{}`), nil)
	if err == nil {
		t.Fatal("RenderFromBytes(..., nil writer) returned nil error; want non-nil")
	}
	if !strings.Contains(err.Error(), "non-nil writer") {
		t.Errorf("expected error to mention `non-nil writer`; got %v", err)
	}
}

type failingWriter struct{ err error }

func (f *failingWriter) Write(p []byte) (int, error) { return 0, f.err }


// TestJSON_RejectsNilWriter pins the iter-1 evaluator's
// hardening ask: the renderer MUST surface a clear error for
// a nil writer rather than panicking inside
// json.NewEncoder(nil).Encode. Composition-root callers pass
// the user-supplied `--findings <path>` file handle directly
// here; a nil-writer panic would mask the operator's typo as
// a stack trace. Mirrors Markdown.Render's nil-writer
// contract (markdown_test.go TestMarkdown_RejectsNilWriter).
func TestJSON_RejectsNilWriter(t *testing.T) {
	err := (report.JSON{}).Render(context.Background(), report.RunArtifact{}, nil)
	if err == nil {
		t.Fatal("Render(..., nil writer) returned nil error; want non-nil")
	}
	if !strings.Contains(err.Error(), "non-nil writer") {
		t.Errorf("expected error to mention `non-nil writer`; got %v", err)
	}
}

// TestJSON_NilWriterCheckPrecedesEncode is a defence-in-depth
// assertion: when BOTH the writer is nil AND the context is
// cancelled, the nil-writer guard MUST fire first so the
// operator's most-actionable diagnostic (their CLI flag points
// to nowhere) is the one they see -- not a cancellation wrap
// that hides the underlying configuration error.
func TestJSON_NilWriterCheckPrecedesEncode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (report.JSON{}).Render(ctx, report.RunArtifact{}, nil)
	if err == nil {
		t.Fatal("Render(cancelled ctx, nil writer) returned nil error; want non-nil")
	}
	if !strings.Contains(err.Error(), "non-nil writer") {
		t.Errorf("expected nil-writer error to take precedence over ctx cancellation; got %v", err)
	}
}
