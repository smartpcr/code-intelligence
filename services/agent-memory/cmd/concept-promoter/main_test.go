package main

// Cmd-binary unit tests for the concept-promoter worker.
// Mirrors cmd/consolidator/main_test.go shape exactly so the
// two binaries' loadConfig / writeMetrics / waitForShutdown
// helpers stay in lockstep. NO live dependencies: no Postgres,
// no Qdrant, no real HTTP listener.

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

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/promoter"
)

// allPromoterEnv enumerates every env var loadConfig reads.
// Each loadConfig test resets ALL of them to empty before
// re-asserting so a stray value in the developer's shell or
// CI runner cannot pollute the "defaults" assertion.
var allPromoterEnv = []string{
	"AGENT_MEMORY_PG_URL",
	"AGENT_MEMORY_QDRANT_URL",
	"AGENT_MEMORY_QDRANT_API_KEY",
	"AGENT_MEMORY_ALLOW_STUB_EMBEDDER",
	"AGENT_MEMORY_PROMOTER_LISTEN_ADDR",
	"AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD",
	"AGENT_MEMORY_PROMOTER_SUPPORT_THRESHOLD",
	"AGENT_MEMORY_PROMOTER_INTERVAL",
	"AGENT_MEMORY_PROMOTER_TICK_TIMEOUT",
	"AGENT_MEMORY_PROMOTER_CANDIDATE_BATCH_SIZE",
	"AGENT_MEMORY_PROMOTER_RETRY_BATCH_SIZE",
	"AGENT_MEMORY_SHUTDOWN_TIMEOUT",
	"AGENT_MEMORY_EMBEDDER",
	"AGENT_MEMORY_EMBEDDER_URL",
	"AGENT_MEMORY_EMBEDDER_MODEL_VERSION",
	"AGENT_MEMORY_EMBEDDER_API_KEY",
	"AGENT_MEMORY_EMBEDDER_TIMEOUT",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allPromoterEnv {
		t.Setenv(k, "")
	}
}

// ────────────────────────────────────────────────────────────
// loadConfig — happy paths
// ────────────────────────────────────────────────────────────

func TestLoadConfig_defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://test/db")
	t.Setenv("AGENT_MEMORY_QDRANT_URL", "http://qdrant:6333")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.PGURL != "postgres://test/db" {
		t.Errorf("PGURL: got %q want postgres://test/db", c.PGURL)
	}
	if c.QdrantURL != "http://qdrant:6333" {
		t.Errorf("QdrantURL: got %q", c.QdrantURL)
	}
	if c.QdrantAPIKey != "" {
		t.Errorf("QdrantAPIKey default should be empty; got %q", c.QdrantAPIKey)
	}
	if c.AllowStubEmbedder {
		t.Errorf("AllowStubEmbedder default should be false")
	}
	if c.ListenAddr != ":8087" {
		t.Errorf("ListenAddr: got %q want :8087", c.ListenAddr)
	}
	if c.ConfidenceThreshold != promoter.DefaultConfidenceThreshold {
		t.Errorf("ConfidenceThreshold: got %v want %v",
			c.ConfidenceThreshold, promoter.DefaultConfidenceThreshold)
	}
	if c.SupportThreshold != promoter.DefaultSupportThreshold {
		t.Errorf("SupportThreshold: got %d want %d",
			c.SupportThreshold, promoter.DefaultSupportThreshold)
	}
	if c.Interval != promoter.DefaultRunInterval {
		t.Errorf("Interval: got %v want %v",
			c.Interval, promoter.DefaultRunInterval)
	}
	if c.TickTimeout != promoter.DefaultTickTimeout {
		t.Errorf("TickTimeout: got %v want %v",
			c.TickTimeout, promoter.DefaultTickTimeout)
	}
	if c.CandidateBatchSize != promoter.DefaultCandidateBatchSize {
		t.Errorf("CandidateBatchSize: got %d want %d",
			c.CandidateBatchSize, promoter.DefaultCandidateBatchSize)
	}
	if c.RetryBatchSize != promoter.DefaultRetryBatchSize {
		t.Errorf("RetryBatchSize: got %d want %d",
			c.RetryBatchSize, promoter.DefaultRetryBatchSize)
	}
	if c.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout: got %v want 30s", c.ShutdownTimeout)
	}
}

func TestLoadConfig_overridesApplied(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://overrides/db")
	t.Setenv("AGENT_MEMORY_QDRANT_URL", "http://q:6333")
	t.Setenv("AGENT_MEMORY_QDRANT_API_KEY", "secret")
	t.Setenv("AGENT_MEMORY_ALLOW_STUB_EMBEDDER", "true")
	t.Setenv("AGENT_MEMORY_PROMOTER_LISTEN_ADDR", "127.0.0.1:9091")
	t.Setenv("AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD", "0.85")
	t.Setenv("AGENT_MEMORY_PROMOTER_SUPPORT_THRESHOLD", "12")
	t.Setenv("AGENT_MEMORY_PROMOTER_INTERVAL", "45s")
	t.Setenv("AGENT_MEMORY_PROMOTER_TICK_TIMEOUT", "3m")
	t.Setenv("AGENT_MEMORY_PROMOTER_CANDIDATE_BATCH_SIZE", "128")
	t.Setenv("AGENT_MEMORY_PROMOTER_RETRY_BATCH_SIZE", "8")
	t.Setenv("AGENT_MEMORY_SHUTDOWN_TIMEOUT", "20s")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.PGURL != "postgres://overrides/db" {
		t.Errorf("PGURL: got %q", c.PGURL)
	}
	if c.QdrantURL != "http://q:6333" {
		t.Errorf("QdrantURL: got %q", c.QdrantURL)
	}
	if c.QdrantAPIKey != "secret" {
		t.Errorf("QdrantAPIKey: got %q want secret", c.QdrantAPIKey)
	}
	if !c.AllowStubEmbedder {
		t.Errorf("AllowStubEmbedder should be true")
	}
	if c.ListenAddr != "127.0.0.1:9091" {
		t.Errorf("ListenAddr: got %q", c.ListenAddr)
	}
	if c.ConfidenceThreshold != 0.85 {
		t.Errorf("ConfidenceThreshold: got %v want 0.85", c.ConfidenceThreshold)
	}
	if c.SupportThreshold != 12 {
		t.Errorf("SupportThreshold: got %d want 12", c.SupportThreshold)
	}
	if c.Interval != 45*time.Second {
		t.Errorf("Interval: got %v want 45s", c.Interval)
	}
	if c.TickTimeout != 3*time.Minute {
		t.Errorf("TickTimeout: got %v want 3m", c.TickTimeout)
	}
	if c.CandidateBatchSize != 128 {
		t.Errorf("CandidateBatchSize: got %d want 128", c.CandidateBatchSize)
	}
	if c.RetryBatchSize != 8 {
		t.Errorf("RetryBatchSize: got %d want 8", c.RetryBatchSize)
	}
	if c.ShutdownTimeout != 20*time.Second {
		t.Errorf("ShutdownTimeout: got %v want 20s", c.ShutdownTimeout)
	}
}

// ────────────────────────────────────────────────────────────
// loadConfig — error paths
// ────────────────────────────────────────────────────────────

func TestLoadConfig_missingPGURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_QDRANT_URL", "http://qdrant:6333")
	if _, err := loadConfig(); err == nil ||
		!strings.Contains(err.Error(), "AGENT_MEMORY_PG_URL") {
		t.Fatalf("expected missing-PG_URL error; got %v", err)
	}
}

func TestLoadConfig_missingQdrantURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://x/y")
	if _, err := loadConfig(); err == nil ||
		!strings.Contains(err.Error(), "AGENT_MEMORY_QDRANT_URL") {
		t.Fatalf("expected missing-QDRANT_URL error; got %v", err)
	}
}

type errorMatrixCase struct {
	name   string
	envKey string
	value  string
	expect string
}

func TestLoadConfig_validationErrors(t *testing.T) {
	cases := []errorMatrixCase{
		// ALLOW_STUB_EMBEDDER: must parse as bool.
		{"allowStub/nonBool", "AGENT_MEMORY_ALLOW_STUB_EMBEDDER", "perhaps",
			"AGENT_MEMORY_ALLOW_STUB_EMBEDDER"},
		// CONFIDENCE_THRESHOLD: parseable + (0, 1].
		{"confidence/nonFloat", "AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD", "abc",
			"AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD"},
		{"confidence/zero", "AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD", "0",
			"AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD"},
		{"confidence/negative", "AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD", "-0.5",
			"AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD"},
		{"confidence/aboveOne", "AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD", "1.5",
			"AGENT_MEMORY_PROMOTER_CONFIDENCE_THRESHOLD"},
		// SUPPORT_THRESHOLD: positive int.
		{"support/nonInt", "AGENT_MEMORY_PROMOTER_SUPPORT_THRESHOLD", "xyz",
			"AGENT_MEMORY_PROMOTER_SUPPORT_THRESHOLD"},
		{"support/zero", "AGENT_MEMORY_PROMOTER_SUPPORT_THRESHOLD", "0",
			"AGENT_MEMORY_PROMOTER_SUPPORT_THRESHOLD"},
		{"support/negative", "AGENT_MEMORY_PROMOTER_SUPPORT_THRESHOLD", "-1",
			"AGENT_MEMORY_PROMOTER_SUPPORT_THRESHOLD"},
		// INTERVAL: parseable + positive.
		{"interval/badParse", "AGENT_MEMORY_PROMOTER_INTERVAL", "10",
			"AGENT_MEMORY_PROMOTER_INTERVAL"},
		{"interval/zero", "AGENT_MEMORY_PROMOTER_INTERVAL", "0s",
			"AGENT_MEMORY_PROMOTER_INTERVAL"},
		{"interval/negative", "AGENT_MEMORY_PROMOTER_INTERVAL", "-5s",
			"AGENT_MEMORY_PROMOTER_INTERVAL"},
		// TICK_TIMEOUT.
		{"tickTimeout/badParse", "AGENT_MEMORY_PROMOTER_TICK_TIMEOUT", "junk",
			"AGENT_MEMORY_PROMOTER_TICK_TIMEOUT"},
		{"tickTimeout/zero", "AGENT_MEMORY_PROMOTER_TICK_TIMEOUT", "0",
			"AGENT_MEMORY_PROMOTER_TICK_TIMEOUT"},
		{"tickTimeout/negative", "AGENT_MEMORY_PROMOTER_TICK_TIMEOUT", "-1m",
			"AGENT_MEMORY_PROMOTER_TICK_TIMEOUT"},
		// CANDIDATE_BATCH_SIZE.
		{"candidateBatch/nonInt", "AGENT_MEMORY_PROMOTER_CANDIDATE_BATCH_SIZE", "xx",
			"AGENT_MEMORY_PROMOTER_CANDIDATE_BATCH_SIZE"},
		{"candidateBatch/zero", "AGENT_MEMORY_PROMOTER_CANDIDATE_BATCH_SIZE", "0",
			"AGENT_MEMORY_PROMOTER_CANDIDATE_BATCH_SIZE"},
		{"candidateBatch/neg", "AGENT_MEMORY_PROMOTER_CANDIDATE_BATCH_SIZE", "-1",
			"AGENT_MEMORY_PROMOTER_CANDIDATE_BATCH_SIZE"},
		// RETRY_BATCH_SIZE.
		{"retryBatch/nonInt", "AGENT_MEMORY_PROMOTER_RETRY_BATCH_SIZE", "yy",
			"AGENT_MEMORY_PROMOTER_RETRY_BATCH_SIZE"},
		{"retryBatch/zero", "AGENT_MEMORY_PROMOTER_RETRY_BATCH_SIZE", "0",
			"AGENT_MEMORY_PROMOTER_RETRY_BATCH_SIZE"},
		{"retryBatch/neg", "AGENT_MEMORY_PROMOTER_RETRY_BATCH_SIZE", "-2",
			"AGENT_MEMORY_PROMOTER_RETRY_BATCH_SIZE"},
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
			t.Setenv("AGENT_MEMORY_QDRANT_URL", "http://qdrant:6333")
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
	m := promoter.NewMetrics()
	rec := httptest.NewRecorder()
	writeMetrics(rec, m)
	body := rec.Body.String()

	for _, name := range []string{
		promoter.MetricPromoterRunsTotal,
		promoter.MetricPromoterErrorsTotal,
		promoter.MetricPromoterLockSkippedTotal,
		promoter.MetricPromoterCandidatesEvaluatedTotal,
		promoter.MetricPromoterConceptsPromotedTotal,
		promoter.MetricPromoterPublishFailuresTotal,
		promoter.MetricPromoterRetriesAttemptedTotal,
	} {
		requireExposition(t, body, name, "counter")
		if v := parseMetric(t, body, name); v != "0" {
			t.Errorf("metric %s: got %s want 0", name, v)
		}
	}
	requireExposition(t, body, promoter.MetricPromoterCandidatesPending, "gauge")
	if v := parseMetric(t, body, promoter.MetricPromoterCandidatesPending); v != "0" {
		t.Errorf("candidates_pending gauge: got %s want 0", v)
	}
}

func TestWriteMetrics_seededCountersRender(t *testing.T) {
	m := promoter.NewMetrics()
	for i := 0; i < 3; i++ {
		m.IncRuns()
	}
	m.IncErrors()
	m.IncLockSkipped()
	m.AddCandidatesEvaluated(17)
	m.AddConceptsPromoted(2)
	m.AddPublishFailures(5)
	m.AddRetriesAttempted(11)
	m.SetCandidatesPending(4)

	rec := httptest.NewRecorder()
	writeMetrics(rec, m)
	body := rec.Body.String()

	want := map[string]string{
		promoter.MetricPromoterRunsTotal:                "3",
		promoter.MetricPromoterErrorsTotal:              "1",
		promoter.MetricPromoterLockSkippedTotal:         "1",
		promoter.MetricPromoterCandidatesEvaluatedTotal: "17",
		promoter.MetricPromoterConceptsPromotedTotal:    "2",
		promoter.MetricPromoterPublishFailuresTotal:     "5",
		promoter.MetricPromoterRetriesAttemptedTotal:    "11",
		promoter.MetricPromoterCandidatesPending:        "4",
	}
	for name, expected := range want {
		typ := "counter"
		if name == promoter.MetricPromoterCandidatesPending {
			typ = "gauge"
		}
		requireExposition(t, body, name, typ)
		if got := parseMetric(t, body, name); got != expected {
			t.Errorf("metric %s: got %s want %s", name, got, expected)
		}
	}
}

func TestWriteMetrics_helpAndTypeOrderingDeterministic(t *testing.T) {
	m := promoter.NewMetrics()
	m.IncRuns()
	m.AddCandidatesEvaluated(4)
	m.SetCandidatesPending(2)

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
// waitForShutdown — SIGINT-race regression coverage
// (cloned verbatim from cmd/consolidator/main_test.go shape)
// ────────────────────────────────────────────────────────────

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
		t.Fatalf("SIGINT-race regression: Shutdown called %d times "+
			"after runErr<-context.Canceled; want exactly 1", got)
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
		t.Fatalf("exit code: got %d want 4", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("Shutdown not called on genuine run failure (called %d times)", got)
	}
}

func TestWaitForShutdown_serveErrUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	runErr := make(chan error, 1)
	mock := &mockShutdowner{serveErr: nil}

	serveErr <- errors.New("listener bind failed")

	go func() {
		<-ctx.Done()
		runErr <- context.Canceled
	}()

	code := waitForShutdown(ctx, mock, serveErr, runErr, cancel,
		2*time.Second, silentLogger())
	if code != 4 {
		t.Fatalf("exit code: got %d want 4 (serve failure)", code)
	}
	if got := mock.shutdownCalled.Load(); got != 1 {
		t.Fatalf("Shutdown should still be called even after serveErr; got %d", got)
	}
}

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
		t.Fatalf("Close fallback NOT invoked after Shutdown error; got %d", got)
	}
}

// ────────────────────────────────────────────────────────────
// stubEmbedder — sanity coverage
// ────────────────────────────────────────────────────────────

func TestStubEmbedder_zeroVectorAndModel(t *testing.T) {
	s := stubEmbedder{}
	vec, err := s.Embed(context.Background(), "any content")
	if err != nil {
		t.Fatalf("stub Embed returned error: %v", err)
	}
	if len(vec) != 768 {
		t.Fatalf("stub Embed dim: got %d want 768", len(vec))
	}
	for i, v := range vec {
		if v != 0 {
			t.Fatalf("stub Embed[%d]: got %v want 0", i, v)
		}
	}
	if s.ModelVersion() != "stub-zero-vector@v0" {
		t.Fatalf("stub ModelVersion: got %q", s.ModelVersion())
	}
}

// ────────────────────────────────────────────────────────────
// apiKeyTransport — header injection without leaking into url
// ────────────────────────────────────────────────────────────

type capturingRoundTripper struct {
	lastReq *http.Request
}

func (c *capturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.lastReq = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestAPIKeyTransport_setsHeaderAndDoesNotMutateInput(t *testing.T) {
	base := &capturingRoundTripper{}
	t1 := &apiKeyTransport{key: "topsecret", base: base}

	req, err := http.NewRequest(http.MethodGet, "http://qdrant:6333/collections", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := t1.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()

	if got := base.lastReq.Header.Get("api-key"); got != "topsecret" {
		t.Fatalf("api-key header: got %q want topsecret", got)
	}
	// The contract is that the original req is left intact for
	// retries. The injected header must live on the CLONE only.
	if req.Header.Get("api-key") != "" {
		t.Fatalf("original request header was mutated; got %q on input req", req.Header.Get("api-key"))
	}
	// And ensure the key never landed in the query string.
	if strings.Contains(req.URL.RawQuery, "topsecret") {
		t.Fatalf("api-key leaked into URL query: %q", req.URL.RawQuery)
	}
}

// ────────────────────────────────────────────────────────────
// loadConfig — new embedder env vars (evaluator-2 finding #3)
// ────────────────────────────────────────────────────────────

func TestLoadConfig_embedderHTTPSuccess(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://test/db")
	t.Setenv("AGENT_MEMORY_QDRANT_URL", "http://qdrant:6333")
	t.Setenv("AGENT_MEMORY_EMBEDDER", "http")
	t.Setenv("AGENT_MEMORY_EMBEDDER_URL", "https://embed.example/v1/embed")
	t.Setenv("AGENT_MEMORY_EMBEDDER_MODEL_VERSION", "text-embed@v3")
	t.Setenv("AGENT_MEMORY_EMBEDDER_API_KEY", "shh")
	t.Setenv("AGENT_MEMORY_EMBEDDER_TIMEOUT", "12s")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.EmbedderKind != "http" {
		t.Errorf("EmbedderKind: got %q want http", c.EmbedderKind)
	}
	if c.EmbedderURL != "https://embed.example/v1/embed" {
		t.Errorf("EmbedderURL: got %q", c.EmbedderURL)
	}
	if c.EmbedderModel != "text-embed@v3" {
		t.Errorf("EmbedderModel: got %q", c.EmbedderModel)
	}
	if c.EmbedderAPIKey != "shh" {
		t.Errorf("EmbedderAPIKey: got %q", c.EmbedderAPIKey)
	}
	if c.EmbedderTimeout != 12*time.Second {
		t.Errorf("EmbedderTimeout: got %v want 12s", c.EmbedderTimeout)
	}
}

func TestLoadConfig_embedderUnknownKindRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://test/db")
	t.Setenv("AGENT_MEMORY_QDRANT_URL", "http://qdrant:6333")
	t.Setenv("AGENT_MEMORY_EMBEDDER", "openai") // not in the {stub, http} allowlist

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected loadConfig to reject AGENT_MEMORY_EMBEDDER=openai")
	} else if !strings.Contains(err.Error(), "AGENT_MEMORY_EMBEDDER") {
		t.Fatalf("error should mention the offending env var; got %v", err)
	}
}

func TestLoadConfig_embedderTimeoutInvalidRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://test/db")
	t.Setenv("AGENT_MEMORY_QDRANT_URL", "http://qdrant:6333")
	t.Setenv("AGENT_MEMORY_EMBEDDER_TIMEOUT", "not-a-duration")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected loadConfig to reject malformed AGENT_MEMORY_EMBEDDER_TIMEOUT")
	}
}

func TestLoadConfig_embedderStubBackcompat(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MEMORY_PG_URL", "postgres://test/db")
	t.Setenv("AGENT_MEMORY_QDRANT_URL", "http://qdrant:6333")
	// Back-compat path: when AGENT_MEMORY_EMBEDDER is unset
	// but ALLOW_STUB_EMBEDDER=true, EmbedderKind should
	// resolve to "stub" so older deploy configs continue
	// to work without an explicit AGENT_MEMORY_EMBEDDER=stub
	// addition.
	t.Setenv("AGENT_MEMORY_ALLOW_STUB_EMBEDDER", "true")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.EmbedderKind != "stub" {
		t.Errorf("EmbedderKind: got %q want stub (back-compat default)", c.EmbedderKind)
	}
	if !c.AllowStubEmbedder {
		t.Errorf("AllowStubEmbedder: got false want true")
	}
}

// ────────────────────────────────────────────────────────────
// httpEmbedder — production embedder behaviour
// (evaluator-2 finding #3)
// ────────────────────────────────────────────────────────────

func TestHTTPEmbedder_postsContentAndReturnsVector(t *testing.T) {
	var (
		gotBody   string
		gotAuth   string
		gotMethod string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = io.WriteString(w,
			`{"vector":[0.1,0.2,0.3],"model_version":"text-embed-3-small"}`)
	}))
	defer srv.Close()

	cfg := config{
		EmbedderKind:    "http",
		EmbedderURL:     srv.URL,
		EmbedderAPIKey:  "topsecret",
		EmbedderTimeout: 5 * time.Second,
	}
	emb, err := newHTTPEmbedder(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newHTTPEmbedder: %v", err)
	}

	vec, err := emb.Embed(context.Background(), "the brown fox")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Fatalf("vector mismatch: got %v", vec)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotAuth != "Bearer topsecret" {
		t.Errorf("Authorization: got %q want 'Bearer topsecret'", gotAuth)
	}
	if !strings.Contains(gotBody, `"content":"the brown fox"`) {
		t.Errorf("request body should contain content; got %q", gotBody)
	}
	// First successful response cached the upstream's
	// model_version. ModelVersion() should reflect that
	// rather than the unset pinned value.
	if mv := emb.ModelVersion(); mv != "text-embed-3-small" {
		t.Errorf("ModelVersion: got %q want text-embed-3-small (cached from response)", mv)
	}
}

func TestHTTPEmbedder_pinnedModelOverridesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w,
			`{"vector":[1.0],"model_version":"this-should-be-ignored"}`)
	}))
	defer srv.Close()

	emb, err := newHTTPEmbedder(config{
		EmbedderKind:    "http",
		EmbedderURL:     srv.URL,
		EmbedderModel:   "pinned@v1",
		EmbedderTimeout: 5 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newHTTPEmbedder: %v", err)
	}
	if _, err := emb.Embed(context.Background(), "hi"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if mv := emb.ModelVersion(); mv != "pinned@v1" {
		t.Errorf("ModelVersion: got %q want pinned@v1 (operator pin must override upstream)", mv)
	}
}

func TestHTTPEmbedder_non2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	emb, err := newHTTPEmbedder(config{
		EmbedderKind:    "http",
		EmbedderURL:     srv.URL,
		EmbedderTimeout: 5 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newHTTPEmbedder: %v", err)
	}
	_, err = emb.Embed(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error on non-2xx response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code; got %v", err)
	}
}

func TestHTTPEmbedder_emptyVectorRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"vector":[],"model_version":"v1"}`)
	}))
	defer srv.Close()

	emb, err := newHTTPEmbedder(config{
		EmbedderKind:    "http",
		EmbedderURL:     srv.URL,
		EmbedderTimeout: 5 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newHTTPEmbedder: %v", err)
	}
	if _, err := emb.Embed(context.Background(), "hi"); err == nil {
		t.Fatal("expected error when upstream returns empty vector")
	}
}

func TestNewHTTPEmbedder_missingURLRejected(t *testing.T) {
	if _, err := newHTTPEmbedder(config{EmbedderKind: "http"},
		slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected error when EmbedderURL is empty")
	}
}

// ────────────────────────────────────────────────────────────
// evaluator-4 finding #2: promoter.ready conditional log
// ────────────────────────────────────────────────────────────

// fixedModelEmbedder reports a constant ModelVersion(). When
// model is empty, the helper-under-test must emit the
// `embedder_model_status="pending_first_embed"` indicator
// instead of `embedder_model=""`.
type fixedModelEmbedder struct{ model string }

func (f fixedModelEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return make([]float32, 4), nil
}
func (f fixedModelEmbedder) ModelVersion() string { return f.model }

// TestEmbedderModelReadyLogAttr_pinnedEmbedsConcreteModelKey
// pins the contract that a pinned (or any
// already-resolved) embedder ends up in the
// `promoter.ready` log under the `embedder_model` key with
// the resolved version.
func TestEmbedderModelReadyLogAttr_pinnedEmbedsConcreteModelKey(t *testing.T) {
	attr := embedderModelReadyLogAttr(fixedModelEmbedder{model: "upstream-bge@2025-01"})
	if attr.Key != "embedder_model" {
		t.Fatalf("attr.Key: got %q want %q", attr.Key, "embedder_model")
	}
	if got := attr.Value.String(); got != "upstream-bge@2025-01" {
		t.Fatalf("attr.Value: got %q want %q", got, "upstream-bge@2025-01")
	}
}

// TestEmbedderModelReadyLogAttr_unpinnedEmitsPendingStatusKey
// is the evaluator-4 finding #2 regression: when
// ModelVersion() is empty (unpinned HTTP mode at startup,
// before the first bootstrap Embed), the log attr MUST be
// `embedder_model_status="pending_first_embed"`, NOT
// `embedder_model=""` (which an operator could mistake for a
// config problem).
//
// A failure here means the conditional in `main.go`'s
// `promoter.ready` builder has regressed and is once again
// emitting a misleading empty `embedder_model` field for the
// production unpinned HTTP configuration.
func TestEmbedderModelReadyLogAttr_unpinnedEmitsPendingStatusKey(t *testing.T) {
	attr := embedderModelReadyLogAttr(fixedModelEmbedder{model: ""})
	if attr.Key != "embedder_model_status" {
		t.Fatalf("attr.Key: got %q want %q (unpinned mode must NOT log an empty embedder_model — evaluator-4 finding #2 regression)", attr.Key, "embedder_model_status")
	}
	if got := attr.Value.String(); got != "pending_first_embed" {
		t.Fatalf("attr.Value: got %q want %q", got, "pending_first_embed")
	}
}

// TestEmbedderModelReadyLogAttr_whitespaceOnlyTreatedAsUnpinned
// pins the strings.TrimSpace branch — a model reported as
// pure whitespace counts as unpinned (the supersede flow
// can't usefully match on " ").
func TestEmbedderModelReadyLogAttr_whitespaceOnlyTreatedAsUnpinned(t *testing.T) {
	attr := embedderModelReadyLogAttr(fixedModelEmbedder{model: "   \t  "})
	if attr.Key != "embedder_model_status" {
		t.Fatalf("attr.Key: got %q want %q (whitespace-only model must fall through to pending_first_embed)", attr.Key, "embedder_model_status")
	}
}
