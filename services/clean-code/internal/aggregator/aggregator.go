package aggregator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/gofrs/uuid"
)

// Aggregator is the cadence-driven worker that materialises the
// three Measurement sub-store derived views (architecture Sec
// 3.10 / Sec 5.2.4 -- Sec 5.2.6).
//
// Construction is via [NewAggregator]; the cadence loop wraps the
// aggregator in [Loop] which calls [Aggregator.Tick] every
// [config.DefaultAggregatorCadence] (15 min).
//
// # Single source of truth (G6)
//
// Tick reads `source.ReadActive`, computes the derived rows in
// process, and writes via `writer.WriteSnapshots`. The aggregator
// holds NO state between ticks -- every snapshot is recomputable
// from `metric_sample` + `metric_sample_active` + `metric_retraction`
// at any time. Restarting the aggregator loses zero correctness.
//
// # Concurrency
//
// [Tick] is safe for concurrent invocation -- it allocates fresh
// working buffers and never touches Aggregator-level state. In
// production exactly one [Loop] drives one [Aggregator]; the
// concurrent-safety property exists so tests can drive multiple
// ticks in parallel against the same Aggregator without
// surprising shared-state behaviour.
type Aggregator struct {
	source SampleSource
	writer SnapshotWriter
	now    func() time.Time

	// System-tier pipeline (Stage 7.2 wiring). When all three
	// are non-nil the aggregator runs a SECOND pass per tick
	// reading [SystemTierInputSource], invoking
	// [SystemTierComposer.Compose] per repo+SHA, and
	// persisting the emitted samples through
	// [SystemTierWriter]. The fields default to nil so the
	// existing two-arg constructor + foundation-only tests
	// keep their behaviour; wiring is opt-in via
	// [WithSystemTier].
	composer       *SystemTierComposer
	sysSource      SystemTierInputSource
	sysWriter      SystemTierWriter

	// linkedReader is the optional Stage 8.7 / Stage 10.1
	// linked-mode adapter hook. When non-nil and the
	// system-tier pass is wired, [tickSystemTier] consults
	// the reader per [SystemTierInput] to optionally overlay
	// cross-repo + call-graph edges fetched from the
	// agent-memory linked-mode endpoint. Default nil keeps
	// the embedded-only pipeline unchanged so existing
	// foundation-only deployments are byte-identical to the
	// pre-Stage-10.1 behaviour.
	//
	// Errors from the reader are split TWO WAYS by
	// [tickSystemTier]:
	//   - context cancellation / deadline -> abort tick
	//   - any other error -> log + leave the affected input
	//     in embedded shape so the composer naturally
	//     degrades the row with `xrepo_edges_unavailable`
	//     (architecture Sec 3.10 step 4 fail-safe contract).
	linkedReader LinkedEdgeReader
	linkedLogger *slog.Logger
}

// LinkedEdgeReader is the optional seam by which the
// aggregator resolves cross-repo and call-graph edges per
// `(repo_id, sha)` for the system-tier composer. Production
// callers wire the `internal/linked` package's
// `AggregatorAdapter` here; tests substitute fakes via the
// in-package helpers (see `system_tier_linked_test.go`).
//
// # Two-axis gating
//
// The reader is the SINGLE place where the linked-mode
// gating logic lives: it is responsible for honouring BOTH
//   1. the global `EnableLinkedModeAdapter` config flag and
//   2. the per-repo `repo.mode = 'linked'` setting (flipped
//      via `mgmt.set_mode`).
//
// When EITHER gate is closed the reader returns
// `{Applicable: false}` and the aggregator leaves the input
// in its embedded shape (composer degrades the row). The
// aggregator NEVER second-guesses the reader; the two-axis
// logic is intentionally NOT duplicated here.
//
// # Failure modes
//
// Implementations distinguish THREE error classes via
// `errors.Is` sentinels:
//
//  1. CONTEXT-CANCEL / DEADLINE-EXCEEDED (`context.Canceled`,
//     `context.DeadlineExceeded`) -- the aggregator's
//     [Aggregator.applyLinkedEdges] propagates these so the
//     outer tick honours operator-requested cancellation
//     regardless of linked-mode wiring.
//
//  2. MODE-STORE FAILURE -- the per-repo `ReadRepoMode` call
//     into `internal/management` failed (PG outage, role
//     misconfig, migration drift). Implementations MUST wrap
//     the underlying error with [ErrLinkedModeStore] so the
//     aggregator can [errors.Is] check it and treat it as
//     FATAL (abort tick). `Report.LinkedEdgeFetchFailures`
//     is NOT incremented on this path -- that counter is
//     reserved for class (3) so the operator signal
//     unambiguously points at agent-memory uptime.
//
//  3. REMOTE / agent-memory failure -- the upstream
//     agent-memory `/v1/cross-repo/edges` call failed
//     (network error, 5xx, 404, malformed JSON). The
//     aggregator LOGS + leaves the input in embedded shape
//     so the composer degrades the row with
//     `xrepo_edges_unavailable`. `Report.LinkedEdgeFetchFailures`
//     IS incremented.
//
// CLASSIFICATION ORDER MATTERS: a cancelled tick can present
// as any of the three classes (a transport may wrap a ctx
// error inside a domain-specific error type), so
// [Aggregator.applyLinkedEdges] checks ctx errors FIRST, then
// [ErrLinkedModeStore], then falls through to the
// remote-degrade path.
type LinkedEdgeReader interface {
	ResolveLinkedEdges(ctx context.Context, repoID uuid.UUID, sha string) (LinkedEdges, error)
}

// LinkedEdges is the value the [LinkedEdgeReader] returns.
// Carries per-EDGE-FAMILY availability flags so the composer
// can degrade `xrepo_dep_depth` and `blast_radius`
// independently (e.g. when agent-memory has indexed cross-
// repo deps but the call graph is still building). An
// `Applicable=false` value is a no-op; the aggregator leaves
// the input unchanged.
type LinkedEdges struct {
	// Applicable is true when the reader successfully
	// resolved edges (or "explicitly empty" edges) for this
	// repo. False when either gating axis is closed (global
	// flag off OR repo mode != linked).
	Applicable bool
	// XRepoEdges populated when Applicable AND the reader
	// fetched cross-repo edges. May be empty.
	XRepoEdges []XRepoEdge
	// XRepoEdgesAvailable signals agent-memory reported a
	// successful cross-repo index for the pair. The
	// aggregator sets `SystemTierInput.XRepoEdgesAvailable`
	// from this flag VERBATIM; the composer reads the
	// SystemTierInput flag to decide whether to degrade.
	XRepoEdgesAvailable bool
	// CallEdges populated when Applicable AND the reader
	// fetched call-graph edges.
	CallEdges []CallEdge
	// CallEdgesAvailable signals agent-memory reported a
	// successful call-graph index for the pair.
	CallEdgesAvailable bool
}

// AggregatorOption configures an [Aggregator].
type AggregatorOption func(*Aggregator)

// WithClock overrides the wall-clock function used to stamp
// `built_at`. Defaults to [time.Now] in production; tests inject
// a deterministic clock so the captured snapshot rows have a
// known timestamp.
func WithClock(now func() time.Time) AggregatorOption {
	return func(a *Aggregator) { a.now = now }
}

// WithSystemTier wires the Stage 7.2 system-tier composer
// pipeline into the aggregator. When this option is applied
// every [Tick] call ALSO:
//
//  1. Reads per-`(repo_id, sha)` inputs from `source`.
//  2. Invokes `composer.Compose` per input.
//  3. Persists the emitted samples through `writer` as
//     `metric_sample(pack='system', source='derived')` rows.
//
// All three arguments MUST be non-nil; passing nil for any of
// them is a wiring bug -- the option panics at startup rather
// than at first tick so the misconfiguration surfaces in the
// composition-root unit test.
//
// # Run-on-empty-foundation semantics
//
// The system-tier pipeline runs INDEPENDENTLY of the
// foundation-snapshot pipeline. Even when the foundation
// `ReadActive` call returns zero observations (e.g. a brand-new
// deployment with no foundation samples yet), the system-tier
// pass still executes -- the [SystemTierInputSource] is the
// canonical reporter of which repo+SHA pairs need a system-tier
// row this tick, and the composer's fail-safe contract
// (architecture Sec 3.10 step 4 lines 637-657) REQUIRES a row
// per input even when every input is missing -- it just emits a
// degraded row carrying the reason. Coupling the system-tier
// pass to foundation observation count would silently drop
// rows the architecture explicitly mandates.
func WithSystemTier(composer *SystemTierComposer, source SystemTierInputSource, writer SystemTierWriter) AggregatorOption {
	if composer == nil {
		panic("aggregator: WithSystemTier: composer is nil")
	}
	if source == nil {
		panic("aggregator: WithSystemTier: source is nil")
	}
	if writer == nil {
		panic("aggregator: WithSystemTier: writer is nil")
	}
	return func(a *Aggregator) {
		a.composer = composer
		a.sysSource = source
		a.sysWriter = writer
	}
}

// WithLinkedEdgeReader wires the optional Stage 10.1
// linked-mode adapter into the aggregator. When set, every
// system-tier tick consults the reader per [SystemTierInput]
// to optionally overlay cross-repo + call-graph edges fetched
// from the agent-memory linked-mode endpoint (architecture
// Sec 8.7).
//
// The reader is the single owner of the two-axis gating logic
// (global `EnableLinkedModeAdapter` config flag AND per-repo
// `repo.mode = 'linked'`); the aggregator NEVER second-
// guesses the reader's `Applicable` verdict.
//
// Panics at startup when `reader == nil` so a wiring bug
// surfaces in the composition-root unit test rather than at
// first tick.
//
// `logger` is optional; when nil, remote agent-memory failures
// are still degraded in place but no log line fires for the
// per-input failure path (operators relying on log-based alerts
// SHOULD pass a non-nil logger).
func WithLinkedEdgeReader(reader LinkedEdgeReader, logger *slog.Logger) AggregatorOption {
	if reader == nil {
		panic("aggregator: WithLinkedEdgeReader: reader is nil")
	}
	return func(a *Aggregator) {
		a.linkedReader = reader
		a.linkedLogger = logger
	}
}

// ErrAggregatorNilSource surfaces a nil [SampleSource] at
// composition-root wiring time.
var ErrAggregatorNilSource = errors.New("aggregator: NewAggregator: source is nil")

// ErrAggregatorNilWriter surfaces a nil [SnapshotWriter] at
// composition-root wiring time.
var ErrAggregatorNilWriter = errors.New("aggregator: NewAggregator: writer is nil")

// ErrLinkedModeStore is the sentinel that [LinkedEdgeReader]
// implementations MUST wrap when the per-repo mode catalog
// (the `internal/management` store reached via
// [management.RepoModeReader.ReadRepoMode]) fails to answer.
//
// Why a sentinel and not a typed error: the aggregator's
// [Aggregator.applyLinkedEdges] needs to distinguish the
// THREE error classes returned by a [LinkedEdgeReader]:
//
//  1. context cancel / deadline  -> abort the tick
//  2. mode-store read failure    -> abort the tick (FATAL);
//     `LinkedEdgeFetchFailures` is NOT incremented because
//     that counter is reserved for agent-memory remote
//     faults and operators must see a clean signal pointing
//     at the management plane (PG mgmt DB, role wiring,
//     migration drift) rather than at agent-memory uptime
//  3. any other error            -> log + leave the input in
//     embedded shape so the downstream composer degrades the
//     row with `xrepo_edges_unavailable`. The
//     `LinkedEdgeFetchFailures` counter IS incremented here
//     so the operator can correlate degradation rate with
//     agent-memory uptime via the Prometheus exporter.
//
// The sentinel lives in this package (NOT in `internal/linked/`)
// because `internal/linked/` already imports
// `internal/aggregator` to satisfy the [LinkedEdgeReader]
// interface; placing the sentinel in `linked` would force a
// cycle. Hosting it here keeps the dependency edge one-way and
// lets EXTERNAL implementations of [LinkedEdgeReader] reuse the
// same classification contract.
var ErrLinkedModeStore = errors.New("aggregator: linked mode store read failed")

// SystemTierWired reports whether the Stage 7.2 system-tier
// pipeline (composer + source + writer) is installed on this
// aggregator. Returns true iff all three system-tier
// dependencies are non-nil; [WithSystemTier] is all-or-nothing
// so this is the canonical observable seam for "is the
// system-tier pass enabled?".
//
// Exposed publicly (not via export_test.go) for two reasons:
//
//  1. Composition-root unit tests in sibling packages (e.g.
//     `cmd/clean-code-aggregator/main_test.go`) need to assert
//     that `buildAggregatorLoop` actually applied
//     [WithSystemTier] -- a non-nil [Loop] alone is insufficient
//     evidence because [NewAggregator] succeeds with no
//     system-tier option and `WithSystemTier` is the second
//     opt-arg that a refactor could silently drop. The iter-5
//     evaluator flagged this exact weakness (item #1).
//  2. Operational `/healthz`-extended surfaces or Prometheus
//     exporters can probe this so a deployment that lost its
//     system-tier wiring (e.g. via a partial config rollback)
//     surfaces as a visible health degradation rather than
//     as silent missing system-tier rows.
//
// This is an O(1) field-tuple check; it touches no I/O and
// makes no allocations. Safe to call from any goroutine.
func (a *Aggregator) SystemTierWired() bool {
	return a != nil && a.composer != nil && a.sysSource != nil && a.sysWriter != nil
}

// NewAggregator constructs an aggregator. Returns an error when
// either dependency is nil so the wiring bug surfaces at startup
// rather than at first tick.
func NewAggregator(source SampleSource, writer SnapshotWriter, opts ...AggregatorOption) (*Aggregator, error) {
	if source == nil {
		return nil, ErrAggregatorNilSource
	}
	if writer == nil {
		return nil, ErrAggregatorNilWriter
	}
	a := &Aggregator{
		source: source,
		writer: writer,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

// repoCohortKey groups observations by `(repo_id, metric_kind,
// scope_kind)` for the per-repo snapshot.
type repoCohortKey struct {
	repoID     uuid.UUID
	metricKind string
	scopeKind  string
}

// cohortKey groups observations by `(metric_kind, scope_kind)`
// for the cross-repo and portfolio rows.
type cohortKey struct {
	metricKind string
	scopeKind  string
}

// Tick executes one aggregation pass:
//
//  1. Captures `built_at` from the injected clock (single value
//     shared by every row written this tick).
//  2. Runs the foundation-snapshot pass via [tickSnapshots]:
//     reads the active observation set, computes per-repo +
//     cross-repo + portfolio rows, and writes them.
//  3. Runs the system-tier composer pass via [tickSystemTier]
//     when wired (independent of foundation observations -- see
//     [WithSystemTier]).
//
// Both passes execute in the SAME tick under the SAME BuiltAt
// so downstream consumers (Insights, eval.gate) see a coherent
// view across foundation snapshots + system-tier rows.
//
// Returns a [Report] summarising the tick. On read or write
// failure in either pass, returns the underlying error; the
// Report value is populated with whatever counters were
// captured up to the failure point.
//
// # Failure-handling order
//
// The snapshot pass runs first. When it fails, the system-tier
// pass DOES NOT run -- the tick is rolled back in spirit (the
// PG writer's transaction is the actual rollback boundary; the
// in-memory writer drops the batch on its own error path).
// When the snapshot pass succeeds and the system-tier pass
// fails, the snapshot rows ARE persisted and the error
// propagates with the system-tier failure context attached so
// the operator can correlate via the Report counters.
func (a *Aggregator) Tick(ctx context.Context) (Report, error) {
	report := Report{BuiltAt: a.now().UTC()}

	if err := a.tickSnapshots(ctx, &report); err != nil {
		return report, err
	}

	if a.composer != nil && a.sysSource != nil && a.sysWriter != nil {
		if err := a.tickSystemTier(ctx, &report); err != nil {
			return report, err
		}
	}
	return report, nil
}

// tickSnapshots is the foundation-snapshot pass extracted from
// the prior monolithic Tick. It reads the active observation
// set, computes per-repo + cross-repo + portfolio rows, and
// writes them via [SnapshotWriter.WriteSnapshots]. Populates
// the Report's snapshot counters in place.
func (a *Aggregator) tickSnapshots(ctx context.Context, report *Report) error {
	obs, err := a.source.ReadActive(ctx)
	if err != nil {
		return fmt.Errorf("aggregator: read active samples: %w", err)
	}
	report.ObservationsRead = len(obs)

	if len(obs) == 0 {
		// No active samples at all -- nothing to snapshot. Emit
		// an empty WriteSnapshots so the writer can record the
		// "fresh built_at, zero rows" tick (a degenerate but
		// legitimate G6 state for a brand-new deployment).
		if err := a.writer.WriteSnapshots(ctx, Snapshots{}); err != nil {
			return fmt.Errorf("aggregator: write snapshots (empty tick): %w", err)
		}
		return nil
	}

	// Step 1: bucket observations by (repo_id, metric_kind, scope_kind).
	repoCohorts := make(map[repoCohortKey][]float64)
	for _, o := range obs {
		k := repoCohortKey{repoID: o.RepoID, metricKind: o.MetricKind, scopeKind: o.ScopeKind}
		repoCohorts[k] = append(repoCohorts[k], o.Value)
	}

	// Step 2: per-repo summaries -> RepoMetricSnapshotRow set
	// and (metric_kind, scope_kind)-indexed per-repo summary
	// table for the cross-repo / portfolio step. Also collect
	// the FLAT observation-value set per cohort so cross-repo
	// percentiles compute over every contributing sample (not
	// just per-repo means) -- architecture Sec 3.10 line 644.
	type perRepoSummary struct {
		repoID  uuid.UUID
		summary summary
	}
	perCohort := make(map[cohortKey][]perRepoSummary)
	cohortValues := make(map[cohortKey][]float64)
	repoRows := make([]RepoMetricSnapshotRow, 0, len(repoCohorts))
	for k, values := range repoCohorts {
		ck := cohortKey{metricKind: k.metricKind, scopeKind: k.scopeKind}
		// Capture the pristine values BEFORE `summarise` sorts
		// them in-place; `append(dst, src...)` copies each
		// element so the cohort slice is independent of any
		// later mutation to `values`.
		cohortValues[ck] = append(cohortValues[ck], values...)
		s := summarise(values)
		repoRows = append(repoRows, RepoMetricSnapshotRow{
			RepoID:     k.repoID,
			MetricKind: k.metricKind,
			ScopeKind:  k.scopeKind,
			Count:      s.count,
			Mean:       s.mean,
			P50:        s.p50,
			P90:        s.p90,
			P99:        s.p99,
			BuiltAt:    report.BuiltAt,
		})
		perCohort[ck] = append(perCohort[ck], perRepoSummary{repoID: k.repoID, summary: s})
	}

	// Deterministic ordering of repo rows for tests + readability.
	sort.Slice(repoRows, func(i, j int) bool {
		if repoRows[i].MetricKind != repoRows[j].MetricKind {
			return repoRows[i].MetricKind < repoRows[j].MetricKind
		}
		if repoRows[i].ScopeKind != repoRows[j].ScopeKind {
			return repoRows[i].ScopeKind < repoRows[j].ScopeKind
		}
		return repoRows[i].RepoID.String() < repoRows[j].RepoID.String()
	})

	// Step 3: per-cohort cross-repo percentile + portfolio rows.
	crossRows := make([]CrossRepoPercentileRow, 0, len(perCohort))
	portfolioRows := make([]PortfolioSnapshotRow, 0, len(perCohort))
	for ck, entries := range perCohort {
		// Sort entries by repo_id for stable histogram bytes
		// (G6 determinism: identical inputs -> identical
		// histogram_json bytes).
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].repoID.String() < entries[j].repoID.String()
		})

		// Per-repo entries feed the histogram_json + portfolio
		// per-repo block; cross-repo p50/p90/p99 are computed
		// over the FULL flat observation set (cohortValues[ck])
		// per architecture Sec 3.10 line 644.
		var totalObs int64
		var totalValueSum float64
		var unweightedMeanAcc float64
		histEntries := make([]HistogramEntry, len(entries))
		portfolioEntries := make([]PortfolioPerRepoEntry, len(entries))
		for i, e := range entries {
			totalObs += e.summary.count
			totalValueSum += e.summary.mean * float64(e.summary.count)
			unweightedMeanAcc += e.summary.mean
			histEntries[i] = HistogramEntry{
				RepoID: e.repoID.String(),
				Count:  e.summary.count,
				Mean:   e.summary.mean,
				P50:    e.summary.p50,
				P90:    e.summary.p90,
				P99:    e.summary.p99,
			}
			portfolioEntries[i] = PortfolioPerRepoEntry{
				RepoID: e.repoID.String(),
				Count:  e.summary.count,
				Mean:   e.summary.mean,
			}
		}

		crossSummary := summarise(cohortValues[ck])
		var weighted float64
		if totalObs > 0 {
			weighted = totalValueSum / float64(totalObs)
		}
		var unweighted float64
		if len(entries) > 0 {
			unweighted = unweightedMeanAcc / float64(len(entries))
		}

		// Serialise histogram_json with the envelope shape.
		histBytes, err := json.Marshal(HistogramEnvelope{Entries: histEntries})
		if err != nil {
			return fmt.Errorf("aggregator: marshal histogram_json (metric_kind=%s, scope_kind=%s): %w", ck.metricKind, ck.scopeKind, err)
		}
		crossRows = append(crossRows, CrossRepoPercentileRow{
			MetricKind:    ck.metricKind,
			ScopeKind:     ck.scopeKind,
			P50:           crossSummary.p50,
			P90:           crossSummary.p90,
			P99:           crossSummary.p99,
			HistogramJSON: histBytes,
			BuiltAt:       report.BuiltAt,
		})

		aggregateBytes, err := json.Marshal(PortfolioAggregate{
			TotalObservations: totalObs,
			RepoCount:         len(entries),
			WeightedMean:      weighted,
			UnweightedMean:    unweighted,
			P50:               crossSummary.p50,
			P90:               crossSummary.p90,
			P99:               crossSummary.p99,
			PerRepo:           portfolioEntries,
		})
		if err != nil {
			return fmt.Errorf("aggregator: marshal aggregate_json (metric_kind=%s, scope_kind=%s): %w", ck.metricKind, ck.scopeKind, err)
		}
		portfolioRows = append(portfolioRows, PortfolioSnapshotRow{
			MetricKind:    ck.metricKind,
			ScopeKind:     ck.scopeKind,
			RepoCount:     len(entries),
			AggregateJSON: aggregateBytes,
			BuiltAt:       report.BuiltAt,
		})
	}

	// Deterministic ordering of cross-repo + portfolio rows.
	sort.Slice(crossRows, func(i, j int) bool {
		if crossRows[i].MetricKind != crossRows[j].MetricKind {
			return crossRows[i].MetricKind < crossRows[j].MetricKind
		}
		return crossRows[i].ScopeKind < crossRows[j].ScopeKind
	})
	sort.Slice(portfolioRows, func(i, j int) bool {
		if portfolioRows[i].MetricKind != portfolioRows[j].MetricKind {
			return portfolioRows[i].MetricKind < portfolioRows[j].MetricKind
		}
		return portfolioRows[i].ScopeKind < portfolioRows[j].ScopeKind
	})

	report.CohortsAggregated = len(perCohort)
	report.RepoMetricSnapshotRowsWritten = len(repoRows)
	report.CrossRepoPercentileRowsWritten = len(crossRows)
	report.PortfolioSnapshotRowsWritten = len(portfolioRows)

	snap := Snapshots{
		RepoMetric:       repoRows,
		CrossRepoPercent: crossRows,
		Portfolio:        portfolioRows,
	}
	if err := a.writer.WriteSnapshots(ctx, snap); err != nil {
		return fmt.Errorf("aggregator: write snapshots: %w", err)
	}
	return nil
}

// tickSystemTier is the Stage 7.2 system-tier pass. It runs
// when the aggregator was constructed with [WithSystemTier].
// Reads per-`(repo_id, sha)` inputs from the wired source,
// invokes the composer per input, and persists ALL emitted
// samples through the wired writer as ONE batch (so a partial
// failure does not leave the active pointer in a torn state).
//
// # Counters
//
// Populates the Report's three system-tier counters
// in place: SystemTierReposComposed (one per input), and
// SystemTierSamplesWritten / SystemTierDegradedSamples
// (totals across all inputs).
//
// # Empty input set
//
// When the source returns zero inputs (e.g. a fresh deployment
// before any foundation rows have been ingested), the pass
// records the zero counters and returns nil -- the writer is
// NOT called with an empty slice (no transaction overhead).
func (a *Aggregator) tickSystemTier(ctx context.Context, report *Report) error {
	inputs, err := a.sysSource.ReadSystemTierInputs(ctx)
	if err != nil {
		return fmt.Errorf("aggregator: read system-tier inputs: %w", err)
	}
	if len(inputs) == 0 {
		return nil
	}

	// Aggregate every input's samples into ONE writer batch so
	// the PG writer's single transaction covers the whole tick
	// (matches the snapshot pass's single-WriteSnapshots
	// contract). Pre-size to the typical case (~10 samples per
	// repo -- the seven canonical kinds plus per-scope blast
	// radius / fan-in expansion bound).
	allSamples := make([]SystemTierSample, 0, len(inputs)*10)
	for i := range inputs {
		if err := a.applyLinkedEdges(ctx, &inputs[i], report); err != nil {
			return err
		}
		out, err := a.composer.Compose(ctx, inputs[i])
		if err != nil {
			return fmt.Errorf("aggregator: compose system-tier (repo_id=%s, sha=%s): %w", inputs[i].RepoID, inputs[i].SHA, err)
		}
		allSamples = append(allSamples, out...)
	}
	report.SystemTierReposComposed = len(inputs)
	report.SystemTierSamplesWritten = len(allSamples)
	for i := range allSamples {
		if allSamples[i].Degraded {
			report.SystemTierDegradedSamples++
		}
	}

	if len(allSamples) == 0 {
		return nil
	}
	if err := a.sysWriter.WriteSystemTierSamples(ctx, allSamples); err != nil {
		return fmt.Errorf("aggregator: write system-tier samples: %w", err)
	}
	return nil
}

// applyLinkedEdges consults the optional [LinkedEdgeReader]
// for the supplied input and overlays the resolved edges +
// availability flags + Mode in place. Updates the Report's
// linked-mode counters in place.
//
// Error split (architecture Sec 3.10 step 4 fail-safe). The
// classification ORDER below matters; see [LinkedEdgeReader]
// godoc for the full rationale.
//
//  1. context.Canceled / context.DeadlineExceeded -> aborts
//     the tick by returning the error verbatim. The
//     aggregator loop honours operator-requested cancellation
//     regardless of the linked-mode wiring. Checked FIRST
//     because a transport may wrap a ctx error inside a
//     domain-specific error type.
//
//  2. errors.Is(err, ErrLinkedModeStore) -> FATAL. The
//     management plane's repo-mode catalog failed (PG outage,
//     role misconfig, migration drift). Aborts the tick by
//     returning the wrapped error. [Report.LinkedEdgeFetchFailures]
//     is NOT incremented on this path -- that counter is
//     reserved for agent-memory remote faults so the
//     operator's degradation-rate signal stays clean.
//
//  3. any other error -> LOGGED and SWALLOWED: the input is
//     left in its embedded shape (Mode=embedded,
//     XRepoEdgesAvailable=false, CallEdgesAvailable=false) so
//     the downstream composer naturally degrades the row with
//     `xrepo_edges_unavailable`. The Report counter
//     [Report.LinkedEdgeFetchFailures] is incremented so
//     operators can correlate degradation rate with
//     agent-memory uptime via the Prometheus exporter.
//
// When the reader is not wired this is a no-op.
func (a *Aggregator) applyLinkedEdges(ctx context.Context, in *SystemTierInput, report *Report) error {
	if a.linkedReader == nil {
		return nil
	}
	report.LinkedEdgeReaderInvocations++
	edges, err := a.linkedReader.ResolveLinkedEdges(ctx, in.RepoID, in.SHA)
	if err != nil {
		// (1) Honour caller cancellation / deadline exceeded
		// even when the linked reader wraps them; both
		// errors.Is and the live ctx.Err() are consulted so
		// a transport that loses the sentinel cannot
		// silently downgrade a cancel to a degrade.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("aggregator: linked edge resolution cancelled (repo_id=%s, sha=%s): %w", in.RepoID, in.SHA, ctx.Err())
			}
			return fmt.Errorf("aggregator: linked edge resolution cancelled (repo_id=%s, sha=%s): %w", in.RepoID, in.SHA, err)
		}
		// (2) FATAL mode-store error -- the management plane
		// catalog read failed. Abort the tick WITHOUT
		// incrementing LinkedEdgeFetchFailures (which is
		// reserved for class (3) remote faults). Operators
		// see this surface as a tick error pointing at the
		// management plane; agent-memory uptime metrics stay
		// clean.
		if errors.Is(err, ErrLinkedModeStore) {
			if a.linkedLogger != nil {
				a.linkedLogger.ErrorContext(ctx, "aggregator: linked mode-store read failed; aborting tick",
					"repo_id", in.RepoID.String(),
					"sha", in.SHA,
					"err", err.Error(),
				)
			}
			return fmt.Errorf("aggregator: linked mode-store read failed (repo_id=%s, sha=%s): %w", in.RepoID, in.SHA, err)
		}
		// (3) Remote agent-memory failure -- log + leave the
		// input in embedded shape so the composer degrades
		// the row. This is the architecture's fail-safe
		// path; surfacing the error here would block every
		// system-tier tick on a single agent-memory outage.
		report.LinkedEdgeFetchFailures++
		if a.linkedLogger != nil {
			a.linkedLogger.WarnContext(ctx, "aggregator: linked edge fetch failed; degrading in place",
				"repo_id", in.RepoID.String(),
				"sha", in.SHA,
				"err", err.Error(),
			)
		}
		return nil
	}
	if !edges.Applicable {
		// Reader signalled "either gate closed for this
		// repo" -- nothing to do. The input keeps its
		// embedded-shape defaults.
		return nil
	}
	in.Mode = SystemTierModeLinked
	in.XRepoEdges = edges.XRepoEdges
	in.XRepoEdgesAvailable = edges.XRepoEdgesAvailable
	in.CallEdges = edges.CallEdges
	in.CallEdgesAvailable = edges.CallEdgesAvailable
	report.LinkedEdgeReaderApplied++
	return nil
}
