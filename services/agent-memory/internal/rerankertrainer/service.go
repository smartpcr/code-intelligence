package rerankertrainer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Default parameter values surfaced both as package constants
// (so the binary's loadConfig can reference them in env-var
// help text) and as the zero-value fallback inside `New`.
const (
	// DefaultRunInterval is the nightly cadence per
	// tech-spec §8.4 line 502 ("nightly cron job").
	DefaultRunInterval = 24 * time.Hour

	// DefaultTickTimeout bounds a single Tick. Generous
	// enough that a full 90-day pair pull completes on a
	// busy dev cluster; tight enough that a stuck query
	// gets noticed.
	DefaultTickTimeout = 30 * time.Minute

	// DefaultTrainingWindow is the trailing window for
	// non-synthetic Episodes per tech-spec §8.4 line 506.
	DefaultTrainingWindow = 90 * 24 * time.Hour

	// DefaultMinEpisodes is the floor below which the
	// trainer SKIPs publication. tech-spec / impl-plan are
	// silent on a specific number; we pick a low default
	// that lets dev/CI environments still publish after
	// modest seeding, while preventing publication of a
	// trivially-small (e.g. 5-pair) training set.
	DefaultMinEpisodes = 50

	// DefaultGrowthThreshold is the on-demand wake gate
	// per implementation-plan §6.4 line 1123: "An on-demand
	// path: if labelled-Episode count has grown by ≥ 5%
	// since the last run, kick off training". 0.05 == 5%.
	DefaultGrowthThreshold = 0.05

	// DefaultGrowthCheckInterval is the cadence at which
	// Run() polls the labelled-Episode count to decide
	// whether to fire an early on-demand tick.
	DefaultGrowthCheckInterval = 1 * time.Hour

	// DefaultActorCapPerWindow is the §9.4 mitigation: per
	// the risk register "rate-limit Episodes labeled
	// 'human_corrected' per operator per hour". 50 is a
	// conservative default; operators tune via env.
	DefaultActorCapPerWindow = 50

	// DefaultActorCapWindow is the sliding window the
	// per-actor cap operates over. 1h matches the §9.4
	// wording ("per operator per hour").
	DefaultActorCapWindow = 1 * time.Hour

	// RerankerTrainerAdvisoryLockKey is the cluster-wide
	// bigint pg_try_advisory_lock key the Reranker Trainer
	// uses to serialise the train-publish phase across
	// replicas. The numeric value is the big-endian ASCII
	// encoding of "RRNKTRNR" -- a `grep -F "RRNKTRNR"`
	// finds every reference, and the value is reproducible
	// from the literal bytes via
	//   printf 'RRNKTRNR' | od -An -tx1 | tr -d ' '
	// yielding 0x52524E4B54524E52.
	//
	// 0x52 0x52 0x4E 0x4B 0x54 0x52 0x4E 0x52 = "RRNKTRNR"
	//
	// Distinct from the Consolidator
	// (ConsolidatorAdvisoryLockKey = 0x434F4E534F4C4944
	// "CONSOLID") and the testpglock AppRoleLoginKey
	// ("AGNTMEM1") / RoRoleLoginKey ("AGNTMEM2") so a
	// concurrent worker cannot block this lock and vice-
	// versa.
	RerankerTrainerAdvisoryLockKey int64 = 0x52524E4B54524E52
)

// Config is the env-derived (or programmatic) configuration
// the Service consumes. Construct via Config{...} literal and
// pass to `New`; missing optional fields fall back to the
// corresponding Default* constant.
type Config struct {
	// RunInterval is the nightly long-poll cadence. Zero
	// or negative falls back to DefaultRunInterval (24h).
	RunInterval time.Duration

	// TickTimeout bounds a single Tick. Zero or negative
	// falls back to DefaultTickTimeout (30m).
	TickTimeout time.Duration

	// TrainingWindow is the trailing Episode window. Zero
	// or negative falls back to DefaultTrainingWindow (90d).
	TrainingWindow time.Duration

	// MinEpisodes is the post-cap floor below which the
	// trainer skips publication. Zero or negative falls
	// back to DefaultMinEpisodes.
	MinEpisodes int

	// GrowthThreshold is the on-demand wake fraction
	// (0.05 == 5%) per impl-plan §6.4 line 1123. Zero or
	// negative disables the on-demand wake entirely.
	GrowthThreshold float64

	// GrowthCheckInterval is the cadence at which Run()
	// polls the labelled-Episode count to decide whether
	// the GrowthThreshold has been crossed. Ignored when
	// GrowthThreshold <= 0.
	GrowthCheckInterval time.Duration

	// ActorCapPerWindow is the §9.4 cap. Zero or negative
	// disables the cap (every correction-derived pair
	// passes through).
	ActorCapPerWindow int

	// ActorCapWindow is the §9.4 sliding window. Zero or
	// negative falls back to DefaultActorCapWindow (1h).
	ActorCapWindow time.Duration

	// AdvisoryLockKey is the bigint key for cross-replica
	// serialisation. Zero falls back to
	// RerankerTrainerAdvisoryLockKey.
	AdvisoryLockKey int64

	// AllowNoopPublish governs whether the in-process
	// NoopTrainer's output -- which carries Status="shadow"
	// by default -- may be force-elevated to "published".
	// Default false: the noop trainer NEVER produces
	// published rows so the §9.10 staleness gate keeps
	// firing on the last REAL published model. Set true
	// only in dev/CI environments that need a published row
	// to exercise downstream code paths.
	AllowNoopPublish bool
}

// Service is the long-lived reranker-trainer object the
// binary hosts. All public methods are goroutine-safe.
type Service struct {
	db      *sql.DB
	cfg     Config
	trainer Trainer
	logger  *slog.Logger
	metrics *Metrics

	// growthBaseline is the labelled-Episode count
	// captured at the most recent successful publish.
	// Read by the on-demand wake check; written by Tick on
	// the success path. Protected by the advisory lock
	// (the Tick that updates it holds the lock).
	growthBaseline int64
}

// New constructs a Service. Panics on a nil *sql.DB OR a nil
// Trainer (both indicate a configuration bug that silently
// no-op'ing would mask).
func New(db *sql.DB, cfg Config, trainer Trainer, logger *slog.Logger) (*Service, error) {
	if db == nil {
		panic("rerankertrainer: nil *sql.DB")
	}
	if trainer == nil {
		panic("rerankertrainer: nil Trainer (pass NoopTrainer{} for dev/test)")
	}
	if cfg.RunInterval <= 0 {
		cfg.RunInterval = DefaultRunInterval
	}
	if cfg.TickTimeout <= 0 {
		cfg.TickTimeout = DefaultTickTimeout
	}
	if cfg.TrainingWindow <= 0 {
		cfg.TrainingWindow = DefaultTrainingWindow
	}
	if cfg.MinEpisodes <= 0 {
		cfg.MinEpisodes = DefaultMinEpisodes
	}
	if cfg.GrowthThreshold < 0 {
		cfg.GrowthThreshold = 0
	}
	if cfg.GrowthCheckInterval <= 0 {
		cfg.GrowthCheckInterval = DefaultGrowthCheckInterval
	}
	if cfg.ActorCapPerWindow < 0 {
		cfg.ActorCapPerWindow = 0
	}
	if cfg.ActorCapWindow <= 0 {
		cfg.ActorCapWindow = DefaultActorCapWindow
	}
	if cfg.AdvisoryLockKey == 0 {
		cfg.AdvisoryLockKey = RerankerTrainerAdvisoryLockKey
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db:      db,
		cfg:     cfg,
		trainer: trainer,
		logger:  logger,
		metrics: NewMetrics(),
	}, nil
}

// Metrics exposes the package counters for the binary's
// /metrics endpoint and for integration tests.
func (s *Service) Metrics() *Metrics { return s.metrics }

// Config returns the resolved configuration (with defaults
// substituted in). Read-only; mutations to the returned struct
// are NOT reflected in the Service.
func (s *Service) Config() Config { return s.cfg }

// TickResult carries the outcome of a single Tick call.
type TickResult struct {
	// Published is the reranker_model row published by
	// this tick. Empty when the tick was a no-op
	// (lock-skipped OR below MinEpisodes OR duplicate
	// fingerprint).
	Published *PublishedModel

	// LockSkipped is true when pg_try_advisory_lock
	// returned false (another replica holds the lock).
	LockSkipped bool

	// BelowMinEpisodes is true when the post-cap labelled-
	// pair count fell below cfg.MinEpisodes so no publish
	// happened.
	BelowMinEpisodes bool

	// DuplicateVersion is true when the trainer returned a
	// fingerprint that already exists in reranker_model
	// (an idempotent retry over identical input). The
	// row is NOT re-inserted; the metric counter is NOT
	// bumped.
	DuplicateVersion bool

	// Positives / Negatives are the post-cap pair counts
	// that fed the training pass.
	Positives int
	Negatives int

	// CappedActors maps actor enum -> dropped count.
	CappedActors map[string]uint64
}

// PublishedModel mirrors the inserted `reranker_model` row.
type PublishedModel struct {
	ModelID     string
	Version     string
	ArtifactURI string
	TrainedAt   time.Time
	Status      string
}

// Tick runs ONE training pass. The lifecycle is:
//
//  1. Acquire pg_try_advisory_lock on a pinned conn. On lock
//     miss the tick is a no-op (LockSkipped=true).
//
//  2. PullPairs across the training window (90d default) for
//     EVERY pair selector -- positives, synthetic positives,
//     negatives, AND parent-of-synthetic-positive all apply
//     the trailing-window predicate (iter-3 review item 5 closed
//     the prior all-time synthetic-positive drift). Then applies
//     the per-actor COMBINED cap across positives and negatives
//     (iter-3 review item 4 closed the prior per-bucket shape).
//
//  3. If the post-cap pair count is below MinEpisodes, skip
//     publication and exit (BelowMinEpisodes=true). The metric
//     `reranker_episodes_below_min_total` is bumped so the
//     operator can correlate sparse-supervision skips against
//     the alert.
//
//  4. Call Trainer.Train. The trainer returns a deterministic
//     TrainingOutput.Version derived from the input
//     fingerprint.
//
//  5. INSERT reranker_model row with the trainer's output. The
//     INSERT uses `ON CONFLICT (version) DO NOTHING` so an
//     idempotent retry over identical input collapses to a
//     no-op (DuplicateVersion=true).
//
//  6. Release the advisory lock.
//
// All errors propagate out and bump `reranker_errors_total`.
// The advisory lock is released by the deferred unlock
// regardless of error path.
func (s *Service) Tick(ctx context.Context) (TickResult, error) {
	s.metrics.IncRuns()

	tickCtx, cancel := context.WithTimeout(ctx, s.cfg.TickTimeout)
	defer cancel()

	result := TickResult{}

	conn, err := s.db.Conn(tickCtx)
	if err != nil {
		s.metrics.IncErrors()
		return result, fmt.Errorf("rerankertrainer: pin conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var locked bool
	if err := conn.QueryRowContext(tickCtx,
		`SELECT pg_try_advisory_lock($1)`, s.cfg.AdvisoryLockKey,
	).Scan(&locked); err != nil {
		s.metrics.IncErrors()
		return result, fmt.Errorf("rerankertrainer: try advisory lock: %w", err)
	}
	if !locked {
		s.metrics.IncLockSkipped()
		result.LockSkipped = true
		s.logger.Info("rerankertrainer.tick.lock_skipped",
			slog.Int64("lock_key", s.cfg.AdvisoryLockKey))
		return result, nil
	}
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer unlockCancel()
		if _, uerr := conn.ExecContext(unlockCtx,
			`SELECT pg_advisory_unlock($1)`, s.cfg.AdvisoryLockKey,
		); uerr != nil {
			s.logger.Warn("rerankertrainer.advisory_unlock_failed",
				slog.String("error", uerr.Error()))
		}
	}()

	now := time.Now().UTC()
	pull, err := PullPairs(tickCtx, s.db, PullOpts{
		Now:               now,
		Window:            s.cfg.TrainingWindow,
		ActorCapPerWindow: s.cfg.ActorCapPerWindow,
		ActorCapWindow:    s.cfg.ActorCapWindow,
	})
	if err != nil {
		s.metrics.IncErrors()
		return result, fmt.Errorf("rerankertrainer: pull pairs: %w", err)
	}
	result.Positives = len(pull.Positives)
	result.Negatives = len(pull.Negatives)
	result.CappedActors = pull.CappedActors

	for actor, n := range pull.CappedActors {
		s.metrics.AddCappedActor(actor, n)
	}
	s.metrics.AddPositivePairs(uint64(len(pull.Positives)))
	s.metrics.AddNegativePairs(uint64(len(pull.Negatives)))

	totalPairs := len(pull.Positives) + len(pull.Negatives)
	if totalPairs < s.cfg.MinEpisodes {
		s.metrics.IncEpisodesBelowMin()
		result.BelowMinEpisodes = true
		s.logger.Info("rerankertrainer.tick.below_min",
			slog.Int("total_pairs", totalPairs),
			slog.Int("min_episodes", s.cfg.MinEpisodes))
		return result, nil
	}

	windowStart := now.Add(-s.cfg.TrainingWindow)
	in := TrainingInput{
		Positives:   pull.Positives,
		Negatives:   pull.Negatives,
		WindowStart: windowStart,
		WindowEnd:   now,
		TrainerTag:  s.trainer.Tag(),
	}
	out, err := s.trainer.Train(tickCtx, in)
	if err != nil {
		s.metrics.IncErrors()
		return result, fmt.Errorf("rerankertrainer: train: %w", err)
	}
	if out.Version == "" {
		// Trainer must always return a version per the
		// Trainer contract; an empty value would defeat
		// the UNIQUE-version idempotency guarantee.
		s.metrics.IncErrors()
		return result, errors.New("rerankertrainer: trainer returned empty Version")
	}
	status := out.PublishStatus
	if status == "" {
		status = StatusShadow
	}
	if s.trainer.Tag() == "noop" {
		// The in-process NoopTrainer carries no real
		// learned signal, so by default its output sits in
		// `shadow` -- preserving the §9.10 staleness gate
		// on the last REAL published model. Operators who
		// need an end-to-end dev/CI flow can set
		// AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH=true to
		// force-elevate every noop row to `published`.
		// Documentation contract: when AllowNoopPublish is
		// true the binary publishes; when false it stays
		// shadow regardless of what the trainer returned.
		// This is the single source of truth for the
		// AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH semantics.
		if s.cfg.AllowNoopPublish {
			status = StatusPublished
		} else {
			status = StatusShadow
		}
	}

	// Iter-20 evaluator item 3: closed-set gate on the
	// resolved status BEFORE it lands in the
	// `reranker_model.status` text column. The validator
	// rejects anything outside {published, shadow}, so a
	// non-noop trainer that returns e.g. `publish_status:
	// "publsihed"` (typo) fails loudly here instead of
	// silently writing an unconsumed row. The noop branch
	// above can only assign `StatusShadow` or
	// `StatusPublished`, so for noop this is a defensive
	// no-op; for real trainers (sidecar / linear) this is
	// the load-bearing gate.
	if err := ValidatePublishStatus(status); err != nil {
		s.metrics.IncErrors()
		return result, fmt.Errorf("rerankertrainer: invalid resolved status: %w", err)
	}

	published, dup, err := s.publish(tickCtx, out, status, now)
	if err != nil {
		s.metrics.IncErrors()
		return result, fmt.Errorf("rerankertrainer: publish: %w", err)
	}
	if dup {
		result.DuplicateVersion = true
		s.logger.Info("rerankertrainer.tick.duplicate_version",
			slog.String("version", out.Version))
		return result, nil
	}

	s.metrics.IncModelsPublished()
	s.metrics.SetLastTrainedAt(published.TrainedAt)
	result.Published = &published

	if base, err := s.labelledEpisodeCount(tickCtx); err == nil {
		s.growthBaseline = base
	}

	s.logger.Info("rerankertrainer.tick.published",
		slog.String("model_id", published.ModelID),
		slog.String("version", published.Version),
		slog.String("status", published.Status),
		slog.Time("trained_at", published.TrainedAt),
		slog.Int("positives", len(pull.Positives)),
		slog.Int("negatives", len(pull.Negatives)))
	return result, nil
}

// publish INSERTs the reranker_model row. Returns dup=true
// when the version already exists (idempotent retry); err is
// non-nil on a real failure.
//
// Iter-20 evaluator item 3: validates `status` against the
// closed set `{published, shadow}` before issuing the INSERT.
// The `reranker_model.status` column is `text` in migration
// 0012_run_tables.sql (no CHECK constraint), so application
// code is the only gate against typos -- this last-line check
// catches bugs in any caller that bypasses the
// `Service.Tick` resolution above.
func (s *Service) publish(ctx context.Context, out TrainingOutput, status string, now time.Time) (PublishedModel, bool, error) {
	if err := ValidatePublishStatus(status); err != nil {
		return PublishedModel{}, false, fmt.Errorf("publish: %w", err)
	}
	metricsJSON, err := MetricsJSON(out.Metrics)
	if err != nil {
		return PublishedModel{}, false, err
	}
	const q = `
		INSERT INTO reranker_model
		    (version, artifact_uri, trained_at, metrics_json, status)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		ON CONFLICT (version) DO NOTHING
		RETURNING model_id::text, trained_at
	`
	var (
		modelID   string
		trainedAt time.Time
	)
	err = s.db.QueryRowContext(ctx, q,
		out.Version, out.ArtifactURI, now, metricsJSON, status,
	).Scan(&modelID, &trainedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// ON CONFLICT fired -- duplicate version.
		return PublishedModel{Version: out.Version}, true, nil
	}
	if err != nil {
		return PublishedModel{}, false, err
	}
	return PublishedModel{
		ModelID:     modelID,
		Version:     out.Version,
		ArtifactURI: out.ArtifactURI,
		TrainedAt:   trainedAt.UTC(),
		Status:      status,
	}, false, nil
}

// labelledEpisodeCount returns the count of Episodes the next
// Tick would pull, scoped to the SAME trailing window
// (`cfg.TrainingWindow`) that PullPairs uses. Used as the
// on-demand wake baseline so the growth trigger compares
// like-with-like instead of counting all-time history (which
// would bury fresh corrections under stale ones).
//
// Includes (all within the trailing TrainingWindow):
//   - kind='synthetic_positive' (positive selector arm 1)
//   - kind='agent' AND outcome IN ('success','failure','degraded')
//     (positive selector arm 2 + negative selector arms 1-2)
//   - parents of synthetic_positives whose synthetic positive
//     landed inside the window (negative selector arm 3)
//
// Counted at the DB layer to avoid pulling the full pair list.
func (s *Service) labelledEpisodeCount(ctx context.Context) (int64, error) {
	const q = `
		WITH windowed_sp AS (
		    SELECT episode_id, synthesized_from_parent_episode_id
		      FROM episode
		     WHERE kind = 'synthetic_positive'
		       AND created_at >= now() - $1::interval
		),
		cand AS (
		    SELECT episode_id FROM windowed_sp
		    UNION
		    SELECT episode_id FROM episode
		     WHERE kind = 'agent'
		       AND outcome IN ('success', 'failure', 'degraded')
		       AND created_at >= now() - $1::interval
		    UNION
		    SELECT parent.episode_id
		      FROM windowed_sp sp
		      JOIN episode parent
		        ON parent.episode_id = sp.synthesized_from_parent_episode_id
		)
		SELECT count(*) FROM cand
	`
	// pq's interval parameter wants a string like "90 days".
	interval := pgIntervalString(s.cfg.TrainingWindow)
	var n int64
	if err := s.db.QueryRowContext(ctx, q, interval).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// pgIntervalString formats a Go duration as a PostgreSQL
// interval literal in whole-seconds. PostgreSQL accepts the
// implicit `"N seconds"` form universally; we use seconds to
// avoid rounding surprises on durations that are not whole
// days.
func pgIntervalString(d time.Duration) string {
	if d <= 0 {
		return "0 seconds"
	}
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}

// LatestPublishedTrainedAt reads the `trained_at` timestamp of
// the most recent `status='published'` row from
// `reranker_model`. Returns `(t, true, nil)` on a hit,
// `(_, false, nil)` when no row exists, and `(_, false, err)`
// on a DB failure.
//
// Exposed so the agent-api binary can wire this method as a
// `RerankerFreshnessSource` consumed by the recall verb's
// §9.10 staleness gate without duplicating the SQL. Lives on
// the package (not the Service) because the freshness check
// is stateless and the recall hot path benefits from not
// going through the Service's tick-budget timeout.
func LatestPublishedTrainedAt(ctx context.Context, db *sql.DB) (time.Time, bool, error) {
	const q = `
		SELECT trained_at
		FROM reranker_model
		WHERE status = 'published'
		ORDER BY trained_at DESC
		LIMIT 1
	`
	var t time.Time
	err := db.QueryRowContext(ctx, q).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return t.UTC(), true, nil
}

// LatestPublishedVersion reads the `version` of the most
// recent `status='published'` row. Used by the agent-api
// binary's reranker wrapper so RecallResponse.RerankerModelVersion
// surfaces the latest published trainer output once one
// exists; falls back to the cold-start literal when no
// published row exists yet.
func LatestPublishedVersion(ctx context.Context, db *sql.DB) (string, bool, error) {
	const q = `
		SELECT version
		FROM reranker_model
		WHERE status = 'published'
		ORDER BY trained_at DESC
		LIMIT 1
	`
	var v string
	err := db.QueryRowContext(ctx, q).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// PublishedArtifact bundles the three fields the recall path
// needs to consume the trained reranker on every request:
// the version (advertised on the response envelope), the
// artifact URI (decoded to score Candidates), and the
// trained-at timestamp (drives the §9.10 staleness gate).
//
// One struct + one SELECT instead of three separate helpers
// keeps the per-request DB cost at exactly one row read per
// recall (impl-plan §1115: "GraphReader reads the latest
// published version on every request").
type PublishedArtifact struct {
	Version     string
	ArtifactURI string
	TrainedAt   time.Time
	Status      string
}

// LatestPublishedArtifact reads (version, artifact_uri,
// trained_at) for the most recent `status='published'` row.
// Returns `(_, false, nil)` when no published row exists so
// the recall wrapper can fall back to the cold-start
// reranker. Returns `(_, false, err)` only on genuine DB
// outages.
//
// The recall hot path calls this on EVERY request. Backed by
// a single-row SELECT against the `idx_reranker_model_status_trained_at`
// partial index (migration 0006), so the lookup is O(1) at
// the storage layer. No caching at this layer — the §6.4
// brief requires every recall to see the latest publish.
func LatestPublishedArtifact(ctx context.Context, db *sql.DB) (PublishedArtifact, bool, error) {
	const q = `
		SELECT version, artifact_uri, trained_at, status::text
		FROM reranker_model
		WHERE status = 'published'
		ORDER BY trained_at DESC
		LIMIT 1
	`
	var a PublishedArtifact
	err := db.QueryRowContext(ctx, q).Scan(&a.Version, &a.ArtifactURI, &a.TrainedAt, &a.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedArtifact{}, false, nil
	}
	if err != nil {
		return PublishedArtifact{}, false, err
	}
	a.TrainedAt = a.TrainedAt.UTC()
	return a, true, nil
}

// Run executes the poll loop. Runs Tick once immediately so a
// fresh deploy does not have to wait a full interval before
// the first sweep, then on either:
//   - the nightly long-poll ticker (cfg.RunInterval), OR
//   - the on-demand growth-fraction check (when
//     cfg.GrowthThreshold > 0): every GrowthCheckInterval, query
//     the labelled-Episode count; if it has grown by ≥
//     GrowthThreshold (e.g. 5%) since the last published baseline,
//     fire a tick AND reset the long-poll ticker.
//
// Per-tick errors are logged but do NOT stop the loop -- a
// transient PostgreSQL hiccup must not orphan training until
// the binary restarts. The loop exits only on ctx
// cancellation.
func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("rerankertrainer.run.start",
		slog.Duration("run_interval", s.cfg.RunInterval),
		slog.Duration("tick_timeout", s.cfg.TickTimeout),
		slog.Duration("training_window", s.cfg.TrainingWindow),
		slog.Int("min_episodes", s.cfg.MinEpisodes),
		slog.Float64("growth_threshold", s.cfg.GrowthThreshold),
		slog.Duration("growth_check_interval", s.cfg.GrowthCheckInterval),
		slog.Int("actor_cap_per_window", s.cfg.ActorCapPerWindow),
		slog.Duration("actor_cap_window", s.cfg.ActorCapWindow),
		slog.Int64("advisory_lock_key", s.cfg.AdvisoryLockKey),
		slog.Bool("allow_noop_publish", s.cfg.AllowNoopPublish),
		slog.String("trainer_tag", s.trainer.Tag()))

	if _, err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("rerankertrainer.run.initial_tick_failed",
			slog.String("error", err.Error()))
	}

	intervalTicker := time.NewTicker(s.cfg.RunInterval)
	defer intervalTicker.Stop()

	var growthChan <-chan time.Time
	if s.cfg.GrowthThreshold > 0 {
		wt := time.NewTicker(s.cfg.GrowthCheckInterval)
		defer wt.Stop()
		growthChan = wt.C
	}

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("rerankertrainer.run.shutdown",
				slog.String("reason", ctx.Err().Error()))
			return ctx.Err()
		case <-intervalTicker.C:
			if _, err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("rerankertrainer.run.tick_failed",
					slog.String("error", err.Error()))
			}
		case <-growthChan:
			if !s.shouldGrowthTick(ctx) {
				continue
			}
			if _, err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("rerankertrainer.run.growth_tick_failed",
					slog.String("error", err.Error()))
			}
			intervalTicker.Reset(s.cfg.RunInterval)
		}
	}
}

// shouldGrowthTick is the on-demand wake predicate. Returns
// true when the current labelled-Episode count has grown by
// >= cfg.GrowthThreshold since the last successful publish's
// baseline (s.growthBaseline). The baseline is zero on first
// boot, in which case any non-zero count triggers a tick (so
// the binary is not "starved" of an initial publish waiting
// for the first 24h interval).
func (s *Service) shouldGrowthTick(ctx context.Context) bool {
	count, err := s.labelledEpisodeCount(ctx)
	if err != nil {
		s.logger.Warn("rerankertrainer.run.growth_count_failed",
			slog.String("error", err.Error()))
		return false
	}
	if s.growthBaseline <= 0 {
		return count > 0
	}
	grew := float64(count-s.growthBaseline) / float64(s.growthBaseline)
	if grew < s.cfg.GrowthThreshold {
		return false
	}
	s.logger.Info("rerankertrainer.run.growth_fired",
		slog.Int64("baseline", s.growthBaseline),
		slog.Int64("current", count),
		slog.Float64("grew", grew),
		slog.Float64("threshold", s.cfg.GrowthThreshold))
	return true
}
