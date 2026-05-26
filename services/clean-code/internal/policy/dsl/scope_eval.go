package dsl

import (
	"fmt"
	"sort"

	"github.com/gofrs/uuid"
)

// ScopeContext is the input to [Predicate.EvalAtScope]. It
// carries every [Sample] that belongs to ONE scope (i.e.
// shares a `scope_id`). The Rule Engine builds one
// ScopeContext per scope at evaluation time and asks the
// predicate to match against the full set.
//
// The predicate is matched at the scope -- NOT at any single
// sample within it. This lets SOLID-class composite recipes
// like SRP (`threshold(lcom4) AND threshold(interface_width)`)
// fire when a class has BOTH a high-LCOM4 sample AND a
// wide-interface sample, even though no single
// [MetricSample] row carries both metric_kinds.
type ScopeContext struct {
	// Samples is the list of [Sample] rows that share the
	// scope. Order does not affect the boolean outcome but
	// is preserved in the returned witness ID list so the
	// caller observes a deterministic finding row.
	Samples []Sample
}

// EvalAtScope evaluates the predicate against a [ScopeContext]
// and returns:
//
//  1. `matched` -- whether the predicate fires for this
//     scope. Equivalent to "does any combination of
//     samples in this scope satisfy the predicate",
//     subject to the per-sample-correlation rules below.
//
//  2. `witnessIDs` -- the deduplicated, sorted set of
//     `Sample.SampleID`s that contributed to the match.
//     This is what the Rule Engine persists into
//     `finding.metric_sample_ids` -- "the EXACT
//     [MetricSample] row(s) that triggered the rule"
//     (architecture Sec 5.4.1 line 1213 + Stage 5.7
//     brief).
//
//  3. `err` -- a malformed AST (e.g. an unbound
//     [ThresholdNode]) returns a structured [*Error].
//     A regular "predicate did not match" outcome is
//     `(false, nil, nil)`.
//
// # Evaluation semantics
//
// The model deliberately distinguishes "per-sample atoms"
// from "cross-sample atoms" so that composite SOLID recipes
// AND ordinary per-sample comparisons both behave correctly:
//
//   - [ThresholdNode] is a CROSS-SAMPLE atom: a bound
//     threshold pins the metric_kind it cares about, so a
//     conjunction of two thresholds for DIFFERENT
//     metric_kinds is legitimately satisfied by two
//     different samples in the same scope.
//
//   - Every other atom ([CompareNode], [FieldNode],
//     [BoolLitNode]) is a PER-SAMPLE atom: it reads the
//     fields of ONE sample, so a conjunction over per-sample
//     atoms MUST share a witness sample (otherwise
//     `metric_kind == 'lcom4' AND value > 5` would
//     misfire when one sample has metric_kind='lcom4'
//     value=1 and a different sample has
//     metric_kind='other' value=10).
//
// # AndNode evaluation -- two phases
//
// Phase 1 (per-sample): try every sample s in the scope; if
// every child of the AND matches against s under the
// per-sample [Predicate.Eval] semantics, the AND matches
// with witness = {s.SampleID}.
//
// Phase 2 (cross-sample): IFF every child of the AND is a
// [ThresholdNode] (i.e. the AND is a pure threshold
// conjunction with no per-sample-correlated atoms), each
// child is independently evaluated at the scope. The AND
// matches iff every child has a non-empty witness set;
// witness = union of each child's witnesses.
//
// Mixed ANDs that combine threshold atoms with per-sample
// comparisons fall back to Phase 1 only -- if Phase 1
// doesn't find a single-sample witness, the AND is false
// even when Phase 2 would have succeeded. This is the
// rubber-duck-flagged false-positive avoidance for rules
// like `metric_kind == 'lcom4' AND value > 5`.
//
// # OrNode / NotNode evaluation
//
//   - OrNode matches if ANY child matches; witness = union
//     of every matching child's witnesses.
//   - NotNode matches iff its child does NOT match at this
//     scope; witness is empty (the negation does not
//     attribute itself to any single sample).
//
// # Leaf atoms (without AndNode)
//
// When a leaf [ThresholdNode] or [CompareNode] appears
// outside an AND, the matcher iterates every sample and
// includes each matching sample in the witness set. This is
// the standard "any sample in scope satisfies the atom"
// semantic.
func (p *Predicate) EvalAtScope(ctx ScopeContext) (bool, []uuid.UUID, error) {
	matched, witnesses, err := evalNodeAtScope(p.root, ctx)
	if err != nil {
		return false, nil, err
	}
	return matched, dedupSortUUIDs(witnesses), nil
}

func evalNodeAtScope(n Node, ctx ScopeContext) (bool, []uuid.UUID, error) {
	switch v := n.(type) {
	case OrNode:
		matched := false
		var witnesses []uuid.UUID
		for _, c := range v.Children {
			ok, w, err := evalNodeAtScope(c, ctx)
			if err != nil {
				return false, nil, err
			}
			if ok {
				matched = true
				witnesses = append(witnesses, w...)
			}
		}
		return matched, witnesses, nil

	case AndNode:
		return evalAndAtScope(v, ctx)

	case NotNode:
		ok, _, err := evalNodeAtScope(v.Child, ctx)
		if err != nil {
			return false, nil, err
		}
		// NOT at scope: matches iff child does not match
		// anywhere in the scope. We deliberately discard
		// the child's witnesses -- a negation does not
		// attribute itself to any sample.
		return !ok, nil, nil

	case BoolLitNode:
		return v.Value, nil, nil

	case CompareNode:
		// Per-sample atom: walk every sample, accumulate
		// matching ones into the witness set.
		var witnesses []uuid.UUID
		for _, s := range ctx.Samples {
			ok, err := evalNode(v, s)
			if err != nil {
				return false, nil, err
			}
			if ok {
				witnesses = append(witnesses, s.SampleID)
			}
		}
		return len(witnesses) > 0, witnesses, nil

	case ThresholdNode:
		if v.Bound == nil {
			return false, nil, newError(ErrBind, v.IDPos,
				"threshold('%s') is not bound; call dsl.Bind before EvalAtScope", v.IDText)
		}
		var witnesses []uuid.UUID
		for _, s := range ctx.Samples {
			if evalThreshold(v.Bound, s) {
				witnesses = append(witnesses, s.SampleID)
			}
		}
		return len(witnesses) > 0, witnesses, nil
	}
	return false, nil, fmt.Errorf("dsl: evalNodeAtScope: unhandled node type %T", n)
}

// evalAndAtScope implements the two-phase AND evaluation
// described in [Predicate.EvalAtScope].
//
// Phase 1 (per-sample) is tried FIRST and short-circuits on
// the first sample that satisfies every child -- this is the
// natural reading of `metric_kind == 'lcom4' AND value > 5`
// and ensures per-sample-correlated conjunctions never
// silently misfire by drawing field values from two
// different samples.
//
// Phase 2 (cross-sample, threshold-only) runs ONLY when
// every AND child is a [ThresholdNode]. Mixed ANDs (some
// thresholds + some per-sample atoms) reject Phase 2 to
// preserve correlation, even though a permissive
// interpretation could find a satisfying assignment.
func evalAndAtScope(n AndNode, ctx ScopeContext) (bool, []uuid.UUID, error) {
	// Phase 1: per-sample shared-witness.
	for _, s := range ctx.Samples {
		allMatch := true
		for _, c := range n.Children {
			ok, err := evalNode(c, s)
			if err != nil {
				return false, nil, err
			}
			if !ok {
				allMatch = false
				break
			}
		}
		if allMatch {
			return true, []uuid.UUID{s.SampleID}, nil
		}
	}
	// Phase 2: cross-sample threshold-only conjunction.
	if !allChildrenAreThresholds(n.Children) {
		return false, nil, nil
	}
	var witnesses []uuid.UUID
	for _, c := range n.Children {
		ok, w, err := evalNodeAtScope(c, ctx)
		if err != nil {
			return false, nil, err
		}
		if !ok {
			return false, nil, nil
		}
		witnesses = append(witnesses, w...)
	}
	return true, witnesses, nil
}

// allChildrenAreThresholds reports whether every node in
// `children` is a [ThresholdNode]. Phase-2 (cross-sample)
// AND is only safe when this is true -- a mixed AND with
// even ONE per-sample atom needs the per-sample correlation
// from Phase 1.
func allChildrenAreThresholds(children []Node) bool {
	for _, c := range children {
		if _, ok := c.(ThresholdNode); !ok {
			return false
		}
	}
	return len(children) > 0
}

// dedupSortUUIDs returns ids with duplicates removed and
// sorted in lexicographic-byte order. Stable order makes
// the finding row deterministic across replays.
func dedupSortUUIDs(ids []uuid.UUID) []uuid.UUID {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		ai := out[i].Bytes()
		bj := out[j].Bytes()
		for k := 0; k < len(ai); k++ {
			if ai[k] != bj[k] {
				return ai[k] < bj[k]
			}
		}
		return false
	})
	return out
}
