package tracelogpruner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// boolPtr returns a pointer to b. Helper because Go does not
// allow `&true` literal expressions and Config callers (the
// binary's loadConfig, integration tests) routinely need to
// pass an explicit *bool for KeepTable.
func boolPtr(b bool) *bool { return &b }

// Default parameter values surfaced both as package constants
// (so the binary's loadConfig can reference them in its env-var
// help text) and as the zero-value fallback inside `New`.
const (
	// DefaultRetention is the §8.1 30-day rolling retention
	// window. Operators may override via Config.Retention to
	// the lower bound of 7 days (tech-spec §8.1 "Lower bound:
	// 7 days") or to an arbitrary larger value.
	DefaultRetention = 30 * 24 * time.Hour

	// DefaultRunInterval is the daily-cron tick. Picked once
	// per 24 hours because the partition grain is one week —
	// a finer interval cannot detach a partition that wasn't
	// already detachable on the previous tick.
	DefaultRunInterval = 24 * time.Hour

	// DefaultPruneTimeout bounds a single Prune invocation so
	// a stuck ALTER TABLE ... DETACH PARTITION cannot stall
	// the daily loop indefinitely. Generous enough that a
	// large multi-partition catch-up after a long binary
	// outage still completes; tight enough that an operator
	// notices a regression within one tick. Override via
	// Config.PruneTimeout.
	DefaultPruneTimeout = 5 * time.Minute
)

// Config is the env-derived (or programmatic) configuration the
// Service consumes. Construct via `Config{...}` literal and
// pass to `New`; missing optional fields fall back to the
// corresponding Default* constant.
type Config struct {
	// ParentTable is the schema-qualified parent table name
	// (e.g. "public.trace_observation_log"). REQUIRED.
	//
	// Why qualified: pg_partman stores `parent_table` in
	// `partman.part_config` as `schema.table` and looks rows
	// up by literal string match. Passing only
	// "trace_observation_log" would silently miss the
	// configuration row and `drop_partition_time` would error
	// out with "no part_config row found for parent table",
	// which the migration runner's per-test schema makes
	// load-bearing (the test schema is random).
	ParentTable string

	// Retention is the rolling window threshold. Partitions
	// whose upper bound is older than `NOW() - Retention` are
	// detached. Zero or negative values fall back to
	// DefaultRetention (30 days).
	Retention time.Duration

	// KeepTable controls whether `partman.drop_partition_time`
	// detaches the partition (true) or both detaches and DROPs
	// the standalone table (false).
	//
	// Default true per the Stage 4.3 implementation-plan brief
	// ("detach partitions older than the §8.1 30-day retention
	// window"); the detached child remains accessible as a
	// standalone table for one-off forensic queries until an
	// out-of-band operator job archives or drops it.
	//
	// Encoded as `*bool` (not `bool`) so `New` can distinguish
	// "omitted, default to true" from "explicitly false".
	// A plain `bool` zero-value would silently DROP partitions
	// for callers that constructed `Config{ParentTable: ...}`
	// without thinking about retention semantics. Use the
	// package-private `boolPtr(true|false)` helper at call
	// sites, or leave nil to accept the safe default.
	KeepTable *bool

	// RunInterval is the cron tick. Defaults to
	// DefaultRunInterval (24h) when zero or negative.
	RunInterval time.Duration

	// PruneTimeout is the per-invocation timeout. Defaults to
	// DefaultPruneTimeout (5m) when zero or negative.
	PruneTimeout time.Duration
}

// Service is the long-lived pruner object the binary hosts.
// All public methods are goroutine-safe (the only mutable state
// is the atomic counters in `metrics`).
type Service struct {
	db     *sql.DB
	cfg    Config
	logger *slog.Logger
	// keepTable is the RESOLVED KeepTable value (with default
	// applied). Stored as a non-pointer field so a caller
	// mutating `*Config().KeepTable` after construction cannot
	// reach into the Service's runtime state — that would
	// violate the documented "mutations to the returned struct
	// are NOT reflected in the Service" contract and create a
	// goroutine-unsafe edge.
	keepTable bool
	metrics   *Metrics
}

// New constructs a Service. Panics on a nil *sql.DB (a
// nil-DB Service has no useful behaviour and silently swallows
// nil-deref panics would mask the configuration bug).
//
// A blank or trailing-/leading-whitespace ParentTable is
// rejected with an error rather than panicked because the env
// loader in `cmd/trace-log-pruner` cannot validate it until
// after construction; we surface a typed error so the binary's
// main can exit with a config-error code rather than a
// goroutine panic stack.
func New(db *sql.DB, cfg Config, logger *slog.Logger) (*Service, error) {
	if db == nil {
		panic("tracelogpruner: nil *sql.DB")
	}
	parent := strings.TrimSpace(cfg.ParentTable)
	if parent == "" {
		return nil, errors.New("tracelogpruner: Config.ParentTable is required")
	}
	if !strings.Contains(parent, ".") {
		return nil, fmt.Errorf(
			"tracelogpruner: Config.ParentTable %q must be schema-qualified (e.g. \"public.trace_observation_log\")",
			parent,
		)
	}
	cfg.ParentTable = parent
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.RunInterval <= 0 {
		cfg.RunInterval = DefaultRunInterval
	}
	if cfg.PruneTimeout <= 0 {
		cfg.PruneTimeout = DefaultPruneTimeout
	}
	// Resolve KeepTable: nil → default true (detach only, the
	// safe §8.1 default), non-nil → use the explicit value.
	// Re-point cfg.KeepTable at our own bool so a later caller
	// mutation of the input pointer cannot race with the
	// runtime read of `s.keepTable`.
	keepTable := true
	if cfg.KeepTable != nil {
		keepTable = *cfg.KeepTable
	}
	cfg.KeepTable = boolPtr(keepTable)
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db:        db,
		cfg:       cfg,
		logger:    logger,
		keepTable: keepTable,
		metrics:   NewMetrics(),
	}, nil
}

// Metrics exposes the package counters for the binary's
// /metrics endpoint and for integration tests.
func (s *Service) Metrics() *Metrics {
	return s.metrics
}

// Config returns the resolved configuration (with defaults
// substituted in). Read-only; mutations to the returned struct
// are NOT reflected in the Service. The returned KeepTable
// pointer is freshly allocated per call so the caller cannot
// reach into the Service's internal storage via the *bool.
func (s *Service) Config() Config {
	out := s.cfg
	out.KeepTable = boolPtr(s.keepTable)
	return out
}

// PruneResult carries the outcome of a single Prune call.
type PruneResult struct {
	// PartitionsDropped is the count of partitions
	// `partman.drop_partition_time` reported as dropped or
	// detached on this run. Mirrors the function's integer
	// return value. May be zero on a healthy day where no
	// partition has fallen out of the retention window yet.
	//
	// Note: pg_partman v5's `drop_partition_time` is declared
	// `RETURNS int` (count of partitions detached or dropped).
	// The function does NOT surface partition names — callers
	// that need the exact relname of a detached child must
	// query `pg_inherits` before and after Prune. The
	// integration test does exactly that via
	// `partitionIsChild`.
	PartitionsDropped int

	// Duration is wall-clock time spent in the pgpartman call.
	Duration time.Duration
}

// Prune runs a single retention pass. Returns the PruneResult
// even on error so a partial-progress run can be diagnosed; the
// caller MUST check the error before treating the result as
// authoritative.
//
// SQL surface
// -----------
// The retention argument is bound as a PostgreSQL interval
// LITERAL (text → ::interval cast inside the SQL) rather than
// as a typed `time.Duration`. lib/pq encodes `time.Duration` as
// a bigint nanosecond count, and `bigint::interval` is not a
// valid cast — the call would surface as a SQLSTATE 42846
// error. Formatting the interval as a `"<seconds> seconds"`
// string and casting to interval is the documented portable
// shape and works against any PostgreSQL ≥ 9.x with any
// pg-driver encoding.
//
// The boolean `p_keep_table` is bound as a plain $3 parameter;
// lib/pq encodes Go bool as PostgreSQL bool natively.
//
// Return-value contract
// ---------------------
// pg_partman v5's `drop_partition_time` is declared
// `RETURNS int` — a SINGLE integer carrying the count of
// partitions detached or dropped on this invocation. We scan it
// with `QueryRowContext(...).Scan(&n)` and increment the
// `trace_log_partitions_dropped_total` counter by exactly that
// `n` so a healthy no-op day (n=0) does NOT bump the counter
// and a multi-partition catch-up day (n>1) bumps by the true
// count. This matches Prometheus counter semantics.
//
// The previous iteration mistakenly treated the return as
// `SETOF text` (partition names); pg_partman v4 used a void
// return + ROW per dropped partition in some forks, but v5 is
// uniformly `int` per the upstream source
// (sql/functions/drop_partition_time.sql).
func (s *Service) Prune(ctx context.Context) (PruneResult, error) {
	s.metrics.IncRuns()

	res := PruneResult{}
	runCtx, cancel := context.WithTimeout(ctx, s.cfg.PruneTimeout)
	defer cancel()

	// Render the retention as a PostgreSQL interval literal.
	// Using seconds preserves sub-day overrides operators may
	// supply (lower bound is 7 days per §8.1 but the unit-test
	// suite supplies sub-second values to exercise edge cases).
	// Float precision is sufficient: 30 days = 2,592,000 s; a
	// float64 mantissa is 53 bits ~ 9×10^15 — comfortably
	// exact for any operationally sensible retention.
	retentionLiteral := fmt.Sprintf("%.6f seconds", s.cfg.Retention.Seconds())

	start := time.Now()
	var dropped int
	err := s.db.QueryRowContext(runCtx, `
		SELECT partman.drop_partition_time(
			p_parent_table := $1::text,
			p_retention    := $2::interval,
			p_keep_table   := $3::boolean
		)
	`, s.cfg.ParentTable, retentionLiteral, s.keepTable).Scan(&dropped)
	if err != nil {
		s.metrics.IncErrors()
		s.logger.Error("tracelogpruner.prune.query",
			slog.String("parent_table", s.cfg.ParentTable),
			slog.String("retention", retentionLiteral),
			slog.Bool("keep_table", s.keepTable),
			slog.String("error", err.Error()))
		return res, fmt.Errorf("tracelogpruner: drop_partition_time: %w", err)
	}
	if dropped < 0 {
		// Defensive: pg_partman never returns negative counts
		// but a future regression there must not feed a
		// uint64 underflow into the metric. Treat as zero and
		// surface in logs.
		s.logger.Warn("tracelogpruner.prune.negative_count",
			slog.String("parent_table", s.cfg.ParentTable),
			slog.Int("dropped", dropped))
		dropped = 0
	}

	res.PartitionsDropped = dropped
	res.Duration = time.Since(start)
	// Increment the Prometheus counter by exactly the returned
	// count: 0 → +0 (no-op), N → +N (catch-up). This is the
	// Stage 4.3 metric contract.
	if dropped > 0 {
		s.metrics.IncPartitionsDropped(uint64(dropped))
	}

	s.logger.Info("tracelogpruner.prune.done",
		slog.String("parent_table", s.cfg.ParentTable),
		slog.String("retention", retentionLiteral),
		slog.Bool("keep_table", s.keepTable),
		slog.Int("partitions_dropped", res.PartitionsDropped),
		slog.Duration("duration", res.Duration))
	return res, nil
}

// Run executes the daily-cron loop. Runs once immediately so
// a fresh deploy or a restart after a long outage catches up
// before the first tick, then on `Config.RunInterval` ticks
// thereafter.
//
// Per-tick errors are logged at Warn but do NOT stop the loop —
// a transient PostgreSQL hiccup must not orphan retention for
// 24 hours. The loop exits only on `ctx` cancellation
// (returning `ctx.Err()`, which for an SIGINT-triggered cancel
// is `context.Canceled` — the binary's main treats that as a
// graceful shutdown).
//
// Concurrency: Run is safe to launch in its own goroutine. It
// holds no external state beyond the Service it was called on,
// and the underlying *sql.DB is itself thread-safe.
func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("tracelogpruner.run.start",
		slog.String("parent_table", s.cfg.ParentTable),
		slog.Duration("retention", s.cfg.Retention),
		slog.Duration("run_interval", s.cfg.RunInterval),
		slog.Duration("prune_timeout", s.cfg.PruneTimeout),
		slog.Bool("keep_table", s.keepTable))

	// Initial sweep so the first detach does not wait an
	// entire RunInterval. Errors are logged but ignored — the
	// loop continues so a transient startup hiccup doesn't
	// crash the binary.
	if _, err := s.Prune(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("tracelogpruner.run.initial_prune_failed",
			slog.String("error", err.Error()))
	}

	ticker := time.NewTicker(s.cfg.RunInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("tracelogpruner.run.shutdown",
				slog.String("reason", ctx.Err().Error()))
			return ctx.Err()
		case <-ticker.C:
			if _, err := s.Prune(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("tracelogpruner.run.prune_failed",
					slog.String("error", err.Error()))
			}
		}
	}
}
