package spaningestor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// fakeLookup is the in-memory Lookup the unit tests use.  The
// production binding (Stage 4.2) is graphreader-backed and not
// in scope here; this fake exists so the §8.6 attribute-mapping
// ladder is exercisable without standing up a Postgres pool.
//
// Indexing model
// --------------
//   - methodsByName: keyed by (repoID, namespace, function);
//     value is the slice of MethodCandidates a name lookup
//     returns.  Tests populate this map directly to exercise
//     "0 candidates", "1 candidate", and ">1 candidates" rungs.
//   - methodsByFile: keyed by (repoID, filepath); value is the
//     slice of MethodCandidates the file contains.  Lookup
//     scans for the one whose [BodyStartLine, BodyEndLine]
//     covers the requested lineno.
//   - blocksByMethod: keyed by methodNodeID; value is the slice
//     of BlockCandidates under that Method.  Lookup scans for
//     the MOST SPECIFIC interval covering lineno (narrowest
//     match wins so nested blocks resolve correctly).
//   - injected*Err: when non-nil, the corresponding Lookup
//     method returns the error untouched, used by the "lookup
//     backend failure" test (rubber-duck finding #6).
type fakeLookup struct {
	methodsByName     map[string][]MethodCandidate
	methodsByFile     map[string][]MethodCandidate
	blocksByMethod    map[string][]BlockCandidate
	injectedNameErr   error
	injectedLocErr    error
	injectedBlockErr  error
	nameCallCount     int
	locationCallCount int
	blockCallCount    int
}

func newFakeLookup() *fakeLookup {
	return &fakeLookup{
		methodsByName:  map[string][]MethodCandidate{},
		methodsByFile:  map[string][]MethodCandidate{},
		blocksByMethod: map[string][]BlockCandidate{},
	}
}

func nameKey(repoID, ns, fn string) string {
	return repoID + "\x00" + ns + "\x00" + fn
}

func fileKey(repoID, fp string) string {
	return repoID + "\x00" + fp
}

func (f *fakeLookup) addMethod(repoID, ns, fn string, c MethodCandidate) {
	k := nameKey(repoID, ns, fn)
	f.methodsByName[k] = append(f.methodsByName[k], c)
	fk := fileKey(repoID, c.FilePath)
	f.methodsByFile[fk] = append(f.methodsByFile[fk], c)
}

func (f *fakeLookup) addBlock(methodNodeID string, b BlockCandidate) {
	f.blocksByMethod[methodNodeID] = append(f.blocksByMethod[methodNodeID], b)
}

func (f *fakeLookup) LookupMethodsByName(_ context.Context, repoID, namespace, function string) ([]MethodCandidate, error) {
	f.nameCallCount++
	if f.injectedNameErr != nil {
		return nil, f.injectedNameErr
	}
	hits := f.methodsByName[nameKey(repoID, namespace, function)]
	if len(hits) == 0 {
		return nil, nil
	}
	out := make([]MethodCandidate, len(hits))
	copy(out, hits)
	return out, nil
}

func (f *fakeLookup) LookupMethodByLocation(_ context.Context, repoID, filepath string, lineno int) (*MethodCandidate, error) {
	f.locationCallCount++
	if f.injectedLocErr != nil {
		return nil, f.injectedLocErr
	}
	hits := f.methodsByFile[fileKey(repoID, filepath)]
	for i := range hits {
		c := hits[i]
		if c.BodyStartLine <= lineno && lineno <= c.BodyEndLine {
			return &c, nil
		}
	}
	return nil, nil
}

func (f *fakeLookup) LookupBlockForMethod(_ context.Context, methodNodeID string, lineno int) (*BlockCandidate, error) {
	f.blockCallCount++
	if f.injectedBlockErr != nil {
		return nil, f.injectedBlockErr
	}
	hits := f.blocksByMethod[methodNodeID]
	// Pick the narrowest covering interval so nested blocks
	// resolve to the innermost.  Ties broken by first-insert
	// order (stable).
	var best *BlockCandidate
	bestWidth := -1
	for i := range hits {
		b := hits[i]
		if b.StartLine <= lineno && lineno <= b.EndLine {
			width := b.EndLine - b.StartLine
			if bestWidth < 0 || width < bestWidth {
				bb := b
				best = &bb
				bestWidth = width
			}
		}
	}
	return best, nil
}

// ---------------------------------------------------------------
// Required scenario 1: clean resolve to Method
//
// Mirrors implementation-plan.md Stage 4.1 test scenario:
//   Given a span with code.namespace=pkg and code.function=Foo.bar(int),
//   When the resolver runs,
//   Then it returns the Method Node whose canonical_signature
//   is pkg.Foo#bar(int).
//
// The fake's match key is the (namespace, function) tuple AS
// EMITTED — the resolver passes both through verbatim; the
// production binding (Stage 4.2) is responsible for whatever
// canonical signature reconstruction its graph requires.  The
// canonical_signature value on the returned candidate is the
// stylized form the implementation plan calls out.
// ---------------------------------------------------------------
func TestResolve_cleanMethodMatch(t *testing.T) {
	t.Parallel()
	repoID := "repo-1"
	fl := newFakeLookup()
	fl.addMethod(repoID, "pkg", "Foo.bar(int)", MethodCandidate{
		NodeID:             "node-bar",
		CanonicalSignature: "pkg.Foo#bar(int)",
		FilePath:           "pkg/foo.go",
		ParamSignature:     "int",
		BodyStartLine:      10,
		BodyEndLine:        20,
	})
	r := New(fl, NewMetrics(), nil)

	got, err := r.Resolve(context.Background(), Span{
		RepoID: repoID,
		Attributes: map[string]string{
			AttrCodeNamespace: "pkg",
			AttrCodeFunction:  "Foo.bar(int)",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Status != StatusMethod {
		t.Fatalf("Status = %v, want StatusMethod", got.Status)
	}
	if got.Method == nil {
		t.Fatal("Method = nil, want non-nil")
	}
	if got.Method.CanonicalSignature != "pkg.Foo#bar(int)" {
		t.Errorf("CanonicalSignature = %q, want %q",
			got.Method.CanonicalSignature, "pkg.Foo#bar(int)")
	}
	if got.Block != nil {
		t.Errorf("Block = %+v, want nil (no code.lineno supplied)", got.Block)
	}
	if got.Reason != ReasonNameMatched {
		t.Errorf("Reason = %v, want ReasonNameMatched", got.Reason)
	}
	if got.BlockOutcome != BlockOutcomeNoLineno {
		t.Errorf("BlockOutcome = %v, want BlockOutcomeNoLineno (no code.lineno)",
			got.BlockOutcome)
	}
	if fl.locationCallCount != 0 {
		t.Errorf("location lookup was called %d times; clean name match must not fall through",
			fl.locationCallCount)
	}
}

// ---------------------------------------------------------------
// Required scenario 2: fallback to filepath/lineno
//
// Given a span missing code.function, When the resolver runs,
// Then it uses code.filepath + code.lineno to locate the
// enclosing Method and returns it.
// ---------------------------------------------------------------
func TestResolve_filepathFallback(t *testing.T) {
	t.Parallel()
	repoID := "repo-2"
	fl := newFakeLookup()
	fl.addMethod(repoID, "svc", "handler", MethodCandidate{
		NodeID:             "node-handler",
		CanonicalSignature: "svc.handler#handle(ctx)",
		FilePath:           "svc/handler.go",
		BodyStartLine:      100,
		BodyEndLine:        200,
	})
	metrics := NewMetrics()
	r := New(fl, metrics, nil)

	got, err := r.Resolve(context.Background(), Span{
		RepoID: repoID,
		Attributes: map[string]string{
			AttrCodeFilepath: "svc/handler.go",
			AttrCodeLineno:   "142",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Status != StatusMethod {
		t.Fatalf("Status = %v, want StatusMethod", got.Status)
	}
	if got.Method == nil || got.Method.NodeID != "node-handler" {
		t.Fatalf("Method = %+v, want node-handler", got.Method)
	}
	if got.Reason != ReasonLocationMatched {
		t.Errorf("Reason = %v, want ReasonLocationMatched", got.Reason)
	}
	// No blocks registered for this method → BlockOutcome must
	// be OutsideBlock (lineno parsed, lookup ran, returned nil).
	if got.BlockOutcome != BlockOutcomeOutsideBlock {
		t.Errorf("BlockOutcome = %v, want BlockOutcomeOutsideBlock", got.BlockOutcome)
	}
	if metrics.UnresolvedFor(repoID) != 0 {
		t.Errorf("UnresolvedFor(%q) = %d, want 0 (fallback resolved)",
			repoID, metrics.UnresolvedFor(repoID))
	}
}

// ---------------------------------------------------------------
// Required scenario 3: unresolved span counted
//
// Given a span with neither code.function nor code.filepath set,
// When the resolver runs, Then it returns Unresolved and
// span_unresolved_total{repo_id=...} is incremented by 1.
// ---------------------------------------------------------------
func TestResolve_unresolvedSpanCounted(t *testing.T) {
	t.Parallel()
	repoID := "repo-3"
	fl := newFakeLookup()
	metrics := NewMetrics()
	r := New(fl, metrics, nil)

	got, err := r.Resolve(context.Background(), Span{
		RepoID:     repoID,
		Attributes: map[string]string{}, // empty
	})
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Status != StatusUnresolved {
		t.Fatalf("Status = %v, want StatusUnresolved", got.Status)
	}
	if got.Method != nil || got.Block != nil {
		t.Errorf("Method/Block must be nil on unresolved; got Method=%+v Block=%+v",
			got.Method, got.Block)
	}
	if got.Reason != ReasonMissingAllAttributes {
		t.Errorf("Reason = %v, want ReasonMissingAllAttributes", got.Reason)
	}
	if metrics.UnresolvedFor(repoID) != 1 {
		t.Errorf("UnresolvedFor(%q) = %d, want 1",
			repoID, metrics.UnresolvedFor(repoID))
	}
	// Snapshot should carry the same value.
	snap := metrics.Snapshot()
	if snap[repoID] != 1 {
		t.Errorf("Snapshot[%q] = %d, want 1", repoID, snap[repoID])
	}
}

// ---------------------------------------------------------------
// Required scenario 4: ambiguous overload disambiguated by code.signature
//
// The implementation-plan calls out "ambiguous overload (use
// code.signature if present per OTel semantic conventions)".
// Two methods share name pkg.Foo.bar but differ on params;
// code.signature picks one.
// ---------------------------------------------------------------
func TestResolve_overloadDisambiguatedBySignature(t *testing.T) {
	t.Parallel()
	repoID := "repo-4"
	fl := newFakeLookup()
	fl.addMethod(repoID, "pkg", "Foo.bar", MethodCandidate{
		NodeID:             "node-bar-int",
		CanonicalSignature: "pkg.Foo#bar(int)",
		FilePath:           "pkg/foo.go",
		ParamSignature:     "int",
		BodyStartLine:      10,
		BodyEndLine:        20,
	})
	fl.addMethod(repoID, "pkg", "Foo.bar", MethodCandidate{
		NodeID:             "node-bar-string",
		CanonicalSignature: "pkg.Foo#bar(string)",
		FilePath:           "pkg/foo.go",
		ParamSignature:     "string",
		BodyStartLine:      30,
		BodyEndLine:        40,
	})
	r := New(fl, NewMetrics(), nil)

	t.Run("signature picks int overload", func(t *testing.T) {
		got, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg",
				AttrCodeFunction:  "Foo.bar",
				AttrCodeSignature: "(int)",
			},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Status != StatusMethod || got.Method == nil || got.Method.NodeID != "node-bar-int" {
			t.Fatalf("Status=%v Method=%+v; want StatusMethod node-bar-int", got.Status, got.Method)
		}
	})

	t.Run("signature picks string overload via full method+params form", func(t *testing.T) {
		got, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg",
				AttrCodeFunction:  "Foo.bar",
				AttrCodeSignature: "bar(string)",
			},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Status != StatusMethod || got.Method == nil || got.Method.NodeID != "node-bar-string" {
			t.Fatalf("Status=%v Method=%+v; want StatusMethod node-bar-string", got.Status, got.Method)
		}
	})

	t.Run("ambiguous without signature falls through to filepath", func(t *testing.T) {
		// No signature; both candidates remain ambiguous.  No
		// filepath either, so the result is unresolved with
		// reason=ambiguous_name.
		got, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg",
				AttrCodeFunction:  "Foo.bar",
			},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Status != StatusUnresolved {
			t.Fatalf("Status = %v, want StatusUnresolved (ambiguous, no fallback)", got.Status)
		}
		if got.Reason != ReasonAmbiguousName {
			t.Errorf("Reason = %v, want ReasonAmbiguousName", got.Reason)
		}
	})

	t.Run("signature that matches neither overload falls through", func(t *testing.T) {
		got, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg",
				AttrCodeFunction:  "Foo.bar",
				AttrCodeSignature: "(float64)",
			},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Status != StatusUnresolved {
			t.Fatalf("Status = %v, want StatusUnresolved", got.Status)
		}
		if got.Reason != ReasonSignatureMismatch {
			t.Errorf("Reason = %v, want ReasonSignatureMismatch", got.Reason)
		}
	})
}

// signature mismatch on a UNIQUE candidate must fall through —
// rubber-duck blocker #2.  Accepting a contradictory unique
// candidate would pollute observation aggregates.
func TestResolve_uniqueCandidateSignatureMismatchFallsThrough(t *testing.T) {
	t.Parallel()
	repoID := "repo-5"
	fl := newFakeLookup()
	fl.addMethod(repoID, "pkg", "Foo.bar", MethodCandidate{
		NodeID:             "node-bar-int",
		CanonicalSignature: "pkg.Foo#bar(int)",
		FilePath:           "pkg/foo.go",
		ParamSignature:     "int",
		BodyStartLine:      10,
		BodyEndLine:        20,
	})
	r := New(fl, NewMetrics(), nil)

	// No filepath fallback configured — expect unresolved.
	got, err := r.Resolve(context.Background(), Span{
		RepoID: repoID,
		Attributes: map[string]string{
			AttrCodeNamespace: "pkg",
			AttrCodeFunction:  "Foo.bar",
			AttrCodeSignature: "(string)", // doesn't match (int)
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Status != StatusUnresolved {
		t.Fatalf("Status = %v, want StatusUnresolved (sig mismatch must not accept unique candidate)",
			got.Status)
	}
	if got.Reason != ReasonSignatureMismatch {
		t.Errorf("Reason = %v, want ReasonSignatureMismatch", got.Reason)
	}
}

// signature mismatch on a unique candidate WITH empty
// ParamSignature is accepted — the graph can't tell us the
// answer, so we trust the unique match (the policy comment in
// chooseMethod calls this out).
func TestResolve_uniqueCandidateEmptyParamSigAcceptsAnySignature(t *testing.T) {
	t.Parallel()
	repoID := "repo-6"
	fl := newFakeLookup()
	fl.addMethod(repoID, "pkg", "Foo.bar", MethodCandidate{
		NodeID:             "node-bar",
		CanonicalSignature: "pkg.Foo#bar",
		FilePath:           "pkg/foo.go",
		ParamSignature:     "", // graph couldn't extract one
		BodyStartLine:      10,
		BodyEndLine:        20,
	})
	r := New(fl, NewMetrics(), nil)

	got, err := r.Resolve(context.Background(), Span{
		RepoID: repoID,
		Attributes: map[string]string{
			AttrCodeNamespace: "pkg",
			AttrCodeFunction:  "Foo.bar",
			AttrCodeSignature: "(string)",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Status != StatusMethod || got.Method == nil || got.Method.NodeID != "node-bar" {
		t.Fatalf("Status=%v Method=%+v; want StatusMethod node-bar", got.Status, got.Method)
	}
}

// ---------------------------------------------------------------
// Required scenario 5: Block boundary edge cases
//
// Per tech-spec §8.6 the Block lookup uses code.lineno against
// the ingested boundaries; if no Block matches, the observation
// attaches to the Method.  The boundary contract from
// internal/repoindexer/ast/block.go is [StartLine, EndLine]
// INCLUSIVE on both ends.  Tests:
//   - lineno strictly inside a block → Block returned
//   - lineno exactly equal to StartLine / EndLine → Block returned
//   - lineno between two non-overlapping blocks → method-only
//     with ReasonLinenoOutsideBlock
//   - nested blocks → narrowest covering wins
// ---------------------------------------------------------------
func TestResolve_blockBoundaryCases(t *testing.T) {
	t.Parallel()
	repoID := "repo-blk"
	fl := newFakeLookup()
	method := MethodCandidate{
		NodeID:             "node-method",
		CanonicalSignature: "pkg.Foo#handle()",
		FilePath:           "pkg/handler.go",
		BodyStartLine:      10,
		BodyEndLine:        80,
	}
	fl.addMethod(repoID, "pkg", "Foo.handle()", method)
	// Two non-overlapping blocks plus an inner nested one that
	// must win the narrowest-interval tiebreak inside [20,30].
	fl.addBlock(method.NodeID, BlockCandidate{
		NodeID: "blk-entry", CanonicalSignature: method.CanonicalSignature + "#block_0_entry",
		Kind: "entry", StartLine: 10, EndLine: 40,
	})
	fl.addBlock(method.NodeID, BlockCandidate{
		NodeID: "blk-inner", CanonicalSignature: method.CanonicalSignature + "#block_1_branch",
		Kind: "branch", StartLine: 20, EndLine: 30,
	})
	fl.addBlock(method.NodeID, BlockCandidate{
		NodeID: "blk-exit", CanonicalSignature: method.CanonicalSignature + "#block_2_exit",
		Kind: "exit", StartLine: 50, EndLine: 80,
	})

	r := New(fl, NewMetrics(), nil)

	type tc struct {
		name             string
		lineno           string
		wantStatus       ResolutionStatus
		wantBlock        string // empty = no block expected
		wantBlockOutcome BlockOutcome
	}
	cases := []tc{
		{"strictly inside outer", "12", StatusBlock, "blk-entry", BlockOutcomeMatched},
		{"start boundary inclusive (entry)", "10", StatusBlock, "blk-entry", BlockOutcomeMatched},
		{"end boundary inclusive (entry)", "40", StatusBlock, "blk-entry", BlockOutcomeMatched},
		{"nested narrower wins (inside 20..30)", "25", StatusBlock, "blk-inner", BlockOutcomeMatched},
		{"nested boundary start", "20", StatusBlock, "blk-inner", BlockOutcomeMatched},
		{"nested boundary end", "30", StatusBlock, "blk-inner", BlockOutcomeMatched},
		{"between blocks (gap 41..49) → method only", "45", StatusMethod, "", BlockOutcomeOutsideBlock},
		{"exit start boundary", "50", StatusBlock, "blk-exit", BlockOutcomeMatched},
		{"exit end boundary", "80", StatusBlock, "blk-exit", BlockOutcomeMatched},
		{"lineno way outside method body", "5000", StatusMethod, "", BlockOutcomeOutsideBlock},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := r.Resolve(context.Background(), Span{
				RepoID: repoID,
				Attributes: map[string]string{
					AttrCodeNamespace: "pkg",
					AttrCodeFunction:  "Foo.handle()",
					AttrCodeLineno:    c.lineno,
				},
			})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Status != c.wantStatus {
				t.Fatalf("Status=%v, want %v", got.Status, c.wantStatus)
			}
			// Method-side reason should always be NameMatched
			// in these cases (name lookup succeeded).
			if got.Reason != ReasonNameMatched {
				t.Errorf("Reason=%v, want ReasonNameMatched", got.Reason)
			}
			if got.BlockOutcome != c.wantBlockOutcome {
				t.Errorf("BlockOutcome=%v, want %v", got.BlockOutcome, c.wantBlockOutcome)
			}
			if c.wantBlock == "" {
				if got.Block != nil {
					t.Errorf("Block=%+v, want nil", got.Block)
				}
			} else {
				if got.Block == nil || got.Block.NodeID != c.wantBlock {
					t.Errorf("Block=%+v, want NodeID=%q", got.Block, c.wantBlock)
				}
			}
		})
	}
}

// ---------------------------------------------------------------
// Required scenario "missing code.function" already covered by
// TestResolve_filepathFallback; this one covers the more subtle
// case where code.function is PRESENT but yields zero name
// candidates — we still fall through to filepath.
// ---------------------------------------------------------------
func TestResolve_unknownFunctionFallsThroughToFilepath(t *testing.T) {
	t.Parallel()
	repoID := "repo-7"
	fl := newFakeLookup()
	// No name candidates registered.  Filepath candidate exists.
	fl.methodsByFile[fileKey(repoID, "svc/handler.go")] = []MethodCandidate{{
		NodeID: "node-handler", CanonicalSignature: "svc/handler#handle",
		FilePath: "svc/handler.go", BodyStartLine: 1, BodyEndLine: 1000,
	}}

	r := New(fl, NewMetrics(), nil)

	got, err := r.Resolve(context.Background(), Span{
		RepoID: repoID,
		Attributes: map[string]string{
			AttrCodeNamespace: "svc",
			AttrCodeFunction:  "Mystery.unknown()",
			AttrCodeFilepath:  "svc/handler.go",
			AttrCodeLineno:    "42",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Status != StatusMethod || got.Method == nil || got.Method.NodeID != "node-handler" {
		t.Fatalf("Status=%v Method=%+v; want StatusMethod via filepath", got.Status, got.Method)
	}
}

// Non-numeric code.lineno → resolver treats as no usable lineno.
// When the name rung produces a Method, we surface it method-only
// (the filepath rung needs lineno to disambiguate enclosure).
func TestResolve_unparseableLinenoMethodOnly(t *testing.T) {
	t.Parallel()
	repoID := "repo-8"
	fl := newFakeLookup()
	fl.addMethod(repoID, "pkg", "Foo.bar", MethodCandidate{
		NodeID: "node-bar", CanonicalSignature: "pkg.Foo#bar()",
		FilePath: "pkg/foo.go", BodyStartLine: 1, BodyEndLine: 100,
	})
	r := New(fl, NewMetrics(), nil)

	got, err := r.Resolve(context.Background(), Span{
		RepoID: repoID,
		Attributes: map[string]string{
			AttrCodeNamespace: "pkg",
			AttrCodeFunction:  "Foo.bar",
			AttrCodeLineno:    "not-a-number",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Status != StatusMethod {
		t.Fatalf("Status=%v, want StatusMethod", got.Status)
	}
	if got.Reason != ReasonNameMatched {
		t.Errorf("Reason=%v, want ReasonNameMatched", got.Reason)
	}
	if got.BlockOutcome != BlockOutcomeLinenoUnparseable {
		t.Errorf("BlockOutcome=%v, want BlockOutcomeLinenoUnparseable", got.BlockOutcome)
	}
}

// Per tech-spec §8.6 the name rung requires BOTH `code.namespace`
// AND `code.function` — if EITHER is missing the resolver must
// fall back to `code.filepath` + `code.lineno` and never call
// LookupMethodsByName.  Empty namespace must NOT broaden the
// search (that would risk a false positive against a
// genuinely-empty-namespace free function in an unrelated repo
// or language).  This test exercises both branches:
//
//   (a) `code.function="main"` with no namespace and NO
//       filepath rescue → must skip rung 1 entirely (no call to
//       LookupMethodsByName), drop the span, and increment
//       span_unresolved_total.
//
//   (b) `code.function="main"` with no namespace BUT filepath
//       rescue available → must still skip rung 1, then succeed
//       via rung 2 with Reason=LocationMatched.
//
// Regression guard: a previous iter pinned the contradictory
// "empty namespace is literal" behaviour (the resolver passed
// the empty namespace through to LookupMethodsByName, allowing
// a free-function match to absorb mis-emitted spans).  See
// evaluator iter-1 finding #1.
func TestResolve_missingNamespaceSkipsNameRung(t *testing.T) {
	t.Parallel()
	repoID := "repo-9"

	t.Run("no filepath rescue → unresolved + counted", func(t *testing.T) {
		fl := newFakeLookup()
		// Both candidates registered.  Neither should be
		// reached because rung 1 must be skipped.
		fl.addMethod(repoID, "pkg", "main", MethodCandidate{
			NodeID: "node-pkg-main", CanonicalSignature: "pkg#main",
			FilePath: "main.go", BodyStartLine: 1, BodyEndLine: 5,
		})
		fl.addMethod(repoID, "", "main", MethodCandidate{
			NodeID: "node-free-main", CanonicalSignature: "#main",
			FilePath: "main.go", BodyStartLine: 10, BodyEndLine: 15,
		})
		metrics := NewMetrics()
		r := New(fl, metrics, nil)

		got, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeFunction: "main",
				// AttrCodeNamespace deliberately omitted
			},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Status != StatusUnresolved {
			t.Fatalf("Status=%v Method=%+v; want StatusUnresolved (rung 1 must skip on missing ns)",
				got.Status, got.Method)
		}
		if fl.nameCallCount != 0 {
			t.Errorf("LookupMethodsByName called %d times; want 0 (§8.6 forbids name rung without both attrs)",
				fl.nameCallCount)
		}
		if metrics.UnresolvedFor(repoID) != 1 {
			t.Errorf("UnresolvedFor(%q)=%d, want 1", repoID, metrics.UnresolvedFor(repoID))
		}
	})

	t.Run("filepath rescue → resolves via rung 2", func(t *testing.T) {
		fl := newFakeLookup()
		fl.addMethod(repoID, "", "main", MethodCandidate{
			NodeID: "node-free-main", CanonicalSignature: "#main",
			FilePath: "main.go", BodyStartLine: 10, BodyEndLine: 15,
		})
		r := New(fl, NewMetrics(), nil)

		got, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeFunction: "main",
				AttrCodeFilepath: "main.go",
				AttrCodeLineno:   "12",
			},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Status != StatusMethod || got.Method == nil || got.Method.NodeID != "node-free-main" {
			t.Fatalf("Status=%v Method=%+v; want StatusMethod node-free-main via filepath",
				got.Status, got.Method)
		}
		if got.Reason != ReasonLocationMatched {
			t.Errorf("Reason=%v, want ReasonLocationMatched (rung 2 must own the resolution)", got.Reason)
		}
		if fl.nameCallCount != 0 {
			t.Errorf("LookupMethodsByName called %d times; want 0", fl.nameCallCount)
		}
	})

	t.Run("namespace present, function missing also skips rung 1", func(t *testing.T) {
		fl := newFakeLookup()
		fl.addMethod(repoID, "pkg", "", MethodCandidate{
			NodeID: "node-empty-fn", CanonicalSignature: "pkg#",
			FilePath: "pkg/x.go", BodyStartLine: 1, BodyEndLine: 5,
		})
		metrics := NewMetrics()
		r := New(fl, metrics, nil)

		got, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg",
				// AttrCodeFunction deliberately omitted
			},
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Status != StatusUnresolved {
			t.Fatalf("Status=%v; want StatusUnresolved", got.Status)
		}
		if fl.nameCallCount != 0 {
			t.Errorf("LookupMethodsByName called %d times; want 0 (function missing)",
				fl.nameCallCount)
		}
		if metrics.UnresolvedFor(repoID) != 1 {
			t.Errorf("UnresolvedFor(%q)=%d, want 1", repoID, metrics.UnresolvedFor(repoID))
		}
	})
}

// Backend failures (Lookup returns an error) must NOT be counted
// as unresolved spans — they are an infrastructure signal, not a
// span-quality signal (rubber-duck blocker #6).  The resolver
// wraps the error in ErrLookupFailure so callers can pattern-
// match.
func TestResolve_lookupBackendErrorPropagatedNotCounted(t *testing.T) {
	t.Parallel()
	repoID := "repo-10"

	t.Run("name lookup error propagates", func(t *testing.T) {
		fl := newFakeLookup()
		fl.injectedNameErr = errors.New("db connection refused")
		metrics := NewMetrics()
		r := New(fl, metrics, nil)

		_, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg",
				AttrCodeFunction:  "Foo.bar",
			},
		})
		if err == nil {
			t.Fatal("Resolve must return the backend error")
		}
		if !errors.Is(err, ErrLookupFailure) {
			t.Errorf("err = %v; want errors.Is(err, ErrLookupFailure)", err)
		}
		if metrics.UnresolvedFor(repoID) != 0 {
			t.Errorf("unresolved counter incremented on backend error: %d",
				metrics.UnresolvedFor(repoID))
		}
	})

	t.Run("location lookup error propagates", func(t *testing.T) {
		fl := newFakeLookup()
		fl.injectedLocErr = errors.New("query timeout")
		metrics := NewMetrics()
		r := New(fl, metrics, nil)

		_, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeFilepath: "svc/handler.go",
				AttrCodeLineno:   "42",
			},
		})
		if err == nil {
			t.Fatal("Resolve must return the backend error")
		}
		if !errors.Is(err, ErrLookupFailure) {
			t.Errorf("err = %v; want errors.Is(err, ErrLookupFailure)", err)
		}
		if metrics.UnresolvedFor(repoID) != 0 {
			t.Errorf("unresolved counter incremented on backend error: %d",
				metrics.UnresolvedFor(repoID))
		}
	})

	t.Run("block lookup error is logged but does not invalidate Method", func(t *testing.T) {
		fl := newFakeLookup()
		fl.addMethod(repoID, "pkg", "Foo.bar", MethodCandidate{
			NodeID: "node-bar", CanonicalSignature: "pkg.Foo#bar",
			FilePath: "pkg/foo.go", BodyStartLine: 1, BodyEndLine: 100,
		})
		fl.injectedBlockErr = errors.New("transient query failure")
		r := New(fl, NewMetrics(), nil)

		got, err := r.Resolve(context.Background(), Span{
			RepoID: repoID,
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg",
				AttrCodeFunction:  "Foo.bar",
				AttrCodeLineno:    "50",
			},
		})
		if err != nil {
			t.Fatalf("Resolve must not propagate block lookup error: %v", err)
		}
		if got.Status != StatusMethod || got.Method == nil {
			t.Fatalf("Status=%v Method=%+v; want StatusMethod fallback", got.Status, got.Method)
		}
		if got.Block != nil {
			t.Errorf("Block=%+v; want nil on block lookup failure", got.Block)
		}
	})
}

// Filepath normalization: Windows-style separators and `./`
// prefix must be normalized BEFORE the lookup is consulted, so
// the production binding can hash directly into its filepath
// index (rubber-duck blocker #1).
func TestResolve_filepathNormalization(t *testing.T) {
	t.Parallel()
	repoID := "repo-fp"
	fl := newFakeLookup()
	fl.methodsByFile[fileKey(repoID, "svc/handler.go")] = []MethodCandidate{{
		NodeID: "node-handler", CanonicalSignature: "svc/handler",
		FilePath: "svc/handler.go", BodyStartLine: 1, BodyEndLine: 1000,
	}}
	r := New(fl, NewMetrics(), nil)

	cases := []struct {
		name string
		fp   string
	}{
		{"forward slashes", "svc/handler.go"},
		{"backslashes (Windows)", `svc\handler.go`},
		{"leading ./", "./svc/handler.go"},
		{"redundant segments", "svc/./handler.go"},
		{"backslashes plus leading .\\", `.\svc\handler.go`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := r.Resolve(context.Background(), Span{
				RepoID: repoID,
				Attributes: map[string]string{
					AttrCodeFilepath: c.fp,
					AttrCodeLineno:   "5",
				},
			})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Status != StatusMethod || got.Method == nil || got.Method.NodeID != "node-handler" {
				t.Fatalf("input %q: Status=%v Method=%+v; want StatusMethod node-handler",
					c.fp, got.Status, got.Method)
			}
		})
	}
}

// Empty filepath after normalization (e.g. just `./`) is not a
// valid filepath rung and must not generate a lookup call.
func TestResolve_emptyFilepathSkipsLocationLookup(t *testing.T) {
	t.Parallel()
	repoID := "repo-empty-fp"
	fl := newFakeLookup()
	r := New(fl, NewMetrics(), nil)

	_, err := r.Resolve(context.Background(), Span{
		RepoID: repoID,
		Attributes: map[string]string{
			AttrCodeFilepath: "./",
			AttrCodeLineno:   "42",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if fl.locationCallCount != 0 {
		t.Errorf("location lookup called %d times on empty filepath; want 0",
			fl.locationCallCount)
	}
}

// Per-repo metrics are independent: filling the counter for one
// repo MUST NOT bleed into another.  Concurrent increments are
// safe and lossless.
func TestMetrics_perRepoIsolatedAndConcurrent(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	const workers = 16
	const perWorker = 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			repoID := fmt.Sprintf("repo-%d", w%4)
			for i := 0; i < perWorker; i++ {
				m.IncUnresolved(repoID)
			}
		}()
	}
	wg.Wait()
	// 4 repos, each touched by 4 workers, each worker adds
	// perWorker.  Total per repo = 4 * perWorker = 400.
	for repo := 0; repo < 4; repo++ {
		got := m.UnresolvedFor(fmt.Sprintf("repo-%d", repo))
		want := int64(workers / 4 * perWorker)
		if got != want {
			t.Errorf("UnresolvedFor(repo-%d) = %d, want %d", repo, got, want)
		}
	}
	// A repo that was never touched returns 0.
	if got := m.UnresolvedFor("unknown"); got != 0 {
		t.Errorf("UnresolvedFor(unknown) = %d, want 0", got)
	}
}

// Nil Lookup panics on construction (defence-in-depth — a
// nil-Lookup Resolver would NPE on the first Resolve call,
// hiding the wiring bug behind a confusing stack trace).
func TestNew_panicsOnNilLookup(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil, ...) must panic")
		}
	}()
	_ = New(nil, NewMetrics(), nil)
}

// Nil Metrics is tolerated — the resolver runs without counter
// tracking (ad-hoc tools shape).
func TestNew_nilMetricsTolerated(t *testing.T) {
	t.Parallel()
	fl := newFakeLookup()
	r := New(fl, nil, nil)
	// Must not panic.
	if _, err := r.Resolve(context.Background(), Span{}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

// Unit tests for the parameter normalizer.  These guard the
// signature-match logic against regressions in the parsing
// path used to compare `code.signature` against MethodCandidate
// ParamSignature.
//
// Expected values mirror `ast.NormalizeSignature` output — the
// SAME normalizer `dispatcher.methodSignature` invokes on the
// candidate side.  Whitespace adjacent to canonical punctuation
// (`,`/`(`/`)`/`<`/`>` etc.) is removed, not preserved.  See
// evaluator iter-1 finding #2.
func TestNormalizeParams(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"int", "int"},
		{"(int)", "int"},
		{"bar(int)", "int"},
		{"Foo.bar(int, string)", "int,string"},
		{"  int ,  string  ", "int,string"},
		{"", ""},
		{"  ", ""},
		{"foo()", ""},
		{"(  int   ,   string  )", "int,string"},
		// Generics and nested parens — the LAST `(` strips so
		// generics on the param-type side survive.
		{"bar(Map<K, V>, int)", "Map<K,V>,int"},
		// Newlines and tabs collapse the same way the
		// canonical normalizer collapses them.
		{"\tint ,\n string\t", "int,string"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			got := normalizeParams(c.in)
			if got != c.want {
				t.Errorf("normalizeParams(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Multi-parameter overload disambiguation must work when the
// candidate's ParamSignature is in repo-canonical form (built
// via ast.NormalizeSignature — i.e. spaces stripped around
// punctuation) and the observed OTel `code.signature` is in
// human-emitted form (spaces around `,`).  This is the
// regression guard for evaluator iter-1 finding #2: when
// normalizeParams only collapsed whitespace runs, the observed
// `(int, string)` normalized to `int, string` and never
// compared equal to the canonical `int,string`.
func TestResolve_signatureMatchUsesCanonicalNormalization(t *testing.T) {
	t.Parallel()
	repoID := "repo-canon"
	fl := newFakeLookup()
	// Both candidates carry repo-canonical ParamSignatures
	// (no whitespace around the comma) — exactly what
	// dispatcher.methodSignature mints into the graph.
	fl.addMethod(repoID, "pkg", "Svc.Do", MethodCandidate{
		NodeID:             "node-int-string",
		CanonicalSignature: "pkg.Svc#Do(int,string)",
		FilePath:           "pkg/svc.go",
		ParamSignature:     "int,string",
		BodyStartLine:      10,
		BodyEndLine:        20,
	})
	fl.addMethod(repoID, "pkg", "Svc.Do", MethodCandidate{
		NodeID:             "node-int-bool",
		CanonicalSignature: "pkg.Svc#Do(int,bool)",
		FilePath:           "pkg/svc.go",
		ParamSignature:     "int,bool",
		BodyStartLine:      30,
		BodyEndLine:        40,
	})
	r := New(fl, NewMetrics(), nil)

	// Observed signature comes in human-emitted form with
	// spaces — must still match the canonical `int,string`.
	cases := []struct {
		name        string
		observedSig string
		wantNodeID  string
	}{
		{"bare params with spaces", "(int, string)", "node-int-string"},
		{"method+params with spaces", "Do(int, string)", "node-int-string"},
		{"FQN with spaces", "pkg.Svc.Do(int, string)", "node-int-string"},
		{"already-canonical", "(int,string)", "node-int-string"},
		{"tabs in place of spaces", "(int,\tstring)", "node-int-string"},
		{"picks bool overload", "(int, bool)", "node-int-bool"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := r.Resolve(context.Background(), Span{
				RepoID: repoID,
				Attributes: map[string]string{
					AttrCodeNamespace: "pkg",
					AttrCodeFunction:  "Svc.Do",
					AttrCodeSignature: c.observedSig,
				},
			})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Status != StatusMethod || got.Method == nil || got.Method.NodeID != c.wantNodeID {
				t.Fatalf("observed=%q: Status=%v Method=%+v; want StatusMethod %s",
					c.observedSig, got.Status, got.Method, c.wantNodeID)
			}
		})
	}
}

// Lineno parsing rejects empty, non-numeric, and non-positive
// values; accepts positive integers possibly surrounded by
// whitespace.
func TestParseLineno(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		wantN  int
		wantOK bool
	}{
		{"", 0, false},
		{"  ", 0, false},
		{"42", 42, true},
		{"  42  ", 42, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"1.5", 0, false},
		{"forty-two", 0, false},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("%q", c.in), func(t *testing.T) {
			n, ok := parseLineno(c.in)
			if n != c.wantN || ok != c.wantOK {
				t.Errorf("parseLineno(%q) = (%d, %v), want (%d, %v)",
					c.in, n, ok, c.wantN, c.wantOK)
			}
		})
	}
}

// String renderings are useful in logs and test failure
// messages; pin them so a future refactor cannot silently change
// the wire form a log scraper might depend on.
func TestResolutionStatusString(t *testing.T) {
	t.Parallel()
	cases := map[ResolutionStatus]string{
		StatusUnresolved: "unresolved",
		StatusMethod:     "method",
		StatusBlock:      "block",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%v.String() = %q, want %q", int(s), got, want)
		}
	}
}

func TestResolutionReasonString(t *testing.T) {
	t.Parallel()
	cases := map[ResolutionReason]string{
		ReasonUnset:                "unset",
		ReasonNameMatched:          "name_matched",
		ReasonLocationMatched:      "location_matched",
		ReasonNoNameMatch:          "no_name_match",
		ReasonAmbiguousName:        "ambiguous_name",
		ReasonSignatureMismatch:    "signature_mismatch",
		ReasonMissingAllAttributes: "missing_all_attributes",
		ReasonNoFilepathMatch:      "no_filepath_match",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(r), got, want)
		}
	}
}

func TestBlockOutcomeString(t *testing.T) {
	t.Parallel()
	cases := map[BlockOutcome]string{
		BlockOutcomeNotAttempted:      "not_attempted",
		BlockOutcomeMatched:           "matched",
		BlockOutcomeNoLineno:          "no_lineno",
		BlockOutcomeLinenoUnparseable: "lineno_unparseable",
		BlockOutcomeOutsideBlock:      "outside_block",
		BlockOutcomeLookupFailed:      "lookup_failed",
	}
	for o, want := range cases {
		if got := o.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(o), got, want)
		}
	}
}
