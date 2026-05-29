package repo_indexer_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/repo_indexer"
)

// fixedRepoID is a stable repo_id literal used across tests.
var fixedRepoID = uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555"))

// otherRepoID is a stable second repo_id literal used to
// exercise the per-repo registered-event invariant.
var otherRepoID = uuid.Must(uuid.FromString("66666666-7777-8888-9999-aaaaaaaaaaaa"))

// validSHA returns a canonical 40-char hex SHA built from
// the repeated byte. Mirrors the test util in
// `internal/ingest/webhook` so both ingest surfaces share
// one canonical SHA-shape contract.
func validSHA(c byte) string {
	return strings.Repeat(string(c), 40)
}

// fixedCommittedAt is the deterministic commit timestamp
// used by the happy-path tests.
func fixedCommittedAt() time.Time {
	return time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
}

// TestIndexer_NewSHAInsertsPending pins the canonical Stage 3.1
// scenario `new-sha-inserts-pending`: a single OnNewSHA call
// for a new SHA INSERTs a `commit` row with
// `scan_status='pending'` AND appends a single
// `repo_event(kind='registered')` row.
//
// Verifies the FOUR canon-critical pieces:
//
//  1. The commit lands.
//  2. The persisted record's ScanStatus equals "pending"
//     (the DB DEFAULT semantic the in-memory fake mirrors).
//  3. Exactly one repo_event lands.
//  4. The event's Kind is the past-tense literal "registered"
//     (NOT "register" / "REGISTERED" / "registered_event").
func TestIndexer_NewSHAInsertsPending(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	req := repo_indexer.CommitEnsureRequest{
		RepoID:      fixedRepoID,
		SHA:         validSHA('a'),
		ParentSHA:   validSHA('b'),
		CommittedAt: fixedCommittedAt(),
		Ref:         "refs/heads/main",
	}

	res, err := idx.OnNewSHA(context.Background(), req)
	if err != nil {
		t.Fatalf("OnNewSHA: %v", err)
	}
	if !res.CommitInserted {
		t.Errorf("CommitInserted=false; want true")
	}
	if !res.EventInserted {
		t.Errorf("EventInserted=false; want true (first SHA for repo registers it)")
	}

	commits := writer.Commits()
	if len(commits) != 1 {
		t.Fatalf("Commits()=%d rows; want 1", len(commits))
	}
	c := commits[0]
	if c.RepoID != fixedRepoID {
		t.Errorf("commit RepoID=%v; want %v", c.RepoID, fixedRepoID)
	}
	if c.SHA != validSHA('a') {
		t.Errorf("commit SHA=%q; want %q", c.SHA, validSHA('a'))
	}
	if c.ParentSHA != validSHA('b') {
		t.Errorf("commit ParentSHA=%q; want %q", c.ParentSHA, validSHA('b'))
	}
	if !c.CommittedAt.Equal(fixedCommittedAt()) {
		t.Errorf("commit CommittedAt=%v; want %v", c.CommittedAt, fixedCommittedAt())
	}
	// THE canonical assertion of Stage 3.1: DB DEFAULT supplies 'pending'.
	if c.ScanStatus != repo_indexer.ScanStatusPending {
		t.Errorf("commit ScanStatus=%q; want %q", c.ScanStatus, repo_indexer.ScanStatusPending)
	}

	events := writer.Events()
	if len(events) != 1 {
		t.Fatalf("Events()=%d; want 1", len(events))
	}
	if events[0].RepoID != fixedRepoID {
		t.Errorf("event RepoID=%v; want %v", events[0].RepoID, fixedRepoID)
	}
	// Past-tense canonical kind per architecture Sec 5.1.4.
	if events[0].Kind != "registered" {
		t.Errorf("event Kind=%q; want %q (architecture Sec 5.1.4 past-tense canon)", events[0].Kind, "registered")
	}
	if events[0].Kind == "register" {
		t.Errorf("event Kind=%q; the present-tense form is forbidden", events[0].Kind)
	}
}

// TestIndexer_DuplicateSHAIsNoOp pins the canonical Stage 3.1
// scenario "duplicate SHA event is a no-op". A second
// OnNewSHA call for the SAME `(repo_id, sha)` returns
// CommitInserted=false and does NOT append a second commit
// row OR a second registered event.
func TestIndexer_DuplicateSHAIsNoOp(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	req := repo_indexer.CommitEnsureRequest{
		RepoID:      fixedRepoID,
		SHA:         validSHA('c'),
		CommittedAt: fixedCommittedAt(),
	}

	res1, err := idx.OnNewSHA(context.Background(), req)
	if err != nil {
		t.Fatalf("first OnNewSHA: %v", err)
	}
	if !res1.CommitInserted {
		t.Fatalf("first call CommitInserted=false; want true")
	}

	// Duplicate delivery.
	res2, err := idx.OnNewSHA(context.Background(), req)
	if err != nil {
		t.Fatalf("second OnNewSHA: %v", err)
	}
	if res2.CommitInserted {
		t.Errorf("duplicate call CommitInserted=true; want false (no-op)")
	}
	if res2.EventInserted {
		t.Errorf("duplicate call EventInserted=true; want false (repo already registered)")
	}

	if got := len(writer.Commits()); got != 1 {
		t.Errorf("Commits=%d; want 1 (duplicate must not append)", got)
	}
	if got := len(writer.Events()); got != 1 {
		t.Errorf("Events=%d; want 1 (duplicate must not append a second registered)", got)
	}
}

// TestIndexer_SecondSHASameRepoDoesNotEmitSecondRegistered
// pins the "exactly one registered event per repo" invariant:
// when a SECOND distinct SHA arrives for an already-known
// repo, the commit row lands but NO new registered event is
// appended.
func TestIndexer_SecondSHASameRepoDoesNotEmitSecondRegistered(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	for _, sha := range []string{validSHA('a'), validSHA('b'), validSHA('c')} {
		req := repo_indexer.CommitEnsureRequest{
			RepoID:      fixedRepoID,
			SHA:         sha,
			CommittedAt: fixedCommittedAt(),
		}
		if _, err := idx.OnNewSHA(context.Background(), req); err != nil {
			t.Fatalf("OnNewSHA(%s): %v", sha[:7], err)
		}
	}

	if got := len(writer.Commits()); got != 3 {
		t.Errorf("Commits=%d; want 3", got)
	}
	if got := len(writer.Events()); got != 1 {
		t.Errorf("Events=%d; want 1 (only the FIRST SHA emits a registered event)", got)
	}
}

// TestIndexer_DifferentReposEachEmitOwnRegistered pins the
// per-repo isolation of the registered-event invariant: two
// distinct repos each get their own `registered` event.
func TestIndexer_DifferentReposEachEmitOwnRegistered(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	for _, repoID := range []uuid.UUID{fixedRepoID, otherRepoID} {
		req := repo_indexer.CommitEnsureRequest{
			RepoID:      repoID,
			SHA:         validSHA('a'),
			CommittedAt: fixedCommittedAt(),
		}
		res, err := idx.OnNewSHA(context.Background(), req)
		if err != nil {
			t.Fatalf("OnNewSHA(%v): %v", repoID, err)
		}
		if !res.EventInserted {
			t.Errorf("repo %v EventInserted=false; want true (first SHA per repo)", repoID)
		}
	}

	events := writer.Events()
	if len(events) != 2 {
		t.Fatalf("Events=%d; want 2", len(events))
	}
	seenRepos := make(map[uuid.UUID]bool)
	for _, e := range events {
		if e.Kind != "registered" {
			t.Errorf("event kind=%q; want %q", e.Kind, "registered")
		}
		seenRepos[e.RepoID] = true
	}
	if !seenRepos[fixedRepoID] || !seenRepos[otherRepoID] {
		t.Errorf("registered events did not cover both repos: %v", seenRepos)
	}
}

// TestIndexer_ValidationRejectsZeroRepoID pins the structural
// guard.
func TestIndexer_ValidationRejectsZeroRepoID(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	req := repo_indexer.CommitEnsureRequest{
		SHA:         validSHA('a'),
		CommittedAt: fixedCommittedAt(),
	}
	_, err := idx.OnNewSHA(context.Background(), req)
	if !errors.Is(err, repo_indexer.ErrZeroRepoID) {
		t.Errorf("err=%v; want ErrZeroRepoID", err)
	}
	if got := len(writer.Commits()); got != 0 {
		t.Errorf("Commits=%d; want 0 (validation should fail before write)", got)
	}
}

// TestIndexer_ValidationRejectsEmptySHA pins the structural
// guard.
func TestIndexer_ValidationRejectsEmptySHA(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	req := repo_indexer.CommitEnsureRequest{
		RepoID:      fixedRepoID,
		CommittedAt: fixedCommittedAt(),
	}
	_, err := idx.OnNewSHA(context.Background(), req)
	if !errors.Is(err, repo_indexer.ErrEmptySHA) {
		t.Errorf("err=%v; want ErrEmptySHA", err)
	}
}

// TestIndexer_ValidationRejectsInvalidSHA pins the strict
// 40-char hex shape (mirrors the churn validator).
func TestIndexer_ValidationRejectsInvalidSHA(t *testing.T) {
	cases := []string{
		"short",
		strings.Repeat("z", 40), // non-hex
		strings.Repeat("a", 39), // truncated
		strings.Repeat("a", 41), // too long
		" " + strings.Repeat("a", 39),
	}
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	for _, sha := range cases {
		req := repo_indexer.CommitEnsureRequest{
			RepoID:      fixedRepoID,
			SHA:         sha,
			CommittedAt: fixedCommittedAt(),
		}
		_, err := idx.OnNewSHA(context.Background(), req)
		if err == nil {
			t.Errorf("SHA=%q: err=nil; want non-nil validation failure", sha)
			continue
		}
		// Whitespace-or-empty SHA surfaces as EmptySHA; everything else as InvalidSHA.
		if !errors.Is(err, repo_indexer.ErrInvalidSHA) && !errors.Is(err, repo_indexer.ErrEmptySHA) {
			t.Errorf("SHA=%q: err=%v; want ErrInvalidSHA or ErrEmptySHA", sha, err)
		}
	}
}

// TestIndexer_ValidationRejectsInvalidParentSHA mirrors the
// SHA guard for the optional ParentSHA: empty is OK (first
// commit of a repo), non-empty-but-malformed is rejected.
func TestIndexer_ValidationRejectsInvalidParentSHA(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	// Empty ParentSHA is permitted.
	good := repo_indexer.CommitEnsureRequest{
		RepoID:      fixedRepoID,
		SHA:         validSHA('a'),
		ParentSHA:   "",
		CommittedAt: fixedCommittedAt(),
	}
	if _, err := idx.OnNewSHA(context.Background(), good); err != nil {
		t.Fatalf("empty ParentSHA should succeed; got %v", err)
	}

	// Non-empty + non-hex is rejected.
	bad := repo_indexer.CommitEnsureRequest{
		RepoID:      fixedRepoID,
		SHA:         validSHA('b'),
		ParentSHA:   "not-a-sha",
		CommittedAt: fixedCommittedAt(),
	}
	_, err := idx.OnNewSHA(context.Background(), bad)
	if !errors.Is(err, repo_indexer.ErrInvalidParentSHA) {
		t.Errorf("bad ParentSHA: err=%v; want ErrInvalidParentSHA", err)
	}
}

// TestIndexer_ValidationRejectsZeroCommittedAt pins the
// non-nullable timestamp guard.
func TestIndexer_ValidationRejectsZeroCommittedAt(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	req := repo_indexer.CommitEnsureRequest{
		RepoID: fixedRepoID,
		SHA:    validSHA('a'),
	}
	_, err := idx.OnNewSHA(context.Background(), req)
	if !errors.Is(err, repo_indexer.ErrZeroCommittedAt) {
		t.Errorf("err=%v; want ErrZeroCommittedAt", err)
	}
}

// TestIndexer_WriterErrorWrapsCatalogWriterFailure pins the
// error-wrapping contract: a writer-side error surfaces as
// [ErrCatalogWriterFailure] so the HTTP layer can map it to
// 500.
func TestIndexer_WriterErrorWrapsCatalogWriterFailure(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	sentinel := errors.New("simulated writer error")
	writer.FailNext(sentinel)

	req := repo_indexer.CommitEnsureRequest{
		RepoID:      fixedRepoID,
		SHA:         validSHA('a'),
		CommittedAt: fixedCommittedAt(),
	}
	_, err := idx.OnNewSHA(context.Background(), req)
	if !errors.Is(err, repo_indexer.ErrCatalogWriterFailure) {
		t.Errorf("err=%v; want wrapped ErrCatalogWriterFailure", err)
	}
}

// TestNewIndexer_PanicsOnNilWriter pins the composition-root
// safety check.
func TestNewIndexer_PanicsOnNilWriter(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewIndexer(nil, nil) did not panic")
		}
	}()
	_ = repo_indexer.NewIndexer(nil, nil)
}

// TestInMemoryCatalogWriter_ConcurrentDeliveriesLinearise
// asserts the writer's atomic guarantee under concurrent
// access: two goroutines POSTing the SAME first-commit
// request MUST produce exactly one commit row AND exactly
// one registered event between them.
//
// The fake's internal mutex serialises; the assertion is
// that the SECOND goroutine observes the first's state
// (CommitInserted=false, EventInserted=false).
func TestInMemoryCatalogWriter_ConcurrentDeliveriesLinearise(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	req := repo_indexer.CommitEnsureRequest{
		RepoID:      fixedRepoID,
		SHA:         validSHA('a'),
		CommittedAt: fixedCommittedAt(),
	}

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make([]repo_indexer.CommitEnsureResult, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func(idx int, ind *repo_indexer.Indexer) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = ind.OnNewSHA(context.Background(), req)
		}(i, idx)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	// Across all goroutines, EXACTLY one must report
	// CommitInserted=true and EXACTLY one must report
	// EventInserted=true (and they should be the SAME
	// goroutine -- the writer ensures both happen
	// atomically).
	inserts, events := 0, 0
	for _, r := range results {
		if r.CommitInserted {
			inserts++
		}
		if r.EventInserted {
			events++
		}
	}
	if inserts != 1 {
		t.Errorf("CommitInserted count = %d; want 1", inserts)
	}
	if events != 1 {
		t.Errorf("EventInserted count = %d; want 1", events)
	}
	if got := len(writer.Commits()); got != 1 {
		t.Errorf("Commits=%d; want 1", got)
	}
	if got := len(writer.Events()); got != 1 {
		t.Errorf("Events=%d; want 1 (concurrent first-commit deliveries must emit exactly one registered event)", got)
	}
}

// TestIndexer_NeverEmitsRegisterPastTenseCanon is the
// canon-guard test pinned by the workstream brief: the kind
// literal MUST be `registered` (past-tense), NEVER
// `register`. A targeted assertion lives here so a `grep -F
// "register"` audit lands one definition site.
func TestIndexer_NeverEmitsRegisterPastTenseCanon(t *testing.T) {
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	req := repo_indexer.CommitEnsureRequest{
		RepoID:      fixedRepoID,
		SHA:         validSHA('a'),
		CommittedAt: fixedCommittedAt(),
	}
	if _, err := idx.OnNewSHA(context.Background(), req); err != nil {
		t.Fatalf("OnNewSHA: %v", err)
	}
	for _, e := range writer.Events() {
		if e.Kind == "register" {
			t.Errorf("event kind=%q; the present-tense form is forbidden (architecture Sec 5.1.4 past-tense canon)", e.Kind)
		}
		if e.Kind != "registered" {
			t.Errorf("event kind=%q; want %q", e.Kind, "registered")
		}
	}
}
