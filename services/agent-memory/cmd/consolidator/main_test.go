package main

// Iter-8 evaluator finding #2 + #3 regression coverage. The cmd
// binary's loadConfig + writeMetrics + waitForShutdown helpers
// are nontrivial enough that a future "harmless looking" edit
// can silently regress one of:
//
//   * the env-parsing validation (loadConfig: bad ints become
//     defaults, bad durations become defaults, missing PG_URL
//     succeeds),
//   * the Prometheus exposition shape that downstream collectors
//     key off (writeMetrics: missing HELP/TYPE lines, wrong
//     gauge vs counter, missing the lag sample),
//   * the graceful-shutdown invariant that srv.Shutdown is
//     invoked on EVERY exit path including the SIGINT-race
//     case where runErr<-context.Canceled wins the select
//     (waitForShutdown).
//
// These tests run in `go test ./cmd/consolidator` with NO live
// dependencies (no PostgreSQL, no real HTTP listener).

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

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/consolidator"
)

// allConsolidatorEnv enumerates every env var loadConfig reads.
// Each loadConfig test resets ALL of them to empty before
// re-asserting so a stray value in the developer's shell or the
// CI runner cannot pollute the "defaults" assertion. This is the
// rubber-duck recommendation #8 from the iter-8 plan critique.
var allConsolidatorEnv = []string{
	"AGENT_MEMORY_PG_URL",
	"AGENT_MEMORY_LISTEN_ADDR",
	"AGENT_MEMORY_CONSOLIDATOR_THRESHOLD",
	"AGENT_MEMORY_CONSOLIDATOR_INTERVAL",
	"AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT",
	"AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N",
	"AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL",
	"AGENT_MEMORY_SHUTDOWN_TIMEOUT",
}

// clearEnv blanks every consolidator env var via t.Setenv so a
// pre-existing shell value in CI cannot leak into the test.
// t.Setenv("FOO", "") and os.Unsetenv("FOO") are equivalent for
// os.Getenv (the only reader inside loadConfig), and t.Setenv
// auto-restores the prior value at test teardown.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allConsolidatorEnv {
		t.Setenv(k, "")
	}
}

// ────────────────────────────────────────────────────────────
// loadConfig — happy paths
// ────────────────────────────────────────────────────────────

func TestLoadConfig_defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://test/db")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.PGURL != "postgres://test/db" {
		t.Errorf("PGURL: got %q want postgres://test/db", c.PGURL)
	}
	if c.ListenAddr != ":8086" {
		t.Errorf("ListenAddr: got %q want :8086", c.ListenAddr)
	}
	if c.Threshold != consolidator.DefaultThreshold {
		t.Errorf("Threshold: got %d want %d (DefaultThreshold)",
			c.Threshold, consolidator.DefaultThreshold)
	}
	if c.Interval != consolidator.DefaultRunInterval {
		t.Errorf("Interval: got %v want %v (DefaultRunInterval)",
			c.Interval, consolidator.DefaultRunInterval)
	}
	if c.TickTimeout != consolidator.DefaultTickTimeout {
		t.Errorf("TickTimeout: got %v want %v (DefaultTickTimeout)",
			c.TickTimeout, consolidator.DefaultTickTimeout)
	}
	if c.WakeAfterN != 0 {
		t.Errorf("WakeAfterN default should be 0 (disabled); got %d", c.WakeAfterN)
	}
	if c.WakeCheckInterval != consolidator.DefaultWakeCheckInterval {
		t.Errorf("WakeCheckInterval: got %v want %v",
			c.WakeCheckInterval, consolidator.DefaultWakeCheckInterval)
	}
	if c.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout: got %v want 30s", c.ShutdownTimeout)
	}
}

func TestLoadConfig_overridesApplied(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://overrides/db")
	t.Setenv("AGENT_MEMORY_LISTEN_ADDR", "127.0.0.1:9090")
	t.Setenv("AGENT_MEMORY_CONSOLIDATOR_THRESHOLD", "42")
	t.Setenv("AGENT_MEMORY_CONSOLIDATOR_INTERVAL", "30s")
	t.Setenv("AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT", "2m")
	t.Setenv("AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N", "25")
	t.Setenv("AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL", "750ms")
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
	if c.Threshold != 42 {
		t.Errorf("Threshold: got %d want 42", c.Threshold)
	}
	if c.Interval != 30*time.Second {
		t.Errorf("Interval: got %v want 30s", c.Interval)
	}
	if c.TickTimeout != 2*time.Minute {
		t.Errorf("TickTimeout: got %v want 2m", c.TickTimeout)
	}
	if c.WakeAfterN != 25 {
		t.Errorf("WakeAfterN: got %d want 25", c.WakeAfterN)
	}
	if c.WakeCheckInterval != 750*time.Millisecond {
		t.Errorf("WakeCheckInterval: got %v want 750ms", c.WakeCheckInterval)
	}
	if c.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout: got %v want 15s", c.ShutdownTimeout)
	}
}

func TestLoadConfig_wakeAfterNZeroExplicit(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://x/y")
	t.Setenv("AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N", "0")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.WakeAfterN != 0 {
		t.Errorf("WakeAfterN=0 should be accepted (disable); got %d", c.WakeAfterN)
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
// Bundling all the bad-value paths through one matrix keeps the
// per-case noise low while still asserting that EVERY parser
// branch produces an error (not a silent fallback to defaults).
type errorMatrixCase struct {
	name   string
	envKey string
	value  string
	expect string // substring of the returned error
}

func TestLoadConfig_validationErrors(t *testing.T) {
	cases := []errorMatrixCase{
		// THRESHOLD: must be positive int.
		{"threshold/nonInt", "AGENT_MEMORY_CONSOLIDATOR_THRESHOLD", "abc",
			"AGENT_MEMORY_CONSOLIDATOR_THRESHOLD"},
		{"threshold/zero", "AGENT_MEMORY_CONSOLIDATOR_THRESHOLD", "0",
			"AGENT_MEMORY_CONSOLIDATOR_THRESHOLD"},
		{"threshold/negative", "AGENT_MEMORY_CONSOLIDATOR_THRESHOLD", "-3",
			"AGENT_MEMORY_CONSOLIDATOR_THRESHOLD"},
		// INTERVAL: must parse + be positive.
		{"interval/badParse", "AGENT_MEMORY_CONSOLIDATOR_INTERVAL", "10",
			"AGENT_MEMORY_CONSOLIDATOR_INTERVAL"},
		{"interval/zero", "AGENT_MEMORY_CONSOLIDATOR_INTERVAL", "0s",
			"AGENT_MEMORY_CONSOLIDATOR_INTERVAL"},
		{"interval/negative", "AGENT_MEMORY_CONSOLIDATOR_INTERVAL", "-5s",
			"AGENT_MEMORY_CONSOLIDATOR_INTERVAL"},
		// TICK_TIMEOUT.
		{"tickTimeout/badParse", "AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT", "junk",
			"AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT"},
		{"tickTimeout/zero", "AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT", "0",
			"AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT"},
		{"tickTimeout/negative", "AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT", "-1m",
			"AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT"},
		// WAKE_AFTER_N: must be non-negative int. (Zero is OK,
		// covered by TestLoadConfig_wakeAfterNZeroExplicit.)
		{"wakeAfterN/nonInt", "AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N", "xyz",
			"AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N"},
		{"wakeAfterN/negative", "AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N", "-1",
			"AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N"},
		// WAKE_CHECK_INTERVAL.
		{"wakeCheck/badParse", "AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL", "five",
			"AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL"},
		{"wakeCheck/zero", "AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL", "0s",
			"AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL"},
		{"wakeCheck/negative", "AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL", "-100ms",
			"AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL"},
		// SHUTDOWN_TIMEOUT.
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

// ────────────────────────────────────────────────────────────
// writeMetrics — exposition format
// ────────────────────────────────────────────────────────────

// parseMetric pulls the SAMPLE line for a given metric name out
// of a /metrics text body, ignoring the HELP/TYPE preamble. We
// parse the body line-by-line rather than running strings.Contains
// because a regression that left # HELP behind but dropped the
// actual sample line would slip past a substring check.
func parseMetric(t *testing.T, body, name string) string {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == name {
			return fields[1]
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan body: %v", err)
	}
	t.Fatalf("metric %q sample line not found in body:\n%s", name, body)
	return ""
}

// requireExposition asserts that the Prometheus text body contains
// BOTH the HELP/TYPE preamble AND a parseable sample line for
// every named metric. This is the rubber-duck recommendation #10
// from the iter-8 plan critique: a substring-only assertion would
// pass even if the sample line itself was missing.
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
	if v := parseMetric(t, body, name); v == "" {
		t.Fatalf("metric %q has empty sample value", name)
	}
}

func TestWriteMetrics_zeroState(t *testing.T) {
	m := consolidator.NewMetrics()
	rec := httptest.NewRecorder()
	writeMetrics(rec, m)
	body := rec.Body.String()

	// All counters render with zero value.
	for _, name := range []string{
		consolidator.MetricConsolidatorRunsTotal,
		consolidator.MetricConsolidatorErrorsTotal,
		consolidator.MetricConsolidatorEpisodesScannedTotal,
		consolidator.MetricConsolidatorConceptsCreatedTotal,
		consolidator.MetricConsolidatorVersionsAppendedTotal,
		consolidator.MetricConsolidatorSupportsAppendedTotal,
		consolidator.MetricConsolidatorSyntheticPositivesCreatedTotal,
		consolidator.MetricConsolidatorSyntheticObservationsMirroredTotal,
	} {
		requireExposition(t, body, name, "counter")
		if v := parseMetric(t, body, name); v != "0" {
			t.Errorf("metric %s: got %s want 0", name, v)
		}
	}
	// Gauge is emitted as a float64 ("%g" formatting) so "0" is
	// the expected literal for an unset gauge.
	requireExposition(t, body, consolidator.MetricConsolidatorEpisodeLag, "gauge")
	if v := parseMetric(t, body, consolidator.MetricConsolidatorEpisodeLag); v != "0" {
		t.Errorf("episode_lag gauge: got %s want 0", v)
	}
}

func TestWriteMetrics_seededCountersRender(t *testing.T) {
	m := consolidator.NewMetrics()
	// Seed each counter with a distinct, prime-ish value so a
	// rendering bug that confuses one counter with another would
	// surface as a value mismatch (not a silent zero-vs-zero).
	for i := 0; i < 3; i++ {
		m.IncRuns()
	}
	m.IncErrors()
	m.AddEpisodesScanned(17)
	m.AddConceptsCreated(2)
	m.AddVersionsAppended(5)
	m.AddSupportsAppended(11)
	m.AddSyntheticPositivesCreated(7)
	m.AddSyntheticObservationsMirrored(13)
	m.SetEpisodeLag(2500 * time.Millisecond)

	rec := httptest.NewRecorder()
	writeMetrics(rec, m)
	body := rec.Body.String()

	want := map[string]string{
		consolidator.MetricConsolidatorRunsTotal:                          "3",
		consolidator.MetricConsolidatorErrorsTotal:                        "1",
		consolidator.MetricConsolidatorEpisodesScannedTotal:               "17",
		consolidator.MetricConsolidatorConceptsCreatedTotal:               "2",
		consolidator.MetricConsolidatorVersionsAppendedTotal:              "5",
		consolidator.MetricConsolidatorSupportsAppendedTotal:              "11",
		consolidator.MetricConsolidatorSyntheticPositivesCreatedTotal:     "7",
		consolidator.MetricConsolidatorSyntheticObservationsMirroredTotal: "13",
		// "%g" formatting on 2.5 prints as "2.5"; this guards
		// against a regression that switched the gauge to e.g.
		// integer formatting or millisecond units.
		consolidator.MetricConsolidatorEpisodeLag: "2.5",
	}
	for name, expected := range want {
		typ := "counter"
		if name == consolidator.MetricConsolidatorEpisodeLag {
			typ = "gauge"
		}
		requireExposition(t, body, name, typ)
		if got := parseMetric(t, body, name); got != expected {
			t.Errorf("metric %s: got %s want %s", name, got, expected)
		}
	}
}

func TestWriteMetrics_lagGaugeStableUnderClampedNegative(t *testing.T) {
	// The implementation contract documented on Metrics.LagSeconds:
	// the gauge clamps negative values to zero so a paranoid
	// partial-failure tick that briefly leaves cursor ahead of
	// max(episode) does NOT expose a negative gauge to scrapers.
	m := consolidator.NewMetrics()
	m.SetEpisodeLag(-5 * time.Second)

	rec := httptest.NewRecorder()
	writeMetrics(rec, m)
	body := rec.Body.String()
	if got := parseMetric(t, body, consolidator.MetricConsolidatorEpisodeLag); got != "0" {
		t.Fatalf("negative lag must clamp to 0 in exposition; got %s", got)
	}
}

func TestWriteMetrics_helpAndTypeOrderingDeterministic(t *testing.T) {
	// The writeMetrics doc claims "stable iteration order so a
	// scrape-vs-scrape diff is deterministic". Two back-to-back
	// renders against the same Metrics MUST produce byte-identical
	// output (modulo intervening writes), or downstream diff-based
	// alerting noise would spike on every scrape.
	m := consolidator.NewMetrics()
	m.IncRuns()
	m.AddEpisodesScanned(4)
	m.SetEpisodeLag(time.Second)

	render := func() string {
		r := httptest.NewRecorder()
		writeMetrics(r, m)
		return r.Body.String()
	}
	a, b := render(), render()
	if a != b {
		t.Fatalf("writeMetrics output not deterministic:\nA:\n%s\nB:\n%s", a, b)
	}
}

// ────────────────────────────────────────────────────────────
// waitForShutdown — iter-8 finding #3 regression
// ────────────────────────────────────────────────────────────

// mockShutdowner is a tiny stub of *http.Server's shutdown
// surface. It records whether Shutdown/Close were called so the
// regression test can assert the graceful path was actually
// walked. When Shutdown fires it writes ErrServerClosed into
// the bound serveErr channel to simulate a real ListenAndServe
// returning post-Shutdown -- this lets the drain in
// waitForShutdown complete without a real network listener.
type mockShutdowner struct {
	serveErr        chan<- error
	shutdownCalled  atomic.Int32
	closeCalled     atomic.Int32
	shutdownErr     error
	postShutdownErr error // value pushed to serveErr on Shutdown
}

func (m *mockShutdowner) Shutdown(_ context.Context) error {
	m.shutdownCalled.Add(1)
	// Simulate the ListenAndServe goroutine returning -- the
	// production code's go func() { serveErr <- srv.ListenAndServe() }()
	// fires exactly this kind of write after srv.Shutdown unblocks
	// the listener.
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

// TestWaitForShutdown_signalPath: SIGINT-style exit via ctx.Done().
// Asserts the documented graceful sequence: Shutdown is called
// exactly once and the exit code is zero.
func TestWaitForShutdown_signalPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{serveErr: serveErr, postShutdownErr: http.ErrServerClosed}

	// Pre-cancel: simulates signal.NotifyContext firing.
	cancel()
	// runErr will fire as the run-loop sees ctx.Done().
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

// TestWaitForShutdown_runErrCanceledStillCallsShutdown: the
// EXACT iter-8 finding #3 regression. SIGINT cancels ctx AND the
// run loop returns context.Canceled. The select inside
// waitForShutdown picks runErr (engineered: runErr is pre-loaded,
// ctx is canceled later in the goroutine so runErr wins
// reliably). Pre-iter-8 code took `return` here and skipped
// srv.Shutdown entirely. Post-fix: Shutdown MUST be called.
func TestWaitForShutdown_runErrCanceledStillCallsShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{serveErr: serveErr, postShutdownErr: http.ErrServerClosed}

	// Pre-load runErr so the select inside waitForShutdown picks
	// it deterministically (the OTHER two cases never become ready
	// until we cancel ctx or write to serveErr, neither of which
	// happens until after waitForShutdown is already past the
	// select).
	runErr <- context.Canceled

	code := waitForShutdown(ctx, mock, serveErr, runErr, cancel,
		2*time.Second, silentLogger())
	if code != 0 {
		t.Fatalf("exit code: got %d want 0", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("ITER-8 #3 REGRESSION: Shutdown was called %d times "+
			"after runErr<-context.Canceled; want exactly 1 to "+
			"prove the graceful HTTP shutdown path is no longer "+
			"bypassed on the SIGINT-race", got)
	}
}

// TestWaitForShutdown_runErrGenuineErrorSurfacesExit4: a non-Canceled
// runErr signals a real run-loop failure. waitForShutdown must
// STILL call Shutdown gracefully (no scrape mid-drop) but return
// exit code 4 so the binary surfaces the failure to its
// supervisor.
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

// TestWaitForShutdown_serveErrUnexpectedExit: serveErr fires
// FIRST (HTTP server died for some reason other than Shutdown).
// waitForShutdown must NOT call Shutdown a second time (serveErr
// already drained) but should drain runErr after canceling ctx.
func TestWaitForShutdown_serveErrUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{serveErr: nil}

	serveErr <- errors.New("listener bind failed")

	go func() {
		// Run loop should see ctx canceled (waitForShutdown calls
		// cancelCtx in the serveErr branch); push a benign Canceled
		// so the drain wait below completes promptly.
		<-ctx.Done()
		runErr <- context.Canceled
	}()

	code := waitForShutdown(ctx, mock, serveErr, runErr, cancel,
		2*time.Second, silentLogger())
	if code != 4 {
		t.Fatalf("exit code: got %d want 4 (serve failure)", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("Shutdown should still be called even after serveErr "+
			"to keep the exit path uniform; got %d", got)
	}
}

// TestWaitForShutdown_shutdownTimeoutFallsBackToClose: if
// srv.Shutdown returns a non-nil error (typically: shutCtx
// deadline expired with in-flight requests), waitForShutdown
// falls back to srv.Close so the listener actually closes and
// the serveErr drain does not wait past the shutdown timeout.
func TestWaitForShutdown_shutdownTimeoutFallsBackToClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{
		serveErr:        serveErr,
		shutdownErr:     context.DeadlineExceeded,
		postShutdownErr: http.ErrServerClosed,
	}

	cancel()
	go func() { runErr <- context.Canceled }()

	code := waitForShutdown(ctx, mock, serveErr, runErr, cancel,
		500*time.Millisecond, silentLogger())
	if code != 0 {
		t.Fatalf("exit code: got %d want 0", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("Shutdown calls: got %d want 1", got)
	}
	if got := mock.closeCalled.Load(); got != 1 {
		t.Fatalf("Close fallback NOT invoked after Shutdown error; "+
			"got close-calls=%d want 1 (otherwise the listener could "+
			"hang past shutdown_timeout)", got)
	}
}
