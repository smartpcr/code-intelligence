// Command reranker-trainer is the Stage 6.4 Reranker Trainer
// worker per implementation-plan.md §6.4 and tech-spec §8.4.
// The binary hosts:
//
//   - a singleton rerankertrainer.Service whose Run loop ticks
//     every AGENT_MEMORY_RERANKER_INTERVAL (default 24h, the
//     "nightly cron job" cadence tech-spec §8.4 line 502
//     specifies), AND fires an on-demand tick whenever the
//     labelled-Episode count has grown by ≥ 5% since the last
//     successful publish (impl-plan §6.4 line 1123);
//
//   - a tiny HTTP surface on AGENT_MEMORY_LISTEN_ADDR (default
//     `:8087`) exposing `/healthz` for liveness and `/metrics`
//     for the Stage 6.4 metric contract (the
//     `reranker_runs_total` / `reranker_errors_total` /
//     `reranker_models_published_total` /
//     `reranker_capped_actor_total` counters plus the
//     `reranker_last_trained_at_seconds` gauge).
//
// The §6.4 e2e scenario "per-operator rate cap engages"
// references the cap counter by both the package's canonical
// `reranker_capped_actor_total` name AND the alternate
// `trainer_capped_actor_total` alias the scenario document
// uses verbatim; the /metrics handler emits BOTH so a
// grep-by-name in the scenario test finds either one.
//
// Configuration (env vars; no flags)
// ----------------------------------
//
// AGENT_MEMORY_PG_URL is the postgres:// DSN (REQUIRED). The
// role MUST hold the following per-table grants (tech-spec
// §8.4 / §8.7.4):
//
//	episode                     SELECT
//	episode_update              SELECT
//	observation                 SELECT
//	synthetic_positive          SELECT
//	recall_context_log          SELECT
//	reranker_model              INSERT, SELECT
//
// All other env vars (with defaults from the rerankertrainer
// package's Default* constants):
//
//	AGENT_MEMORY_RERANKER_INTERVAL              Long-poll cadence ("nightly
//	                                            cron job", tech-spec §8.4).
//	                                            Default 24h.
//	AGENT_MEMORY_RERANKER_TICK_TIMEOUT          Per-tick timeout. Default 30m.
//	AGENT_MEMORY_RERANKER_WINDOW                Trailing window for non-
//	                                            synthetic Episodes. Default
//	                                            90d (tech-spec §8.4 line 506).
//	AGENT_MEMORY_RERANKER_MIN_EPISODES          Floor below which a tick
//	                                            SKIPs publication. Default 50.
//	AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD      On-demand wake fraction
//	                                            (0.05 == 5%) per impl-plan
//	                                            §6.4 line 1123. Set to 0 to
//	                                            disable the on-demand path.
//	                                            Default 0.05.
//	AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL Cadence of the on-demand
//	                                            count poll. Default 1h.
//	AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW  Per-actor correction cap
//	                                            (tech-spec §9.4). Default 50.
//	                                            Set to 0 to disable.
//	AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW      Sliding-window length for
//	                                            the per-actor cap. Default 1h.
//	AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH    When true, the in-process
//	                                            NoopTrainer's output is
//	                                            force-elevated from "shadow"
//	                                            to "published". Dev/CI only.
//	                                            Default false.
//	AGENT_MEMORY_RERANKER_TRAINER_KIND          Trainer implementation to
//	                                            load. One of: "sidecar" (POSTs
//	                                            labelled pairs to an external
//	                                            HTTP trainer for the BERT-class
//	                                            cross-encoder per impl-plan
//	                                            §6.4 step-3; PRODUCTION
//	                                            DEFAULT when ENDPOINT is set),
//	                                            "linear" (in-process logistic
//	                                            baseline, DEV/CI ONLY —
//	                                            ≪200M-param model, no cross-
//	                                            encoder; must be set
//	                                            explicitly), or "noop" (zero-
//	                                            fit artifact for hermetic
//	                                            tests; must be set
//	                                            explicitly).
//	                                            When unset: defaults to
//	                                            "sidecar" if
//	                                            AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT
//	                                            is non-empty, else the binary
//	                                            REFUSES TO START — the
//	                                            previous silent linear
//	                                            fallback was a production
//	                                            footgun, so an operator
//	                                            running without a BERT
//	                                            sidecar MUST acknowledge
//	                                            that explicitly.
//	AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT      Base URL of the cross-encoder
//	                                            sidecar (no trailing slash).
//	                                            Used only when trainer kind
//	                                            is "sidecar".
//	AGENT_MEMORY_RERANKER_TRAINER_TAG           Trainer tag carried into
//	                                            TrainingInput.TrainerTag and
//	                                            the version fingerprint.
//	                                            Defaults to "linear" /
//	                                            "sidecar" / "noop" based on
//	                                            the resolved kind; override
//	                                            to a sidecar model name
//	                                            (e.g. "bert-base-uncased").
//	AGENT_MEMORY_LISTEN_ADDR                    HTTP bind for /healthz +
//	                                            /metrics. Default `:8087`
//	                                            (the consolidator owns :8086,
//	                                            trace-log-pruner :8085, the
//	                                            Span Ingestor :4318).
//	AGENT_MEMORY_SHUTDOWN_TIMEOUT               Graceful-shutdown budget.
//	                                            Default 30s.
//
// Exit codes
// ----------
//
//	0  graceful shutdown (SIGINT / SIGTERM)
//	2  configuration error (missing required env, malformed DSN)
//	3  startup failure (DB ping)
//	4  runtime failure (Run returned a non-Canceled error)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/rerankertrainer"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("reranker-trainer.config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Stage 8.3 step 2 — OTel trace export.
	tracerSetup, err := obs.SetupTracer(ctx, obs.ServiceNameRerankerTrainer, logger)
	if err != nil {
		logger.Error("reranker-trainer.otel.setup_failed", slog.String("error", err.Error()))
		os.Exit(2)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracerSetup.Shutdown(shutCtx)
	}()
	logger.Info("reranker-trainer.otel.ready",
		slog.Bool("exporting", tracerSetup.Exporting),
		slog.String("endpoint", tracerSetup.EndpointResolved))

	db, err := openPG(ctx, cfg, logger)
	if err != nil {
		logger.Error("reranker-trainer.pg", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer func() { _ = db.Close() }()

	trainer, err := selectTrainer(cfg)
	if err != nil {
		logger.Error("reranker-trainer.trainer", slog.String("error", err.Error()))
		os.Exit(2)
	}
	svc, err := rerankertrainer.New(db, rerankertrainer.Config{
		RunInterval:         cfg.Interval,
		TickTimeout:         cfg.TickTimeout,
		TrainingWindow:      cfg.TrainingWindow,
		MinEpisodes:         cfg.MinEpisodes,
		GrowthThreshold:     cfg.GrowthThreshold,
		GrowthCheckInterval: cfg.GrowthCheckInterval,
		ActorCapPerWindow:   cfg.ActorCapPerWindow,
		ActorCapWindow:      cfg.ActorCapWindow,
		AllowNoopPublish:    cfg.AllowNoopPublish,
	}, trainer, logger)
	if err != nil {
		logger.Error("reranker-trainer.service", slog.String("error", err.Error()))
		os.Exit(2)
	}
	// Stage 8.3 step 2 -- wire the OTel tracer so each Tick
	// produces a `rerankertrainer.tick` span (previously the
	// SDK was set up but never started a span).
	svc.ApplyOptions(rerankertrainer.WithTracer(tracerSetup.Tracer))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetrics(w, svc.Metrics())
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- svc.Run(ctx) }()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("reranker-trainer.listen", slog.String("addr", cfg.ListenAddr))
		serveErr <- srv.ListenAndServe()
	}()

	logger.Info("reranker-trainer.ready",
		slog.Duration("interval", cfg.Interval),
		slog.Duration("tick_timeout", cfg.TickTimeout),
		slog.Duration("training_window", cfg.TrainingWindow),
		slog.Int("min_episodes", cfg.MinEpisodes),
		slog.Float64("growth_threshold", cfg.GrowthThreshold),
		slog.Duration("growth_check_interval", cfg.GrowthCheckInterval),
		slog.Int("actor_cap_per_window", cfg.ActorCapPerWindow),
		slog.Duration("actor_cap_window", cfg.ActorCapWindow),
		slog.Bool("allow_noop_publish", cfg.AllowNoopPublish),
		slog.String("trainer_tag", trainer.Tag()),
		slog.String("listen_addr", cfg.ListenAddr))

	if code := waitForShutdown(ctx, srv, serveErr, runErr, stop,
		cfg.ShutdownTimeout, logger); code != 0 {
		os.Exit(code)
	}
}

// selectTrainer resolves cfg.TrainerKind into a Trainer.
//
// Production: "sidecar" when AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT
// is set; the binary POSTs labelled pairs to an external
// Python sidecar that fits the BERT-class cross-encoder per
// impl-plan §6.4 step-3 (cap ≤200M params). A reference
// sidecar implementation ships at
// `services/agent-memory/cmd/reranker-sidecar/` using
// sentence-transformers' `cross-encoder/ms-marco-MiniLM-L-12-v2`
// (~33M params, well under the 200M cap).
//
// Dev/CI: "linear" (in-process logistic baseline; opt-in via
// AGENT_MEMORY_RERANKER_TRAINER_KIND=linear) or "noop"
// (zero-fit; opt-in via AGENT_MEMORY_RERANKER_TRAINER_KIND=noop).
// The binary refuses to silently fall back to the linear
// baseline because the §6.4 brief mandates the BERT trainer
// for production — see loadConfig for the no-config error.
func selectTrainer(cfg config) (rerankertrainer.Trainer, error) {
	switch cfg.TrainerKind {
	case "noop":
		t := rerankertrainer.NoopTrainer{}
		if cfg.TrainerTag != "" {
			return taggedTrainer{Trainer: t, tag: cfg.TrainerTag}, nil
		}
		return t, nil
	case "linear":
		t := rerankertrainer.LinearTrainer{}
		if cfg.TrainerTag != "" {
			return taggedTrainer{Trainer: t, tag: cfg.TrainerTag}, nil
		}
		return t, nil
	case "sidecar":
		if cfg.TrainerEndpoint == "" {
			return nil, errors.New("rerankertrainer: sidecar kind requires AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT")
		}
		s := rerankertrainer.SidecarTrainer{
			Endpoint:    cfg.TrainerEndpoint,
			TagName:     cfg.TrainerTag,
		}
		return s, nil
	default:
		return nil, fmt.Errorf("rerankertrainer: unknown trainer kind %q", cfg.TrainerKind)
	}
}

// taggedTrainer wraps a Trainer to override its Tag(). Used
// only when AGENT_MEMORY_RERANKER_TRAINER_TAG is set, so the
// operator can carry a richer model identity (e.g. a git sha)
// into the reranker_model.version fingerprint without modifying
// the underlying trainer implementation.
type taggedTrainer struct {
	rerankertrainer.Trainer
	tag string
}

func (t taggedTrainer) Tag() string { return t.tag }

// httpShutdowner is the *http.Server surface waitForShutdown drives.
// Carved out as an interface so the unit test can exercise the
// SIGINT-race regression without binding a real listener.
type httpShutdowner interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// waitForShutdown is the binary's single graceful-exit state
// machine. It blocks on the first of three exit triggers
// (signal-cancelled `ctx`, an unexpected `serveErr`, or
// `runErr`) and then ALWAYS walks the documented graceful
// HTTP shutdown path, regardless of which trigger fired first.
//
// Mirrors the consolidator binary's iter-8 fix for the same
// race: on SIGINT/SIGTERM, both `<-ctx.Done()` and the
// runErr send (carrying context.Canceled) become ready in the
// SAME scheduler tick, and Go's select picks one at random.
// Whenever runErr won the race, srv.Shutdown was never invoked
// and in-flight `/metrics` + `/healthz` scrapes were dropped
// abruptly. Routing every branch through the shared shutdown
// block makes the outcome identical regardless of which
// channel the runtime selects.
//
// Returns the OS exit code: 0 on graceful shutdown, 4 on a
// runtime/serve failure that needs to surface through os.Exit.
func waitForShutdown(
	ctx context.Context,
	srv httpShutdowner,
	serveErr, runErr <-chan error,
	cancelCtx context.CancelFunc,
	shutdownTimeout time.Duration,
	logger *slog.Logger,
) int {
	exitCode := 0
	serveDone := false
	runDone := false
	select {
	case <-ctx.Done():
		logger.Info("reranker-trainer.shutdown.signal")
	case err := <-serveErr:
		serveDone = true
		cancelCtx()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("reranker-trainer.serve",
				slog.String("error", err.Error()))
			exitCode = 4
		}
	case err := <-runErr:
		runDone = true
		cancelCtx()
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("reranker-trainer.run",
				slog.String("error", err.Error()))
			exitCode = 4
		}
	}

	shutCtx, cancelShut := context.WithTimeout(
		context.Background(), shutdownTimeout)
	defer cancelShut()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Warn("reranker-trainer.shutdown.error",
			slog.String("error", err.Error()))
		// Shutdown returned an error (commonly: the shutCtx
		// deadline expired while in-flight requests refused to
		// drain). Force the listener closed so ListenAndServe
		// returns promptly and the serveErr drain below does
		// not block past shutCtx.Done().
		_ = srv.Close()
	}
	if !serveDone {
		select {
		case <-serveErr:
		case <-shutCtx.Done():
			logger.Warn("reranker-trainer.shutdown.serve_timeout")
		}
	}
	if !runDone {
		select {
		case <-runErr:
		case <-shutCtx.Done():
			logger.Warn("reranker-trainer.shutdown.run_timeout")
		}
	}
	logger.Info("reranker-trainer.shutdown.done")
	return exitCode
}

type config struct {
	PGURL               string
	Interval            time.Duration
	TickTimeout         time.Duration
	TrainingWindow      time.Duration
	MinEpisodes         int
	GrowthThreshold     float64
	GrowthCheckInterval time.Duration
	ActorCapPerWindow   int
	ActorCapWindow      time.Duration
	AllowNoopPublish    bool
	TrainerKind         string // "linear" | "sidecar" | "noop"
	TrainerEndpoint     string // sidecar URL, empty for linear/noop
	TrainerTag          string // override trainer Tag(); empty = use trainer default
	ListenAddr          string
	ShutdownTimeout     time.Duration
}

func loadConfig() (config, error) {
	c := config{
		PGURL:               os.Getenv("AGENT_MEMORY_PG_URL"),
		Interval:            rerankertrainer.DefaultRunInterval,
		TickTimeout:         rerankertrainer.DefaultTickTimeout,
		TrainingWindow:      rerankertrainer.DefaultTrainingWindow,
		MinEpisodes:         rerankertrainer.DefaultMinEpisodes,
		GrowthThreshold:     rerankertrainer.DefaultGrowthThreshold,
		GrowthCheckInterval: rerankertrainer.DefaultGrowthCheckInterval,
		ActorCapPerWindow:   rerankertrainer.DefaultActorCapPerWindow,
		ActorCapWindow:      rerankertrainer.DefaultActorCapWindow,
		ListenAddr:          os.Getenv("AGENT_MEMORY_LISTEN_ADDR"),
		ShutdownTimeout:     30 * time.Second,
	}
	if c.PGURL == "" {
		return c, errors.New("AGENT_MEMORY_PG_URL is required")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8087"
	}
	if v := os.Getenv("AGENT_MEMORY_RERANKER_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_INTERVAL: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_INTERVAL: must be positive, got %v", d)
		}
		c.Interval = d
	}
	if v := os.Getenv("AGENT_MEMORY_RERANKER_TICK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_TICK_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_TICK_TIMEOUT: must be positive, got %v", d)
		}
		c.TickTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_RERANKER_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_WINDOW: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_WINDOW: must be positive, got %v", d)
		}
		c.TrainingWindow = d
	}
	if v := os.Getenv("AGENT_MEMORY_RERANKER_MIN_EPISODES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_MIN_EPISODES: must be positive int, got %q", v)
		}
		c.MinEpisodes = n
	}
	if v := os.Getenv("AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD: must be non-negative float, got %q", v)
		}
		c.GrowthThreshold = f
	}
	if v := os.Getenv("AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL: must be positive, got %v", d)
		}
		c.GrowthCheckInterval = d
	}
	if v := os.Getenv("AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW: must be non-negative int, got %q", v)
		}
		c.ActorCapPerWindow = n
	}
	if v := os.Getenv("AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW: must be positive, got %v", d)
		}
		c.ActorCapWindow = d
	}
	if v := os.Getenv("AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH: %w", err)
		}
		c.AllowNoopPublish = b
	}
	c.TrainerEndpoint = os.Getenv("AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT")
	c.TrainerTag = os.Getenv("AGENT_MEMORY_RERANKER_TRAINER_TAG")
	if v := os.Getenv("AGENT_MEMORY_RERANKER_TRAINER_KIND"); v != "" {
		switch v {
		case "linear", "sidecar", "noop":
			c.TrainerKind = v
		default:
			return c, fmt.Errorf("AGENT_MEMORY_RERANKER_TRAINER_KIND: must be one of {linear,sidecar,noop}, got %q", v)
		}
	} else {
		// Pick the production default: sidecar when the
		// endpoint is wired. When neither KIND nor ENDPOINT
		// is set the binary REFUSES to start — the previous
		// silent linear fallback was a production footgun
		// (the binary would happily train a ≪200M baseline
		// where the operator expected a BERT cross-encoder),
		// and the §6.4 brief mandates the BERT trainer.
		// Dev/CI environments that genuinely want the
		// in-process baseline MUST opt in via
		// AGENT_MEMORY_RERANKER_TRAINER_KIND=linear.
		if c.TrainerEndpoint != "" {
			c.TrainerKind = "sidecar"
		} else {
			return c, errors.New(
				"rerankertrainer: no trainer configured: set AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT for the production BERT sidecar, " +
					"or set AGENT_MEMORY_RERANKER_TRAINER_KIND=linear (dev/CI baseline) / =noop (hermetic CI) to opt out explicitly")
		}
	}
	if c.TrainerKind == "sidecar" && c.TrainerEndpoint == "" {
		return c, errors.New("AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT is required when AGENT_MEMORY_RERANKER_TRAINER_KIND=sidecar")
	}
	if v := os.Getenv("AGENT_MEMORY_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_SHUTDOWN_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_SHUTDOWN_TIMEOUT: must be positive, got %v", d)
		}
		c.ShutdownTimeout = d
	}
	return c, nil
}

func openPG(ctx context.Context, cfg config, logger *slog.Logger) (*sql.DB, error) {
	pool, err := sql.Open("postgres", cfg.PGURL)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	// The trainer holds ONE session-pinned conn per tick (for
	// the advisory lock + the long-running pair pull), plus a
	// few pool connections for the publish INSERT and the
	// growth-check SELECT. A small pool is plenty.
	pool.SetMaxOpenConns(4)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	logger.Info("reranker-trainer.pg.connected")
	return pool, nil
}

// writeMetrics renders the Service's counter snapshot plus the
// per-actor cap counters plus the last-trained gauge in
// Prometheus text-format. Matches the hand-rolled exposition
// the consolidator binary uses so the two binaries' /metrics
// endpoints parse identically.
//
// The §6.4 e2e scenario "per-operator rate cap engages"
// references the cap counter by both the package's canonical
// `reranker_capped_actor_total` name AND the
// `trainer_capped_actor_total` alias the scenario document
// uses verbatim. The handler emits BOTH families so a
// grep-by-name in the scenario test finds either one.
func writeMetrics(w http.ResponseWriter, m *rerankertrainer.Metrics) {
	snap := m.Snapshot()
	helps := map[string]string{
		rerankertrainer.MetricRerankerRunsTotal:             "Reranker Trainer Tick invocations since binary start (success or failure).",
		rerankertrainer.MetricRerankerErrorsTotal:           "Reranker Trainer Tick invocations that surfaced a non-nil error since binary start.",
		rerankertrainer.MetricRerankerLockSkippedTotal:      "Reranker Trainer Tick invocations that skipped because pg_try_advisory_lock returned false (another replica holds the cross-process lock).",
		rerankertrainer.MetricRerankerModelsPublishedTotal:  "reranker_model rows the Reranker Trainer has INSERTed across all ticks. Duplicate-fingerprint ticks (ON CONFLICT DO NOTHING idempotent retries) do NOT bump this counter.",
		rerankertrainer.MetricRerankerPositivePairsTotal:    "Positive labelled pairs the Reranker Trainer has cumulatively trained over (post-cap synthetic_positive Episodes plus their parent agent Episodes).",
		rerankertrainer.MetricRerankerNegativePairsTotal:    "Negative labelled pairs the Reranker Trainer has cumulatively trained over (post-cap operator-correction Episodes plus the plain failure/degraded Episodes).",
		rerankertrainer.MetricRerankerEpisodesBelowMinTotal: "Reranker Trainer Tick invocations that skipped publication because post-cap labelled-pair count fell below MinEpisodes.",
	}
	counterOrder := []string{
		rerankertrainer.MetricRerankerRunsTotal,
		rerankertrainer.MetricRerankerErrorsTotal,
		rerankertrainer.MetricRerankerLockSkippedTotal,
		rerankertrainer.MetricRerankerModelsPublishedTotal,
		rerankertrainer.MetricRerankerPositivePairsTotal,
		rerankertrainer.MetricRerankerNegativePairsTotal,
		rerankertrainer.MetricRerankerEpisodesBelowMinTotal,
	}
	for _, name := range counterOrder {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, helps[name])
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", name)
		_, _ = fmt.Fprintf(w, "%s %d\n", name, snap[name])
	}

	// Per-actor cap counter, emitted under BOTH the canonical
	// and §6.4-scenario-alias names so a grep-by-either-name
	// in the scenario test finds the value.
	cappedHelp := "Correction-derived negative labelled pairs the Reranker Trainer has DROPPED on the per-actor §9.4 cap, per actor."
	for _, name := range []string{
		rerankertrainer.MetricRerankerCappedActorTotal,
		rerankertrainer.AltMetricCappedActorTotal,
	} {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, cappedHelp)
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", name)
		for _, sample := range m.CappedActorSnapshot() {
			_, _ = fmt.Fprintf(w, "%s{actor=%q} %d\n", name, sample.Actor, sample.Count)
		}
	}

	// Last-trained-at gauge (tech-spec §8.4: "publish a new
	// `reranker_model` row with `version`, `trained_at`,
	// `metrics_json`"). Exposed as Unix seconds so downstream
	// dashboards can compute `time() - reranker_last_trained_at_seconds`
	// to drive the §9.10 staleness alert.
	_, _ = fmt.Fprintf(w, "# HELP %s Unix seconds of the most recent successful reranker_model publish observed by this process. 0 when no publish has yet been observed.\n",
		rerankertrainer.MetricRerankerLastTrainedAtSeconds)
	_, _ = fmt.Fprintf(w, "# TYPE %s gauge\n", rerankertrainer.MetricRerankerLastTrainedAtSeconds)
	last := m.LastTrainedAt()
	var lastUnix int64
	if !last.IsZero() {
		lastUnix = last.Unix()
	}
	_, _ = fmt.Fprintf(w, "%s %d\n", rerankertrainer.MetricRerankerLastTrainedAtSeconds, lastUnix)

	// Stage 8.3 step 1 alias: implementation-plan.md lists
	// the freshness gauge as the suffix-less spelling
	// `reranker_last_trained_at`. We emit it with the SAME
	// sample value (and AS A GAUGE with its own HELP/TYPE) so
	// dashboards / alert rules written against the Stage 8.3
	// brief resolve, and the established `_seconds` name the
	// §9.10 staleness alert references keeps working.
	_, _ = fmt.Fprintf(w, "# HELP %s Stage 8.3 alias of %s; identical sample, suffix-less name.\n",
		rerankertrainer.AltMetricRerankerLastTrainedAt,
		rerankertrainer.MetricRerankerLastTrainedAtSeconds)
	_, _ = fmt.Fprintf(w, "# TYPE %s gauge\n", rerankertrainer.AltMetricRerankerLastTrainedAt)
	_, _ = fmt.Fprintf(w, "%s %d\n", rerankertrainer.AltMetricRerankerLastTrainedAt, lastUnix)
}
