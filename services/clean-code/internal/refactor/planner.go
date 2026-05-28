package refactor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// -----------------------------------------------------------------------------
// Reader / writer interfaces -- the orchestration boundaries
// -----------------------------------------------------------------------------

// PolicyReader returns the currently-active [PolicySnapshot]
// (the bundle of `policy_version_id` + `refactor_weights`)
// per the canonical "evaluator-pickup" query: read the latest
// `policy_activation` row, then dereference to the
// `policy_version` it pins (architecture Sec 5.3.4 + Sec 5.3.3).
//
// Implementations:
//
//   - [StewardPolicyReader] -- wraps [steward.Steward], the
//     production composition root. Reads from
//     `clean_code.policy_activation` and
//     `clean_code.policy_version`.
//
// Returns `(zero, false, nil)` when no [steward.PolicyActivation]
// row exists yet -- a fresh-deploy steady state. The [Planner]
// translates that into [ErrNoActivePolicy] so the caller can
// distinguish "no policy yet" from "read failed".
type PolicyReader interface {
	ActivePolicyVersion(ctx context.Context) (PolicySnapshot, bool, error)
}

// MetricSampleReader returns the per-scope foundation-tier
// metric values for a given (repo_id, sha). The returned
// [ScopeInputs] slice has exactly one row per `scope_id`; for
// each scope the relevant `Cyclo` / `CognitiveComplexity` /
// `ModificationCount` / `CouplingBetweenObjects` / `FanOut`
// fields are populated (with their `Has<Field>` bool set to
// `true`) according to which `metric_kind` values were
// present in the database.
//
// When multiple `metric_sample` rows exist for the same
// (`scope_id`, `metric_kind`) -- a deliberate consequence of
// the append-only G3 invariant -- the reader MUST return the
// row with the largest `metric_version` (the latest
// re-computation supersedes earlier ones).
//
// `metricKinds` is the closed filter set (the planner passes
// [HotSpotInputMetricKinds]); the reader MUST ignore samples
// whose `metric_kind` is outside this set.
//
// Implementations:
//
//   - [InMemoryMetricSampleReader] -- a process-local slice
//     used by unit tests and the scaffold-mode composition
//     root.
//   - [SQLMetricSampleReader] -- the production reader
//     against `clean_code.metric_sample`.
type MetricSampleReader interface {
	ScopeMetrics(
		ctx context.Context,
		repoID uuid.UUID,
		sha string,
		metricKinds []string,
	) ([]ScopeInputs, error)
}

// FindingReader returns the per-scope count of "qualifying"
// findings for a given (repo_id, sha). The canonical filter
// is `delta IN ('new', 'newly_failing')` per architecture
// Sec 5.4.1 lines 1186-1190 -- the two delta values that
// represent fresh tech-debt the planner should surface.
// (`unchanged` rows would double-count chronic issues;
// `resolved` rows would invert the signal.)
//
// The reader MUST NOT include findings whose `delta` is
// outside the qualifying set; the [IsHotSpotQualifyingDelta]
// helper documents the canonical closed set.
//
// Returns an empty map when no qualifying findings exist;
// callers MUST treat "absent key" as zero-count.
//
// Implementations:
//
//   - [InMemoryFindingReader] -- a process-local slice.
//   - [SQLFindingReader] -- the production reader against
//     `clean_code.finding`.
type FindingReader interface {
	FindingCountsByScope(
		ctx context.Context,
		repoID uuid.UUID,
		sha string,
	) (map[uuid.UUID]int, error)
}

// HotSpotWriter persists one batch of `hot_spot` rows. The
// SQL implementation runs them in a single transaction so a
// partial batch never lands. The hot_spot table is
// append-only (architecture Sec 5.5.1) -- a re-rank at a new
// SHA inserts new rows; existing rows remain.
//
// Implementations:
//
//   - [InMemoryHotSpotWriter] -- collects rows in a slice.
//   - [SQLHotSpotWriter] -- INSERTs into `clean_code.hot_spot`.
type HotSpotWriter interface {
	WriteHotSpots(ctx context.Context, rows []HotSpot) error
}

// -----------------------------------------------------------------------------
// Sentinel errors and result type
// -----------------------------------------------------------------------------

// ErrNoActivePolicy is returned by [Planner.Plan] when the
// [PolicyReader] reports no active policy version. Distinct
// from a read failure so the caller can branch:
//
//	switch {
//	case errors.Is(err, ErrNoActivePolicy):
//	    // fresh deploy: skip hotspot ranking this tick
//	case err != nil:
//	    // transport/store failure: alert + retry
//	}
var ErrNoActivePolicy = errors.New(
	"refactor: no active policy version")

// ErrNilPolicyReader / ErrNilMetricSampleReader /
// ErrNilFindingReader / ErrNilHotSpotWriter signal a wiring
// bug in the composition root: [NewPlanner] requires every
// dependency to be non-nil.
var (
	ErrNilPolicyReader       = errors.New("refactor: PolicyReader is nil")
	ErrNilMetricSampleReader = errors.New("refactor: MetricSampleReader is nil")
	ErrNilFindingReader      = errors.New("refactor: FindingReader is nil")
	ErrNilHotSpotWriter      = errors.New("refactor: HotSpotWriter is nil")
)

// PlanResult is the structured return value of
// [Planner.Plan]: the policy snapshot the plan was scored
// against, the emitted `hot_spot` rows (in deterministic
// sort order, highest score first), and the per-row
// breakdowns useful for telemetry / explainability. The
// breakdown is intentionally NOT persisted -- the canonical
// `hot_spot` schema (migration 0003) has no breakdown
// column.
type PlanResult struct {
	// PolicyVersionID stamps the snapshot the plan was
	// scored against. Equal to every `HotSpot.PolicyVersionID`
	// in [PlanResult.HotSpots].
	PolicyVersionID uuid.UUID

	// HotSpots is the persisted batch in canonical sort
	// order (Score DESC, ScopeID ASC).
	HotSpots []HotSpot

	// Breakdowns mirrors [HotSpots] index-for-index: the
	// per-dimension z-scores and raw finding count that
	// produced each row's `Score`. Useful for observability
	// (a "why is this scope ranked highest?" debug surface)
	// but intentionally not persisted to the hot_spot table.
	Breakdowns []Breakdown
}

// -----------------------------------------------------------------------------
// Planner -- the orchestrator
// -----------------------------------------------------------------------------

// Planner is the Stage 8.1 Refactor Planner. It owns the
// read → compute → write loop the architecture pins as the
// "sole writer" of `clean_code.hot_spot`:
//
//  1. READ the currently-active [steward.PolicyVersion]
//     (Sec 5.3.3) and extract its [RefactorWeights] +
//     `policy_version_id`.
//  2. READ the per-scope `metric_sample` rows for
//     [HotSpotInputMetricKinds] at the given (repo_id, sha).
//  3. READ + COUNT the per-scope `finding` rows whose
//     `delta IN ('new', 'newly_failing')` at the same
//     (repo_id, sha).
//  4. COMPOSE one [ScopeInputs] per scope_id (union of the
//     scope_ids returned by step 2 and step 3) and call
//     [Computer.Compute].
//  5. WRITE the resulting [HotSpot] rows via the
//     [HotSpotWriter] (one batch, one transaction in the SQL
//     impl).
//
// The Planner is stateless across calls; every Plan() runs
// the full loop. Concurrency is the caller's responsibility:
// the Planner does not lock and several Plan() goroutines
// can run side-by-side against distinct (repo_id, sha)
// tuples.
type Planner struct {
	policy   PolicyReader
	metrics  MetricSampleReader
	findings FindingReader
	writer   HotSpotWriter
	compute  *Computer
}

// NewPlanner wires a [Planner] with the four required
// dependencies + optional [Option] arguments forwarded to
// the underlying [Computer]. Returns an error when any
// dependency is nil so a composition-root wiring bug
// surfaces as a clean error rather than a nil-pointer panic
// at Plan() time.
//
// Pass [WithIDFactory] / [WithClock] to override the
// [Computer]'s ID factory or clock (typically used by tests
// to pin deterministic output).
func NewPlanner(
	policy PolicyReader,
	metrics MetricSampleReader,
	findings FindingReader,
	writer HotSpotWriter,
	opts ...Option,
) (*Planner, error) {
	if policy == nil {
		return nil, ErrNilPolicyReader
	}
	if metrics == nil {
		return nil, ErrNilMetricSampleReader
	}
	if findings == nil {
		return nil, ErrNilFindingReader
	}
	if writer == nil {
		return nil, ErrNilHotSpotWriter
	}
	return &Planner{
		policy:   policy,
		metrics:  metrics,
		findings: findings,
		writer:   writer,
		compute:  NewComputer(opts...),
	}, nil
}

// Plan executes the full read → compute → write loop for one
// (repo_id, sha) tuple. Returns:
//
//   - [ErrNoActivePolicy] when no [steward.PolicyVersion] has
//     been activated yet (a clean signal so the caller can
//     skip ranking on fresh deploys).
//   - A wrapped error when any of the four dependencies fails.
//   - On success: a [PlanResult] whose [HotSpots] slice is
//     the persisted batch.
//
// An empty input (no scope had any metric_sample row and no
// scope had any qualifying finding) returns a [PlanResult]
// with `HotSpots == nil` and the writer is still called with
// a nil slice; writer implementations MUST treat that as a
// no-op rather than failing.
//
// The PolicySnapshot returned by step 1 is the SAME snapshot
// passed to [Computer.Compute] in step 4: all rows in the
// batch carry the SAME `policy_version_id` -- the canonical
// "policy attribution" guarantee architecture Sec 5.5.1 pins.
func (p *Planner) Plan(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
) (PlanResult, error) {
	// Step 1: read the active policy snapshot.
	snap, ok, err := p.policy.ActivePolicyVersion(ctx)
	if err != nil {
		return PlanResult{}, fmt.Errorf("refactor.Plan: read active policy: %w", err)
	}
	if !ok {
		return PlanResult{}, ErrNoActivePolicy
	}

	// Step 2: read per-scope metric samples.
	metricInputs, err := p.metrics.ScopeMetrics(ctx, repoID, sha, HotSpotInputMetricKinds)
	if err != nil {
		return PlanResult{}, fmt.Errorf("refactor.Plan: read metric_sample: %w", err)
	}

	// Step 3: read + count per-scope qualifying findings.
	findingCounts, err := p.findings.FindingCountsByScope(ctx, repoID, sha)
	if err != nil {
		return PlanResult{}, fmt.Errorf("refactor.Plan: count findings: %w", err)
	}

	// Step 4: compose ScopeInputs (union of scope_ids).
	inputs := mergeMetricsAndFindings(metricInputs, findingCounts)
	if len(inputs) == 0 {
		// Empty input is well-defined: write nothing, return
		// nil HotSpots. Still call the writer (with nil) so
		// the contract is honoured.
		if err := p.writer.WriteHotSpots(ctx, nil); err != nil {
			return PlanResult{}, fmt.Errorf("refactor.Plan: write empty batch: %w", err)
		}
		return PlanResult{PolicyVersionID: snap.PolicyVersionID}, nil
	}

	comps, err := p.compute.Compute(snap, repoID, sha, inputs)
	if err != nil {
		return PlanResult{}, fmt.Errorf("refactor.Plan: compute: %w", err)
	}

	// Step 5: extract HotSpot + Breakdown columns and write.
	rows := make([]HotSpot, len(comps))
	bks := make([]Breakdown, len(comps))
	for i, c := range comps {
		rows[i] = c.HotSpot
		bks[i] = c.Breakdown
	}
	if err := p.writer.WriteHotSpots(ctx, rows); err != nil {
		return PlanResult{}, fmt.Errorf("refactor.Plan: write hot_spot batch: %w", err)
	}

	return PlanResult{
		PolicyVersionID: snap.PolicyVersionID,
		HotSpots:        rows,
		Breakdowns:      bks,
	}, nil
}

// mergeMetricsAndFindings combines the per-scope metric rows
// from the [MetricSampleReader] with the per-scope finding
// counts from the [FindingReader] into a single deterministic
// `[]ScopeInputs`. Scopes that appear in only one of the two
// inputs still emit a row (with `FindingCount=0` when no
// finding was found, or with no metric fields populated when
// the scope had findings but no metric samples). Output is
// sorted by `ScopeID ASC` for determinism.
func mergeMetricsAndFindings(
	metricRows []ScopeInputs,
	findingCounts map[uuid.UUID]int,
) []ScopeInputs {
	// Index metric rows by ScopeID. A reader that emits
	// multiple rows per scope_id is a contract violation;
	// the last-write-wins here is a defensive fallback but
	// the Computer will additionally reject duplicates.
	byScope := make(map[uuid.UUID]ScopeInputs, len(metricRows))
	for _, r := range metricRows {
		byScope[r.ScopeID] = r
	}
	// Union the scope_id set.
	scopeIDs := make(map[uuid.UUID]struct{}, len(metricRows)+len(findingCounts))
	for id := range byScope {
		scopeIDs[id] = struct{}{}
	}
	for id := range findingCounts {
		scopeIDs[id] = struct{}{}
	}
	if len(scopeIDs) == 0 {
		return nil
	}
	out := make([]ScopeInputs, 0, len(scopeIDs))
	for id := range scopeIDs {
		in := byScope[id] // zero-valued when absent
		in.ScopeID = id
		in.FindingCount = findingCounts[id] // zero when absent
		out = append(out, in)
	}
	// Deterministic order: byte-lex on the uuid. Stage 8.1
	// invariant: the same set of input rows always produces
	// the same ScopeInputs slice across processes.
	sort.Slice(out, func(i, j int) bool {
		return uuidLess(out[i].ScopeID, out[j].ScopeID)
	})
	return out
}

// -----------------------------------------------------------------------------
// StewardPolicyReader -- production [PolicyReader]
// -----------------------------------------------------------------------------

// StewardPolicyReader adapts a [steward.Steward] to the
// [PolicyReader] contract. Production composition root wiring
// looks like:
//
//	st := steward.New(steward.Config{Store: store, Signer: signer})
//	planner, _ := refactor.NewPlanner(
//	    &refactor.StewardPolicyReader{Steward: st},
//	    metricReader,
//	    findingReader,
//	    hotSpotWriter,
//	)
//
// The adapter holds NO state; it can be value-shared across
// goroutines safely.
type StewardPolicyReader struct {
	// Steward is the steward whose `ActivePolicyVersion`
	// method this adapter calls. MUST be non-nil; the
	// [Planner] wires this through [NewPlanner] which
	// rejects a nil reader.
	Steward *steward.Steward
}

// ActivePolicyVersion implements [PolicyReader] by delegating
// to [steward.Steward.ActivePolicyVersion] and projecting the
// returned [steward.PolicyVersion] onto a [PolicySnapshot].
func (r *StewardPolicyReader) ActivePolicyVersion(ctx context.Context) (PolicySnapshot, bool, error) {
	if r == nil || r.Steward == nil {
		return PolicySnapshot{}, false, errors.New(
			"refactor.StewardPolicyReader: Steward is nil")
	}
	pv, ok, err := r.Steward.ActivePolicyVersion(ctx)
	if err != nil {
		return PolicySnapshot{}, false, err
	}
	if !ok {
		return PolicySnapshot{}, false, nil
	}
	return PolicySnapshot{
		PolicyVersionID: pv.PolicyVersionID,
		Weights:         pv.RefactorWeights,
	}, true, nil
}

// -----------------------------------------------------------------------------
// In-memory implementations -- used by tests and scaffold mode
// -----------------------------------------------------------------------------

// InMemoryMetricSample is a single `metric_sample` row used
// by [InMemoryMetricSampleReader]. Mirrors the
// canonical-natural-key columns from architecture Sec 5.2.1.
type InMemoryMetricSample struct {
	RepoID        uuid.UUID
	SHA           string
	ScopeID       uuid.UUID
	MetricKind    string
	MetricVersion int
	Value         float64
}

// InMemoryMetricSampleReader is a process-local
// [MetricSampleReader] backed by a slice. Used by unit
// tests and by the scaffold-mode composition root when
// `CLEAN_CODE_PG_URL` is unset.
//
// The reader's filter + dedupe logic mirrors what the
// production [SQLMetricSampleReader] implements:
//
//   - rows whose `metric_kind` is OUTSIDE the supplied
//     `metricKinds` filter are skipped,
//   - rows whose (repo_id, sha) doesn't match are skipped,
//   - for each (scope_id, metric_kind) pair the row with
//     the largest `metric_version` wins.
type InMemoryMetricSampleReader struct {
	mu      sync.Mutex
	samples []InMemoryMetricSample
}

// NewInMemoryMetricSampleReader returns a fresh reader.
func NewInMemoryMetricSampleReader() *InMemoryMetricSampleReader {
	return &InMemoryMetricSampleReader{}
}

// Insert appends a sample. Concurrent-safe.
func (r *InMemoryMetricSampleReader) Insert(s InMemoryMetricSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, s)
}

// ScopeMetrics implements [MetricSampleReader] using the
// dedupe + filter rules documented on the interface.
func (r *InMemoryMetricSampleReader) ScopeMetrics(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
	metricKinds []string,
) ([]ScopeInputs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kindSet := make(map[string]struct{}, len(metricKinds))
	for _, k := range metricKinds {
		kindSet[k] = struct{}{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// First pass: filter and dedupe by (scope_id, metric_kind)
	// taking the largest metric_version.
	type key struct {
		scopeID    uuid.UUID
		metricKind string
	}
	best := make(map[key]InMemoryMetricSample)
	for _, s := range r.samples {
		if s.RepoID != repoID || s.SHA != sha {
			continue
		}
		if _, ok := kindSet[s.MetricKind]; !ok {
			continue
		}
		k := key{scopeID: s.ScopeID, metricKind: s.MetricKind}
		if existing, ok := best[k]; ok && existing.MetricVersion >= s.MetricVersion {
			continue
		}
		best[k] = s
	}

	// Second pass: assemble per-scope ScopeInputs.
	byScope := make(map[uuid.UUID]*ScopeInputs)
	for k, s := range best {
		in, ok := byScope[k.scopeID]
		if !ok {
			fresh := ScopeInputs{ScopeID: k.scopeID}
			byScope[k.scopeID] = &fresh
			in = byScope[k.scopeID]
		}
		applyMetricSampleToScopeInputs(in, k.metricKind, s.Value)
	}

	out := make([]ScopeInputs, 0, len(byScope))
	for _, in := range byScope {
		out = append(out, *in)
	}
	sort.Slice(out, func(i, j int) bool {
		return uuidLess(out[i].ScopeID, out[j].ScopeID)
	})
	return out, nil
}

// applyMetricSampleToScopeInputs sets the right
// [ScopeInputs] field for one (metric_kind, value) pair --
// the single point that maps the canonical metric_kind set
// onto the struct fields. A new HotSpot input metric_kind
// requires (1) a new const + slice entry, (2) a new field
// on ScopeInputs, AND (3) a case here.
func applyMetricSampleToScopeInputs(in *ScopeInputs, metricKind string, value float64) {
	switch metricKind {
	case MetricKindCyclo:
		in.Cyclo = value
		in.HasCyclo = true
	case MetricKindCognitiveComplexity:
		in.CognitiveComplexity = value
		in.HasCognitiveComplexity = true
	case MetricKindModificationCountInWindow:
		in.ModificationCount = value
		in.HasModificationCount = true
	case MetricKindCouplingBetweenObjects:
		in.CouplingBetweenObjects = value
		in.HasCouplingBetweenObjects = true
	case MetricKindFanOut:
		in.FanOut = value
		in.HasFanOut = true
	}
}

// InMemoryFinding is a single `finding` row used by
// [InMemoryFindingReader]. Only the columns the planner's
// COUNT consumes are modelled.
type InMemoryFinding struct {
	RepoID  uuid.UUID
	SHA     string
	ScopeID uuid.UUID
	Delta   rule_engine.Delta
}

// InMemoryFindingReader is a process-local [FindingReader]
// backed by a slice. Its filter + count logic mirrors the
// production [SQLFindingReader]: rows whose (repo_id, sha)
// match AND whose `Delta` is in [HotSpotQualifyingDeltas]
// contribute one to the per-scope counter.
type InMemoryFindingReader struct {
	mu       sync.Mutex
	findings []InMemoryFinding
}

// NewInMemoryFindingReader returns a fresh reader.
func NewInMemoryFindingReader() *InMemoryFindingReader {
	return &InMemoryFindingReader{}
}

// Insert appends a finding. Concurrent-safe.
func (r *InMemoryFindingReader) Insert(f InMemoryFinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.findings = append(r.findings, f)
}

// FindingCountsByScope implements [FindingReader]. Counts
// only rows whose `Delta` qualifies per
// [IsHotSpotQualifyingDelta].
func (r *InMemoryFindingReader) FindingCountsByScope(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
) (map[uuid.UUID]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[uuid.UUID]int)
	for _, f := range r.findings {
		if f.RepoID != repoID || f.SHA != sha {
			continue
		}
		if !IsHotSpotQualifyingDelta(f.Delta) {
			continue
		}
		out[f.ScopeID]++
	}
	return out, nil
}

// InMemoryHotSpotWriter is a process-local [HotSpotWriter]
// that collects every batch in a slice. Tests inspect
// [InMemoryHotSpotWriter.Rows] to assert which rows the
// planner emitted.
type InMemoryHotSpotWriter struct {
	mu   sync.Mutex
	rows []HotSpot
}

// NewInMemoryHotSpotWriter returns a fresh writer.
func NewInMemoryHotSpotWriter() *InMemoryHotSpotWriter {
	return &InMemoryHotSpotWriter{}
}

// WriteHotSpots implements [HotSpotWriter] by appending the
// batch to the in-memory slice. A nil batch is a no-op.
func (w *InMemoryHotSpotWriter) WriteHotSpots(ctx context.Context, rows []HotSpot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rows = append(w.rows, rows...)
	return nil
}

// Rows returns a snapshot of every row ever written to this
// writer, in insert order. Returns a fresh slice so callers
// can mutate it without affecting subsequent reads.
func (w *InMemoryHotSpotWriter) Rows() []HotSpot {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]HotSpot, len(w.rows))
	copy(out, w.rows)
	return out
}

// Reset clears the writer's state. Used by tests that
// exercise multiple Plan() calls back-to-back.
func (w *InMemoryHotSpotWriter) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rows = nil
}

// -----------------------------------------------------------------------------
// SQL-backed implementations -- production persistence path
// -----------------------------------------------------------------------------

// schemaName is the canonical PostgreSQL schema the
// CLEAN-CODE service owns. Mirrors [steward.DefaultSchema] so
// schema drift between the two packages surfaces as a compile
// error rather than a SQL error at runtime.
const schemaName = steward.DefaultSchema

// SQLMetricSampleReader is the production [MetricSampleReader]
// against `clean_code.metric_sample`. The implementation
// applies the dedupe-by-max-`metric_version` rule via SQL
// (DISTINCT ON) so the application code never sees more than
// one row per (scope_id, metric_kind).
type SQLMetricSampleReader struct {
	db *sql.DB
}

// NewSQLMetricSampleReader wraps db. The caller owns the
// `*sql.DB` lifecycle.
func NewSQLMetricSampleReader(db *sql.DB) *SQLMetricSampleReader {
	return &SQLMetricSampleReader{db: db}
}

// ScopeMetrics issues the canonical foundation-tier query
// and assembles per-scope [ScopeInputs] rows. The SELECT
// uses PostgreSQL's `DISTINCT ON` to pick the largest
// `metric_version` per (scope_id, metric_kind); the result
// rows are scanned and folded into the [ScopeInputs] struct
// fields via [applyMetricSampleToScopeInputs] (so the
// metric_kind -> struct field mapping has ONE definition
// site).
func (r *SQLMetricSampleReader) ScopeMetrics(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
	metricKinds []string,
) ([]ScopeInputs, error) {
	q := fmt.Sprintf(`
		SELECT DISTINCT ON (scope_id, metric_kind)
		    scope_id, metric_kind, value
		  FROM %s.metric_sample
		 WHERE repo_id = $1
		   AND sha = $2
		   AND metric_kind = ANY($3)
		   AND value IS NOT NULL
		 ORDER BY scope_id, metric_kind, metric_version DESC
	`, schemaName)
	rows, err := r.db.QueryContext(ctx, q, repoID, sha, pq.Array(metricKinds))
	if err != nil {
		return nil, fmt.Errorf("refactor.SQLMetricSampleReader.ScopeMetrics: query: %w", err)
	}
	defer rows.Close()

	byScope := make(map[uuid.UUID]*ScopeInputs)
	for rows.Next() {
		var (
			scopeID    uuid.UUID
			metricKind string
			value      float64
		)
		if err := rows.Scan(&scopeID, &metricKind, &value); err != nil {
			return nil, fmt.Errorf("refactor.SQLMetricSampleReader.ScopeMetrics: scan: %w", err)
		}
		in, ok := byScope[scopeID]
		if !ok {
			fresh := ScopeInputs{ScopeID: scopeID}
			byScope[scopeID] = &fresh
			in = byScope[scopeID]
		}
		applyMetricSampleToScopeInputs(in, metricKind, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("refactor.SQLMetricSampleReader.ScopeMetrics: iterate: %w", err)
	}

	out := make([]ScopeInputs, 0, len(byScope))
	for _, in := range byScope {
		out = append(out, *in)
	}
	sort.Slice(out, func(i, j int) bool {
		return uuidLess(out[i].ScopeID, out[j].ScopeID)
	})
	return out, nil
}

// SQLFindingReader is the production [FindingReader] against
// `clean_code.finding`. Aggregates the COUNT in SQL so the
// application code processes one row per scope, not one per
// finding.
type SQLFindingReader struct {
	db *sql.DB
}

// NewSQLFindingReader wraps db.
func NewSQLFindingReader(db *sql.DB) *SQLFindingReader {
	return &SQLFindingReader{db: db}
}

// FindingCountsByScope issues the canonical qualifying-
// findings query: COUNT(*) GROUP BY scope_id where
// `delta IN ('new', 'newly_failing')`. The IN list is built
// from [HotSpotQualifyingDeltas] so a future canonical-set
// change lands in one place.
func (r *SQLFindingReader) FindingCountsByScope(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
) (map[uuid.UUID]int, error) {
	qualifying := make([]string, len(HotSpotQualifyingDeltas))
	for i, d := range HotSpotQualifyingDeltas {
		qualifying[i] = string(d)
	}
	q := fmt.Sprintf(`
		SELECT scope_id, COUNT(*)::bigint
		  FROM %s.finding
		 WHERE repo_id = $1
		   AND sha = $2
		   AND delta::text = ANY($3)
		 GROUP BY scope_id
	`, schemaName)
	rows, err := r.db.QueryContext(ctx, q, repoID, sha, pq.Array(qualifying))
	if err != nil {
		return nil, fmt.Errorf("refactor.SQLFindingReader.FindingCountsByScope: query: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]int)
	for rows.Next() {
		var (
			scopeID uuid.UUID
			count   int64
		)
		if err := rows.Scan(&scopeID, &count); err != nil {
			return nil, fmt.Errorf("refactor.SQLFindingReader.FindingCountsByScope: scan: %w", err)
		}
		out[scopeID] = int(count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("refactor.SQLFindingReader.FindingCountsByScope: iterate: %w", err)
	}
	return out, nil
}

// SQLHotSpotWriter is the production [HotSpotWriter] against
// `clean_code.hot_spot`. INSERTs the batch in a single
// transaction so a partial batch never lands (append-only
// invariant from architecture Sec 5.5.1).
type SQLHotSpotWriter struct {
	db *sql.DB
}

// NewSQLHotSpotWriter wraps db.
func NewSQLHotSpotWriter(db *sql.DB) *SQLHotSpotWriter {
	return &SQLHotSpotWriter{db: db}
}

// WriteHotSpots INSERTs every row in `rows` in a single
// transaction. Uses an explicit `created_at` column rather
// than the table's `DEFAULT now()` so all rows in the batch
// share the same timestamp (the [Computer]'s clock
// snapshot, which is taken ONCE per Compute call).
func (w *SQLHotSpotWriter) WriteHotSpots(ctx context.Context, rows []HotSpot) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("refactor.SQLHotSpotWriter.WriteHotSpots: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op when Commit succeeded

	q := fmt.Sprintf(`
		INSERT INTO %s.hot_spot (
		    hotspot_id,
		    repo_id,
		    sha,
		    scope_id,
		    score,
		    policy_version_id,
		    created_at
		) VALUES (
		    $1, $2, $3, $4, $5, $6, $7
		)
	`, schemaName)
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("refactor.SQLHotSpotWriter.WriteHotSpots: prepare: %w", err)
	}
	defer stmt.Close()

	for i, hs := range rows {
		_, err := stmt.ExecContext(
			ctx,
			hs.HotspotID,
			hs.RepoID,
			hs.SHA,
			hs.ScopeID,
			hs.Score,
			hs.PolicyVersionID,
			hs.CreatedAt.UTC(),
		)
		if err != nil {
			return fmt.Errorf(
				"refactor.SQLHotSpotWriter.WriteHotSpots: insert row[%d] (hotspot_id=%s): %w",
				i, hs.HotspotID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("refactor.SQLHotSpotWriter.WriteHotSpots: commit: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------------

// (intentionally empty; uuidLess lives in hotspot.go and is
// shared between the Computer's sort and the Planner's
// scope-id deduplication so a future change to the byte-lex
// ordering rule lands in one place.)
