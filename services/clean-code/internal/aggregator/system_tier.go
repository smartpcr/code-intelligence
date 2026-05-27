package aggregator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// SystemTierComposer materialises the SEVEN canonical system-tier
// `metric_sample` rows per architecture Sec 1.4.2 + Sec 3.10 step
// 4. It is the in-process composition seam the Cross-Repo
// Aggregator drives once per tick per `(repo_id, sha)` pair.
//
// # Canonical metric_kind set (closed list)
//
// The composer is permitted to emit EXACTLY these seven
// `metric_kind` values:
//
//   - `xrepo_dep_depth`           -- repo scope
//   - `arch_debt_ratio`           -- package + repo scope
//   - `velocity_trend`            -- repo scope
//   - `arch_fitness`              -- repo scope
//   - `blast_radius`              -- method + class scope
//   - `xservice_test_reliability` -- repo scope
//   - `knowledge_index`           -- file + repo scope
//
// The Stage 7.1 evaluator iter-1 item 7 finding pins an
// explicit anti-pattern: NO invented metric_kind like
// `p50.system` / `p90.system` / `p95.system` / `p99.system`
// is allowed. Percentile vectors live in
// `clean_code.cross_repo_percentile` columns, not as fake
// metric_kind rows. The composer's [CanonicalSystemTierMetricKinds]
// list is the closed set; the [SystemTierComposer.Compose] return
// slice may not carry any other metric_kind string.
//
// # Pack / source contract (G1 / G6)
//
// Every emitted [SystemTierSample] carries
// [recipes.PackSystem] (`"system"`) and
// [recipes.SourceDerived] (`"derived"`). The
// Cross-Repo Aggregator is the SOLE writer of
// `pack='system'` rows per architecture Sec 1.5 G1 + Sec
// 1.5.1 row 2 (the Phase 1.5 role grant carve-out on
// `clean_code.metric_sample` enforces this at the database
// layer via the `clean_code_xrepo_aggregator` role; see
// migration `0004_roles.up.sql:382-397`). The composer
// rejects callers that supply a non-canonical Pack or
// Source by stamping the canonical literals unconditionally
// at output time.
//
// # Fail-safe contract (architecture Sec 3.10 step 4
// lines 637-657 / tech-spec Sec 4.2 lines 325-339)
//
// The composer NEVER silently drops a system-tier row.
// When a required input is missing, the row is STILL
// EMITTED with `degraded=true` and `degraded_reason` drawn
// from the architecture Sec 8.2 closed list:
//
//   - [DegradedReasonXRepoEdgesUnavailable] -- cross-repo
//     edges required by `xrepo_dep_depth` or `blast_radius`
//     are not present (embedded mode or agent-memory
//     unavailable). Per architecture Sec 1.4.2 row 5,
//     `blast_radius` is ALWAYS degraded with this reason in
//     embedded mode; the architecture treats local fan-in as
//     a non-substitute for the call-graph signal.
//   - [DegradedReasonSamplesPending] -- a required foundation
//     or ingested input row has not yet arrived (e.g.
//     `cycle_member` for `arch_debt_ratio`,
//     `pass_first_try_ratio` for `xservice_test_reliability`,
//     churn author data for `knowledge_index`).
//
// Per migration `0002_measurement.up.sql:367-369` the
// `metric_sample_value_present_unless_degraded` CHECK
// constraint accepts `value IS NULL` IFF `degraded=true`;
// the composer therefore sets [SystemTierSample.Value] to
// `nil` on every degraded row. Non-degraded rows MUST carry
// a finite float (Inf / NaN are rejected at compose time).
//
// # Plan vs. architecture inconsistency (documented)
//
// The implementation-plan Stage 7.2 test scenario
// `embedded-mode-writes-degraded-row` illustrates
// `arch_debt_ratio` as the degraded row in embedded mode;
// architecture Sec 1.4.2 row 2 only names `cycle_member`
// (a LOCAL foundation input) as its required input -- it
// does NOT consume cross-repo edges. The composer follows
// the architecture: in embedded mode `arch_debt_ratio` is
// non-degraded when `cycle_member` rows are present, and
// degraded with [DegradedReasonSamplesPending] (not
// [DegradedReasonXRepoEdgesUnavailable]) when they are not.
// The two kinds that the architecture pins to embedded-mode
// degradation are `xrepo_dep_depth` and `blast_radius` (Sec
// 1.4.2 rows 1 and 5).
//
// # Determinism (G6)
//
// [SystemTierComposer.Compose] sorts its output slice by
// `(metric_kind, scope_kind, scope_id)` so identical inputs
// produce a byte-identical output slice across calls.
// `sample_id` is minted via the injected
// [SystemTierComposerOption] factory (defaulting to
// [uuid.NewV4]); tests inject a deterministic factory.
type SystemTierComposer struct {
	newSampleID func() (uuid.UUID, error)
	now         func() time.Time
}

// Canonical system-tier metric_kind constants (architecture
// Sec 1.4.2). These literals are the only metric_kind values
// the composer is permitted to emit. Defined as named
// constants so a `grep -nF "xrepo_dep_depth"` lands one
// definition site and so a typo in a downstream caller is a
// compile error rather than a silent SQL drift.
const (
	// SystemMetricXRepoDepDepth is architecture Sec 1.4.2 row 1
	// -- the longest dependency chain through the portfolio.
	// Scope: repo. Requires cross-repo edges; embedded mode
	// emits a degraded row with
	// [DegradedReasonXRepoEdgesUnavailable].
	SystemMetricXRepoDepDepth = "xrepo_dep_depth"

	// SystemMetricArchDebtRatio is architecture Sec 1.4.2 row 2
	// -- sum of cycle weights / total package weight. Scope:
	// package and repo. Requires foundation `cycle_member`
	// inputs; missing inputs degrade with
	// [DegradedReasonSamplesPending].
	SystemMetricArchDebtRatio = "arch_debt_ratio"

	// SystemMetricVelocityTrend is architecture Sec 1.4.2 row 3
	// -- trend slope across rolling
	// `modification_count_in_window` samples. Scope: repo.
	// Requires at least two window observations; fewer windows
	// degrade with [DegradedReasonSamplesPending].
	SystemMetricVelocityTrend = "velocity_trend"

	// SystemMetricArchFitness is architecture Sec 1.4.2 row 4
	// -- composite "is the structure getting better or worse"
	// score over `cycle_member`, `fan_in`, and
	// `coupling_between_objects`. Scope: repo. Missing inputs
	// degrade with [DegradedReasonSamplesPending].
	SystemMetricArchFitness = "arch_fitness"

	// SystemMetricBlastRadius is architecture Sec 1.4.2 row 5
	// -- "if this method breaks, how much breaks with it?".
	// Scope: method and class. Requires call-graph input
	// (linked mode); embedded mode emits a degraded row with
	// [DegradedReasonXRepoEdgesUnavailable] per the explicit
	// row 5 contract -- local `fan_in` is NOT a substitute.
	SystemMetricBlastRadius = "blast_radius"

	// SystemMetricXServiceTestReliability is architecture Sec
	// 1.4.2 row 6 -- rolling test-flakiness measure composed
	// from the ingested foundation `pass_first_try_ratio` row.
	// Scope: repo. Missing input degrades with
	// [DegradedReasonSamplesPending].
	SystemMetricXServiceTestReliability = "xservice_test_reliability"

	// SystemMetricKnowledgeIndex is architecture Sec 1.4.2 row
	// 7 -- bus-factor-style index over
	// `modification_count_in_window` plus per-author churn data
	// from `ingest.churn`. Scope: file and repo. Missing churn
	// degrades with [DegradedReasonSamplesPending].
	SystemMetricKnowledgeIndex = "knowledge_index"
)

// CanonicalSystemTierMetricKinds is the closed list of the
// SEVEN canonical system-tier metric_kind strings the composer
// is permitted to emit (architecture Sec 1.4.2). The slice
// order is the architecture-row order; callers MUST NOT rely
// on the order (the composer's emit order is determined by
// scope + scope_id sort, not by this slice).
//
// Returned as a package-level slice for the closed-set test
// in `system_tier_test.go`. Tests that compare against the
// canonical set MUST use [SystemTierMetricKindSet] for
// presence checks rather than indexing into this slice, so
// future re-orderings (none expected in v1) cannot break the
// closed-set assertion.
var CanonicalSystemTierMetricKinds = []string{
	SystemMetricXRepoDepDepth,
	SystemMetricArchDebtRatio,
	SystemMetricVelocityTrend,
	SystemMetricArchFitness,
	SystemMetricBlastRadius,
	SystemMetricXServiceTestReliability,
	SystemMetricKnowledgeIndex,
}

// SystemTierMetricKindSet returns a fresh map keyed by the
// seven canonical metric_kind strings. Tests use it to assert
// the composer's output set is a SUBSET of the closed list
// (and the canonical list is a SUBSET of the output set on the
// happy path).
func SystemTierMetricKindSet() map[string]struct{} {
	out := make(map[string]struct{}, len(CanonicalSystemTierMetricKinds))
	for _, k := range CanonicalSystemTierMetricKinds {
		out[k] = struct{}{}
	}
	return out
}

// IsSystemTierMetricKind reports whether `k` is one of the
// seven canonical system-tier metric_kinds.
func IsSystemTierMetricKind(k string) bool {
	for _, c := range CanonicalSystemTierMetricKinds {
		if c == k {
			return true
		}
	}
	return false
}

// SystemMetricVersion is the v1 `metric_version` stamp on
// every emitted system-tier sample. A definitional change to
// any of the SEVEN canonical metrics (formula or input set)
// MUST bump this constant; architecture C4 / G2 then treats
// the bumped row as a NEW active row at the same
// `(repo_id, sha, scope_id, metric_kind)` quintuple's
// extended key. Pinned as a constant so a `grep -nF
// "SystemMetricVersion"` lands one definition site.
const SystemMetricVersion = 1

// Canonical degraded_reason values the composer is permitted
// to set on a degraded system-tier row (architecture Sec 8.2
// closed list). Two of the four Sec 8.2 reasons apply at the
// composer; the other two
// (`policy_signature_invalid` / `percentile_stale`) are
// emitted by other components (evaluator gate and Insights
// surface, respectively) and the composer MUST NOT use them.
const (
	// DegradedReasonXRepoEdgesUnavailable signals that a
	// cross-repo edge set required by `xrepo_dep_depth` or
	// `blast_radius` is not present (embedded mode, or linked
	// mode where the agent-memory adapter has not yet returned
	// edges). Architecture Sec 8.2 entry 1.
	DegradedReasonXRepoEdgesUnavailable = "xrepo_edges_unavailable"

	// DegradedReasonSamplesPending signals that a required
	// foundation or ingested `metric_sample` input has not yet
	// arrived (e.g. `cycle_member` for `arch_debt_ratio`).
	// Architecture Sec 8.2 entry 2.
	DegradedReasonSamplesPending = "samples_pending"
)

// SystemTierMode is the aggregator's deployment mode for the
// composer. The mode determines which cross-repo inputs are
// available and therefore which metric_kinds degrade
// unconditionally with
// [DegradedReasonXRepoEdgesUnavailable].
type SystemTierMode string

const (
	// SystemTierModeEmbedded is the default v1 mode -- the
	// service runs without the agent-memory linked-mode
	// adapter, so cross-repo edges are unavailable. Per
	// architecture Sec 1.4.2 rows 1 and 5, `xrepo_dep_depth`
	// and `blast_radius` rows are emitted with `degraded=true,
	// degraded_reason='xrepo_edges_unavailable'` in this mode.
	SystemTierModeEmbedded SystemTierMode = "embedded"

	// SystemTierModeLinked is the opt-in mode where the
	// agent-memory adapter (architecture Sec 8.7) supplies
	// cross-repo edges. In linked mode the composer may
	// compute non-degraded `xrepo_dep_depth` and
	// `blast_radius` rows.
	SystemTierModeLinked SystemTierMode = "linked"
)

// SystemTierSample is the in-memory shape of one system-tier
// `clean_code.metric_sample` row the composer emits. Field
// names mirror the migration `0002_measurement.up.sql:257`
// schema columns the writer will persist (minus the
// server-generated `created_at` / `sample_date_bucket` and
// the writer-defaulted `pack` / `source`).
//
// # Value nullability
//
// `Value` is `*float64` because migration
// `0002_measurement.up.sql:288` permits a NULL value IFF
// `degraded=true` (CHECK constraint
// `metric_sample_value_present_unless_degraded`). The
// composer leaves Value nil on every degraded row and sets a
// finite float on every non-degraded row; the writer maps nil
// to PostgreSQL `NULL` directly.
//
// # Pack / source canonicality
//
// `Pack` and `Source` are stamped to the canonical literals
// at compose time and the composer's invariant check rejects
// any drift; the fields exist so the writer's SQL bind step
// has them explicit rather than implicit.
type SystemTierSample struct {
	// SampleID is the new row's `metric_sample.sample_id`
	// UUID, minted by the composer's injected factory so the
	// writer can address the row without a SELECT after
	// INSERT.
	SampleID uuid.UUID
	// RepoID is the parent `metric_sample.repo_id` FK.
	RepoID uuid.UUID
	// SHA is the per-tick HEAD SHA at which the composer ran.
	// Every sample in one Compose call shares the same SHA per
	// the per-tick semantics in architecture Sec 3.10 line 1041.
	SHA string
	// ScopeID is the `scope_binding.scope_id` the sample
	// attaches to. The composer reads scopes from
	// [SystemTierInput.Scopes]; per-kind logic picks the
	// scopes whose `scope_kind` matches the kind's permitted
	// scope_kind set.
	ScopeID uuid.UUID
	// ScopeKind is the `scope_binding.scope_kind` enum value
	// (e.g. `repo`, `package`, `method`). Denormalised onto
	// the sample for the composer's invariant check and for
	// test assertions; the writer can re-derive it via JOIN.
	ScopeKind string
	// MetricKind is one of the seven canonical system-tier
	// strings (see [CanonicalSystemTierMetricKinds]).
	MetricKind string
	// MetricVersion is the `metric_sample.metric_version`
	// stamp. Always [SystemMetricVersion] in v1.
	MetricVersion int
	// Value is the computed metric value. nil iff Degraded.
	Value *float64
	// Pack is always `recipes.PackSystem` ("system"). Stamped
	// here for explicitness; the composer's invariant check
	// asserts the canonical literal.
	Pack string
	// Source is always `recipes.SourceDerived` ("derived").
	// Stamped here for explicitness; the composer's invariant
	// check asserts the canonical literal.
	Source string
	// Degraded is true when a required input was missing and
	// the row carries [DegradedReasonXRepoEdgesUnavailable] or
	// [DegradedReasonSamplesPending].
	Degraded bool
	// DegradedReason is one of the two composer-permitted
	// values (see the package degraded-reason constants).
	// Empty when Degraded is false; required when Degraded is
	// true.
	DegradedReason string
	// ProducerRunID is the parent `scan_run.scan_run_id` the
	// composer ran under. Stamped onto every sample for the
	// `metric_sample.producer_run_id` FK (architecture Sec
	// 5.2.1 line 905).
	ProducerRunID uuid.UUID
	// Attrs are per-sample attributes the composer stamps for
	// downstream consumers (e.g. the upstream foundation
	// `cycle_id` for `arch_debt_ratio`). Composer-emitted
	// attribute keys are documented per-kind in the
	// [SystemTierComposer.Compose] doc comment.
	Attrs map[string]string
}

// ScopeRef is the (scope_id, scope_kind) tuple the composer
// reads from [SystemTierInput.Scopes] to determine which
// scopes exist at the tick's repo+SHA. Per the iter-1
// rubber-duck finding (item 2 in this stage's design
// review), scope existence is decoupled from foundation
// sample presence: a method scope that exists in the AST but
// has no foundation `fan_in` sample yet still produces a
// `blast_radius` degraded row at that scope (rather than
// being silently omitted because no foundation row was
// available to anchor the iteration).
type ScopeRef struct {
	ScopeID   uuid.UUID
	ScopeKind string
}

// FoundationSample is one foundation-tier active
// `metric_sample` row the composer treats as input. The
// composer reads inputs like `cycle_member`, `fan_in`,
// `coupling_between_objects`, `modification_count_in_window`,
// and `pass_first_try_ratio` (ingested-pack) through this
// shape.
//
// Per architecture Sec 5.2.1 the foundation rows the composer
// consumes are always non-degraded (foundation / ingested
// writers never set `degraded=true`); the composer therefore
// does not carry a degraded flag here.
type FoundationSample struct {
	ScopeID    uuid.UUID
	ScopeKind  string
	MetricKind string
	Value      float64
	Attrs      map[string]string
}

// XRepoEdge is one cross-repo dependency edge consumed by the
// composer when computing `xrepo_dep_depth` in linked mode.
// In embedded mode the slice is empty; the composer treats
// emptiness as the degradation signal.
//
// Edges are directed: `FromRepo` depends on `ToRepo`. The
// composer computes the longest dependency CHAIN by following
// `FromRepo -> ToRepo` traversal from each repo.
type XRepoEdge struct {
	FromRepo uuid.UUID
	ToRepo   uuid.UUID
}

// CallEdge is one downstream call-graph edge consumed by the
// composer when computing `blast_radius` in linked mode. In
// embedded mode the slice is unused (`blast_radius` is
// unconditionally degraded per architecture Sec 1.4.2 row 5).
//
// Edges are directed: `FromScope` (a method or class) is
// called by `ToScope` -- i.e. `ToScope` would break if
// `FromScope` regresses. The composer counts distinct
// `ToScope`s reachable transitively from each method / class
// scope to produce the blast-radius value.
type CallEdge struct {
	FromScope uuid.UUID
	ToScope   uuid.UUID
}

// SystemTierInput is the per-tick per-repo input the composer
// reads. One Compose call covers all SEVEN canonical
// metric_kinds for one `(repo_id, sha)` pair.
//
// The composer is a PURE function of this input: identical
// SystemTierInput values produce byte-identical output (modulo
// SampleID, which is minted from the injected factory). This
// lets G6 re-materialise the system-tier sample set
// deterministically from foundation rows at any time.
type SystemTierInput struct {
	// Mode is the aggregator deployment mode. Embedded mode
	// unconditionally degrades `xrepo_dep_depth` and
	// `blast_radius`; linked mode allows them to compute.
	Mode SystemTierMode
	// RepoID is the repository the composer is producing rows
	// for. Stamped onto every emitted SystemTierSample.
	RepoID uuid.UUID
	// SHA is the HEAD SHA at this tick. Stamped onto every
	// emitted SystemTierSample.
	SHA string
	// ProducerRunID is the parent `scan_run.scan_run_id` for
	// the FK on `metric_sample.producer_run_id`.
	ProducerRunID uuid.UUID
	// Scopes is the complete set of scope_binding rows at
	// `(repo_id, sha)`. Per the iter-1 rubber-duck finding the
	// composer iterates THIS list (not the foundation sample
	// list) to determine which scopes to emit a system-tier
	// row for. Scopes whose `scope_kind` is not a target of
	// any canonical kind (e.g. `interface`, `block`) are
	// ignored.
	Scopes []ScopeRef
	// Foundation is the active foundation-tier (and
	// ingested-tier) sample set for `(repo_id, sha)`. The
	// composer consumes these as the per-kind formula inputs.
	Foundation []FoundationSample
	// XRepoEdges is the cross-repo dependency edge set the
	// `xrepo_dep_depth` composer reads in linked mode. Empty
	// in embedded mode.
	XRepoEdges []XRepoEdge
	// CallEdges is the downstream call-graph edge set the
	// `blast_radius` composer reads in linked mode. Empty in
	// embedded mode (`blast_radius` is unconditionally
	// degraded regardless of this slice's content in embedded
	// mode -- see architecture Sec 1.4.2 row 5).
	CallEdges []CallEdge
	// VelocityWindows carries per-window aggregate values for
	// the rolling 4-window `velocity_trend` computation. A
	// slice length < 2 degrades the row with samples_pending
	// per architecture Sec 1.4.2 row 3 brief
	// "degrades to flat if fewer than 2 windows are populated".
	// The composer uses NULL value on a degraded row per the
	// implementation-plan Stage 7.2 fail-safe note
	// "value may be NULL".
	VelocityWindows []float64
	// AuthorsByScope maps `scope_id` (file scope) to the list
	// of distinct author identifiers for the
	// `knowledge_index` bus-factor computation. nil / empty
	// when `ingest.churn` has not yet supplied churn data;
	// the composer emits degraded knowledge_index rows in
	// that case.
	AuthorsByScope map[uuid.UUID][]string
}

// SystemTierComposerOption configures a [SystemTierComposer].
type SystemTierComposerOption func(*SystemTierComposer)

// WithSystemTierSampleIDFactory overrides the UUID factory
// used for `metric_sample.sample_id`. Tests inject a
// deterministic counter so emitted sample IDs are stable
// across test runs.
func WithSystemTierSampleIDFactory(f func() (uuid.UUID, error)) SystemTierComposerOption {
	return func(c *SystemTierComposer) { c.newSampleID = f }
}

// WithSystemTierClock overrides the wall-clock function. The
// composer does not stamp timestamps on the emitted
// SystemTierSample today (the writer's `created_at DEFAULT
// now()` mints it server-side), but the clock is exposed for
// future use and for symmetry with [Aggregator.WithClock].
func WithSystemTierClock(now func() time.Time) SystemTierComposerOption {
	return func(c *SystemTierComposer) { c.now = now }
}

// ErrSystemTierComposerNilSampleIDFactory surfaces a nil
// sample-id factory at composition-root wiring time -- a
// programmer-bug class that should fail loudly rather than
// produce zero-valued sample_ids that collide on insert.
var ErrSystemTierComposerNilSampleIDFactory = errors.New("aggregator: NewSystemTierComposer: sample-id factory is nil")

// ErrSystemTierComposerInvalidInput is returned when the
// caller's [SystemTierInput] is internally inconsistent
// (empty RepoID / empty SHA / nil zero ProducerRunID). The
// composer surfaces this as a real error rather than panicking
// so the caller can decide whether to log + skip or escalate.
var ErrSystemTierComposerInvalidInput = errors.New("aggregator.SystemTierComposer: invalid input")

// NewSystemTierComposer constructs a composer. Returns the
// composer (never nil) and, on misconfiguration, an error.
// Default option set:
//
//   - sample-id factory: [uuid.NewV4]
//   - clock: [time.Now]
func NewSystemTierComposer(opts ...SystemTierComposerOption) (*SystemTierComposer, error) {
	c := &SystemTierComposer{
		newSampleID: uuid.NewV4,
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.newSampleID == nil {
		return nil, ErrSystemTierComposerNilSampleIDFactory
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// Compose emits one [SystemTierSample] per
// `(metric_kind, scope_id)` pair the composer covers at this
// repo + SHA. Every kind in [CanonicalSystemTierMetricKinds]
// produces AT LEAST ONE row when a target scope exists:
//
//   - Per-kind scope filtering: the composer reads
//     [SystemTierInput.Scopes] and keeps only scopes whose
//     `scope_kind` matches the kind's architecture-pinned
//     permitted scope_kind set (e.g. `arch_debt_ratio` emits
//     at `package` and `repo` scopes; `blast_radius` at
//     `method` and `class`).
//   - Per-kind fail-safe: when a required input is missing for
//     a given scope, the composer EMITS a row with
//     `Degraded=true` and the appropriate `DegradedReason`
//     (architecture Sec 8.2 closed list). Value is nil
//     (NULL) on every degraded row per the migration
//     `metric_sample_value_present_unless_degraded` CHECK.
//   - Per-kind formula: when inputs are present, the composer
//     computes the architecture Sec 1.4.2 formula (or its v1
//     proxy where the architecture pins a "composite"
//     placeholder).
//
// Output ordering: emitted samples are sorted by
// `(MetricKind, ScopeKind, ScopeID)` so identical inputs
// produce a byte-identical output slice (G6 determinism).
//
// Errors: returns [ErrSystemTierComposerInvalidInput] on a
// malformed input (empty RepoID, empty SHA, or empty
// ProducerRunID). Returns the underlying error from the
// sample-id factory if it fails to mint a UUID.
func (c *SystemTierComposer) Compose(ctx context.Context, in SystemTierInput) ([]SystemTierSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.RepoID == (uuid.UUID{}) {
		return nil, fmt.Errorf("%w: RepoID is the zero UUID", ErrSystemTierComposerInvalidInput)
	}
	if in.SHA == "" {
		return nil, fmt.Errorf("%w: SHA is empty", ErrSystemTierComposerInvalidInput)
	}
	if in.ProducerRunID == (uuid.UUID{}) {
		return nil, fmt.Errorf("%w: ProducerRunID is the zero UUID", ErrSystemTierComposerInvalidInput)
	}

	// Index foundation samples by (scope_id, metric_kind) for
	// O(1) lookup in the per-kind composers. A scope MAY carry
	// multiple foundation samples (one per metric_kind); the
	// per-kind composer picks the inputs it needs.
	bySample := make(map[foundationKey]FoundationSample, len(in.Foundation))
	bySampleAll := make(map[foundationKey][]FoundationSample, len(in.Foundation))
	byKind := make(map[string][]FoundationSample, len(in.Foundation))
	for _, fs := range in.Foundation {
		k := foundationKey{ScopeID: fs.ScopeID, MetricKind: fs.MetricKind}
		bySample[k] = fs
		bySampleAll[k] = append(bySampleAll[k], fs)
		byKind[fs.MetricKind] = append(byKind[fs.MetricKind], fs)
	}

	// Bucket scopes by scope_kind for per-kind iteration.
	scopesByKind := make(map[string][]ScopeRef, len(in.Scopes))
	for _, s := range in.Scopes {
		scopesByKind[s.ScopeKind] = append(scopesByKind[s.ScopeKind], s)
	}
	// Deterministic per-kind scope ordering so the composer's
	// output is stable for a given input.
	for k := range scopesByKind {
		sortScopeRefs(scopesByKind[k])
	}

	// Pre-build the cross-repo and call-graph adjacency lists
	// once per Compose call so the per-kind logic runs in
	// linear time.
	xrepoAdj := buildXRepoAdjacency(in.XRepoEdges)
	callAdj := buildCallAdjacency(in.CallEdges)

	var out []SystemTierSample

	// Per-kind dispatch. Each composer function returns 0..N
	// samples; every kind that could conceivably emit a row at
	// this input MUST emit (degraded or not) per the fail-safe
	// contract.
	rows, err := c.composeXRepoDepDepth(in, scopesByKind, xrepoAdj)
	if err != nil {
		return nil, err
	}
	out = append(out, rows...)

	rows, err = c.composeArchDebtRatio(in, scopesByKind, byKind, bySampleAll)
	if err != nil {
		return nil, err
	}
	out = append(out, rows...)

	rows, err = c.composeVelocityTrend(in, scopesByKind)
	if err != nil {
		return nil, err
	}
	out = append(out, rows...)

	rows, err = c.composeArchFitness(in, scopesByKind, byKind)
	if err != nil {
		return nil, err
	}
	out = append(out, rows...)

	rows, err = c.composeBlastRadius(in, scopesByKind, callAdj)
	if err != nil {
		return nil, err
	}
	out = append(out, rows...)

	rows, err = c.composeXServiceTestReliability(in, scopesByKind, bySample)
	if err != nil {
		return nil, err
	}
	out = append(out, rows...)

	rows, err = c.composeKnowledgeIndex(in, scopesByKind, byKind)
	if err != nil {
		return nil, err
	}
	out = append(out, rows...)

	// Deterministic ordering for G6.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MetricKind != out[j].MetricKind {
			return out[i].MetricKind < out[j].MetricKind
		}
		if out[i].ScopeKind != out[j].ScopeKind {
			return out[i].ScopeKind < out[j].ScopeKind
		}
		return out[i].ScopeID.String() < out[j].ScopeID.String()
	})

	// Centralised invariant check. Catches any per-kind
	// composer regression (wrong pack, non-canonical kind,
	// non-finite value, degraded without reason, etc.) at the
	// composer boundary -- the writer never sees a malformed
	// row.
	for i := range out {
		if err := validateSystemTierSample(&out[i]); err != nil {
			return nil, fmt.Errorf("aggregator.SystemTierComposer: invariant violated on emitted sample %d: %w", i, err)
		}
	}
	return out, nil
}

// foundationKey is the (scope_id, metric_kind) lookup key
// for foundation samples.
type foundationKey struct {
	ScopeID    uuid.UUID
	MetricKind string
}

// sortScopeRefs sorts in place by ScopeID for deterministic
// output.
func sortScopeRefs(refs []ScopeRef) {
	sort.SliceStable(refs, func(i, j int) bool {
		return refs[i].ScopeID.String() < refs[j].ScopeID.String()
	})
}

// buildXRepoAdjacency returns a `from -> []to` adjacency
// list. The composer uses it for the longest-path traversal
// in `xrepo_dep_depth`. A nil / empty edge slice yields an
// empty map; the composer's downstream logic treats both as
// the embedded-mode signal.
func buildXRepoAdjacency(edges []XRepoEdge) map[uuid.UUID][]uuid.UUID {
	adj := make(map[uuid.UUID][]uuid.UUID, len(edges))
	for _, e := range edges {
		adj[e.FromRepo] = append(adj[e.FromRepo], e.ToRepo)
	}
	for k := range adj {
		// Stable per-node ordering for deterministic traversal.
		sort.Slice(adj[k], func(i, j int) bool {
			return adj[k][i].String() < adj[k][j].String()
		})
	}
	return adj
}

// buildCallAdjacency returns a `from -> []to` adjacency list
// for the call-graph. Same shape and determinism as
// [buildXRepoAdjacency].
func buildCallAdjacency(edges []CallEdge) map[uuid.UUID][]uuid.UUID {
	adj := make(map[uuid.UUID][]uuid.UUID, len(edges))
	for _, e := range edges {
		adj[e.FromScope] = append(adj[e.FromScope], e.ToScope)
	}
	for k := range adj {
		sort.Slice(adj[k], func(i, j int) bool {
			return adj[k][i].String() < adj[k][j].String()
		})
	}
	return adj
}

// composeXRepoDepDepth emits one `xrepo_dep_depth` row per
// `repo`-kind scope. In embedded mode the row is unconditionally
// degraded with [DegradedReasonXRepoEdgesUnavailable] per
// architecture Sec 1.4.2 row 1; in linked mode the value is the
// longest dependency chain length reachable from this RepoID
// through the cross-repo edge set.
func (c *SystemTierComposer) composeXRepoDepDepth(
	in SystemTierInput,
	scopesByKind map[string][]ScopeRef,
	xrepoAdj map[uuid.UUID][]uuid.UUID,
) ([]SystemTierSample, error) {
	repoScopes := scopesByKind[scopeKindRepo]
	if len(repoScopes) == 0 {
		// No repo scope = nothing to anchor xrepo_dep_depth on.
		// Per the fail-safe contract, we emit ZERO rows here
		// rather than degraded rows; the absence of a repo scope
		// is a data-shape error, not a missing-input one. The
		// caller's [SystemTierInput.Scopes] MUST include the repo
		// scope row in production wiring.
		return nil, nil
	}

	embedded := in.Mode != SystemTierModeLinked || len(in.XRepoEdges) == 0
	out := make([]SystemTierSample, 0, len(repoScopes))
	for _, s := range repoScopes {
		if embedded {
			row, err := c.makeDegradedSample(in, s, SystemMetricXRepoDepDepth, DegradedReasonXRepoEdgesUnavailable, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
			continue
		}
		depth := float64(longestDepDepth(in.RepoID, xrepoAdj))
		row, err := c.makeSample(in, s, SystemMetricXRepoDepDepth, depth, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// longestDepDepth returns the longest simple path (in
// edges) reachable from `start` in the directed graph
// `adj`. The traversal uses an explicit visited set per
// recursion stack so cycles in the dependency graph terminate
// rather than blow the call stack -- v1 treats a cycle as a
// "depth cap" at the SCC entry point.
func longestDepDepth(start uuid.UUID, adj map[uuid.UUID][]uuid.UUID) int {
	visited := make(map[uuid.UUID]bool)
	return dfsLongestPath(start, adj, visited)
}

func dfsLongestPath(node uuid.UUID, adj map[uuid.UUID][]uuid.UUID, visited map[uuid.UUID]bool) int {
	if visited[node] {
		return 0
	}
	visited[node] = true
	defer delete(visited, node)
	best := 0
	for _, next := range adj[node] {
		d := 1 + dfsLongestPath(next, adj, visited)
		if d > best {
			best = d
		}
	}
	return best
}

// composeArchDebtRatio emits one row per `package` scope and
// one row per `repo` scope. Required input: foundation
// `cycle_member` rows (architecture Sec 1.4.2 row 2). Missing
// inputs degrade with [DegradedReasonSamplesPending] per the
// composer's fail-safe contract (the architecture does NOT
// require cross-repo edges here -- contrast with the
// implementation-plan Stage 7.2 illustrative scenario, which
// the package doc comment explicitly reconciles).
//
// Formula (v1): per-package value is the count of
// `cycle_member` rows whose `scope_id` equals the package
// scope, divided by the total `cycle_member` count for the
// repo (used as a denominator proxy when no explicit package
// weight is supplied). Per-repo value is the mean of the
// per-package ratios. Both are in [0, 1].
func (c *SystemTierComposer) composeArchDebtRatio(
	in SystemTierInput,
	scopesByKind map[string][]ScopeRef,
	byKind map[string][]FoundationSample,
	bySampleAll map[foundationKey][]FoundationSample,
) ([]SystemTierSample, error) {
	pkgScopes := scopesByKind[scopeKindPackage]
	repoScopes := scopesByKind[scopeKindRepo]
	if len(pkgScopes) == 0 && len(repoScopes) == 0 {
		return nil, nil
	}

	cycleMembers := byKind[foundationKindCycleMember]
	noInputs := len(cycleMembers) == 0
	totalCount := float64(len(cycleMembers))

	out := make([]SystemTierSample, 0, len(pkgScopes)+len(repoScopes))

	// Per-package rows.
	for _, s := range pkgScopes {
		if noInputs {
			row, err := c.makeDegradedSample(in, s, SystemMetricArchDebtRatio, DegradedReasonSamplesPending, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
			continue
		}
		pkgCount := float64(len(bySampleAll[foundationKey{ScopeID: s.ScopeID, MetricKind: foundationKindCycleMember}]))
		var ratio float64
		if totalCount > 0 {
			ratio = pkgCount / totalCount
		}
		row, err := c.makeSample(in, s, SystemMetricArchDebtRatio, ratio, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}

	// Per-repo row(s). The repo aggregate is the mean of the
	// per-package ratios; when there are no package scopes,
	// the aggregate is the bare cycle-fraction (cycles per
	// foundation row -- a trivial proxy for v1).
	for _, s := range repoScopes {
		if noInputs {
			row, err := c.makeDegradedSample(in, s, SystemMetricArchDebtRatio, DegradedReasonSamplesPending, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
			continue
		}
		var ratio float64
		if len(pkgScopes) > 0 {
			var acc float64
			for _, p := range pkgScopes {
				cnt := float64(len(bySampleAll[foundationKey{ScopeID: p.ScopeID, MetricKind: foundationKindCycleMember}]))
				if totalCount > 0 {
					acc += cnt / totalCount
				}
			}
			ratio = acc / float64(len(pkgScopes))
		} else {
			ratio = 1.0
		}
		row, err := c.makeSample(in, s, SystemMetricArchDebtRatio, ratio, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// composeVelocityTrend emits one row per repo scope per
// architecture Sec 1.4.2 row 3. The composer needs at least 2
// window observations to compute a slope; fewer windows
// degrade the row with [DegradedReasonSamplesPending] and a
// nil value (per implementation-plan Stage 7.2 fail-safe
// "value may be NULL").
//
// Formula: simple linear regression slope over the
// VelocityWindows slice (windows ordered oldest -> newest).
func (c *SystemTierComposer) composeVelocityTrend(
	in SystemTierInput,
	scopesByKind map[string][]ScopeRef,
) ([]SystemTierSample, error) {
	repoScopes := scopesByKind[scopeKindRepo]
	if len(repoScopes) == 0 {
		return nil, nil
	}
	out := make([]SystemTierSample, 0, len(repoScopes))
	insufficient := len(in.VelocityWindows) < 2
	var slope float64
	if !insufficient {
		slope = linearSlope(in.VelocityWindows)
	}
	for _, s := range repoScopes {
		if insufficient {
			row, err := c.makeDegradedSample(in, s, SystemMetricVelocityTrend, DegradedReasonSamplesPending, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
			continue
		}
		row, err := c.makeSample(in, s, SystemMetricVelocityTrend, slope, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// linearSlope returns the slope of the best-fit line through
// `ys` (with implicit x = 0, 1, 2, ..., n-1). Returns 0 for
// constant series or n < 2 inputs.
func linearSlope(ys []float64) float64 {
	n := float64(len(ys))
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumXX float64
	for i, y := range ys {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	den := n*sumXX - sumX*sumX
	if den == 0 {
		return 0
	}
	return (n*sumXY - sumX*sumY) / den
}

// composeArchFitness emits one row per repo scope per
// architecture Sec 1.4.2 row 4. Required inputs:
// `cycle_member`, `fan_in`, `coupling_between_objects`.
// Missing any of the three degrades with
// [DegradedReasonSamplesPending].
//
// Formula (v1 composite): `1 / (1 + cycle_count) -
// 0.5 * normalised_mean_coupling`. The intuition: cycles drag
// the score down (more cycles -> closer to 0), coupling
// penalises further. Clamped to [0, 1]. The architecture
// pinned formula is a "composite" placeholder; the v1 shape
// is a reasonable starting point that can be re-tuned in a
// future metric_version bump.
func (c *SystemTierComposer) composeArchFitness(
	in SystemTierInput,
	scopesByKind map[string][]ScopeRef,
	byKind map[string][]FoundationSample,
) ([]SystemTierSample, error) {
	repoScopes := scopesByKind[scopeKindRepo]
	if len(repoScopes) == 0 {
		return nil, nil
	}

	cycleMembers := byKind[foundationKindCycleMember]
	fanIn := byKind[foundationKindFanIn]
	coupling := byKind[foundationKindCouplingBetweenObjects]

	missing := len(cycleMembers) == 0 || len(fanIn) == 0 || len(coupling) == 0

	out := make([]SystemTierSample, 0, len(repoScopes))
	var fitness float64
	if !missing {
		cycleScore := 1.0 / (1.0 + float64(len(cycleMembers)))
		couplingMean := meanValues(coupling)
		// Normalise coupling by an arbitrary v1 cap so the
		// term lands in [0, 0.5]. The Stage 8.x rule-tuning
		// workstream may re-pick this denominator.
		couplingNorm := couplingMean / (1.0 + couplingMean)
		fitness = cycleScore - 0.5*couplingNorm
		if fitness < 0 {
			fitness = 0
		}
		if fitness > 1 {
			fitness = 1
		}
	}
	for _, s := range repoScopes {
		if missing {
			row, err := c.makeDegradedSample(in, s, SystemMetricArchFitness, DegradedReasonSamplesPending, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
			continue
		}
		row, err := c.makeSample(in, s, SystemMetricArchFitness, fitness, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// meanValues returns the arithmetic mean of the foundation
// sample values. Returns 0 for empty input.
func meanValues(xs []FoundationSample) float64 {
	if len(xs) == 0 {
		return 0
	}
	var acc float64
	for _, x := range xs {
		acc += x.Value
	}
	return acc / float64(len(xs))
}

// composeBlastRadius emits one row per `method`-kind scope
// and one row per `class`-kind scope per architecture Sec
// 1.4.2 row 5. In embedded mode the row is UNCONDITIONALLY
// degraded with [DegradedReasonXRepoEdgesUnavailable] per the
// row 5 contract -- the architecture treats local fan_in as
// a non-substitute for the call-graph signal in v1. Linked
// mode counts downstream scopes reachable from each scope via
// the call-graph adjacency list.
func (c *SystemTierComposer) composeBlastRadius(
	in SystemTierInput,
	scopesByKind map[string][]ScopeRef,
	callAdj map[uuid.UUID][]uuid.UUID,
) ([]SystemTierSample, error) {
	methodScopes := scopesByKind[scopeKindMethod]
	classScopes := scopesByKind[scopeKindClass]
	total := len(methodScopes) + len(classScopes)
	if total == 0 {
		return nil, nil
	}
	embedded := in.Mode != SystemTierModeLinked
	out := make([]SystemTierSample, 0, total)
	emit := func(s ScopeRef) error {
		if embedded {
			row, err := c.makeDegradedSample(in, s, SystemMetricBlastRadius, DegradedReasonXRepoEdgesUnavailable, nil)
			if err != nil {
				return err
			}
			out = append(out, row)
			return nil
		}
		radius := float64(reachableCount(s.ScopeID, callAdj))
		row, err := c.makeSample(in, s, SystemMetricBlastRadius, radius, nil)
		if err != nil {
			return err
		}
		out = append(out, row)
		return nil
	}
	for _, s := range methodScopes {
		if err := emit(s); err != nil {
			return nil, err
		}
	}
	for _, s := range classScopes {
		if err := emit(s); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// reachableCount returns the count of DISTINCT downstream
// scopes reachable from `start` in the directed graph `adj`
// (exclusive of `start`). v1 uses a simple BFS with a visited
// set so cycles terminate.
func reachableCount(start uuid.UUID, adj map[uuid.UUID][]uuid.UUID) int {
	visited := map[uuid.UUID]bool{start: true}
	queue := []uuid.UUID{start}
	count := 0
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range adj[node] {
			if visited[next] {
				continue
			}
			visited[next] = true
			count++
			queue = append(queue, next)
		}
	}
	return count
}

// composeXServiceTestReliability emits one row per repo scope
// per architecture Sec 1.4.2 row 6. Required input: the
// ingested-pack foundation `pass_first_try_ratio` row at the
// repo scope (architecture Sec 3.10 step 4 lines 651-657 +
// tech-spec Sec 4.1.1 row "pass_first_try_ratio"). Missing
// input degrades with [DegradedReasonSamplesPending].
//
// Formula (v1): pass-through of the foundation
// `pass_first_try_ratio` value. The architecture pins this
// as a "rolling test-flakiness measure"; the v1 pass-through
// is the simplest correct shape, and any rolling-window
// aggregation is a metric_version bump.
func (c *SystemTierComposer) composeXServiceTestReliability(
	in SystemTierInput,
	scopesByKind map[string][]ScopeRef,
	bySample map[foundationKey]FoundationSample,
) ([]SystemTierSample, error) {
	repoScopes := scopesByKind[scopeKindRepo]
	if len(repoScopes) == 0 {
		return nil, nil
	}
	out := make([]SystemTierSample, 0, len(repoScopes))
	for _, s := range repoScopes {
		fs, ok := bySample[foundationKey{ScopeID: s.ScopeID, MetricKind: foundationKindPassFirstTryRatio}]
		if !ok {
			row, err := c.makeDegradedSample(in, s, SystemMetricXServiceTestReliability, DegradedReasonSamplesPending, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
			continue
		}
		row, err := c.makeSample(in, s, SystemMetricXServiceTestReliability, fs.Value, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// composeKnowledgeIndex emits one row per file scope and one
// row per repo scope per architecture Sec 1.4.2 row 7.
// Required inputs: foundation `modification_count_in_window`
// (locates active files) AND per-scope author list (from
// `ingest.churn`). Missing inputs degrade with
// [DegradedReasonSamplesPending] -- the architecture
// explicitly names `samples_pending` for this metric when
// `ingest.churn` has not yet arrived.
//
// Formula (v1): per-file value is `1 - max(author_share)`
// where `max(author_share)` is the most active author's
// fraction of the contributors (bus-factor-style: 0 means a
// single owner, closer to 1 means knowledge is spread).
// Per-repo value is the mean of the per-file values.
func (c *SystemTierComposer) composeKnowledgeIndex(
	in SystemTierInput,
	scopesByKind map[string][]ScopeRef,
	byKind map[string][]FoundationSample,
) ([]SystemTierSample, error) {
	fileScopes := scopesByKind[scopeKindFile]
	repoScopes := scopesByKind[scopeKindRepo]
	if len(fileScopes) == 0 && len(repoScopes) == 0 {
		return nil, nil
	}

	modCounts := byKind[foundationKindModificationCount]
	noMods := len(modCounts) == 0
	noAuthors := len(in.AuthorsByScope) == 0

	missing := noMods || noAuthors

	out := make([]SystemTierSample, 0, len(fileScopes)+len(repoScopes))

	perFile := make(map[uuid.UUID]float64, len(fileScopes))
	if !missing {
		for _, s := range fileScopes {
			authors := in.AuthorsByScope[s.ScopeID]
			perFile[s.ScopeID] = busFactor(authors)
		}
	}

	for _, s := range fileScopes {
		if missing {
			row, err := c.makeDegradedSample(in, s, SystemMetricKnowledgeIndex, DegradedReasonSamplesPending, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
			continue
		}
		row, err := c.makeSample(in, s, SystemMetricKnowledgeIndex, perFile[s.ScopeID], nil)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	for _, s := range repoScopes {
		if missing {
			row, err := c.makeDegradedSample(in, s, SystemMetricKnowledgeIndex, DegradedReasonSamplesPending, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
			continue
		}
		var acc float64
		var n int
		for _, v := range perFile {
			acc += v
			n++
		}
		var mean float64
		if n > 0 {
			mean = acc / float64(n)
		}
		row, err := c.makeSample(in, s, SystemMetricKnowledgeIndex, mean, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// busFactor returns `1 - max(author_share)` where
// `max(author_share)` is the most active author's fraction of
// the contributor list. Each entry in `authors` is counted as
// one contribution. Returns 0 for empty input (single owner
// proxy). The returned value is in [0, 1).
func busFactor(authors []string) float64 {
	if len(authors) == 0 {
		return 0
	}
	counts := make(map[string]int, len(authors))
	for _, a := range authors {
		counts[a]++
	}
	var max int
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	share := float64(max) / float64(len(authors))
	return 1.0 - share
}

// makeSample stamps a non-degraded SystemTierSample with
// canonical pack/source, MetricVersion = SystemMetricVersion,
// the input's repo/sha/producer_run_id, and a freshly minted
// sample_id. The caller MUST provide a finite (non-NaN, non-Inf)
// value; non-finite floats are rejected.
func (c *SystemTierComposer) makeSample(
	in SystemTierInput,
	scope ScopeRef,
	metricKind string,
	value float64,
	attrs map[string]string,
) (SystemTierSample, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return SystemTierSample{}, fmt.Errorf("aggregator.SystemTierComposer: non-finite value for metric_kind=%q at scope_id=%s: %v", metricKind, scope.ScopeID, value)
	}
	id, err := c.newSampleID()
	if err != nil {
		return SystemTierSample{}, fmt.Errorf("aggregator.SystemTierComposer: mint sample_id: %w", err)
	}
	v := value
	return SystemTierSample{
		SampleID:       id,
		RepoID:         in.RepoID,
		SHA:            in.SHA,
		ScopeID:        scope.ScopeID,
		ScopeKind:      scope.ScopeKind,
		MetricKind:     metricKind,
		MetricVersion:  SystemMetricVersion,
		Value:          &v,
		Pack:           string(recipes.PackSystem),
		Source:         string(recipes.SourceDerived),
		Degraded:       false,
		DegradedReason: "",
		ProducerRunID:  in.ProducerRunID,
		Attrs:          copyAttrs(attrs),
	}, nil
}

// makeDegradedSample stamps a degraded SystemTierSample with
// `Value=nil`, the given degraded reason, and canonical pack /
// source. Per migration `0002_measurement.up.sql:367-369` the
// `metric_sample_value_present_unless_degraded` CHECK requires
// `value IS NULL OR degraded=true` -- the composer always
// chooses the NULL-value branch on degraded rows so the
// downstream writer's binding is unambiguous.
func (c *SystemTierComposer) makeDegradedSample(
	in SystemTierInput,
	scope ScopeRef,
	metricKind string,
	reason string,
	attrs map[string]string,
) (SystemTierSample, error) {
	id, err := c.newSampleID()
	if err != nil {
		return SystemTierSample{}, fmt.Errorf("aggregator.SystemTierComposer: mint sample_id (degraded): %w", err)
	}
	return SystemTierSample{
		SampleID:       id,
		RepoID:         in.RepoID,
		SHA:            in.SHA,
		ScopeID:        scope.ScopeID,
		ScopeKind:      scope.ScopeKind,
		MetricKind:     metricKind,
		MetricVersion:  SystemMetricVersion,
		Value:          nil,
		Pack:           string(recipes.PackSystem),
		Source:         string(recipes.SourceDerived),
		Degraded:       true,
		DegradedReason: reason,
		ProducerRunID:  in.ProducerRunID,
		Attrs:          copyAttrs(attrs),
	}, nil
}

// copyAttrs returns a deep copy of `m` (or nil for nil input).
// The composer never holds a reference to caller-provided maps
// so a downstream mutation cannot perturb an emitted row.
func copyAttrs(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// validateSystemTierSample asserts the centralised invariants
// every emitted [SystemTierSample] MUST satisfy:
//
//   - MetricKind is one of [CanonicalSystemTierMetricKinds].
//   - MetricVersion is [SystemMetricVersion] (v1).
//   - Pack is `recipes.PackSystem`.
//   - Source is `recipes.SourceDerived`.
//   - Degraded=true requires DegradedReason in the
//     composer-permitted closed set and Value=nil.
//   - Degraded=false requires DegradedReason="" and a finite
//     non-nil Value.
//   - SampleID, RepoID, ScopeID, ProducerRunID are non-zero
//     UUIDs.
//   - SHA, ScopeKind are non-empty.
//
// Any violation is a composer-side regression; the test suite
// catches it via the centralised check rather than per-kind
// assertions.
func validateSystemTierSample(s *SystemTierSample) error {
	if s == nil {
		return errors.New("nil sample")
	}
	if !IsSystemTierMetricKind(s.MetricKind) {
		return fmt.Errorf("metric_kind %q is not in the canonical system-tier closed set", s.MetricKind)
	}
	if s.MetricVersion != SystemMetricVersion {
		return fmt.Errorf("metric_version=%d, want %d", s.MetricVersion, SystemMetricVersion)
	}
	if s.Pack != string(recipes.PackSystem) {
		return fmt.Errorf("pack=%q, want %q", s.Pack, string(recipes.PackSystem))
	}
	if s.Source != string(recipes.SourceDerived) {
		return fmt.Errorf("source=%q, want %q", s.Source, string(recipes.SourceDerived))
	}
	if s.SampleID == (uuid.UUID{}) {
		return errors.New("SampleID is the zero UUID")
	}
	if s.RepoID == (uuid.UUID{}) {
		return errors.New("RepoID is the zero UUID")
	}
	if s.ScopeID == (uuid.UUID{}) {
		return errors.New("ScopeID is the zero UUID")
	}
	if s.ProducerRunID == (uuid.UUID{}) {
		return errors.New("ProducerRunID is the zero UUID")
	}
	if s.SHA == "" {
		return errors.New("SHA is empty")
	}
	if s.ScopeKind == "" {
		return errors.New("ScopeKind is empty")
	}
	if s.Degraded {
		switch s.DegradedReason {
		case DegradedReasonXRepoEdgesUnavailable, DegradedReasonSamplesPending:
			// ok
		default:
			return fmt.Errorf("degraded=true but degraded_reason=%q is not in the composer's permitted closed set", s.DegradedReason)
		}
		if s.Value != nil {
			return fmt.Errorf("degraded=true requires Value=nil, got %v", *s.Value)
		}
		return nil
	}
	if s.DegradedReason != "" {
		return fmt.Errorf("degraded=false requires DegradedReason empty, got %q", s.DegradedReason)
	}
	if s.Value == nil {
		return errors.New("degraded=false requires non-nil Value")
	}
	if math.IsNaN(*s.Value) || math.IsInf(*s.Value, 0) {
		return fmt.Errorf("non-finite Value: %v", *s.Value)
	}
	return nil
}

// Canonical scope_kind string literals the composer reads
// from [SystemTierInput.Scopes]. Pinned here so a single
// `grep -nF "repo"` lands the canonical token (mirrors the
// migration `0002_measurement.up.sql:140-152` enum).
const (
	scopeKindRepo    = "repo"
	scopeKindPackage = "package"
	scopeKindFile    = "file"
	scopeKindClass   = "class"
	scopeKindMethod  = "method"
)

// Canonical foundation-tier metric_kind strings the composer
// reads from [SystemTierInput.Foundation]. Pinned here so a
// `grep -nF "cycle_member"` lands one definition site that
// cites the upstream producer.
const (
	foundationKindCycleMember             = "cycle_member"
	foundationKindFanIn                   = "fan_in"
	foundationKindCouplingBetweenObjects  = "coupling_between_objects"
	foundationKindModificationCount       = "modification_count_in_window"
	foundationKindPassFirstTryRatio       = "pass_first_try_ratio"
)

// SystemTierWriter is the persistence seam the composer's
// emitted samples flow through. The production implementation
// (Stage 7.x follow-up) targets `clean_code.metric_sample`
// AND the `metric_sample_active` pointer table inside one
// transaction per WriteSystemTierSamples call -- the same
// active-row UPSERT pattern the foundation writer uses (see
// `metric_ingestor/pg_metric_sample_writer.go`).
//
// The composer's Stage 7.2 contract is pure-function: it
// produces samples and returns them. Persistence is a
// separate concern; this interface defines the seam so the
// follow-up PG implementation lands cleanly. The in-memory
// implementation [InMemorySystemTierWriter] captures writes
// for the composer's unit + integration tests today.
//
// Per architecture Sec 5.2.1 line 1041, when the writer's
// tick lands on a SHA where an active derived row already
// exists for a `(repo_id, sha, scope_id, metric_kind,
// metric_version)` quintuple, the implementation MUST skip
// the insert for that SHA (the deduplication is a writer
// concern, not a composer concern). The PG implementation
// will enforce this via an existence check on
// `metric_sample_active` before the INSERT.
type SystemTierWriter interface {
	// WriteSystemTierSamples persists `samples` as a single
	// atomic unit. Implementations MUST honour the active-row
	// uniqueness contract per the doc comment above. Empty
	// slice MUST be a no-op (no transaction, no error).
	WriteSystemTierSamples(ctx context.Context, samples []SystemTierSample) error
}

// InMemorySystemTierWriter is the test-side
// [SystemTierWriter]. Each successful WriteSystemTierSamples
// call appends the slice to a goroutine-safe slice of slices;
// tests assert on the captured shape.
//
// The in-memory writer ALSO enforces the canonical pack /
// source / kind invariants (mirroring the centralised check
// inside the composer) so a test that fakes a SystemTierSample
// outside the composer cannot bypass the canonical-shape gate
// the production PG writer will likewise enforce at the SQL
// constraint layer.
type InMemorySystemTierWriter struct {
	mu      sync.Mutex
	batches [][]SystemTierSample
	// failErr, when non-nil, is returned by every
	// WriteSystemTierSamples call without recording the
	// samples. Tests set it to simulate a PG outage.
	failErr error
}

// NewInMemorySystemTierWriter constructs an empty writer.
func NewInMemorySystemTierWriter() *InMemorySystemTierWriter {
	return &InMemorySystemTierWriter{}
}

// SetFailError configures the writer to return `err` on
// every subsequent WriteSystemTierSamples call. Pass nil to
// clear.
func (w *InMemorySystemTierWriter) SetFailError(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failErr = err
}

// WriteSystemTierSamples implements [SystemTierWriter].
// Validates each sample's canonical shape before recording the
// batch; a malformed sample fails the whole batch (no partial
// record).
func (w *InMemorySystemTierWriter) WriteSystemTierSamples(ctx context.Context, samples []SystemTierSample) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(samples) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failErr != nil {
		return w.failErr
	}
	for i := range samples {
		if err := validateSystemTierSample(&samples[i]); err != nil {
			return fmt.Errorf("aggregator.InMemorySystemTierWriter: sample %d invariant violated: %w", i, err)
		}
	}
	// Deep-copy the slice so a caller mutating its locals
	// after the write does not perturb the captured batch.
	cp := make([]SystemTierSample, len(samples))
	copy(cp, samples)
	w.batches = append(w.batches, cp)
	return nil
}

// Batches returns a copy of the captured write history. Each
// entry is one WriteSystemTierSamples call; tests assert on
// the resulting slice of slices.
func (w *InMemorySystemTierWriter) Batches() [][]SystemTierSample {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([][]SystemTierSample, len(w.batches))
	for i, b := range w.batches {
		cp := make([]SystemTierSample, len(b))
		copy(cp, b)
		out[i] = cp
	}
	return out
}

// AllSamples returns a flat slice of every sample captured
// across every Batches() entry, in invocation order. Useful
// for tests that don't care about batch boundaries.
func (w *InMemorySystemTierWriter) AllSamples() []SystemTierSample {
	w.mu.Lock()
	defer w.mu.Unlock()
	var n int
	for _, b := range w.batches {
		n += len(b)
	}
	out := make([]SystemTierSample, 0, n)
	for _, b := range w.batches {
		out = append(out, b...)
	}
	return out
}
