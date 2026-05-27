package coverage_test

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/coverage"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// Real-world Cobertura sample: two `<class>` entries
// share `internal/svc/handler.go` (Foo + Bar in the same
// file). One class has overlapping line numbers with the
// other (lines 10, 20, 30 in both) so the dedup-by-unique-
// line-number aggregation is exercised. We also include a
// second package with a unique file to verify per-file
// aggregation ordering is deterministic.
const sampleCoberturaXML = `<?xml version="1.0" ?>
<coverage line-rate="0.75" branch-rate="0.5" timestamp="1700000000" version="1.0">
  <sources><source>/repo</source></sources>
  <packages>
    <package name="internal.svc">
      <classes>
        <class name="Foo" filename="internal/svc/handler.go" line-rate="0.6" branch-rate="0.5">
          <lines>
            <line number="10" hits="3" branch="false"/>
            <line number="20" hits="0" branch="false"/>
            <line number="30" hits="1" branch="true" condition-coverage="50% (1/2)"/>
            <line number="40" hits="2" branch="false"/>
          </lines>
        </class>
        <class name="Bar" filename="internal/svc/handler.go" line-rate="0.8" branch-rate="0.5">
          <lines>
            <line number="10" hits="5" branch="false"/>
            <line number="20" hits="0" branch="false"/>
            <line number="30" hits="0" branch="true" condition-coverage="0% (0/2)"/>
            <line number="50" hits="1" branch="false"/>
          </lines>
        </class>
      </classes>
    </package>
    <package name="internal.util">
      <classes>
        <class name="Helper" filename="internal/util/helper.go" line-rate="1.0">
          <lines>
            <line number="1" hits="1" branch="false"/>
            <line number="2" hits="1" branch="false"/>
          </lines>
        </class>
      </classes>
    </package>
  </packages>
</coverage>`

func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.FromString(s)
	if err != nil {
		t.Fatalf("uuid.FromString(%q): %v", s, err)
	}
	return u
}

func newPopulatedResolver(t *testing.T, repoID uuid.UUID, paths ...string) (*coverage.MapScopeResolver, map[string]uuid.UUID) {
	t.Helper()
	r := coverage.NewMapScopeResolver()
	ids := map[string]uuid.UUID{}
	for _, p := range paths {
		id, err := uuid.NewV4()
		if err != nil {
			t.Fatalf("uuid.NewV4: %v", err)
		}
		ids[p] = id
		r.Add(repoID, p, id, recipes.ScopeRef{
			Kind:          scope.KindFile,
			QualifiedName: p,
			Path:          p,
		})
	}
	return r, ids
}

func TestParseXML_AggregatesByUniqueLineNumber(t *testing.T) {
	repoID := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	sha := strings.Repeat("a", 40)
	p, err := coverage.ParseXML([]byte(sampleCoberturaXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	if len(p.Files) != 2 {
		t.Fatalf("expected 2 aggregated files, got %d (%+v)", len(p.Files), p.Files)
	}
	// Deterministic ordering: alphabetical by path.
	if p.Files[0].FilePath != "internal/svc/handler.go" {
		t.Errorf("files[0].FilePath = %q, want %q", p.Files[0].FilePath, "internal/svc/handler.go")
	}
	if p.Files[1].FilePath != "internal/util/helper.go" {
		t.Errorf("files[1].FilePath = %q, want %q", p.Files[1].FilePath, "internal/util/helper.go")
	}
	handler := p.Files[0]
	// Unique line numbers across Foo + Bar: 10, 20, 30, 40, 50 = 5 lines valid.
	if handler.LinesValid != 5 {
		t.Errorf("LinesValid = %d, want 5 (unique line numbers across the two classes)", handler.LinesValid)
	}
	// Line 10 covered (3 in Foo, 5 in Bar -> max 5 > 0).
	// Line 20 NOT covered (0 in both).
	// Line 30 covered (1 in Foo > 0, despite 0 in Bar -- max-hits rule).
	// Line 40 covered (2 in Foo).
	// Line 50 covered (1 in Bar).
	// = 4 covered.
	if handler.LinesCovered != 4 {
		t.Errorf("LinesCovered = %d, want 4 (max-hits-per-line aggregation across classes)", handler.LinesCovered)
	}
	// Branches: only line 30 has branch="true" in both classes.
	// max valid across the pair = max(2, 2) = 2; max covered = max(1, 0) = 1.
	if handler.BranchesValid != 2 {
		t.Errorf("BranchesValid = %d, want 2 (max-of-pair across overlapping branch line)", handler.BranchesValid)
	}
	if handler.BranchesCovered != 1 {
		t.Errorf("BranchesCovered = %d, want 1 (max-of-pair across overlapping branch line)", handler.BranchesCovered)
	}
}

func TestParseXML_RejectsMalformedXML(t *testing.T) {
	repoID := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"unclosed tag", `<?xml version="1.0"?><coverage><packages>`},
		{"non-XML", "not xml"},
		{"wrong root", `<?xml version="1.0"?><report><x/></report>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := coverage.ParseXML([]byte(c.body), repoID, sha)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, coverage.ErrMalformedXML) {
				t.Errorf("err = %v, want errors.Is ErrMalformedXML", err)
			}
		})
	}
}

func TestParseXML_RejectsUnsafeFilePath(t *testing.T) {
	repoID := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name string
		body string
	}{
		{
			name: "absolute unix path",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename="/etc/passwd"><lines><line number="1" hits="1"/></lines></class></classes></package></packages></coverage>`,
		},
		{
			name: "windows drive root",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename="C:\Windows\evil.cs"><lines><line number="1" hits="1"/></lines></class></classes></package></packages></coverage>`,
		},
		{
			name: "dotdot escape",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename="../../../etc/passwd"><lines><line number="1" hits="1"/></lines></class></classes></package></packages></coverage>`,
		},
		{
			name: "empty filename",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename=""><lines><line number="1" hits="1"/></lines></class></classes></package></packages></coverage>`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := coverage.ParseXML([]byte(c.body), repoID, sha)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, coverage.ErrUnsafeFilePath) {
				t.Errorf("err = %v, want errors.Is ErrUnsafeFilePath", err)
			}
		})
	}
}

func TestParseXML_RejectsMalformedConditionCoverage(t *testing.T) {
	repoID := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name string
		body string
	}{
		{
			name: "missing condition-coverage on branch=true",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename="x.go"><lines><line number="1" hits="1" branch="true"/></lines></class></classes></package></packages></coverage>`,
		},
		{
			name: "malformed condition-coverage token",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename="x.go"><lines><line number="1" hits="1" branch="true" condition-coverage="100%"/></lines></class></classes></package></packages></coverage>`,
		},
		{
			name: "zero valid",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename="x.go"><lines><line number="1" hits="1" branch="true" condition-coverage="100% (0/0)"/></lines></class></classes></package></packages></coverage>`,
		},
		{
			name: "covered greater than valid",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename="x.go"><lines><line number="1" hits="1" branch="true" condition-coverage="100% (3/2)"/></lines></class></classes></package></packages></coverage>`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := coverage.ParseXML([]byte(c.body), repoID, sha)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, coverage.ErrMalformedConditionCoverage) {
				t.Errorf("err = %v, want errors.Is ErrMalformedConditionCoverage", err)
			}
		})
	}
}

func TestParseXML_RejectsInvalidLineNumber(t *testing.T) {
	repoID := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	sha := strings.Repeat("a", 40)
	body := `<?xml version="1.0"?><coverage><packages><package><classes><class filename="x.go"><lines><line number="0" hits="1"/></lines></class></classes></package></packages></coverage>`
	_, err := coverage.ParseXML([]byte(body), repoID, sha)
	if err == nil || !errors.Is(err, coverage.ErrMalformedXML) {
		t.Fatalf("expected ErrMalformedXML for line number=0, got %v", err)
	}
}

func TestPayloadValidate(t *testing.T) {
	good := func() *coverage.Payload {
		return &coverage.Payload{
			RepoID: mustParseUUID(t, "11111111-1111-1111-1111-111111111111"),
			SHA:    strings.Repeat("a", 40),
			Files: []coverage.FileCoverage{{
				FilePath:     "x.go",
				LinesCovered: 1,
				LinesValid:   2,
			}},
		}
	}
	cases := []struct {
		name string
		mut  func(*coverage.Payload)
		want error
	}{
		{"empty repo id", func(p *coverage.Payload) { p.RepoID = uuid.Nil }, coverage.ErrEmptyRepoID},
		{"empty sha", func(p *coverage.Payload) { p.SHA = "" }, coverage.ErrEmptySHA},
		{"invalid sha", func(p *coverage.Payload) { p.SHA = "deadbeef" }, coverage.ErrInvalidSHA},
		{"empty files", func(p *coverage.Payload) { p.Files = nil }, coverage.ErrEmptyFiles},
		{"empty file_path", func(p *coverage.Payload) { p.Files[0].FilePath = "" }, coverage.ErrEmptyFilePath},
		{"unsafe file_path", func(p *coverage.Payload) { p.Files[0].FilePath = "/abs/x.go" }, coverage.ErrUnsafeFilePath},
		{"negative line covered", func(p *coverage.Payload) { p.Files[0].LinesCovered = -1 }, coverage.ErrInvalidLineCount},
		{"covered > valid lines", func(p *coverage.Payload) { p.Files[0].LinesCovered = 5; p.Files[0].LinesValid = 2 }, coverage.ErrInvalidLineCount},
		{"negative branch covered", func(p *coverage.Payload) { p.Files[0].BranchesCovered = -1 }, coverage.ErrInvalidBranchCount},
		{"covered > valid branches", func(p *coverage.Payload) {
			p.Files[0].BranchesCovered = 3
			p.Files[0].BranchesValid = 1
		}, coverage.ErrInvalidBranchCount},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := good()
			c.mut(p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate: expected error, got nil")
			}
			if !errors.Is(err, c.want) {
				t.Errorf("Validate err = %v, want errors.Is %v", err, c.want)
			}
		})
	}
}

func TestHydrate_EmitsOnlyCanonicalKinds(t *testing.T) {
	repoID := mustParseUUID(t, "22222222-2222-2222-2222-222222222222")
	sha := strings.Repeat("b", 40)
	scanRunID := mustParseUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	p, err := coverage.ParseXML([]byte(sampleCoberturaXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	res, _ := newPopulatedResolver(t, repoID, "internal/svc/handler.go", "internal/util/helper.go")
	h := coverage.NewHydrator(res)
	out, err := h.Hydrate(context.Background(), p, scanRunID)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if out.SkippedUnboundScopeCount != 0 {
		t.Errorf("SkippedUnboundScopeCount = %d, want 0", out.SkippedUnboundScopeCount)
	}
	// handler.go has both line+branch coverage; helper.go has line only.
	// Expected rows: handler.go line ratio + branch ratio + helper.go line ratio = 3.
	if len(out.Rows) != 3 {
		t.Fatalf("len(Rows) = %d, want 3 (%+v)", len(out.Rows), out.Rows)
	}
	kinds := map[string]int{}
	for _, r := range out.Rows {
		kinds[r.MetricKind]++
		// Forbidden legacy aliases must NEVER appear.
		if r.MetricKind == "coverage_line" || r.MetricKind == "coverage_branch" {
			t.Errorf("emitted forbidden legacy metric_kind %q (iter-1 evaluator item 4)", r.MetricKind)
		}
		if r.Pack != recipes.PackIngested {
			t.Errorf("Row Pack = %q, want %q", r.Pack, recipes.PackIngested)
		}
		if r.Source != recipes.SourceIngested {
			t.Errorf("Row Source = %q, want %q", r.Source, recipes.SourceIngested)
		}
		if r.MetricVersion != coverage.MetricVersion {
			t.Errorf("Row MetricVersion = %d, want %d", r.MetricVersion, coverage.MetricVersion)
		}
		if r.Scope.Kind != scope.KindFile {
			t.Errorf("Row Scope.Kind = %q, want %q", r.Scope.Kind, scope.KindFile)
		}
		if r.Scope.LocalID != r.ScopeID.String() {
			t.Errorf("Row Scope.LocalID = %q, want ScopeID.String() %q", r.Scope.LocalID, r.ScopeID.String())
		}
		if r.SHA != sha {
			t.Errorf("Row SHA = %q, want %q", r.SHA, sha)
		}
		if r.Value < 0 || r.Value > 1 {
			t.Errorf("Row Value = %f for kind %q, want in [0,1]", r.Value, r.MetricKind)
		}
	}
	if kinds[coverage.MetricKindCoverageLineRatio] != 2 {
		t.Errorf("coverage_line_ratio count = %d, want 2 (one per file)", kinds[coverage.MetricKindCoverageLineRatio])
	}
	if kinds[coverage.MetricKindCoverageBranchRatio] != 1 {
		t.Errorf("coverage_branch_ratio count = %d, want 1 (only handler.go has branches)", kinds[coverage.MetricKindCoverageBranchRatio])
	}
	// Verify ProducerRunID is stamped on every emitted row
	// so the metric_sample.producer_run_id FK is satisfied.
	for i, r := range out.Rows {
		if r.ProducerRunID != scanRunID {
			t.Errorf("Rows[%d].ProducerRunID = %s, want %s", i, r.ProducerRunID, scanRunID)
		}
	}
	// Deterministic ordering: handler.go line, handler.go branch, helper.go line.
	wantOrder := []struct {
		path string
		kind string
	}{
		{"internal/svc/handler.go", coverage.MetricKindCoverageLineRatio},
		{"internal/svc/handler.go", coverage.MetricKindCoverageBranchRatio},
		{"internal/util/helper.go", coverage.MetricKindCoverageLineRatio},
	}
	for i, w := range wantOrder {
		if out.Rows[i].FilePath != w.path || out.Rows[i].MetricKind != w.kind {
			t.Errorf("Rows[%d] = (%q, %q), want (%q, %q)", i, out.Rows[i].FilePath, out.Rows[i].MetricKind, w.path, w.kind)
		}
	}
}

func TestHydrate_SkipsUnboundScopeAndCounts(t *testing.T) {
	repoID := mustParseUUID(t, "33333333-3333-3333-3333-333333333333")
	sha := strings.Repeat("c", 40)
	scanRunID := mustParseUUID(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	p, err := coverage.ParseXML([]byte(sampleCoberturaXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	// Resolver only knows about helper.go; handler.go is unbound.
	res, _ := newPopulatedResolver(t, repoID, "internal/util/helper.go")
	h := coverage.NewHydrator(res)
	out, err := h.Hydrate(context.Background(), p, scanRunID)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if out.SkippedUnboundScopeCount != 1 {
		t.Errorf("SkippedUnboundScopeCount = %d, want 1", out.SkippedUnboundScopeCount)
	}
	if len(out.SkippedUnboundScopeFiles) != 1 || out.SkippedUnboundScopeFiles[0] != "internal/svc/handler.go" {
		t.Errorf("SkippedUnboundScopeFiles = %v, want [internal/svc/handler.go]", out.SkippedUnboundScopeFiles)
	}
	// Only helper.go is emitted (line ratio only).
	if len(out.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1", len(out.Rows))
	}
	if out.Rows[0].FilePath != "internal/util/helper.go" {
		t.Errorf("Row[0].FilePath = %q, want internal/util/helper.go", out.Rows[0].FilePath)
	}
}

func TestHydrate_SuppressesZeroValid(t *testing.T) {
	repoID := mustParseUUID(t, "44444444-4444-4444-4444-444444444444")
	sha := strings.Repeat("d", 40)
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid.NewV4: %v", err)
	}
	res := coverage.NewMapScopeResolver()
	res.Add(repoID, "x.go", id, recipes.ScopeRef{Kind: scope.KindFile, Path: "x.go", QualifiedName: "x.go"})
	res.Add(repoID, "y.go", id, recipes.ScopeRef{Kind: scope.KindFile, Path: "y.go", QualifiedName: "y.go"})
	p := &coverage.Payload{
		RepoID: repoID,
		SHA:    sha,
		Files: []coverage.FileCoverage{
			// Branches-only file (LinesValid == 0).
			{FilePath: "x.go", LinesCovered: 0, LinesValid: 0, BranchesCovered: 1, BranchesValid: 2},
			// Lines-only file (BranchesValid == 0).
			{FilePath: "y.go", LinesCovered: 3, LinesValid: 4, BranchesCovered: 0, BranchesValid: 0},
		},
	}
	h := coverage.NewHydrator(res)
	out, err := h.Hydrate(context.Background(), p, mustParseUUID(t, "cccccccc-cccc-cccc-cccc-cccccccccccc"))
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2 (one branch-only row + one line-only row)", len(out.Rows))
	}
	got := map[string]string{}
	for _, r := range out.Rows {
		got[r.FilePath] = r.MetricKind
	}
	if got["x.go"] != coverage.MetricKindCoverageBranchRatio {
		t.Errorf("x.go emitted %q, want only branch ratio", got["x.go"])
	}
	if got["y.go"] != coverage.MetricKindCoverageLineRatio {
		t.Errorf("y.go emitted %q, want only line ratio", got["y.go"])
	}
}

// faultyResolver returns a transient error so the
// hydrator's abort-on-error path is covered.
type faultyResolver struct{ msg string }

func (f *faultyResolver) ResolveFileScope(_ context.Context, _ uuid.UUID, _ string, _ string) (uuid.UUID, recipes.ScopeRef, bool, error) {
	return uuid.Nil, recipes.ScopeRef{}, false, errors.New(f.msg)
}

func TestHydrate_TransientResolverErrorAborts(t *testing.T) {
	repoID := mustParseUUID(t, "55555555-5555-5555-5555-555555555555")
	sha := strings.Repeat("e", 40)
	p, err := coverage.ParseXML([]byte(sampleCoberturaXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	h := coverage.NewHydrator(&faultyResolver{msg: "pg pool drained"})
	out, err := h.Hydrate(context.Background(), p, mustParseUUID(t, "dddddddd-dddd-dddd-dddd-dddddddddddd"))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, coverage.ErrScopeResolutionFailed) {
		t.Errorf("err = %v, want errors.Is ErrScopeResolutionFailed", err)
	}
	if len(out.Rows) != 0 {
		t.Errorf("partial output produced: %d rows", len(out.Rows))
	}
}

// wrongKindResolver returns the zero scope_id with
// found=true to exercise the zero-UUID defence path.
type wrongKindResolver struct {
	scopeID uuid.UUID
	kind    scope.Kind
}

func (w *wrongKindResolver) ResolveFileScope(_ context.Context, _ uuid.UUID, _ string, p string) (uuid.UUID, recipes.ScopeRef, bool, error) {
	return w.scopeID, recipes.ScopeRef{Kind: w.kind, Path: p, QualifiedName: p}, true, nil
}

func TestHydrate_RejectsZeroScopeIDWithFound(t *testing.T) {
	repoID := mustParseUUID(t, "66666666-6666-6666-6666-666666666666")
	sha := strings.Repeat("f", 40)
	p := &coverage.Payload{
		RepoID: repoID,
		SHA:    sha,
		Files: []coverage.FileCoverage{
			{FilePath: "x.go", LinesCovered: 1, LinesValid: 2},
		},
	}
	h := coverage.NewHydrator(&wrongKindResolver{scopeID: uuid.Nil, kind: scope.KindFile})
	_, err := h.Hydrate(context.Background(), p, mustParseUUID(t, "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, coverage.ErrScopeResolutionFailed) {
		t.Errorf("err = %v, want errors.Is ErrScopeResolutionFailed", err)
	}
}

func TestHydrate_RejectsNonFileScopeKind(t *testing.T) {
	repoID := mustParseUUID(t, "77777777-7777-7777-7777-777777777777")
	sha := strings.Repeat("9", 40)
	p := &coverage.Payload{
		RepoID: repoID,
		SHA:    sha,
		Files: []coverage.FileCoverage{
			{FilePath: "x.go", LinesCovered: 1, LinesValid: 2},
		},
	}
	id, _ := uuid.NewV4()
	h := coverage.NewHydrator(&wrongKindResolver{scopeID: id, kind: scope.KindClass})
	_, err := h.Hydrate(context.Background(), p, mustParseUUID(t, "ffffffff-ffff-ffff-ffff-ffffffffffff"))
	if err == nil || !errors.Is(err, coverage.ErrScopeResolutionFailed) {
		t.Errorf("err = %v, want errors.Is ErrScopeResolutionFailed (non-file kind)", err)
	}
}

func TestParseXML_NormalisesBackslashes(t *testing.T) {
	repoID := mustParseUUID(t, "88888888-8888-8888-8888-888888888888")
	sha := strings.Repeat("a", 40)
	body := `<?xml version="1.0"?><coverage><packages><package><classes><class filename="src\handler.go"><lines><line number="1" hits="1" branch="false"/></lines></class></classes></package></packages></coverage>`
	p, err := coverage.ParseXML([]byte(body), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	if len(p.Files) != 1 || p.Files[0].FilePath != "src/handler.go" {
		t.Errorf("file_path = %q, want %q (backslash should fold to forward slash)", p.Files[0].FilePath, "src/handler.go")
	}
}

func TestParseXML_StripsLeadingDotSlash(t *testing.T) {
	repoID := mustParseUUID(t, "99999999-9999-9999-9999-999999999999")
	sha := strings.Repeat("a", 40)
	body := `<?xml version="1.0"?><coverage><packages><package><classes><class filename="./src/handler.go"><lines><line number="1" hits="1" branch="false"/></lines></class></classes></package></packages></coverage>`
	p, err := coverage.ParseXML([]byte(body), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	if len(p.Files) != 1 || p.Files[0].FilePath != "src/handler.go" {
		t.Errorf("file_path = %q, want %q (leading ./ should strip)", p.Files[0].FilePath, "src/handler.go")
	}
}

func TestParseXML_SortsFilesDeterministically(t *testing.T) {
	repoID := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	sha := strings.Repeat("a", 40)
	body := `<?xml version="1.0"?><coverage><packages>
  <package><classes><class filename="z/last.go"><lines><line number="1" hits="1"/></lines></class></classes></package>
  <package><classes><class filename="a/first.go"><lines><line number="1" hits="1"/></lines></class></classes></package>
  <package><classes><class filename="m/middle.go"><lines><line number="1" hits="1"/></lines></class></classes></package>
</packages></coverage>`
	p, err := coverage.ParseXML([]byte(body), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	got := make([]string, len(p.Files))
	for i, f := range p.Files {
		got[i] = f.FilePath
	}
	want := []string{"a/first.go", "m/middle.go", "z/last.go"}
	if !sort.StringsAreSorted(got) {
		t.Errorf("file order = %v, expected sorted ascending", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Files[%d].FilePath = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseXML_RejectsTrailingContent(t *testing.T) {
	repoID := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name string
		body string
	}{
		{
			name: "trailing element",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename="x.go"><lines><line number="1" hits="1"/></lines></class></classes></package></packages></coverage><extra/>`,
		},
		{
			name: "trailing text",
			body: `<?xml version="1.0"?><coverage><packages><package><classes><class filename="x.go"><lines><line number="1" hits="1"/></lines></class></classes></package></packages></coverage>garbage`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := coverage.ParseXML([]byte(c.body), repoID, sha)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, coverage.ErrTrailingContent) {
				t.Errorf("err = %v, want errors.Is ErrTrailingContent", err)
			}
		})
	}
}

func TestParseXML_AcceptsTrailingWhitespaceAndComments(t *testing.T) {
	repoID := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	sha := strings.Repeat("a", 40)
	body := "<?xml version=\"1.0\"?><coverage><packages><package><classes><class filename=\"x.go\"><lines><line number=\"1\" hits=\"1\"/></lines></class></classes></package></packages></coverage>\n  \n<!-- footer -->\n"
	if _, err := coverage.ParseXML([]byte(body), repoID, sha); err != nil {
		t.Fatalf("ParseXML rejected trailing whitespace/comments: %v", err)
	}
}

func TestExtractRootMetadata_HappyPath(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<coverage repo_id="11111111-1111-1111-1111-111111111111" sha="abcdef0123456789abcdef0123456789abcdef01">
  <packages/>
</coverage>`)
	repoID, sha, err := coverage.ExtractRootMetadata(body)
	if err != nil {
		t.Fatalf("ExtractRootMetadata: %v", err)
	}
	want := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	if repoID != want {
		t.Errorf("repoID = %v, want %v", repoID, want)
	}
	if sha != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Errorf("sha = %q, want abcdef...", sha)
	}
}

func TestExtractRootMetadata_RejectsMissingOrMalformed(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr error
	}{
		{
			name:    "empty body",
			body:    "",
			wantErr: coverage.ErrMalformedXML,
		},
		{
			name:    "wrong root",
			body:    `<?xml version="1.0"?><report repo_id="11111111-1111-1111-1111-111111111111" sha="abcdef0123456789abcdef0123456789abcdef01"/>`,
			wantErr: coverage.ErrMalformedXML,
		},
		{
			name:    "missing repo_id",
			body:    `<?xml version="1.0"?><coverage sha="abcdef0123456789abcdef0123456789abcdef01"/>`,
			wantErr: coverage.ErrEmptyRepoID,
		},
		{
			name:    "missing sha",
			body:    `<?xml version="1.0"?><coverage repo_id="11111111-1111-1111-1111-111111111111"/>`,
			wantErr: coverage.ErrEmptySHA,
		},
		{
			name:    "invalid repo_id",
			body:    `<?xml version="1.0"?><coverage repo_id="not-a-uuid" sha="abcdef0123456789abcdef0123456789abcdef01"/>`,
			wantErr: coverage.ErrInvalidRepoID,
		},
		{
			name:    "zero repo_id",
			body:    `<?xml version="1.0"?><coverage repo_id="00000000-0000-0000-0000-000000000000" sha="abcdef0123456789abcdef0123456789abcdef01"/>`,
			wantErr: coverage.ErrEmptyRepoID,
		},
		{
			name:    "invalid sha",
			body:    `<?xml version="1.0"?><coverage repo_id="11111111-1111-1111-1111-111111111111" sha="not-a-sha"/>`,
			wantErr: coverage.ErrInvalidSHA,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := coverage.ExtractRootMetadata([]byte(c.body))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, c.wantErr) {
				t.Errorf("err = %v, want errors.Is %v", err, c.wantErr)
			}
		})
	}
}

func TestHydrate_RejectsZeroScanRunID(t *testing.T) {
	repoID := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	sha := strings.Repeat("a", 40)
	p, err := coverage.ParseXML([]byte(sampleCoberturaXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	res, _ := newPopulatedResolver(t, repoID, "internal/svc/handler.go", "internal/util/helper.go")
	h := coverage.NewHydrator(res)
	_, err = h.Hydrate(context.Background(), p, uuid.Nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, coverage.ErrZeroScanRunID) {
		t.Errorf("err = %v, want errors.Is ErrZeroScanRunID", err)
	}
}

func TestHydrate_SkipLoggerEmitsOneLinePerSkippedFile(t *testing.T) {
	repoID := mustParseUUID(t, "33333333-3333-3333-3333-333333333333")
	sha := strings.Repeat("a", 40)
	scanRunID := mustParseUUID(t, "12345678-1234-1234-1234-123456781234")
	p, err := coverage.ParseXML([]byte(sampleCoberturaXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	// Resolver only knows about helper.go; handler.go is skipped.
	res, _ := newPopulatedResolver(t, repoID, "internal/util/helper.go")
	var sink testLogSink
	logger := slog.New(&sink)
	h := coverage.NewHydrator(res).WithSkipLogger(logger)
	_, err = h.Hydrate(context.Background(), p, scanRunID)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if sink.count != 1 {
		t.Errorf("skip log emissions = %d, want 1 (handler.go is the only unbound file)", sink.count)
	}
	if !sink.sawEvent {
		t.Errorf("expected at least one log carrying event=%q", coverage.CoverageSkippedUnboundScopeMetric)
	}
}

// testLogSink is a minimal slog.Handler that counts the
// number of records it receives and notes whether any of
// them carried the `coverage_skipped_unbound_scope` event
// attribute.
type testLogSink struct {
	count    int
	sawEvent bool
}

func (s *testLogSink) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (s *testLogSink) Handle(_ context.Context, r slog.Record) error {
	s.count++
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "event" && a.Value.String() == coverage.CoverageSkippedUnboundScopeMetric {
			s.sawEvent = true
		}
		return true
	})
	return nil
}
func (s *testLogSink) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *testLogSink) WithGroup(_ string) slog.Handler      { return s }

func TestToMetricSampleRecords_ProducesOneRecordPerRow(t *testing.T) {
	repoID := mustParseUUID(t, "22222222-2222-2222-2222-222222222222")
	sha := strings.Repeat("b", 40)
	scanRunID := mustParseUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	p, err := coverage.ParseXML([]byte(sampleCoberturaXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	res, _ := newPopulatedResolver(t, repoID, "internal/svc/handler.go", "internal/util/helper.go")
	h := coverage.NewHydrator(res)
	out, err := h.Hydrate(context.Background(), p, scanRunID)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	mintCount := 0
	mint := func() (uuid.UUID, error) {
		mintCount++
		return uuid.NewV4()
	}
	records, err := out.ToMetricSampleRecords(repoID, mint, func(seed coverage.MetricSampleSeed) any {
		return seed
	})
	if err != nil {
		t.Fatalf("ToMetricSampleRecords: %v", err)
	}
	if len(records) != len(out.Rows) {
		t.Errorf("len(records) = %d, want %d (one per HydratedCoverageRow)", len(records), len(out.Rows))
	}
	if mintCount != len(out.Rows) {
		t.Errorf("mint invocations = %d, want %d", mintCount, len(out.Rows))
	}
	for i, raw := range records {
		seed := raw.(coverage.MetricSampleSeed)
		if seed.RepoID != repoID {
			t.Errorf("records[%d].RepoID = %v, want %v", i, seed.RepoID, repoID)
		}
		if seed.ProducerRunID != scanRunID {
			t.Errorf("records[%d].ProducerRunID = %v, want %v", i, seed.ProducerRunID, scanRunID)
		}
		if seed.SHA != sha {
			t.Errorf("records[%d].SHA = %q, want %q", i, seed.SHA, sha)
		}
		if seed.SampleID == uuid.Nil {
			t.Errorf("records[%d].SampleID is the zero UUID (mint must populate)", i)
		}
	}
}
