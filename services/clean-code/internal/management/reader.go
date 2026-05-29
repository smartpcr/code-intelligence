package management

// Stage 6.3 -- Management read verbs and Insights projections.
//
// This file owns the canonical read surface of the clean-code
// service. Eight verbs land here, partitioned by architecture
// Sec 5.2.1 lines 949-1015 into two disjoint READ MODES:
//
//   * SHA-PINNED (six verbs) -- `mgmt.read.repo`,
//     `mgmt.read.metric_sample`, `mgmt.read.metric_samples`,
//     `mgmt.read.findings`, `mgmt.read.regressions`,
//     `mgmt.read.refactor_plan`. These return the canonical
//     active row at the requested SHA without substitution.
//     Reads route through the Phase 1.3 `metric_sample_active`
//     side relation (migration `0002_measurement.up.sql:506-552`)
//     and filter retracted samples via `metric_retraction`
//     (architecture Sec 5.2.2 lines 1035-1037).
//
//   * LATEST-DASHBOARD (two verbs) -- `mgmt.read.cross_repo`,
//     `mgmt.read.portfolio`. These return the latest
//     pre-computed snapshot rows from `cross_repo_percentile`
//     and `portfolio_snapshot` (Phase 7 aggregator output).
//     Every response runs through the Insights freshness
//     projection ([insights.Freshness]); a stale `built_at`
//     stamps `degraded=true, degraded_reason='percentile_stale'`
//     on the envelope (architecture Sec 7.3 / tech-spec Sec 8.2
//     `freshness_window_seconds`).
//
// # Single Reader, explicit mode (architecture Sec 6.3)
//
// "Both modes route through a single `internal/management/
// reader.go` with explicit `mode = sha_pinned | latest_dashboard`."
// The mode is BAKED INTO the verb shape, NOT a caller argument:
// the SHA-pinned verbs always return [ReadModeSHAPinned]; the
// latest-dashboard verbs always return [ReadModeLatestDashboard].
// Callers MUST treat the response's `Mode` as the canonical
// signal of which projection semantics apply -- e.g. the
// Stage 6.4 HTTP gateway uses Mode to decide whether to attach
// the freshness banner on the wire.
//
// # MetricsBackend seam + production PG implementation
//
// Reader holds a single [MetricsBackend] interface (8 methods,
// one per verb's underlying read). Tests inject a pure-Go fake
// (see `reader_test.go`); the PRODUCTION PG-backed implementation
// lives in `pg_metrics_backend.go` in this same package as
// [PGMetricsBackend] -- constructed via [NewPGMetricsBackend]
// (default `clean_code` schema) or [NewPGMetricsBackendWithSchema]
// for an isolated test schema. Both constructors return
// `(*PGMetricsBackend, error)` -- the canonical Stage 6.4 HTTP
// gateway wiring shape is:
//
//	pg, err := management.NewPGMetricsBackend(db)
//	if err != nil { return err }
//	r := management.NewReader(km, management.WithMetricsBackend(pg))
//
// Stage 6.3's mandate is the verb-to-table contract, the
// mode-tagging, the freshness wiring, AND the PG implementation
// of every read verb; the composition-root call site lands in
// Stage 6.4 alongside the HTTP route handlers.
//
// # Reader-side invariant assertions (rubber-duck item 3)
//
// SHA-pinned reads carry a defensive post-condition check: a
// row returned with a mismatched `repo_id` or `sha` is treated
// as a backend invariant violation ([ErrBackendInvariantViolation]).
// This is cheap "trust but verify": the PG backend's WHERE
// clause is the primary correctness gate; this guard catches a
// fake backend that returns the wrong row OR a future writer
// bug that lands a row at the wrong quintuple.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/management/insights"
	"forge/services/clean-code/internal/policy/keys"
)

// ErrManagerUnavailable is returned by [Reader] methods when
// the underlying [keys.Manager] dependency is nil. Catches a
// composition-root wiring bug at the first verb call rather
// than at runtime via a nil-pointer panic.
var ErrManagerUnavailable = errors.New("management: signing key manager not wired")

// ErrBackendUnavailable is returned by [Reader] read verbs when
// the [MetricsBackend] dependency is nil. Mirrors
// [ErrManagerUnavailable]: the verb is wired but its backing
// substrate is not -- the HTTP layer maps this to a 503 per the
// scaffold-mode contract.
var ErrBackendUnavailable = errors.New("management: metrics backend not wired")

// ErrNotFound is returned by [Reader] read verbs when the row
// the caller asked for does not exist (e.g. no metric_sample
// at the requested quintuple, no refactor_plan at the
// requested (repo_id, sha)). Distinct from a backend
// infrastructure error so the HTTP layer can map this to a
// 404 while infrastructure errors stay 500/503.
var ErrNotFound = errors.New("management: row not found")

// ErrBackendInvariantViolation is returned by SHA-pinned
// [Reader] verbs when the backend returned a row whose
// `repo_id` / `sha` / `scope_id` / `metric_kind` did NOT match
// the request. The Reader is the boundary that enforces "a
// SHA-pinned reader NEVER silently substitutes a later-SHA
// value for an earlier-SHA query" (architecture Sec 5.2.1
// lines 971-977); this sentinel surfaces a backend bug at the
// boundary instead of letting a wrong-SHA row propagate into
// a `Finding` or a wire response.
var ErrBackendInvariantViolation = errors.New("management: backend returned a row that does not match the requested SHA-pinned identity")

// ReadMode is the canonical mode tag stamped on every read
// verb response envelope per architecture Sec 5.2.1 lines
// 949-1015 and Sec 6.3. The two values are DISJOINT -- a
// single response is either SHA-pinned or latest-dashboard,
// never both.
type ReadMode string

// Canonical read-mode values. Stored as exported string
// constants (rather than `iota` integers) so the wire layer
// can echo the mode tag verbatim and operators can grep for
// the literal across the codebase.
const (
	// ReadModeSHAPinned tags responses from verbs that read
	// the canonical active row at a specific SHA without
	// substitution. The SHA-pinned six are:
	// `mgmt.read.repo`, `mgmt.read.metric_sample`,
	// `mgmt.read.metric_samples`, `mgmt.read.findings`,
	// `mgmt.read.regressions`, `mgmt.read.refactor_plan`.
	// `mgmt.read.repo` has no sha argument but is bucketed
	// here per the Stage 6.3 brief because it returns a
	// catalog row by primary key (no substitution semantics
	// apply either way).
	ReadModeSHAPinned ReadMode = "sha_pinned"

	// ReadModeLatestDashboard tags responses from verbs that
	// read the latest pre-computed aggregator snapshot. The
	// latest-dashboard two are: `mgmt.read.cross_repo`,
	// `mgmt.read.portfolio`. Both attach the Insights
	// freshness banner ([insights.DegradedReasonPercentileStale])
	// when the snapshot's `built_at` is older than
	// [insights.FreshnessWindowSeconds].
	ReadModeLatestDashboard ReadMode = "latest_dashboard"
)

// -----------------------------------------------------------
// Row projections -- one type per underlying table the Reader
// surface returns.
// -----------------------------------------------------------

// RepoRow is the `mgmt.read.repo` projection of
// `clean_code.repo` (architecture Sec 5.1.1).
type RepoRow struct {
	RepoID            uuid.UUID `json:"repo_id"`
	DisplayName       string    `json:"display_name"`
	DefaultBranch     string    `json:"default_branch"`
	Mode              string    `json:"mode"`
	RepoURL           string    `json:"repo_url,omitempty"`
	DefaultBranchHead string    `json:"default_branch_head,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// MetricSampleRow is the `mgmt.read.metric_sample(s)`
// projection of `clean_code.metric_sample` (architecture
// Sec 5.2.1). The Reader returns ONLY rows that the
// active-row pointer resolves to (Phase 1.3
// `metric_sample_active` join) AND whose `sample_id` has no
// matching `metric_retraction` (architecture Sec 5.2.2 lines
// 1035-1037).
type MetricSampleRow struct {
	SampleID       uuid.UUID `json:"sample_id"`
	RepoID         uuid.UUID `json:"repo_id"`
	SHA            string    `json:"sha"`
	ScopeID        uuid.UUID `json:"scope_id"`
	MetricKind     string    `json:"metric_kind"`
	MetricVersion  int       `json:"metric_version"`
	Value          *float64  `json:"value,omitempty"`
	Pack           string    `json:"pack"`
	Source         string    `json:"source"`
	Degraded       bool      `json:"degraded"`
	DegradedReason string    `json:"degraded_reason,omitempty"`
	ProducerRunID  uuid.UUID `json:"producer_run_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// MetricSamplesFilter narrows the rows
// `mgmt.read.metric_samples(repo_id, sha, filter)` returns.
// Empty / zero fields are wildcards -- e.g. an unset
// `MetricKind` returns every metric kind at (repo_id, sha).
type MetricSamplesFilter struct {
	MetricKind string    `json:"metric_kind,omitempty"`
	ScopeID    uuid.UUID `json:"scope_id,omitempty"`
	Pack       string    `json:"pack,omitempty"`
	Source     string    `json:"source,omitempty"`
}

// FindingRow is the `mgmt.read.findings` /
// `mgmt.read.regressions` projection of `clean_code.finding`
// (architecture Sec 5.4.1 lines 1183-1190).
//
// `Delta` is the canonical four-value closed set
// `new | newly_failing | unchanged | resolved` (architecture
// Sec 5.4.1 line 1189). `mgmt.read.regressions` returns
// EXCLUSIVELY rows whose `Delta == "newly_failing"`.
type FindingRow struct {
	FindingID        uuid.UUID `json:"finding_id"`
	EvaluationRunID  uuid.UUID `json:"evaluation_run_id"`
	RepoID           uuid.UUID `json:"repo_id"`
	SHA              string    `json:"sha"`
	ScopeID          uuid.UUID `json:"scope_id"`
	RuleID           string    `json:"rule_id"`
	RuleVersion      int       `json:"rule_version"`
	PolicyVersionID  uuid.UUID `json:"policy_version_id"`
	MetricSampleIDs  []uuid.UUID `json:"metric_sample_ids,omitempty"`
	Severity         string    `json:"severity"`
	Delta            string    `json:"delta"`
	ExplanationMD    string    `json:"explanation_md,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// RefactorTaskRow is the per-task projection embedded inside
// the `mgmt.read.refactor_plan` response (architecture Sec
// 5.5.3 lines 1239-1250). `Kind` is the canonical five-value
// closed set
// `split_class | extract_method | invert_dependency | break_cycle | consolidate_duplication`.
type RefactorTaskRow struct {
	TaskID        uuid.UUID `json:"task_id"`
	PlanID        uuid.UUID `json:"plan_id"`
	ScopeID       uuid.UUID `json:"scope_id"`
	Kind          string    `json:"kind"`
	EffortHours   float64   `json:"effort_hours"`
	RuleID        string    `json:"rule_id"`
	DescriptionMD string    `json:"description_md"`
	CreatedAt     time.Time `json:"created_at"`
}

// RefactorPlanRow is the `mgmt.read.refactor_plan` projection
// of `clean_code.refactor_plan` + `clean_code.refactor_task`
// (architecture Sec 5.5.2 lines 1226-1237, Sec 5.5.3 lines
// 1239-1250). The plan row carries its embedded tasks so the
// caller (Insights dashboard) does not need a second round-trip.
type RefactorPlanRow struct {
	PlanID     uuid.UUID         `json:"plan_id"`
	RepoID     uuid.UUID         `json:"repo_id"`
	SHA        string            `json:"sha"`
	HotspotIDs []uuid.UUID       `json:"hotspot_ids,omitempty"`
	SummaryMD  string            `json:"summary_md,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	Tasks      []RefactorTaskRow `json:"tasks,omitempty"`
}

// CrossRepoRow is the `mgmt.read.cross_repo` projection of
// `clean_code.cross_repo_percentile` (architecture Sec 5.2.5
// lines 1072-1080). The fields `P50`, `P90`, `P99`, and
// `HistogramJSON` are echoed VERBATIM from the row -- the
// Reader does NOT recompute aggregates on the read path
// (impl-plan Stage 6.3 scenario `latest-dashboard-returns-snapshot`).
// `BuiltAt` is the aggregator's tick timestamp; the
// freshness projection compares it against
// [insights.FreshnessWindowSeconds].
type CrossRepoRow struct {
	PercentileID  uuid.UUID       `json:"percentile_id"`
	MetricKind    string          `json:"metric_kind"`
	ScopeKind     string          `json:"scope_kind"`
	P50           float64         `json:"p50"`
	P90           float64         `json:"p90"`
	P99           float64         `json:"p99"`
	HistogramJSON json.RawMessage `json:"histogram_json,omitempty"`
	BuiltAt       time.Time       `json:"built_at"`
}

// PortfolioRow is the `mgmt.read.portfolio` projection of
// `clean_code.portfolio_snapshot` (architecture Sec 5.2.6
// lines 1085-1090). Like [CrossRepoRow], `AggregateJSON` is
// echoed verbatim (no on-the-fly aggregate). `BuiltAt` feeds
// the freshness projection.
type PortfolioRow struct {
	PortfolioSnapshotID uuid.UUID       `json:"portfolio_snapshot_id"`
	MetricKind          string          `json:"metric_kind"`
	ScopeKind           string          `json:"scope_kind"`
	RepoCount           int             `json:"repo_count"`
	AggregateJSON       json.RawMessage `json:"aggregate_json,omitempty"`
	BuiltAt             time.Time       `json:"built_at"`
}

// -----------------------------------------------------------
// Response envelopes -- each verb returns a typed envelope
// carrying the canonical [ReadMode] tag.
// -----------------------------------------------------------

// RepoResponse is the `mgmt.read.repo` envelope.
type RepoResponse struct {
	Mode ReadMode `json:"mode"`
	Repo *RepoRow `json:"repo"`
}

// MetricSampleResponse is the `mgmt.read.metric_sample`
// envelope. `Sample` is the active row at the requested
// quintuple; nil when [ErrNotFound] would otherwise fire (the
// method returns `(nil, ErrNotFound)` in that case so the
// envelope's `Sample` is always non-nil on the success path).
type MetricSampleResponse struct {
	Mode   ReadMode         `json:"mode"`
	Sample *MetricSampleRow `json:"sample"`
}

// MetricSamplesResponse is the `mgmt.read.metric_samples`
// envelope. An empty `Samples` slice is a valid success
// (no samples at the filter).
type MetricSamplesResponse struct {
	Mode    ReadMode          `json:"mode"`
	Samples []MetricSampleRow `json:"samples"`
}

// FindingsResponse is the `mgmt.read.findings` /
// `mgmt.read.regressions` envelope.
type FindingsResponse struct {
	Mode     ReadMode     `json:"mode"`
	Findings []FindingRow `json:"findings"`
}

// RefactorPlanResponse is the `mgmt.read.refactor_plan`
// envelope.
type RefactorPlanResponse struct {
	Mode ReadMode         `json:"mode"`
	Plan *RefactorPlanRow `json:"plan"`
}

// CrossRepoResponse is the `mgmt.read.cross_repo` envelope.
// `Degraded`, `DegradedReason`, and `BuiltAt` are populated
// by the [insights.Freshness] projection running on the
// snapshot's `built_at`. `DegradedReason` is empty (and
// omitted from JSON) when `Degraded=false`.
type CrossRepoResponse struct {
	Mode           ReadMode      `json:"mode"`
	Row            *CrossRepoRow `json:"row"`
	Degraded       bool          `json:"degraded"`
	DegradedReason string        `json:"degraded_reason,omitempty"`
	BuiltAt        time.Time     `json:"built_at"`
	Window         time.Duration `json:"window"`
}

// PortfolioResponse is the `mgmt.read.portfolio` envelope.
// The verb returns one [PortfolioRow] per (metric_kind,
// scope_kind) pair -- the brief's `mgmt.read.portfolio(metric_kind)`
// signature returns every scope_kind for the requested
// metric_kind. The freshness verdict is the WORST-CASE across
// rows: `Degraded=true` iff ANY row's `built_at` is stale.
type PortfolioResponse struct {
	Mode           ReadMode       `json:"mode"`
	Rows           []PortfolioRow `json:"rows"`
	Degraded       bool           `json:"degraded"`
	DegradedReason string         `json:"degraded_reason,omitempty"`
	OldestBuiltAt  time.Time      `json:"oldest_built_at"`
	Window         time.Duration  `json:"window"`
}

// AgedMutesResponse is the `mgmt.read.insights.aged_mutes`
// envelope. Wraps the [insights.AgedMutes] projection's slice
// in the canonical [ReadMode] tag so the HTTP layer's wire
// shape stays uniform with the other latest-dashboard verbs.
//
// `ThresholdDays` echoes the threshold the report was computed
// under (default 90, or the caller-supplied override). A
// dashboard renders this in the column header so an operator
// who passed an explicit `threshold_days` argument can confirm
// the cutoff -- WITHOUT the echo a stale browser tab caching
// the URL bar's argument could silently render rows under a
// different threshold than the one the operator typed.
type AgedMutesResponse struct {
	Mode          ReadMode            `json:"mode"`
	AgedMutes     []insights.AgedMute `json:"aged_mutes"`
	ThresholdDays int                 `json:"threshold_days"`
}

// -----------------------------------------------------------
// MetricsBackend -- the single read-side data-source seam.
// -----------------------------------------------------------

// MetricsBackend is the read-only data source the [Reader]
// delegates every read verb to. One method per verb -- a
// production PG-backed implementation lands in a downstream
// stage; tests inject a pure-Go fake.
//
// # Backend contracts
//
//   - All eight methods are CONCURRENCY-SAFE; the Reader
//     shares one backend instance across requests.
//
//   - SHA-pinned methods MUST honour the exact requested
//     identity. They MUST resolve the active row through
//     `metric_sample_active` (Phase 1.3 side relation) AND
//     filter out retracted samples (rows whose `sample_id`
//     appears in `metric_retraction` per architecture
//     Sec 5.2.2 lines 1035-1037).
//
//   - SHA-pinned methods that return a single row MUST
//     return [ErrNotFound] when no row matches. The Reader
//     surfaces this as-is to the caller.
//
//   - Latest-dashboard methods MUST return the snapshot row
//     verbatim (no on-the-fly recompute). The freshness
//     check is the Reader's responsibility -- the backend
//     simply returns the row + its `built_at`.
//
//   - Latest-dashboard methods MAY return an empty slice
//     (portfolio) or [ErrNotFound] (cross_repo) when the
//     aggregator has not yet populated the table.
type MetricsBackend interface {
	// ReadRepo returns the `clean_code.repo` row whose
	// `repo_id` matches. Returns [ErrNotFound] when no such
	// row exists.
	ReadRepo(ctx context.Context, repoID uuid.UUID) (*RepoRow, error)

	// ReadMetricSample returns the ACTIVE row at the
	// quintuple `(repoID, sha, scopeID, metricKind)` -- the
	// implementation joins `metric_sample_active` to
	// `metric_sample` and filters retracted samples. The
	// `metric_version` is resolved to the latest active
	// version at this quintuple.
	ReadMetricSample(ctx context.Context, repoID uuid.UUID, sha string, scopeID uuid.UUID, metricKind string) (*MetricSampleRow, error)

	// ReadMetricSamples returns every active sample at
	// `(repoID, sha)` matching the optional `filter`. Empty
	// result is a valid success.
	ReadMetricSamples(ctx context.Context, repoID uuid.UUID, sha string, filter MetricSamplesFilter) ([]MetricSampleRow, error)

	// ReadFindings returns every `Finding` row at
	// `(repoID, sha)`, regardless of `delta`.
	ReadFindings(ctx context.Context, repoID uuid.UUID, sha string) ([]FindingRow, error)

	// ReadRegressions returns ONLY the `Finding` rows at
	// `(repoID, sha)` whose `delta='newly_failing'` --
	// the canonical "regressions" bucket per architecture
	// Sec 5.4.1 lines 1188-1190.
	ReadRegressions(ctx context.Context, repoID uuid.UUID, sha string) ([]FindingRow, error)

	// ReadRefactorPlan returns the most recent
	// `refactor_plan` at `(repoID, sha)` with its embedded
	// `refactor_task` rows. Returns [ErrNotFound] when no
	// plan has been generated.
	ReadRefactorPlan(ctx context.Context, repoID uuid.UUID, sha string) (*RefactorPlanRow, error)

	// ReadCrossRepo returns the single
	// `cross_repo_percentile` row keyed by
	// `(metric_kind, scope_kind)`. Returns [ErrNotFound]
	// when the aggregator has not yet populated this key.
	ReadCrossRepo(ctx context.Context, metricKind, scopeKind string) (*CrossRepoRow, error)

	// ReadPortfolio returns every `portfolio_snapshot` row
	// matching the given `metric_kind` (one row per
	// scope_kind). Empty result is a valid success when the
	// aggregator has not yet populated this metric_kind.
	ReadPortfolio(ctx context.Context, metricKind string) ([]PortfolioRow, error)
}

// -----------------------------------------------------------
// Reader -- the read-side surface composed from the keys
// manager, the metrics backend, and the freshness projection.
// -----------------------------------------------------------

// Reader is the read-side surface of the clean-code service.
// Stage 5.1 wired the [keys.Manager]; Stage 6.3 adds the
// [MetricsBackend] + [insights.Freshness] composition for the
// eight `mgmt.read.*` verbs.
//
// All Reader methods are safe for concurrent use; concurrency
// guarantees come from the underlying read sources (the
// `keys.Manager` cache is RWMutex-guarded, the
// [MetricsBackend] contract pins concurrency safety).
type Reader struct {
	signingKeys                 *keys.Manager
	metrics                     MetricsBackend
	freshness                   *insights.Freshness
	freshnessExplicitlyDisabled bool
	agedMutes                   *insights.AgedMutes
}

// ReaderOption configures a [Reader] at construction. Used
// with [NewReader]'s variadic options shape so callers can
// add backends without breaking the Stage 5.1 single-arg
// constructor signature.
type ReaderOption func(*Reader)

// WithMetricsBackend wires `b` as the [Reader]'s
// [MetricsBackend]. nil is permitted -- the affected verbs
// then return [ErrBackendUnavailable].
func WithMetricsBackend(b MetricsBackend) ReaderOption {
	return func(r *Reader) { r.metrics = b }
}

// WithInsightsFreshness wires `f` as the [Reader]'s
// [insights.Freshness] projection. nil is permitted -- the
// latest-dashboard verbs then skip the freshness banner
// (they still return the snapshot row).
//
// Composition root callers SHOULD pass
// [insights.NewPercentileFreshness] to honour the tech-spec
// Sec 8.2 `freshness_window_seconds=3600` contract.
func WithInsightsFreshness(f *insights.Freshness) ReaderOption {
	return func(r *Reader) { r.freshness = f }
}

// WithAgedMutes wires `a` as the [Reader]'s [insights.AgedMutes]
// projection -- the read seam behind `mgmt.read.insights.aged_mutes`
// (Stage 10.2). nil is permitted -- the verb then returns
// [ErrBackendUnavailable], mirroring [WithMetricsBackend]'s
// "unwired -> 503" convention.
//
// Composition root callers wire a production [insights.AgedMutes]
// constructed via [insights.NewAgedMutes] (default 90-day
// threshold; pass nil for the clock to default to
// [insights.SystemClock]). A typical wiring is:
//
//	r := management.NewReader(km,
//	    management.WithMetricsBackend(pg),
//	    management.WithAgedMutes(insights.NewAgedMutes(overrideReader, nil)),
//	)
//
// The override-reader adapter (which bridges
// `steward.Store` -> `insights.OverrideReader`) lives in this
// management package (see [OverrideReaderFromStore]) so insights
// stays free of a steward dependency (see the package doc on
// [aged_mutes.go]).
func WithAgedMutes(a *insights.AgedMutes) ReaderOption {
	return func(r *Reader) { r.agedMutes = a }
}

// NewReader constructs a Reader. signingKeys MAY be nil for
// scaffold-mode bring-ups -- in that case `ListActiveSigningKeys`
// returns [ErrManagerUnavailable] and the HTTP handler
// translates it into a 503.
//
// Stage 6.3 adds variadic options so the composition root can
// also wire a [MetricsBackend] and an [insights.Freshness]
// projection without breaking Stage 5.1 callers:
//
//	r := management.NewReader(km,
//	    management.WithMetricsBackend(pgBackend),
//	    management.WithInsightsFreshness(insights.NewPercentileFreshness()),
//	)
//
// # Freshness auto-default (iter 2 evaluator item 5)
//
// When no [WithInsightsFreshness] option is passed (or it
// passes nil), [NewReader] STILL wires a default
// [insights.NewPercentileFreshness]. A latest-dashboard read
// from a composition root that forgot to inject freshness used
// to silently return stale snapshots as non-degraded; now the
// canonical 3600s window kicks in automatically. Callers that
// genuinely want to disable freshness must do so explicitly
// via [WithoutFreshness].
func NewReader(signingKeys *keys.Manager, opts ...ReaderOption) *Reader {
	r := &Reader{signingKeys: signingKeys}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	if r.freshness == nil && !r.freshnessExplicitlyDisabled {
		r.freshness = insights.NewPercentileFreshness()
	}
	return r
}

// WithoutFreshness EXPLICITLY disables the freshness banner on
// latest-dashboard reads. Without this option [NewReader]
// auto-wires [insights.NewPercentileFreshness] (iter 2 evaluator
// item 5) -- callers that genuinely want to bypass the banner
// (e.g. a one-off operator tool that wants the raw snapshot
// regardless of staleness) must opt out explicitly.
func WithoutFreshness() ReaderOption {
	return func(r *Reader) {
		r.freshness = nil
		r.freshnessExplicitlyDisabled = true
	}
}

// ListActiveSigningKeys returns the canonical
// `policy.keys.list_active` projection: every signing key that
// is currently inside its `[valid_from, valid_until)` window,
// sorted newest-first.
//
// Empty result is valid (returns nil, nil) -- callers
// interpret it as "no key has been activated yet" which is
// distinct from "the keys subsystem is mis-wired"
// ([ErrManagerUnavailable]).
func (r *Reader) ListActiveSigningKeys(ctx context.Context) ([]keys.ActiveKeyView, error) {
	if r.signingKeys == nil {
		return nil, ErrManagerUnavailable
	}
	return r.signingKeys.ListActive(ctx)
}

// -----------------------------------------------------------
// SHA-pinned read verbs (six of them).
// -----------------------------------------------------------

// ReadRepo serves `mgmt.read.repo(repo_id)`. The response's
// `Mode` is always [ReadModeSHAPinned] -- catalog reads carry
// no SHA semantics, so the bucket is informational (the
// Stage 6.3 brief lists `mgmt.read.repo` in the SHA-pinned
// group).
func (r *Reader) ReadRepo(ctx context.Context, repoID uuid.UUID) (*RepoResponse, error) {
	if r.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	row, err := r.metrics.ReadRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound
	}
	if row.RepoID != repoID {
		return nil, fmt.Errorf("%w: ReadRepo returned repo_id=%s, want %s", ErrBackendInvariantViolation, row.RepoID, repoID)
	}
	return &RepoResponse{Mode: ReadModeSHAPinned, Repo: row}, nil
}

// ReadMetricSample serves
// `mgmt.read.metric_sample(repo_id, sha, scope_id, metric_kind)`.
// The backend resolves through `metric_sample_active` and
// filters retracted samples; the Reader's post-condition guard
// asserts the returned row carries the exact requested
// quintuple identity.
func (r *Reader) ReadMetricSample(ctx context.Context, repoID uuid.UUID, sha string, scopeID uuid.UUID, metricKind string) (*MetricSampleResponse, error) {
	if r.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	row, err := r.metrics.ReadMetricSample(ctx, repoID, sha, scopeID, metricKind)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound
	}
	if row.RepoID != repoID || row.SHA != sha || row.ScopeID != scopeID || row.MetricKind != metricKind {
		return nil, fmt.Errorf(
			"%w: ReadMetricSample requested (%s, %s, %s, %s) but got (%s, %s, %s, %s)",
			ErrBackendInvariantViolation,
			repoID, sha, scopeID, metricKind,
			row.RepoID, row.SHA, row.ScopeID, row.MetricKind,
		)
	}
	return &MetricSampleResponse{Mode: ReadModeSHAPinned, Sample: row}, nil
}

// ReadMetricSamples serves
// `mgmt.read.metric_samples(repo_id, sha, filter)`. An empty
// result is a valid success. The Reader's per-row guard
// asserts every returned sample carries the exact requested
// (repo_id, sha).
func (r *Reader) ReadMetricSamples(ctx context.Context, repoID uuid.UUID, sha string, filter MetricSamplesFilter) (*MetricSamplesResponse, error) {
	if r.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	rows, err := r.metrics.ReadMetricSamples(ctx, repoID, sha, filter)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].RepoID != repoID || rows[i].SHA != sha {
			return nil, fmt.Errorf(
				"%w: ReadMetricSamples row %d carries (%s, %s) but request was (%s, %s)",
				ErrBackendInvariantViolation, i,
				rows[i].RepoID, rows[i].SHA, repoID, sha,
			)
		}
	}
	if rows == nil {
		rows = []MetricSampleRow{}
	}
	return &MetricSamplesResponse{Mode: ReadModeSHAPinned, Samples: rows}, nil
}

// ReadFindings serves `mgmt.read.findings(repo_id, sha)`. The
// Reader returns ALL findings at (repo_id, sha) regardless of
// `delta`; callers that want only the regressions subset call
// [Reader.ReadRegressions].
func (r *Reader) ReadFindings(ctx context.Context, repoID uuid.UUID, sha string) (*FindingsResponse, error) {
	if r.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	rows, err := r.metrics.ReadFindings(ctx, repoID, sha)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].RepoID != repoID || rows[i].SHA != sha {
			return nil, fmt.Errorf(
				"%w: ReadFindings row %d carries (%s, %s) but request was (%s, %s)",
				ErrBackendInvariantViolation, i,
				rows[i].RepoID, rows[i].SHA, repoID, sha,
			)
		}
	}
	if rows == nil {
		rows = []FindingRow{}
	}
	return &FindingsResponse{Mode: ReadModeSHAPinned, Findings: rows}, nil
}

// ReadRegressions serves `mgmt.read.regressions(repo_id,
// sha)`. The backend MUST filter `delta='newly_failing'`; the
// Reader's per-row guard pins both the SHA-pinned identity
// AND the canonical delta value so a backend bug that
// returned a `delta='resolved'` row would be caught here.
func (r *Reader) ReadRegressions(ctx context.Context, repoID uuid.UUID, sha string) (*FindingsResponse, error) {
	if r.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	rows, err := r.metrics.ReadRegressions(ctx, repoID, sha)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].RepoID != repoID || rows[i].SHA != sha {
			return nil, fmt.Errorf(
				"%w: ReadRegressions row %d carries (%s, %s) but request was (%s, %s)",
				ErrBackendInvariantViolation, i,
				rows[i].RepoID, rows[i].SHA, repoID, sha,
			)
		}
		if rows[i].Delta != "newly_failing" {
			return nil, fmt.Errorf(
				"%w: ReadRegressions row %d carries delta=%q, want %q",
				ErrBackendInvariantViolation, i,
				rows[i].Delta, "newly_failing",
			)
		}
	}
	if rows == nil {
		rows = []FindingRow{}
	}
	return &FindingsResponse{Mode: ReadModeSHAPinned, Findings: rows}, nil
}

// ReadRefactorPlan serves `mgmt.read.refactor_plan(repo_id,
// sha)`. Returns [ErrNotFound] when no plan exists at this
// (repo_id, sha).
func (r *Reader) ReadRefactorPlan(ctx context.Context, repoID uuid.UUID, sha string) (*RefactorPlanResponse, error) {
	if r.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	row, err := r.metrics.ReadRefactorPlan(ctx, repoID, sha)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound
	}
	if row.RepoID != repoID || row.SHA != sha {
		return nil, fmt.Errorf(
			"%w: ReadRefactorPlan returned (repo=%s, sha=%s), want (%s, %s)",
			ErrBackendInvariantViolation,
			row.RepoID, row.SHA, repoID, sha,
		)
	}
	return &RefactorPlanResponse{Mode: ReadModeSHAPinned, Plan: row}, nil
}

// -----------------------------------------------------------
// Latest-dashboard read verbs (two of them).
// -----------------------------------------------------------

// ReadCrossRepo serves `mgmt.read.cross_repo(metric_kind,
// scope_kind)`. Returns the verbatim
// `cross_repo_percentile` row (no on-the-fly recompute,
// impl-plan Stage 6.3 scenario `latest-dashboard-returns-snapshot`)
// PLUS the freshness banner produced by [insights.Freshness]
// running on the row's `built_at` (Stage 7.3 scenarios
// `stale-percentile-banner-on-insights` and
// `fresh-percentile-no-banner`).
//
// When no row exists for the requested key the Reader
// returns [ErrNotFound]; callers receive a clean "snapshot
// not yet populated" signal rather than a degraded envelope.
//
// Iter 2 evaluator item 4: the Reader's per-row guard asserts
// the returned `metric_kind` / `scope_kind` match the request,
// preventing a backend bug from substituting a different
// key's snapshot at the read boundary.
func (r *Reader) ReadCrossRepo(ctx context.Context, metricKind, scopeKind string) (*CrossRepoResponse, error) {
	if r.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	row, err := r.metrics.ReadCrossRepo(ctx, metricKind, scopeKind)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound
	}
	if row.MetricKind != metricKind || row.ScopeKind != scopeKind {
		return nil, fmt.Errorf(
			"%w: ReadCrossRepo requested (%s, %s) but got (%s, %s)",
			ErrBackendInvariantViolation,
			metricKind, scopeKind, row.MetricKind, row.ScopeKind,
		)
	}
	resp := &CrossRepoResponse{
		Mode:    ReadModeLatestDashboard,
		Row:     row,
		BuiltAt: row.BuiltAt,
	}
	if r.freshness != nil {
		status := r.freshness.Evaluate(row.BuiltAt)
		resp.Degraded = status.Degraded
		resp.DegradedReason = status.Reason
		resp.Window = status.Window
	}
	return resp, nil
}

// ReadPortfolio serves `mgmt.read.portfolio(metric_kind)`.
// Returns every snapshot row matching the metric_kind (one
// per scope_kind). Empty result is a valid success when the
// aggregator has not yet populated rows for this metric_kind;
// the response envelope still carries the latest-dashboard
// Mode tag so the HTTP layer's wire shape is uniform.
//
// Freshness verdict semantics: the envelope's `Degraded` is
// true iff ANY row's `built_at` is stale. `OldestBuiltAt`
// echoes the oldest `built_at` so operators can attribute
// the worst-case staleness.
//
// Iter 2 evaluator item 4: every row is checked for matching
// `metric_kind`; a backend that returns a row for a different
// metric_kind is rejected at the Reader boundary.
func (r *Reader) ReadPortfolio(ctx context.Context, metricKind string) (*PortfolioResponse, error) {
	if r.metrics == nil {
		return nil, ErrBackendUnavailable
	}
	rows, err := r.metrics.ReadPortfolio(ctx, metricKind)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []PortfolioRow{}
	}
	for i := range rows {
		if rows[i].MetricKind != metricKind {
			return nil, fmt.Errorf(
				"%w: ReadPortfolio row %d carries metric_kind=%q but request was %q",
				ErrBackendInvariantViolation, i,
				rows[i].MetricKind, metricKind,
			)
		}
	}
	resp := &PortfolioResponse{
		Mode: ReadModeLatestDashboard,
		Rows: rows,
	}
	if len(rows) == 0 {
		return resp, nil
	}
	oldest := rows[0].BuiltAt
	for i := 1; i < len(rows); i++ {
		if rows[i].BuiltAt.Before(oldest) {
			oldest = rows[i].BuiltAt
		}
	}
	resp.OldestBuiltAt = oldest
	if r.freshness != nil {
		status := r.freshness.Evaluate(oldest)
		resp.Degraded = status.Degraded
		resp.DegradedReason = status.Reason
		resp.Window = status.Window
	}
	return resp, nil
}

// ReadAgedMutes serves `mgmt.read.insights.aged_mutes(threshold_days?)`
// (Stage 10.2). Returns every override(mute=true) row whose
// latest-row-wins reduction per (rule_id, scope_filter) is
// older than the supplied `thresholdDays`. The report is
// EXCLUSIVELY a read surface -- no enforcement is performed
// (iter 1 evaluator item 5 + tech-spec Sec 10A "mute lifecycle"
// pin: v1 has NO TTL enforcement in code). Operators unmute
// by appending an `override(mute=false)` row through
// `mgmt.override`; the aged-mute pair drops off the report on
// the next call (the canonical
// `unmute-removes-from-report` scenario).
//
// `thresholdDays`:
//
//   - nil -- use the default 90-day threshold pinned at
//     [insights.AgedMuteDefaultThresholdDays].
//   - non-nil with positive value -- the operator-supplied
//     override (e.g. `?threshold_days=60`).
//   - non-nil with zero or negative value -- treated as the
//     default. Guards against a missing-arg HTTP call
//     accidentally surfacing every mute (zero-threshold ->
//     every mute is aged).
//
// Returns [ErrBackendUnavailable] when [WithAgedMutes] was
// not supplied at composition time -- mirrors the
// "verb mounted, substrate unwired -> 503" convention used by
// the other Reader methods. The HTTP layer maps this to a
// 503 Service Unavailable.
//
// Iter 2 evaluator item 3: this method ALSO maps the two
// "wired but unusable" sentinels to [ErrBackendUnavailable]
// so the operator-facing failure mode is uniform regardless
// of WHERE the composition-root wiring bug is:
//
//   - [insights.ErrAgedMuteReaderUnavailable]: the projection
//     has a nil [insights.OverrideReader] (e.g. the
//     composition root passed `nil` to
//     [insights.NewAgedMutes]).
//   - [ErrAgedMuteOverrideStoreUnavailable]: the production
//     [OverrideReaderFromStore] adapter was constructed with
//     a nil [steward.Store].
//
// Both surface as a 503 so the dashboard renders an
// "unavailable" tile instead of leaking the internal
// scaffold-mode error string to the operator. The mapping is
// pinned by `TestReader_ReadAgedMutes_*Unavailable*` tests in
// `reader_aged_mutes_test.go`.
//
// Returns the underlying [insights.OverrideReader] error
// (verbatim, wrapped to preserve `errors.Is`) when the
// backend scan fails. Callers SHOULD treat any error other
// than [ErrBackendUnavailable] as a backend infrastructure
// fault and surface a 500 -- the report is a best-effort
// dashboard read; failing closed is preferable to silently
// rendering an empty report under a backend outage.
func (r *Reader) ReadAgedMutes(ctx context.Context, thresholdDays *int) (*AgedMutesResponse, error) {
	if r.agedMutes == nil {
		return nil, ErrBackendUnavailable
	}
	effectiveDays := insights.AgedMuteDefaultThresholdDays
	if thresholdDays != nil && *thresholdDays > 0 {
		effectiveDays = *thresholdDays
	}
	report, err := r.agedMutes.ReportWithThreshold(
		ctx,
		time.Duration(effectiveDays)*24*time.Hour,
	)
	if err != nil {
		if errors.Is(err, insights.ErrAgedMuteReaderUnavailable) ||
			errors.Is(err, ErrAgedMuteOverrideStoreUnavailable) {
			return nil, ErrBackendUnavailable
		}
		return nil, err
	}
	if report == nil {
		// Defensive: the [insights.AgedMutes] contract
		// returns a non-nil empty slice, but a future
		// reader implementation might miss it. JSON `[]`
		// (not `null`) is the wire shape this verb pins.
		report = []insights.AgedMute{}
	}
	return &AgedMutesResponse{
		Mode:          ReadModeLatestDashboard,
		AgedMutes:     report,
		ThresholdDays: effectiveDays,
	}, nil
}
