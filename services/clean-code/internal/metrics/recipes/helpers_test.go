package recipes_test

import (
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// findMethodDraft locates the draft for `methodLocalID` in
// `drafts` and fails the test loudly when no such draft
// exists. Fatal-failure-on-missing keeps each per-test
// assertion crisp (no defensive nil checks).
func findMethodDraft(t *testing.T, drafts []recipes.MetricSampleDraft, methodLocalID string) recipes.MetricSampleDraft {
	t.Helper()
	for _, d := range drafts {
		if d.Scope.Kind == scope.KindMethod && d.Scope.LocalID == methodLocalID {
			return d
		}
	}
	t.Fatalf("no method draft with LocalID=%q in %v", methodLocalID, draftSummary(drafts))
	return recipes.MetricSampleDraft{}
}

// findFileDraft returns the (single) file-level draft from
// the slice. Fails the test when zero OR more than one
// file-level draft is present (a recipe MUST emit exactly one
// row at scope_kind=file per AST).
func findFileDraft(t *testing.T, drafts []recipes.MetricSampleDraft) recipes.MetricSampleDraft {
	t.Helper()
	var matches []recipes.MetricSampleDraft
	for _, d := range drafts {
		if d.Scope.Kind == scope.KindFile {
			matches = append(matches, d)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one file-level draft, got %d (%v)", len(matches), draftSummary(drafts))
	}
	return matches[0]
}

// draftSummary returns a compact debug string listing every
// draft's `(scope_kind, local_id, value)`. Used in test
// fatal messages so a failing assertion reports the actual
// emit shape, not just the missing entry.
func draftSummary(drafts []recipes.MetricSampleDraft) string {
	out := "["
	for i, d := range drafts {
		if i > 0 {
			out += ", "
		}
		out += "(" + string(d.Scope.Kind) + "/" + d.Scope.LocalID + "=" + ftoa(d.Value) + ")"
	}
	return out + "]"
}

// ftoa is a tiny float64 formatter used only by [draftSummary]
// to avoid pulling `strconv` / `fmt` into the test helper
// dependency surface.
func ftoa(f float64) string {
	if f == float64(int64(f)) {
		return itoa(int(f))
	}
	// Fallback: never reached in current tests (values are
	// integral). Returning the raw float bits as a sentinel
	// keeps the helper total.
	return "non-integer"
}
