// Package reliability owns the §8.3 SLO and load-envelope data
// model that downstream tooling (the Stage 8.4 load-test
// calibration harness, future autoscaler hints, alert rules)
// reads from.
//
// The values here mirror tech-spec.md §8.3 verbatim. They are
// the v1 contract numbers downstream load tests measure
// against. Any change to the locked values is a §8.3 override
// — see tech-spec.md §8.3 "Override route".
//
// Why a separate package (not part of `internal/obs`):
//   - `internal/obs` owns the wire-level metric NAMES and
//     histogram bucket boundaries; `internal/reliability` owns
//     the higher-level SLO + traffic-shape data the harness
//     and the autoscaler reason about. Splitting them keeps
//     `obs` minimal (it has no notion of "envelope" or
//     "verb"), and keeps `reliability` independent of any
//     specific exposition format.
//   - Both packages depend on each other only at the constant
//     level (`obs.MetricAgentRecallDurationSeconds` is mirrored
//     here verbatim via the per-verb `MetricName` field so a
//     consumer that reasons about a `LoadProfile` does not
//     need to import `obs`).
package reliability

import (
	"errors"
	"fmt"
	"time"
)

// LoadProfile names a traffic envelope: a closed set of per-
// verb RPS targets, p95 / p99 SLO budgets, and learning-
// quality goals. The Stage 8.4 calibration harness consumes a
// LoadProfile to know which verbs to drive, at what rate, and
// what it is allowed to consider "passing".
//
// A LoadProfile is plain data: no method on this type talks
// to the network or the clock. Construct one via
// [NominalLoadProfile] (the production §8.3 envelope) or by
// composing your own VerbProfile slice for a custom run
// (smoke, soak, burst).
type LoadProfile struct {
	// Name is a short identifier the harness stamps onto the
	// calibration report ("nominal", "smoke", "burst-1min").
	Name string

	// Verbs is the closed list of per-verb traffic shapes.
	// Empty is a programming error; the harness rejects it
	// via [LoadProfile.Validate].
	Verbs []VerbProfile

	// DefaultDuration is the wall-clock duration the
	// harness drives the envelope for unless the operator
	// overrides it on the command line. Stage 8.4 pins this
	// to 30 minutes for the nominal envelope per the
	// implementation-plan.md step 2 ("Run the harness ... for
	// 30 minutes").
	DefaultDuration time.Duration

	// ErrorBudgetRatio is the per-verb failure-rate cap.
	// Stage 8.4 acceptance scenario 1 says "no verb errored
	// above the 1 % budget"; the nominal profile pins this
	// to 0.01.
	ErrorBudgetRatio float64

	// LearningQuality pins the two learning-quality SLO
	// thresholds from tech-spec §8.3 (rank-of-correct-node
	// @ k=20 ≤ 5 and Concept-hit fraction @ k=20 ≥ 25 %).
	// Reported numerically in the calibration artifact even
	// when not met — the operator decides whether a miss
	// blocks the release.
	LearningQuality LearningQualityTargets
}

// VerbProfile is the per-verb slice of a LoadProfile. One
// VerbProfile drives one Scenario at one sustained RPS for
// the LoadProfile's duration.
type VerbProfile struct {
	// Verb is the closed-set verb name as it appears in the
	// proto IDL and the metric brief — e.g. "agent.recall",
	// "agent.observe", "agent.expand", "agent.summarize",
	// "mgmt.ingest_spans".
	Verb string

	// MetricName is the obs.* histogram base name the
	// harness writes its samples to. The /metrics surface
	// exposes this histogram as an APPROXIMATE per-verb
	// latency signal whose buckets align with the §8.3 SLO
	// thresholds — a downstream `histogram_quantile()` over
	// it produces bucket-midpoint percentiles that may
	// differ from the artifact's nearest-rank percentiles
	// by up to one bucket width. See
	// `cmd/loadtest-harness/main.go::loadtestHarnessRunDurationSeconds`
	// for the bucket-quantization caveat and the canonical
	// PromQL query shape (e.g.
	// "agent_recall_duration_seconds").
	MetricName string

	// SustainedRPS is the planned-arrival rate in
	// requests-per-second. mgmt.ingest_spans is naturally
	// expressed in batches-per-minute in the spec; we
	// convert to RPS here (50/min → 0.8333 RPS) so every
	// scenario in the harness ticks on the same axis.
	SustainedRPS float64

	// BurstRPS is the §8.3 short-burst envelope (1 minute
	// for agent.recall / agent.observe per the spec). The
	// nominal harness does NOT drive bursts (the soak run
	// stays at SustainedRPS); a follow-up soak/burst story
	// can read this field to extend the envelope.
	BurstRPS float64

	// SLO95Seconds / SLO99Seconds are the §8.3 latency
	// budgets. The harness compares the per-verb measured
	// percentile against these and stamps `met=true|false`
	// in the report. Crossing these is INFORMATIONAL on the
	// nominal calibration run (the §8.3 numbers are
	// explicitly "provisional, subject to load-test
	// calibration"); the operator pins post-calibration
	// numbers via the §8.3 override route after reviewing
	// the artifact.
	SLO95Seconds float64
	SLO99Seconds float64
}

// Interval returns the planned inter-arrival gap (1/SustainedRPS).
// Zero RPS returns 0; the harness MUST treat that as "skip this
// verb" rather than dividing by zero.
func (v VerbProfile) Interval() time.Duration {
	if v.SustainedRPS <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / v.SustainedRPS)
}

// PlannedRequests returns the number of arrivals the open-loop
// scheduler would fire during the given duration at
// SustainedRPS. Used by the harness to size sample buffers up
// front and by tests to assert "drove at least N requests".
func (v VerbProfile) PlannedRequests(d time.Duration) int {
	if v.SustainedRPS <= 0 || d <= 0 {
		return 0
	}
	planned := v.SustainedRPS * d.Seconds()
	if planned < 1 {
		return 0
	}
	return int(planned)
}

// LearningQualityTargets pins the §8.3 learning-quality SLOs.
// The harness measures both via labelled queries (see
// `internal/loadtest/calibration/config.go.LabeledQuery`) and
// reports the numeric value even when the threshold is missed.
//
// IMPORTANT — proxy semantics. The §8.3 SLO definition joins
// `Observation` rows on positive-outcome Episodes against
// the originating `RecallContextLog.node_ids` order; that
// measurement requires a post-hoc database join the harness
// does not perform. The harness measures a labelled-query
// proxy: given a fixture pair (query, expected_node_id), it
// computes the rank of expected_node_id in the recall response
// and (separately) whether any expected_concept_id appeared
// in the top-K concept list. The calibration artifact MUST
// surface this proxy distinction explicitly so the operator
// reads the right number.
type LearningQualityTargets struct {
	// K is the rank cutoff. §8.3 pins K=20.
	K int

	// MaxMedianRankAtK is the §8.3 cap on the median
	// rank-of-correct-node (≤ 5).
	MaxMedianRankAtK int

	// MinConceptHitFractionAtK is the §8.3 floor on the
	// concept-hit fraction (≥ 0.25).
	MinConceptHitFractionAtK float64
}

// Validate returns a non-nil error when the profile is unsafe
// to feed into the harness (empty verb list, negative rates,
// nonsensical budgets). Returning ALL collected errors at
// once via errors.Join lets the operator fix them in a single
// edit pass.
func (p LoadProfile) Validate() error {
	var errs []error
	if p.Name == "" {
		errs = append(errs, errors.New("reliability: LoadProfile.Name is required"))
	}
	if len(p.Verbs) == 0 {
		errs = append(errs, errors.New("reliability: LoadProfile.Verbs must be non-empty"))
	}
	if p.DefaultDuration <= 0 {
		errs = append(errs, fmt.Errorf("reliability: LoadProfile.DefaultDuration must be positive (got %v)", p.DefaultDuration))
	}
	if p.ErrorBudgetRatio < 0 || p.ErrorBudgetRatio > 1 {
		errs = append(errs, fmt.Errorf("reliability: LoadProfile.ErrorBudgetRatio must be in [0, 1] (got %v)", p.ErrorBudgetRatio))
	}
	seen := make(map[string]struct{}, len(p.Verbs))
	for i, v := range p.Verbs {
		if v.Verb == "" {
			errs = append(errs, fmt.Errorf("reliability: LoadProfile.Verbs[%d].Verb is required", i))
		}
		if v.MetricName == "" {
			errs = append(errs, fmt.Errorf("reliability: LoadProfile.Verbs[%d](%s).MetricName is required", i, v.Verb))
		}
		if v.SustainedRPS <= 0 {
			errs = append(errs, fmt.Errorf("reliability: LoadProfile.Verbs[%d](%s).SustainedRPS must be > 0 (got %v)", i, v.Verb, v.SustainedRPS))
		}
		if v.BurstRPS < 0 {
			errs = append(errs, fmt.Errorf("reliability: LoadProfile.Verbs[%d](%s).BurstRPS must be >= 0 (got %v)", i, v.Verb, v.BurstRPS))
		}
		if v.SLO95Seconds <= 0 {
			errs = append(errs, fmt.Errorf("reliability: LoadProfile.Verbs[%d](%s).SLO95Seconds must be > 0", i, v.Verb))
		}
		if v.SLO99Seconds <= 0 {
			errs = append(errs, fmt.Errorf("reliability: LoadProfile.Verbs[%d](%s).SLO99Seconds must be > 0", i, v.Verb))
		}
		if v.SLO95Seconds > 0 && v.SLO99Seconds > 0 && v.SLO99Seconds < v.SLO95Seconds {
			errs = append(errs, fmt.Errorf("reliability: LoadProfile.Verbs[%d](%s).SLO99Seconds (%v) < SLO95Seconds (%v)", i, v.Verb, v.SLO99Seconds, v.SLO95Seconds))
		}
		if _, dup := seen[v.Verb]; dup {
			errs = append(errs, fmt.Errorf("reliability: LoadProfile.Verbs has duplicate verb %q", v.Verb))
		}
		seen[v.Verb] = struct{}{}
	}
	lq := p.LearningQuality
	if lq.K <= 0 {
		errs = append(errs, fmt.Errorf("reliability: LoadProfile.LearningQuality.K must be > 0 (got %d)", lq.K))
	}
	if lq.MaxMedianRankAtK <= 0 {
		errs = append(errs, fmt.Errorf("reliability: LoadProfile.LearningQuality.MaxMedianRankAtK must be > 0 (got %d)", lq.MaxMedianRankAtK))
	}
	if lq.MinConceptHitFractionAtK < 0 || lq.MinConceptHitFractionAtK > 1 {
		errs = append(errs, fmt.Errorf("reliability: LoadProfile.LearningQuality.MinConceptHitFractionAtK must be in [0, 1] (got %v)", lq.MinConceptHitFractionAtK))
	}
	return errors.Join(errs...)
}

// Verb returns the named VerbProfile and ok=true when present.
// The harness uses this for per-verb wiring decisions
// (e.g., enable the IngestSpans driver only when the profile
// includes it).
func (p LoadProfile) Verb(name string) (VerbProfile, bool) {
	for _, v := range p.Verbs {
		if v.Verb == name {
			return v, true
		}
	}
	return VerbProfile{}, false
}

// Canonical verb identifiers used by the §8.3 envelope and
// the Stage 8.4 harness wiring. Centralised so a scenario can
// match the verb by constant rather than a string literal that
// drifts.
const (
	VerbAgentRecall     = "agent.recall"
	VerbAgentObserve    = "agent.observe"
	VerbAgentExpand     = "agent.expand"
	VerbAgentSummarize  = "agent.summarize"
	VerbMgmtIngestSpans = "mgmt.ingest_spans"
)

// NominalLoadProfile returns the tech-spec §8.3 envelope as a
// LoadProfile. The five entries mirror the §8.3 table verbatim;
// see `docs/stories/code-intelligence-AGENT-MEMORY/tech-spec.md`
// §8.3 "SLO targets and throughput envelopes" for the source of
// truth. Renaming a verb here without updating §8.3 is a
// breaking-contract change.
func NominalLoadProfile() LoadProfile {
	return LoadProfile{
		Name:             "nominal",
		DefaultDuration:  30 * time.Minute,
		ErrorBudgetRatio: 0.01,
		Verbs: []VerbProfile{
			{
				Verb:         VerbAgentRecall,
				MetricName:   "agent_recall_duration_seconds",
				SustainedRPS: 50,
				BurstRPS:     200,
				SLO95Seconds: 1.5,
				SLO99Seconds: 4,
			},
			{
				Verb:         VerbAgentObserve,
				MetricName:   "agent_observe_duration_seconds",
				SustainedRPS: 50,
				BurstRPS:     200,
				SLO95Seconds: 0.4,
				SLO99Seconds: 1.5,
			},
			{
				Verb:         VerbAgentExpand,
				MetricName:   "agent_expand_duration_seconds",
				SustainedRPS: 20,
				BurstRPS:     0,
				SLO95Seconds: 1.5,
				SLO99Seconds: 4,
			},
			{
				Verb:         VerbAgentSummarize,
				MetricName:   "agent_summarize_duration_seconds",
				SustainedRPS: 5,
				BurstRPS:     0,
				SLO95Seconds: 4,
				SLO99Seconds: 10,
			},
			{
				// §8.3 expresses ingest_spans in batches/min;
				// convert to RPS so every scenario shares the
				// same axis: 50 batches/min ÷ 60 s/min = 0.8333 RPS.
				Verb:         VerbMgmtIngestSpans,
				MetricName:   "mgmt_ingest_spans_batch_duration_seconds",
				SustainedRPS: 50.0 / 60.0,
				BurstRPS:     0,
				SLO95Seconds: 2,
				SLO99Seconds: 5,
			},
		},
		LearningQuality: LearningQualityTargets{
			K:                        20,
			MaxMedianRankAtK:         5,
			MinConceptHitFractionAtK: 0.25,
		},
	}
}

// SmokeProfile returns a scaled-down envelope suitable for a
// CI / unit-test integration check: same verb set, sub-second
// duration, low single-digit RPS so the harness completes in
// well under a second on a developer laptop. Used by the
// `harness_test.go` clean-run scenario and the
// `loadtest-harness` binary's `--profile=smoke` mode for
// operator smoke tests.
func SmokeProfile() LoadProfile {
	p := NominalLoadProfile()
	p.Name = "smoke"
	p.DefaultDuration = 200 * time.Millisecond
	for i := range p.Verbs {
		// Cap each verb at 50 RPS for the smoke envelope so
		// a sub-second run still exercises the open-loop
		// scheduler without saturating the unit-test process
		// (≈10 samples per verb at 200 ms × 50 RPS).
		if p.Verbs[i].SustainedRPS > 50 {
			p.Verbs[i].SustainedRPS = 50
		}
	}
	return p
}
