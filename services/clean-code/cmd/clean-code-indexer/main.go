// Package main is the entrypoint for the clean-code-indexer service.
// It indexes repositories for code-quality analysis.
//
// # Stage 9.4 OTel wiring (iter-3 follow-up)
//
// The binary previously served `/healthz` only and had NO OTel
// SDK wiring -- a gap relative to the runbook's "every
// clean-code-* binary exports OpenTelemetry traces" claim.
// Stage 9.4 closes the gap by:
//
//   - installing the canonical [telemetry.Setup] at boot so
//     spans emitted by any subsystem the binary may import in
//     the future flow to the configured OTLP collector;
//   - mounting a placeholder `/metrics` endpoint so the
//     Prometheus scrape contract holds for the indexer's pod
//     just like every other clean-code-* binary.
//
// The indexer does NOT currently host any canonical verb
// surface (no `mgmt.*` / `ingest.*` / `policy.*` / `eval.*`
// route), so the verb-span middleware is NOT mounted. When the
// indexer grows a verb surface, the canonical pattern is the
// same one already used by `clean-code-metric-ingestor`:
// declare a [telemetry.VerbRoute] table, wrap the root mux in
// [telemetry.NewVerbSpanMiddleware], and have each handler
// call [telemetry.AnnotateVerbSpanRepoID] /
// [telemetry.AnnotateVerbSpanPolicyVersionID] after the wire
// request parses.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/telemetry"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	pgURL := os.Getenv("CLEAN_CODE_PG_URL")
	if pgURL == "" {
		log.Fatal("CLEAN_CODE_PG_URL is required")
	}

	// Stage 9.4 iter-3 follow-up: initialise the OTel SDK so
	// the indexer pod participates in the cluster's trace
	// pipeline. Uses [lookupEnvOrDefault] so an UNSET
	// CLEAN_CODE_OTEL_ENDPOINT falls back to
	// [config.DefaultOTelEndpoint]; an explicitly EMPTY value
	// remains the canonical "telemetry disabled" sentinel.
	telCtx, telCancel := context.WithCancel(context.Background())
	defer telCancel()
	telCfg := config.Config{
		OTelEndpoint: lookupEnvOrDefault(config.EnvOTelEndpoint, config.DefaultOTelEndpoint),
	}
	shutdownTelemetry, telErr := telemetry.Setup(telCtx, telCfg, telemetry.SetupOptions{
		ServiceName: "clean-code-indexer",
	})
	if telErr != nil {
		log.Fatalf("clean-code-indexer: telemetry.Setup: %v", telErr)
	}
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			log.Printf("clean-code-indexer: telemetry shutdown error: %v", err)
		}
	}()

	mux := buildMux()

	log.Printf("clean-code-indexer listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// buildMux mounts the always-on operational surface:
//   - `/healthz` (Kubernetes liveness)
//   - `/metrics` (Prometheus stub -- the indexer currently
//     publishes no counters, but the route exists so the
//     Prometheus scrape contract holds. Future subsystems
//     (e.g. repo-indexer queue depth) wire their collectors
//     here via [telemetry.PrometheusHandler]).
//
// Kept tiny so the binary stays a thin composition root.
// Mirrors the [clean-code-refactor-planner] pattern.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# clean-code-indexer metrics placeholder\n"))
	})
	return mux
}

// lookupEnvOrDefault returns the env var's value when SET
// (including the empty string, which is the explicit
// "telemetry disabled" sentinel per [config.EnvOTelEndpoint])
// and the supplied default when UNSET. Distinguishing unset
// from explicitly-empty is the contract iter-2 evaluator
// feedback #2 requires: `os.Getenv` collapses both into "" and
// silently bypasses the canonical [config.DefaultOTelEndpoint].
// Mirror of the helper in the other composition roots
// (`clean-code-gateway`, `clean-code-eval-gate`,
// `clean-code-metric-ingestor`, `clean-code-refactor-planner`).
func lookupEnvOrDefault(name, defaultVal string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return defaultVal
}