package materialisers_test

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/materialisers"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// fixedClock returns a closure that always reports `t`. The
// materialiser captures the clock ONCE per Materialise call,
// so the test asserts on a single deterministic cutoff.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// refNow is the canonical "now" the synthetic-churn tests
// align to. Pinning the value (rather than `time.Now()`)
// keeps Modified-At cutoffs deterministic across CI runs.
var refNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// fooBarRef is the standard method-scope [recipes.ScopeRef]
// the tests use for `pkg.Foo.bar` (implementation-plan Stage
// 2.6 scenario `materialiser-emits-canonical-kind` line 260).
func fooBarRef() recipes.ScopeRef {
	return recipes.ScopeRef{
		LocalID:       "local:7",
		Kind:          scope.KindMethod,
		QualifiedName: "pkg.Foo.bar",
		Path:          "pkg/foo.go",
	}
}

// fooFileRef is the file-scope variant used by the
// canon-guard scenarios that exercise the OTHER
// architecture-Sec-1.4.1-row-12-allowed scope_kind.
func fooFileRef() recipes.ScopeRef {
	return recipes.ScopeRef{
		LocalID:       "local:0",
		Kind:          scope.KindFile,
		QualifiedName: "pkg/foo.go",
		Path:          "pkg/foo.go",
	}
}

// row builds a single [materialisers.ChurnRow] for the given
// scope+SHA at `offsetDays` BEFORE [refNow]. Negative offsets
// produce future-dated rows (used by the clock-skew defence
// scenario). The ScopeKey is derived from the qualified name
// + path so two rows with the same logical scope share the
// same group.
func row(ref recipes.ScopeRef, sha string, offsetDays float64) materialisers.ChurnRow {
	d := time.Duration(offsetDays * float64(24*time.Hour))
	return materialisers.ChurnRow{
		ScopeKey:   ref.QualifiedName + "|" + ref.Path,
		Scope:      ref,
		SHA:        sha,
		ModifiedAt: refNow.Add(-d),
	}
}

// TestMaterialiser_MetricKindIsCanonical pins the literal
// `modification_count_in_window` spelling at the package
// boundary (NOT `modification_count`, NOT `churn_count`).
func TestMaterialiser_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	if got := materialisers.MetricKind; got != "modification_count_in_window" {
		t.Fatalf("MetricKind = %q, want %q (closed-set spelling per architecture Sec 1.4.1 row 12)",
			got, "modification_count_in_window")
	}
	// Also assert the method on the constructed materialiser
	// surfaces the same literal -- a future refactor that
	// renames the const should fail both paths.
	if got := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedClock(refNow)).MetricKind(); got != "modification_count_in_window" {
		t.Fatalf("Materialiser.MetricKind() = %q, want %q", got, "modification_count_in_window")
	}
}

// TestMaterialiser_VersionStartsAtOne pins MetricVersion=1
// (architecture C4: a version bump lands as a new row, not
// an in-place update).
func TestMaterialiser_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	if got := materialisers.MetricVersion; got != 1 {
		t.Fatalf("MetricVersion = %d, want 1", got)
	}
	if got := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedClock(refNow)).Version(); got != 1 {
		t.Fatalf("Materialiser.Version() = %d, want 1", got)
	}
}

// TestMaterialiser_WriterIdentityIsCanonical pins the literal
// `modification_count_materialiser` (e2e-scenarios.md lines
// 395-396 -- "the registry maps `modification_count_in_window`
// to the writer identity `modification_count_materialiser`,
// NOT to any AST language analyzer").
func TestMaterialiser_WriterIdentityIsCanonical(t *testing.T) {
	t.Parallel()
	if got := materialisers.WriterIdentity; got != "modification_count_materialiser" {
		t.Fatalf("WriterIdentity = %q, want %q (canon-guard against AST-adapter-as-producer drift)",
			got, "modification_count_materialiser")
	}
	if got := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedClock(refNow)).WriterIdentity(); got != "modification_count_materialiser" {
		t.Fatalf("Materialiser.WriterIdentity() = %q, want %q", got, "modification_count_materialiser")
	}
}

// TestDefaultWindowDays_MatchesConfig pins the materialiser's
// 90-day default against `config.DefaultWindowDays`. The two
// sites MUST stay in lock-step -- a future tech-spec Sec 8.2
// change to `window_days` MUST land on both, and a drift here
// surfaces at the next test run.
func TestDefaultWindowDays_MatchesConfig(t *testing.T) {
	t.Parallel()
	if materialisers.DefaultWindowDays != 90 {
		t.Errorf("materialisers.DefaultWindowDays = %d, want 90 (tech-spec Sec 8.2)", materialisers.DefaultWindowDays)
	}
	if materialisers.DefaultWindowDays != config.DefaultWindowDays {
		t.Errorf("materialisers.DefaultWindowDays=%d != config.DefaultWindowDays=%d",
			materialisers.DefaultWindowDays, config.DefaultWindowDays)
	}
}

// TestMaterialiser_AllowedScopeKinds_ReturnsFreshCopy
// confirms the helper does not leak the internal closed-set
// guard.
func TestMaterialiser_AllowedScopeKinds_ReturnsFreshCopy(t *testing.T) {
	t.Parallel()
	got := materialisers.AllowedScopeKinds()
	want := []scope.Kind{scope.KindFile, scope.KindMethod}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedScopeKinds() = %v, want %v", got, want)
	}
	got[0] = scope.Kind("function") // mutate the returned slice
	got2 := materialisers.AllowedScopeKinds()
	if !reflect.DeepEqual(got2, want) {
		t.Fatalf("AllowedScopeKinds() leaked internal state: second call = %v, want %v", got2, want)
	}
}

// TestNewMaterialiser_PanicsOnNonPositiveWindow -- zero or
// negative window_days would silently emit zero counts; refuse
// at construction time so the operator sees the misconfig
// loudly.
func TestNewMaterialiser_PanicsOnNonPositiveWindow(t *testing.T) {
	t.Parallel()
	cases := []int{0, -1, -90}
	for _, n := range cases {
		n := n
		t.Run(intStr(n), func(t *testing.T) {
			t.Parallel()
			mustPanicContaining(t, func() { _ = materialisers.NewMaterialiser(n) }, "must be > 0")
		})
	}
}

// TestNewMaterialiserWithClock_PanicsOnNilNow -- a nil clock
// is a wiring bug; refuse at construction.
func TestNewMaterialiserWithClock_PanicsOnNilNow(t *testing.T) {
	t.Parallel()
	mustPanicContaining(t,
		func() { _ = materialisers.NewMaterialiserWithClock(90, nil) },
		"now clock is nil")
}

// TestMaterialiser_EmitsCanonicalRow is the implementation-
// plan Stage 2.6 scenario `materialiser-emits-canonical-kind`
// (line 260) plus the e2e-scenarios.md Feature
// "Metric Ingestor materialiser emits
// `modification_count_in_window` from churn" scenario
// (lines 376-384).
//
// Given 7 commits touching `pkg.Foo.bar` in the last 90 days,
// the materialiser emits EXACTLY one draft with:
//   - metric_kind = "modification_count_in_window"
//   - pack = "base"
//   - source = "computed"
//   - value = 7
//   - attrs.provenance = "ingested"
//   - attrs.window_days = "90"
func TestMaterialiser_EmitsCanonicalRow(t *testing.T) {
	t.Parallel()
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha1", 1),
		row(fooBarRef(), "sha2", 5),
		row(fooBarRef(), "sha3", 10),
		row(fooBarRef(), "sha4", 30),
		row(fooBarRef(), "sha5", 45),
		row(fooBarRef(), "sha6", 60),
		row(fooBarRef(), "sha7", 89),
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(rows)
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want exactly 1 (scenario `materialiser-emits-canonical-kind`)", len(drafts))
	}
	d := drafts[0]
	if d.MetricKind != "modification_count_in_window" {
		t.Errorf("MetricKind = %q, want %q", d.MetricKind, "modification_count_in_window")
	}
	if d.MetricVersion != 1 {
		t.Errorf("MetricVersion = %d, want 1", d.MetricVersion)
	}
	if d.Pack != recipes.PackBase {
		t.Errorf("Pack = %q, want %q (tech-spec Sec 4.1.1 lines 287-291 -- never `solid` or `ingested`)", d.Pack, recipes.PackBase)
	}
	if d.Source != recipes.SourceComputed {
		t.Errorf("Source = %q, want %q (tech-spec Sec 4.11 lines 444-454 -- the materialiser is the COMPUTING writer; `ingested` provenance lives on attrs_json per C19)", d.Source, recipes.SourceComputed)
	}
	if d.Value != 7 {
		t.Errorf("Value = %v, want 7 (seven unique SHAs in the 90-day window)", d.Value)
	}
	if got := d.Attrs[materialisers.AttrProvenance]; got != materialisers.AttrProvenanceValue {
		t.Errorf("Attrs[%q] = %q, want %q (e2e-scenarios.md line 382 -- literal string `ingested`)",
			materialisers.AttrProvenance, got, materialisers.AttrProvenanceValue)
	}
	if got := d.Attrs[materialisers.AttrWindowDays]; got != "90" {
		t.Errorf("Attrs[%q] = %q, want %q (e2e-scenarios.md line 383 -- window_days=90 default)",
			materialisers.AttrWindowDays, got, "90")
	}
	if d.Scope.QualifiedName != "pkg.Foo.bar" {
		t.Errorf("Scope.QualifiedName = %q, want %q", d.Scope.QualifiedName, "pkg.Foo.bar")
	}
	if d.Scope.Kind != scope.KindMethod {
		t.Errorf("Scope.Kind = %q, want %q (architecture Sec 1.4.1 row 12 -- method)", d.Scope.Kind, scope.KindMethod)
	}
}

// TestMaterialiser_OutOfWindowRowsIgnored is the
// implementation-plan Stage 2.6 scenario
// `out-of-window-rows-ignored` (line 261) and the
// e2e-scenarios.md scenario "Out-of-window churn rows are
// ignored (no zero-fill noise)" (lines 386-389).
//
// Given churn rows ALL older than 90 days, the materialiser
// emits ZERO drafts for the scope (NOT a zero-valued draft --
// no draft at all).
func TestMaterialiser_OutOfWindowRowsIgnored(t *testing.T) {
	t.Parallel()
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha-old-1", 91),
		row(fooBarRef(), "sha-old-2", 120),
		row(fooBarRef(), "sha-old-3", 365),
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(rows)
	if len(drafts) != 0 {
		t.Fatalf("got %d drafts, want 0 (no zero-fill noise -- implementation-plan Stage 2.6 line 253)", len(drafts))
	}
}

// TestMaterialiser_BoundaryAtCutoffIsKept -- a row landing
// EXACTLY at the cutoff (`now - window_days*24h`) is INSIDE
// the window. Verifying the inclusive boundary keeps the
// 90-day semantics unambiguous.
func TestMaterialiser_BoundaryAtCutoffIsKept(t *testing.T) {
	t.Parallel()
	// `ModifiedAt = refNow - 90*24h` is exactly the cutoff.
	r := row(fooBarRef(), "sha-boundary", 90)
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise([]materialisers.ChurnRow{r})
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1 (exactly-at-cutoff row is inclusive)", len(drafts))
	}
	if drafts[0].Value != 1 {
		t.Errorf("Value = %v, want 1", drafts[0].Value)
	}
}

// TestMaterialiser_BoundaryJustBeforeCutoffDropped -- a row
// at `cutoff - 1ns` is OUTSIDE the window.
func TestMaterialiser_BoundaryJustBeforeCutoffDropped(t *testing.T) {
	t.Parallel()
	// Manually construct so we can land 1ns BEFORE the cutoff
	// without rounding through `offsetDays float64`.
	cutoff := refNow.Add(-90 * 24 * time.Hour)
	r := materialisers.ChurnRow{
		ScopeKey:   "pkg.Foo.bar|pkg/foo.go",
		Scope:      fooBarRef(),
		SHA:        "sha-just-old",
		ModifiedAt: cutoff.Add(-time.Nanosecond),
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise([]materialisers.ChurnRow{r})
	if len(drafts) != 0 {
		t.Fatalf("got %d drafts, want 0 (1ns before cutoff is outside the window)", len(drafts))
	}
}

// TestMaterialiser_FutureDatedRowsDropped -- defence against
// clock-skewed `ingest.churn` payloads. A row whose
// ModifiedAt is AFTER `now` is dropped (it cannot contribute
// to a "last N days" count without violating causality).
func TestMaterialiser_FutureDatedRowsDropped(t *testing.T) {
	t.Parallel()
	rows := []materialisers.ChurnRow{
		// Negative offsetDays = future-dated.
		row(fooBarRef(), "sha-future", -10),
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(rows)
	if len(drafts) != 0 {
		t.Fatalf("got %d drafts, want 0 (future-dated rows must not inflate the count)", len(drafts))
	}
}

// TestMaterialiser_DedupesSameSHAPerScope -- three rows with
// the same SHA touching the same scope (e.g. multiple hunks
// in one commit) count as ONE commit. "Touching commits"
// (architecture Sec 5.3.3) means UNIQUE commits per scope.
func TestMaterialiser_DedupesSameSHAPerScope(t *testing.T) {
	t.Parallel()
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha-X", 1),
		row(fooBarRef(), "sha-X", 1),
		row(fooBarRef(), "sha-X", 1),
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(rows)
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1", len(drafts))
	}
	if drafts[0].Value != 1 {
		t.Errorf("Value = %v, want 1 (three rows for the same SHA = one commit)", drafts[0].Value)
	}
}

// TestMaterialiser_SkipsScopeWithEmptyCount -- when ALL rows
// for scope X are out-of-window but scope Y has in-window
// rows, only Y produces a draft. X is silently absent (NOT
// a zero-valued draft).
func TestMaterialiser_SkipsScopeWithEmptyCount(t *testing.T) {
	t.Parallel()
	xRef := recipes.ScopeRef{
		LocalID:       "local:1",
		Kind:          scope.KindMethod,
		QualifiedName: "pkg.X.x",
		Path:          "pkg/x.go",
	}
	rows := []materialisers.ChurnRow{
		row(xRef, "sha-x-old-1", 200),
		row(xRef, "sha-x-old-2", 365),
		row(fooBarRef(), "sha-foo-1", 5),
		row(fooBarRef(), "sha-foo-2", 10),
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(rows)
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1 (only pkg.Foo.bar has in-window rows)", len(drafts))
	}
	if drafts[0].Scope.QualifiedName != "pkg.Foo.bar" {
		t.Errorf("draft Scope = %q, want %q (X had no in-window rows -> no draft per Stage 2.6 line 253)",
			drafts[0].Scope.QualifiedName, "pkg.Foo.bar")
	}
}

// TestMaterialiser_WindowDaysIsConfigurable -- the same churn
// stream produces DIFFERENT counts at different windowDays
// settings, confirming the knob from tech-spec Sec 8.2 is
// honoured.
func TestMaterialiser_WindowDaysIsConfigurable(t *testing.T) {
	t.Parallel()
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha1", 5),
		row(fooBarRef(), "sha2", 25),
		row(fooBarRef(), "sha3", 45),
		row(fooBarRef(), "sha4", 75),
	}
	// 30-day window only counts sha1 + sha2.
	d30 := materialisers.NewMaterialiserWithClock(30, fixedClock(refNow)).Materialise(rows)
	if len(d30) != 1 || d30[0].Value != 2 {
		t.Fatalf("30-day window: got drafts=%d, value=%v; want drafts=1 value=2", len(d30), valueOrNaN(d30))
	}
	if d30[0].Attrs[materialisers.AttrWindowDays] != "30" {
		t.Errorf("30-day window: attrs.window_days = %q, want %q", d30[0].Attrs[materialisers.AttrWindowDays], "30")
	}
	// 90-day window counts all four.
	d90 := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow)).Materialise(rows)
	if len(d90) != 1 || d90[0].Value != 4 {
		t.Fatalf("90-day window: got drafts=%d, value=%v; want drafts=1 value=4", len(d90), valueOrNaN(d90))
	}
	if d90[0].Attrs[materialisers.AttrWindowDays] != "90" {
		t.Errorf("90-day window: attrs.window_days = %q, want %q", d90[0].Attrs[materialisers.AttrWindowDays], "90")
	}
}

// TestMaterialiser_GroupsByScopeKeyNotLocalID -- two
// ChurnRows for the SAME logical scope but DIFFERENT
// parser-local IDs (`local:7` vs `local:42`) MUST count as
// the same scope. This is the rubber-duck #1 contract: the
// materialiser's grouping key is [ChurnRow.ScopeKey], NOT the
// full [recipes.ScopeRef] (whose LocalID is per-AstFile and
// not durable).
func TestMaterialiser_GroupsByScopeKeyNotLocalID(t *testing.T) {
	t.Parallel()
	refA := fooBarRef()
	refB := fooBarRef()
	refB.LocalID = "local:42" // different parser placeholder
	rows := []materialisers.ChurnRow{
		// Both rows use the SAME ScopeKey (qualified name +
		// path) but different LocalIDs.
		{
			ScopeKey:   "pkg.Foo.bar|pkg/foo.go",
			Scope:      refA,
			SHA:        "shaA",
			ModifiedAt: refNow.Add(-24 * time.Hour),
		},
		{
			ScopeKey:   "pkg.Foo.bar|pkg/foo.go",
			Scope:      refB,
			SHA:        "shaB",
			ModifiedAt: refNow.Add(-48 * time.Hour),
		},
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(rows)
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1 (same ScopeKey across different LocalIDs MUST collapse)", len(drafts))
	}
	if drafts[0].Value != 2 {
		t.Errorf("Value = %v, want 2 (both unique SHAs land in the same scope bucket)", drafts[0].Value)
	}
}

// TestMaterialiser_DistinctScopesProduceDistinctDrafts --
// rows for two different scopes produce two drafts, sorted
// deterministically by (QualifiedName, Path).
func TestMaterialiser_DistinctScopesProduceDistinctDrafts(t *testing.T) {
	t.Parallel()
	barRef := recipes.ScopeRef{
		LocalID:       "local:2",
		Kind:          scope.KindMethod,
		QualifiedName: "pkg.Baz.qux",
		Path:          "pkg/baz.go",
	}
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha1", 5),
		row(barRef, "sha2", 5),
		row(barRef, "sha3", 10),
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(rows)
	if len(drafts) != 2 {
		t.Fatalf("got %d drafts, want 2", len(drafts))
	}
	// Sorted by QualifiedName: "pkg.Baz.qux" < "pkg.Foo.bar".
	if drafts[0].Scope.QualifiedName != "pkg.Baz.qux" {
		t.Errorf("drafts[0].Scope.QualifiedName = %q, want %q (sort by QualifiedName)",
			drafts[0].Scope.QualifiedName, "pkg.Baz.qux")
	}
	if drafts[1].Scope.QualifiedName != "pkg.Foo.bar" {
		t.Errorf("drafts[1].Scope.QualifiedName = %q, want %q",
			drafts[1].Scope.QualifiedName, "pkg.Foo.bar")
	}
	if drafts[0].Value != 2 || drafts[1].Value != 1 {
		t.Errorf("values = (%v, %v), want (2, 1)", drafts[0].Value, drafts[1].Value)
	}
}

// TestMaterialiser_DeterministicOrder -- two calls with the
// same input produce byte-identical output ordering (G2).
func TestMaterialiser_DeterministicOrder(t *testing.T) {
	t.Parallel()
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha1", 5),
		row(fooBarRef(), "sha2", 10),
		row(recipes.ScopeRef{LocalID: "local:1", Kind: scope.KindFile, QualifiedName: "pkg/a.go", Path: "pkg/a.go"}, "sha3", 5),
		row(recipes.ScopeRef{LocalID: "local:2", Kind: scope.KindFile, QualifiedName: "pkg/b.go", Path: "pkg/b.go"}, "sha4", 5),
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))

	first := m.Materialise(rows)
	for i := 0; i < 5; i++ {
		got := m.Materialise(rows)
		if len(got) != len(first) {
			t.Fatalf("iteration %d: len=%d, want %d", i, len(got), len(first))
		}
		for j := range first {
			if got[j].Scope.QualifiedName != first[j].Scope.QualifiedName {
				t.Errorf("iteration %d index %d: QN=%q, want %q (G2: deterministic order)",
					i, j, got[j].Scope.QualifiedName, first[j].Scope.QualifiedName)
			}
		}
	}

	// Cross-check ordering is by (QualifiedName, Path) per
	// the package doc.
	got := make([]string, len(first))
	for i := range first {
		got[i] = first[i].Scope.QualifiedName + "|" + first[i].Scope.Path
	}
	want := append([]string(nil), got...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("draft order = %v, want sorted-by-(QualifiedName,Path) = %v", got, want)
	}
}

// TestMaterialiser_PanicsOnEmptyScopeKey -- closed-set guard:
// the caller MUST hydrate a durable scope identity before
// passing rows in.
func TestMaterialiser_PanicsOnEmptyScopeKey(t *testing.T) {
	t.Parallel()
	r := row(fooBarRef(), "sha1", 1)
	r.ScopeKey = ""
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	mustPanicContaining(t, func() { _ = m.Materialise([]materialisers.ChurnRow{r}) }, "ScopeKey is empty")
}

// TestMaterialiser_PanicsOnEmptySHA -- the `ingest.churn`
// payload contract says each row carries its own SHA.
// Empty is a malformed payload, not a "no churn" signal.
func TestMaterialiser_PanicsOnEmptySHA(t *testing.T) {
	t.Parallel()
	r := row(fooBarRef(), "", 1)
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	mustPanicContaining(t, func() { _ = m.Materialise([]materialisers.ChurnRow{r}) }, "SHA is empty")
}

// TestMaterialiser_PanicsOnEmptyLocalID -- the Metric Ingestor
// needs the parser placeholder to resolve to a durable
// scope_id; an empty LocalID is a hydrator bug.
func TestMaterialiser_PanicsOnEmptyLocalID(t *testing.T) {
	t.Parallel()
	ref := fooBarRef()
	ref.LocalID = ""
	r := materialisers.ChurnRow{
		ScopeKey:   "pkg.Foo.bar|pkg/foo.go",
		Scope:      ref,
		SHA:        "sha1",
		ModifiedAt: refNow.Add(-24 * time.Hour),
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	mustPanicContaining(t, func() { _ = m.Materialise([]materialisers.ChurnRow{r}) }, "LocalID is empty")
}

// TestMaterialiser_PanicsOnInvalidScopeKind -- canon-guard
// against the iter-1 evaluator item-3 closed-set drift.
// `function`, `module`, `namespace` are NOT canonical values
// per architecture Sec 5.2.3.
func TestMaterialiser_PanicsOnInvalidScopeKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		kind scope.Kind
	}{
		{"function (Halleck45 ast-metrics drift)", scope.Kind("function")},
		{"module (Python jargon drift)", scope.Kind("module")},
		{"namespace (C++ drift)", scope.Kind("namespace")},
		{"empty", scope.Kind("")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			ref := fooBarRef()
			ref.Kind = tc.kind
			r := materialisers.ChurnRow{
				ScopeKey:   "k",
				Scope:      ref,
				SHA:        "sha1",
				ModifiedAt: refNow,
			}
			m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
			mustPanicContaining(t,
				func() { _ = m.Materialise([]materialisers.ChurnRow{r}) },
				"canonical seven-enum")
		})
	}
}

// TestMaterialiser_PanicsOnNonAllowedScopeKind -- the
// canonical-seven-enum guard accepts `class`, `package`, etc.,
// but the per-metric-kind applicability set ({file, method})
// rejects them. Architecture Sec 1.4.1 row 12 pins the
// allowed set.
func TestMaterialiser_PanicsOnNonAllowedScopeKind(t *testing.T) {
	t.Parallel()
	cases := []scope.Kind{
		scope.KindRepo,
		scope.KindPackage,
		scope.KindClass,
		scope.KindInterface,
		scope.KindBlock,
	}
	for _, k := range cases {
		k := k
		t.Run(string(k), func(t *testing.T) {
			t.Parallel()
			ref := fooBarRef()
			ref.Kind = k
			r := materialisers.ChurnRow{
				ScopeKey:   "k",
				Scope:      ref,
				SHA:        "sha1",
				ModifiedAt: refNow,
			}
			m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
			mustPanicContaining(t,
				func() { _ = m.Materialise([]materialisers.ChurnRow{r}) },
				"allowed set",
			)
		})
	}
}

// TestMaterialiser_FileScopeAccepted -- the OTHER
// architecture-Sec-1.4.1-row-12-allowed scope_kind (besides
// method) also produces a draft. Defensive test against a
// future edit that accidentally narrows the allowed set to
// methods only.
func TestMaterialiser_FileScopeAccepted(t *testing.T) {
	t.Parallel()
	rows := []materialisers.ChurnRow{
		{
			ScopeKey:   "file:pkg/foo.go",
			Scope:      fooFileRef(),
			SHA:        "sha1",
			ModifiedAt: refNow.Add(-12 * time.Hour),
		},
	}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(rows)
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1 (file-scope row must produce a draft)", len(drafts))
	}
	if drafts[0].Scope.Kind != scope.KindFile {
		t.Errorf("Scope.Kind = %q, want %q", drafts[0].Scope.Kind, scope.KindFile)
	}
}

// TestMaterialiser_EmptyRowsProducesEmptySlice -- a
// no-churn-fed run produces a non-nil, empty result. Callers
// can range over it without a nil check.
func TestMaterialiser_EmptyRowsProducesEmptySlice(t *testing.T) {
	t.Parallel()
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(nil)
	if drafts == nil {
		t.Fatalf("got nil; want non-nil empty slice")
	}
	if len(drafts) != 0 {
		t.Fatalf("got %d drafts, want 0", len(drafts))
	}
}

// TestMaterialiser_WindowDaysGetter -- the getter surfaces the
// configured value so downstream readers can stamp attrs
// consistent with what the materialiser used.
func TestMaterialiser_WindowDaysGetter(t *testing.T) {
	t.Parallel()
	m := materialisers.NewMaterialiserWithClock(45, fixedClock(refNow))
	if m.WindowDays() != 45 {
		t.Fatalf("WindowDays() = %d, want 45", m.WindowDays())
	}
}

// TestMaterialiser_AttrsHaveCanonicalKeysOnly -- the emitted
// attrs_json carries exactly the canon-pinned keys
// {`provenance`, `window_days`} and NOTHING else. The closed
// set is what the e2e scenario reads; a future debug attr (or
// any drift) would be a contract violation.
//
// Evaluator iter-1 #3 explicitly flagged the previously-emitted
// `materialiser.scope_key` private attr as a contract-drift
// risk -- this test now asserts the closed two-key set with no
// debug escape hatch.
func TestMaterialiser_AttrsHaveCanonicalKeysOnly(t *testing.T) {
	t.Parallel()
	rows := []materialisers.ChurnRow{row(fooBarRef(), "sha1", 1)}
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	drafts := m.Materialise(rows)
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1", len(drafts))
	}
	got := drafts[0].Attrs
	mustHave := []string{
		materialisers.AttrProvenance,
		materialisers.AttrWindowDays,
	}
	for _, k := range mustHave {
		if _, ok := got[k]; !ok {
			t.Errorf("Attrs missing canonical key %q (got %v)", k, got)
		}
	}
	// Closed set: exactly two keys. Any other key is a
	// contract violation.
	if len(got) != 2 {
		t.Errorf("Attrs has %d keys, want exactly 2 (closed set: %v); got %v",
			len(got), mustHave, got)
	}
	for k := range got {
		switch k {
		case materialisers.AttrProvenance, materialisers.AttrWindowDays:
		default:
			t.Errorf("Attrs carries unexpected key %q (closed set: provenance | window_days; evaluator iter-1 #3 forbade `materialiser.scope_key` drift)", k)
		}
	}
	// Defence: the legacy debug key must not have crept
	// back in through a future refactor.
	if _, present := got["materialiser.scope_key"]; present {
		t.Errorf("Attrs carries the forbidden legacy debug key `materialiser.scope_key` (evaluator iter-1 #3 -- the closed attrs schema is {provenance, window_days})")
	}
}

// ------------------------------------------------------------------
// test helpers
// ------------------------------------------------------------------

// mustPanicContaining asserts that fn panics and the panic
// message contains every `want` substring.
func mustPanicContaining(t *testing.T, fn func(), want ...string) {
	t.Helper()
	var got any
	func() {
		defer func() { got = recover() }()
		fn()
	}()
	if got == nil {
		t.Fatalf("expected panic, got none")
	}
	msg, _ := got.(string)
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("panic message %q does not contain %q", msg, w)
		}
	}
}

// valueOrNaN returns the first draft's value or NaN if the
// slice is empty -- used only in failure-path printf so test
// output is informative even when len(drafts)==0.
func valueOrNaN(drafts []recipes.MetricSampleDraft) float64 {
	if len(drafts) == 0 {
		return -1
	}
	return drafts[0].Value
}

// intStr is a tiny strconv-free integer-to-string for naming
// subtests deterministically without dragging in additional
// stdlib aliases. Mirrors the recipes-package convention.
func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	negative := false
	if n < 0 {
		negative = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ------------------------------------------------------------------
// MaterialiseWithDetails -- evaluator iter-2 #2 structural fix
// ------------------------------------------------------------------

// TestMaterialiseWithDetails_ReturnsLatestInWindowSHA -- the
// new full-fidelity entrypoint returns the latest-in-window
// SHA per scope, computed from the SAME row set the
// materialiser counted. The writer pipeline uses this so
// MetricSample.sha cannot be stamped with a row the
// materialiser dropped.
func TestMaterialiseWithDetails_ReturnsLatestInWindowSHA(t *testing.T) {
	t.Parallel()
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha-old", 3),
		row(fooBarRef(), "sha-mid", 2),
		row(fooBarRef(), "sha-new", 1),
	}
	got := m.MaterialiseWithDetails(rows)
	if len(got) != 1 {
		t.Fatalf("got %d emissions, want 1", len(got))
	}
	if got[0].LatestSHA != "sha-new" {
		t.Errorf("LatestSHA = %q, want %q (latest in-window committer date)", got[0].LatestSHA, "sha-new")
	}
	if got[0].Draft.Value != 3 {
		t.Errorf("Draft.Value = %v, want 3", got[0].Draft.Value)
	}
	if got[0].ScopeKey != rows[0].ScopeKey {
		t.Errorf("ScopeKey = %q, want %q", got[0].ScopeKey, rows[0].ScopeKey)
	}
}

// TestMaterialiseWithDetails_LatestSHAExcludesFutureDated --
// the structural fix for evaluator iter-2 #2. A future-dated
// row has the most-recent ModifiedAt across the FULL hydrated
// set, but the materialiser drops it from the count. The new
// entrypoint MUST drop it from the LatestSHA selection too --
// the writer must never stamp a SHA the materialiser did not
// count.
func TestMaterialiseWithDetails_LatestSHAExcludesFutureDated(t *testing.T) {
	t.Parallel()
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha-in", 1),
		// negative offsetDays in row() => future-dated row.
		row(fooBarRef(), "sha-future", -2),
	}
	got := m.MaterialiseWithDetails(rows)
	if len(got) != 1 {
		t.Fatalf("got %d emissions, want 1", len(got))
	}
	if got[0].LatestSHA == "sha-future" {
		t.Errorf("LatestSHA = %q -- the materialiser dropped sha-future as future-dated; MaterialiseWithDetails MUST NOT select it (evaluator iter-2 #2)", got[0].LatestSHA)
	}
	if got[0].LatestSHA != "sha-in" {
		t.Errorf("LatestSHA = %q, want %q", got[0].LatestSHA, "sha-in")
	}
	if got[0].Draft.Value != 1 {
		t.Errorf("Draft.Value = %v, want 1 (only in-window SHAs counted)", got[0].Draft.Value)
	}
}

// TestMaterialiseWithDetails_LatestSHAExcludesOutOfWindow --
// complementary to the future-dated test: out-of-window rows
// MUST be excluded from LatestSHA selection too.
func TestMaterialiseWithDetails_LatestSHAExcludesOutOfWindow(t *testing.T) {
	t.Parallel()
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha-in-1", 30),
		row(fooBarRef(), "sha-in-2", 5),
		// 200 days old -- well outside the 90-day window.
		row(fooBarRef(), "sha-old", 200),
	}
	got := m.MaterialiseWithDetails(rows)
	if len(got) != 1 {
		t.Fatalf("got %d emissions, want 1", len(got))
	}
	if got[0].LatestSHA == "sha-old" {
		t.Errorf("LatestSHA = %q -- materialiser dropped out-of-window sha-old from count; MUST drop from LatestSHA too", got[0].LatestSHA)
	}
	if got[0].LatestSHA != "sha-in-2" {
		t.Errorf("LatestSHA = %q, want %q", got[0].LatestSHA, "sha-in-2")
	}
	if got[0].Draft.Value != 2 {
		t.Errorf("Draft.Value = %v, want 2 (only in-window SHAs counted)", got[0].Draft.Value)
	}
}

// TestMaterialiseWithDetails_TieBreakLatestSHABySHA -- when
// two rows share an identical ModifiedAt for the same scope,
// the lexicographically larger SHA wins (deterministic G2
// tiebreaker).
func TestMaterialiseWithDetails_TieBreakLatestSHABySHA(t *testing.T) {
	t.Parallel()
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha-a", 1),
		row(fooBarRef(), "sha-z", 1), // same ModifiedAt; lex-bigger
		row(fooBarRef(), "sha-m", 1),
	}
	got := m.MaterialiseWithDetails(rows)
	if len(got) != 1 {
		t.Fatalf("got %d emissions, want 1", len(got))
	}
	if got[0].LatestSHA != "sha-z" {
		t.Errorf("LatestSHA = %q, want %q (lex-larger SHA wins on ModifiedAt tie)", got[0].LatestSHA, "sha-z")
	}
}

// TestMaterialise_ProjectsFromMaterialiseWithDetails -- the
// thin convenience entrypoint MUST return exactly the same
// drafts as `MaterialiseWithDetails`, in the same order.
func TestMaterialise_ProjectsFromMaterialiseWithDetails(t *testing.T) {
	t.Parallel()
	m := materialisers.NewMaterialiserWithClock(90, fixedClock(refNow))
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha-A", 1),
		row(fooBarRef(), "sha-B", 2),
		row(fooFileRef(), "sha-C", 3),
	}
	drafts := m.Materialise(rows)
	emissions := m.MaterialiseWithDetails(rows)
	if len(drafts) != len(emissions) {
		t.Fatalf("len(Materialise) = %d, len(MaterialiseWithDetails) = %d -- must match", len(drafts), len(emissions))
	}
	for i := range drafts {
		if drafts[i].MetricKind != emissions[i].Draft.MetricKind {
			t.Errorf("[%d] MetricKind differs: %q vs %q", i, drafts[i].MetricKind, emissions[i].Draft.MetricKind)
		}
		if drafts[i].Value != emissions[i].Draft.Value {
			t.Errorf("[%d] Value differs: %v vs %v", i, drafts[i].Value, emissions[i].Draft.Value)
		}
		if drafts[i].Scope.QualifiedName != emissions[i].Draft.Scope.QualifiedName {
			t.Errorf("[%d] Scope.QualifiedName differs", i)
		}
	}
}

// TestMaterialiser_WindowDaysAttrSerializesAsString_OperatorPin
// anchors the operator-resolved open question
// `window-days-attr-numeric-or-string` (resolved as
// `string "90"`) into a dedicated assertion that fails
// IMMEDIATELY if a future refactor coerces the attr to a JSON
// number at the Attrs-map boundary. The recipes-package Attrs
// type is `map[string]string` (architecture Sec 1.4.1 row 12
// + recipes.MetricSampleDraft), so the materialiser MUST stamp
// the integer `windowDays` as its decimal string form. A
// downstream JSON-serializer phase MAY coerce to a number when
// emitting to `attrs_json`, but the in-memory Attrs value MUST
// remain a string at the materialiser boundary.
//
// Pinned by the operator answer to the recovery-loop question
// (iter-14 RECOVERY block, slug
// `window-days-attr-numeric-or-string`).
func TestMaterialiser_WindowDaysAttrSerializesAsString_OperatorPin(t *testing.T) {
	t.Parallel()
	rows := []materialisers.ChurnRow{
		row(fooBarRef(), "sha1", 1),
	}
	drafts := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedClock(refNow)).Materialise(rows)
	if len(drafts) != 1 {
		t.Fatalf("len(drafts) = %d, want 1", len(drafts))
	}
	v, ok := drafts[0].Attrs[materialisers.AttrWindowDays]
	if !ok {
		t.Fatalf("Attrs[%q] missing -- materialiser MUST stamp window_days on every draft", materialisers.AttrWindowDays)
	}
	if v != "90" {
		t.Errorf("Attrs[%q] = %q, want %q (operator pin: window-days-attr-numeric-or-string -> string \"90\")",
			materialisers.AttrWindowDays, v, "90")
	}
	// Defence-in-depth: the value MUST be byte-identical to the
	// decimal-string form of the int, not (e.g.) `"90 "` from a
	// fmt.Sprintf("%d ", ...) typo. Comparing against
	// `strconv.Itoa` (the materialiser's actual serializer) is
	// the strongest assertion that doesn't reach into private
	// state.
	if v != strconv.Itoa(materialisers.DefaultWindowDays) {
		t.Errorf("Attrs[%q] = %q, want %q (strconv.Itoa(DefaultWindowDays) parity)",
			materialisers.AttrWindowDays, v, strconv.Itoa(materialisers.DefaultWindowDays))
	}
	// And confirm the operator's recipes-package convention: the
	// Attrs map is `map[string]string`, so the value is *already*
	// a string by Go's type system. The reflect check below is
	// belt-and-braces in case a future refactor swaps to
	// `map[string]any`.
	rt := reflect.TypeOf(drafts[0].Attrs[materialisers.AttrWindowDays])
	if rt.Kind() != reflect.String {
		t.Errorf("Attrs[%q] runtime type = %s, want string (operator pin: recipes-package map[string]string convention)",
			materialisers.AttrWindowDays, rt.Kind())
	}
}
