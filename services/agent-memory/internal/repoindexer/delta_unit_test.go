package repoindexer

// delta_unit_test.go covers the hermetic (no-PostgreSQL) seams of
// the Stage 3.4 delta handler. The functions tested here are:
//
//   - detectRenamePairs — within-file rename detection. The
//     correctness gate is evaluator finding #4: pairing MUST key
//     off (parent_canonical_signature, Kind) and reject buckets
//     with cardinality != (1, 1). The prior implementation grouped
//     by Kind alone, so two unrelated edits (one delete + one add)
//     of the same Kind anywhere in the file became a false rename.
//
//   - filterAppearedTouched — new-side rename-candidate filter.
//     Pins evaluator iter-3 finding #2: a TouchedNode with
//     Inserted=false (typical of a partial-failure replay where
//     the new member was already inserted on attempt 1) MUST still
//     pair with a disappeared old. The pre-fix `!Inserted` gate
//     broke rename detection on retry; the post-fix contract
//     keys solely on canonical_signature (was-in-old vs new) so
//     retries that re-enter the path produce identical pairings.
//
//   - DeltaSummary.AffectedNodeCount — pinned because the field
//     drives the `repo.delta_ingested` event payload that
//     downstream embedding consumers gate retire logic on.
//
// All tests are pure unit tests; they need no database fixture
// and run in the default `go test` invocation alongside the parser
// tests so a regression to either pre-fix shape is caught long
// before the integration suite spins up a PG.

import (
	"reflect"
	"sort"
	"testing"
)

// TestDetectRenamePairs_pairsOneToOneWithinParentBucket exercises
// the happy-path pairing: exactly one disappeared old + one
// appeared new under the same (parent_sig, Kind) becomes a
// rename pair, and `unpairedOld` is empty.
func TestDetectRenamePairs_pairsOneToOneWithinParentBucket(t *testing.T) {
	disappeared := []descendantRow{
		{NodeID: "old-method-1", Kind: "method", CanonicalSignature: "sig-old", ParentNodeID: "class-old-id", ParentCanonicalSignature: "parent-class-sig"},
	}
	appearedNew := []TouchedNode{
		{NodeID: "new-method-1", Kind: "method", CanonicalSignature: "sig-new", ParentNodeID: "class-new-id", Inserted: true},
	}
	newParentSigByID := map[string]string{
		"class-new-id": "parent-class-sig",
	}

	pairs, unpaired := detectRenamePairs(disappeared, appearedNew, newParentSigByID)
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair, got %d: %+v", len(pairs), pairs)
	}
	if pairs[0].oldRow.NodeID != "old-method-1" || pairs[0].newNode.NodeID != "new-method-1" {
		t.Errorf("pair = %+v, want (old-method-1 -> new-method-1)", pairs[0])
	}
	if len(unpaired) != 0 {
		t.Errorf("expected no unpaired, got %+v", unpaired)
	}
}

// TestDetectRenamePairs_doesNotPairAcrossDifferentParents is the
// load-bearing assertion for evaluator finding #4. Two methods
// with the same Kind ('method') under DIFFERENT parent classes
// MUST NOT be paired. Prior to the fix they were, because the
// bucket key was Kind alone.
func TestDetectRenamePairs_doesNotPairAcrossDifferentParents(t *testing.T) {
	disappeared := []descendantRow{
		{NodeID: "old-method-A", Kind: "method", CanonicalSignature: "sig-A", ParentNodeID: "class-A-id", ParentCanonicalSignature: "parent-class-A"},
	}
	appearedNew := []TouchedNode{
		{NodeID: "new-method-B", Kind: "method", CanonicalSignature: "sig-B", ParentNodeID: "class-B-id", Inserted: true},
	}
	newParentSigByID := map[string]string{
		"class-B-id": "parent-class-B", // different parent sig from old side
	}

	pairs, unpaired := detectRenamePairs(disappeared, appearedNew, newParentSigByID)
	if len(pairs) != 0 {
		t.Errorf("expected NO pairs (different parents); got %+v", pairs)
	}
	if len(unpaired) != 1 || unpaired[0].NodeID != "old-method-A" {
		t.Errorf("expected old-method-A in unpaired; got %+v", unpaired)
	}
}

// TestDetectRenamePairs_rejectsMultipleOnEitherSide pins the
// "strict 1↔1 cardinality" rule. When more than one entry exists
// in a bucket on either side, the pairing is ambiguous and the
// whole bucket falls through to plain retire.
func TestDetectRenamePairs_rejectsMultipleOnEitherSide(t *testing.T) {
	// Two deletes + one add under the same parent → ambiguous.
	disappeared := []descendantRow{
		{NodeID: "old-1", Kind: "method", CanonicalSignature: "s1", ParentNodeID: "p-old", ParentCanonicalSignature: "parent"},
		{NodeID: "old-2", Kind: "method", CanonicalSignature: "s2", ParentNodeID: "p-old", ParentCanonicalSignature: "parent"},
	}
	appearedNew := []TouchedNode{
		{NodeID: "new-1", Kind: "method", CanonicalSignature: "s-new", ParentNodeID: "p-new", Inserted: true},
	}
	newParentSigByID := map[string]string{"p-new": "parent"}

	pairs, unpaired := detectRenamePairs(disappeared, appearedNew, newParentSigByID)
	if len(pairs) != 0 {
		t.Errorf("expected NO pairs (ambiguous 2-vs-1 bucket); got %+v", pairs)
	}
	if len(unpaired) != 2 {
		t.Errorf("expected both old rows in unpaired; got %+v", unpaired)
	}

	// Symmetric: one delete + two adds.
	disappeared = []descendantRow{
		{NodeID: "old-X", Kind: "method", CanonicalSignature: "sX", ParentNodeID: "p-old", ParentCanonicalSignature: "parent"},
	}
	appearedNew = []TouchedNode{
		{NodeID: "new-A", Kind: "method", CanonicalSignature: "sA", ParentNodeID: "p-new", Inserted: true},
		{NodeID: "new-B", Kind: "method", CanonicalSignature: "sB", ParentNodeID: "p-new", Inserted: true},
	}
	pairs, unpaired = detectRenamePairs(disappeared, appearedNew, newParentSigByID)
	if len(pairs) != 0 {
		t.Errorf("expected NO pairs (ambiguous 1-vs-2 bucket); got %+v", pairs)
	}
	if len(unpaired) != 1 {
		t.Errorf("expected old-X in unpaired; got %+v", unpaired)
	}
}

// TestDetectRenamePairs_doesNotPairAcrossDifferentKinds asserts a
// Class disappearing and a Method appearing under the same parent
// do NOT pair — the bucket key includes Kind so the two land in
// different buckets.
func TestDetectRenamePairs_doesNotPairAcrossDifferentKinds(t *testing.T) {
	disappeared := []descendantRow{
		{NodeID: "old-class", Kind: "class", CanonicalSignature: "c-sig", ParentNodeID: "file", ParentCanonicalSignature: "file-sig"},
	}
	appearedNew := []TouchedNode{
		{NodeID: "new-method", Kind: "method", CanonicalSignature: "m-sig", ParentNodeID: "file", Inserted: true},
	}
	newParentSigByID := map[string]string{"file": "file-sig"}

	pairs, unpaired := detectRenamePairs(disappeared, appearedNew, newParentSigByID)
	if len(pairs) != 0 {
		t.Errorf("expected NO pairs (different Kinds); got %+v", pairs)
	}
	if len(unpaired) != 1 {
		t.Errorf("expected old-class unpaired; got %+v", unpaired)
	}
}

// TestDetectRenamePairs_emptyParentSigDoesNotCollapse confirms a
// disappeared row with an unknown parent canonical signature does
// NOT silently collapse into an empty-string bucket where it
// could falsely pair with another orphan. Orphans fall through
// to the unpaired residue and the caller bulk-retires them with
// no rename annotation.
func TestDetectRenamePairs_emptyParentSigDoesNotCollapse(t *testing.T) {
	disappeared := []descendantRow{
		{NodeID: "orphan-1", Kind: "method", CanonicalSignature: "s1", ParentNodeID: "p1", ParentCanonicalSignature: ""},
		{NodeID: "orphan-2", Kind: "method", CanonicalSignature: "s2", ParentNodeID: "p2", ParentCanonicalSignature: ""},
	}
	// One new method whose parent ALSO resolves to empty — this
	// is the false-pair attack the empty-sentinel guards against.
	appearedNew := []TouchedNode{
		{NodeID: "new-method", Kind: "method", CanonicalSignature: "sN", ParentNodeID: "pN", Inserted: true},
	}
	newParentSigByID := map[string]string{
		"pN": "", // empty sig → unpairable on the new side too
	}

	pairs, unpaired := detectRenamePairs(disappeared, appearedNew, newParentSigByID)
	if len(pairs) != 0 {
		t.Errorf("expected NO pairs (empty parent sigs MUST NOT collapse into a shared bucket); got %+v", pairs)
	}
	// Both orphans must end up in unpaired so the caller still
	// bulk-retires them with no rename annotation.
	if len(unpaired) != 2 {
		t.Errorf("expected 2 unpaired orphans, got %d: %+v", len(unpaired), unpaired)
	}
}

// TestDetectRenamePairs_outputOrderIsDeterministic pins the
// stable iteration order (input `disappeared` order, not map
// iteration). Without this, a flaky test loop could pass on one
// machine and fail on another that re-orders bucket map keys.
func TestDetectRenamePairs_outputOrderIsDeterministic(t *testing.T) {
	// Three independent buckets, each with a 1↔1 pair. The
	// returned `pairs` slice should preserve the input order of
	// `disappeared` (B, A, C) not bucket-map iteration order.
	disappeared := []descendantRow{
		{NodeID: "old-B", Kind: "method", CanonicalSignature: "B", ParentNodeID: "pB", ParentCanonicalSignature: "B-parent"},
		{NodeID: "old-A", Kind: "method", CanonicalSignature: "A", ParentNodeID: "pA", ParentCanonicalSignature: "A-parent"},
		{NodeID: "old-C", Kind: "method", CanonicalSignature: "C", ParentNodeID: "pC", ParentCanonicalSignature: "C-parent"},
	}
	appearedNew := []TouchedNode{
		{NodeID: "new-A", Kind: "method", CanonicalSignature: "A2", ParentNodeID: "pAnew", Inserted: true},
		{NodeID: "new-B", Kind: "method", CanonicalSignature: "B2", ParentNodeID: "pBnew", Inserted: true},
		{NodeID: "new-C", Kind: "method", CanonicalSignature: "C2", ParentNodeID: "pCnew", Inserted: true},
	}
	newParentSigByID := map[string]string{
		"pAnew": "A-parent", "pBnew": "B-parent", "pCnew": "C-parent",
	}

	pairs, _ := detectRenamePairs(disappeared, appearedNew, newParentSigByID)
	if len(pairs) != 3 {
		t.Fatalf("expected 3 pairs, got %d: %+v", len(pairs), pairs)
	}
	gotOrder := []string{pairs[0].oldRow.NodeID, pairs[1].oldRow.NodeID, pairs[2].oldRow.NodeID}
	wantOrder := []string{"old-B", "old-A", "old-C"}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("pair order = %v, want %v (input-disappeared order)", gotOrder, wantOrder)
	}
}

// TestFilterAppearedTouched_keepsInsertedFalseForRetrySafety
// pins evaluator iter-3 finding #2. The pre-fix implementation
// gated the rename-target set on `TouchedNode.Inserted=true`,
// which was retry-unsafe: a partial-failure replay where the
// new member was already inserted on attempt 1 (so attempt 2
// sees Inserted=false from the graphwriter's idempotent re-
// insert) would miss the rename pair and tombstone the old
// member without `superseded_by_node_id` / `renamed_to`.
//
// The post-fix contract is "include every TouchedNode whose
// canonical_signature is NOT in oldSigSet, regardless of
// Inserted". This test fails loudly if anyone re-adds the
// Inserted gate.
func TestFilterAppearedTouched_keepsInsertedFalseForRetrySafety(t *testing.T) {
	oldSigSet := map[string]struct{}{
		"sig-still-present": {},
		"sig-old-only":      {},
	}
	touched := []TouchedNode{
		// New sig + Inserted=true → kept (the classic case).
		{NodeID: "n-inserted-true", Kind: "method", CanonicalSignature: "sig-new-1", Inserted: true},
		// New sig + Inserted=false (RETRY SCENARIO) → MUST be
		// kept. This is the regression gate: pre-fix code
		// skipped this entry and broke rename pairing on
		// retry.
		{NodeID: "n-inserted-false", Kind: "method", CanonicalSignature: "sig-new-2", Inserted: false},
		// Old sig + Inserted=true → dropped (sig was already
		// in oldDescendants so it can't be a "newly appeared"
		// rename target; idempotent re-confirm of an unchanged
		// node).
		{NodeID: "n-old-sig-inserted", Kind: "method", CanonicalSignature: "sig-still-present", Inserted: true},
		// Old sig + Inserted=false → dropped (same reason).
		{NodeID: "n-old-sig-noop", Kind: "method", CanonicalSignature: "sig-still-present", Inserted: false},
	}

	got := filterAppearedTouched(touched, oldSigSet)
	gotIDs := make([]string, 0, len(got))
	for _, t := range got {
		gotIDs = append(gotIDs, t.NodeID)
	}
	sort.Strings(gotIDs)
	wantIDs := []string{"n-inserted-false", "n-inserted-true"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("filterAppearedTouched returned %v, want %v (Inserted=false MUST be kept for retry safety)", gotIDs, wantIDs)
	}
}

// TestDetectRenamePairs_pairsAcrossInsertedFalseRetry mirrors
// the iter-3 finding #2 regression at the rename-detector level.
// The new emit (simulated retry) contains a TouchedNode with
// Inserted=false whose sig is NEW (not in oldSigSet) — paired
// with a disappeared old of the same (parent_sig, Kind) bucket,
// the detector MUST still emit one rename pair. Pre-fix, the
// caller's appearedNew filter dropped the Inserted=false entry
// and the detector saw an empty new bucket → no pair.
func TestDetectRenamePairs_pairsAcrossInsertedFalseRetry(t *testing.T) {
	oldSigSet := map[string]struct{}{
		"sig-old-method": {},
	}
	// Retry path: the new method was already inserted on
	// attempt 1, so attempt 2's dispatcher returns Inserted=false.
	touched := []TouchedNode{
		{NodeID: "new-method-retry", Kind: "method", CanonicalSignature: "sig-new-method", ParentNodeID: "class-new", Inserted: false},
	}
	appearedNew := filterAppearedTouched(touched, oldSigSet)
	if len(appearedNew) != 1 {
		t.Fatalf("filterAppearedTouched dropped Inserted=false entry: got %+v", appearedNew)
	}
	disappeared := []descendantRow{
		{NodeID: "old-method", Kind: "method", CanonicalSignature: "sig-old-method", ParentNodeID: "class-old", ParentCanonicalSignature: "class-sig"},
	}
	newParentSigByID := map[string]string{
		"class-new": "class-sig",
	}
	pairs, unpaired := detectRenamePairs(disappeared, appearedNew, newParentSigByID)
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair across retry (Inserted=false on new), got %d: pairs=%+v unpaired=%+v", len(pairs), pairs, unpaired)
	}
	if pairs[0].oldRow.NodeID != "old-method" || pairs[0].newNode.NodeID != "new-method-retry" {
		t.Errorf("pair = %+v, want (old-method -> new-method-retry)", pairs[0])
	}
	if len(unpaired) != 0 {
		t.Errorf("unpaired = %+v, want empty (retry path must still pair)", unpaired)
	}
}

// TestDeltaSummary_AffectedNodeCount pins the formula the
// `repo.delta_ingested` event publishes. Defined as
// "Nodes the delta either emitted OR retired" — emitted + retired,
// NOT edges retired (the brief says "affected NODE count").
func TestDeltaSummary_AffectedNodeCount(t *testing.T) {
	cases := []struct {
		name string
		s    DeltaSummary
		want int
	}{
		{name: "zero", s: DeltaSummary{}, want: 0},
		{name: "emit only", s: DeltaSummary{NodesEmitted: 5}, want: 5},
		{name: "retire only", s: DeltaSummary{NodesRetired: 7}, want: 7},
		{name: "emit + retire", s: DeltaSummary{NodesEmitted: 3, NodesRetired: 4}, want: 7},
		{
			name: "edges_not_counted",
			s: DeltaSummary{
				NodesEmitted:           1,
				NodesRetired:           1,
				EdgesRetired:           99, // MUST NOT bump the count
				RenamedToEdgesInserted: 99, // MUST NOT bump the count
			},
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.AffectedNodeCount(); got != tc.want {
				t.Errorf("AffectedNodeCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// (Helper for any future test that wants sorted assertions.)
var _ = sort.Strings
