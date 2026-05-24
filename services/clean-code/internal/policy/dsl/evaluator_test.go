package dsl

import (
	"errors"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
)

// sampleSRPClass is the canonical "wide class with LCOM4=12"
// sample used across the evaluator tests.
func sampleSRPClass() Sample {
	return Sample{
		SampleID:      uuid.Must(uuid.NewV4()),
		RepoID:        uuid.Must(uuid.NewV4()),
		SHA:           "abc123",
		ScopeID:       uuid.Must(uuid.NewV4()),
		ScopeKind:     "class",
		MetricKind:    "lcom4",
		MetricVersion: 1,
		Value:         12,
		HasValue:      true,
		Pack:          "solid",
		Source:        "computed",
	}
}

// sampleFanInMethod returns a fan_in sample that does NOT
// match the SRP rule.
func sampleFanInMethod() Sample {
	return Sample{
		SampleID:      uuid.Must(uuid.NewV4()),
		RepoID:        uuid.Must(uuid.NewV4()),
		SHA:           "abc123",
		ScopeID:       uuid.Must(uuid.NewV4()),
		ScopeKind:     "method",
		MetricKind:    "fan_in",
		MetricVersion: 1,
		Value:         50,
		HasValue:      true,
		Pack:          "solid",
		Source:        "computed",
	}
}

func TestEval_SimpleEquality(t *testing.T) {
	t.Parallel()
	pred, err := Compile("metric_kind == 'lcom4'", nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if ok, err := pred.Eval(sampleSRPClass()); err != nil || !ok {
		t.Errorf("lcom4 sample: got (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := pred.Eval(sampleFanInMethod()); err != nil || ok {
		t.Errorf("fan_in sample: got (%v, %v), want (false, nil)", ok, err)
	}
}

func TestEval_AndComposition(t *testing.T) {
	t.Parallel()
	src := "metric_kind == 'lcom4' AND scope_kind == 'class' AND value > 10"
	pred, err := Compile(src, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	ok, err := pred.Eval(sampleSRPClass())
	if err != nil || !ok {
		t.Errorf("matching sample: got (%v, %v), want (true, nil)", ok, err)
	}
	// Drop the value below threshold -> fails.
	s := sampleSRPClass()
	s.Value = 5
	ok, err = pred.Eval(s)
	if err != nil || ok {
		t.Errorf("below threshold: got (%v, %v), want (false, nil)", ok, err)
	}
}

func TestEval_OrComposition(t *testing.T) {
	t.Parallel()
	src := "metric_kind == 'fan_in' OR metric_kind == 'fan_out'"
	pred, err := Compile(src, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	s := sampleFanInMethod()
	if ok, err := pred.Eval(s); err != nil || !ok {
		t.Errorf("fan_in: got (%v, %v), want (true, nil)", ok, err)
	}
	s.MetricKind = "fan_out"
	if ok, err := pred.Eval(s); err != nil || !ok {
		t.Errorf("fan_out: got (%v, %v), want (true, nil)", ok, err)
	}
	s.MetricKind = "lcom4"
	if ok, err := pred.Eval(s); err != nil || ok {
		t.Errorf("lcom4: got (%v, %v), want (false, nil)", ok, err)
	}
}

func TestEval_NotInverts(t *testing.T) {
	t.Parallel()
	pred, err := Compile("NOT (metric_kind == 'lcom4')", nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if ok, _ := pred.Eval(sampleSRPClass()); ok {
		t.Errorf("NOT (lcom4) on lcom4 sample: want false")
	}
	if ok, _ := pred.Eval(sampleFanInMethod()); !ok {
		t.Errorf("NOT (lcom4) on fan_in sample: want true")
	}
}

func TestEval_PrecedenceNotAndOr(t *testing.T) {
	t.Parallel()
	// `NOT A AND B` should be `(NOT A) AND B` -- NOT binds
	// tighter than AND.
	src := "NOT metric_kind == 'fan_in' AND value > 10"
	pred, err := Compile(src, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// SRP class: metric_kind=lcom4 (so NOT matches), value=12>10 -> true.
	if ok, _ := pred.Eval(sampleSRPClass()); !ok {
		t.Errorf("expected true on SRP class")
	}
	// fan_in method: NOT fan_in -> false -> AND short-circuits to false.
	if ok, _ := pred.Eval(sampleFanInMethod()); ok {
		t.Errorf("expected false on fan_in method")
	}

	// `A OR B AND C` should be `A OR (B AND C)`.
	src2 := "metric_kind == 'lcom4' OR scope_kind == 'method' AND value > 100"
	pred2, err := Compile(src2, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// SRP class: lcom4 -> true (first disjunct).
	if ok, _ := pred2.Eval(sampleSRPClass()); !ok {
		t.Errorf("expected true on SRP class")
	}
	// fan_in method, value=50: scope_kind=method but value<100 -> AND=false; first disjunct fan_in!=lcom4 -> false; overall false.
	if ok, _ := pred2.Eval(sampleFanInMethod()); ok {
		t.Errorf("expected false on fan_in method with value=50")
	}
}

func TestEval_DegradedRowMissingValue(t *testing.T) {
	t.Parallel()
	// A degraded sample with HasValue=false should NOT
	// match a value comparison (returns false silently, no
	// error -- the rule engine separately records the
	// degraded flag).
	pred, err := Compile("value > 10", nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	s := sampleSRPClass()
	s.HasValue = false
	s.Degraded = true
	s.DegradedReason = "samples_pending"
	if ok, err := pred.Eval(s); err != nil || ok {
		t.Errorf("missing value: got (%v, %v), want (false, nil)", ok, err)
	}
}

// TestEval_ThresholdAtom covers the threshold('<uuid>') atom
// shape: it triple-checks metric_kind, scope_kind, and value
// against the [Threshold] row before returning true.
func TestEval_ThresholdAtom(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV4())
	resolver := MapResolver{
		id: Threshold{
			ThresholdID: id,
			MetricKind:  "lcom4",
			ScopeKind:   "class",
			Op:          OpGE,
			Value:       10,
		},
	}
	src := "threshold('" + id.String() + "')"
	pred, err := Compile(src, resolver)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// Matching sample (lcom4 class, value=12>=10).
	if ok, _ := pred.Eval(sampleSRPClass()); !ok {
		t.Errorf("matching sample: want true")
	}
	// Value below threshold.
	s := sampleSRPClass()
	s.Value = 5
	if ok, _ := pred.Eval(s); ok {
		t.Errorf("value=5 < 10: want false")
	}
	// metric_kind mismatch (fan_in not lcom4) -> false.
	if ok, _ := pred.Eval(sampleFanInMethod()); ok {
		t.Errorf("fan_in sample: want false (metric_kind mismatch)")
	}
	// scope_kind mismatch (method not class) -> false.
	s2 := sampleSRPClass()
	s2.ScopeKind = "method"
	if ok, _ := pred.Eval(s2); ok {
		t.Errorf("scope_kind=method: want false")
	}
}

// TestEval_ThresholdCoversAllOps verifies each ThresholdOp.
func TestEval_ThresholdCoversAllOps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		op   ThresholdOp
		val  float64
		want bool
	}{
		{OpGT, 11, true},
		{OpGT, 12, false},
		{OpGE, 12, true},
		{OpGE, 13, false},
		{OpLT, 13, true},
		{OpLT, 12, false},
		{OpLE, 12, true},
		{OpLE, 11, false},
		{OpEQ, 12, true},
		{OpEQ, 11, false},
	}
	for _, c := range cases {
		c := c
		t.Run(string(c.op)+"_threshold_"+c.op.String(), func(t *testing.T) {
			t.Parallel()
			id := uuid.Must(uuid.NewV4())
			resolver := MapResolver{
				id: Threshold{
					ThresholdID: id,
					MetricKind:  "lcom4",
					ScopeKind:   "class",
					Op:          c.op,
					Value:       c.val,
				},
			}
			pred, err := Compile("threshold('"+id.String()+"')", resolver)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			s := sampleSRPClass() // Value=12
			got, _ := pred.Eval(s)
			if got != c.want {
				t.Errorf("op=%s value=%v threshold=%v: got %v, want %v",
					c.op, s.Value, c.val, got, c.want)
			}
		})
	}
}

// String exposes ThresholdOp for the subtest name in
// TestEval_ThresholdCoversAllOps.
func (op ThresholdOp) String() string { return string(op) }

func TestBind_RejectsUnknownThresholdUUID(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV4())
	other := uuid.Must(uuid.NewV4())
	resolver := MapResolver{
		other: Threshold{
			ThresholdID: other,
			MetricKind:  "lcom4",
			ScopeKind:   "class",
			Op:          OpGE,
			Value:       10,
		},
	}
	src := "threshold('" + id.String() + "')"
	_, err := Compile(src, resolver)
	if err == nil {
		t.Fatalf("expected bind error for unknown uuid")
	}
	if !errors.Is(err, ErrBind) || !errors.Is(err, ErrUnknownThreshold) {
		t.Errorf("err=%v, want ErrBind+ErrUnknownThreshold", err)
	}
	var dsErr *Error
	if !errors.As(err, &dsErr) {
		t.Fatalf("err=%T %v, want *dsl.Error", err, err)
	}
	if dsErr.Pos.IsZero() {
		t.Errorf("err has zero Position; want line/column")
	}
}

func TestBind_RejectsMalformedUUID(t *testing.T) {
	t.Parallel()
	_, err := Compile("threshold('not-a-uuid')", MapResolver{})
	if err == nil {
		t.Fatalf("expected bind error")
	}
	if !errors.Is(err, ErrBind) {
		t.Errorf("err=%v, want ErrBind", err)
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err=%v, want substring 'not a valid UUID'", err)
	}
}

func TestBind_NilResolverWithThresholdRefIsError(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV4())
	_, err := Compile("threshold('"+id.String()+"')", nil)
	if err == nil {
		t.Fatalf("expected bind error when threshold present but resolver is nil")
	}
	if !errors.Is(err, ErrBind) {
		t.Errorf("err=%v, want ErrBind", err)
	}
}

func TestBind_NilResolverWithoutThresholdRefIsOK(t *testing.T) {
	t.Parallel()
	// A predicate with no threshold() atom must compile
	// with a nil resolver -- avoids forcing every rule
	// owner to synthesise an empty resolver.
	pred, err := Compile("metric_kind == 'lcom4'", nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if ok, err := pred.Eval(sampleSRPClass()); err != nil || !ok {
		t.Errorf("got (%v, %v), want (true, nil)", ok, err)
	}
}

func TestBind_InvalidThresholdMetricKindRejected(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV4())
	// A Threshold row that somehow landed with a
	// non-canonical metric_kind is rejected at Bind time --
	// belt-and-braces against an upstream insert bug.
	resolver := MapResolver{
		id: Threshold{
			ThresholdID: id,
			MetricKind:  "lines_of_code", // non-canonical
			ScopeKind:   "class",
			Op:          OpGE,
			Value:       10,
		},
	}
	_, err := Compile("threshold('"+id.String()+"')", resolver)
	if err == nil {
		t.Fatalf("expected bind error")
	}
	if !errors.Is(err, ErrBind) {
		t.Errorf("err=%v, want ErrBind", err)
	}
}

// TestBind_RejectsMismatchedThresholdID guards against a
// stale or mis-keyed [ThresholdResolver] returning a
// [Threshold] whose ThresholdID does not match the UUID
// requested by `threshold('<uuid>')`. Threshold rows are
// uniquely keyed by their own threshold_id (architecture G3:
// rows are immutable), so any mismatch indicates an upstream
// bug -- silently binding the wrong row would let an
// evaluator gate the wrong predicate.
//
// This is the evaluator-feedback regression guard for
// iter 2 / finding 2.
func TestBind_RejectsMismatchedThresholdID(t *testing.T) {
	t.Parallel()
	requested := uuid.Must(uuid.NewV4())
	actual := uuid.Must(uuid.NewV4())
	// Resolver indexes by `requested` but the row inside
	// carries `actual` as ThresholdID -- the kind of bug a
	// hand-rolled adapter could ship.
	resolver := MapResolver{
		requested: Threshold{
			ThresholdID: actual,
			MetricKind:  "lcom4",
			ScopeKind:   "class",
			Op:          OpGE,
			Value:       10,
		},
	}
	_, err := Compile("threshold('"+requested.String()+"')", resolver)
	if err == nil {
		t.Fatalf("expected bind error for mismatched threshold_id")
	}
	if !errors.Is(err, ErrBind) {
		t.Errorf("err=%v, want ErrBind", err)
	}
	if !strings.Contains(err.Error(), "mismatched threshold_id") {
		t.Errorf("err=%v, want substring 'mismatched threshold_id'", err)
	}
	var dsErr *Error
	if !errors.As(err, &dsErr) {
		t.Fatalf("err=%T %v, want *dsl.Error", err, err)
	}
	if dsErr.Pos.IsZero() {
		t.Errorf("err has zero Position; want line/column")
	}
}

// TestEval_DeterministicSameInputs is the
// `dsl-deterministic` Stage 5.4 test scenario: the same
// predicate evaluated twice over the same sample MUST return
// the same boolean.
func TestEval_DeterministicSameInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		s    Sample
	}{
		{"lcom4_match", "metric_kind == 'lcom4' AND value > 10", sampleSRPClass()},
		{"or_chain", "metric_kind == 'fan_in' OR metric_kind == 'fan_out'", sampleFanInMethod()},
		{"not_branch", "NOT (metric_kind == 'lcom4')", sampleSRPClass()},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			pred, err := Compile(c.src, nil)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			first, err1 := pred.Eval(c.s)
			second, err2 := pred.Eval(c.s)
			if err1 != nil || err2 != nil {
				t.Fatalf("Eval errs: %v / %v", err1, err2)
			}
			if first != second {
				t.Errorf("non-deterministic: first=%v second=%v", first, second)
			}
		})
	}
}

// TestEval_UnboundThresholdNodeErrors guards against a code
// path that constructs an AST manually and skips Bind.
// Compile is the only public entry that always Binds, but
// we still want Eval to fail loudly if a caller hand-rolls
// an AST with a ThresholdNode whose Bound is nil.
func TestEval_UnboundThresholdNodeErrors(t *testing.T) {
	t.Parallel()
	node := ThresholdNode{
		nodeBase: nodeBase{pos: Position{Line: 1, Column: 1}},
		IDText:   "11111111-1111-1111-1111-111111111111",
	}
	pred := &Predicate{root: node}
	_, err := pred.Eval(sampleSRPClass())
	if err == nil {
		t.Fatalf("expected error on unbound ThresholdNode")
	}
	if !errors.Is(err, ErrBind) {
		t.Errorf("err=%v, want ErrBind", err)
	}
}

// TestEval_FieldOnFieldComparison covers a comparison whose
// BOTH sides are fields -- e.g. `value == value` (always
// true on present-value rows). Exercises the operand
// dispatcher's two-field code path.
func TestEval_FieldOnFieldComparison(t *testing.T) {
	t.Parallel()
	pred, err := Compile("metric_kind == metric_kind", nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	ok, err := pred.Eval(sampleSRPClass())
	if err != nil || !ok {
		t.Errorf("got (%v, %v), want (true, nil)", ok, err)
	}
}

// TestEval_StringOnLeft covers swapping the field/lit order
// in the canon-guard.
func TestEval_StringOnLeft(t *testing.T) {
	t.Parallel()
	pred, err := Compile("'lcom4' == metric_kind", nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if ok, _ := pred.Eval(sampleSRPClass()); !ok {
		t.Errorf("want true on SRP class sample")
	}

	// Canon-guard still fires when the literal is on the left.
	_, err = Parse("'lines_of_code' == metric_kind")
	if err == nil {
		t.Fatalf("expected canon-guard to reject 'lines_of_code'")
	}
	if !errors.Is(err, ErrSemantic) {
		t.Errorf("err=%v, want ErrSemantic", err)
	}
}

// TestEval_BoolLiteralOnLeft confirms that `false ==
// degraded` and `true == false` (bool literal on the LHS of
// a comparison) parse AND evaluate correctly. The prior
// iter rejected those as trailing junk; this test is the
// regression guard for iter 2 / finding 1.
func TestEval_BoolLiteralOnLeft(t *testing.T) {
	t.Parallel()
	// false == degraded -- true iff sample is NOT degraded.
	pred1, err := Compile("false == degraded", nil)
	if err != nil {
		t.Fatalf("Compile(false == degraded): %v", err)
	}
	s := sampleSRPClass() // Degraded=false (zero value)
	if ok, err := pred1.Eval(s); err != nil || !ok {
		t.Errorf("false == degraded on non-degraded sample: got (%v, %v), want (true, nil)", ok, err)
	}
	s.Degraded = true
	if ok, err := pred1.Eval(s); err != nil || ok {
		t.Errorf("false == degraded on degraded sample: got (%v, %v), want (false, nil)", ok, err)
	}

	// true == false -- constant false.
	pred2, err := Compile("true == false", nil)
	if err != nil {
		t.Fatalf("Compile(true == false): %v", err)
	}
	if ok, err := pred2.Eval(sampleSRPClass()); err != nil || ok {
		t.Errorf("true == false: got (%v, %v), want (false, nil)", ok, err)
	}

	// true == true -- constant true.
	pred3, err := Compile("true == true", nil)
	if err != nil {
		t.Fatalf("Compile(true == true): %v", err)
	}
	if ok, err := pred3.Eval(sampleSRPClass()); err != nil || !ok {
		t.Errorf("true == true: got (%v, %v), want (true, nil)", ok, err)
	}
}
