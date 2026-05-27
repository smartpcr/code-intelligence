package management

// Stage 6.3 -- production PostgreSQL implementation of
// [MetricsBackend].
//
// This file is the database-backed read substrate for the
// eight `mgmt.read.*` verbs. The [Reader] delegates every read
// to a [MetricsBackend]; [PGMetricsBackend] is the canonical
// implementation that hits the `clean_code` schema directly.
//
// # Active-row contract (architecture Sec 5.2.1 + Sec 5.2.2)
//
// SHA-pinned metric_sample reads MUST resolve through the
// Phase 1.3 `metric_sample_active` side relation AND filter
// out retracted samples via `metric_retraction` (architecture
// Sec 5.2.2 lines 1035-1037). This file's
// [PGMetricsBackend.ReadMetricSample] and
// [PGMetricsBackend.ReadMetricSamples] mirror the canonical
// join + anti-join shape established in
// `internal/rule_engine/sql_store.go:listMetricSamplesQuery`:
//
//	SELECT ms.*
//	  FROM <schema>.metric_sample_active msa
//	  JOIN <schema>.metric_sample        ms  ON ms.sample_id = msa.sample_id
//	  LEFT JOIN <schema>.metric_retraction mr ON mr.sample_id = msa.sample_id
//	 WHERE msa.repo_id = $1 AND msa.sha = $2
//	   AND mr.sample_id IS NULL
//
// Two rows at the same `(repo_id, sha, scope_id, metric_kind,
// metric_version)` quintuple -- one INSERT-only `metric_sample`
// pre-retraction, one INSERT-only `metric_sample`
// post-retraction-and-rescan -- are disambiguated by the
// `metric_sample_active.sample_id` pointer (which is re-pointed
// at the post-retraction row by the writer's UPSERT in Stage
// 3.3) AND the `metric_retraction` anti-join (which would
// otherwise let an UNPATCHED stale active pointer leak the
// retracted sample's value into a finding evaluation). Without
// the anti-join, a retract-then-stop scenario leaves the
// active pointer aimed at the retracted row; this backend's
// SQL is the only thing standing between that retracted value
// and `eval.gate`.
//
// # Reader-side invariant guard (rubber-duck item 3)
//
// The [Reader]'s post-condition checks (`row.repo_id == request.repo_id`,
// same for `sha`, `scope_id`, `metric_kind`) are layered above
// this SQL. The SQL itself is the primary correctness gate;
// the Reader guard is "trust but verify" and catches a future
// writer-bug that lands a sample at the wrong quintuple. The
// two layers are NOT redundant: the SQL filters; the guard
// surfaces backend bugs as a typed
// [ErrBackendInvariantViolation] instead of letting a wrong-
// SHA row propagate.
//
// # Schema isolation (test-friendly)
//
// The constructor takes a schema name so isolated integration
// tests can stand up the same shape in `clean_code_mgmt_test`
// or any other isolated schema. The default is the canonical
// `clean_code` schema. All identifiers are quoted via
// `pq.QuoteIdentifier` so a schema like `foo"bar` cannot inject.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// Sentinel errors emitted by [NewPGMetricsBackend] at
// composition-root wiring time.
var (
	// ErrPGMetricsBackendNilDB surfaces a nil *sql.DB at
	// wiring time.
	ErrPGMetricsBackendNilDB = errors.New("management: NewPGMetricsBackend: *sql.DB is nil")

	// ErrPGMetricsBackendEmptySchema surfaces an empty schema
	// name at wiring time.
	ErrPGMetricsBackendEmptySchema = errors.New("management: NewPGMetricsBackendWithSchema: schema is empty")
)

const pgMetricsBackendDefaultSchema = "clean_code"

// PGMetricsBackend is the production [MetricsBackend]. It
// holds an `*sql.DB` handle (the caller owns its lifecycle;
// PGMetricsBackend does NOT call `Close`) and a pre-validated
// schema name.
//
// Concurrency: `*sql.DB` is safe for concurrent use across
// goroutines, and PGMetricsBackend holds no mutable state of
// its own, so the same instance services every concurrent
// read.
type PGMetricsBackend struct {
	db     *sql.DB
	schema string
}

// NewPGMetricsBackend wires `db` against the canonical
// `clean_code` schema. Returns [ErrPGMetricsBackendNilDB]
// when `db` is nil.
func NewPGMetricsBackend(db *sql.DB) (*PGMetricsBackend, error) {
	return NewPGMetricsBackendWithSchema(db, pgMetricsBackendDefaultSchema)
}

// NewPGMetricsBackendWithSchema is the test-friendly
// schema-isolated constructor. The schema name is validated
// at wiring time -- an empty / whitespace-only name surfaces
// as [ErrPGMetricsBackendEmptySchema] rather than panicking
// at the first query.
func NewPGMetricsBackendWithSchema(db *sql.DB, schema string) (*PGMetricsBackend, error) {
	if db == nil {
		return nil, ErrPGMetricsBackendNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGMetricsBackendEmptySchema
	}
	return &PGMetricsBackend{db: db, schema: schema}, nil
}

// qual returns the schema-qualified, quoted identifier for
// the given table name (e.g. `"clean_code"."metric_sample"`).
func (b *PGMetricsBackend) qual(table string) string {
	return pq.QuoteIdentifier(b.schema) + "." + pq.QuoteIdentifier(table)
}

// ReadRepo implements [MetricsBackend]. Single-row read on
// the catalog table (architecture Sec 5.1.1).
func (b *PGMetricsBackend) ReadRepo(ctx context.Context, repoID uuid.UUID) (*RepoRow, error) {
	stmt := fmt.Sprintf(
		`SELECT repo_id, display_name, default_branch, mode::text,
		        COALESCE(repo_url, ''), COALESCE(default_branch_head, ''),
		        created_at
		   FROM %s
		  WHERE repo_id = $1`,
		b.qual("repo"),
	)
	row := b.db.QueryRowContext(ctx, stmt, repoID)
	var (
		rid           string
		displayName   string
		defaultBranch string
		mode          string
		repoURL       string
		defBranchHead string
	)
	out := &RepoRow{}
	if err := row.Scan(&rid, &displayName, &defaultBranch, &mode, &repoURL, &defBranchHead, &out.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("management.ReadRepo: scan: %w", err)
	}
	parsed, err := uuid.FromString(rid)
	if err != nil {
		return nil, fmt.Errorf("management.ReadRepo: parse repo_id=%q: %w", rid, err)
	}
	out.RepoID = parsed
	out.DisplayName = displayName
	out.DefaultBranch = defaultBranch
	out.Mode = mode
	out.RepoURL = repoURL
	out.DefaultBranchHead = defBranchHead
	return out, nil
}

// activeRowSelect returns the canonical "active row, not
// retracted" projection used by both [ReadMetricSample] and
// [ReadMetricSamples].
//
// Mirrors `internal/rule_engine/sql_store.go:listMetricSamplesQuery`
// modulo column set: management reads need the FULL
// [MetricSampleRow] projection (producer_run_id, created_at,
// degraded_reason text), the rule_engine path lifts only the
// DSL-relevant subset. The JOIN structure is identical so the
// active-row + retraction invariant is enforced consistently
// across both readers.
func (b *PGMetricsBackend) activeRowSelect(extraWhere string) string {
	return fmt.Sprintf(
		`SELECT ms.sample_id, ms.repo_id, ms.sha, ms.scope_id,
		        ms.metric_kind, ms.metric_version,
		        ms.value,
		        ms.pack::text, ms.source::text,
		        ms.degraded, COALESCE(ms.degraded_reason::text, ''),
		        ms.producer_run_id, ms.created_at
		   FROM %s msa
		   JOIN %s ms ON ms.sample_id = msa.sample_id
		   LEFT JOIN %s mr ON mr.sample_id = msa.sample_id
		  WHERE msa.repo_id = $1
		    AND msa.sha     = $2
		    AND mr.sample_id IS NULL%s
		  ORDER BY ms.scope_id, ms.metric_kind, ms.metric_version DESC`,
		b.qual("metric_sample_active"),
		b.qual("metric_sample"),
		b.qual("metric_retraction"),
		extraWhere,
	)
}

// ReadMetricSample implements [MetricsBackend] via the canonical
// active-row join + retraction anti-join, narrowed to a single
// quintuple. The `metric_sample_active` side relation's primary
// key is the FIVE-COLUMN tuple
// (repo_id, sha, scope_id, metric_kind, metric_version)
// (migration `0002_measurement.up.sql:519`); the workstream brief's
// 4-arg verb signature `(repo_id, sha, scope_id, metric_kind)`
// resolves to the LATEST `metric_version` for that quartet via
// `ORDER BY ms.metric_version DESC` in [activeRowSelect] +
// `LIMIT 1` on the first scan, satisfying the
// [MetricsBackend.ReadMetricSample] "latest active version" doc
// contract. Multiple active versions for the same quartet are a
// supported schema state (e.g. metric_version bumps that ship a
// new computation while leaving the previous one routable for
// downstream tools); the read verb returns the highest version.
func (b *PGMetricsBackend) ReadMetricSample(ctx context.Context, repoID uuid.UUID, sha string, scopeID uuid.UUID, metricKind string) (*MetricSampleRow, error) {
	stmt := b.activeRowSelect(`
		    AND msa.scope_id    = $3
		    AND msa.metric_kind = $4`) + `
		  LIMIT 1`
	rows, err := b.db.QueryContext(ctx, stmt, repoID, sha, scopeID, metricKind)
	if err != nil {
		return nil, fmt.Errorf("management.ReadMetricSample: query: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("management.ReadMetricSample: rows: %w", err)
		}
		return nil, ErrNotFound
	}
	row, err := scanMetricSampleRow(rows)
	if err != nil {
		return nil, fmt.Errorf("management.ReadMetricSample: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("management.ReadMetricSample: rows: %w", err)
	}
	return row, nil
}

// ReadMetricSamples implements [MetricsBackend] via the
// canonical active-row join + retraction anti-join, narrowed by
// the optional filter shape. Empty result is a valid success.
func (b *PGMetricsBackend) ReadMetricSamples(ctx context.Context, repoID uuid.UUID, sha string, filter MetricSamplesFilter) ([]MetricSampleRow, error) {
	var sb strings.Builder
	args := []any{repoID, sha}
	if filter.MetricKind != "" {
		args = append(args, filter.MetricKind)
		fmt.Fprintf(&sb, "\n		    AND msa.metric_kind = $%d", len(args))
	}
	if filter.ScopeID != (uuid.UUID{}) {
		args = append(args, filter.ScopeID)
		fmt.Fprintf(&sb, "\n		    AND msa.scope_id = $%d", len(args))
	}
	if filter.Pack != "" {
		args = append(args, filter.Pack)
		fmt.Fprintf(&sb, "\n		    AND ms.pack::text = $%d", len(args))
	}
	if filter.Source != "" {
		args = append(args, filter.Source)
		fmt.Fprintf(&sb, "\n		    AND ms.source::text = $%d", len(args))
	}
	stmt := b.activeRowSelect(sb.String())
	rows, err := b.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("management.ReadMetricSamples: query: %w", err)
	}
	defer rows.Close()
	out := make([]MetricSampleRow, 0)
	for rows.Next() {
		r, err := scanMetricSampleRow(rows)
		if err != nil {
			return nil, fmt.Errorf("management.ReadMetricSamples: %w", err)
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("management.ReadMetricSamples: rows: %w", err)
	}
	return out, nil
}

// scanMetricSampleRow scans the canonical 13-column shape
// declared by [activeRowSelect] into a [MetricSampleRow].
// Shared so [ReadMetricSample] and [ReadMetricSamples] never
// drift on column order.
func scanMetricSampleRow(rows *sql.Rows) (*MetricSampleRow, error) {
	var (
		sampleID       string
		repoIDRaw      string
		sha            string
		scopeIDRaw     string
		metricKind     string
		metricVersion  int
		value          sql.NullFloat64
		pack           string
		source         string
		degraded       bool
		degradedReason string
		producerRunID  string
		createdAt      = struct{}{}
	)
	_ = createdAt // placate unused
	row := &MetricSampleRow{}
	if err := rows.Scan(
		&sampleID, &repoIDRaw, &sha, &scopeIDRaw,
		&metricKind, &metricVersion,
		&value, &pack, &source,
		&degraded, &degradedReason,
		&producerRunID, &row.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("scan metric_sample row: %w", err)
	}
	sUUID, err := uuid.FromString(sampleID)
	if err != nil {
		return nil, fmt.Errorf("parse sample_id=%q: %w", sampleID, err)
	}
	rUUID, err := uuid.FromString(repoIDRaw)
	if err != nil {
		return nil, fmt.Errorf("parse repo_id=%q: %w", repoIDRaw, err)
	}
	scUUID, err := uuid.FromString(scopeIDRaw)
	if err != nil {
		return nil, fmt.Errorf("parse scope_id=%q: %w", scopeIDRaw, err)
	}
	prUUID, err := uuid.FromString(producerRunID)
	if err != nil {
		return nil, fmt.Errorf("parse producer_run_id=%q: %w", producerRunID, err)
	}
	row.SampleID = sUUID
	row.RepoID = rUUID
	row.SHA = sha
	row.ScopeID = scUUID
	row.MetricKind = metricKind
	row.MetricVersion = metricVersion
	if value.Valid {
		v := value.Float64
		row.Value = &v
	}
	row.Pack = pack
	row.Source = source
	row.Degraded = degraded
	row.DegradedReason = degradedReason
	row.ProducerRunID = prUUID
	return row, nil
}

// findingSelect returns the canonical findings projection used
// by both [ReadFindings] and [ReadRegressions].
func (b *PGMetricsBackend) findingSelect(extraWhere string) string {
	return fmt.Sprintf(
		`SELECT finding_id, evaluation_run_id, repo_id, sha, scope_id,
		        rule_id, rule_version, policy_version_id,
		        COALESCE(metric_sample_ids::text, '[]'),
		        severity::text, delta::text,
		        COALESCE(explanation_md, ''),
		        created_at
		   FROM %s
		  WHERE repo_id = $1 AND sha = $2%s
		  ORDER BY created_at, finding_id`,
		b.qual("finding"),
		extraWhere,
	)
}

// ReadFindings implements [MetricsBackend] -- every finding at
// (repo_id, sha), regardless of delta.
func (b *PGMetricsBackend) ReadFindings(ctx context.Context, repoID uuid.UUID, sha string) ([]FindingRow, error) {
	stmt := b.findingSelect("")
	return b.queryFindings(ctx, stmt, repoID, sha)
}

// ReadRegressions implements [MetricsBackend] -- the
// `delta='newly_failing'` subset of findings at (repo_id, sha)
// (architecture Sec 5.4.1 line 1189). The Reader's per-row
// guard also asserts this on the way out, but the WHERE clause
// here is the primary correctness gate.
func (b *PGMetricsBackend) ReadRegressions(ctx context.Context, repoID uuid.UUID, sha string) ([]FindingRow, error) {
	stmt := b.findingSelect(`
		    AND delta = 'newly_failing'`)
	return b.queryFindings(ctx, stmt, repoID, sha)
}

func (b *PGMetricsBackend) queryFindings(ctx context.Context, stmt string, repoID uuid.UUID, sha string) ([]FindingRow, error) {
	rows, err := b.db.QueryContext(ctx, stmt, repoID, sha)
	if err != nil {
		return nil, fmt.Errorf("management.queryFindings: query: %w", err)
	}
	defer rows.Close()
	out := make([]FindingRow, 0)
	for rows.Next() {
		var (
			findingID     string
			runID         string
			repoIDRaw     string
			rowSHA        string
			scopeID       string
			ruleID        string
			ruleVer       int
			policyID      string
			sampleIDsJSON string
			severity      string
			delta         string
			explanation   string
		)
		row := FindingRow{}
		if err := rows.Scan(
			&findingID, &runID, &repoIDRaw, &rowSHA, &scopeID,
			&ruleID, &ruleVer, &policyID,
			&sampleIDsJSON, &severity, &delta, &explanation,
			&row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("management.queryFindings: scan: %w", err)
		}
		fUUID, err := uuid.FromString(findingID)
		if err != nil {
			return nil, fmt.Errorf("management.queryFindings: parse finding_id=%q: %w", findingID, err)
		}
		eUUID, err := uuid.FromString(runID)
		if err != nil {
			return nil, fmt.Errorf("management.queryFindings: parse evaluation_run_id=%q: %w", runID, err)
		}
		rUUID, err := uuid.FromString(repoIDRaw)
		if err != nil {
			return nil, fmt.Errorf("management.queryFindings: parse repo_id=%q: %w", repoIDRaw, err)
		}
		scUUID, err := uuid.FromString(scopeID)
		if err != nil {
			return nil, fmt.Errorf("management.queryFindings: parse scope_id=%q: %w", scopeID, err)
		}
		pUUID, err := uuid.FromString(policyID)
		if err != nil {
			return nil, fmt.Errorf("management.queryFindings: parse policy_version_id=%q: %w", policyID, err)
		}
		row.FindingID = fUUID
		row.EvaluationRunID = eUUID
		row.RepoID = rUUID
		row.SHA = rowSHA
		row.ScopeID = scUUID
		row.RuleID = ruleID
		row.RuleVersion = ruleVer
		row.PolicyVersionID = pUUID
		row.Severity = severity
		row.Delta = delta
		row.ExplanationMD = explanation
		if sampleIDsJSON != "" && sampleIDsJSON != "[]" {
			var ids []string
			if err := json.Unmarshal([]byte(sampleIDsJSON), &ids); err != nil {
				return nil, fmt.Errorf("management.queryFindings: parse metric_sample_ids: %w", err)
			}
			row.MetricSampleIDs = make([]uuid.UUID, 0, len(ids))
			for _, s := range ids {
				u, err := uuid.FromString(s)
				if err != nil {
					return nil, fmt.Errorf("management.queryFindings: parse metric_sample_ids[%s]: %w", s, err)
				}
				row.MetricSampleIDs = append(row.MetricSampleIDs, u)
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("management.queryFindings: rows: %w", err)
	}
	return out, nil
}

// ReadRefactorPlan implements [MetricsBackend]. Returns the
// most recent `refactor_plan` at (repo_id, sha) with its
// embedded `refactor_task` rows. Two queries:
//   - plan_id + plan row by (repo_id, sha) ORDER BY created_at DESC LIMIT 1
//   - all refactor_task rows for that plan_id
//
// Returns [ErrNotFound] when no plan exists.
func (b *PGMetricsBackend) ReadRefactorPlan(ctx context.Context, repoID uuid.UUID, sha string) (*RefactorPlanRow, error) {
	planStmt := fmt.Sprintf(
		`SELECT plan_id, repo_id, sha,
		        COALESCE(hotspot_ids::text, '[]'),
		        COALESCE(summary_md, ''),
		        created_at
		   FROM %s
		  WHERE repo_id = $1 AND sha = $2
		  ORDER BY created_at DESC, plan_id DESC
		  LIMIT 1`,
		b.qual("refactor_plan"),
	)
	row := b.db.QueryRowContext(ctx, planStmt, repoID, sha)
	var (
		planID        string
		repoIDRaw     string
		rowSHA        string
		hotspotsJSON  string
		summary       string
	)
	plan := &RefactorPlanRow{}
	if err := row.Scan(&planID, &repoIDRaw, &rowSHA, &hotspotsJSON, &summary, &plan.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("management.ReadRefactorPlan: plan scan: %w", err)
	}
	pUUID, err := uuid.FromString(planID)
	if err != nil {
		return nil, fmt.Errorf("management.ReadRefactorPlan: parse plan_id=%q: %w", planID, err)
	}
	rUUID, err := uuid.FromString(repoIDRaw)
	if err != nil {
		return nil, fmt.Errorf("management.ReadRefactorPlan: parse repo_id=%q: %w", repoIDRaw, err)
	}
	plan.PlanID = pUUID
	plan.RepoID = rUUID
	plan.SHA = rowSHA
	plan.SummaryMD = summary
	if hotspotsJSON != "" && hotspotsJSON != "[]" {
		var ids []string
		if err := json.Unmarshal([]byte(hotspotsJSON), &ids); err != nil {
			return nil, fmt.Errorf("management.ReadRefactorPlan: parse hotspot_ids: %w", err)
		}
		plan.HotspotIDs = make([]uuid.UUID, 0, len(ids))
		for _, s := range ids {
			u, err := uuid.FromString(s)
			if err != nil {
				return nil, fmt.Errorf("management.ReadRefactorPlan: parse hotspot_ids[%s]: %w", s, err)
			}
			plan.HotspotIDs = append(plan.HotspotIDs, u)
		}
	}
	// Fetch embedded tasks for the resolved plan.
	taskStmt := fmt.Sprintf(
		`SELECT task_id, plan_id, scope_id, kind::text,
		        effort_hours, rule_id,
		        COALESCE(description_md, ''),
		        created_at
		   FROM %s
		  WHERE plan_id = $1
		  ORDER BY created_at, task_id`,
		b.qual("refactor_task"),
	)
	taskRows, err := b.db.QueryContext(ctx, taskStmt, pUUID)
	if err != nil {
		return nil, fmt.Errorf("management.ReadRefactorPlan: task query: %w", err)
	}
	defer taskRows.Close()
	for taskRows.Next() {
		var (
			taskID      string
			tPlanID     string
			scopeID     string
			kind        string
			effortHours float64
			ruleID      string
			descr       string
		)
		task := RefactorTaskRow{}
		if err := taskRows.Scan(&taskID, &tPlanID, &scopeID, &kind, &effortHours, &ruleID, &descr, &task.CreatedAt); err != nil {
			return nil, fmt.Errorf("management.ReadRefactorPlan: task scan: %w", err)
		}
		tUUID, err := uuid.FromString(taskID)
		if err != nil {
			return nil, fmt.Errorf("management.ReadRefactorPlan: parse task_id=%q: %w", taskID, err)
		}
		tpUUID, err := uuid.FromString(tPlanID)
		if err != nil {
			return nil, fmt.Errorf("management.ReadRefactorPlan: parse task plan_id=%q: %w", tPlanID, err)
		}
		scUUID, err := uuid.FromString(scopeID)
		if err != nil {
			return nil, fmt.Errorf("management.ReadRefactorPlan: parse task scope_id=%q: %w", scopeID, err)
		}
		task.TaskID = tUUID
		task.PlanID = tpUUID
		task.ScopeID = scUUID
		task.Kind = kind
		task.EffortHours = effortHours
		task.RuleID = ruleID
		task.DescriptionMD = descr
		plan.Tasks = append(plan.Tasks, task)
	}
	if err := taskRows.Err(); err != nil {
		return nil, fmt.Errorf("management.ReadRefactorPlan: task rows: %w", err)
	}
	return plan, nil
}

// ReadCrossRepo implements [MetricsBackend] -- the latest-
// dashboard `cross_repo_percentile` row for (metric_kind,
// scope_kind). The table's PK is on `percentile_id` but the
// (metric_kind, scope_kind) pair is the natural key the
// aggregator pins by latest `built_at`, so we sort DESC and
// LIMIT 1 (architecture Sec 5.2.5).
func (b *PGMetricsBackend) ReadCrossRepo(ctx context.Context, metricKind, scopeKind string) (*CrossRepoRow, error) {
	stmt := fmt.Sprintf(
		`SELECT percentile_id, metric_kind, scope_kind::text,
		        p50, p90, p99,
		        COALESCE(histogram_json::text, '{}'),
		        built_at
		   FROM %s
		  WHERE metric_kind = $1 AND scope_kind::text = $2
		  ORDER BY built_at DESC, percentile_id DESC
		  LIMIT 1`,
		b.qual("cross_repo_percentile"),
	)
	row := b.db.QueryRowContext(ctx, stmt, metricKind, scopeKind)
	var (
		percentileID  string
		rowMetricKind string
		rowScopeKind  string
		histogram     string
	)
	out := &CrossRepoRow{}
	if err := row.Scan(&percentileID, &rowMetricKind, &rowScopeKind, &out.P50, &out.P90, &out.P99, &histogram, &out.BuiltAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("management.ReadCrossRepo: scan: %w", err)
	}
	pUUID, err := uuid.FromString(percentileID)
	if err != nil {
		return nil, fmt.Errorf("management.ReadCrossRepo: parse percentile_id=%q: %w", percentileID, err)
	}
	out.PercentileID = pUUID
	out.MetricKind = rowMetricKind
	out.ScopeKind = rowScopeKind
	if histogram != "" && histogram != "{}" {
		out.HistogramJSON = json.RawMessage(histogram)
	}
	return out, nil
}

// ReadPortfolio implements [MetricsBackend]. Returns the
// latest `portfolio_snapshot` row per (metric_kind, scope_kind)
// for the given metric_kind. A scope_kind may have multiple
// snapshots across time; we surface the most recent one per
// scope_kind via a DISTINCT ON SQL pattern (architecture
// Sec 5.2.6).
func (b *PGMetricsBackend) ReadPortfolio(ctx context.Context, metricKind string) ([]PortfolioRow, error) {
	stmt := fmt.Sprintf(
		`SELECT DISTINCT ON (scope_kind::text)
		        portfolio_snapshot_id, metric_kind, scope_kind::text,
		        repo_count,
		        COALESCE(aggregate_json::text, '{}'),
		        built_at
		   FROM %s
		  WHERE metric_kind = $1
		  ORDER BY scope_kind::text, built_at DESC, portfolio_snapshot_id DESC`,
		b.qual("portfolio_snapshot"),
	)
	rows, err := b.db.QueryContext(ctx, stmt, metricKind)
	if err != nil {
		return nil, fmt.Errorf("management.ReadPortfolio: query: %w", err)
	}
	defer rows.Close()
	out := make([]PortfolioRow, 0)
	for rows.Next() {
		var (
			snapshotID    string
			rowMetricKind string
			rowScopeKind  string
			repoCount     int
			aggregate     string
		)
		row := PortfolioRow{}
		if err := rows.Scan(&snapshotID, &rowMetricKind, &rowScopeKind, &repoCount, &aggregate, &row.BuiltAt); err != nil {
			return nil, fmt.Errorf("management.ReadPortfolio: scan: %w", err)
		}
		sUUID, err := uuid.FromString(snapshotID)
		if err != nil {
			return nil, fmt.Errorf("management.ReadPortfolio: parse portfolio_snapshot_id=%q: %w", snapshotID, err)
		}
		row.PortfolioSnapshotID = sUUID
		row.MetricKind = rowMetricKind
		row.ScopeKind = rowScopeKind
		row.RepoCount = repoCount
		if aggregate != "" && aggregate != "{}" {
			row.AggregateJSON = json.RawMessage(aggregate)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("management.ReadPortfolio: rows: %w", err)
	}
	return out, nil
}

// Compile-time assertion: PGMetricsBackend satisfies the
// MetricsBackend interface.
var _ MetricsBackend = (*PGMetricsBackend)(nil)
