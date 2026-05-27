package keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	// lib/pq driver registration. Importing here for side-
	// effects so callers of [Build] don't need to remember to
	// blank-import the driver themselves; one less footgun.
	_ "github.com/lib/pq"
)

// KMS provider labels. Operators set this via the
// `CLEAN_CODE_KMS_PROVIDER` env var consumed by
// `internal/config`. The closed set is documented at the
// service level so a `grep -rnF "KMSProviderLocal"` lands on
// every consumer.
const (
	// KMSProviderLocal is the v1 production-capable provider:
	// envelope-encrypted Ed25519 seeds under an AES-256-GCM
	// master key the operator injects via the
	// `CLEAN_CODE_KMS_MASTER_KEY_HEX` env var. The master key
	// NEVER touches PostgreSQL.
	KMSProviderLocal = "local"

	// KMSProviderInMemory is the test / scaffold provider:
	// keypairs live in process memory and are gone on exit.
	// NOT suitable for production -- a restart loses every
	// active key. Stage 5.1 ships this so `go test ./...`
	// and dev-loop bring-ups stay green without a master key
	// or a Postgres handle.
	KMSProviderInMemory = "in-memory"
)

// AllKMSProviders is the canonical closed set of KMS provider
// labels Stage 5.1 ships. Future stages append new labels here
// (e.g. `azure-key-vault`, `aws-kms`); the closed set is the
// contract that lets the composition root validate operator
// config without referencing every provider impl.
var AllKMSProviders = []string{KMSProviderLocal, KMSProviderInMemory}

// BuildConfig bundles the inputs the [Build] factory consumes.
// All fields are EXPLICIT: the factory never reads env vars
// directly -- that's the composition root's job. This keeps
// the unit-test path env-free.
type BuildConfig struct {
	// KMSProvider selects between [KMSProviderLocal] and
	// [KMSProviderInMemory]. Required.
	KMSProvider string

	// KMSMasterKeyHex is the AES-256 master key encoded as 64
	// lowercase hex chars. Required when KMSProvider ==
	// KMSProviderLocal. Ignored otherwise.
	KMSMasterKeyHex string

	// DB is the SQL handle for the [SQLStore]. When nil, the
	// factory falls back to [InMemoryStore]. The composition
	// root is responsible for closing the *sql.DB on shutdown.
	DB *sql.DB

	// Overlap is the rotation overlap window. Defaults to
	// [DefaultOverlap] (24h) when zero. Sourced from
	// `config.PolicyPublishOverlapSeconds`.
	Overlap time.Duration

	// MintFirstKeyIfEmpty controls whether [Bootstrap] mints
	// a first key when the store is empty. Production
	// deployments set this true so a fresh schema is signable
	// the moment /readyz turns green. Tests set it false to
	// verify the no-active-key error path.
	MintFirstKeyIfEmpty bool

	// Clock overrides the wall-clock source. nil = time.Now.
	Clock func() time.Time
}

// BuildResult is what [Build] returns. The caller wires
// Manager into the rest of the service, registers
// HealthCheck against `internal/health.Handler`, and -- when
// non-nil -- calls Close on shutdown to release any open
// resources owned by the factory.
type BuildResult struct {
	Manager     *Manager
	HealthCheck func(ctx context.Context) error
	// Close releases factory-owned resources. The *sql.DB
	// supplied via BuildConfig.DB is NOT closed (caller owns
	// its lifecycle).
	Close func()
}

// Build is the composition-root entry point. It validates the
// supplied config FAIL-CLOSED (an incoherent mix returns an
// error rather than silently picking a less-secure default),
// constructs the KMS + Store, and runs [Bootstrap].
//
// Validation rules (per rubber-duck critique #G):
//
//   - KMSProvider must be in [AllKMSProviders]; an unknown
//     label fails fast.
//
//   - When KMSProvider == "local" AND KMSMasterKeyHex == "":
//     fail. A "local" provider with no master key cannot seal
//     anything.
//
//   - When DB != nil AND KMSProvider == "in-memory": fail.
//     Persisting public-key rows to disk while sealing private
//     material in process memory is a footgun -- a restart
//     loses every private key and the service then signs
//     against ghosts.
//
//   - When DB == nil AND KMSProvider == "local": fail. The
//     LocalSealedKMS persists nothing itself; the in-memory
//     store would also lose the rows on restart, defeating
//     the point of envelope encryption.
func Build(ctx context.Context, cfg BuildConfig) (*BuildResult, error) {
	if err := validateBuildConfig(cfg); err != nil {
		return nil, err
	}

	var kms KMS
	switch cfg.KMSProvider {
	case KMSProviderLocal:
		k, err := NewLocalSealedKMS(cfg.KMSMasterKeyHex)
		if err != nil {
			return nil, fmt.Errorf("policy/keys: Build: %w", err)
		}
		kms = k
	case KMSProviderInMemory:
		kms = NewInMemoryKMS(nil)
	default:
		// validateBuildConfig already rejected this; keep the
		// branch so a future enum drift is caught by the
		// switch exhaustiveness rather than a silent fall-
		// through.
		return nil, fmt.Errorf("policy/keys: Build: KMSProvider=%q is not in %v", cfg.KMSProvider, AllKMSProviders)
	}

	var store Store
	if cfg.DB != nil {
		sqlStore, err := NewSQLStore(cfg.DB)
		if err != nil {
			return nil, fmt.Errorf("policy/keys: Build: %w", err)
		}
		store = sqlStore
	} else {
		store = NewInMemoryStore()
	}

	manager, check, err := Bootstrap(ctx, Config{
		KMS:     kms,
		Store:   store,
		Overlap: cfg.Overlap,
		Clock:   cfg.Clock,
	}, cfg.MintFirstKeyIfEmpty)
	if err != nil {
		return nil, err
	}

	return &BuildResult{
		Manager:     manager,
		HealthCheck: check,
		Close:       func() { /* nothing factory-owned yet */ },
	}, nil
}

// validateBuildConfig enforces the fail-closed rules listed in
// the [Build] doc-comment. Returned errors are intended to
// surface verbatim in the composition root's startup log so
// the operator can spot the misconfiguration without grepping
// the source.
func validateBuildConfig(cfg BuildConfig) error {
	if cfg.KMSProvider == "" {
		return errors.New("policy/keys: BuildConfig.KMSProvider is required (one of: local, in-memory)")
	}
	knownProvider := false
	for _, p := range AllKMSProviders {
		if p == cfg.KMSProvider {
			knownProvider = true
			break
		}
	}
	if !knownProvider {
		return fmt.Errorf("policy/keys: BuildConfig.KMSProvider=%q is not in the canonical closed set %v",
			cfg.KMSProvider, AllKMSProviders)
	}

	switch cfg.KMSProvider {
	case KMSProviderLocal:
		if cfg.KMSMasterKeyHex == "" {
			return errors.New("policy/keys: BuildConfig.KMSProvider=local requires KMSMasterKeyHex " +
				"(set CLEAN_CODE_KMS_MASTER_KEY_HEX to 64 lowercase hex chars)")
		}
		if cfg.DB == nil {
			return errors.New("policy/keys: BuildConfig.KMSProvider=local requires a *sql.DB store " +
				"(set CLEAN_CODE_PG_URL); refusing to seal private keys against an in-memory store " +
				"that loses every row on restart")
		}
	case KMSProviderInMemory:
		if cfg.DB != nil {
			return errors.New("policy/keys: BuildConfig.KMSProvider=in-memory must NOT be paired " +
				"with a *sql.DB store -- private keys would be lost on restart while public-key rows " +
				"persisted to disk. Either use KMSProvider=local or remove the DB handle.")
		}
		if cfg.KMSMasterKeyHex != "" {
			return errors.New("policy/keys: BuildConfig.KMSProvider=in-memory must NOT be paired with " +
				"a master key (the in-memory KMS does not seal anything). Did you mean KMSProvider=local?")
		}
	}
	return nil
}
