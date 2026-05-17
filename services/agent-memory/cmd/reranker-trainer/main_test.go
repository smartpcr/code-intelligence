package main

// Regression coverage for the reranker-trainer binary's loadConfig
// + writeMetrics + waitForShutdown helpers. These tests run in
// `go test ./cmd/reranker-trainer` with NO live dependencies (no
// PostgreSQL, no real HTTP listener) and lock down:
//
//   * env-var parsing rules (loadConfig: defaults, overrides, and
//     rejection of malformed values),
//   * the Prometheus exposition shape (writeMetrics: HELP/TYPE
//     preamble for every metric, the §6.4-scenario alias counter
//     is emitted alongside the canonical name, and the
//     last-trained gauge round-trips Unix seconds correctly),
//   * the graceful-shutdown invariant that srv.Shutdown is invoked
//     on EVERY exit path -- including the SIGINT-race where
//     runErr<-context.Canceled wins the select (this was the
//     iter-8 finding #3 regression on the sibling consolidator
//     binary; we lock it down here too so a future "harmless
//     looking" edit cannot reintroduce it).

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/rerankertrainer"
)

// allRerankerEnv enumerates every env var loadConfig reads.
// Each loadConfig test resets ALL of them to empty before
// re-asserting so a stray value in the developer's shell or the
// CI runner cannot pollute the "defaults" assertion.
var allRerankerEnv = []string{
	"AGENT_MEMORY_PG_URL",
	"AGENT_MEMORY_LISTEN_ADDR",
	"AGENT_MEMORY_RERANKER_INTERVAL",
	"AGENT_MEMORY_RERANKER_TICK_TIMEOUT",
	"AGENT_MEMORY_RERANKER_WINDOW",
	"AGENT_MEMORY_RERANKER_MIN_EPISODES",
	"AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD",
	"AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL",
	"AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW",
	"AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW",
	"AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH",
	"AGENT_MEMORY_RERANKER_TRAINER_KIND",
	"AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT",
	"AGENT_MEMORY_RERANKER_TRAINER_TAG",
	"AGENT_MEMORY_SHUTDOWN_TIMEOUT",
}

// clearEnv blanks every reranker-trainer env var via t.Setenv so a
// pre-existing shell value in CI cannot leak into the test.
// t.Setenv auto-restores the prior value at test teardown.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allRerankerEnv {
		t.Setenv(k, "")
	}
}

// ────────────────────────────────────────────────────────────
// loadConfig — happy paths
// ────────────────────────────────────────────────────────────

func TestLoadConfig_defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://test/db")
	// The trainer kind must be set explicitly: the binary
	// refuses to silently fall back to the linear baseline
	// in production. For the defaults test we opt-in to
	// linear so we can exercise the rest of the defaults
	// without standing up a sidecar.
	t.Setenv("AGENT_MEMORY_RERANKER_TRAINER_KIND", "linear")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.PGURL != "postgres://test/db" {
		t.Errorf("PGURL: got %q want postgres://test/db", c.PGURL)
	}
	if c.TrainerKind != "linear" {
		t.Errorf("TrainerKind: got %q want %q", c.TrainerKind, "linear")
	}
	if c.ListenAddr != ":8087" {
		t.Errorf("ListenAddr: got %q want :8087 (reranker-trainer's reserved port)", c.ListenAddr)
	}
	if c.Interval != rerankertrainer.DefaultRunInterval {
		t.Errorf("Interval: got %v want %v (DefaultRunInterval)",
			c.Interval, rerankertrainer.DefaultRunInterval)
	}
	if c.TickTimeout != rerankertrainer.DefaultTickTimeout {
		t.Errorf("TickTimeout: got %v want %v (DefaultTickTimeout)",
			c.TickTimeout, rerankertrainer.DefaultTickTimeout)
	}
	if c.TrainingWindow != rerankertrainer.DefaultTrainingWindow {
		t.Errorf("TrainingWindow: got %v want %v (DefaultTrainingWindow)",
			c.TrainingWindow, rerankertrainer.DefaultTrainingWindow)
	}
	if c.MinEpisodes != rerankertrainer.DefaultMinEpisodes {
		t.Errorf("MinEpisodes: got %d want %d (DefaultMinEpisodes)",
			c.MinEpisodes, rerankertrainer.DefaultMinEpisodes)
	}
	if c.GrowthThreshold != rerankertrainer.DefaultGrowthThreshold {
		t.Errorf("GrowthThreshold: got %v want %v (DefaultGrowthThreshold)",
			c.GrowthThreshold, rerankertrainer.DefaultGrowthThreshold)
	}
	if c.GrowthCheckInterval != rerankertrainer.DefaultGrowthCheckInterval {
		t.Errorf("GrowthCheckInterval: got %v want %v (DefaultGrowthCheckInterval)",
			c.GrowthCheckInterval, rerankertrainer.DefaultGrowthCheckInterval)
	}
	if c.ActorCapPerWindow != rerankertrainer.DefaultActorCapPerWindow {
		t.Errorf("ActorCapPerWindow: got %d want %d (DefaultActorCapPerWindow)",
			c.ActorCapPerWindow, rerankertrainer.DefaultActorCapPerWindow)
	}
	if c.ActorCapWindow != rerankertrainer.DefaultActorCapWindow {
		t.Errorf("ActorCapWindow: got %v want %v (DefaultActorCapWindow)",
			c.ActorCapWindow, rerankertrainer.DefaultActorCapWindow)
	}
	if c.AllowNoopPublish {
		t.Errorf("AllowNoopPublish default should be false (no-op trainer publishes shadow only); got true")
	}
	if c.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout: got %v want 30s", c.ShutdownTimeout)
	}
}

func TestLoadConfig_overridesApplied(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://overrides/db")
	t.Setenv("AGENT_MEMORY_LISTEN_ADDR", "127.0.0.1:9090")
	t.Setenv("AGENT_MEMORY_RERANKER_INTERVAL", "12h")
	t.Setenv("AGENT_MEMORY_RERANKER_TICK_TIMEOUT", "5m")
	t.Setenv("AGENT_MEMORY_RERANKER_WINDOW", "30d") // ParseDuration does not accept "d"
	// "30d" is not valid Go duration syntax; use hours instead.
	t.Setenv("AGENT_MEMORY_RERANKER_WINDOW", "720h")
	t.Setenv("AGENT_MEMORY_RERANKER_MIN_EPISODES", "120")
	t.Setenv("AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD", "0.10")
	t.Setenv("AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL", "15m")
	t.Setenv("AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW", "25")
	t.Setenv("AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW", "2h")
	t.Setenv("AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH", "true")
	t.Setenv("AGENT_MEMORY_RERANKER_TRAINER_KIND", "linear")
	t.Setenv("AGENT_MEMORY_SHUTDOWN_TIMEOUT", "15s")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.PGURL != "postgres://overrides/db" {
		t.Errorf("PGURL: got %q", c.PGURL)
	}
	if c.ListenAddr != "127.0.0.1:9090" {
		t.Errorf("ListenAddr: got %q", c.ListenAddr)
	}
	if c.Interval != 12*time.Hour {
		t.Errorf("Interval: got %v want 12h", c.Interval)
	}
	if c.TickTimeout != 5*time.Minute {
		t.Errorf("TickTimeout: got %v want 5m", c.TickTimeout)
	}
	if c.TrainingWindow != 720*time.Hour {
		t.Errorf("TrainingWindow: got %v want 720h", c.TrainingWindow)
	}
	if c.MinEpisodes != 120 {
		t.Errorf("MinEpisodes: got %d want 120", c.MinEpisodes)
	}
	if c.GrowthThreshold != 0.10 {
		t.Errorf("GrowthThreshold: got %v want 0.10", c.GrowthThreshold)
	}
	if c.GrowthCheckInterval != 15*time.Minute {
		t.Errorf("GrowthCheckInterval: got %v want 15m", c.GrowthCheckInterval)
	}
	if c.ActorCapPerWindow != 25 {
		t.Errorf("ActorCapPerWindow: got %d want 25", c.ActorCapPerWindow)
	}
	if c.ActorCapWindow != 2*time.Hour {
		t.Errorf("ActorCapWindow: got %v want 2h", c.ActorCapWindow)
	}
	if !c.AllowNoopPublish {
		t.Errorf("AllowNoopPublish: got false want true")
	}
	if c.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout: got %v want 15s", c.ShutdownTimeout)
	}
}

// TestLoadConfig_actorCapZeroAccepted asserts that zero disables
// the per-actor cap rather than triggering a validation error.
// This matches the documented contract on ActorCapPerWindow:
// "<= 0 disables the cap".
func TestLoadConfig_actorCapZeroAccepted(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://x/y")
	t.Setenv("AGENT_MEMORY_RERANKER_TRAINER_KIND", "linear")
	t.Setenv("AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW", "0")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.ActorCapPerWindow != 0 {
		t.Errorf("ActorCapPerWindow=0 should be accepted (disable); got %d", c.ActorCapPerWindow)
	}
}

// TestLoadConfig_growthThresholdZeroAccepted asserts that zero
// (meaning "wake on the first new episode") is a legal value.
func TestLoadConfig_growthThresholdZeroAccepted(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://x/y")
	t.Setenv("AGENT_MEMORY_RERANKER_TRAINER_KIND", "linear")
	t.Setenv("AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD", "0")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.GrowthThreshold != 0 {
		t.Errorf("GrowthThreshold=0 should be accepted; got %v", c.GrowthThreshold)
	}
}

// ────────────────────────────────────────────────────────────
// loadConfig — error paths
// ────────────────────────────────────────────────────────────

func TestLoadConfig_missingPGURL(t *testing.T) {
	clearEnv(t)
	if _, err := loadConfig(); err == nil ||
		!strings.Contains(err.Error(), "AGENT_MEMORY_PG_URL") {
		t.Fatalf("expected missing-PG_URL error; got %v", err)
	}
}

// errorMatrixCase covers a single env-var validation rejection.
type errorMatrixCase struct {
	name   string
	envKey string
	value  string
	expect string
}

func TestLoadConfig_validationErrors(t *testing.T) {
	cases := []errorMatrixCase{
		// INTERVAL
		{"interval/badParse", "AGENT_MEMORY_RERANKER_INTERVAL", "10",
			"AGENT_MEMORY_RERANKER_INTERVAL"},
		{"interval/zero", "AGENT_MEMORY_RERANKER_INTERVAL", "0s",
			"AGENT_MEMORY_RERANKER_INTERVAL"},
		{"interval/negative", "AGENT_MEMORY_RERANKER_INTERVAL", "-5s",
			"AGENT_MEMORY_RERANKER_INTERVAL"},
		// TICK_TIMEOUT
		{"tickTimeout/badParse", "AGENT_MEMORY_RERANKER_TICK_TIMEOUT", "junk",
			"AGENT_MEMORY_RERANKER_TICK_TIMEOUT"},
		{"tickTimeout/zero", "AGENT_MEMORY_RERANKER_TICK_TIMEOUT", "0",
			"AGENT_MEMORY_RERANKER_TICK_TIMEOUT"},
		{"tickTimeout/negative", "AGENT_MEMORY_RERANKER_TICK_TIMEOUT", "-1m",
			"AGENT_MEMORY_RERANKER_TICK_TIMEOUT"},
		// WINDOW
		{"window/badParse", "AGENT_MEMORY_RERANKER_WINDOW", "30d",
			"AGENT_MEMORY_RERANKER_WINDOW"},
		{"window/zero", "AGENT_MEMORY_RERANKER_WINDOW", "0s",
			"AGENT_MEMORY_RERANKER_WINDOW"},
		{"window/negative", "AGENT_MEMORY_RERANKER_WINDOW", "-72h",
			"AGENT_MEMORY_RERANKER_WINDOW"},
		// MIN_EPISODES (must be positive)
		{"minEpisodes/nonInt", "AGENT_MEMORY_RERANKER_MIN_EPISODES", "abc",
			"AGENT_MEMORY_RERANKER_MIN_EPISODES"},
		{"minEpisodes/zero", "AGENT_MEMORY_RERANKER_MIN_EPISODES", "0",
			"AGENT_MEMORY_RERANKER_MIN_EPISODES"},
		{"minEpisodes/negative", "AGENT_MEMORY_RERANKER_MIN_EPISODES", "-3",
			"AGENT_MEMORY_RERANKER_MIN_EPISODES"},
		// GROWTH_THRESHOLD (must be non-negative float)
		{"growthThreshold/badParse", "AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD", "xyz",
			"AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD"},
		{"growthThreshold/negative", "AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD", "-0.1",
			"AGENT_MEMORY_RERANKER_GROWTH_THRESHOLD"},
		// GROWTH_CHECK_INTERVAL
		{"growthCheck/badParse", "AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL", "five",
			"AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL"},
		{"growthCheck/zero", "AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL", "0s",
			"AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL"},
		{"growthCheck/negative", "AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL", "-1m",
			"AGENT_MEMORY_RERANKER_GROWTH_CHECK_INTERVAL"},
		// ACTOR_CAP_PER_WINDOW (must be non-negative int)
		{"actorCap/nonInt", "AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW", "abc",
			"AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW"},
		{"actorCap/negative", "AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW", "-1",
			"AGENT_MEMORY_RERANKER_ACTOR_CAP_PER_WINDOW"},
		// ACTOR_CAP_WINDOW (must be positive)
		{"actorCapWindow/badParse", "AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW", "junk",
			"AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW"},
		{"actorCapWindow/zero", "AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW", "0",
			"AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW"},
		{"actorCapWindow/negative", "AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW", "-1m",
			"AGENT_MEMORY_RERANKER_ACTOR_CAP_WINDOW"},
		// ALLOW_NOOP_PUBLISH (bool)
		{"allowNoopPublish/notBool", "AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH", "kindof",
			"AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH"},
		// SHUTDOWN_TIMEOUT
		{"shutdown/badParse", "AGENT_MEMORY_SHUTDOWN_TIMEOUT", "noton",
			"AGENT_MEMORY_SHUTDOWN_TIMEOUT"},
		{"shutdown/zero", "AGENT_MEMORY_SHUTDOWN_TIMEOUT", "0",
			"AGENT_MEMORY_SHUTDOWN_TIMEOUT"},
		{"shutdown/negative", "AGENT_MEMORY_SHUTDOWN_TIMEOUT", "-30s",
			"AGENT_MEMORY_SHUTDOWN_TIMEOUT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("AGENT_MEMORY_PG_URL", "postgres://x/y")
			// Satisfy the F3 strict-trainer-config gate so
			// this matrix only exercises the per-env-var
			// validation it was written for; otherwise
			// every case would trip the "no trainer
			// configured" error before reaching the
			// scenario under test.
			t.Setenv("AGENT_MEMORY_RERANKER_TRAINER_KIND", "linear")
			t.Setenv(c.envKey, c.value)
			_, err := loadConfig()
			if err == nil {
				t.Fatalf("expected error for %s=%q; got nil", c.envKey, c.value)
			}
			if !strings.Contains(err.Error(), c.expect) {
				t.Fatalf("error %q does not mention %q (env var key)",
					err.Error(), c.expect)
			}
		})
	}
}

// TestLoadConfig_rejectsSilentLinearFallback exercises the F3
// strict-trainer-config gate: when neither
// AGENT_MEMORY_RERANKER_TRAINER_KIND nor
// AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT is set, the binary
// REFUSES to start. The previous default was a silent linear
// fallback, which left operators expecting BERT training with
// a sub-200-parameter logistic baseline.
func TestLoadConfig_rejectsSilentLinearFallback(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://x/y")
	_, err := loadConfig()
	if err == nil {
		t.Fatalf("expected error when no trainer kind/endpoint is set, got nil")
	}
	if !strings.Contains(err.Error(), "TRAINER_ENDPOINT") || !strings.Contains(err.Error(), "TRAINER_KIND") {
		t.Errorf("error %q must surface BOTH env var names so the operator knows the two opt-in paths", err.Error())
	}
}

// TestLoadConfig_acceptsExplicitNoop exercises the
// hermetic-CI opt-in: setting KIND=noop with no endpoint
// must succeed (noop is intentionally endpoint-less).
func TestLoadConfig_acceptsExplicitNoop(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://x/y")
	t.Setenv("AGENT_MEMORY_RERANKER_TRAINER_KIND", "noop")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig with KIND=noop: %v", err)
	}
	if c.TrainerKind != "noop" {
		t.Errorf("TrainerKind = %q, want %q", c.TrainerKind, "noop")
	}
}

// TestLoadConfig_acceptsSidecarWithEndpoint asserts the
// production happy path: ENDPOINT set, KIND defaults to
// sidecar.
func TestLoadConfig_acceptsSidecarWithEndpoint(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://x/y")
	t.Setenv("AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT", "http://reranker-sidecar:8088")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig with ENDPOINT set: %v", err)
	}
	if c.TrainerKind != "sidecar" {
		t.Errorf("TrainerKind = %q, want %q (default when ENDPOINT is set)", c.TrainerKind, "sidecar")
	}
}

// ────────────────────────────────────────────────────────────
// writeMetrics — exposition format
// ────────────────────────────────────────────────────────────

// parseMetric pulls the SAMPLE line for a given metric name out
// of a /metrics text body, ignoring the HELP/TYPE preamble. The
// optional `labelSuffix` lets callers disambiguate dimensional
// samples (e.g. `reranker_capped_actor_total{actor="op-a"}`).
func parseMetric(t *testing.T, body, name, labelSuffix string) string {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		matchName := fields[0] == name
		if labelSuffix != "" {
			matchName = strings.HasPrefix(fields[0], name+"{") &&
				strings.Contains(fields[0], labelSuffix)
		}
		if matchName {
			return fields[1]
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan body: %v", err)
	}
	t.Fatalf("metric %q (labelSuffix=%q) sample line not found in body:\n%s",
		name, labelSuffix, body)
	return ""
}

func requireExposition(t *testing.T, body, name, typ string) {
	t.Helper()
	wantHELP := "# HELP " + name + " "
	if !strings.Contains(body, wantHELP) {
		t.Fatalf("body missing %q line:\n%s", wantHELP, body)
	}
	wantTYPE := "# TYPE " + name + " " + typ
	if !strings.Contains(body, wantTYPE) {
		t.Fatalf("body missing %q line:\n%s", wantTYPE, body)
	}
}

func TestWriteMetrics_zeroState(t *testing.T) {
	m := rerankertrainer.NewMetrics()
	rec := httptest.NewRecorder()
	writeMetrics(rec, m)
	body := rec.Body.String()

	// All counters render with HELP+TYPE and a zero sample line.
	for _, name := range []string{
		rerankertrainer.MetricRerankerRunsTotal,
		rerankertrainer.MetricRerankerErrorsTotal,
		rerankertrainer.MetricRerankerLockSkippedTotal,
		rerankertrainer.MetricRerankerModelsPublishedTotal,
		rerankertrainer.MetricRerankerPositivePairsTotal,
		rerankertrainer.MetricRerankerNegativePairsTotal,
		rerankertrainer.MetricRerankerEpisodesBelowMinTotal,
	} {
		requireExposition(t, body, name, "counter")
		if v := parseMetric(t, body, name, ""); v != "0" {
			t.Errorf("metric %s: got %s want 0", name, v)
		}
	}
	// Per-actor cap counter and its alias have HELP/TYPE
	// emitted even when zero actors have been capped (so
	// scrape parsers see a valid metric family). No sample
	// line is required for a zero-cardinality dimensional
	// counter.
	requireExposition(t, body,
		rerankertrainer.MetricRerankerCappedActorTotal, "counter")
	requireExposition(t, body,
		rerankertrainer.AltMetricCappedActorTotal, "counter")

	// Last-trained gauge renders as zero when no publish has
	// been observed.
	requireExposition(t, body,
		rerankertrainer.MetricRerankerLastTrainedAtSeconds, "gauge")
	if v := parseMetric(t, body,
		rerankertrainer.MetricRerankerLastTrainedAtSeconds, ""); v != "0" {
		t.Errorf("last_trained_at_seconds: got %s want 0", v)
	}
}

func TestWriteMetrics_seededCountersRender(t *testing.T) {
	m := rerankertrainer.NewMetrics()
	// Seed each counter with a distinct prime-ish value so a
	// rendering bug that swaps two counters surfaces as a value
	// mismatch (not a silent zero-vs-zero).
	for i := 0; i < 3; i++ {
		m.IncRuns()
	}
	m.IncErrors()
	m.IncLockSkipped()
	m.IncLockSkipped()
	for i := 0; i < 4; i++ {
		m.IncModelsPublished()
	}
	m.AddPositivePairs(17)
	m.AddNegativePairs(11)
	m.IncEpisodesBelowMin()

	rec := httptest.NewRecorder()
	writeMetrics(rec, m)
	body := rec.Body.String()

	want := map[string]string{
		rerankertrainer.MetricRerankerRunsTotal:             "3",
		rerankertrainer.MetricRerankerErrorsTotal:           "1",
		rerankertrainer.MetricRerankerLockSkippedTotal:      "2",
		rerankertrainer.MetricRerankerModelsPublishedTotal:  "4",
		rerankertrainer.MetricRerankerPositivePairsTotal:    "17",
		rerankertrainer.MetricRerankerNegativePairsTotal:    "11",
		rerankertrainer.MetricRerankerEpisodesBelowMinTotal: "1",
	}
	for name, expected := range want {
		requireExposition(t, body, name, "counter")
		if got := parseMetric(t, body, name, ""); got != expected {
			t.Errorf("metric %s: got %s want %s", name, got, expected)
		}
	}
}

// TestWriteMetrics_cappedActorEmittedUnderBothNames is the §6.4
// scenario contract. The e2e doc names the metric
// `trainer_capped_actor_total` while the package canonical is
// `reranker_capped_actor_total`; the binary MUST emit BOTH so a
// scrape parser keyed on either name finds the sample.
func TestWriteMetrics_cappedActorEmittedUnderBothNames(t *testing.T) {
	m := rerankertrainer.NewMetrics()
	m.AddCappedActor("operator-alice", 7)

	rec := httptest.NewRecorder()
	writeMetrics(rec, m)
	body := rec.Body.String()

	// HELP+TYPE for both families.
	requireExposition(t, body,
		rerankertrainer.MetricRerankerCappedActorTotal, "counter")
	requireExposition(t, body,
		rerankertrainer.AltMetricCappedActorTotal, "counter")

	// Sample line for each family with the actor label.
	canon := parseMetric(t, body,
		rerankertrainer.MetricRerankerCappedActorTotal,
		`actor="operator-alice"`)
	alias := parseMetric(t, body,
		rerankertrainer.AltMetricCappedActorTotal,
		`actor="operator-alice"`)
	if canon != "7" {
		t.Errorf("canonical capped-actor: got %s want 7", canon)
	}
	if alias != "7" {
		t.Errorf("alias capped-actor: got %s want 7", alias)
	}
	if canon != alias {
		t.Errorf("canonical (%s) and alias (%s) must report identical counts",
			canon, alias)
	}
}

// TestWriteMetrics_lastTrainedAtGaugeRendersUnixSeconds: the
// gauge MUST emit Unix seconds (matching the §9.10 alert query
// `time() - reranker_last_trained_at_seconds`). A regression to
// e.g. milliseconds, ns, or a string format would flip the alert
// math and silently mute the staleness page.
func TestWriteMetrics_lastTrainedAtGaugeRendersUnixSeconds(t *testing.T) {
	m := rerankertrainer.NewMetrics()
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	m.SetLastTrainedAt(now)

	rec := httptest.NewRecorder()
	writeMetrics(rec, m)
	body := rec.Body.String()

	got := parseMetric(t, body,
		rerankertrainer.MetricRerankerLastTrainedAtSeconds, "")
	wantUnix := now.Unix()
	wantStr := strings.TrimSpace(asDecimal(wantUnix))
	if got != wantStr {
		t.Fatalf("last_trained_at_seconds: got %q want %q (unix seconds)",
			got, wantStr)
	}
}

// asDecimal is a tiny helper to avoid pulling strconv into the
// test file's import list a second time (already present
// transitively via rerankertrainer).
func asDecimal(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestWriteMetrics_renderDeterministic(t *testing.T) {
	// Two back-to-back renders against the same Metrics MUST
	// produce byte-identical output. The Snapshot map iterates
	// in a fixed `counterOrder` slice precisely to guarantee
	// this, and the per-actor sample uses CappedActorSnapshot()
	// which sorts by actor.
	m := rerankertrainer.NewMetrics()
	m.IncRuns()
	m.AddPositivePairs(3)
	m.AddCappedActor("zeta", 1)
	m.AddCappedActor("alpha", 2)
	m.SetLastTrainedAt(time.Unix(1_700_000_000, 0))

	render := func() string {
		r := httptest.NewRecorder()
		writeMetrics(r, m)
		return r.Body.String()
	}
	a, b := render(), render()
	if a != b {
		t.Fatalf("writeMetrics output not deterministic:\nA:\n%s\nB:\n%s", a, b)
	}
	// Sanity: actors sorted alpha-before-zeta in the
	// canonical-name family.
	canon := rerankertrainer.MetricRerankerCappedActorTotal
	idxAlpha := strings.Index(a, canon+`{actor="alpha"}`)
	idxZeta := strings.Index(a, canon+`{actor="zeta"}`)
	if idxAlpha < 0 || idxZeta < 0 || idxAlpha > idxZeta {
		t.Fatalf("CappedActorSnapshot must sort actors alpha-before-zeta; "+
			"idxAlpha=%d idxZeta=%d body=\n%s", idxAlpha, idxZeta, a)
	}
}

// ────────────────────────────────────────────────────────────
// waitForShutdown — graceful-exit regression coverage
// ────────────────────────────────────────────────────────────

// mockShutdowner is a tiny stub of *http.Server's shutdown
// surface. It records whether Shutdown/Close were called so the
// regression tests can assert the graceful path was actually
// walked. When Shutdown fires it writes ErrServerClosed into the
// bound serveErr channel to simulate a real ListenAndServe
// returning post-Shutdown.
type mockShutdowner struct {
	serveErr        chan<- error
	shutdownCalled  atomic.Int32
	closeCalled     atomic.Int32
	shutdownErr     error
	postShutdownErr error
}

func (m *mockShutdowner) Shutdown(_ context.Context) error {
	m.shutdownCalled.Add(1)
	if m.serveErr != nil {
		select {
		case m.serveErr <- m.postShutdownErr:
		default:
		}
	}
	return m.shutdownErr
}

func (m *mockShutdowner) Close() error {
	m.closeCalled.Add(1)
	return nil
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWaitForShutdown_signalPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{serveErr: serveErr, postShutdownErr: http.ErrServerClosed}

	cancel()
	go func() { runErr <- context.Canceled }()

	code := waitForShutdown(ctx, mock, serveErr, runErr, cancel,
		2*time.Second, silentLogger())
	if code != 0 {
		t.Fatalf("exit code: got %d want 0", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("Shutdown called %d times; want 1", got)
	}
	if got := mock.closeCalled.Load(); got != 0 {
		t.Fatalf("Close should not be called on clean shutdown; got %d", got)
	}
}

// TestWaitForShutdown_runErrCanceledStillCallsShutdown locks down
// the same SIGINT-race iter-8 finding #3 from the sibling
// consolidator binary. runErr carrying context.Canceled MUST
// still invoke srv.Shutdown so in-flight scrapes drain.
func TestWaitForShutdown_runErrCanceledStillCallsShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{serveErr: serveErr, postShutdownErr: http.ErrServerClosed}

	runErr <- context.Canceled

	code := waitForShutdown(ctx, mock, serveErr, runErr, cancel,
		2*time.Second, silentLogger())
	if code != 0 {
		t.Fatalf("exit code: got %d want 0", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("SIGINT-race regression: Shutdown was called %d times "+
			"after runErr<-context.Canceled; want exactly 1 to prove "+
			"the graceful HTTP shutdown path is no longer bypassed", got)
	}
}

func TestWaitForShutdown_runErrGenuineErrorSurfacesExit4(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{serveErr: serveErr, postShutdownErr: http.ErrServerClosed}

	runErr <- errors.New("database driver exploded")

	code := waitForShutdown(ctx, mock, serveErr, runErr, cancel,
		2*time.Second, silentLogger())
	if code != 4 {
		t.Fatalf("exit code: got %d want 4 (genuine run failure)", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("Shutdown not called on genuine run failure (called %d times); "+
			"in-flight scrapes would drop", got)
	}
}

func TestWaitForShutdown_serveErrUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{serveErr: nil} // do NOT auto-write on Shutdown

	serveErr <- errors.New("listener bind failed")

	go func() {
		// Drain the runErr channel after ctx cancels so the
		// post-Shutdown drain loop does not block.
		<-ctx.Done()
		runErr <- context.Canceled
	}()

	code := waitForShutdown(ctx, mock, serveErr, runErr, cancel,
		2*time.Second, silentLogger())
	if code != 4 {
		t.Fatalf("exit code: got %d want 4 (genuine serve failure)", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("Shutdown not called on serve failure (called %d times)", got)
	}
}

// TestWaitForShutdown_shutdownTimeoutFallsBackToClose asserts
// the bounded shutdown path: when srv.Shutdown returns an error
// (typically a deadline-exceeded from the shutCtx timeout), the
// code MUST call srv.Close as a fallback so the listener is
// forced down and the binary does not hang past
// shutdownTimeout.
func TestWaitForShutdown_shutdownTimeoutFallsBackToClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{
		serveErr:    serveErr,
		shutdownErr: context.DeadlineExceeded,
	}
	// Pre-drain runErr so the post-Shutdown drain does not block.
	runErr <- context.Canceled
	// Also pre-load serveErr so the same drain in the
	// serve branch completes without blocking.
	serveErr <- http.ErrServerClosed

	code := waitForShutdown(ctx, mock, serveErr, runErr, cancel,
		50*time.Millisecond, silentLogger())
	if code != 0 {
		t.Fatalf("exit code: got %d want 0 (shutdown error is benign)", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("Shutdown called %d times; want 1", got)
	}
	if got := mock.closeCalled.Load(); got != 1 {
		t.Fatalf("shutdown returned an error so Close MUST be called as "+
			"fallback; got %d", got)
	}
}
