package partitionmaintainer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Default parameter values surfaced both as package constants
// (so the binary's loadConfig can reference them in env help
// text) and as the zero-value fallback inside `New`.
const (
	// DefaultMaintenanceInterval is the Stage 8.2 SLO --
	// "every 10 minutes" per implementation-plan §8.2 step 1.
	// Override via Config.MaintenanceInterval to align with
	// pg_partman_bgw cadence changes.
	DefaultMaintenanceInterval = 10 * time.Minute

	// DefaultMaintenanceTimeout bounds a single RunMaintenance
	// invocation so a stuck CREATE TABLE under partman
	// (e.g. waiting on an AccessExclusiveLock from a
	// long-running tenant transaction) cannot stall the loop
	// indefinitely. Generous enough that a multi-parent
	// catch-up after a long binary outage still completes;
	// tight enough that an operator notices a regression
	// within ~3 ticks.
	DefaultMaintenanceTimeout = 2 * time.Minute

	// DefaultLagScrapeInterval is the gauge-refresh cadence.
	// Set to 1 minute so the §8.2 alert (which uses `for: 10m`
	// in the rule file) has ≥10 distinct evaluation samples
	// inside its persistence window.
	DefaultLagScrapeInterval = 1 * time.Minute

	// DefaultLagScrapeTimeout bounds a single ScrapeLag call.
	// The scrape is a read-only catalog walk -- one or two
	// short queries per registered parent -- so 30s is
	// comfortably above the worst observed scrape on a busy
	// cluster.
	DefaultLagScrapeTimeout = 30 * time.Second

	// LagForwardWindow is the "+1 day" the metric definition
	// adds when computing parent lag (per implementation-plan
	// §8.2: "oldest NEXT-DAY partition that is missing"). It is
	// not configurable -- a smaller window would race the bgw
	// tick and a larger one would mute the alert's purpose.
	LagForwardWindow = 24 * time.Hour
)

// Config is the env-derived (or programmatic) configuration the
// Service consumes. Construct via `Config{...}` literal and pass
// to `New`; missing optional fields fall back to the
// corresponding Default* constant.
type Config struct {
	// ParentTables is an explicit allow-list of schema-
	// qualified parent names (e.g. `"public.episode"`). When
	// non-empty it bypasses the `partman.part_config` lookup
	// entirely.
	//
	// Useful in tests that want to scope to a specific test
	// schema and avoid touching other tenants' parents. In
	// production the list is left nil so the binary picks up
	// every registered parent.
	//
	// Each entry MUST be schema-qualified. New rejects an entry
	// that lacks a dot for the same reason as the
	// tracelogpruner: `partman.part_config.parent_table` is
	// looked up by literal string match.
	ParentTables []string

	// SchemaFilter restricts the `partman.part_config` lookup
	// to parents whose first dotted component matches the
	// filter. Compared via `split_part(parent_table, '.', 1)`
	// so `foo` does NOT accidentally match `foobar.table`.
	// Ignored when ParentTables is non-empty.
	SchemaFilter string

	// MaintenanceInterval is the cron tick for RunMaintenance.
	// Defaults to DefaultMaintenanceInterval (10m) when zero
	// or negative.
	MaintenanceInterval time.Duration

	// MaintenanceTimeout bounds a single RunMaintenance call.
	// Defaults to DefaultMaintenanceTimeout (2m) when zero or
	// negative.
	MaintenanceTimeout time.Duration

	// LagScrapeInterval is the cron tick for ScrapeLag.
	// Defaults to DefaultLagScrapeInterval (1m) when zero or
	// negative.
	LagScrapeInterval time.Duration

	// LagScrapeTimeout bounds a single ScrapeLag call.
	// Defaults to DefaultLagScrapeTimeout (30s) when zero or
	// negative.
	LagScrapeTimeout time.Duration
}

// Service is the long-lived maintainer object the binary hosts.
// All public methods are goroutine-safe (the only mutable state
// is the atomic counters in `metrics`).
type Service struct {
	db     *sql.DB
	cfg    Config
	logger *slog.Logger
	// parentTables is the RESOLVED scope list passed to New.
	// A copy of cfg.ParentTables is taken here so a caller
	// mutating the input slice cannot race with reads inside
	// the run loop.
	parentTables []string
	metrics      *Metrics
}

// New constructs a Service. Panics on a nil *sql.DB (a nil-DB
// Service has no useful behaviour; silently swallowing a
// nil-deref panic would mask the configuration bug). Returns a
// typed error for blank / unqualified ParentTables entries so
// the binary's main can exit with a config-error code.
func New(db *sql.DB, cfg Config, logger *slog.Logger) (*Service, error) {
	if db == nil {
		panic("partitionmaintainer: nil *sql.DB")
	}
	if cfg.MaintenanceInterval <= 0 {
		cfg.MaintenanceInterval = DefaultMaintenanceInterval
	}
	if cfg.MaintenanceTimeout <= 0 {
		cfg.MaintenanceTimeout = DefaultMaintenanceTimeout
	}
	if cfg.LagScrapeInterval <= 0 {
		cfg.LagScrapeInterval = DefaultLagScrapeInterval
	}
	if cfg.LagScrapeTimeout <= 0 {
		cfg.LagScrapeTimeout = DefaultLagScrapeTimeout
	}
	cfg.SchemaFilter = strings.TrimSpace(cfg.SchemaFilter)

	parents := make([]string, 0, len(cfg.ParentTables))
	for i, raw := range cfg.ParentTables {
		p := strings.TrimSpace(raw)
		if p == "" {
			return nil, fmt.Errorf(
				"partitionmaintainer: Config.ParentTables[%d] is blank", i,
			)
		}
		if !strings.Contains(p, ".") {
			return nil, fmt.Errorf(
				"partitionmaintainer: Config.ParentTables[%d] = %q must be schema-qualified (e.g. \"public.episode\")",
				i, p,
			)
		}
		parents = append(parents, p)
	}
	cfg.ParentTables = parents

	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db:           db,
		cfg:          cfg,
		logger:       logger,
		parentTables: append([]string(nil), parents...),
		metrics:      NewMetrics(),
	}, nil
}

// Metrics exposes the package counters / gauge for the binary's
// /metrics endpoint and for integration tests.
func (s *Service) Metrics() *Metrics { return s.metrics }

// Config returns the resolved configuration (with defaults
// substituted in). Read-only; mutations to the returned struct
// are NOT reflected in the Service. ParentTables is a fresh
// slice per call so the caller cannot reach into the Service's
// internal storage.
func (s *Service) Config() Config {
	out := s.cfg
	out.ParentTables = append([]string(nil), s.parentTables...)
	return out
}

// scopedParents resolves the in-scope parent list for the
// current tick. When Config.ParentTables is non-empty we use
// the explicit list verbatim. Otherwise we query
// partman.part_config, optionally narrowed by Config.SchemaFilter.
//
// The lookup uses `split_part(parent_table, '.', 1)` (not LIKE)
// so a filter of `foo` does NOT match `foobar.table` -- a real
// concern in shared dev clusters where multiple agent-memory
// stories' per-test schemas coexist.
//
// Returns an empty (non-nil) slice when no parents match; the
// caller treats that as a no-op rather than an error, but
// surfaces the count via the parents_observed gauge so an
// operator can notice a scope misconfiguration.
func (s *Service) scopedParents(ctx context.Context) ([]string, error) {
	if len(s.parentTables) > 0 {
		out := make([]string, len(s.parentTables))
		copy(out, s.parentTables)
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT parent_table
		  FROM partman.part_config
		 WHERE ($1::text = '' OR split_part(parent_table, '.', 1) = $1::text)
		 ORDER BY parent_table
	`, s.cfg.SchemaFilter)
	if err != nil {
		return nil, fmt.Errorf("partitionmaintainer: select part_config: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var parents []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("partitionmaintainer: scan part_config row: %w", err)
		}
		parents = append(parents, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("partitionmaintainer: iterate part_config rows: %w", err)
	}
	return parents, nil
}

// MaintenanceResult carries the outcome of a single
// RunMaintenance call.
type MaintenanceResult struct {
	// ParentsMaintained is the number of in-scope parents the
	// call iterated `partman.run_maintenance` over. When
	// ParentTables is empty AND SchemaFilter is empty AND the
	// per-parent loop fan-out is skipped (the "cluster-wide"
	// path -- one call with no p_parent_table), this is 0; a
	// non-zero value means the per-parent loop ran for the
	// scoped subset.
	ParentsMaintained int

	// Duration is wall-clock time spent in the partman calls.
	Duration time.Duration
}

// RunMaintenance issues one maintenance pass.
//
// SQL surface
// -----------
// Two execution shapes:
//
//  1. Cluster-wide (Config.ParentTables empty AND
//     Config.SchemaFilter empty): a single
//     `SELECT partman.run_maintenance(p_analyze := false)` call
//     with no parent argument. partman v5 walks every row in
//     `partman.part_config` and provisions / detaches per its
//     configured premake / retention.
//
//  2. Per-parent (scope is restricted): one
//     `SELECT partman.run_maintenance(p_parent_table := $1,
//     p_analyze := false)` call per in-scope parent. We do NOT
//     issue the cluster-wide call in this mode because that
//     would touch out-of-scope parents (every other tenant's
//     per-test schema in a shared CI cluster).
//
// `p_analyze := false` is the documented Stage 8.2 default:
// the maintenance loop should never block the autovacuum
// scheduler with an opportunistic ANALYZE. Operators who want
// ANALYZE after a partition rotation should configure the
// part_config row's `analyze` column directly, not the per-call
// flag.
//
// Concurrency
// -----------
// Safe to call concurrently with ScrapeLag; both read-only
// against partman.part_config and the maintenance call holds
// only the partman advisory lock for its own duration.
// Concurrent RunMaintenance calls against the same parent
// serialise behind the partman advisory lock, so a double-tick
// is harmless but wasteful -- the caller (Run) ensures only one
// in-flight call per parent at any time.
func (s *Service) RunMaintenance(ctx context.Context) (MaintenanceResult, error) {
	s.metrics.IncMaintenanceRuns()

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.MaintenanceTimeout)
	defer cancel()

	start := time.Now()
	res := MaintenanceResult{}

	if len(s.parentTables) == 0 && s.cfg.SchemaFilter == "" {
		// Cluster-wide path. partman v5 walks part_config and
		// maintains every registered parent. p_analyze :=
		// false per the Stage 8.2 implementation-plan note.
		if _, err := s.db.ExecContext(runCtx, `
			SELECT partman.run_maintenance(p_analyze := false)
		`); err != nil {
			s.metrics.IncMaintenanceErrors()
			s.logger.Error("partitionmaintainer.maintenance.query",
				slog.String("scope", "cluster"),
				slog.String("error", err.Error()))
			return res, fmt.Errorf("partitionmaintainer: run_maintenance(cluster): %w", err)
		}
		res.Duration = time.Since(start)
		s.logger.Info("partitionmaintainer.maintenance.done",
			slog.String("scope", "cluster"),
			slog.Duration("duration", res.Duration))
		return res, nil
	}

	parents, err := s.scopedParents(runCtx)
	if err != nil {
		s.metrics.IncMaintenanceErrors()
		s.logger.Error("partitionmaintainer.maintenance.scope",
			slog.String("error", err.Error()))
		return res, err
	}
	for _, parent := range parents {
		if _, err := s.db.ExecContext(runCtx, `
			SELECT partman.run_maintenance(
				p_parent_table := $1::text,
				p_analyze      := false
			)
		`, parent); err != nil {
			s.metrics.IncMaintenanceErrors()
			s.logger.Error("partitionmaintainer.maintenance.query",
				slog.String("scope", "scoped"),
				slog.String("parent", parent),
				slog.String("error", err.Error()))
			return res, fmt.Errorf("partitionmaintainer: run_maintenance(%q): %w", parent, err)
		}
		res.ParentsMaintained++
	}
	res.Duration = time.Since(start)
	s.logger.Info("partitionmaintainer.maintenance.done",
		slog.String("scope", "scoped"),
		slog.Int("parents", res.ParentsMaintained),
		slog.Duration("duration", res.Duration))
	return res, nil
}

// ParentLag is the per-parent decomposition of a single
// ScrapeLag pass, returned to callers (and integration tests)
// that want the breakdown behind the aggregate gauge.
type ParentLag struct {
	// ParentTable is the schema-qualified name.
	ParentTable string
	// LagSeconds is the lag (clamped to >= 0) in whole seconds.
	// Zero means the latest non-default child's end_time is
	// at or beyond `now() + 1 day` -- the premake buffer is
	// healthy for this parent.
	LagSeconds int64
}

// LagResult carries the outcome of a single ScrapeLag pass.
type LagResult struct {
	// MaxLagSeconds is `max(0, max(per-parent lag))` across
	// the in-scope parents -- the value written to the
	// partition_provision_lag gauge.
	MaxLagSeconds int64

	// ParentLags is the per-parent breakdown the aggregate
	// MaxLagSeconds was computed from. Nil when no parents
	// were in scope.
	ParentLags []ParentLag

	// Duration is wall-clock time spent in the lag SQL.
	Duration time.Duration
}

// ScrapeLag computes the `partition_provision_lag`
// gauge and updates the package's Metrics in place.
//
// Per-parent lag SQL
// ------------------
// The query computes, for one parent:
//
//	max(0, EXTRACT(EPOCH FROM (now() + interval '1 day'
//	                            - max(child_end_time))))
//
// where `child_end_time` comes from
// `partman.show_partition_info(child, NULL, parent)`. We filter
// out the DEFAULT partition in a CTE BEFORE the LATERAL call so
// `show_partition_info` is never invoked on a child without a
// well-defined upper bound (which would error or return a
// nonsense end_time on some pg_partman versions).
//
// `to_regclass($1::text)` returns NULL (not an error) when the
// parent has been concurrently dropped between the part_config
// lookup and this query. A NULL inhparent yields zero CTE rows
// → COALESCE(...,0) → 0 lag, which the caller treats as a
// non-issue (a dropped parent contributes nothing to the MAX).
//
// Aggregate
// ---------
// The MAX across in-scope parents is the
// "oldest next-day partition that is missing" per
// implementation-plan §8.2: one parent with a stale forward
// window is sufficient to raise the alert.
func (s *Service) ScrapeLag(ctx context.Context) (LagResult, error) {
	s.metrics.IncLagScrapes()

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.LagScrapeTimeout)
	defer cancel()

	res := LagResult{}
	start := time.Now()

	parents, err := s.scopedParents(runCtx)
	if err != nil {
		s.metrics.IncLagScrapeErrors()
		s.logger.Error("partitionmaintainer.scrape.scope",
			slog.String("error", err.Error()))
		return res, err
	}
	s.metrics.SetParentsObserved(uint64(len(parents)))

	if len(parents) == 0 {
		// No in-scope parents → no observation. Reset the
		// gauge to zero so a previous spike does not linger
		// after the operator narrows the scope to an empty
		// set.
		s.metrics.SetProvisionLagSeconds(0)
		res.Duration = time.Since(start)
		s.logger.Info("partitionmaintainer.scrape.empty_scope",
			slog.Duration("duration", res.Duration))
		return res, nil
	}

	res.ParentLags = make([]ParentLag, 0, len(parents))
	var maxLag int64
	for _, parent := range parents {
		lag, err := s.scrapeParentLag(runCtx, parent)
		if err != nil {
			s.metrics.IncLagScrapeErrors()
			s.logger.Error("partitionmaintainer.scrape.parent",
				slog.String("parent", parent),
				slog.String("error", err.Error()))
			return res, err
		}
		res.ParentLags = append(res.ParentLags, ParentLag{
			ParentTable: parent,
			LagSeconds:  lag,
		})
		if lag > maxLag {
			maxLag = lag
		}
	}
	res.MaxLagSeconds = maxLag
	res.Duration = time.Since(start)

	// Clamp to >= 0 (defensive -- the SQL already does this).
	// Store as uint64; lag is monotonically bounded by the
	// age of the cluster, comfortably under uint64 max.
	if maxLag < 0 {
		maxLag = 0
	}
	s.metrics.SetProvisionLagSeconds(uint64(maxLag))

	s.logger.Info("partitionmaintainer.scrape.done",
		slog.Int("parents", len(parents)),
		slog.Int64("max_lag_seconds", res.MaxLagSeconds),
		slog.Duration("duration", res.Duration))
	return res, nil
}

// scrapeParentLag runs the lag SQL for one parent and returns
// the lag in whole seconds. Returns (0, nil) when the parent has
// no non-default children (no premake yet -> no max to compute);
// the caller treats that as a non-issue because either the
// parent was just created (the migration's partman registration
// will provision children on the next maintenance tick) or it
// was concurrently dropped (a stale row in part_config).
func (s *Service) scrapeParentLag(ctx context.Context, parent string) (int64, error) {
	const lagQuery = `
		WITH children AS (
			SELECT format('%I.%I', n.nspname, c.relname) AS child_table
			  FROM pg_inherits i
			  JOIN pg_class c ON c.oid = i.inhrelid
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE i.inhparent = to_regclass($1::text)
			   AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
		)
		SELECT COALESCE(
			GREATEST(
				0,
				EXTRACT(EPOCH FROM (
					now() + interval '1 day' - max(pi.child_end_time)
				))::bigint
			),
			0
		)::bigint
		  FROM children
		  CROSS JOIN LATERAL partman.show_partition_info(
		      child_table, NULL, $1::text
		  ) pi
	`
	var lag int64
	if err := s.db.QueryRowContext(ctx, lagQuery, parent).Scan(&lag); err != nil {
		// sql.ErrNoRows is impossible here -- the outer SELECT
		// has a COALESCE so a row is always produced even when
		// the CTE is empty. A wrapped error is therefore
		// always a real driver / catalog failure worth
		// surfacing.
		return 0, fmt.Errorf("scrape lag for %q: %w", parent, err)
	}
	if lag < 0 {
		lag = 0
	}
	return lag, nil
}

// Run executes the maintenance + lag-scrape loops. Two goroutines
// share the parent context; either one returning an error from
// `ctx.Err()` (i.e. cancellation) tears the other down.
//
// Per-tick errors are logged at Warn but do NOT stop a loop --
// a transient PostgreSQL hiccup must not orphan partition
// provisioning for 10 minutes or freeze the lag gauge for an
// hour.
//
// Concurrency: Run is safe to launch in its own goroutine.
func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("partitionmaintainer.run.start",
		slog.Int("explicit_parents", len(s.parentTables)),
		slog.String("schema_filter", s.cfg.SchemaFilter),
		slog.Duration("maintenance_interval", s.cfg.MaintenanceInterval),
		slog.Duration("maintenance_timeout", s.cfg.MaintenanceTimeout),
		slog.Duration("lag_scrape_interval", s.cfg.LagScrapeInterval),
		slog.Duration("lag_scrape_timeout", s.cfg.LagScrapeTimeout))

	// Initial sweep so the first observation does not wait one
	// whole interval. Errors are logged but ignored -- the
	// loop continues so a transient startup hiccup doesn't
	// crash the binary.
	if _, err := s.RunMaintenance(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("partitionmaintainer.run.initial_maintenance_failed",
			slog.String("error", err.Error()))
	}
	if _, err := s.ScrapeLag(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("partitionmaintainer.run.initial_scrape_failed",
			slog.String("error", err.Error()))
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		ticker := time.NewTicker(s.cfg.MaintenanceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.RunMaintenance(ctx); err != nil && !errors.Is(err, context.Canceled) {
					s.logger.Warn("partitionmaintainer.run.maintenance_failed",
						slog.String("error", err.Error()))
				}
			}
		}
	}()

	go func() {
		defer wg.Done()
		ticker := time.NewTicker(s.cfg.LagScrapeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.ScrapeLag(ctx); err != nil && !errors.Is(err, context.Canceled) {
					s.logger.Warn("partitionmaintainer.run.scrape_failed",
						slog.String("error", err.Error()))
				}
			}
		}
	}()

	<-ctx.Done()
	wg.Wait()
	s.logger.Info("partitionmaintainer.run.shutdown",
		slog.String("reason", ctx.Err().Error()))
	return ctx.Err()
}
