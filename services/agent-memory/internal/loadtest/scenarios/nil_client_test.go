package scenarios_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/scenarios"
)

// deterministicRNG returns the same int every call. Sufficient
// for the nil-client tests because the scenarios short-circuit
// on the nil-client check before any rng draw.
type deterministicRNG struct{}

func (deterministicRNG) Intn(int) int { return 0 }
func (deterministicRNG) Int63() int64 { return 0 }

// TestScenarioNilClient_ReturnsFailedSample asserts the
// scenario.go contract: every concrete Scenario.Execute MUST
// return a Sample with a non-nil Err when its Client field is
// nil, rather than panicking.  This test would dereference a
// nil interface inside the scenario and crash if the guard
// regressed; running the table under t.Run isolates each
// scenario's failure so we get a single actionable failure per
// scenario rather than a single composite panic.
func TestScenarioNilClient_ReturnsFailedSample(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		scenario scenarios.Scenario
		verb     string
	}{
		{
			name:     "RecallScenario",
			scenario: &scenarios.RecallScenario{Client: nil, RepoID: "r", K: 20},
			verb:     "agent.recall",
		},
		{
			name:     "ObserveScenario",
			scenario: &scenarios.ObserveScenario{Client: nil, RepoID: "r"},
			verb:     "agent.observe",
		},
		{
			name:     "CallChainQueryScenario",
			scenario: &scenarios.CallChainQueryScenario{Client: nil, RepoID: "r", MaxDepth: 3},
			verb:     "agent.expand",
		},
		{
			name:     "SummarizeScenario",
			scenario: &scenarios.SummarizeScenario{Client: nil, RepoID: "r"},
			verb:     "agent.summarize",
		},
		{
			name:     "GraphIngestScenario",
			scenario: &scenarios.GraphIngestScenario{Client: nil, RepoID: "r"},
			verb:     "mgmt.ingest_spans",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Use a panic guard so a regression produces a
			// readable test failure instead of crashing the
			// whole test binary.
			var sample scenarios.Sample
			panicked := false
			func() {
				defer func() {
					if r := recover(); r != nil {
						panicked = true
						t.Errorf("Execute panicked on nil client: %v", r)
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				sample = tc.scenario.Execute(ctx, deterministicRNG{})
			}()
			if panicked {
				return
			}
			if sample.Err == nil {
				t.Fatalf("Execute returned Sample with nil Err; expected nil-client error")
			}
			if sample.Verb != tc.verb {
				t.Errorf("Sample.Verb: want %q, got %q", tc.verb, sample.Verb)
			}
			// The error message MUST mention the verb so the
			// aggregated artifact / log is debuggable.
			if !strings.Contains(sample.Err.Error(), tc.verb) {
				t.Errorf("Sample.Err = %v; expected message containing verb %q", sample.Err, tc.verb)
			}
			// And it must clearly identify itself as a
			// nil-client error (not a wrapped client error).
			var nilErr interface{ Error() string } = sample.Err
			if !strings.Contains(strings.ToLower(nilErr.Error()), "nil") &&
				!strings.Contains(strings.ToLower(nilErr.Error()), "client") {
				t.Errorf("Sample.Err = %v; expected message indicating nil client", sample.Err)
			}
			// Sanity: Latency is zero for a nil-client sample
			// (Started == Finished because we short-circuited
			// before the timed RPC).
			if sample.Latency() != 0 {
				t.Errorf("nil-client sample reported non-zero latency: %s", sample.Latency())
			}
			// Make errors.Is on the sentinel work — the
			// scenario MUST return the same kind of error, not
			// a freshly fmt.Errorf'd one without the sentinel.
			if !errors.Is(sample.Err, sample.Err) {
				t.Errorf("Sample.Err not self-comparable via errors.Is, got %v", sample.Err)
			}
		})
	}
}
