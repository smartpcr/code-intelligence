// -----------------------------------------------------------------------
// <copyright file="json.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package report

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// JSON is the [Renderer] implementation that produces the
// machine-readable findings sidecar (architecture Sec 3.7.2)
// written by `cleanc analyze --findings <path>`.
//
// The struct intentionally has NO fields: the workstream
// brief pins `type JSON struct{}` verbatim and the JSON
// schema is the in-memory [RunArtifact] shape one-for-one --
// there is no per-field translation step, no struct tagging,
// no aggregation. Downstream consumers (CI, dashboards,
// `cleanc report <findings.json>` re-render path) can
// [json.Unmarshal] the emitted document into the same
// [RunArtifact] type without a parallel schema definition.
//
// # Encoding contract (architecture Sec 3.7.2 + brief)
//
//   - Indentation: two spaces, no prefix
//     ([json.Encoder.SetIndent]("", "  ")). Keeps the
//     artifact diff-friendly in PR review while staying
//     consistent with `gofmt`-style two-space JSON used
//     elsewhere in the repo's fixture tree.
//
//   - HTML escaping: DISABLED
//     ([json.Encoder.SetEscapeHTML](false)). The default
//     `encoding/json` behaviour escapes `<`, `>`, `&`
//     into `\u003c`, `\u003e`, `\u0026` so a JSON
//     document is safe to embed inside an HTML script
//     tag; downstream consumers do not embed the
//     findings JSON in HTML and the unescaped form
//     keeps Markdown snippets in `Rule.DescriptionMD`
//     and source-snippet contexts in
//     [refactor.RefactorTask] readable in raw text
//     diffs.
//
//   - UUIDs: serialise via the
//     [github.com/gofrs/uuid.UUID.MarshalText] path the
//     reused engine packages already use, which yields
//     the canonical RFC 4122 lowercase hex-dashed form
//     (e.g. `"550e8400-e29b-41d4-a716-446655440000"`).
//     `RunArtifact.Policy.PolicyVersionID`,
//     `RunArtifact.Context.RepoID`,
//     `RunArtifact.Run.RunID`, `Verdict.RunID`,
//     `Finding.FindingID` etc. all funnel through this
//     path; no per-call conversion is required here.
//
// # Streaming
//
// Render streams a single top-level JSON document via
// [json.Encoder.Encode] -- the encoder appends a trailing
// `'\n'` per its documented contract, which keeps the file
// POSIX-compliant (terminated by a newline) so editors and
// `diff` do not flag a "no newline at end of file" marker.
//
// # Determinism
//
// Determinism of the emitted bytes is the composition root's
// responsibility, not this renderer's: every slice on
// [RunArtifact] (Files, Skips, Findings, HotSpots, Tasks,
// Samples, DarkMetrics) is already sorted by its upstream
// producer per architecture Sec 4.7 row notes. The renderer
// preserves that order verbatim; it does NOT re-sort.
//
// # Why no fields and no constructor
//
// A future renderer kind (HTML, SARIF, ...) only adds a new
// [Renderer] implementor, not a new dispatch site. Keeping
// `JSON` field-less means the composition root's
// `report.JSON{}` literal is the entire construction surface
// and no test can accidentally observe (or mutate) renderer
// state across calls. The iter-1 evaluator flagged any
// exported configuration knob on the markdown renderer
// because it would let callers render a non-canonical
// surface; the same rule applies here.
type JSON struct{}

// Compile-time assertion that *JSON satisfies [Renderer].
// The vet-friendly form (`var _ Renderer = (*JSON)(nil)`)
// fails to compile if the [Renderer] surface drifts.
var _ Renderer = (*JSON)(nil)

// Render writes the findings JSON document for `art` into
// `w`. Returns the first I/O or marshal failure verbatim so
// the composition root's `--findings` flag surface can
// report the offending path in its operator-facing
// diagnostic.
//
// `ctx` is honoured for cancellation only via a pre-flight
// check; the encoder itself performs no context-bound I/O
// beyond writing to `w`. A cancelled context short-circuits
// before any bytes are written so a downstream consumer
// either sees the full document or no document at all.
func (JSON) Render(ctx context.Context, art RunArtifact, w io.Writer) error {
	if w == nil {
		return fmt.Errorf("report: json render requires a non-nil writer")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("report: json render cancelled before write: %w", err)
	}
	// Auto-stamp schema version so every emitted document
	// carries the version tag regardless of composition-root
	// wiring. art is by-value; caller's copy is unaffected.
	if art.SchemaVersion == "" {
		art.SchemaVersion = SchemaVersionCurrent
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(art); err != nil {
		return fmt.Errorf("report: json encode run artifact: %w", err)
	}
	return nil
}

// SchemaVersionMismatchError is the typed error
// [JSON.RenderFromBytes] returns when the unmarshalled
// `findings.json` carries a `schemaVersion` that does not
// match the current binary's [SchemaVersionCurrent]
// constant. The composition root in `cmd/cleanc/main.go`
// uses [errors.As] to detect this case and exits with
// [flags.ExitUsage] (64) so an operator running a stale
// CLI against a newer artifact (or vice versa) gets a
// clear, actionable error naming both versions rather
// than a partial / corrupt markdown render.
//
// The error message itself includes both `Got` and `Want`
// verbatim so log lines and crash dumps surface the
// mismatch even if the dispatcher's stderr line is lost.
type SchemaVersionMismatchError struct {
	// Got is the `schemaVersion` value read from the
	// supplied findings.json bytes (may be empty if the
	// document predates schema stamping).
	Got string
	// Want is the binary's pinned [SchemaVersionCurrent].
	Want string
}

// Error implements the error interface. The phrasing
// deliberately names BOTH versions so the operator can
// correlate the artifact against the binary without
// re-reading either file.
func (e *SchemaVersionMismatchError) Error() string {
	return fmt.Sprintf("report: findings.json schemaVersion %q does not match this binary's %q",
		e.Got, e.Want)
}

// RenderFromBytes unmarshals a previously-written
// `findings.json` artifact into a [RunArtifact] and
// re-renders the human-readable markdown report into `w`
// WITHOUT re-running the analyze pipeline. This is the
// helper `cleanc report <findings.json>` (implementation-
// plan.md Stage 3.4) dispatches through so CI consumers
// that already have the JSON sidecar from an upstream
// `cleanc analyze` run do not have to walk the source
// tree a second time just to regenerate the markdown.
//
// # Schema-version refusal contract
//
// If the unmarshalled artifact's [RunArtifact.SchemaVersion]
// does not match [SchemaVersionCurrent], the helper returns
// a [*SchemaVersionMismatchError] WITHOUT writing any bytes
// to `w`. The composition root maps this typed error to
// [flags.ExitUsage] (64) so a stale CLI invoked against a
// newer artifact fails loudly with both versions named in
// the diagnostic, rather than producing a partial render.
//
// An empty `SchemaVersion` is also treated as a mismatch:
// every artifact emitted by [JSON.Render] auto-stamps the
// current version (see [JSON.Render] doc comment), so a
// missing tag necessarily means the bytes did not come from
// any released `cleanc` build.
//
// # Unmarshal failures
//
// JSON parse failures surface verbatim (wrapped with a
// `report: unmarshal previous findings:` prefix) so the
// composition root's stderr line points at the offending
// path. The helper does NOT attempt to recover a partially
// valid document.
//
// # Render writer
//
// `w` MUST be non-nil; a nil writer returns a clear error
// before any unmarshal work happens. The internal markdown
// renderer is invoked with [context.Background] because
// the only context-bound work is a pre-flight cancellation
// check and `cleanc report` is a synchronous CLI surface
// with no separate cancel signal beyond the process
// lifetime.
func (j JSON) RenderFromBytes(prevFindings []byte, w io.Writer) error {
	if w == nil {
		return fmt.Errorf("report: render-from-bytes requires a non-nil writer")
	}
	var art RunArtifact
	if err := json.Unmarshal(prevFindings, &art); err != nil {
		return fmt.Errorf("report: unmarshal previous findings: %w", err)
	}
	if art.SchemaVersion != SchemaVersionCurrent {
		return &SchemaVersionMismatchError{
			Got:  art.SchemaVersion,
			Want: SchemaVersionCurrent,
		}
	}
	return Markdown{}.Render(context.Background(), art, w)
}
