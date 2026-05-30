package scopebinding_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
)

// TestMintScopeID_Deterministic locks in the CLEAN-CODE arch
// G2 stability guarantee: two invocations against the same
// `(repoID, scope_kind, canonical_signature, first_seen_sha)`
// tuple MUST produce identical UUID bytes. The e2e Phase 1
// scenario "ScopeID stability across re-runs" requires the
// same.
func TestMintScopeID_Deterministic(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	first := scopebinding.MintScopeID(repoID, "class", "pkg.Foo", "abcd1234")
	second := scopebinding.MintScopeID(repoID, "class", "pkg.Foo", "abcd1234")
	if first != second {
		t.Fatalf("MintScopeID is non-deterministic: %s != %s", first, second)
	}
	if first == uuid.Nil {
		t.Fatalf("MintScopeID returned uuid.Nil")
	}
	if got := first[6] >> 4; got != 0x5 {
		t.Fatalf("MintScopeID returned a non-v5 UUID (version nibble %#x)", got)
	}
}

// TestMintScopeID_DistinctOnAnyDiff exercises the four input
// dimensions: changing repoID, kind, signature, OR first-seen
// SHA MUST change the resulting UUID. Otherwise the G2
// invariant ("the same logical scope produces the same id")
// is violated in either direction.
func TestMintScopeID_DistinctOnAnyDiff(t *testing.T) {
	t.Parallel()
	repoA := uuid.Must(uuid.NewV4())
	repoB := uuid.Must(uuid.NewV4())
	base := scopebinding.MintScopeID(repoA, "class", "pkg.Foo", "abcd1234")
	dim := map[string]uuid.UUID{
		"different repoID":    scopebinding.MintScopeID(repoB, "class", "pkg.Foo", "abcd1234"),
		"different kind":      scopebinding.MintScopeID(repoA, "method", "pkg.Foo", "abcd1234"),
		"different signature": scopebinding.MintScopeID(repoA, "class", "pkg.Bar", "abcd1234"),
		"different SHA":       scopebinding.MintScopeID(repoA, "class", "pkg.Foo", "deadbeef"),
	}
	for name, id := range dim {
		if id == base {
			t.Errorf("changing %s did not change the scope id (base=%s)", name, base)
		}
	}
}

// TestMintScopeID_PanicsOnZeroRepoID exercises the guard
// against the most common upstream bug: forgetting to mint
// `RepoID` before building bindings. The orchestrator must
// have constructed a non-zero RepoID via
// [repocontext.MintRepoID] before any scope_id is derived.
func TestMintScopeID_PanicsOnZeroRepoID(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on zero repoID, got none")
		}
	}()
	scopebinding.MintScopeID(uuid.Nil, "class", "pkg.Foo", "abcd1234")
}

// TestTryMintScopeID_PropagatesError gives orchestrator code
// a soft-fail path so it can surface a `WalkSkip` instead of
// aborting the whole run.
func TestTryMintScopeID_PropagatesError(t *testing.T) {
	t.Parallel()
	_, err := scopebinding.TryMintScopeID(uuid.Nil, "class", "pkg.Foo", "abcd1234")
	if err == nil {
		t.Fatalf("expected error on zero repoID, got nil")
	}
}

// TestTable_RoundTrip mirrors the e2e Phase 1 "Insert then
// Get returns the same row" scenario.
func TestTable_RoundTrip(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	id := scopebinding.MintScopeID(repoID, "class", "pkg.Foo", "abcd1234")
	in := scopebinding.ScopeBinding{
		ScopeID:   id,
		ScopeKind: "class",
		Signature: "pkg.Foo",
		FilePath:  "pkg/foo.go",
		StartLine: 10,
		EndLine:   200,
		Language:  "go",
	}
	table := scopebinding.NewTable()
	if err := table.Insert(in); err != nil {
		t.Fatalf("Insert returned unexpected error: %v", err)
	}
	out, ok := table.Get(id)
	if !ok {
		t.Fatalf("Get(X) returned ok=false after Insert(X)")
	}
	if out != in {
		t.Fatalf("Get(X) returned %#v; want %#v", out, in)
	}
}

// TestTable_MissingReturnsZero exercises the lookup-miss
// path; the prompt emitter's fail-closed check relies on
// `ok=false` rather than a zero-but-present row.
func TestTable_MissingReturnsZero(t *testing.T) {
	t.Parallel()
	table := scopebinding.NewTable()
	got, ok := table.Get(uuid.Must(uuid.NewV4()))
	if ok {
		t.Fatalf("Get on empty table reported ok=true")
	}
	if got != (scopebinding.ScopeBinding{}) {
		t.Fatalf("missing lookup returned non-zero struct: %#v", got)
	}
}

// TestTable_InsertZeroScopeIDReturnsError guards against the
// most common programmer bug: forgetting to mint the scope id
// before insert. A zero ScopeID is a wiring bug -- Insert
// MUST surface it as a caller-visible [scopebinding.ErrZeroScopeID]
// error and MUST NOT mutate the table; the orchestrator can
// then either log the diagnostic (best-effort mode) or fail
// loud (strict mode) rather than silently dropping a stray
// binding.
func TestTable_InsertZeroScopeIDReturnsError(t *testing.T) {
	t.Parallel()
	table := scopebinding.NewTable()
	err := table.Insert(scopebinding.ScopeBinding{ScopeID: uuid.Nil, Signature: "stray"})
	if err == nil {
		t.Fatalf("expected error on zero ScopeID insert; got nil")
	}
	if !errors.Is(err, scopebinding.ErrZeroScopeID) {
		t.Fatalf("expected errors.Is(err, ErrZeroScopeID); got err=%v", err)
	}
	if got := table.Len(); got != 0 {
		t.Fatalf("zero ScopeID insert mutated the table; Len()=%d (want 0)", got)
	}
	if _, ok := table.Get(uuid.Nil); ok {
		t.Fatalf("zero ScopeID lookup returned ok=true after rejected insert")
	}
}

// TestTable_ConcurrentInsert exercises the sync.Map backing
// store under the orchestrator's GOMAXPROCS fan-out pattern.
// Race detector should not fire (CI runs `go test -race`).
func TestTable_ConcurrentInsert(t *testing.T) {
	t.Parallel()
	table := scopebinding.NewTable()
	const N = 32
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, N)
	for i := 0; i < N; i++ {
		ids[i] = uuid.Must(uuid.NewV4())
	}
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			if err := table.Insert(scopebinding.ScopeBinding{
				ScopeID:   ids[idx],
				ScopeKind: "method",
				Signature: "pkg.Fn",
				FilePath:  "pkg/fn.go",
				StartLine: idx + 1,
				EndLine:   idx + 10,
				Language:  "go",
			}); err != nil {
				t.Errorf("concurrent Insert returned error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := table.Len(); got != N {
		t.Fatalf("Len() after %d concurrent inserts = %d", N, got)
	}
	for i, id := range ids {
		out, ok := table.Get(id)
		if !ok {
			t.Errorf("missing binding %d", i)
			continue
		}
		if out.StartLine != i+1 {
			t.Errorf("binding %d: StartLine=%d; want %d", i, out.StartLine, i+1)
		}
	}
}

// TestTable_LastWriteWins guards the documented overwrite
// semantics. Re-inserting the same `ScopeID` MUST not
// duplicate the entry, and the later struct MUST be the
// one returned by Get.
func TestTable_LastWriteWins(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	id := scopebinding.MintScopeID(repoID, "method", "pkg.Fn", "abcd1234")
	table := scopebinding.NewTable()
	if err := table.Insert(scopebinding.ScopeBinding{ScopeID: id, EndLine: 10}); err != nil {
		t.Fatalf("first Insert returned error: %v", err)
	}
	if err := table.Insert(scopebinding.ScopeBinding{ScopeID: id, EndLine: 25}); err != nil {
		t.Fatalf("second Insert returned error: %v", err)
	}
	if got := table.Len(); got != 1 {
		t.Fatalf("Len() after two inserts of same id = %d; want 1", got)
	}
	out, _ := table.Get(id)
	if out.EndLine != 25 {
		t.Fatalf("last-write-wins not honoured; EndLine=%d (want 25)", out.EndLine)
	}
}
