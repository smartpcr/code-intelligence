package dsl

import (
	"strings"
	"testing"

	"github.com/gofrs/uuid"
)

// TestThreshold_Validate_AcceptsAllCanonicalMetricKinds pins
// the closed metric_kind set: every metric_kind member of
// the canonical catalogue (implementation-plan lines 30-32)
// MUST validate cleanly when paired with a canonical
// scope_kind and a valid op. Regression guard against the
// canonical set drifting out of sync with the merged
// planning artifacts (iter-2 finding: `coverage_line_ratio`
// + `coverage_branch_ratio` were missing).
func TestThreshold_Validate_AcceptsAllCanonicalMetricKinds(t *testing.T) {
	t.Parallel()
	for _, mk := range ListCanonicalMetricKinds() {
		mk := mk
		t.Run(mk, func(t *testing.T) {
			t.Parallel()
			id := uuid.Must(uuid.NewV4())
			th := Threshold{
				ThresholdID: id,
				MetricKind:  mk,
				ScopeKind:   "repo",
				Op:          OpGE,
				Value:       0,
			}
			if err := th.Validate(); err != nil {
				t.Errorf("Validate(%q) = %v, want nil", mk, err)
			}
		})
	}
}

// TestThreshold_Validate_RejectsLegacyCoverageAliases is the
// counter-invariant: the legacy aliases `coverage_line` and
// `coverage_branch` MUST be rejected even though the
// canonical set now includes the `_ratio`-suffixed names.
// Implementation-plan line 31 lists both bare names in the
// "NEVER written" negative clause.
func TestThreshold_Validate_RejectsLegacyCoverageAliases(t *testing.T) {
	t.Parallel()
	for _, alias := range []string{"coverage_line", "coverage_branch"} {
		alias := alias
		t.Run(alias, func(t *testing.T) {
			t.Parallel()
			th := Threshold{
				ThresholdID: uuid.Must(uuid.NewV4()),
				MetricKind:  alias,
				ScopeKind:   "file",
				Op:          OpGE,
				Value:       0,
			}
			err := th.Validate()
			if err == nil {
				t.Fatalf("Validate(%q) = nil, want error", alias)
			}
			if !strings.Contains(err.Error(), "not in the canonical set") {
				t.Errorf("Validate(%q) err = %q, want substring 'not in the canonical set'", alias, err.Error())
			}
		})
	}
}

// TestCompile_CoverageRatioThresholdEndToEnd exercises a
// realistic coverage-policy shape (Stage 5.4 brief: thresholds
// reference Threshold rows; predicates apply over MetricSample
// rows). Builds a `threshold('<uuid>')` predicate keyed off
// `coverage_line_ratio < 0.8` and evaluates it against both
// a passing and a failing Sample.
func TestCompile_CoverageRatioThresholdEndToEnd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		metricKind string
		op         ThresholdOp
		threshold  float64
		sampleVal  float64
		wantMatch  bool
	}{
		{
			name:       "line_ratio_below_threshold_matches",
			metricKind: "coverage_line_ratio",
			op:         OpLT,
			threshold:  0.8,
			sampleVal:  0.65,
			wantMatch:  true,
		},
		{
			name:       "line_ratio_at_or_above_threshold_no_match",
			metricKind: "coverage_line_ratio",
			op:         OpLT,
			threshold:  0.8,
			sampleVal:  0.9,
			wantMatch:  false,
		},
		{
			name:       "branch_ratio_below_threshold_matches",
			metricKind: "coverage_branch_ratio",
			op:         OpLT,
			threshold:  0.6,
			sampleVal:  0.4,
			wantMatch:  true,
		},
		{
			name:       "branch_ratio_at_or_above_threshold_no_match",
			metricKind: "coverage_branch_ratio",
			op:         OpLT,
			threshold:  0.6,
			sampleVal:  0.85,
			wantMatch:  false,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			id := uuid.Must(uuid.NewV4())
			resolver := MapResolver{
				id: Threshold{
					ThresholdID: id,
					MetricKind:  c.metricKind,
					ScopeKind:   "file",
					Op:          c.op,
					Value:       c.threshold,
				},
			}
			src := "threshold('" + id.String() + "')"
			pred, err := Compile(src, resolver)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			s := Sample{
				SampleID:      uuid.Must(uuid.NewV4()),
				RepoID:        uuid.Must(uuid.NewV4()),
				SHA:           "deadbeef",
				ScopeID:       uuid.Must(uuid.NewV4()),
				ScopeKind:     "file",
				MetricKind:    c.metricKind,
				MetricVersion: 1,
				Value:         c.sampleVal,
				HasValue:      true,
				Pack:          "ingested",
				Source:        "ingested",
			}
			got, evalErr := pred.Eval(s)
			if evalErr != nil {
				t.Fatalf("Eval: %v", evalErr)
			}
			if got != c.wantMatch {
				t.Errorf("Eval = %v, want %v", got, c.wantMatch)
			}
		})
	}
}

// TestCompile_RejectsLegacyCoverageAlias_AtParseTime guards
// the canon-guard at the parse phase: a predicate referring
// to `coverage_line` / `coverage_branch` (the legacy bare
// names) MUST be rejected with ErrSemantic before reaching
// Bind. Mirrors the malformed-input case in parser_test but
// asserts via the full Compile() entry point so a future
// refactor of the parser surface still keeps the canon-guard.
func TestCompile_RejectsLegacyCoverageAlias_AtParseTime(t *testing.T) {
	t.Parallel()
	for _, alias := range []string{"coverage_line", "coverage_branch"} {
		alias := alias
		t.Run(alias, func(t *testing.T) {
			t.Parallel()
			_, err := Compile("metric_kind == '"+alias+"'", nil)
			if err == nil {
				t.Fatalf("Compile(%q): want error", alias)
			}
			if !strings.Contains(err.Error(), "unknown metric_kind") {
				t.Errorf("Compile(%q) err = %v, want substring 'unknown metric_kind'", alias, err.Error())
			}
		})
	}
}
