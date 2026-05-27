package dsl

import (
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
)

// ThresholdOp is the closed set of comparison operators a
// [Threshold] row may carry. Mirrors the
// `clean_code.threshold.op` ENUM declared in migration 0003
// and architecture Sec 5.3.5 line 1157.
type ThresholdOp string

// Canonical threshold operators. String constants MUST match
// the DB ENUM labels verbatim so a Threshold row read via the
// Steward [SQLStore] is consumable by the evaluator with no
// translation layer.
const (
	OpGT ThresholdOp = "gt"
	OpGE ThresholdOp = "ge"
	OpLT ThresholdOp = "lt"
	OpLE ThresholdOp = "le"
	OpEQ ThresholdOp = "eq"
)

// IsValid reports whether op is a member of the closed
// [ThresholdOp] set.
func (op ThresholdOp) IsValid() bool {
	switch op {
	case OpGT, OpGE, OpLT, OpLE, OpEQ:
		return true
	default:
		return false
	}
}

// Compare evaluates `lhs <op> rhs`, returning the boolean
// result of applying op. The function is pure -- no IO, no
// allocation, no panics on NaN (NaN comparisons return false
// per IEEE 754).
func (op ThresholdOp) Compare(lhs, rhs float64) bool {
	switch op {
	case OpGT:
		return lhs > rhs
	case OpGE:
		return lhs >= rhs
	case OpLT:
		return lhs < rhs
	case OpLE:
		return lhs <= rhs
	case OpEQ:
		return lhs == rhs
	default:
		return false
	}
}

// Threshold is the DSL-local mirror of the [steward.Threshold]
// row from the Policy / rules sub-store (architecture Sec
// 5.3.5). It carries everything the evaluator needs to apply
// a `threshold('<uuid>')` atom against a [Sample] without
// further IO.
//
// Held by reference inside a bound [Predicate]; the Threshold
// itself is immutable (G3) so sharing the pointer across
// goroutines is safe.
type Threshold struct {
	ThresholdID uuid.UUID
	MetricKind  string
	ScopeKind   string
	Op          ThresholdOp
	Value       float64
}

// Validate returns an error if any field is outside the
// closed set the schema enforces. The Steward [SQLStore]
// validates on insert; [Bind] re-validates on lookup so a
// caller who constructs a [Threshold] by hand (e.g. in a
// unit test) cannot bypass the canon-guard.
func (t Threshold) Validate() error {
	if t.ThresholdID == uuid.Nil {
		return errors.New("dsl: threshold: threshold_id must not be Nil")
	}
	if t.MetricKind == "" {
		return errors.New("dsl: threshold: metric_kind must not be empty")
	}
	if !IsCanonicalMetricKind(t.MetricKind) {
		return fmt.Errorf("dsl: threshold: metric_kind %q is not in the canonical set", t.MetricKind)
	}
	if !IsCanonicalScopeKind(t.ScopeKind) {
		return fmt.Errorf("dsl: threshold: scope_kind %q is not in the canonical set", t.ScopeKind)
	}
	if !t.Op.IsValid() {
		return fmt.Errorf("dsl: threshold: op %q is not in the canonical set", t.Op)
	}
	return nil
}

// ThresholdResolver looks up [Threshold] rows by their UUID
// at [Bind] time. The resolver is consulted ONCE per
// compilation; the bound [Predicate] captures the resolved
// pointers and evaluation never touches the resolver. This
// preserves the Stage 5.4 purity invariant ("predicates are
// pure functions over MetricSample rows -- no side effects,
// no IO").
//
// Implementations MUST be deterministic over their input set
// -- the `dsl-deterministic` Stage 5.4 test scenario relies
// on this. A simple in-memory map is the canonical shape;
// the Rule Engine wraps the policy's [ThresholdRef] slice in
// a [MapResolver].
type ThresholdResolver interface {
	// Lookup returns the Threshold row for id, or
	// ErrUnknownThreshold if id is not present.
	Lookup(id uuid.UUID) (Threshold, error)
}

// ErrUnknownThreshold is returned by [ThresholdResolver]
// implementations when the requested threshold_id is not in
// the active policy's [PolicyVersion.ThresholdRefs] set.
// [Bind] wraps this into a structured [Error] with the
// offending source position.
var ErrUnknownThreshold = errors.New("dsl: threshold_id not registered with this policy version")

// MapResolver is the canonical in-memory [ThresholdResolver].
// Used by tests and by the Rule Engine after it loads the
// policy's [Threshold] rows from [steward.SQLStore].
type MapResolver map[uuid.UUID]Threshold

// Lookup implements [ThresholdResolver].
func (m MapResolver) Lookup(id uuid.UUID) (Threshold, error) {
	t, ok := m[id]
	if !ok {
		return Threshold{}, fmt.Errorf("%w: %s", ErrUnknownThreshold, id)
	}
	return t, nil
}
