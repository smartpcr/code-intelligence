// Package config loads the clean-code service's runtime
// configuration from environment variables (and optionally a YAML
// file via CLEAN_CODE_CONFIG_FILE), exposing the five normative
// operator pins from architecture Sec 1.6 plus the numeric defaults
// from tech-spec Sec 8.2 as typed fields.
//
// Stage 1.1 (implementation-plan.md) carves out two acceptance
// criteria for this package:
//
//  1. The five operator pins -- `ast-mode-default`,
//     `external-metric-coverage-format`, `gate-degraded-policy`,
//     `policy-signing-required`, `refactor-effort-source` -- must
//     surface as typed fields with the operator-pinned defaults
//     when env / file omit them (scenario `config-honours-pins`).
//  2. The loader is the single source of truth -- no other
//     package may read CLEAN_CODE_* env vars directly.
//
// All pin values are intentionally stored as the canonical
// architecture-Sec-1.6 strings (e.g. "embedded", "Cobertura XML")
// rather than enums, so a log line emitted from the loader
// produces output that lexically matches the spec doc and a
// reviewer's `grep -F` lands on the same string in both places.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Canonical default values for the five operator pins
// (architecture Sec 1.6). These constants are the public source of
// truth for the defaults so other packages can reference them in
// switch statements / validators.
const (
	// DefaultASTModeDefault is the default for operator pin
	// `ast-mode-default` (architecture Sec 1.6 row 1): the AST
	// Adapter runs in-process inside the Metric Ingestor.
	DefaultASTModeDefault = "embedded"
	// DefaultExternalMetricCoverageFormat is the default for
	// operator pin `external-metric-coverage-format` (architecture
	// Sec 1.6 row 2): the only `ingest.coverage` payload format
	// in v1.
	DefaultExternalMetricCoverageFormat = "Cobertura XML"
	// DefaultGateDegradedPolicy is the default for operator pin
	// `gate-degraded-policy` (architecture Sec 1.6 row 3):
	// `eval.gate` never blocks on degraded conditions.
	DefaultGateDegradedPolicy = "warn"
	// DefaultPolicySigningRequired is the default for operator
	// pin `policy-signing-required` (architecture Sec 1.6 row 4):
	// the evaluator agent MUST verify the `PolicyVersion.signature`
	// on every `eval.gate` call.
	DefaultPolicySigningRequired = "v1 required"
	// DefaultRefactorEffortSource is the default for operator pin
	// `refactor-effort-source` (architecture Sec 1.6 row 5):
	// effort estimates come from an ML model trained on historical
	// commits.
	DefaultRefactorEffortSource = "ML model from historical commits"
)

// Numeric defaults from tech-spec Sec 8.2.
const (
	// DefaultScanTimeout is the tech-spec Sec 8.2 `scan_timeout`
	// row: 30 min is long enough for a 1M-LOC monorepo at p95
	// parse cost; short enough that an orphaned scan is detected
	// within a single sweep.
	DefaultScanTimeout = 30 * time.Minute
	// DefaultPeriodicSweepCadence is the tech-spec Sec 8.2
	// `periodic_sweep_cadence` row: sweeps `ScanRun` rows in
	// `running` state past `scan_timeout` and transitions them
	// to `failed`.
	DefaultPeriodicSweepCadence = 5 * time.Minute
	// DefaultWindowDays is the tech-spec Sec 8.2 `window_days`
	// row: the commit-window the Metric Ingestor uses to
	// materialise `modification_count_in_window` SOLID input
	// rows on `ingest.churn` arrival.
	DefaultWindowDays = 90
	// DefaultFreshnessWindowSeconds is the tech-spec Sec 8.2
	// `freshness_window_seconds` row: Insights stale-percentile
	// threshold.
	DefaultFreshnessWindowSeconds = 3600
	// DefaultPolicyPublishOverlapSeconds is the tech-spec Sec 8.2
	// `policy_publish_overlap_min_seconds` row: minimum
	// key-rotation overlap (C13 mitigation).
	DefaultPolicyPublishOverlapSeconds = 86400
	// DefaultAggregatorCadence is the tech-spec Sec 8.2
	// `aggregator_cadence` row: the period between
	// Cross-Repo Aggregator ticks. 15 min is shorter than
	// `freshness_window_seconds` (3600) so the Insights
	// surface's percentile freshness banner has 3-4x
	// headroom under nominal load (architecture Sec 3.10).
	DefaultAggregatorCadence = 15 * time.Minute
	// DefaultKMSProvider is the canonical default for the
	// Stage 5.1 `kms-provider` knob. The empty default
	// preserves scaffold-mode startup -- the composition root
	// branches on this value to decide whether to wire the
	// SQL-backed `internal/policy/keys` package against a
	// LocalSealedKMS or stay in-memory. Setting it to
	// `"in-memory"` would force every operator who omits the
	// env var into scaffold mode silently; leaving it empty
	// makes the operator's intent explicit at config-load
	// time.
	DefaultKMSProvider = ""
	// DefaultLinkedAdapterTimeout is the per-request HTTP
	// timeout the Stage 10.1 linked-mode adapter applies to
	// each agent-memory cross-repo-edge fetch (architecture
	// Sec 8.7). 5s matches the agent-memory team's published
	// graph-reader p99 SLO (Sec 6.2.3) with ~5x headroom and
	// is short enough to keep aggregator tick latency
	// bounded on a single agent-memory outage (the
	// aggregator degrades the affected row to
	// `xrepo_edges_unavailable` and moves on rather than
	// blocking the tick).
	DefaultLinkedAdapterTimeout = 5 * time.Second
)

// Default network bind addresses for the empty scaffold. Operators
// override via env in any non-trivial deployment.
const (
	// DefaultHTTPAddr is the bind for the /healthz + /readyz
	// listener. Stage 1.1 acceptance criteria require these
	// endpoints on the primary HTTP listener.
	DefaultHTTPAddr = ":8080"
	// DefaultPrometheusAddr is the dedicated Prometheus scrape
	// listener. Splitting scrape and serve traffic keeps the
	// liveness probe insulated from scrape backpressure.
	DefaultPrometheusAddr = ":9090"
	// DefaultOTelEndpoint is the OTLP gRPC endpoint of the local
	// dev OTel collector (per deploy/local/docker-compose.yml).
	DefaultOTelEndpoint = "localhost:4317"
	// DefaultLogLevel is the slog level emitted by the JSON
	// logger when CLEAN_CODE_LOG_LEVEL is unset.
	DefaultLogLevel = "info"
)

// Env var names. Centralised here so `grep -nF "CLEAN_CODE_"
// services/clean-code/internal/config/` returns the canonical list.
const (
	EnvConfigFile                   = "CLEAN_CODE_CONFIG_FILE"
	EnvASTModeDefault               = "CLEAN_CODE_AST_MODE_DEFAULT"
	EnvExternalMetricCoverageFormat = "CLEAN_CODE_EXTERNAL_COVERAGE_FORMAT"
	EnvGateDegradedPolicy           = "CLEAN_CODE_GATE_DEGRADED_POLICY"
	EnvPolicySigningRequired        = "CLEAN_CODE_POLICY_SIGNING_REQUIRED"
	EnvRefactorEffortSource         = "CLEAN_CODE_REFACTOR_EFFORT_SOURCE"
	// EnvEffortSourceAlias is the compose-shorthand alias for
	// EnvRefactorEffortSource. PR #148's
	// `tests/e2e/phase-08-refactor-planner/docker-compose.yml`
	// uses the short form to stay aligned with `ml-model-uri`
	// and `ml-model-version` env-var lines. Both the canonical
	// and alias spellings populate the same Config field; the
	// canonical name wins if BOTH are set (so an operator who
	// deliberately sets the canonical pin in one place cannot
	// be silently overridden by a compose default).
	EnvEffortSourceAlias = "CLEAN_CODE_EFFORT_SOURCE"
	// EnvMLModelURI carries the URI of the ML effort-model
	// artefact the Stage 9.3 refactor-planner loads when the
	// resolved [EnvRefactorEffortSource] is `ml`. Required in
	// the ML branch; an empty value with `ml` source fails
	// fast at planner construction via
	// [refactor.ErrMLModelURIMissing].
	EnvMLModelURI = "CLEAN_CODE_ML_MODEL_URI"
	// EnvMLModelVersion is the semantic version pin the loaded
	// ML model claims. The planner compares this against
	// `policy_version.refactor_weights.effort_model_version`
	// at every estimate; a drift returns
	// [refactor.ErrMLModelVersionMismatch] and aborts the
	// atomic plan + tasks write so the architecture Sec 5.5.3
	// reproducibility invariant survives operator misconfig.
	EnvMLModelVersion = "CLEAN_CODE_ML_MODEL_VERSION"
	EnvHTTPAddr       = "CLEAN_CODE_HTTP_ADDR"
	EnvPrometheusAddr               = "CLEAN_CODE_PROMETHEUS_ADDR"
	EnvOTelEndpoint                 = "CLEAN_CODE_OTEL_ENDPOINT"
	// EnvAstScanRoot points at the local on-disk root the
	// Metric Ingestor's [DirectoryAstFileSource] walks.
	// Layout convention is `<root>/<repo_id>/<sha>/...`.
	// Empty leaves the source as the
	// [EmptyAstFileSource] scaffold (Phase 4 supplies the
	// PG-backed AST cache that replaces both).
	EnvAstScanRoot = "CLEAN_CODE_AST_SCAN_ROOT"

	// EnvPGURL is the canonical env var name for the PostgreSQL
	// DSN (per docs/stories/code-intelligence-CLEAN-CODE/
	// e2e-scenarios.md table at line 41-49). The Go field on
	// Config is `PostgresURL`; the wire-level env var is
	// `CLEAN_CODE_PG_URL` -- DO NOT rename the env var without
	// also updating e2e-scenarios.md.
	EnvPGURL                       = "CLEAN_CODE_PG_URL"
	// EnvMgmtPGURL is the OPTIONAL libpq DSN the metric-ingestor
	// binary connects to under the `clean_code_management` role
	// for `repo_event` INSERTs (Stage 3.4 retract intent /
	// rescan-related events). The Metric Ingestor's own role
	// (`clean_code_metric_ingestor`) is granted INSERT on
	// `scan_run` and `metric_retraction` ONLY -- repo_event
	// INSERT belongs to the management role per
	// migrations/0004_roles.up.sql lines 313 / 348 / 374.
	//
	// When set, the binary opens a SECOND `*sql.DB` against this
	// DSN and routes `PGRepoEventAppender` writes through it
	// while keeping `PGRetractScanRunStore`, `PGRetractionStore`,
	// and `PGRescanScanRunStore` on the metric-ingestor handle
	// (CLEAN_CODE_PG_URL).
	//
	// When UNSET, the binary refuses to mount the `mgmt.*` write
	// verbs unless [EnvAllowSharedPGRole] is truthy
	// (dev/E2E-only opt-in that re-uses a single DSN across both
	// roles). Production deployments MUST set role-distinct
	// credentials.
	EnvMgmtPGURL = "CLEAN_CODE_MGMT_PG_URL"
	// EnvAllowSharedPGRole is the explicit dev/E2E-mode opt-in
	// for sharing a single DSN across the
	// `clean_code_metric_ingestor` and `clean_code_management`
	// roles. Accepts any boolean literal
	// (1|true|yes|on / 0|false|no|off). Default false.
	//
	// When false (production default), the metric-ingestor
	// binary REFUSES to mount the Stage 3.4 management write
	// verbs unless [EnvMgmtPGURL] is set -- the operator MUST
	// supply role-distinct credentials so a future role-grant
	// audit reflects the documented Sec 7.2 ACL boundary.
	//
	// When true, the binary uses CLEAN_CODE_PG_URL for BOTH
	// roles and logs a WARN at startup. Intended ONLY for local
	// `docker compose` E2E runs where the test fixture uses a
	// single superuser DSN.
	EnvAllowSharedPGRole = "CLEAN_CODE_ALLOW_SHARED_PG_ROLE"
	EnvLogLevel          = "CLEAN_CODE_LOG_LEVEL"
	EnvScanTimeout                 = "CLEAN_CODE_SCAN_TIMEOUT"
	EnvPeriodicSweepCadence        = "CLEAN_CODE_PERIODIC_SWEEP_CADENCE"
	EnvWindowDays                  = "CLEAN_CODE_WINDOW_DAYS"
	EnvFreshnessWindowSeconds      = "CLEAN_CODE_FRESHNESS_WINDOW_SECONDS"
	EnvPolicyPublishOverlapSeconds = "CLEAN_CODE_POLICY_PUBLISH_OVERLAP_SECONDS"
	// EnvAggregatorCadence is the operator-facing knob for
	// the Cross-Repo Aggregator tick period (Stage 7.1,
	// tech-spec Sec 8.2 `aggregator_cadence`). Accepts any
	// [time.ParseDuration] value (e.g. `15m`, `5m`, `1h`).
	// Default: [DefaultAggregatorCadence] (15 min).
	EnvAggregatorCadence = "CLEAN_CODE_AGGREGATOR_CADENCE"
	// EnvDisableAggregator is the explicit operator opt-out
	// for the Stage 7.1 Cross-Repo Aggregator loop. Default
	// false (loop enabled). When true, the composition root
	// SKIPS construction of the aggregator goroutine entirely.
	// Production MUST keep this false; intended for legacy
	// E2E environments that lack the Stage 1.3 snapshot
	// tables.
	EnvDisableAggregator = "CLEAN_CODE_DISABLE_AGGREGATOR"
	// EnvKMSProvider names the operator-facing knob that picks
	// the policy-signing KMS adapter. Closed set: `local` |
	// `in-memory`. Unset leaves the service in scaffold mode
	// (no production key persistence). Per Stage 5.1
	// tech-spec Sec 8.4.
	EnvKMSProvider = "CLEAN_CODE_KMS_PROVIDER"
	// EnvKMSMasterKeyHex is the 64-char lowercase hex
	// encoding of the AES-256 master key the LocalSealedKMS
	// uses to wrap Ed25519 seeds. The composition root
	// reads this once at startup and the value MUST NOT be
	// echoed into any log line. Operators are expected to
	// inject this via their secret manager (env var, k8s
	// Secret, etc.) and never check it into source.
	EnvKMSMasterKeyHex = "CLEAN_CODE_KMS_MASTER_KEY_HEX"

	// EnvWebhookHMACSecret is the shared HMAC-SHA256 secret
	// every external `ingest.*` webhook verifies request
	// bodies against via the `X-Hub-Signature-256` header
	// (tech-spec Sec 8.5 "REST + HMAC-signed";
	// e2e-scenarios.md lines 48/588/602/610 pin this exact
	// env-var name as the SHARED external-ingest secret). The
	// value is the raw secret string (NOT hex-encoded; the
	// hex sits in the header). NEVER logged.
	//
	// # Minimum strength
	//
	// `Validate` rejects any non-empty value shorter than
	// [MinWebhookHMACSecretBytes] (32 bytes -- matches the
	// HMAC-SHA256 output width and the e2e-scenarios.md
	// "32-byte HMAC secret" recommendation at line 588). A
	// one-character secret would defeat the auth boundary;
	// 32 bytes is a practical guard against operator typos
	// and copy-paste truncation. Use a CSPRNG to generate it:
	// `head -c 32 /dev/urandom | base64`.
	//
	// # When unset (default)
	//
	// The webhook is NOT MOUNTED at all in production wiring
	// -- the route returns the standard 404 ("verb does not
	// exist in this build"), keeping an unauthenticated
	// `Ingestor.Run` driver out of production until either
	// the HMAC layer or the Phase 3.12 production hardening
	// lands.
	EnvWebhookHMACSecret = "CLEAN_CODE_WEBHOOK_HMAC_SECRET" //nolint:gosec // env var name, not a credential

	// EnvEnableScaffoldChurnWebhook is the explicit
	// operator-facing opt-in for the Stage 2.6 scaffold-mode
	// churn webhook. It accepts any non-empty value (the
	// canonical form is `true`); setting it constitutes
	// acknowledging the SCAFFOLD-MODE LIMITATION that the
	// webhook persists into an in-memory writer and DATA IS
	// LOST ON RESTART (the production-grade PG-backed writer
	// lands in Phase 3.2). The webhook is mounted iff BOTH
	// this flag AND [EnvWebhookHMACSecret] are set; one
	// without the other is a configuration error that fails
	// fast at startup.
	EnvEnableScaffoldChurnWebhook = "CLEAN_CODE_ENABLE_SCAFFOLD_CHURN_WEBHOOK"

	// EnvEnableScaffoldIndexerWebhook is the explicit
	// operator-facing opt-in for the Stage 3.1 scaffold-mode
	// Repo Indexer webhook + CLI rescan trigger. It accepts
	// any non-empty boolean value (canonical form `true`).
	// Setting it constitutes acknowledging the SCAFFOLD-MODE
	// LIMITATION that when no `CLEAN_CODE_PG_URL` is wired the
	// webhook persists commits + repo_events into an in-memory
	// writer and DATA IS LOST ON RESTART (the production
	// PG-backed [repo_indexer.PGCatalogWriter] is wired
	// automatically when [EnvPGURL] is set).
	//
	// Both `/v1/indexer/webhook` and `/v1/indexer/rescan` are
	// mounted iff this flag AND [EnvWebhookHMACSecret] are
	// set; the indexer reuses the SHARED external-ingest HMAC
	// secret (architecture Sec 8.5 -- every external
	// `ingest.*` and `indexer.*` surface verifies request
	// bodies against the same secret). One without the other
	// is a configuration error that fails fast at startup.
	EnvEnableScaffoldIndexerWebhook = "CLEAN_CODE_ENABLE_SCAFFOLD_INDEXER_WEBHOOK"

	// EnvDisableStaleSweep is the operator-facing opt-out for
	// the Stage 3.5 stale ScanRun sweep loop. It accepts any
	// boolean literal (1|true|yes|on / 0|false|no|off). Default
	// unset = sweep enabled. Set this to a truthy value in
	// environments that still run the legacy `001_init.sql`
	// `scan_run(commit_sha,kind,status,finished_at)` schema --
	// the sweep targets the canonical
	// `0001_catalog_lifecycle` shape and produces repeated
	// UPDATE errors at every cadence tick when pointed at the
	// legacy table. Operators MUST leave the sweep enabled in
	// production (canonical schema).
	//
	// Centralised here per the config-package contract
	// (architecture Sec 1.6 / package doc lines 15-16): no
	// other package reads `CLEAN_CODE_*` env vars directly.
	EnvDisableStaleSweep = "CLEAN_CODE_DISABLE_STALE_SWEEP"

	// EnvEnableLegacyDemoAPI is the operator-facing opt-in for
	// the legacy `001_init.sql`-shaped HTTP surface on the
	// metric-ingestor binary (`/v1/ingestor/process` and
	// `/v1/ingestor/scan-run`). Default unset = NOT MOUNTED;
	// the production composition root exposes only the
	// canonical health/metrics endpoints + the Stage 3.5 sweep
	// loop. Set this in legacy E2E environments that still
	// drive the older `commit_sha`/`finished_at` shape;
	// production deployments MUST leave this unset (the
	// canonical Stage 1.2 `0001_catalog_lifecycle` schema does
	// not have the columns these handlers write).
	EnvEnableLegacyDemoAPI = "CLEAN_CODE_ENABLE_LEGACY_DEMO_API"

	// EnvEnableExternalIngestWebhook is the explicit
	// operator-facing opt-in for the Stage 4.1
	// production-grade `/v1/ingest/{verb}` Router on the
	// metric-ingestor binary. Default unset = NOT MOUNTED
	// (the route returns the standard 404 -- no
	// unauthenticated path can reach Ingestor.Run). Set this
	// to any truthy value to mount the Router; the Router
	// requires BOTH this flag AND [EnvWebhookHMACSecret] AND
	// [EnvWebhookSigningKeyID] to be set -- one without the
	// others is a configuration error that fails fast at
	// startup. The Router persists scan_run rows with
	// `payload_hash` set (migration 0009's partial unique
	// index) so retries across restarts and replicas
	// short-circuit to the prior scan_run_id.
	EnvEnableExternalIngestWebhook = "CLEAN_CODE_ENABLE_EXTERNAL_INGEST_WEBHOOK"

	// EnvWebhookSigningKeyID is the operator-facing key-id
	// the publishers MUST send in the `X-Signing-Key-Id`
	// header. Pinned per deployment; the Router rejects
	// any other key-id with 401 / HMAC_UNKNOWN_KEY_ID.
	// Empty when [EnvEnableExternalIngestWebhook] is unset.
	EnvWebhookSigningKeyID = "CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID" //nolint:gosec // env var name, not a credential

	// EnvEnableLinkedModeAdapter is the explicit operator
	// opt-in for the Stage 10.1 linked-mode adapter (the
	// `internal/linked` HTTP client that wraps the
	// agent-memory cross-repo edge endpoint -- architecture
	// Sec 8.7). Default false: the aggregator runs in
	// embedded-only mode and stamps
	// `degraded_reason='xrepo_edges_unavailable'` for the
	// cross-repo-edge-dependent rows. Setting this true
	// REQUIRES [EnvLinkedAgentMemoryEndpoint] to also be
	// set; the symmetric interlock matches the existing
	// webhook-opt-in pattern.
	//
	// Note that this flag only opens the GLOBAL gate; per-
	// repo opt-in still requires `mgmt.set_mode(repo_id,
	// 'linked')`. The adapter consults both axes before
	// firing an HTTP call.
	EnvEnableLinkedModeAdapter = "CLEAN_CODE_ENABLE_LINKED_MODE_ADAPTER"

	// EnvLinkedAgentMemoryEndpoint is the agent-memory
	// cross-repo-edge service base URL (e.g.
	// `https://agent-memory.internal/`). The
	// `internal/linked` client appends `/v1/cross-repo/edges`
	// at request time. MUST be a parseable http(s) URL when
	// set; empty leaves the linked-mode adapter unwired.
	// Interlocks with [EnvEnableLinkedModeAdapter].
	EnvLinkedAgentMemoryEndpoint = "CLEAN_CODE_LINKED_AGENT_MEMORY_ENDPOINT"

	// EnvLinkedAdapterTimeout overrides the linked-mode
	// adapter's per-request HTTP timeout. Accepts any value
	// `time.ParseDuration` accepts (`5s`, `750ms`, etc.).
	// Default [DefaultLinkedAdapterTimeout]. A non-positive
	// value is rejected with a hard error at config-load
	// time to surface operator typos before runtime.
	EnvLinkedAdapterTimeout = "CLEAN_CODE_LINKED_ADAPTER_TIMEOUT"
)

// MinWebhookHMACSecretBytes is the minimum length (in bytes,
// i.e. `len(string)`) the loader enforces on
// [EnvWebhookHMACSecret] when it is set. Matches the
// HMAC-SHA256 output width and the e2e-scenarios.md "32-byte
// HMAC secret" guidance (line 588). Pinned as a const so a
// future operator pin to a different floor is a one-line
// change.
const MinWebhookHMACSecretBytes = 32

// Config is the in-memory shape of the service's runtime
// configuration. Every field is exported so wired packages can
// reference it directly without a getter call per field.
type Config struct {
	// --- Operator pins (architecture Sec 1.6) ---

	// ASTModeDefault carries the `ast-mode-default` pin. Allowed
	// values: `embedded` | `linked`. Default: `embedded`.
	ASTModeDefault string

	// ExternalMetricCoverageFormat carries the
	// `external-metric-coverage-format` pin. Default: `Cobertura XML`.
	ExternalMetricCoverageFormat string

	// GateDegradedPolicy carries the `gate-degraded-policy` pin.
	// Allowed values: `warn` | `block`. Default: `warn`.
	GateDegradedPolicy string

	// PolicySigningRequired carries the `policy-signing-required`
	// pin. Default: `v1 required`.
	PolicySigningRequired string

	// RefactorEffortSource carries the `refactor-effort-source`
	// pin. Default: `ML model from historical commits`.
	// Wire-level env var: [EnvRefactorEffortSource]
	// (`CLEAN_CODE_REFACTOR_EFFORT_SOURCE`). The compose
	// shorthand [EnvEffortSourceAlias]
	// (`CLEAN_CODE_EFFORT_SOURCE`) populates the same field;
	// the canonical name wins if BOTH are set.
	RefactorEffortSource string

	// MLModelURI is the URI of the ML effort-model artefact
	// the Stage 9.3 refactor-planner loads when
	// [RefactorEffortSource] resolves to `ml`. Wire-level
	// env var: [EnvMLModelURI] (`CLEAN_CODE_ML_MODEL_URI`).
	// Required in the ML branch; the planner construction
	// returns [refactor.ErrMLModelURIMissing] when empty.
	MLModelURI string

	// MLModelVersion is the semantic version the loaded ML
	// model claims. Wire-level env var: [EnvMLModelVersion]
	// (`CLEAN_CODE_ML_MODEL_VERSION`). Required in the ML
	// branch; the planner compares this at every estimate
	// against `policy_version.refactor_weights.effort_model_version`
	// and aborts the atomic plan + tasks write on drift via
	// [refactor.ErrMLModelVersionMismatch].
	MLModelVersion string

	// --- Network bind addresses ---

	// HTTPAddr is the bind address for the /healthz + /readyz
	// listener.
	HTTPAddr string

	// PrometheusAddr is the bind address for the Prometheus scrape
	// endpoint.
	PrometheusAddr string

	// OTelEndpoint is the OTLP gRPC endpoint the service exports
	// traces / metrics to.
	OTelEndpoint string

	// --- Storage ---

	// PostgresURL is the libpq DSN the service connects to for
	// the `clean_code` schema. Empty in scaffold mode -- the
	// /readyz probe stays 503 until a PG pool registers a
	// readiness check.
	PostgresURL string

	// ManagementPostgresURL is the OPTIONAL libpq DSN the
	// metric-ingestor binary uses under the
	// `clean_code_management` role for `repo_event` INSERTs.
	// See [EnvMgmtPGURL] for the role-boundary rationale and
	// [AllowSharedPGRole] for the dev-mode opt-out. Empty in
	// scaffold mode.
	ManagementPostgresURL string

	// AllowSharedPGRole is the dev/E2E opt-in that lets the
	// metric-ingestor binary mount the Stage 3.4 management
	// verbs against [PostgresURL] alone (i.e. use the same DSN
	// for both the ingestor and management roles). Default
	// false. See [EnvAllowSharedPGRole].
	AllowSharedPGRole bool

	// --- Observability ---

	// LogLevel is the slog level the JSON logger emits at.
	// Allowed values: `debug` | `info` | `warn` | `error`.
	LogLevel string

	// --- Numeric defaults from tech-spec Sec 8.2 ---

	// ScanTimeout is the tech-spec Sec 8.2 `scan_timeout` value.
	ScanTimeout time.Duration
	// PeriodicSweepCadence is the tech-spec Sec 8.2
	// `periodic_sweep_cadence` value.
	PeriodicSweepCadence time.Duration
	// WindowDays is the tech-spec Sec 8.2 `window_days` value --
	// also surfaces inside `PolicyVersion.refactor_weights`.
	WindowDays int
	// FreshnessWindowSeconds is the tech-spec Sec 8.2
	// `freshness_window_seconds` value -- Insights stale-percentile
	// threshold. `eval.gate` does NOT depend on this (C17).
	FreshnessWindowSeconds int
	// PolicyPublishOverlapSeconds is the tech-spec Sec 8.2
	// `policy_publish_overlap_min_seconds` value.
	PolicyPublishOverlapSeconds int
	// AggregatorCadence is the tech-spec Sec 8.2
	// `aggregator_cadence` value -- the period between
	// Cross-Repo Aggregator ticks (Stage 7.1, architecture
	// Sec 3.10).
	AggregatorCadence time.Duration
	// DisableAggregator is the explicit operator opt-out for
	// the Stage 7.1 Cross-Repo Aggregator loop. Default
	// false (loop enabled). See [EnvDisableAggregator].
	DisableAggregator bool

	// --- Policy signing (Stage 5.1) ---

	// KMSProvider selects the policy-signing KMS adapter.
	// Closed set: `""` (scaffold; signing disabled), `"local"`
	// (envelope-encrypted Ed25519 seeds under a master key),
	// `"in-memory"` (test-only; private keys live in heap).
	// Defaults to `""`. See tech-spec Sec 8.4.
	KMSProvider string

	// KMSMasterKeyHex is the 64-char lowercase hex encoding
	// of the AES-256 master key the LocalSealedKMS uses.
	// Required when `KMSProvider == "local"`. NEVER logged.
	KMSMasterKeyHex string

	// --- Stage 2.6 churn webhook (scaffold mode) ---

	// WebhookHMACSecret is the shared HMAC-SHA256 secret used
	// by every external `ingest.*` webhook to verify request
	// bodies. See [EnvWebhookHMACSecret] for the operator-
	// facing contract and [MinWebhookHMACSecretBytes] for the
	// minimum-length guard. Empty in scaffold mode leaves the
	// webhook UNMOUNTED. NEVER logged.
	WebhookHMACSecret string

	// EnableScaffoldChurnWebhook is the explicit
	// operator-acknowledged opt-in for the Stage 2.6 scaffold
	// churn webhook. See [EnvEnableScaffoldChurnWebhook] for
	// the explicit "data lost on restart" rationale.
	EnableScaffoldChurnWebhook bool

	// --- Stage 3.1 Repo Indexer webhook (scaffold mode) ---

	// EnableScaffoldIndexerWebhook is the explicit
	// operator-acknowledged opt-in for the Stage 3.1
	// scaffold-mode Repo Indexer webhook + CLI rescan
	// trigger. See [EnvEnableScaffoldIndexerWebhook] for the
	// rationale. The indexer reuses [WebhookHMACSecret] (the
	// SHARED external-ingest HMAC secret).
	EnableScaffoldIndexerWebhook bool

	// --- Stage 3.5 stale-ScanRun sweep loop ---

	// DisableStaleSweep is the explicit operator opt-out for
	// the Stage 3.5 stale ScanRun sweep loop. Default false
	// (sweep enabled). When true, the metric-ingestor binary
	// SKIPS construction of the sweep goroutine entirely --
	// no PGScanRunStore is instantiated, no loop ticks fire,
	// and `/metrics` returns no sweep counters. Intended for
	// legacy E2E environments running the older
	// `001_init.sql` `scan_run` shape; production MUST keep
	// this false. See [EnvDisableStaleSweep].
	DisableStaleSweep bool

	// EnableLegacyDemoAPI is the operator opt-in for the
	// legacy `001_init.sql`-shaped HTTP routes
	// (`/v1/ingestor/process`, `/v1/ingestor/scan-run`).
	// Default false: production composition root mounts only
	// health/metrics. Setting this true in legacy E2E
	// environments mounts the older handlers that write the
	// `commit_sha`/`finished_at` columns. The Stage 1.2
	// canonical schema does not expose those columns; mixing
	// canonical migrations with this flag set is a wiring
	// error. See [EnvEnableLegacyDemoAPI].
	EnableLegacyDemoAPI bool

	// --- Stage 4.1 production-grade /v1/ingest/{verb}
	//     Router ---

	// EnableExternalIngestWebhook is the explicit operator
	// opt-in for the Stage 4.1 production-grade
	// `/v1/ingest/{verb}` Router. Default false (not
	// mounted). Setting this true requires
	// [WebhookHMACSecret] AND [WebhookSigningKeyID] to be
	// set; one without the others fails fast at startup.
	// See [EnvEnableExternalIngestWebhook].
	EnableExternalIngestWebhook bool

	// WebhookSigningKeyID is the canonical `signing_key_id`
	// publishers MUST send in the `X-Signing-Key-Id` header.
	// Empty when [EnableExternalIngestWebhook] is false.
	// See [EnvWebhookSigningKeyID].
	WebhookSigningKeyID string

	// --- Stage 3.2 Metric Ingestor scan-source ---

	// AstScanRoot points at the local on-disk root the
	// Metric Ingestor's directory-backed [AstFileSource]
	// walks. Layout convention is
	// `<AstScanRoot>/<repo_id>/<sha>/...`. Empty in
	// scaffold deploys -- the dispatcher falls back to the
	// [EmptyAstFileSource], which yields no files and
	// emits a `ast_files_seen=0` log line per dispatch.
	// Phase 4 replaces both with the PG-backed AST cache.
	AstScanRoot string

	// --- Stage 10.1 linked-mode adapter (architecture Sec 8.7) ---

	// EnableLinkedModeAdapter is the explicit operator
	// opt-in for the `internal/linked` adapter that wraps
	// the agent-memory cross-repo edge service. Default
	// false: the aggregator runs in embedded-only mode and
	// the composer degrades `xrepo_dep_depth` /
	// `blast_radius` rows with
	// `xrepo_edges_unavailable`. Setting this true REQUIRES
	// [LinkedAgentMemoryEndpoint] to also be set. See
	// [EnvEnableLinkedModeAdapter] for the env-var contract.
	//
	// Note this opens only the GLOBAL gate; per-repo opt-in
	// still requires the per-repo `mode='linked'` setting
	// (set via `mgmt.set_mode`). The two-axis gating lives
	// inside the linked package's `AggregatorAdapter`.
	EnableLinkedModeAdapter bool

	// LinkedAgentMemoryEndpoint is the agent-memory
	// cross-repo-edge service base URL (e.g.
	// `https://agent-memory.internal/`). MUST be a
	// parseable http(s) URL when set. Empty when
	// [EnableLinkedModeAdapter] is false. See
	// [EnvLinkedAgentMemoryEndpoint].
	LinkedAgentMemoryEndpoint string

	// LinkedAdapterTimeout is the per-request HTTP timeout
	// the linked-mode adapter applies to each agent-memory
	// fetch. Defaults to [DefaultLinkedAdapterTimeout].
	// See [EnvLinkedAdapterTimeout].
	LinkedAdapterTimeout time.Duration
}

// Defaults returns a Config populated with the canonical
// architecture Sec 1.6 + tech-spec Sec 8.2 defaults. Callers
// who want to start from defaults and only override a few
// fields can use this directly instead of going through Load.
func Defaults() Config {
	return Config{
		ASTModeDefault:               DefaultASTModeDefault,
		ExternalMetricCoverageFormat: DefaultExternalMetricCoverageFormat,
		GateDegradedPolicy:           DefaultGateDegradedPolicy,
		PolicySigningRequired:        DefaultPolicySigningRequired,
		RefactorEffortSource:         DefaultRefactorEffortSource,
		HTTPAddr:                     DefaultHTTPAddr,
		PrometheusAddr:               DefaultPrometheusAddr,
		OTelEndpoint:                 DefaultOTelEndpoint,
		LogLevel:                     DefaultLogLevel,
		ScanTimeout:                  DefaultScanTimeout,
		PeriodicSweepCadence:         DefaultPeriodicSweepCadence,
		WindowDays:                   DefaultWindowDays,
		FreshnessWindowSeconds:       DefaultFreshnessWindowSeconds,
		PolicyPublishOverlapSeconds:  DefaultPolicyPublishOverlapSeconds,
		AggregatorCadence:            DefaultAggregatorCadence,
		KMSProvider:                  DefaultKMSProvider,
		KMSMasterKeyHex:              "",
		LinkedAdapterTimeout:         DefaultLinkedAdapterTimeout,
	}
}

// Load reads the service configuration from CLEAN_CODE_* env vars.
// Missing values fall back to the canonical defaults pinned at
// architecture Sec 1.6 (operator pins) and tech-spec Sec 8.2
// (numeric defaults). Malformed values produce a hard error so a
// misconfigured deployment fails fast at startup rather than
// silently defaulting (the `unset` and `malformed` cases are
// intentionally NOT treated as equivalent).
//
// If CLEAN_CODE_CONFIG_FILE points at an existing file, it is
// parsed first (simple `KEY=VALUE` lines, one per line, `#`
// comments allowed); env vars then override file values so an
// operator can patch a single field without rewriting the file.
func Load() (Config, error) {
	cfg := Defaults()

	file := os.Getenv(EnvConfigFile)
	if file != "" {
		overrides, err := parseConfigFile(file)
		if err != nil {
			return Config{}, fmt.Errorf("config: parsing %s: %w", file, err)
		}
		if err := applyOverrides(&cfg, overrides); err != nil {
			return Config{}, fmt.Errorf("config: applying file overrides from %s: %w", file, err)
		}
	}

	// Env overrides go in last so operators can override a single
	// field without editing the file.
	envOverrides := readEnvOverrides()
	if err := applyOverrides(&cfg, envOverrides); err != nil {
		return Config{}, fmt.Errorf("config: applying env overrides: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces the closed-set constraints from architecture
// Sec 1.6 + tech-spec Sec 4.x for the fields that have one. Free-
// form fields (HTTPAddr, PostgresURL, etc.) are NOT validated here
// -- their dial-time failures already surface as readiness probe
// failures via /readyz.
func (c Config) Validate() error {
	switch c.ASTModeDefault {
	case "embedded", "linked":
	default:
		return fmt.Errorf("config: ast-mode-default=%q is not one of {embedded, linked}", c.ASTModeDefault)
	}
	switch c.GateDegradedPolicy {
	case "warn", "block":
	default:
		return fmt.Errorf("config: gate-degraded-policy=%q is not one of {warn, block}", c.GateDegradedPolicy)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log-level=%q is not one of {debug, info, warn, error}", c.LogLevel)
	}
	if c.WindowDays <= 0 {
		return fmt.Errorf("config: window-days=%d must be > 0", c.WindowDays)
	}
	if c.FreshnessWindowSeconds <= 0 {
		return fmt.Errorf("config: freshness-window-seconds=%d must be > 0", c.FreshnessWindowSeconds)
	}
	if c.PolicyPublishOverlapSeconds <= 0 {
		return fmt.Errorf("config: policy-publish-overlap-seconds=%d must be > 0", c.PolicyPublishOverlapSeconds)
	}
	if c.ScanTimeout <= 0 {
		return fmt.Errorf("config: scan-timeout=%s must be > 0", c.ScanTimeout)
	}
	if c.PeriodicSweepCadence <= 0 {
		return fmt.Errorf("config: periodic-sweep-cadence=%s must be > 0", c.PeriodicSweepCadence)
	}
	if c.AggregatorCadence <= 0 {
		return fmt.Errorf("config: aggregator-cadence=%s must be > 0", c.AggregatorCadence)
	}
	// KMS provider closed-set + interlocks.
	switch c.KMSProvider {
	case "", "local", "in-memory":
	default:
		return fmt.Errorf("config: kms-provider=%q is not one of {\"\", local, in-memory}", c.KMSProvider)
	}
	if c.KMSProvider == "local" {
		// Length check matches `keys.LocalKMSMasterKeyLen=32`
		// (= 64 hex chars). The deeper hex-decode + AES key
		// schedule construction happens at start-up inside
		// `keys.NewLocalSealedKMS`; the config layer just
		// pins the shape so an operator gets a clean error
		// before reaching the policy/keys package.
		hex := c.KMSMasterKeyHex
		if len(hex) != 64 {
			return fmt.Errorf("config: kms-provider=local requires kms-master-key-hex of exactly 64 hex chars; got %d chars", len(hex))
		}
	}
	if c.KMSProvider != "local" && c.KMSMasterKeyHex != "" {
		return fmt.Errorf("config: kms-master-key-hex is set but kms-provider=%q is not \"local\"", c.KMSProvider)
	}
	// Stage 2.6 scaffold-churn-webhook interlock: BOTH the
	// HMAC secret AND the explicit opt-in flag must be set to
	// enable the webhook; setting only one is always a
	// misconfiguration (an opt-in with no HMAC = unauthenticated
	// surface; HMAC without opt-in = unwanted surface). A
	// non-empty secret must also clear the minimum-length
	// guard ([MinWebhookHMACSecretBytes]) so an operator typo
	// or copy-paste truncation cannot mount a trivially
	// brute-forceable HMAC boundary (evaluator iter-6 #5).
	if c.EnableScaffoldChurnWebhook && c.WebhookHMACSecret == "" {
		return fmt.Errorf("config: %s=true requires %s to be set (HMAC verification is mandatory when the webhook is mounted)",
			EnvEnableScaffoldChurnWebhook, EnvWebhookHMACSecret)
	}
	if !c.EnableScaffoldChurnWebhook && c.WebhookHMACSecret != "" && !c.EnableScaffoldIndexerWebhook && !c.EnableExternalIngestWebhook {
		return fmt.Errorf("config: %s is set but none of %s / %s / %s is set; the webhooks stay UNMOUNTED until at least one opt-in flag is enabled (avoids an unintended public surface)",
			EnvWebhookHMACSecret, EnvEnableScaffoldChurnWebhook, EnvEnableScaffoldIndexerWebhook, EnvEnableExternalIngestWebhook)
	}
	if c.EnableScaffoldIndexerWebhook && c.WebhookHMACSecret == "" {
		return fmt.Errorf("config: %s=true requires %s to be set (HMAC verification is mandatory when the Repo Indexer webhook is mounted)",
			EnvEnableScaffoldIndexerWebhook, EnvWebhookHMACSecret)
	}
	// Stage 4.1 production-grade /v1/ingest/{verb} Router
	// interlock: enabling the Router requires BOTH a non-empty
	// signing-key-id AND a non-empty HMAC secret. The Router
	// rejects publishers that don't quote the configured
	// signing_key_id, so a missing key-id would brick the
	// surface; a missing secret would be an unauthenticated
	// boundary.
	if c.EnableExternalIngestWebhook {
		if c.WebhookHMACSecret == "" {
			return fmt.Errorf("config: %s=true requires %s to be set (HMAC verification is mandatory when the external-ingest Router is mounted)",
				EnvEnableExternalIngestWebhook, EnvWebhookHMACSecret)
		}
		if c.WebhookSigningKeyID == "" {
			return fmt.Errorf("config: %s=true requires %s to be set (the Router rejects any signing_key_id it does not recognise)",
				EnvEnableExternalIngestWebhook, EnvWebhookSigningKeyID)
		}
	}
	if c.WebhookSigningKeyID != "" && !c.EnableExternalIngestWebhook {
		return fmt.Errorf("config: %s is set but %s is not; the signing key id is consumed only by the external-ingest Router",
			EnvWebhookSigningKeyID, EnvEnableExternalIngestWebhook)
	}
	if c.WebhookHMACSecret != "" && len(c.WebhookHMACSecret) < MinWebhookHMACSecretBytes {
		return fmt.Errorf("config: %s must be at least %d bytes long (got %d); use a CSPRNG-generated secret such as `head -c 32 /dev/urandom | base64`",
			EnvWebhookHMACSecret, MinWebhookHMACSecretBytes, len(c.WebhookHMACSecret))
	}
	// Stage 10.1 linked-mode adapter interlock: enabling the
	// adapter requires a parseable http(s) endpoint, and a
	// non-empty endpoint without the explicit opt-in is
	// always a misconfiguration (the endpoint is consumed
	// ONLY by the linked-mode adapter). The symmetric
	// interlock matches the existing webhook-opt-in
	// patterns at lines 738-758 above. The per-request
	// timeout MUST be strictly positive when set; the
	// loader's [DefaultLinkedAdapterTimeout] preserves a
	// safe default when the operator omits the env var.
	endpoint := strings.TrimSpace(c.LinkedAgentMemoryEndpoint)
	if c.EnableLinkedModeAdapter {
		if endpoint == "" {
			return fmt.Errorf("config: %s=true requires %s to be set (the linked-mode adapter has no agent-memory base URL)",
				EnvEnableLinkedModeAdapter, EnvLinkedAgentMemoryEndpoint)
		}
	}
	if endpoint != "" && !c.EnableLinkedModeAdapter {
		return fmt.Errorf("config: %s is set but %s is not; the endpoint is consumed only by the linked-mode adapter",
			EnvLinkedAgentMemoryEndpoint, EnvEnableLinkedModeAdapter)
	}
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil {
			return fmt.Errorf("config: %s=%q is not a parseable URL: %w", EnvLinkedAgentMemoryEndpoint, endpoint, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("config: %s=%q must use http or https scheme (got %q)", EnvLinkedAgentMemoryEndpoint, endpoint, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("config: %s=%q must include a host", EnvLinkedAgentMemoryEndpoint, endpoint)
		}
	}
	if c.LinkedAdapterTimeout <= 0 {
		return fmt.Errorf("config: %s=%s must be > 0", EnvLinkedAdapterTimeout, c.LinkedAdapterTimeout)
	}
	return nil
}

// readEnvOverrides collects every CLEAN_CODE_* env var into a
// flat map. Empty / unset vars are skipped so the file value
// (if any) survives.
func readEnvOverrides() map[string]string {
	keys := []string{
		EnvASTModeDefault,
		EnvExternalMetricCoverageFormat,
		EnvGateDegradedPolicy,
		EnvPolicySigningRequired,
		EnvRefactorEffortSource,
		EnvEffortSourceAlias,
		EnvMLModelURI,
		EnvMLModelVersion,
		EnvHTTPAddr,
		EnvPrometheusAddr,
		EnvOTelEndpoint,
		EnvPGURL,
		EnvMgmtPGURL,
		EnvAllowSharedPGRole,
		EnvLogLevel,
		EnvScanTimeout,
		EnvPeriodicSweepCadence,
		EnvWindowDays,
		EnvFreshnessWindowSeconds,
		EnvPolicyPublishOverlapSeconds,
		EnvAggregatorCadence,
		EnvDisableAggregator,
		EnvKMSProvider,
		EnvKMSMasterKeyHex,
		EnvWebhookHMACSecret,
		EnvEnableScaffoldChurnWebhook,
		EnvEnableScaffoldIndexerWebhook,
		EnvDisableStaleSweep,
		EnvEnableLegacyDemoAPI,
		EnvEnableExternalIngestWebhook,
		EnvWebhookSigningKeyID,
		EnvAstScanRoot,
		EnvEnableLinkedModeAdapter,
		EnvLinkedAgentMemoryEndpoint,
		EnvLinkedAdapterTimeout,
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			out[k] = v
		}
	}
	return out
}

// applyOverrides maps a flat key->string override map onto cfg.
// Unknown keys produce a hard error so a typo in a config file
// fails fast rather than silently doing nothing.
func applyOverrides(cfg *Config, overrides map[string]string) error {
	for k, v := range overrides {
		switch k {
		case EnvASTModeDefault:
			cfg.ASTModeDefault = v
		case EnvExternalMetricCoverageFormat:
			cfg.ExternalMetricCoverageFormat = v
		case EnvGateDegradedPolicy:
			cfg.GateDegradedPolicy = v
		case EnvPolicySigningRequired:
			cfg.PolicySigningRequired = v
		case EnvRefactorEffortSource:
			cfg.RefactorEffortSource = v
		case EnvEffortSourceAlias:
			// Compose-shorthand alias. The canonical
			// EnvRefactorEffortSource wins if BOTH are
			// supplied: an operator-set canonical value MUST
			// NOT be silently overridden by a compose default.
			// We consult the override MAP directly (not the
			// cfg field) so the precedence is independent of
			// the non-deterministic map iteration order AND
			// independent of the Defaults() pre-seed.
			if _, hasCanonical := overrides[EnvRefactorEffortSource]; !hasCanonical {
				cfg.RefactorEffortSource = v
			}
		case EnvMLModelURI:
			cfg.MLModelURI = v
		case EnvMLModelVersion:
			cfg.MLModelVersion = v
		case EnvHTTPAddr:
			cfg.HTTPAddr = v
		case EnvPrometheusAddr:
			cfg.PrometheusAddr = v
		case EnvOTelEndpoint:
			cfg.OTelEndpoint = v
		case EnvPGURL:
			cfg.PostgresURL = v
		case EnvMgmtPGURL:
			cfg.ManagementPostgresURL = v
		case EnvAllowSharedPGRole:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.AllowSharedPGRole = true
			case "0", "false", "no", "off":
				cfg.AllowSharedPGRole = false
			default:
				return fmt.Errorf("%s=%q: not a boolean (accepted: 1|true|yes|on / 0|false|no|off)", k, v)
			}
		case EnvLogLevel:
			cfg.LogLevel = v
		case EnvScanTimeout:
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s=%q: %w", k, v, err)
			}
			cfg.ScanTimeout = d
		case EnvPeriodicSweepCadence:
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s=%q: %w", k, v, err)
			}
			cfg.PeriodicSweepCadence = d
		case EnvWindowDays:
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s=%q: %w", k, v, err)
			}
			cfg.WindowDays = n
		case EnvFreshnessWindowSeconds:
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s=%q: %w", k, v, err)
			}
			cfg.FreshnessWindowSeconds = n
		case EnvPolicyPublishOverlapSeconds:
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s=%q: %w", k, v, err)
			}
			cfg.PolicyPublishOverlapSeconds = n
		case EnvAggregatorCadence:
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s=%q: %w", k, v, err)
			}
			cfg.AggregatorCadence = d
		case EnvDisableAggregator:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.DisableAggregator = true
			case "0", "false", "no", "off":
				cfg.DisableAggregator = false
			default:
				return fmt.Errorf("%s=%q: not a boolean (accepted: 1|true|yes|on / 0|false|no|off)", k, v)
			}
		case EnvKMSProvider:
			cfg.KMSProvider = v
		case EnvKMSMasterKeyHex:
			cfg.KMSMasterKeyHex = v
		case EnvWebhookHMACSecret:
			cfg.WebhookHMACSecret = v
		case EnvEnableScaffoldChurnWebhook:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.EnableScaffoldChurnWebhook = true
			case "0", "false", "no", "off":
				cfg.EnableScaffoldChurnWebhook = false
			default:
				return fmt.Errorf("%s=%q: not a boolean (accepted: 1|true|yes|on / 0|false|no|off)", k, v)
			}
		case EnvEnableScaffoldIndexerWebhook:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.EnableScaffoldIndexerWebhook = true
			case "0", "false", "no", "off":
				cfg.EnableScaffoldIndexerWebhook = false
			default:
				return fmt.Errorf("%s=%q: not a boolean (accepted: 1|true|yes|on / 0|false|no|off)", k, v)
			}
		case EnvDisableStaleSweep:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.DisableStaleSweep = true
			case "0", "false", "no", "off":
				cfg.DisableStaleSweep = false
			default:
				return fmt.Errorf("%s=%q: not a boolean (accepted: 1|true|yes|on / 0|false|no|off)", k, v)
			}
		case EnvEnableLegacyDemoAPI:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.EnableLegacyDemoAPI = true
			case "0", "false", "no", "off":
				cfg.EnableLegacyDemoAPI = false
			default:
				return fmt.Errorf("%s=%q: not a boolean (accepted: 1|true|yes|on / 0|false|no|off)", k, v)
			}
		case EnvEnableExternalIngestWebhook:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.EnableExternalIngestWebhook = true
			case "0", "false", "no", "off":
				cfg.EnableExternalIngestWebhook = false
			default:
				return fmt.Errorf("%s=%q: not a boolean (accepted: 1|true|yes|on / 0|false|no|off)", k, v)
			}
		case EnvWebhookSigningKeyID:
			cfg.WebhookSigningKeyID = v
		case EnvAstScanRoot:
			cfg.AstScanRoot = v
		case EnvEnableLinkedModeAdapter:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.EnableLinkedModeAdapter = true
			case "0", "false", "no", "off":
				cfg.EnableLinkedModeAdapter = false
			default:
				return fmt.Errorf("%s=%q: not a boolean (accepted: 1|true|yes|on / 0|false|no|off)", k, v)
			}
		case EnvLinkedAgentMemoryEndpoint:
			cfg.LinkedAgentMemoryEndpoint = v
		case EnvLinkedAdapterTimeout:
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s=%q: %w", k, v, err)
			}
			cfg.LinkedAdapterTimeout = d
		default:
			return fmt.Errorf("unknown config key %q", k)
		}
	}
	return nil
}

// parseConfigFile reads a simple `KEY=VALUE` file (one per line,
// `#` comments allowed). Quoted values are NOT unwrapped -- the
// expected use case is operator-managed env-style files, not
// shell scripts.
//
// A file containing only comments / blank lines is treated as
// "no overrides" and returns an empty map. Operators commonly
// keep a fully-commented template checked in, and forcing a
// startup crash on that input is hostile to that workflow --
// the loader already tolerates the env var being unset, so a
// file with zero effective entries is the natural equivalent.
func parseConfigFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", lineNo+1, raw)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", lineNo+1)
		}
		out[key] = val
	}
	return out, nil
}
