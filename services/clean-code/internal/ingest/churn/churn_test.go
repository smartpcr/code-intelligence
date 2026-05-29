package churn_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// fixedRepoID is the canonical fixture repo_id. Pinned as a
// `time-uuid` literal so the tests are deterministic across
// CI runs.
var fixedRepoID = uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555"))

func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.FromString(s)
	if err != nil {
		t.Fatalf("FromString(%q): %v", s, err)
	}
	return u
}

func newResolverWithFiles(t *testing.T, files map[string]string) *churn.MapScopeResolver {
	t.Helper()
	r := churn.NewMapScopeResolver()
	for path, scopeIDStr := range files {
		sid := mustParseUUID(t, scopeIDStr)
		r.Add(fixedRepoID, path, sid, recipes.ScopeRef{
			LocalID:       sid.String(),
			Kind:          scope.KindFile,
			QualifiedName: path,
			Path:          path,
		})
	}
	return r
}

// helper for "now"-bound dates.
func now() time.Time {
	return time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
}

// TestPayloadValidate_RejectsEmptyRepoID -- a zero repo_id at
// the payload boundary is always an uninitialised caller
// value; surface it as a structured error so the HTTP handler
// stage can map to 400.
func TestPayloadValidate_RejectsEmptyRepoID(t *testing.T) {
	t.Parallel()
	p := &churn.Payload{
		RepoID: uuid.Nil,
		Rows:   []churn.PayloadRow{{SHA: "1111111111111111111111111111111111111111", FilePath: "a.go", ModifiedAt: now()}},
	}
	err := p.Validate()
	if !errors.Is(err, churn.ErrEmptyRepoID) {
		t.Fatalf("Validate() = %v, want errors.Is ErrEmptyRepoID", err)
	}
}

// TestPayloadValidate_RejectsEmptyRows -- a 0-row payload is
// a no-op; surface so the publisher knows.
func TestPayloadValidate_RejectsEmptyRows(t *testing.T) {
	t.Parallel()
	p := &churn.Payload{RepoID: fixedRepoID, Rows: nil}
	if err := p.Validate(); !errors.Is(err, churn.ErrEmptyRows) {
		t.Fatalf("Validate() = %v, want errors.Is ErrEmptyRows", err)
	}
	p2 := &churn.Payload{RepoID: fixedRepoID, Rows: []churn.PayloadRow{}}
	if err := p2.Validate(); !errors.Is(err, churn.ErrEmptyRows) {
		t.Fatalf("Validate() = %v, want errors.Is ErrEmptyRows", err)
	}
}

// TestPayloadValidate_RejectsEmptySHA -- per-row SHA contract:
// every row carries its own commit identity (arch Sec 4.4 line
// 781).
func TestPayloadValidate_RejectsEmptySHA(t *testing.T) {
	t.Parallel()
	cases := []string{"", "   ", "\t"}
	for _, s := range cases {
		s := s
		t.Run(strings.Replace(s, " ", "_", -1)+"_blank", func(t *testing.T) {
			t.Parallel()
			p := &churn.Payload{
				RepoID: fixedRepoID,
				Rows:   []churn.PayloadRow{{SHA: s, FilePath: "a.go", ModifiedAt: now()}},
			}
			if err := p.Validate(); !errors.Is(err, churn.ErrEmptySHA) {
				t.Fatalf("Validate() = %v, want errors.Is ErrEmptySHA", err)
			}
		})
	}
}

// TestPayloadValidate_RejectsEmptyFilePath -- the hydrator
// resolves file_path to a durable scope_id; an empty path is
// unresolvable.
func TestPayloadValidate_RejectsEmptyFilePath(t *testing.T) {
	t.Parallel()
	p := &churn.Payload{
		RepoID: fixedRepoID,
		Rows:   []churn.PayloadRow{{SHA: "1111111111111111111111111111111111111111", FilePath: "", ModifiedAt: now()}},
	}
	if err := p.Validate(); !errors.Is(err, churn.ErrEmptyFilePath) {
		t.Fatalf("Validate() = %v, want errors.Is ErrEmptyFilePath", err)
	}
}

// TestPayloadValidate_RejectsZeroModifiedAt -- the
// materialiser's window math cannot bucket a zero timestamp.
func TestPayloadValidate_RejectsZeroModifiedAt(t *testing.T) {
	t.Parallel()
	p := &churn.Payload{
		RepoID: fixedRepoID,
		Rows:   []churn.PayloadRow{{SHA: "1111111111111111111111111111111111111111", FilePath: "a.go", ModifiedAt: time.Time{}}},
	}
	if err := p.Validate(); !errors.Is(err, churn.ErrZeroModifiedAt) {
		t.Fatalf("Validate() = %v, want errors.Is ErrZeroModifiedAt", err)
	}
}

// TestPayloadValidate_AcceptsFutureDates -- per rubber-duck
// review: payload validator does NOT reject future-dated
// rows; the materialiser's window math drops them downstream
// (clock-skew defence). A strict-mode caller can filter at a
// higher layer.
func TestPayloadValidate_AcceptsFutureDates(t *testing.T) {
	t.Parallel()
	p := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{{
			SHA:        "1111111111111111111111111111111111111111",
			FilePath:   "a.go",
			ModifiedAt: now().Add(365 * 24 * time.Hour),
		}},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() rejected a future date: %v (the materialiser MUST handle clock-skew, not the payload validator)", err)
	}
}

// TestPayloadValidate_RejectsNilReceiver -- a nil Payload
// pointer should be a structured error, not a nil-deref panic.
func TestPayloadValidate_RejectsNilReceiver(t *testing.T) {
	t.Parallel()
	var p *churn.Payload
	if err := p.Validate(); err == nil {
		t.Fatalf("nil receiver: Validate() = nil, want error")
	}
}

// TestPayload_JSONRoundTrip -- the wire-format JSON tags on
// [PayloadRow] match the publisher contract (snake_case field
// names: `sha`, `file_path`, `modified_at`, `author`).
func TestPayload_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "1111111111111111111111111111111111111111", FilePath: "internal/foo.go", ModifiedAt: now(), Author: "alice@example.com"},
			{SHA: "2222222222222222222222222222222222222222", FilePath: "internal/bar.go", ModifiedAt: now().Add(-24 * time.Hour)},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(b)
	mustContain := []string{`"repo_id":"11111111-2222-3333-4444-555555555555"`, `"sha":"1111111111111111111111111111111111111111"`, `"file_path":"internal/foo.go"`, `"modified_at":"2026-05-24T12:00:00Z"`, `"author":"alice@example.com"`}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("JSON output missing %q; got %s", s, got)
		}
	}
	// Author omitempty.
	if strings.Contains(got, `"sha":"2222222222222222222222222222222222222222"`) && strings.Contains(got, `"author":""`) {
		t.Errorf("JSON output for sha2 carries `\"author\":\"\"`; want omitempty")
	}
	// Round-trip.
	var out churn.Payload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.RepoID != in.RepoID {
		t.Errorf("RepoID round-trip: got %v, want %v", out.RepoID, in.RepoID)
	}
	if len(out.Rows) != 2 || out.Rows[0].SHA != "1111111111111111111111111111111111111111" || out.Rows[1].FilePath != "internal/bar.go" {
		t.Errorf("Rows round-trip lost data: %+v", out.Rows)
	}
}

// TestHydrate_HappyPath -- 3 payload rows produce 3 hydrated
// rows in payload order, each carrying its own per-row SHA
// and resolving to the correct file-scope `scope_id`.
func TestHydrate_HappyPath(t *testing.T) {
	t.Parallel()
	fooID := "aaaaaaaa-0000-0000-0000-000000000001"
	barID := "aaaaaaaa-0000-0000-0000-000000000002"
	resolver := newResolverWithFiles(t, map[string]string{
		"internal/foo.go": fooID,
		"internal/bar.go": barID,
	})
	h := churn.NewHydrator(resolver)

	p := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: now().Add(-24 * time.Hour)},
			{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FilePath: "internal/bar.go", ModifiedAt: now().Add(-48 * time.Hour)},
			{SHA: "cccccccccccccccccccccccccccccccccccccccc", FilePath: "internal/foo.go", ModifiedAt: now().Add(-72 * time.Hour)},
		},
	}
	got, err := h.Hydrate(context.Background(), p)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	// Order preserved.
	wantPath := []string{"internal/foo.go", "internal/bar.go", "internal/foo.go"}
	wantSHA := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccccccccccc"}
	for i, hr := range got {
		if hr.Row.Scope.Path != wantPath[i] {
			t.Errorf("row %d: Path = %q, want %q", i, hr.Row.Scope.Path, wantPath[i])
		}
		if hr.Row.SHA != wantSHA[i] {
			t.Errorf("row %d: SHA = %q, want %q", i, hr.Row.SHA, wantSHA[i])
		}
		if hr.Row.Scope.Kind != scope.KindFile {
			t.Errorf("row %d: Kind = %q, want %q (Stage 2.6 file-scope only)", i, hr.Row.Scope.Kind, scope.KindFile)
		}
		if hr.ScopeID == uuid.Nil {
			t.Errorf("row %d: ScopeID is the zero UUID", i)
		}
		// ScopeKey is the durable UUID string.
		if hr.Row.ScopeKey != hr.ScopeID.String() {
			t.Errorf("row %d: ScopeKey = %q, want scope_id UUID string %q", i, hr.Row.ScopeKey, hr.ScopeID.String())
		}
		// Modified time normalised to UTC.
		if hr.Row.ModifiedAt.Location() != time.UTC {
			t.Errorf("row %d: ModifiedAt.Location() = %v, want UTC", i, hr.Row.ModifiedAt.Location())
		}
	}
	// Two foo.go rows resolve to the SAME scope_id (so the
	// materialiser groups them).
	if got[0].ScopeID != got[2].ScopeID {
		t.Errorf("foo.go rows 0 + 2 have different ScopeIDs (%v vs %v); same file MUST resolve to same scope_id",
			got[0].ScopeID, got[2].ScopeID)
	}
	if got[0].ScopeID == got[1].ScopeID {
		t.Errorf("different files (foo.go vs bar.go) have the SAME ScopeID; resolver fixture broken")
	}
}

// TestHydrate_ScopeResolverErrorIsWrappedAndStopsHydration --
// a single unresolvable row aborts the entire hydrate
// (writer-ownership: no partial output).
func TestHydrate_ScopeResolverErrorIsWrappedAndStopsHydration(t *testing.T) {
	t.Parallel()
	resolver := newResolverWithFiles(t, map[string]string{
		"internal/foo.go": "aaaaaaaa-0000-0000-0000-000000000001",
		// bar.go intentionally NOT registered.
	})
	h := churn.NewHydrator(resolver)
	p := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: now()},
			{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FilePath: "internal/bar.go", ModifiedAt: now()},
		},
	}
	got, err := h.Hydrate(context.Background(), p)
	if err == nil {
		t.Fatalf("Hydrate succeeded with unresolvable row; want error")
	}
	if !errors.Is(err, churn.ErrScopeResolutionFailed) {
		t.Errorf("err = %v, want errors.Is ErrScopeResolutionFailed", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil on hydrate error (writer-ownership: no partial output)", got)
	}
}

// TestHydrate_RejectsZeroScopeID -- a resolver that returns
// the zero UUID is a fixture bug; surface loudly.
func TestHydrate_RejectsZeroScopeID(t *testing.T) {
	t.Parallel()
	resolver := churn.NewMapScopeResolver()
	resolver.Add(fixedRepoID, "internal/foo.go", uuid.Nil, recipes.ScopeRef{
		LocalID:       "x",
		Kind:          scope.KindFile,
		QualifiedName: "internal/foo.go",
		Path:          "internal/foo.go",
	})
	h := churn.NewHydrator(resolver)
	_, err := h.Hydrate(context.Background(), &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: now()},
		},
	})
	if err == nil || !errors.Is(err, churn.ErrScopeResolutionFailed) {
		t.Fatalf("Hydrate accepted zero ScopeID; err = %v", err)
	}
	if !strings.Contains(err.Error(), "zero UUID") {
		t.Errorf("err message missing `zero UUID`; got %v", err)
	}
}

// TestHydrate_RejectsNonFileScopeKind -- the resolver MUST
// return scope_kind='file' at Stage 2.6. A non-file kind is
// rejected because AST-line-attribution (the method-scope
// prerequisite) is not yet built.
func TestHydrate_RejectsNonFileScopeKind(t *testing.T) {
	t.Parallel()
	resolver := churn.NewMapScopeResolver()
	resolver.Add(fixedRepoID, "internal/foo.go",
		mustParseUUID(t, "aaaaaaaa-0000-0000-0000-000000000001"),
		recipes.ScopeRef{
			LocalID:       "x",
			Kind:          scope.KindMethod, // <- wrong for Stage 2.6
			QualifiedName: "pkg.Foo.bar",
			Path:          "internal/foo.go",
		})
	h := churn.NewHydrator(resolver)
	_, err := h.Hydrate(context.Background(), &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: now()},
		},
	})
	if err == nil || !errors.Is(err, churn.ErrScopeResolutionFailed) {
		t.Fatalf("Hydrate accepted non-file kind; err = %v", err)
	}
	if !strings.Contains(err.Error(), "file-scope rows only") {
		t.Errorf("err message missing Stage 2.6 file-only guard; got %v", err)
	}
}

// TestNewHydrator_PanicsOnNilResolver -- composition-root
// wiring bug should fail loudly.
func TestNewHydrator_PanicsOnNilResolver(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewHydrator(nil): want panic, got nil")
		}
	}()
	_ = churn.NewHydrator(nil)
}

// TestHydrate_PropagatesValidationError -- an invalid payload
// surfaces the validator's structured error verbatim (no
// resolver call).
func TestHydrate_PropagatesValidationError(t *testing.T) {
	t.Parallel()
	called := false
	resolver := failResolver{onCall: func() { called = true }}
	h := churn.NewHydrator(resolver)
	_, err := h.Hydrate(context.Background(), &churn.Payload{
		RepoID: uuid.Nil, // <- fails Validate
		Rows:   []churn.PayloadRow{{SHA: "1111111111111111111111111111111111111111", FilePath: "a.go", ModifiedAt: now()}},
	})
	if !errors.Is(err, churn.ErrEmptyRepoID) {
		t.Fatalf("err = %v, want errors.Is ErrEmptyRepoID", err)
	}
	if called {
		t.Errorf("ScopeResolver was called despite payload validation failure")
	}
}

// TestHydrate_OverwritesScopeRefLocalIDWithScopeID -- the
// hydrator is the documented Metric-Ingestor-side rewrite step
// for `recipes.ScopeRef.LocalID` (see ScopeRef Sec "The Metric
// Ingestor rewrites it to a durable scope_id UUID"). The
// Stage 2.6 sweep relies on this invariant to recover the
// scope_id from a materialiser-emitted draft. This test pins
// the invariant: whatever LocalID the resolver returns, the
// hydrated row carries `LocalID == scopeID.String()`.
func TestHydrate_OverwritesScopeRefLocalIDWithScopeID(t *testing.T) {
	t.Parallel()
	sid := mustParseUUID(t, "aaaaaaaa-0000-0000-0000-000000000007")
	resolver := churn.NewMapScopeResolver()
	// Resolver supplies a DIFFERENT LocalID than the scope_id
	// (e.g. a parser placeholder leak). The hydrator MUST
	// rewrite.
	resolver.Add(fixedRepoID, "internal/foo.go", sid, recipes.ScopeRef{
		LocalID:       "local:42", // <- resolver-supplied parser placeholder
		Kind:          scope.KindFile,
		QualifiedName: "internal/foo.go",
		Path:          "internal/foo.go",
	})
	h := churn.NewHydrator(resolver)
	got, err := h.Hydrate(context.Background(), &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: now()},
		},
	})
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Row.Scope.LocalID != sid.String() {
		t.Errorf("Scope.LocalID = %q, want %q (scope_id UUID string -- hydrator MUST rewrite the resolver's parser placeholder)",
			got[0].Row.Scope.LocalID, sid.String())
	}
}

// TestRows_ExtractsChurnRows -- the convenience helper that
// the Metric Ingestor's churn-sweep uses to feed the
// materialiser.
func TestRows_ExtractsChurnRows(t *testing.T) {
	t.Parallel()
	resolver := newResolverWithFiles(t, map[string]string{
		"a.go": "aaaaaaaa-0000-0000-0000-000000000001",
		"b.go": "aaaaaaaa-0000-0000-0000-000000000002",
	})
	h := churn.NewHydrator(resolver)
	p := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "a.go", ModifiedAt: now()},
			{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FilePath: "b.go", ModifiedAt: now()},
		},
	}
	hydrated, err := h.Hydrate(context.Background(), p)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	rows := churn.Rows(hydrated)
	if len(rows) != 2 {
		t.Fatalf("Rows() len = %d, want 2", len(rows))
	}
	if rows[0].SHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || rows[1].SHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("Rows() order/values: %+v", rows)
	}
}

// TestScopeIDByKey_BuildsLookup -- the join helper produces
// `scope_key -> scope_id` so the sweep can stamp the durable
// UUID on each emitted draft.
func TestScopeIDByKey_BuildsLookup(t *testing.T) {
	t.Parallel()
	fooID := "aaaaaaaa-0000-0000-0000-000000000001"
	barID := "aaaaaaaa-0000-0000-0000-000000000002"
	resolver := newResolverWithFiles(t, map[string]string{
		"a.go": fooID,
		"b.go": barID,
	})
	h := churn.NewHydrator(resolver)
	hydrated, err := h.Hydrate(context.Background(), &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "a.go", ModifiedAt: now()},
			{SHA: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", FilePath: "a.go", ModifiedAt: now().Add(-time.Hour)},
			{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FilePath: "b.go", ModifiedAt: now()},
		},
	})
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	lookup := churn.ScopeIDByKey(hydrated)
	if len(lookup) != 2 {
		t.Fatalf("len(lookup) = %d, want 2 (same file -> single bucket)", len(lookup))
	}
	if lookup[fooID].String() != fooID {
		t.Errorf("lookup[%q] = %v, want %v", fooID, lookup[fooID], fooID)
	}
	if lookup[barID].String() != barID {
		t.Errorf("lookup[%q] = %v, want %v", barID, lookup[barID], barID)
	}
}

// TestScanRunKindExternalPerRow_Canon -- the Metric Ingestor
// pins this literal; a future refactor that bumps the spelling
// must update both sides at once.
func TestScanRunKindExternalPerRow_Canon(t *testing.T) {
	t.Parallel()
	if got := churn.ScanRunKindExternalPerRow; got != "external_per_row" {
		t.Errorf("ScanRunKindExternalPerRow = %q, want %q (architecture Sec 4.4 line 782)", got, "external_per_row")
	}
}

// failResolver is a ScopeResolver that records whether it
// was invoked. Used by `TestHydrate_PropagatesValidationError`
// to assert the validator runs BEFORE the resolver.
type failResolver struct {
	onCall func()
}

func (f failResolver) ResolveFile(_ context.Context, _ uuid.UUID, _ string) (uuid.UUID, recipes.ScopeRef, error) {
	if f.onCall != nil {
		f.onCall()
	}
	return uuid.Nil, recipes.ScopeRef{}, errors.New("failResolver: always fails")
}
