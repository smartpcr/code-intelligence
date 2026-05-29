package composition

import (
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/coverage"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/test_balance"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/webhook"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/materialisers"
)

// IngestRouterConfig is the input bundle for
// [BuildIngestRouter]. Pinned as a struct so the
// composition root can extend it (e.g. multi-key HMAC
// resolvers in a future stage) without changing the
// helper's positional signature.
type IngestRouterConfig struct {
	// SigningKeyID names the HMAC key the resolver
	// recognises (the canonical `X-CleanCode-KeyID`
	// header from publishers). Empty is rejected.
	SigningKeyID string

	// HMACSecret is the raw HMAC-SHA256 shared secret.
	// Empty is rejected. A future multi-tenant resolver
	// will replace this single-key map with a per-tenant
	// store.
	HMACSecret string

	// IdempotencyCacheMaxEntries caps the in-process
	// response_body cache. Zero defaults to
	// [DefaultIdempotencyCacheMaxEntries]. Set
	// explicitly to lower the cap for memory-constrained
	// deployments.
	IdempotencyCacheMaxEntries int
}

// DefaultIdempotencyCacheMaxEntries is the cap
// [BuildIngestRouter] installs when the caller leaves
// [IngestRouterConfig.IdempotencyCacheMaxEntries] zero.
// 65 536 entries is the doc-recommended cap on
// `webhook.NewInMemoryIdempotencyStore` (see idempotency.go
// lines 220-226) -- caps the cache at well under 100 MiB
// of resident memory even with maximum-size payloads.
const DefaultIdempotencyCacheMaxEntries = 65536

// BuildIngestRouter assembles the production
// [webhook.Router] for the four canonical ingest verbs
// (`ingest.churn`, `ingest.coverage`, `ingest.test_balance`,
// `ingest.defects`).
//
// The helper takes ONE `*sql.DB` handle authenticated as
// `clean_code_metric_ingestor` -- every store it wires
// runs under that role's grants. A composition root that
// wants to share a handle across roles (dev / E2E) passes
// the shared handle here; the role boundary is enforced
// by the credentials on the handle.
//
// All verbs share the SAME durable [scan_run] lifecycle
// seam (PG-backed `INSERT ON CONFLICT (verb, payload_hash)`)
// so retries across restart/replica short-circuit
// deterministically. The in-process idempotency cache layers
// ON TOP of the durable seam (same-process replay).
//
// Returns a non-nil error if the DB is nil, the signing
// key id is empty, the HMAC secret is empty, or any
// underlying store constructor fails.
func BuildIngestRouter(ingestorDB *sql.DB, cfg IngestRouterConfig, logger *slog.Logger) (*webhook.Router, error) {
	if ingestorDB == nil {
		return nil, fmt.Errorf("BuildIngestRouter: ingestorDB is nil")
	}
	if cfg.SigningKeyID == "" {
		return nil, fmt.Errorf("BuildIngestRouter: SigningKeyID is empty")
	}
	if cfg.HMACSecret == "" {
		return nil, fmt.Errorf("BuildIngestRouter: HMACSecret is empty")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cacheCap := cfg.IdempotencyCacheMaxEntries
	if cacheCap == 0 {
		cacheCap = DefaultIdempotencyCacheMaxEntries
	}

	// Durable scan_run lifecycle seam: PG-backed INSERT
	// ON CONFLICT against migration 0009 partial unique
	// index `scan_run_payload_hash_verb_uniq`.
	extStore, err := metric_ingestor.NewPGExternalScanRunStore(ingestorDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGExternalScanRunStore: %w", err)
	}
	scanRunRepo := webhook.NewPGScanRunRepository(extStore)

	idempotencyStore := webhook.NewInMemoryIdempotencyStore(cacheCap)
	resolver := webhook.NewStaticSecretResolver(map[string][]byte{
		cfg.SigningKeyID: []byte(cfg.HMACSecret),
	})

	churnEventStore, err := churn.NewPGChurnEventStore(ingestorDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGChurnEventStore: %w", err)
	}
	churnIngester := churn.NewIngester(churnEventStore)
	churnHandler := webhook.NewChurnVerbHandler(churnIngester)

	// Coverage + test_balance share the PG-backed
	// MetricSampleWriter. The churn handler bypasses it
	// (staging into clean_code.churn_event instead) but
	// the legacy Ingestor is still wired because the
	// coverage handler dispatches through it.
	sampleWriter, err := metric_ingestor.NewPGMetricSampleWriter(ingestorDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGMetricSampleWriter: %w", err)
	}
	mat := materialisers.NewMaterialiser(materialisers.DefaultWindowDays)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	churnSweep := metric_ingestor.NewChurnSweep(mat, hyd, sampleWriter)

	repoURLLookup, err := metric_ingestor.NewPGRepoURLLookup(ingestorDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGRepoURLLookup: %w", err)
	}
	covResolver, err := coverage.NewPGScopeResolver(ingestorDB, repoURLLookup.LookupRepoURL)
	if err != nil {
		return nil, fmt.Errorf("coverage.NewPGScopeResolver: %w", err)
	}
	covHydrator := coverage.NewHydrator(covResolver).WithSkipLogger(logger)
	covSweep := metric_ingestor.NewCoverageSweep(covHydrator, sampleWriter).
		EnsureCoverageSkipLoggerAttached(logger)

	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, churnSweep).
		WithCoverageSweep(covSweep)
	coverageHandler := webhook.NewCoverageVerbHandler(ing)

	testBalanceScopeResolver, err := metric_ingestor.NewPGScopeBindingResolver(ingestorDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGScopeBindingResolver(test_balance): %w", err)
	}
	testBalanceWriter := test_balance.NewWriter(sampleWriter, testBalanceScopeResolver)
	testBalanceHandler := webhook.NewTestBalanceVerbHandler(testBalanceWriter)

	// Defects verb: v1 store-only at the scan_run
	// boundary -- no writer dependency. No metric_sample
	// row written by this verb in v1 (tech-spec Sec 4.11
	// row 4 + Sec 10A pin).
	defectsHandler := webhook.NewDefectsVerbHandler()

	router := webhook.NewRouter(webhook.RouterConfig{
		Resolver:    resolver,
		Store:       idempotencyStore,
		ScanRunRepo: scanRunRepo,
		Verbs:       []webhook.VerbHandler{churnHandler, coverageHandler, testBalanceHandler, defectsHandler},
		Logger:      logger,
	})
	return router, nil
}
