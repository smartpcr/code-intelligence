// -----------------------------------------------------------------------
// <copyright file="markdown.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package report

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// Markdown is the [Renderer] implementation that produces the
// human-readable `report.md` surface (architecture Sec 3.7.1).
//
// The zero value is usable: every knob falls back to the
// architecture-pinned default ([parser.DefaultRegistry] for the
// active parser fleet). Tests inject a custom [Markdown.Parsers]
// to assert against a known language set without depending on
// the process-wide default.
type Markdown struct {
	// Parsers is the parser registry consulted for the
	// header's "active parser fleet" row. When nil, the
	// renderer falls back to [parser.DefaultRegistry] as
	// architecture Sec 3.7.1 step 1 mandates ("active parser
	// fleet from `parser.DefaultRegistry().Languages()`").
	//
	// Exposed as a struct field (rather than a function-level
	// override) so the composition root can thread the SAME
	// registry instance the orchestrator used; tests use it
	// to inject a deterministic fleet without racing the
	// package-level default.
	Parsers *parser.Registry
}

// Compile-time assertion that *Markdown satisfies [Renderer].
// The vet-friendly form (`var _ Renderer = (*Markdown)(nil)`)
// fails to compile if the [Renderer] surface drifts.
var _ Renderer = (*Markdown)(nil)

// Render writes the markdown report for `art` into `w`. The
// output is buffered through a [bufio.Writer] so a renderer
// whose downstream `w` is unbuffered (e.g. `os.Stdout`) still
// performs a single syscall per flush rather than one per
// `Fprintf`.
//
// The current stage emits exactly two architecture-pinned
// sections (Sec 3.7.1 steps 1 and 2):
//
//   - Header: repo path, head SHA, policy id + version, active
//     parser fleet, dark-metric summary count.
//   - Verdict: a single line `Verdict: <pass|warn|block>`.
//
// Downstream report-stage workstreams add the remaining four
// sections (findings, hot-spots, refactor plan, diagnostics)
// in place on this same `Markdown` writer; the
// [RunArtifact] container deliberately ships every field they
// need today (architecture Sec 4.7) so the section additions
// land as new render helpers without touching this method's
// signature or the [RunArtifact] shape.
//
// `ctx` is honoured for cancellation between the header and
// verdict blocks so a caller that cancels mid-render does not
// flush a partial header byte stream past the cancellation
// point.
func (m Markdown) Render(ctx context.Context, art RunArtifact, w io.Writer) error {
	if w == nil {
		return fmt.Errorf("report: Markdown.Render: writer is nil")
	}
	bw := bufio.NewWriter(w)
	if err := m.renderHeader(ctx, art, bw); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("report: Markdown.Render: %w", err)
	}
	if err := m.renderVerdict(art, bw); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("report: Markdown.Render: flush: %w", err)
	}
	return nil
}

// renderHeader emits architecture Sec 3.7.1 step 1: a level-1
// heading followed by a bullet list of the five header rows.
//
// Layout is pinned so a future schema audit (and the unit
// test in markdown_test.go) sees the rows in the architecture
// order, not the field-declaration order of [RunArtifact]:
//
//   - "Repo path"          -- from `RunArtifact.Context.RootPath`
//   - "Head SHA"           -- from `RunArtifact.Context.HeadSHA`
//   - "Policy"             -- the architecture-pinned
//     "policy id + version" pair (`Policy.PolicyVersionID`
//     and `Policy.Name`)
//   - "Active parser fleet" -- the registry's `Languages()`
//     slice joined as a comma-separated value
//   - "Dark metrics"       -- the integer count of distinct
//     `(metric_kind, language)` dark rows
//
// Each row is emitted with `**Label:** value` markdown so an
// operator skimming the file sees the label in bold without
// the renderer relying on a table grid (tables render badly
// on narrow terminals and in code-review diffs; the bullet
// list survives both).
func (m Markdown) renderHeader(ctx context.Context, art RunArtifact, w *bufio.Writer) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("report: Markdown.Render: %w", err)
	}
	if _, err := fmt.Fprintln(w, "# Clean-code report"); err != nil {
		return wrapWrite(err)
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return wrapWrite(err)
	}
	if _, err := fmt.Fprintf(w, "- **Repo path:** %s\n", headerValue(art.Context.RootPath)); err != nil {
		return wrapWrite(err)
	}
	if _, err := fmt.Fprintf(w, "- **Head SHA:** %s\n", headerValue(art.Context.HeadSHA)); err != nil {
		return wrapWrite(err)
	}
	if _, err := fmt.Fprintf(w, "- **Policy:** %s\n", formatPolicyHeader(art)); err != nil {
		return wrapWrite(err)
	}
	if _, err := fmt.Fprintf(w, "- **Active parser fleet:** %s\n", formatParserFleet(m.parserRegistry())); err != nil {
		return wrapWrite(err)
	}
	if _, err := fmt.Fprintf(w, "- **Dark metrics:** %d\n", len(art.DarkMetrics)); err != nil {
		return wrapWrite(err)
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return wrapWrite(err)
	}
	return nil
}

// renderVerdict emits architecture Sec 3.7.1 step 2: the
// LITERAL single-line `Verdict: <pass|warn|block>` echoing
// `RunArtifact.Verdict.Verdict`.
//
// The architecture and tech-spec pin the lowercase canonical
// labels [rule_engine.VerdictPass] / [rule_engine.VerdictWarn]
// / [rule_engine.VerdictBlock] -- the renderer emits the
// literal underlying string verbatim. When the verdict has
// not been stamped yet (the zero value, i.e. the rule engine
// has not yet executed against this artifact), the renderer
// emits the architecture-mandated "unknown" placeholder so
// the line never disappears from the report -- the operator
// always sees a Verdict line even if the engine bailed
// before stamping one.
func (m Markdown) renderVerdict(art RunArtifact, w *bufio.Writer) error {
	if _, err := fmt.Fprintln(w, "## Verdict"); err != nil {
		return wrapWrite(err)
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return wrapWrite(err)
	}
	if _, err := fmt.Fprintf(w, "Verdict: %s\n", verdictLabel(art.Verdict.Verdict)); err != nil {
		return wrapWrite(err)
	}
	return nil
}

// parserRegistry returns the registry the renderer reads for
// the active parser fleet row. Defaults to
// [parser.DefaultRegistry] per architecture Sec 3.7.1 step 1
// when [Markdown.Parsers] is nil.
func (m Markdown) parserRegistry() *parser.Registry {
	if m.Parsers != nil {
		return m.Parsers
	}
	return parser.DefaultRegistry()
}

// formatPolicyHeader produces the "policy id + version" cell
// per architecture Sec 3.7.1 step 1. v1 [steward.PolicyVersion]
// carries the identity-only `PolicyVersionID` UUID and a
// human-tagged `Name` string; the renderer joins them with
// the literal " name=" separator so a grep against the
// rendered report can pull the id back out independent of the
// name's contents.
//
// When `Policy.PolicyVersionID` is the zero UUID (a CLI run
// that has not loaded a dev policy yet, or a test fixture
// that omits the field), the renderer emits the
// [emptyHeaderValue] placeholder so the row is never blank.
func formatPolicyHeader(art RunArtifact) string {
	id := art.Policy.PolicyVersionID.String()
	if art.Policy.PolicyVersionID == uuid.Nil {
		id = emptyHeaderValue
	}
	name := strings.TrimSpace(art.Policy.Name)
	if name == "" {
		name = emptyHeaderValue
	}
	return fmt.Sprintf("%s name=%s", id, name)
}

// formatParserFleet renders the registry's `Languages()`
// slice as a comma-separated list (e.g. `go, java, python,
// typescript`). The registry returns its languages already
// sorted (`parser.Registry.Languages`); the renderer
// preserves that order verbatim so the header is
// byte-identical across runs.
//
// An empty registry (a hypothetical test that strips the
// fleet) emits [emptyHeaderValue] rather than a bare empty
// string so the row is never visually blank.
func formatParserFleet(reg *parser.Registry) string {
	if reg == nil {
		return emptyHeaderValue
	}
	langs := reg.Languages()
	if len(langs) == 0 {
		return emptyHeaderValue
	}
	return strings.Join(langs, ", ")
}

// verdictLabel returns the lowercase canonical verdict label
// for `v`. The valid set is pinned by [rule_engine.Verdict.IsValid];
// any other value (including the zero "" -- the engine has
// not stamped a verdict yet) renders as the
// [unknownVerdictLabel] placeholder so the single-line
// Verdict block is never empty.
func verdictLabel(v rule_engine.Verdict) string {
	if v.IsValid() {
		return string(v)
	}
	return unknownVerdictLabel
}

// headerValue returns `v` verbatim or the [emptyHeaderValue]
// placeholder when `v` is empty. Centralised so every header
// row's empty-value treatment is identical.
func headerValue(v string) string {
	if v == "" {
		return emptyHeaderValue
	}
	return v
}

// wrapWrite wraps an underlying writer error with the
// package-qualified prefix the [Renderer] contract requires
// (the composition root surfaces the wrapped string verbatim
// in its `--out <path>` diagnostic).
func wrapWrite(err error) error {
	return fmt.Errorf("report: Markdown.Render: write: %w", err)
}

const (
	// emptyHeaderValue is the visible placeholder the
	// renderer emits when a header row's source value is
	// empty (zero UUID, empty string). Pinned as a constant
	// so the empty-value glyph is consistent across rows
	// and so a future operator-facing change lands in one
	// place.
	emptyHeaderValue = "(unset)"

	// unknownVerdictLabel is the placeholder emitted in the
	// Verdict block when the rule engine has not stamped a
	// canonical verdict on the [RunArtifact]. The verdict
	// closed set per architecture Sec 5.4.3 is
	// `{pass, warn, block}`; anything else (including the
	// empty zero value) is the "engine has not run /
	// degraded short-circuit before stamping" case.
	unknownVerdictLabel = "unknown"
)
