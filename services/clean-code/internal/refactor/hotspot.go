package refactor

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// Canonical input metric_kind constants for the composite
// hotspot score. Pinned as named constants so a
// `grep -nF "modification_count_in_window"` over the planner
// tree lands one definition site and so a typo in a downstream
// caller is a compile error rather than a silent SQL drift.
//
// The set MUST match architecture Sec 1.4.1 rows 1, 6, 8, 9,
// 12 verbatim; the closed-set tests in [hotspot_test.go]
// cross-check against [dsl.CanonicalMetricKinds].
const (
	// MetricKindCyclo is architecture Sec 1.4.1 row 1 --
	// cyclomatic complexity. One input to the composite
	// `complexity` raw value (combined with
	// [MetricKindCognitiveComplexity] by sum).
	MetricKindCyclo = "cyclo"

	// MetricKindCognitiveComplexity is architecture Sec 1.4.1
	// row 6 -- cognitive complexity per Sonar's formulation.
	// Second input to the composite `complexity` raw value.
	MetricKindCognitiveComplexity = "cognitive_complexity"

	// MetricKindModificationCountInWindow is architecture Sec
	// 1.4.1 row 12 -- the per-scope commit-touch count over
	// `PolicyVersion.refactor_weights.window_days`. Sole input
	// to the composite `churn` raw value.
	MetricKindModificationCountInWindow = "modification_count_in_window"

	// MetricKindCouplingBetweenObjects is architecture Sec
	// 1.4.1 row 9 -- the CBO measure. One input to the
	// composite `coupling` raw value (combined with
	// [MetricKindFanOut] by sum).
	MetricKindCouplingBetweenObjects = "coupling_between_objects"

	// MetricKindFanOut is architecture Sec 1.4.1 row 8 --
	// outgoing-edge count. Second input to the composite
	// `coupling` raw value.
	MetricKindFanOut = "fan_out"
)

// HotSpotInputMetricKinds is the closed list of foundation-
// tier `metric_kind` strings the composite hotspot score
// consumes (architecture Sec 1.4.1 + Sec 3.9). The slice
// order follows the architecture-row order
// (cyclo, cognitive_complexity, modification_count_in_window,
// coupling_between_objects, fan_out); callers MUST NOT rely
// on the order.
//
// Used by the test in [hotspot_test.go] to assert the
// hotspot's input set is exactly these five strings -- no
// more, no fewer. A new input metric_kind requires (1) an
// entry in this slice + a sibling constant above, (2) a new
// field on [ScopeInputs], and (3) an update to architecture
// Sec 3.9.
var HotSpotInputMetricKinds = []string{
	MetricKindCyclo,
	MetricKindCognitiveComplexity,
	MetricKindModificationCountInWindow,
	MetricKindCouplingBetweenObjects,
	MetricKindFanOut,
}

// HotSpotQualifyingDeltas is the closed set of
// [rule_engine.Delta] values that contribute to a scope's
// `finding_count` term in the composite-score formula. Per
// architecture Sec 5.4.1 lines 1186-1190 canonical delta enum:
//
//   - [rule_engine.DeltaNew] -- the rule fired at this scope
//     for the FIRST SHA ever. Counts because a fresh finding
//     is fresh tech-debt the planner SHOULD surface.
//   - [rule_engine.DeltaNewlyFailing] -- the regression
//     bucket. Counts for the same reason.
//
// The other two delta values are intentionally EXCLUDED:
//
//   - [rule_engine.DeltaUnchanged] -- a previously-failing
//     finding is still failing. Already on the planner's
//     radar from a prior tick; counting again would
//     double-weight chronic issues.
//   - [rule_engine.DeltaResolved] -- the finding has been
//     healed. Counting would invert the signal (the planner
//     would prioritise a scope precisely because it just got
//     fixed).
//
// The list mirrors the impl-plan Stage 8.1 step that pins the
// SQL filter `delta IN ('newly_failing','new')`.
var HotSpotQualifyingDeltas = []rule_engine.Delta{
	rule_engine.DeltaNew,
	rule_engine.DeltaNewlyFailing,
}

// IsHotSpotQualifyingDelta reports whether a `finding.delta`
// value contributes to the composite-score `finding_count`
// term. Consumers MUST use this helper rather than open-
// coding the string set so a future delta-enum change lands
// in one place.
func IsHotSpotQualifyingDelta(d rule_engine.Delta) bool {
	for _, k := range HotSpotQualifyingDeltas {
		if d == k {
			return true
		}
	}
	return false
}

// madToSigma is the textbook MAD-to-sigma normalisation
// constant `1 / Phi^{-1}(0.75)`. Multiplying MAD by this
// constant produces a robust estimator that is
// asymptotically equivalent to the standard deviation under
// a normal distribution. Pinned as a constant so the
// formula's intent is greppable and the deterministic
// numerical value is part of the source-controlled
// definition.
const madToSigma = 1.4826

// ScopeInputs is the raw per-scope input bundle the
// [Computer] consumes. Each numeric field has a companion
// `Has<Field>` bool so callers can distinguish "metric_sample
// row absent" from "metric_sample row present with value 0".
// The Has bool MUST be true whenever the corresponding value
// was read from a [MetricSample] row; the Computer's
// distribution builder filters by these bools and skips
// scopes that lack a dimension.
//
// `ScopeID` is the logical FK to `scope_binding.scope_id`
// (architecture Sec 5.2.3); the caller is responsible for
// resolving the join from the four input metric_kind
// `metric_sample.scope_id` columns plus the `finding.scope_id`
// column down to one [ScopeInputs] row per `scope_id`.
//
// `FindingCount` is the COUNT of `finding` rows at
// `(repo_id, sha, scope_id)` whose `delta` is a member of
// [HotSpotQualifyingDeltas]. A value of 0 is legitimate (no
// qualifying findings at the scope); no `HasFindingCount`
// companion is needed because the count is an aggregate and
// "missing" is meaningless.
type ScopeInputs struct {
	// ScopeID is the logical FK to scope_binding.scope_id.
	// MUST be set (non-zero) for the Computer to accept the
	// row; an empty UUID returns [ErrInvalidScopeInputs].
	ScopeID uuid.UUID

	// Cyclo is the per-scope `cyclo` MetricSample.value.
	// `HasCyclo=false` means no `cyclo` sample row was found
	// for this scope; the value is ignored. When
	// `HasCyclo=true` and `Cyclo=0`, the scope contributes
	// the zero value to the complexity distribution.
	Cyclo    float64
	HasCyclo bool

	// CognitiveComplexity is the per-scope
	// `cognitive_complexity` MetricSample.value. Companion
	// `HasCognitiveComplexity` mirrors [HasCyclo].
	CognitiveComplexity    float64
	HasCognitiveComplexity bool

	// ModificationCount is the per-scope
	// `modification_count_in_window` MetricSample.value.
	// Companion `HasModificationCount` mirrors [HasCyclo].
	// The Metric Ingestor's churn materialiser writes this
	// row per Sec 3.5.1.b.
	ModificationCount    float64
	HasModificationCount bool

	// CouplingBetweenObjects is the per-scope
	// `coupling_between_objects` MetricSample.value.
	// Companion `HasCouplingBetweenObjects` mirrors
	// [HasCyclo].
	CouplingBetweenObjects    float64
	HasCouplingBetweenObjects bool

	// FanOut is the per-scope `fan_out` MetricSample.value.
	// Companion `HasFanOut` mirrors [HasCyclo].
	FanOut    float64
	HasFanOut bool

	// FindingCount is the COUNT of `finding` rows at
	// `(repo_id, sha, scope_id)` whose `delta` is in
	// [HotSpotQualifyingDeltas]. MUST be >= 0; a negative
	// value returns [ErrInvalidScopeInputs].
	FindingCount int
}

// RawComplexity returns the combined complexity raw value
// for the scope and a presence bool. The raw value is the SUM
// of the present `cyclo` and `cognitive_complexity` inputs
// (architecture Sec 3.9 line 605 input set + impl-plan Stage
// 8.1 step "complexity from `cyclo`+`cognitive_complexity`").
//
// Presence semantics: the bool is true iff AT LEAST ONE of
// the two component metrics is present. When both are absent
// the scope is excluded from the complexity distribution and
// contributes z=0 for the complexity dimension.
func (in ScopeInputs) RawComplexity() (float64, bool) {
	if !in.HasCyclo && !in.HasCognitiveComplexity {
		return 0, false
	}
	var v float64
	if in.HasCyclo {
		v += in.Cyclo
	}
	if in.HasCognitiveComplexity {
		v += in.CognitiveComplexity
	}
	return v, true
}

// RawChurn returns the churn raw value -- the
// `modification_count_in_window` sample -- and its presence
// bool. The churn dimension has a single input metric_kind
// (architecture Sec 3.9 line 606 + impl-plan Stage 8.1 step
// "churn from `modification_count_in_window`") so no
// aggregation is needed.
func (in ScopeInputs) RawChurn() (float64, bool) {
	if !in.HasModificationCount {
		return 0, false
	}
	return in.ModificationCount, true
}

// RawCoupling returns the combined coupling raw value for
// the scope and a presence bool. The raw value is the SUM of
// the present `coupling_between_objects` and `fan_out`
// inputs (architecture Sec 3.9 line 607 input set + impl-plan
// Stage 8.1 step "coupling from `coupling_between_objects`+`fan_out`").
//
// Presence mirrors [RawComplexity]: true iff at least one of
// the two component metrics is present.
func (in ScopeInputs) RawCoupling() (float64, bool) {
	if !in.HasCouplingBetweenObjects && !in.HasFanOut {
		return 0, false
	}
	var v float64
	if in.HasCouplingBetweenObjects {
		v += in.CouplingBetweenObjects
	}
	if in.HasFanOut {
		v += in.FanOut
	}
	return v, true
}

// Breakdown is the per-input view of a scope's score. Useful
// for explainability (which dimension drove the score?), for
// tests, and for a future stage that may surface the
// breakdown via the Insights surface. The breakdown is NOT
// persisted on the `clean_code.hot_spot` row in the v1
// schema -- only the composite [HotSpot.Score] is stored
// (architecture Sec 5.5.1 lines 1244-1254 column list).
type Breakdown struct {
	// ComplexityZ is the robust z-score of the scope's
	// [ScopeInputs.RawComplexity] over the repo's complexity
	// distribution. When the scope had no complexity input
	// (RawComplexity returned has=false), this field is 0.
	ComplexityZ float64

	// ChurnZ is the robust z-score of the scope's churn
	// input. Same missing-input fallback as [ComplexityZ].
	ChurnZ float64

	// CouplingZ is the robust z-score of the scope's
	// coupling input. Same missing-input fallback as
	// [ComplexityZ].
	CouplingZ float64

	// FindingCount mirrors [ScopeInputs.FindingCount] -- the
	// COUNT of qualifying findings for the scope. NOT
	// z-scored (architecture Sec 3.9 line 608: the delta term
	// multiplies the RAW count).
	FindingCount int
}

// HotSpot mirrors the `clean_code.hot_spot` row per
// architecture Sec 5.5.1 lines 1244-1254 and the SQL CREATE
// TABLE in migration 0003 lines 764-779. The column shape is
// EXACTLY this six-field tuple plus the PK; per the
// implementation-plan Stage 8.1 step the per-input z-scores
// and finding_count are NOT canonical columns on the row.
type HotSpot struct {
	// HotspotID is the row PK. The default factory is
	// [uuid.NewV4]; tests inject a deterministic factory via
	// [WithIDFactory].
	HotspotID uuid.UUID

	// RepoID FK -> `clean_code.repo.repo_id`.
	RepoID uuid.UUID

	// SHA is the commit SHA the hot-spot row was scored at.
	SHA string

	// ScopeID is the logical FK to `scope_binding.scope_id`
	// (the SQL FK is added by a follow-up migration once
	// both stages have landed; see the comment in migration
	// 0003 line 770-771).
	ScopeID uuid.UUID

	// Score is the composite hotspot score per architecture
	// Sec 3.9 (lines 602-609). Full precision is preserved
	// at-rest; callers that need to render the score (e.g.
	// the Insights dashboard) may round, but the row column
	// is the raw `double precision`.
	Score float64

	// PolicyVersionID is the `clean_code.policy_version.
	// policy_version_id` whose `refactor_weights` produced
	// this row's score. Carrying the id on every row is the
	// canonical mechanism for replay-safe re-derivation: a
	// future re-scoring with old weights remains
	// reproducible because the row records which weights
	// signed off (architecture Sec 5.5.1 line 1253 +
	// impl-plan Stage 8.1 step "embeds `policy_version_id`
	// on every `hot_spot` row").
	PolicyVersionID uuid.UUID

	// CreatedAt is the row's `created_at timestamptz NOT
	// NULL DEFAULT now()`. The [Computer] sets this from a
	// single `now()` reading at the start of [Computer.Compute]
	// so all rows in a batch share one timestamp. Callers
	// MAY override the value before persisting; the SQL
	// writer is responsible for translating the in-memory
	// timestamp to the column.
	CreatedAt time.Time
}

// Computation pairs a [HotSpot] row with its [Breakdown] for
// downstream consumers. The Breakdown is NOT persisted in v1
// (per the implementation-plan Stage 8.1 step) but is
// returned in-memory so the Insights dashboard / tests /
// future stages can introspect the score.
type Computation struct {
	// HotSpot is the persistable row exactly as it will
	// appear in `clean_code.hot_spot`. [Computer.Compute]
	// already minted [HotSpot.HotspotID] and set
	// [HotSpot.CreatedAt]; the writer just `INSERT`s.
	HotSpot HotSpot

	// Breakdown carries the per-input z-scores and the raw
	// finding count. Used by tests to assert the formula and
	// by future Insights surfaces to explain a score. NOT
	// persisted in v1.
	Breakdown Breakdown
}

// PolicySnapshot bundles the two attributes the [Computer]
// needs from the active `PolicyVersion`: the id (stamped on
// every `hot_spot` row) and the refactor weights (used by
// the composite-score formula). The bundle exists so a
// caller cannot accidentally pass weights from one policy
// version and the id from another -- the type system makes
// the invariant "the score was computed with THESE weights
// from THAT policy_version" impossible to violate.
//
// The composition root populates the snapshot from a single
// read against [steward.Steward.ActivePolicyVersion]:
//
//	pv, ok, err := steward.ActivePolicyVersion(ctx)
//	if !ok { ... }
//	snap := refactor.PolicySnapshot{
//	    PolicyVersionID: pv.PolicyVersionID,
//	    Weights:         pv.RefactorWeights,
//	}
type PolicySnapshot struct {
	// PolicyVersionID is stamped on every emitted
	// [HotSpot.PolicyVersionID]. MUST be non-zero; an empty
	// UUID returns [ErrInvalidPolicySnapshot].
	PolicyVersionID uuid.UUID

	// Weights is the per-policy composite-score weight set
	// from [steward.PolicyVersion.RefactorWeights]. The
	// Alpha/Beta/Gamma/Delta scalars feed [Score]; the other
	// fields (EffortModelVersion, WindowDays,
	// FreshnessWindowSeconds) are not consumed at this stage
	// but are carried for forward-compat.
	Weights steward.RefactorWeights
}

// Score applies the composite-score formula verbatim per
// architecture Sec 3.9 lines 602-609:
//
//	score = alpha * complexity_z
//	      + beta  * churn_z
//	      + gamma * coupling_z
//	      + delta * finding_count
//
// `finding_count` is multiplied as the RAW integer count;
// the other three terms are pre-computed robust z-scores
// (the [Breakdown] fields already carry the z-scored
// values). Score is a pure function: same input bytes
// produce same output bytes; no clock / id / IO touches the
// formula.
//
// The function does NOT validate finiteness of the weights
// or the breakdown -- the [Computer] performs that
// validation up front via [validatePolicySnapshot] and
// [validateScopeInputs]; callers using Score in isolation
// (tests, debugging tools) accept responsibility for
// supplying finite inputs.
func Score(w steward.RefactorWeights, b Breakdown) float64 {
	return w.Alpha*b.ComplexityZ +
		w.Beta*b.ChurnZ +
		w.Gamma*b.CouplingZ +
		w.Delta*float64(b.FindingCount)
}

// RobustZ computes the robust z-score of `x` against
// `distribution`:
//
//	median = median(distribution)
//	MAD    = median(|d_i - median|)
//	z      = (x - median) / (madToSigma * MAD)
//
// where [madToSigma] = 1.4826. The distribution slice is NOT
// mutated (a defensive copy is taken before sorting).
//
// # Edge cases
//
//   - Empty distribution: returns 0 (no signal to z-score
//     against).
//   - All values identical (true constant distribution): the
//     median equals every point so `MAD = 0` AND the
//     standard deviation is 0; returns 0.
//   - MAD = 0 but distribution is NOT constant (sparse
//     case, e.g. `[0, 0, 0, 100]`): falls back to the
//     standard z-score `(x - mean) / stddev`. This rescues
//     the outlier case where MAD-only would erase the
//     signal.
//   - `x` outside the distribution's support: legitimate;
//     the returned z is whatever the formula produces (can
//     exceed +-3 for genuine outliers).
//   - `NaN` / `Inf` inputs: NOT validated here. The
//     [Computer] rejects non-finite inputs before calling
//     RobustZ; isolated tests must avoid them.
func RobustZ(distribution []float64, x float64) float64 {
	if len(distribution) == 0 {
		return 0
	}
	// Defensive copy + sort so input is not mutated.
	buf := make([]float64, len(distribution))
	copy(buf, distribution)
	sort.Float64s(buf)
	med := medianSorted(buf)

	// Build |d_i - med| and find its median (MAD).
	dev := make([]float64, len(buf))
	for i, v := range buf {
		dev[i] = math.Abs(v - med)
	}
	sort.Float64s(dev)
	mad := medianSorted(dev)

	if mad == 0 {
		// MAD = 0 covers two distinct cases: (a) all values
		// equal (constant distribution) -- no signal, return
		// 0; (b) sparse outlier (e.g. [0,0,0,100]) -- median
		// and mass coincide at 0 but the outlier is real,
		// fall back to standard z-score. Distinguish by
		// stddev: 0 in case (a), non-zero in case (b).
		mean := 0.0
		for _, v := range buf {
			mean += v
		}
		mean /= float64(len(buf))
		ssq := 0.0
		for _, v := range buf {
			d := v - mean
			ssq += d * d
		}
		variance := ssq / float64(len(buf))
		stddev := math.Sqrt(variance)
		if stddev == 0 {
			return 0
		}
		return (x - mean) / stddev
	}
	return (x - med) / (madToSigma * mad)
}

// medianSorted returns the median of a slice that is ALREADY
// sorted in ascending order. Linear in length only because
// the caller has already paid the sort cost; sort.Float64s
// is the canonical sort used here and the caller passes the
// sorted buffer down. Callers that operate on an unsorted
// slice MUST sort first.
//
// On an even-length slice the median is the arithmetic mean
// of the two middle elements; on an odd-length slice it is
// the single middle element. The implementation is
// deliberately simple: no quickselect, no percentile
// approximation -- correctness over speed for the small
// (<= O(repo file count)) distributions the planner sees.
func medianSorted(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	mid := n / 2
	if n%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// Option configures a [Computer]. Two options ship in this
// stage: [WithIDFactory] (deterministic uuid minting) and
// [WithClock] (deterministic timestamp). Tests inject both
// so a fixture can pin byte-identical output across runs;
// production wiring uses neither (the defaults
// [uuid.NewV4] and [time.Now] are correct).
type Option func(*Computer)

// WithIDFactory overrides the [Computer]'s uuid factory.
// The factory MUST return a fresh uuid per call (the
// [Computer] mints one per emitted HotSpot row); returning
// the same uuid twice violates the `hot_spot.hotspot_id`
// PRIMARY KEY contract. Tests typically inject a counter-
// backed factory.
//
// A nil factory is rejected at [Computer.Compute] time
// rather than at [NewComputer] (so a wiring bug that passes
// `WithIDFactory(nil)` does not panic at construction; the
// failure surfaces with a clear error on the next Compute
// call).
func WithIDFactory(f func() (uuid.UUID, error)) Option {
	return func(c *Computer) { c.newID = f }
}

// WithClock overrides the [Computer]'s clock. The clock
// MUST be stable: the [Computer] calls it ONCE at the start
// of [Computer.Compute] and stamps every emitted row's
// `CreatedAt` with that single reading, so a clock that
// advances mid-batch would NOT be observed.
//
// Like [WithIDFactory], a nil clock is rejected at
// [Computer.Compute] time so a misconfigured `WithClock(nil)`
// surfaces as a clean error.
func WithClock(f func() time.Time) Option {
	return func(c *Computer) { c.now = f }
}

// Computer is the in-process actor that turns [ScopeInputs]
// + a [PolicySnapshot] into ranked [Computation] values.
// The Computer is stateless after [NewComputer] returns
// (the factories captured in fields are immutable); it is
// safe for concurrent use.
//
// Per architecture Sec 3.9 the Computer is the COMPUTE step
// of the Refactor Planner. Persistence into the
// `clean_code.hot_spot` table is a follow-up stage's
// responsibility; this stage ships the deterministic
// computation only.
type Computer struct {
	newID func() (uuid.UUID, error)
	now   func() time.Time
}

// NewComputer returns a fully-initialised [Computer] with
// the production defaults: [uuid.NewV4] for ids,
// [time.Now] for timestamps. Tests override either via
// [WithIDFactory] / [WithClock].
func NewComputer(opts ...Option) *Computer {
	c := &Computer{
		newID: uuid.NewV4,
		now:   time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Compute applies the composite-score formula to the given
// inputs and returns one [Computation] per `ScopeInputs`
// row, sorted by `(Score DESC, ScopeID ASC)` for
// determinism (architecture G6).
//
// Step-by-step (mirrors architecture Sec 3.9):
//
//  1. Validate the snapshot and per-scope inputs (no NaN /
//     Inf, no duplicate scope_id, finite weights). On the
//     first invalid row the function returns the wrapped
//     sentinel error and emits no rows.
//
//  2. Snapshot `now()` once so every emitted row shares one
//     `CreatedAt` (the rubber-duck design review pinned
//     this; see package doc).
//
//  3. Build three distributions
//     (complexity, churn, coupling) from the present raw
//     values across the input set. Scopes missing a
//     dimension are EXCLUDED from that dimension's
//     distribution but remain in the output (their z for the
//     missing dimension is 0).
//
//  4. Compute the per-scope robust z-scores via [RobustZ].
//
//  5. Apply [Score] with the snapshot's weights.
//
//  6. Mint the HotSpot row's uuid via the configured
//     factory and assemble the [Computation].
//
//  7. Sort the result deterministically: `Score DESC`
//     primary, `ScopeID ASC` secondary (so equal scores tie
//     deterministically rather than by sort instability).
//
// `repoID` and `sha` are stamped on every emitted
// `HotSpot.RepoID` / `HotSpot.SHA`. An empty `sha` returns
// [ErrEmptySHA] (mirrors the architecture's pin that a
// scoring run is bound to a specific commit).
//
// Compute returns `(nil, nil)` on empty input -- an empty
// scope list is a legitimate "nothing to score" condition,
// NOT an error.
func (c *Computer) Compute(
	policy PolicySnapshot,
	repoID uuid.UUID,
	sha string,
	inputs []ScopeInputs,
) ([]Computation, error) {
	if c.newID == nil {
		return nil, fmt.Errorf("refactor.Compute: %w", ErrNilIDFactory)
	}
	if c.now == nil {
		return nil, fmt.Errorf("refactor.Compute: %w", ErrNilClock)
	}
	if err := validatePolicySnapshot(policy); err != nil {
		return nil, fmt.Errorf("refactor.Compute: %w", err)
	}
	if repoID == uuid.Nil {
		return nil, fmt.Errorf("refactor.Compute: %w", ErrEmptyRepoID)
	}
	if sha == "" {
		return nil, fmt.Errorf("refactor.Compute: %w", ErrEmptySHA)
	}
	if len(inputs) == 0 {
		return nil, nil
	}

	// Validate inputs and detect duplicate ScopeIDs in one
	// pass. We emit a deterministic error so the caller can
	// branch on the sentinel.
	seen := make(map[uuid.UUID]struct{}, len(inputs))
	for i := range inputs {
		if err := validateScopeInputs(inputs[i]); err != nil {
			return nil, fmt.Errorf(
				"refactor.Compute: input[%d]: %w", i, err)
		}
		if _, dup := seen[inputs[i].ScopeID]; dup {
			return nil, fmt.Errorf(
				"refactor.Compute: input[%d]: %w (scope_id=%s)",
				i, ErrDuplicateScopeID, inputs[i].ScopeID)
		}
		seen[inputs[i].ScopeID] = struct{}{}
	}

	// Snapshot the clock once so every row in this batch
	// shares one CreatedAt.
	createdAt := c.now()

	// Build the three per-dimension distributions from the
	// PRESENT raw values. Scopes missing a dimension are
	// excluded from that distribution but remain in the
	// output list with z=0 for that dim.
	complexityDist := make([]float64, 0, len(inputs))
	churnDist := make([]float64, 0, len(inputs))
	couplingDist := make([]float64, 0, len(inputs))
	rawComplexity := make([]float64, len(inputs))
	rawChurn := make([]float64, len(inputs))
	rawCoupling := make([]float64, len(inputs))
	hasComplexity := make([]bool, len(inputs))
	hasChurn := make([]bool, len(inputs))
	hasCoupling := make([]bool, len(inputs))
	for i := range inputs {
		if v, ok := inputs[i].RawComplexity(); ok {
			rawComplexity[i] = v
			hasComplexity[i] = true
			complexityDist = append(complexityDist, v)
		}
		if v, ok := inputs[i].RawChurn(); ok {
			rawChurn[i] = v
			hasChurn[i] = true
			churnDist = append(churnDist, v)
		}
		if v, ok := inputs[i].RawCoupling(); ok {
			rawCoupling[i] = v
			hasCoupling[i] = true
			couplingDist = append(couplingDist, v)
		}
	}

	out := make([]Computation, len(inputs))
	for i := range inputs {
		var bd Breakdown
		if hasComplexity[i] {
			bd.ComplexityZ = RobustZ(complexityDist, rawComplexity[i])
		}
		if hasChurn[i] {
			bd.ChurnZ = RobustZ(churnDist, rawChurn[i])
		}
		if hasCoupling[i] {
			bd.CouplingZ = RobustZ(couplingDist, rawCoupling[i])
		}
		bd.FindingCount = inputs[i].FindingCount

		score := Score(policy.Weights, bd)
		// Defense-in-depth: NaN/Inf weights or raw values
		// should have been rejected upstream, but if anything
		// slipped through (a future bug), refuse to emit a
		// row carrying nonsense.
		if !isFinite(score) {
			return nil, fmt.Errorf(
				"refactor.Compute: input[%d] (scope_id=%s): %w (score=%v)",
				i, inputs[i].ScopeID, ErrNonFiniteScore, score)
		}

		id, err := c.newID()
		if err != nil {
			return nil, fmt.Errorf(
				"refactor.Compute: input[%d]: mint hotspot_id: %w", i, err)
		}
		// A factory that returns (uuid.Nil, nil) would produce
		// a hot_spot row with a zero PRIMARY KEY -- the
		// hot_spot SQL writer would either fail the
		// PK-uniqueness CHECK or, worse, succeed once and
		// then collide on the next batch. Reject up front
		// rather than emit a poisoned row.
		if id == uuid.Nil {
			return nil, fmt.Errorf(
				"refactor.Compute: input[%d] (scope_id=%s): %w",
				i, inputs[i].ScopeID, ErrIDFactoryReturnedNil)
		}

		out[i] = Computation{
			HotSpot: HotSpot{
				HotspotID:       id,
				RepoID:          repoID,
				SHA:             sha,
				ScopeID:         inputs[i].ScopeID,
				Score:           score,
				PolicyVersionID: policy.PolicyVersionID,
				CreatedAt:       createdAt,
			},
			Breakdown: bd,
		}
	}

	// Deterministic ordering: highest score first, ScopeID
	// ASC as the tie-breaker (rather than relying on the
	// input order, which the caller does not pin). Score
	// equality is compared by raw float bits so a NaN that
	// somehow slips past the validator does not produce
	// silently unstable sort behaviour -- but since
	// validation rejects NaN upstream this branch should be
	// unreachable in practice.
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := out[i].HotSpot.Score, out[j].HotSpot.Score
		if si != sj {
			return si > sj
		}
		ai := out[i].HotSpot.ScopeID
		aj := out[j].HotSpot.ScopeID
		return uuidLess(ai, aj)
	})

	return out, nil
}

// uuidLess provides a total ordering over uuid.UUID byte
// representations. The standard library does not export a
// comparison so we lift the byte comparison here; the
// ordering MUST be stable across runs (the determinism
// guarantee) so a lexicographic byte compare is sufficient.
func uuidLess(a, b uuid.UUID) bool {
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// validatePolicySnapshot rejects a snapshot whose
// PolicyVersionID is empty or whose weights carry a non-
// finite scalar. Run once at the top of [Computer.Compute].
func validatePolicySnapshot(p PolicySnapshot) error {
	if p.PolicyVersionID == uuid.Nil {
		return ErrInvalidPolicySnapshot
	}
	if !isFinite(p.Weights.Alpha) ||
		!isFinite(p.Weights.Beta) ||
		!isFinite(p.Weights.Gamma) ||
		!isFinite(p.Weights.Delta) {
		return fmt.Errorf("%w: weight is NaN or Inf", ErrInvalidPolicySnapshot)
	}
	return nil
}

// validateScopeInputs rejects a per-scope input bundle that
// is structurally invalid: empty ScopeID, negative
// FindingCount, or any present raw metric value that is
// NaN/Inf. Per-Has filtering means an absent metric is fine
// (the value is ignored); a present-but-non-finite value is
// a data-corruption signal that the planner refuses.
func validateScopeInputs(in ScopeInputs) error {
	if in.ScopeID == uuid.Nil {
		return ErrInvalidScopeInputs
	}
	if in.FindingCount < 0 {
		return fmt.Errorf("%w: finding_count must be >= 0 (got %d)",
			ErrInvalidScopeInputs, in.FindingCount)
	}
	if in.HasCyclo && !isFinite(in.Cyclo) {
		return fmt.Errorf("%w: cyclo is NaN or Inf", ErrInvalidScopeInputs)
	}
	if in.HasCognitiveComplexity && !isFinite(in.CognitiveComplexity) {
		return fmt.Errorf("%w: cognitive_complexity is NaN or Inf",
			ErrInvalidScopeInputs)
	}
	if in.HasModificationCount && !isFinite(in.ModificationCount) {
		return fmt.Errorf("%w: modification_count_in_window is NaN or Inf",
			ErrInvalidScopeInputs)
	}
	if in.HasCouplingBetweenObjects && !isFinite(in.CouplingBetweenObjects) {
		return fmt.Errorf("%w: coupling_between_objects is NaN or Inf",
			ErrInvalidScopeInputs)
	}
	if in.HasFanOut && !isFinite(in.FanOut) {
		return fmt.Errorf("%w: fan_out is NaN or Inf", ErrInvalidScopeInputs)
	}
	return nil
}

// isFinite reports whether v is neither NaN nor Inf. The
// composite-score formula is closed under finite reals; a
// non-finite scalar anywhere in the input poisons the score
// and may render the ranking nonsensical. The validator
// rejects up-front so the in-database row carries only
// well-defined values.
func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// Sentinel errors. Defined as exported sentinels so callers
// can branch via `errors.Is` rather than string-matching the
// wrapped message.
var (
	// ErrInvalidPolicySnapshot is returned by [Computer.Compute]
	// when the supplied [PolicySnapshot] has a zero
	// PolicyVersionID or carries a non-finite weight. The
	// wrapped error includes a precise reason so a 400-class
	// HTTP echo does not require the caller to grep the
	// source.
	ErrInvalidPolicySnapshot = errors.New(
		"refactor: invalid PolicySnapshot")

	// ErrInvalidScopeInputs is returned when a per-scope
	// input fails validation (zero ScopeID, negative
	// FindingCount, or a present-but-non-finite raw metric).
	ErrInvalidScopeInputs = errors.New(
		"refactor: invalid ScopeInputs")

	// ErrDuplicateScopeID is returned when the input slice
	// carries more than one row at the same `scope_id`.
	// The composite-score formula is per-scope; merging two
	// rows would silently double-count or worse, so the
	// planner refuses ambiguous input rather than guess.
	ErrDuplicateScopeID = errors.New(
		"refactor: duplicate scope_id in inputs")

	// ErrEmptyRepoID is returned when the caller passes an
	// empty `repo_id` to [Computer.Compute]. The `hot_spot`
	// table's `repo_id` column is `NOT NULL REFERENCES
	// clean_code.repo`; a zero uuid would fail the FK at
	// INSERT time, so the planner refuses up front.
	ErrEmptyRepoID = errors.New(
		"refactor: repo_id is required")

	// ErrEmptySHA is returned when the caller passes an
	// empty `sha` to [Computer.Compute]. Every `hot_spot`
	// row is bound to a specific commit; an empty sha would
	// produce an unbinding hotspot row that can never be
	// re-resolved.
	ErrEmptySHA = errors.New(
		"refactor: sha is required")

	// ErrNonFiniteScore is returned (defensively) if the
	// computed score evaluates to NaN or Inf. The validator
	// rejects NaN/Inf in inputs and weights, so this error
	// should be unreachable in practice -- but we surface it
	// rather than silently emitting a row with a poisoned
	// score column.
	ErrNonFiniteScore = errors.New(
		"refactor: computed score is NaN or Inf")

	// ErrNilIDFactory is returned by [Computer.Compute] when
	// the caller wired [WithIDFactory] with a nil function.
	// Deferred to Compute time (not NewComputer) so a wiring
	// bug surfaces as a clean error rather than a
	// construction-time panic.
	ErrNilIDFactory = errors.New(
		"refactor: WithIDFactory function is nil")

	// ErrNilClock is the same shape but for [WithClock].
	ErrNilClock = errors.New(
		"refactor: WithClock function is nil")

	// ErrIDFactoryReturnedNil is returned by [Computer.Compute]
	// when the configured ID factory returns `(uuid.Nil, nil)`.
	// The hot_spot row's `hotspot_id` is the table's PRIMARY
	// KEY; a zero uuid is either rejected by the SQL CHECK or
	// (worse) succeeds the first INSERT and collides on the
	// second. The planner refuses to emit a row whose primary
	// key is zero rather than rely on the database to catch it.
	ErrIDFactoryReturnedNil = errors.New(
		"refactor: ID factory returned uuid.Nil")
)
