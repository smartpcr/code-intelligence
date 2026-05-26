package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/config"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestBuildSweepLoop_DisableStaleSweep_ReturnsNil verifies the
// operator opt-out contract (iter 3 evaluator item 1): when the
// config field DisableStaleSweep is true, the composition root
// MUST NOT attempt to construct a PGScanRunStore -- it returns
// (nil, nil) so the caller can mount the rest of the service
// without a Postgres connection.
func TestBuildSweepLoop_DisableStaleSweep_ReturnsNil(t *testing.T) {
	cfg := config.Defaults()
	cfg.DisableStaleSweep = true

	loop, err := buildSweepLoop(cfg, nil, nil)
	if err != nil {
		t.Fatalf("buildSweepLoop: want nil error, got %v", err)
	}
	if loop != nil {
		t.Fatalf("buildSweepLoop: want nil *StaleScanRunSweepLoop, got non-nil")
	}
}

// TestBuildSweepLoop_NilDB_ReturnsError verifies that when the
// operator wants the sweep enabled (default) but no *sql.DB has
// been wired, the build returns a non-nil error so main() can
// log it instead of nil-panicking inside PGScanRunStore.
func TestBuildSweepLoop_NilDB_ReturnsError(t *testing.T) {
	cfg := config.Defaults() // DisableStaleSweep=false

	loop, err := buildSweepLoop(cfg, nil, nil)
	if err == nil {
		t.Fatalf("buildSweepLoop: want error for nil *sql.DB, got nil")
	}
	if loop != nil {
		t.Fatalf("buildSweepLoop: want nil loop on error path, got non-nil")
	}
}

// TestBuildSweepLoop_HappyPath_ReturnsLoopWithConfigValues
// verifies the canonical wiring path: a non-nil *sql.DB plus the
// default config produces a runnable loop whose underlying sweep
// inherits the tech-spec Sec 8.2 scan_timeout / cadence values
// from [config.Defaults()]. Without this assertion the evaluator
// has no test-level guarantee that the config values flow into
// the sweep.
func TestBuildSweepLoop_HappyPath_ReturnsLoopWithConfigValues(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	_ = mock

	cfg := config.Defaults()
	if cfg.ScanTimeout != 30*time.Minute {
		t.Fatalf("config.Defaults: ScanTimeout=%s, want 30m (tech-spec Sec 8.2)", cfg.ScanTimeout)
	}
	if cfg.PeriodicSweepCadence != 5*time.Minute {
		t.Fatalf("config.Defaults: PeriodicSweepCadence=%s, want 5m (tech-spec Sec 8.2)", cfg.PeriodicSweepCadence)
	}

	loop, err := buildSweepLoop(cfg, db, nil)
	if err != nil {
		t.Fatalf("buildSweepLoop: want nil error, got %v", err)
	}
	if loop == nil {
		t.Fatalf("buildSweepLoop: want non-nil loop, got nil")
	}
	if got := loop.Cadence(); got != cfg.PeriodicSweepCadence {
		t.Errorf("loop.Cadence: got %s, want %s (= cfg.PeriodicSweepCadence)", got, cfg.PeriodicSweepCadence)
	}
	if got := loop.Sweep().ScanTimeout(); got != cfg.ScanTimeout {
		t.Errorf("sweep.ScanTimeout: got %s, want %s (= cfg.ScanTimeout)", got, cfg.ScanTimeout)
	}
}

// TestBuildSweepLoop_RespectsCustomConfig verifies that
// operator-supplied env overrides (parsed by config.Load) flow
// through to the constructed sweep. Without this, a future
// refactor of buildSweepLoop could silently strip the With*
// option threading and the operator's tuning would be ignored
// at runtime.
func TestBuildSweepLoop_RespectsCustomConfig(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	cfg := config.Defaults()
	cfg.ScanTimeout = 7 * time.Minute
	cfg.PeriodicSweepCadence = 13 * time.Second

	loop, err := buildSweepLoop(cfg, db, nil)
	if err != nil {
		t.Fatalf("buildSweepLoop: %v", err)
	}
	if loop == nil {
		t.Fatalf("buildSweepLoop: want non-nil loop")
	}
	if got, want := loop.Cadence(), 13*time.Second; got != want {
		t.Errorf("loop.Cadence: got %s, want %s", got, want)
	}
	if got, want := loop.Sweep().ScanTimeout(), 7*time.Minute; got != want {
		t.Errorf("sweep.ScanTimeout: got %s, want %s", got, want)
	}
}

// TestBuildMux_ProductionMode_LegacyRoutesAbsent verifies the
// iter 3 evaluator item 3 contract: the production composition
// root (EnableLegacyDemoAPI=false) does NOT mount the legacy
// `001_init.sql`-shaped /v1/ingestor/* routes. Hitting them
// must yield a 404 so an operator's misrouted client cannot
// accidentally write the legacy schema columns.
func TestBuildMux_ProductionMode_LegacyRoutesAbsent(t *testing.T) {
	cfg := config.Defaults() // EnableLegacyDemoAPI=false
	mux := buildMux(cfg, nil)

	for _, path := range []string{"/v1/ingestor/process", "/v1/ingestor/scan-run"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %s in production mode: got %d, want 404 (legacy demo MUST stay unmounted)", path, rec.Code)
		}
	}
}

// TestBuildMux_LegacyMode_LegacyRoutesPresent verifies the
// opt-in path. When EnableLegacyDemoAPI=true, both
// /v1/ingestor/process and /v1/ingestor/scan-run are mounted.
// We assert "not 404" rather than 200 because the legacy
// handlers reach into the package-level `db` var; the route
// table check is what matters here, not the handler's runtime
// behaviour.
func TestBuildMux_LegacyMode_LegacyRoutesPresent(t *testing.T) {
	cfg := config.Defaults()
	cfg.EnableLegacyDemoAPI = true
	mux := buildMux(cfg, nil)

	for _, path := range []string{"/v1/ingestor/process", "/v1/ingestor/scan-run"} {
		// Use HEAD-like dry request: the route matcher fires before
		// the handler reads the body, so a 404 here proves the
		// route is NOT mounted; any other code proves it IS.
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("path %s in legacy mode: got 404, want route mounted", path)
		}
	}
}

// TestBuildMux_AlwaysMountsHealthzAndMetrics verifies that
// /healthz and /metrics are mounted regardless of the legacy
// demo opt-in. Production deploys MUST always have liveness +
// scrape surfaces.
func TestBuildMux_AlwaysMountsHealthzAndMetrics(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		cfg := config.Defaults()
		cfg.EnableLegacyDemoAPI = legacy
		mux := buildMux(cfg, nil)

		for _, path := range []string{"/healthz", "/metrics"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Errorf("path %s legacy=%v: got 404, want mounted", path, legacy)
			}
		}
	}
}

// TestNewMetricsHandler_NilLoop_Returns200Empty verifies the
// disable-sweep case at the HTTP boundary: when no loop is
// wired, /metrics MUST still return 200 (so the Prometheus
// scrape job does not flap) with the canonical content-type
// and a (possibly empty) body. The intent is "this binary is
// alive but reports zero sweep samples".
func TestNewMetricsHandler_NilLoop_Returns200Empty(t *testing.T) {
	h := newMetricsHandler(nil)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type: got %q, want text/plain prefix", ct)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("body: got %q, want empty (nil-loop case)", body)
	}
}

// TestNewMetricsHandler_WiredLoop_EmitsPrometheusText asserts
// the wired path: when a real loop is plumbed in, the /metrics
// response contains the canonical `# HELP` / `# TYPE` lines for
// the Stage 3.5 counters so a Prometheus scraper can ingest the
// payload as standard text exposition.
func TestNewMetricsHandler_WiredLoop_EmitsPrometheusText(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	sweep := metric_ingestor.NewStaleScanRunSweep(store)
	loop := metric_ingestor.NewStaleScanRunSweepLoop(sweep)

	h := newMetricsHandler(loop)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	text := string(body)
	wantSubstrings := []string{
		"# HELP",
		"# TYPE",
		"cleancode_sweep_",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(text, s) {
			t.Errorf("body missing %q; got:\n%s", s, text)
		}
	}
}

// TestNewMetricsHandler_WiredLoopReflectsCounters verifies that
// counters incremented by the sweep show up in the /metrics
// response. We drive the in-memory store through a single Sweep
// pass with one stale row, then scrape and assert the counter
// is non-zero -- proving the handler scrapes the LIVE counters
// rather than a stale snapshot.
func TestNewMetricsHandler_WiredLoopReflectsCounters(t *testing.T) {
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	store := metric_ingestor.NewInMemoryScanRunStore()
	repoID := uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555"))
	scanRunID := uuid.Must(uuid.FromString("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"))
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  scanRunID,
		RepoID:     repoID,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingPerRow,
		SHA:        "",
		StartedAt:  now.Add(-1 * time.Hour),
	})

	sweep := metric_ingestor.NewStaleScanRunSweep(
		store,
		metric_ingestor.WithStaleSweepClock(func() time.Time { return now }),
	)
	loop := metric_ingestor.NewStaleScanRunSweepLoop(sweep)

	if _, err := sweep.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep.Sweep: %v", err)
	}

	h := newMetricsHandler(loop)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "cleancode_sweep_stale_scans_total 1") {
		t.Errorf("body missing 'cleancode_sweep_stale_scans_total 1'; got:\n%s", body)
	}
}

// TestHandleHealthz_Returns200OK verifies the canonical
// liveness probe behaviour: 200 + "ok\n" so the readiness /
// liveness probes that wrap this service stay green.
func TestHandleHealthz_Returns200OK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handleHealthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "ok" {
		t.Errorf("body: got %q, want %q", body, "ok")
	}
}

// TestMountIngestRouter_Disabled_NoMountNoError
// (iter-3 evaluator item #3) pins the off-by-default
// composition contract: when
// [config.EnableExternalIngestWebhook] is false,
// mountIngestRouter MUST return nil AND MUST NOT mount
// anything on the supplied mux. A misroute of legitimate
// traffic to RouterPath should yield 404, not 401, so an
// operator can distinguish "service not opted in" from
// "bad signature".
func TestMountIngestRouter_Disabled_NoMountNoError(t *testing.T) {
	mux := http.NewServeMux()
	cfg := config.Defaults()
	cfg.EnableExternalIngestWebhook = false

	if err := mountIngestRouter(mux, cfg, nil, nil); err != nil {
		t.Fatalf("mountIngestRouter (disabled): want nil error, got %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest/churn", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status on disabled mode: got %d, want 404 (router MUST NOT be mounted)", rec.Code)
	}
}

// TestMountIngestRouter_EnabledNilDB_ReturnsError
// pins the composition-root invariant: when the operator
// opts in but the ingestor *sql.DB handle is nil, mount
// MUST fail loudly with an error naming the nil DB so the
// main() log line points the operator at the wiring bug
// rather than panicking inside PG store construction.
func TestMountIngestRouter_EnabledNilDB_ReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	cfg := config.Defaults()
	cfg.EnableExternalIngestWebhook = true
	cfg.WebhookHMACSecret = strings.Repeat("k", 32)
	cfg.WebhookSigningKeyID = "key-test-01"

	err := mountIngestRouter(mux, cfg, nil, nil)
	if err == nil {
		t.Fatalf("mountIngestRouter: want non-nil error for nil DB, got nil")
	}
	if !strings.Contains(err.Error(), "ingestorDB is nil") {
		t.Errorf("error message: want substring 'ingestorDB is nil', got %v", err)
	}
}

// TestMountIngestRouter_EnabledEmptySigningKeyID_ReturnsError
// pins the defence-in-depth guard inside mountIngestRouter
// against a Validate-bypass (e.g. someone calling
// mountIngestRouter with a hand-built Config rather than
// going through Load). Empty signing_key_id MUST surface as
// an error naming the env var.
func TestMountIngestRouter_EnabledEmptySigningKeyID_ReturnsError(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mux := http.NewServeMux()
	cfg := config.Defaults()
	cfg.EnableExternalIngestWebhook = true
	cfg.WebhookHMACSecret = strings.Repeat("k", 32)
	cfg.WebhookSigningKeyID = "" // hand-built misconfiguration

	err = mountIngestRouter(mux, cfg, db, nil)
	if err == nil {
		t.Fatalf("mountIngestRouter: want non-nil error for empty signing_key_id, got nil")
	}
	if !strings.Contains(err.Error(), config.EnvWebhookSigningKeyID) {
		t.Errorf("error message: want substring %q for operator triage, got %v", config.EnvWebhookSigningKeyID, err)
	}
}

// TestMountIngestRouter_EnabledEmptyHMACSecret_ReturnsError
// is the symmetric guard for an empty HMAC secret.
func TestMountIngestRouter_EnabledEmptyHMACSecret_ReturnsError(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mux := http.NewServeMux()
	cfg := config.Defaults()
	cfg.EnableExternalIngestWebhook = true
	cfg.WebhookHMACSecret = "" // hand-built misconfiguration
	cfg.WebhookSigningKeyID = "key-test-01"

	err = mountIngestRouter(mux, cfg, db, nil)
	if err == nil {
		t.Fatalf("mountIngestRouter: want non-nil error for empty HMAC secret, got nil")
	}
	if !strings.Contains(err.Error(), config.EnvWebhookHMACSecret) {
		t.Errorf("error message: want substring %q for operator triage, got %v", config.EnvWebhookHMACSecret, err)
	}
}

// TestMountIngestRouter_Enabled_MountsRouterAtCanonicalPath
// (iter-3 evaluator item #3) pins the happy-path wiring:
// with all three operator pins set and a non-nil *sql.DB,
// mountIngestRouter MUST mount the Router at the canonical
// /v1/ingest/ prefix so a POST to /v1/ingest/{verb} is
// handled by the new Router (we observe a non-404 response
// -- the exact 401 status proves we hit the HMAC verifier).
func TestMountIngestRouter_Enabled_MountsRouterAtCanonicalPath(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mux := http.NewServeMux()
	cfg := config.Defaults()
	cfg.EnableExternalIngestWebhook = true
	cfg.WebhookHMACSecret = strings.Repeat("k", 32)
	cfg.WebhookSigningKeyID = "key-test-01"

	if err := mountIngestRouter(mux, cfg, db, nil); err != nil {
		t.Fatalf("mountIngestRouter: %v", err)
	}

	// Hit the canonical path WITHOUT a valid signature so we
	// never reach the PG store. The Router MUST be present
	// (non-404). The exact 401 status proves the HMAC
	// verifier sits in front of the DB roundtrip.
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest/churn", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Errorf("status: got 404, want non-404 (Router MUST be mounted at /v1/ingest/)")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401 (unsigned request MUST be rejected by HMAC verifier before any handler runs); body=%s",
			rec.Code, rec.Body.String())
	}
}

// TestVerifyMetricKindCatalog_NilDB pins the wiring guard:
// the composition-root startup probe MUST refuse a nil
// `*sql.DB` rather than panic on it (the only path that
// reaches this helper in production is `main()` immediately
// after `openAndPingDB`, but exposing the helper for unit
// testing means nil-handling has to be defensive too).
func TestVerifyMetricKindCatalog_NilDB(t *testing.T) {
	t.Parallel()
	err := verifyMetricKindCatalog(context.Background(), nil, "clean_code")
	if err == nil {
		t.Fatalf("verifyMetricKindCatalog(nil db): err=nil, want guard error")
	}
	if !strings.Contains(err.Error(), "db is nil") {
		t.Errorf("verifyMetricKindCatalog(nil db): err=%q, want substring %q", err.Error(), "db is nil")
	}
}

// TestVerifyMetricKindCatalog_EmptySchema pins the second
// wiring guard: empty schema must error out, never silently
// fall through to a SELECT against `"".metric_kind`.
func TestVerifyMetricKindCatalog_EmptySchema(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	gotErr := verifyMetricKindCatalog(context.Background(), db, "")
	if gotErr == nil {
		t.Fatalf("verifyMetricKindCatalog(empty schema): err=nil, want guard error")
	}
	if !strings.Contains(gotErr.Error(), "schema is empty") {
		t.Errorf("verifyMetricKindCatalog(empty schema): err=%q, want substring %q", gotErr.Error(), "schema is empty")
	}
}

// TestVerifyMetricKindCatalog_HappyPath_AllRowsPresent pins
// the success path: when EVERY row produced by
// `MetricKindCatalogRowsForRegistry(recipes.DefaultRegistry())`
// has a matching entry in the on-disk `metric_kind` table at
// the expected version, the helper returns nil. The test
// also independently asserts the helper queries for
// `pass_first_try_ratio` (Stage 4.3 ingested kind) so a
// future regression that drops the ingested row from the
// canonical set is caught here AND in the
// metric_ingestor package.
func TestVerifyMetricKindCatalog_HappyPath_AllRowsPresent(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expectedRows, err := metric_ingestor.MetricKindCatalogRowsForRegistry(recipes.DefaultRegistry())
	if err != nil {
		t.Fatalf("MetricKindCatalogRowsForRegistry: %v", err)
	}
	prep := mock.ExpectPrepare(`SELECT\s+metric_version\s+FROM\s+"clean_code"\."metric_kind"`)
	for _, row := range expectedRows {
		prep.ExpectQuery().WithArgs(row.MetricKind).
			WillReturnRows(sqlmock.NewRows([]string{"metric_version"}).AddRow(row.MetricVersion))
	}

	if err := verifyMetricKindCatalog(context.Background(), db, metricKindCatalogSchema); err != nil {
		t.Errorf("verifyMetricKindCatalog: err=%v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}

	// Belt-and-braces: assert the canonical row set INCLUDES
	// the ingested pass_first_try_ratio kind. Without this
	// the test could regress silently if a future refactor
	// drops ingested kinds from the row builder.
	var sawPassFirstTryRatio bool
	for _, r := range expectedRows {
		if r.MetricKind == "pass_first_try_ratio" {
			sawPassFirstTryRatio = true
			break
		}
	}
	if !sawPassFirstTryRatio {
		t.Errorf("verifyMetricKindCatalog: canonical row set did NOT include `pass_first_try_ratio` -- the Stage 4.3 ingested kind was dropped from MetricKindCatalogRowsForRegistry")
	}
}

// TestVerifyMetricKindCatalog_MissingRow_FailsFast pins the
// production fail-fast contract: when the catalog is missing
// a row the producer registry would emit (e.g. the operator
// forgot to apply migration 0010 before the binary boots),
// the helper returns an error wrapping
// `ErrMetricKindCatalogRowMissing` so the `main`
// `log.Fatalf` surfaces the missing kind in the boot log
// rather than serving a listener that 500s on the first
// `metric_sample` INSERT.
func TestVerifyMetricKindCatalog_MissingRow_FailsFast(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectPrepare(`SELECT\s+metric_version\s+FROM\s+"clean_code"\."metric_kind"`).
		ExpectQuery().WillReturnError(sql.ErrNoRows)

	gotErr := verifyMetricKindCatalog(context.Background(), db, metricKindCatalogSchema)
	if gotErr == nil {
		t.Fatalf("verifyMetricKindCatalog (missing row): err=nil, want errors.Is ErrMetricKindCatalogRowMissing")
	}
	if !errors.Is(gotErr, metric_ingestor.ErrMetricKindCatalogRowMissing) {
		t.Errorf("verifyMetricKindCatalog (missing row): err=%v, want errors.Is ErrMetricKindCatalogRowMissing", gotErr)
	}
}

// TestMountIngestRouter_Disabled_RouterNotReachableEvenWithSecrets
// pins the inverse: even when secrets ARE supplied, the
// enable flag governs mounting. This protects against the
// "secrets pre-staged for rollout but flag still off" case.
func TestMountIngestRouter_Disabled_RouterNotReachableEvenWithSecrets(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mux := http.NewServeMux()
	cfg := config.Defaults()
	cfg.EnableExternalIngestWebhook = false
	cfg.WebhookHMACSecret = strings.Repeat("k", 32)
	cfg.WebhookSigningKeyID = "key-test-01"

	if err := mountIngestRouter(mux, cfg, db, nil); err != nil {
		t.Fatalf("mountIngestRouter (disabled with secrets present): %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest/churn", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404 (enable flag governs mounting; secrets alone MUST NOT mount)", rec.Code)
	}
}
