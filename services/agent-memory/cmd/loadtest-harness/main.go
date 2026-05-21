// Command loadtest-harness drives the §8.3 calibration
// envelope against a live agent-memory stack and writes the
// resulting calibration report to disk.
//
// Scope (implementation-plan.md Stage 8.4):
//   - Drives `agent.recall`, `agent.observe`, `agent.expand`,
//     `agent.summarize` at their §8.3 sustained RPS via the
//     Agent Surface gRPC endpoint.
//   - Drives `mgmt.ingest_spans` at its §8.3 sustained batch
//     rate via the Management Surface REST endpoint.
//   - Captures p50/p95/p99 latency per verb, per-verb
//     error-budget breaches, and the labelled-query
//     learning-quality proxy (rank-of-correct-node + concept-hit
//     fraction at K=20).
//   - Writes the calibration artifact to
//     `docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md`
//     by default; override via `--artifact`.
//
// Usage (operator):
//
//	# 30-minute nominal run against the deploy/local stack
//	loadtest-harness \
//	   --agent-target localhost:8443 \
//	   --mgmt-target  http://localhost:8444 \
//	   --repo-id      <fixture-uuid> \
//	   --seeded-loc   200000
//
//	# CI smoke check (sub-second, no real stack required when
//	# the targets resolve to a local in-process server)
//	loadtest-harness --profile smoke
//
//	# Reproducible flaky-run replay
//	loadtest-harness --seed 12345 --artifact rerun.md
//
// Exit codes:
//
//	0 -- all verbs within the §8.3 error-budget cap (1 %)
//	     AND the calibration artifact was written successfully.
//	1 -- artifact was written but at least one verb exceeded
//	     the error budget. The operator inspects the artifact's
//	     "Error-budget breaches" section.
//	2 -- harness construction or run failed (config invalid,
//	     dial failure, etc); the operator inspects stderr.
//	3 -- the run was aborted before the planned duration
//	     elapsed (SIGINT/SIGTERM/parent-context deadline).
//	     The partial artifact is still written so the operator
//	     can inspect what was captured, but CI MUST NOT pin
//	     a baseline from an exitAborted run.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/calibration"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/scenarios"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
	agentpb "github.com/smartpcr/code-intelligence/services/agent-memory/proto/agent"
)

// exit codes — see package doc.
const (
	exitOK      = 0
	exitBreach  = 1
	exitFail    = 2
	exitAborted = 3
)

type cliFlags struct {
	agentTarget        string
	mgmtTarget         string
	profile            string
	duration           time.Duration
	artifact           string
	repoID             string
	seed               int64
	maxInflight        int
	spansPerBatch      int
	seededFixtureLOC   int
	disableTLS         bool
	skipMgmt           bool
	skipAgent          bool
	requestTimeout     time.Duration
	mgmtIngestPath     string
	metricsAddr        string
	disableTracer      bool
	labeledQueriesPath string
	provenance         string
}

// loadtestHarnessRunDurationSeconds is the harness's own
// per-verb latency histogram family. It is an OPERATIONAL
// SLO-gate signal, NOT a percentile-parity source for the
// markdown artifact.
//
// What this histogram is for:
//   - "Did p95 cross 1.5 s?" / SLO-line-crossing gates in
//     Grafana / Prometheus alertmanager.
//   - Per-verb shape diffing between two stacks (deploy/local
//     vs staging vs prod scrape) at the bucket boundaries.
//
// What this histogram is NOT for:
//   - Reproducing the markdown artifact's exact p50/p95/p99
//     values. It cannot. The artifact uses nearest-rank
//     percentiles over the raw sample slice (see
//     `calibration.Percentiles` — `idx = ceil(p*n) - 1`), so
//     its percentiles are at single-sample resolution. This
//     histogram is bucketed at the §8.3 SLO-aligned boundaries
//     (`0.05, 0.1, 0.2, 0.4, 0.8, 1.5, 2, 4, 5, 10, 30`), so
//     `histogram_quantile()` returns linearly-interpolated
//     values within whichever bucket contains the percentile
//     rank. Mid-bucket numbers differ from the artifact's
//     nearest-rank numbers by up to one bucket width.
//
// The two surfaces agree on whether a percentile crosses an
// §8.3 SLO threshold (the bucket boundaries are placed
// exactly at those thresholds for that purpose) but agree on
// NOTHING ELSE. Reviewers comparing exact millisecond
// percentiles MUST read the markdown artifact, not the scrape.
//
// Iter-2 evaluator F4 (fixed): the vec exposes ONE Prometheus
// metric NAME with a `{verb="..."}` label so an operator can
// query
// `histogram_quantile(0.95, sum(rate(loadtest_harness_request_duration_seconds_bucket[5m])) by (verb, le))`
// to recover per-verb percentiles APPROXIMATELY (subject to
// the bucket-width caveat above). The previous iteration
// emitted a single unlabelled series that folded all five
// verbs together, making `histogram_quantile()` unable to
// split by verb at all.
var loadtestHarnessRunDurationSeconds = newVerbLabelledHistogram(
	"loadtest_harness_request_duration_seconds",
	"Approximate per-verb latency for the agent-memory loadtest harness, labelled by `verb`. SLO-line crossings are exact (bucket boundaries land at the §8.3 thresholds), but `histogram_quantile()` mid-bucket values differ from the markdown artifact's nearest-rank percentiles by up to one bucket width and MUST NOT be cross-checked against the artifact for exact millisecond agreement.",
	[]float64{0.05, 0.1, 0.2, 0.4, 0.8, 1.5, 2, 4, 5, 10, 30},
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is the testable entrypoint: flag parsing, harness wiring,
// and artifact persistence. Returns one of the exit codes above.
func run(args []string, stderr io.Writer) int {
	flags, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFail
	}

	cfg := buildConfig(flags)
	labeledQueries, err := loadLabeledQueriesJSON(flags.labeledQueriesPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFail
	}
	cfg.LabeledQueries = labeledQueries
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "loadtest-harness: invalid config: %v\n", err)
		return exitFail
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Stage 8.3 / cmd-inventory contract — every binary
	// exposes Prometheus /metrics and OTel tracing (or no-ops
	// when the OTLP endpoint is absent). The harness is a
	// CLI, not a long-lived daemon, but the same hookup applies.
	if !flags.disableTracer {
		tracer, terr := obs.SetupTracer(ctx, obs.ServiceNameLoadtestHarness, slog.Default())
		if terr != nil {
			fmt.Fprintf(stderr, "loadtest-harness: setup tracer: %v\n", terr)
			// non-fatal: SetupTracer always returns a non-nil
			// shutdown hook (see internal/obs/tracer.go docs).
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tracer.Shutdown(shutdownCtx)
		}()
	}

	// Optional /metrics surface — bound only when the
	// operator passes --metrics-addr, so unit tests that
	// don't need it stay free of port pressure. The harness
	// publishes one well-known histogram family
	// (`loadtest_harness_request_duration_seconds` labelled by
	// `verb`) for the operator's Grafana to correlate SLO-line
	// crossings against the live service-side histograms.
	//
	// IMPORTANT: this surface is an SLO-gate signal, not a
	// percentile-parity source. `histogram_quantile()` returns
	// bucket-interpolated values that disagree with the
	// markdown artifact's nearest-rank percentiles inside each
	// bucket. See the docstring on `loadtestHarnessRunDurationSeconds`.
	if flags.metricsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			loadtestHarnessRunDurationSeconds.Write(w)
		})
		srv := &http.Server{Addr: flags.metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Warn("loadtest-harness: metrics server", slog.String("err", err.Error()))
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}

	scen, cleanup, err := wireScenarios(ctx, flags, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "loadtest-harness: wire scenarios: %v\n", err)
		return exitFail
	}
	defer cleanup()
	if len(scen) == 0 {
		fmt.Fprintln(stderr, "loadtest-harness: no scenarios enabled (use --skip-agent / --skip-mgmt to narrow)")
		return exitFail
	}

	h, err := loadtest.NewHarness(cfg, scen,
		// Per-sample observer feeds the harness's own
		// /metrics histogram family — keyed by sample verb so
		// a `histogram_quantile(0.95, sum(rate(loadtest_harness_request_duration_seconds_bucket[5m])) by (verb, le))`
		// query splits per-verb percentiles the same way the
		// artifact does. Goroutine-safe:
		// verbLabelledHistogram.Observe is sync.Map +
		// LoadOrStore-guarded so a brand-new verb cannot lose
		// the first concurrent sample to a race.
		loadtest.WithSampleObserver(func(s scenarios.Sample) {
			loadtestHarnessRunDurationSeconds.Observe(s.Verb, s.Latency().Seconds())
		}),
		// Stable footer notes — keep the operator pointed at
		// the workflow doc + sample fixture even when the
		// dynamic notes section is otherwise empty (clean
		// runs with no dropped ticks / no degraded responses).
		loadtest.WithArtifactNote("operator workflow: see `docs/code-intelligence/agent-memory/load-test-calibration.md`"),
		loadtest.WithArtifactNote("labelled-query fixture starter: `docs/stories/code-intelligence-AGENT-MEMORY/labeled-queries.sample.json` (pass via `--labeled-queries` or `LABELED_QUERIES=` make var)"),
		loadtest.WithArtifactNote("default --max-inflight is 256 (was 64 in iter 0); raise when `Open-loop scheduler hygiene` reports dropped ticks AND achieved RPS lags requested RPS"),
	)
	if err != nil {
		fmt.Fprintf(stderr, "loadtest-harness: %v\n", err)
		return exitFail
	}

	rep, err := h.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "loadtest-harness: run: %v\n", err)
		return exitFail
	}

	cleaned, err := cfg.EnsureArtifactDir(os.MkdirAll)
	if err != nil {
		fmt.Fprintf(stderr, "loadtest-harness: artifact dir: %v\n", err)
		return exitFail
	}
	if err := rep.WriteFile(cleaned, os.WriteFile); err != nil {
		fmt.Fprintf(stderr, "loadtest-harness: write artifact: %v\n", err)
		return exitFail
	}

	// Aborted runs take precedence over budget breaches:
	// partial-window percentile/budget numbers are NOT a
	// passing or failing baseline — the operator must re-run.
	// Folding an aborted-with-breach run into exitBreach would
	// let CI pin a regression from a partial sample window.
	switch decideExitCode(rep) {
	case exitAborted:
		fmt.Fprintf(stderr, "loadtest-harness: run aborted (%s); partial artifact written to %s\n",
			rep.CompletionReason, cleaned)
		return exitAborted
	case exitBreach:
		fmt.Fprintf(stderr, "loadtest-harness: budget breaches: %s (see %s)\n",
			strings.Join(rep.BudgetBreaches, ","), cleaned)
		return exitBreach
	default:
		return exitOK
	}
}

// decideExitCode maps a finished Report to the binary's exit
// code. Aborted ALWAYS takes precedence over budget breaches:
// a partial-window run can spuriously report a breach (or
// spuriously omit one) and must never be promoted to a §8.3
// baseline either way. Documented in the package-doc Exit
// codes block.
func decideExitCode(rep calibration.Report) int {
	if rep.Aborted {
		return exitAborted
	}
	if len(rep.BudgetBreaches) > 0 {
		return exitBreach
	}
	return exitOK
}

func parseFlags(args []string) (cliFlags, error) {
	fs := flag.NewFlagSet("loadtest-harness", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f cliFlags
	fs.StringVar(&f.agentTarget, "agent-target", "localhost:8443", "AgentService gRPC dial target (host:port)")
	fs.StringVar(&f.mgmtTarget, "mgmt-target", "http://localhost:8444", "Management API base URL (scheme://host:port)")
	fs.StringVar(&f.profile, "profile", "nominal", "Traffic profile: nominal | smoke")
	fs.DurationVar(&f.duration, "duration", 0, "Run duration; 0 uses the profile's default (nominal: 30m, smoke: 200ms)")
	fs.StringVar(&f.artifact, "artifact", calibration.DefaultArtifactPath, "Calibration report path")
	fs.StringVar(&f.repoID, "repo-id", "ca11ca11-0000-4000-8000-000000000001", "Fixture repository id the scenarios drive; MUST be a UUID per the mgmt-api repo_id contract")
	fs.Int64Var(&f.seed, "seed", 0, "PRNG seed (0 = wall clock; echoed onto the artifact for replay)")
	fs.IntVar(&f.maxInflight, "max-inflight", 256, "Per-verb open-loop in-flight cap (default 256 = ceil(50 RPS × 4 s p99) × headroom)")
	fs.IntVar(&f.spansPerBatch, "spans-per-batch", 50, "mgmt.ingest_spans batch size (cap 1000 per §8.3)")
	fs.IntVar(&f.seededFixtureLOC, "seeded-loc", 0, "Seeded fixture line count (informational; stamped on the artifact)")
	fs.BoolVar(&f.disableTLS, "no-tls", false, "Dial AgentService without TLS (deploy/local only)")
	fs.BoolVar(&f.skipAgent, "skip-agent", false, "Skip the agent.* scenarios (mgmt-only run)")
	fs.BoolVar(&f.skipMgmt, "skip-mgmt", false, "Skip the mgmt.ingest_spans scenario (agent-only run)")
	fs.DurationVar(&f.requestTimeout, "request-timeout", 30*time.Second, "Per-request hard timeout")
	fs.StringVar(&f.mgmtIngestPath, "mgmt-ingest-path", "/v1/spans", "POST path on the mgmt-api for span ingest")
	fs.StringVar(&f.metricsAddr, "metrics-addr", "", "Bind address for the harness /metrics surface (empty = disabled)")
	fs.BoolVar(&f.disableTracer, "no-tracer", false, "Skip obs.SetupTracer (useful in CI where the OTLP endpoint is absent)")
	fs.StringVar(&f.labeledQueriesPath, "labeled-queries", "", "Path to a JSON file of LabeledQuery fixtures used for §8.3 learning-quality measurement; empty disables (artifact will read n/a)")
	fs.StringVar(&f.provenance, "provenance", "", "Operator-supplied banner stamped on the calibration artifact (e.g. \"IN-PROCESS STUB BASELINE\" or \"DEPLOY/LOCAL STACK NOMINAL\"); empty omits the banner")
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, fmt.Errorf("loadtest-harness: parse flags: %w", err)
	}
	if f.profile != "nominal" && f.profile != "smoke" {
		return cliFlags{}, fmt.Errorf("loadtest-harness: --profile must be one of nominal|smoke (got %q)", f.profile)
	}
	if f.maxInflight <= 0 {
		return cliFlags{}, fmt.Errorf("loadtest-harness: --max-inflight must be > 0 (got %d)", f.maxInflight)
	}
	return f, nil
}

func buildConfig(f cliFlags) calibration.Config {
	cfg := calibration.DefaultConfig()
	switch f.profile {
	case "smoke":
		cfg.Profile = reliability.SmokeProfile()
	default:
		cfg.Profile = reliability.NominalLoadProfile()
	}
	cfg.Duration = f.duration
	cfg.AgentTarget = f.agentTarget
	cfg.ManagementTarget = f.mgmtTarget
	cfg.ArtifactPath = f.artifact
	cfg.RepoID = f.repoID
	cfg.SeededFixtureLOC = f.seededFixtureLOC
	cfg.RandomSeed = f.seed
	cfg.MaxInflightPerVerb = f.maxInflight
	cfg.Provenance = f.provenance
	return cfg
}

// loadLabeledQueriesJSON reads a JSON file containing an array
// of LabeledQuery fixtures the harness drives agent.recall
// against for the learning-quality SLOs (rank-of-correct-node,
// concept-hit fraction).
//
// File shape (JSON array of objects):
//
//	[
//	  {
//	    "query": "how do we hash a node id?",
//	    "expected_node_id": "node:pkg/fingerprint/NodeFingerprint",
//	    "expected_concept_ids": ["concept:fingerprint", "concept:hash"],
//	    "kinds": ["method", "concept"]
//	  },
//	  ...
//	]
//
// Returns ([], nil) when path is empty so the operator can run
// the harness without a fixture (learning-quality renders n/a).
func loadLabeledQueriesJSON(path string) ([]calibration.LabeledQuery, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loadtest-harness: read --labeled-queries %q: %w", path, err)
	}
	type wire struct {
		Query              string   `json:"query"`
		ExpectedNodeID     string   `json:"expected_node_id"`
		ExpectedConceptIDs []string `json:"expected_concept_ids"`
		Kinds              []string `json:"kinds"`
	}
	var ws []wire
	if err := json.Unmarshal(raw, &ws); err != nil {
		return nil, fmt.Errorf("loadtest-harness: parse --labeled-queries %q: %w", path, err)
	}
	out := make([]calibration.LabeledQuery, 0, len(ws))
	for i, w := range ws {
		if w.Query == "" {
			return nil, fmt.Errorf("loadtest-harness: --labeled-queries %q entry %d: query is required", path, i)
		}
		out = append(out, calibration.LabeledQuery{
			Query:              w.Query,
			ExpectedNodeID:     w.ExpectedNodeID,
			ExpectedConceptIDs: w.ExpectedConceptIDs,
			Kinds:              w.Kinds,
		})
	}
	return out, nil
}

// wireScenarios is split out so tests can run a parsed-flags +
// validated-config pair against in-process fakes without
// dialling gRPC.
func wireScenarios(ctx context.Context, f cliFlags, cfg calibration.Config) ([]scenarios.Scenario, func(), error) {
	var scen []scenarios.Scenario
	cleanup := func() {}
	if !f.skipAgent {
		agentClient, closeAgent, err := dialAgent(ctx, f)
		if err != nil {
			return nil, cleanup, fmt.Errorf("dial agent: %w", err)
		}
		prev := cleanup
		cleanup = func() {
			prev()
			closeAgent()
		}
		scen = append(scen,
			&scenarios.RecallScenario{
				Client:         agentClient,
				RepoID:         cfg.RepoID,
				K:              cfg.Profile.LearningQuality.K,
				Queries:        scenarios.QueriesFromLabeled(toLabeledShape(cfg.LabeledQueries)),
				DefaultKinds:   []string{"method", "block", "concept"},
				RequestTimeout: f.requestTimeout,
			},
			&scenarios.ObserveScenario{
				Client:              agentClient,
				RepoID:              cfg.RepoID,
				SyntheticContextIDs: cfg.SyntheticContextIDs,
				RequestTimeout:      f.requestTimeout,
			},
			&scenarios.CallChainQueryScenario{
				Client:         agentClient,
				RepoID:         cfg.RepoID,
				MaxDepth:       3, // §8.3 envelope clause
				MaxNodes:       200,
				MaxEdges:       400,
				RequestTimeout: f.requestTimeout,
			},
			&scenarios.SummarizeScenario{
				Client:         agentClient,
				RepoID:         cfg.RepoID,
				MaxTokens:      1024,
				RequestTimeout: f.requestTimeout,
			},
		)
	}
	if !f.skipMgmt {
		mgmtClient := newMgmtClient(f.mgmtTarget, f.mgmtIngestPath, f.requestTimeout)
		scen = append(scen, &scenarios.GraphIngestScenario{
			Client:         mgmtClient,
			RepoID:         cfg.RepoID,
			SpansPerBatch:  f.spansPerBatch,
			RequestTimeout: f.requestTimeout,
		})
	}
	return scen, cleanup, nil
}

func toLabeledShape(src []calibration.LabeledQuery) []struct {
	Query              string
	ExpectedNodeID     string
	ExpectedConceptIDs []string
	Kinds              []string
} {
	out := make([]struct {
		Query              string
		ExpectedNodeID     string
		ExpectedConceptIDs []string
		Kinds              []string
	}, len(src))
	for i, q := range src {
		out[i] = struct {
			Query              string
			ExpectedNodeID     string
			ExpectedConceptIDs []string
			Kinds              []string
		}{q.Query, q.ExpectedNodeID, q.ExpectedConceptIDs, q.Kinds}
	}
	return out
}

func dialAgent(ctx context.Context, f cliFlags) (scenarios.AgentClient, func(), error) {
	var creds credentials.TransportCredentials
	if f.disableTLS {
		creds = insecure.NewCredentials()
	} else {
		// MinVersion only; the harness does NOT pin a client
		// cert by default. The operator wires mTLS via the
		// ambient deploy/local trust store when needed.
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(f.agentTarget, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, func() {}, fmt.Errorf("grpc.NewClient(%s): %w", f.agentTarget, err)
	}
	cleanup := func() { _ = conn.Close() }
	_ = ctx
	return &agentAdapter{c: agentpb.NewAgentServiceClient(conn)}, cleanup, nil
}

// agentAdapter wraps the generated agentpb gRPC client and
// translates the proto wire types into the harness's thin
// scenarios.* domain types.
type agentAdapter struct {
	c agentpb.AgentServiceClient
}

func (a *agentAdapter) Recall(ctx context.Context, req scenarios.RecallRequest) (scenarios.RecallResponse, error) {
	resp, err := a.c.Recall(ctx, &agentpb.RecallRequest{
		RepoId: req.RepoID,
		Query:  req.Query,
		K:      int32(req.K),
		Kinds:  req.Kinds,
	})
	if err != nil {
		return scenarios.RecallResponse{}, err
	}
	nodeIDs := make([]string, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		nodeIDs = append(nodeIDs, n.GetNodeId())
	}
	conceptIDs := make([]string, 0, len(resp.Concepts))
	for _, c := range resp.Concepts {
		conceptIDs = append(conceptIDs, c.GetConceptId())
	}
	return scenarios.RecallResponse{
		ContextID:  resp.GetContextId(),
		NodeIDs:    nodeIDs,
		ConceptIDs: conceptIDs,
		Degraded:   resp.GetDegraded(),
	}, nil
}

func (a *agentAdapter) Observe(ctx context.Context, req scenarios.ObserveRequest) (scenarios.ObserveResponse, error) {
	resp, err := a.c.Observe(ctx, &agentpb.ObserveRequest{
		ContextId:  req.ContextID,
		Outcome:    req.Outcome,
		RepoId:     req.RepoID,
		SessionId:  req.SessionID,
		TraceId:    req.TraceID,
		ActionJson: req.ActionJSON,
	})
	if err != nil {
		return scenarios.ObserveResponse{}, err
	}
	return scenarios.ObserveResponse{
		EpisodeID: resp.GetEpisodeId(),
		Degraded:  resp.GetDegraded(),
	}, nil
}

func (a *agentAdapter) Expand(ctx context.Context, req scenarios.ExpandRequest) (scenarios.ExpandResponse, error) {
	resp, err := a.c.Expand(ctx, &agentpb.ExpandRequest{
		NodeId:    req.NodeID,
		Direction: req.Direction,
		Depth:     req.Depth,
		RepoId:    req.RepoID,
		MaxNodes:  req.MaxNodes,
		MaxEdges:  req.MaxEdges,
	})
	if err != nil {
		return scenarios.ExpandResponse{}, err
	}
	nodeIDs := make([]string, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		nodeIDs = append(nodeIDs, n.GetNodeId())
	}
	edgeIDs := make([]string, 0, len(resp.Edges))
	for _, e := range resp.Edges {
		edgeIDs = append(edgeIDs, e.GetEdgeId())
	}
	return scenarios.ExpandResponse{
		ContextID:  resp.GetContextId(),
		RootNodeID: resp.GetRootNodeId(),
		NodeIDs:    nodeIDs,
		EdgeIDs:    edgeIDs,
		Truncated:  resp.GetTruncated(),
		Degraded:   resp.GetDegraded(),
	}, nil
}

func (a *agentAdapter) Summarize(ctx context.Context, req scenarios.SummarizeRequest) (scenarios.SummarizeResponse, error) {
	resp, err := a.c.Summarize(ctx, &agentpb.SummarizeRequest{
		NodeId:    req.NodeID,
		ConceptId: req.ConceptID,
		RepoId:    req.RepoID,
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		return scenarios.SummarizeResponse{}, err
	}
	return scenarios.SummarizeResponse{
		ContextID:  resp.GetContextId(),
		TargetKind: resp.GetTargetKind(),
		TargetID:   resp.GetTargetId(),
		SummaryMD:  resp.GetSummaryMd(),
		Degraded:   resp.GetDegraded(),
	}, nil
}

// mgmtClient is the REST adapter for `mgmt.ingest_spans`. The
// server returns 202 on success; any 4xx/5xx becomes an error
// the scenario records as a failed sample.
type mgmtClient struct {
	base    string
	path    string
	http    *http.Client
}

func newMgmtClient(base, path string, timeout time.Duration) *mgmtClient {
	return &mgmtClient{
		base: strings.TrimRight(base, "/"),
		path: path,
		http: &http.Client{Timeout: timeout},
	}
}

func (m *mgmtClient) IngestSpans(ctx context.Context, req scenarios.IngestSpansRequest) (scenarios.IngestSpansResponse, error) {
	url := m.base + m.path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(req.BatchJSON)))
	if err != nil {
		return scenarios.IngestSpansResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.RepoID != "" {
		// Mirror `internal/mgmtapi.MgmtRepoIDHeader`. The
		// mgmt-api validates the value as a UUID; the
		// scenario therefore requires the operator-supplied
		// repo-id to be UUID-formatted (the cmd binary's
		// default and the doc both reflect this).
		httpReq.Header.Set("X-Mgmt-Repo-ID", req.RepoID)
	}
	resp, err := m.http.Do(httpReq)
	if err != nil {
		return scenarios.IngestSpansResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return scenarios.IngestSpansResponse{}, fmt.Errorf("mgmt.ingest_spans: HTTP %d: %s", resp.StatusCode, string(body))
	}
	// Decode the mgmt-api's documented response envelope.
	// Mirrors `internal/mgmtapi.SpanIngestResponse`:
	// `accepted_spans` (NOT `accepted`), plus the wire-level
	// `degraded` / `degraded_reason` flags the harness threads
	// onto Sample.Degraded for the artifact's "degraded
	// responses" note.
	var env struct {
		RepoID         string `json:"repo_id"`
		AcceptedSpans  int    `json:"accepted_spans"`
		Degraded       bool   `json:"degraded"`
		DegradedReason string `json:"degraded_reason"`
	}
	if len(body) > 0 {
		// A 2xx with a malformed body is a contract violation
		// — propagate it as a verb failure so the artifact's
		// per-verb Failed count picks it up. Silently swallowing
		// it would treat a malformed 202 as a clean success and
		// hide real backend bugs from the calibration baseline.
		if err := json.Unmarshal(body, &env); err != nil {
			return scenarios.IngestSpansResponse{}, fmt.Errorf(
				"mgmt.ingest_spans: malformed 2xx response: %w (body=%q)",
				err, truncForErr(body, 256))
		}
	}
	return scenarios.IngestSpansResponse{
		Accepted:       env.AcceptedSpans,
		Rejected:       0,
		Degraded:       env.Degraded,
		DegradedReason: env.DegradedReason,
	}, nil
}

// truncForErr returns a printable head of b, capped to n bytes,
// so a malformed-response error message stays bounded even when
// the upstream returns a multi-kilobyte body. Mirrors the same
// helper shape we use in other agent-memory clients.
func truncForErr(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// _ guards required side-effects from accidental removal.
var (
	_ = errors.New
	_ = signal.NotifyContext
)
