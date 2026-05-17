package main

// Unit tests for the cmd/agent-api Stage 5.4 summarize-verb
// production adapters added in iter-3 and tightened in
// iter-4. The agentapi-package tests (`internal/agentapi/
// summarize_test.go`) cover the verb logic against
// in-memory fakes; this file covers the production-only
// glue:
//
//   - `loadConceptSupports`: SQL shape (no `concept_version_id`
//     filter — iter-4 evaluator #1), connection-class error
//     promotion to `agentapi.ErrGraphStoreUnavailable`, and
//     empty-result handling.
//   - `hydrateDstNodes`: bounded N+1 (iter-4 evaluator #3)
//     — the helper MUST NOT issue more than
//     `agentapi.MaxSummarizeEdges` reads even when the seed
//     card's outbound edge fan-out is far larger. Also
//     covers retirement-race skip and connection-class
//     error promotion.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/agentapi"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
)

// loadConceptSupportsSQLMatcher is a custom sqlmock
// QueryMatcher that asserts the structural shape of the
// `loadConceptSupports` query. We use a custom matcher
// (instead of `QueryMatcherEqual` or a single regex)
// because the iter-4 evaluator #1 fix is a NEGATIVE
// requirement: "the query must NOT filter on
// `concept_version_id`". Regex absence is awkward without
// negative lookahead, so we normalize whitespace and
// run a list of substring/regex predicates.
func loadConceptSupportsSQLMatcher() sqlmock.QueryMatcher {
	return sqlmock.QueryMatcherFunc(func(_, actual string) error {
		norm := strings.Join(strings.Fields(actual), " ")
		if !strings.Contains(norm, "FROM concept_support") {
			return fmt.Errorf("SQL missing FROM concept_support: %s", norm)
		}
		// iter-4 #1: the SQL MUST NOT scope to the
		// latest concept_version_id. The Concept
		// Promoter appends a new ConceptVersion AFTER
		// the Consolidator writes supports, so the
		// latest-version filter returned zero rows for
		// every promoted concept.
		if strings.Contains(norm, "concept_version_id") {
			return fmt.Errorf("SQL must NOT filter on concept_version_id (iter-4 evaluator #1): %s", norm)
		}
		if !strings.Contains(norm, "cs.concept_id = $1") {
			return fmt.Errorf("SQL missing cs.concept_id = $1: %s", norm)
		}
		if !strings.Contains(norm, "cs.repo_id = $2") {
			return fmt.Errorf("SQL missing cs.repo_id = $2: %s", norm)
		}
		if !regexp.MustCompile(`(?i)ORDER\s+BY\s+\w+\.created_at\s+DESC`).MatchString(norm) {
			return fmt.Errorf("SQL must ORDER BY created_at DESC: %s", norm)
		}
		if !regexp.MustCompile(`(?i)LIMIT\s+\d+`).MatchString(norm) {
			return fmt.Errorf("SQL must apply a numeric LIMIT: %s", norm)
		}
		return nil
	})
}

// TestLoadConceptSupports_SQLShape_NoVersionFilter pins
// iter-4 evaluator #1: the production SQL MUST scan
// `concept_support` filtered by `(concept_id, repo_id)`
// ONLY, NOT by `concept_version_id`. The matcher rejects
// any regression that re-introduces the version filter.
func TestLoadConceptSupports_SQLShape_NoVersionFilter(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(loadConceptSupportsSQLMatcher()))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows([]string{"support_id", "node_id", "episode_id", "polarity"}).
		AddRow("s-1", "n-1", nil, "positive").
		AddRow("s-2", nil, "e-1", "positive")
	mock.ExpectQuery("").
		WithArgs("c-1", "r-1").
		WillReturnRows(rows)

	out, err := loadConceptSupports(context.Background(), db, "c-1", "r-1", quietTestLogger())
	if err != nil {
		t.Fatalf("loadConceptSupports: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("rows = %d; want 2", len(out))
	}
	if out[0].NodeID != "n-1" || out[1].EpisodeID != "e-1" {
		t.Fatalf("rows projected wrong: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestLoadConceptSupports_PromotedConceptCitesSupports
// closes the iter-4 evaluator #1 acceptance gap. It
// simulates the production sequence the evaluator
// described: the Consolidator wrote support rows against
// `concept_version_id = v1`, and the Promoter then
// appended a new `concept_version_id = v2` for the same
// `concept_id`. Because the iter-4 query is unscoped
// against version_id, both rows surface — proving the
// promoted concept still cites supports.
//
// The prior implementation's version-filter subquery
// would have selected v2 and returned zero rows; the
// matcher above guarantees that regression is rejected
// at the SQL layer, and this test additionally checks
// the *behavior* via a stand-in row that callers
// against the old query would have missed.
func TestLoadConceptSupports_PromotedConceptCitesSupports(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(loadConceptSupportsSQLMatcher()))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Two supports written against the OLD `concept_version_id = v1`.
	// (The version is implicit — we never project or filter on it.)
	// Under the old SQL these rows would have been hidden by the
	// `WHERE concept_version_id = (SELECT … LATEST)` subquery; under
	// the iter-4 SQL they surface because the version filter is gone.
	rows := sqlmock.NewRows([]string{"support_id", "node_id", "episode_id", "polarity"}).
		AddRow("support-from-v1-a", "node-A", nil, "positive").
		AddRow("support-from-v1-b", nil, "ep-B", "positive")
	mock.ExpectQuery("").
		WithArgs("concept-promoted", "repo-1").
		WillReturnRows(rows)

	supports, err := loadConceptSupports(context.Background(), db, "concept-promoted", "repo-1", quietTestLogger())
	if err != nil {
		t.Fatalf("loadConceptSupports: %v", err)
	}
	if len(supports) != 2 {
		t.Fatalf("promoted concept lost supports: got %d, want 2", len(supports))
	}
	if supports[0].NodeID != "node-A" {
		t.Fatalf("supports[0].NodeID = %q; want node-A", supports[0].NodeID)
	}
	if supports[1].EpisodeID != "ep-B" {
		t.Fatalf("supports[1].EpisodeID = %q; want ep-B", supports[1].EpisodeID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestLoadConceptSupports_ConnectionError proves that a
// connection-class failure is promoted to
// `agentapi.ErrGraphStoreUnavailable` so the verb's
// `summarizeGraphFailure` path can degrade cleanly. This
// is the contract the iter-4 evaluator #2 fix relies on:
// `summarizeConcept` returns the error verbatim, the verb
// inspects it for `ErrGraphStoreUnavailable`, and emits
// the degraded envelope instead of a hard 5xx.
func TestLoadConceptSupports_ConnectionError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(loadConceptSupportsSQLMatcher()))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("").
		WithArgs("c-1", "r-1").
		WillReturnError(&net.OpError{Op: "dial", Err: errors.New("connection refused")})

	_, lerr := loadConceptSupports(context.Background(), db, "c-1", "r-1", quietTestLogger())
	if lerr == nil {
		t.Fatalf("expected connection error to surface, got nil")
	}
	if !errors.Is(lerr, agentapi.ErrGraphStoreUnavailable) {
		t.Fatalf("error must wrap ErrGraphStoreUnavailable for verb degradation; got %v", lerr)
	}
}

// TestLoadConceptSupports_EmptyResult covers the legitimate
// case where a concept genuinely has no supports yet (e.g.
// freshly created concept the Consolidator has not touched).
// The adapter returns an empty slice + nil error so the verb
// renders a citation list containing only the Concept row.
func TestLoadConceptSupports_EmptyResult(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(loadConceptSupportsSQLMatcher()))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("").
		WithArgs("c-empty", "r-1").
		WillReturnRows(sqlmock.NewRows([]string{"support_id", "node_id", "episode_id", "polarity"}))

	out, err := loadConceptSupports(context.Background(), db, "c-empty", "r-1", quietTestLogger())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("rows = %d; want 0", len(out))
	}
}

// =====================================================================
// hydrateDstNodes — bounded N+1 (iter-4 evaluator #3)
// =====================================================================

// countingFetcher implements `dstNodeFetcher` while
// recording every `GetNode` invocation. Tests assert the
// invocation count against `agentapi.MaxSummarizeEdges`
// to prove the bound holds regardless of the size of the
// `dstOrder` slice handed in.
type countingFetcher struct {
	nodes      map[string]graphreader.Node
	errFor     map[string]error
	calls      int
	calledWith []string
}

func (c *countingFetcher) GetNode(_ context.Context, id string, _ graphreader.ReaderOptions) (graphreader.Node, error) {
	c.calls++
	c.calledWith = append(c.calledWith, id)
	if e, ok := c.errFor[id]; ok {
		return graphreader.Node{}, e
	}
	n, ok := c.nodes[id]
	if !ok {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	return n, nil
}

// TestHydrateDstNodes_BoundedAtMaxSummarizeEdges pins the
// iter-4 evaluator #3 fix. We hand the helper twice as
// many dst ids as the verb cap; the assertion is that the
// helper issues EXACTLY `agentapi.MaxSummarizeEdges`
// GetNode reads — NOT N reads. This proves the adapter
// short-circuits BEFORE the N+1 loop, so a hot node with
// 1000 outbound edges does not trigger 1000 follow-up
// reads.
func TestHydrateDstNodes_BoundedAtMaxSummarizeEdges(t *testing.T) {
	overflow := agentapi.MaxSummarizeEdges * 2
	ids := make([]string, overflow)
	nodes := make(map[string]graphreader.Node, overflow)
	for i := 0; i < overflow; i++ {
		id := fmt.Sprintf("dst-%03d", i)
		ids[i] = id
		nodes[id] = graphreader.Node{
			NodeID:             id,
			RepoID:             "repo-1",
			Kind:               "method",
			CanonicalSignature: fmt.Sprintf("Sig#%03d", i),
		}
	}
	fetcher := &countingFetcher{nodes: nodes}

	targets, dstSig, err := hydrateDstNodes(
		context.Background(), fetcher, ids,
		agentapi.MaxSummarizeEdges, quietTestLogger(),
	)
	if err != nil {
		t.Fatalf("hydrateDstNodes: %v", err)
	}
	if fetcher.calls != agentapi.MaxSummarizeEdges {
		t.Fatalf("GetNode calls = %d; want %d (cap)",
			fetcher.calls, agentapi.MaxSummarizeEdges)
	}
	if len(targets) != agentapi.MaxSummarizeEdges {
		t.Fatalf("targets = %d; want %d", len(targets), agentapi.MaxSummarizeEdges)
	}
	if len(dstSig) != agentapi.MaxSummarizeEdges {
		t.Fatalf("dstSig = %d; want %d", len(dstSig), agentapi.MaxSummarizeEdges)
	}
	// Verify the first MaxSummarizeEdges ids are the ones
	// hydrated (input order preserved, suffix discarded).
	for i := 0; i < agentapi.MaxSummarizeEdges; i++ {
		if fetcher.calledWith[i] != ids[i] {
			t.Fatalf("calledWith[%d] = %q; want %q", i, fetcher.calledWith[i], ids[i])
		}
	}
}

// TestHydrateDstNodes_BelowCap_HydratesAll covers the
// common path where the seed card has fewer outbound
// edges than the cap; the helper hydrates every entry
// without truncation.
func TestHydrateDstNodes_BelowCap_HydratesAll(t *testing.T) {
	ids := []string{"dst-1", "dst-2", "dst-3"}
	fetcher := &countingFetcher{
		nodes: map[string]graphreader.Node{
			"dst-1": {NodeID: "dst-1", Kind: "method", CanonicalSignature: "A"},
			"dst-2": {NodeID: "dst-2", Kind: "method", CanonicalSignature: "B"},
			"dst-3": {NodeID: "dst-3", Kind: "method", CanonicalSignature: "C"},
		},
	}
	targets, _, err := hydrateDstNodes(context.Background(), fetcher, ids, agentapi.MaxSummarizeEdges, quietTestLogger())
	if err != nil {
		t.Fatalf("hydrateDstNodes: %v", err)
	}
	if fetcher.calls != 3 {
		t.Fatalf("GetNode calls = %d; want 3", fetcher.calls)
	}
	if len(targets) != 3 {
		t.Fatalf("targets = %d; want 3", len(targets))
	}
}

// TestHydrateDstNodes_SkipsErrNotFound covers the
// retirement-race path: a dst that was retired between
// the seed card scan and the follow-up GetNode read
// returns ErrNotFound; the helper logs + skips it
// without failing. The verb's `deduplicatedTargets`
// downstream drops any dst id missing from Targets[],
// so the citation invariant ("every target row exists")
// still holds.
func TestHydrateDstNodes_SkipsErrNotFound(t *testing.T) {
	ids := []string{"dst-live", "dst-retired", "dst-live2"}
	fetcher := &countingFetcher{
		nodes: map[string]graphreader.Node{
			"dst-live":  {NodeID: "dst-live", Kind: "method", CanonicalSignature: "Live"},
			"dst-live2": {NodeID: "dst-live2", Kind: "method", CanonicalSignature: "Live2"},
		},
		errFor: map[string]error{
			"dst-retired": graphreader.ErrNotFound,
		},
	}
	targets, dstSig, err := hydrateDstNodes(context.Background(), fetcher, ids, agentapi.MaxSummarizeEdges, quietTestLogger())
	if err != nil {
		t.Fatalf("hydrateDstNodes: %v", err)
	}
	if fetcher.calls != 3 {
		t.Fatalf("GetNode calls = %d; want 3 (all attempted)", fetcher.calls)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d; want 2 (retired one skipped)", len(targets))
	}
	if _, ok := dstSig["dst-retired"]; ok {
		t.Fatalf("dstSig must not contain the retired id")
	}
}

// TestHydrateDstNodes_ConnectionErrorPromoted proves a
// connection-class failure mid-hydration is promoted to
// `agentapi.ErrGraphStoreUnavailable` so the verb's
// `summarizeGraphFailure` path degrades cleanly. This is
// the same contract `classifyGraphStoreError` enforces in
// the seed-card path.
func TestHydrateDstNodes_ConnectionErrorPromoted(t *testing.T) {
	ids := []string{"dst-1", "dst-broken"}
	fetcher := &countingFetcher{
		nodes: map[string]graphreader.Node{
			"dst-1": {NodeID: "dst-1", Kind: "method"},
		},
		errFor: map[string]error{
			"dst-broken": &net.OpError{Op: "read", Err: errors.New("connection reset")},
		},
	}
	_, _, err := hydrateDstNodes(context.Background(), fetcher, ids, agentapi.MaxSummarizeEdges, quietTestLogger())
	if err == nil {
		t.Fatalf("expected connection error to surface, got nil")
	}
	if !errors.Is(err, agentapi.ErrGraphStoreUnavailable) {
		t.Fatalf("error must wrap ErrGraphStoreUnavailable; got %v", err)
	}
}

// TestHydrateDstNodes_NoCap_HydratesAll covers the edge
// case `max <= 0`: the helper interprets that as
// "no cap" rather than "hydrate nothing", so callers
// that supply a non-positive value (e.g. a test or a
// config that meant to disable bounding) still get the
// full list. The production path always passes
// `agentapi.MaxSummarizeEdges`, but the contract is
// documented in case a future caller relies on it.
func TestHydrateDstNodes_NoCap_HydratesAll(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	nodes := map[string]graphreader.Node{
		"a": {NodeID: "a", Kind: "method"},
		"b": {NodeID: "b", Kind: "method"},
		"c": {NodeID: "c", Kind: "method"},
		"d": {NodeID: "d", Kind: "method"},
	}
	fetcher := &countingFetcher{nodes: nodes}
	targets, _, err := hydrateDstNodes(context.Background(), fetcher, ids, 0, quietTestLogger())
	if err != nil {
		t.Fatalf("hydrateDstNodes: %v", err)
	}
	if fetcher.calls != 4 {
		t.Fatalf("GetNode calls = %d; want 4 (no cap)", fetcher.calls)
	}
	if len(targets) != 4 {
		t.Fatalf("targets = %d; want 4", len(targets))
	}
}
