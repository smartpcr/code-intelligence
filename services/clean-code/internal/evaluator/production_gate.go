package evaluator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// ProductionGateConfig bundles the dependencies the
// composition root injects when wiring the production
// [Gate] via [NewProductionGate]. The helper wires the
// concrete [SQLDegradedRunStore], [SQLSampleReadiness], and
// the steward-backed [PolicyVersionReader] +
// [PolicySignatureVerifier] adapters in one place so the
// caller does not have to assemble the four sub-dependencies
// manually.
//
// Per Stage 5.7 evaluator feedback #2: prior to this iter,
// the `NewGateWithEngine` constructor existed only with
// interface stubs; the gate could not actually write
// degraded audit rows or invoke the synchronous engine in a
// real composition root. [NewProductionGate] closes that
// gap: it returns a [*Gate] whose [Gate.Evaluate] path is
// fully production-wired against PostgreSQL.
type ProductionGateConfig struct {
	// DB is the `*sql.DB` handle used for sample-readiness
	// reads (`clean_code.commit.scan_status`) AND degraded
	// audit writes (`clean_code.evaluation_run` +
	// `clean_code.evaluation_verdict`). Required.
	//
	// Per migrations/0004_roles.up.sql:455-465, the role
	// authenticating this handle MUST have INSERT on the
	// Audit tables AND SELECT on `commit`. The canonical
	// production role is `clean_code_evaluator`.
	DB *sql.DB

	// Schema overrides the canonical `"clean_code"` schema
	// name. Empty defaults to `"clean_code"`.
	Schema string

	// Steward is the policy/rule/threshold reader the
	// gate uses to (a) resolve a `policy_version_id` ->
	// canonical [steward.PolicyVersion] row via
	// [steward.SQLStore.GetPolicyVersion] and (b)
	// re-verify the persisted signature against the
	// canonical bytes of THAT policy version via
	// [steward.Steward.VerifyPolicyVersionSignature].
	// Required.
	Steward *steward.Steward

	// StewardStore is the underlying read-only store the
	// steward wraps. Required (the gate's
	// [PolicyVersionReader] delegates directly to the
	// store's [steward.SQLStore.GetPolicyVersion]).
	StewardStore *steward.SQLStore

	// Engine is the synchronous rule engine the gate
	// delegates to on the happy path. Required.
	Engine RuleEngine

	// KeyManager is the [keys.Manager] wired into the
	// underlying [Gate.VerifyPolicy] surface (Stage 5.6
	// unchanged path). MAY be nil for evaluate-only
	// deployments that do not need the legacy bundle
	// signature path; in that case
	// [Gate.VerifyPolicy] will return [ErrGateUnwired]
	// while [Gate.Evaluate] still works.
	KeyManager *keys.Manager

	// WalWriter is the Audit WAL writer (Stage 9.1 /
	// architecture Sec 7.10). REQUIRED -- the gate's
	// degraded short-circuits (signature-invalid,
	// samples-pending) write `evaluation_run` +
	// `evaluation_verdict` rows that MUST be mirrored to
	// the WAL inside the same SQL transaction. The
	// composition root reads `CLEAN_CODE_AUDIT_WAL_DIR`,
	// constructs the writer, and passes it here so
	// [NewSQLDegradedRunStore] receives a non-nil writer
	// per the brief's "row+WAL atomically" contract.
	WalWriter *wal.Writer
}

// stewardPolicyAdapter adapts the steward's SQLStore onto
// the gate's [PolicyVersionReader] interface. The
// SQLStore's `GetPolicyVersion` signature already matches
// the interface exactly -- this adapter is a method-set
// witness so the steward package does not need to declare
// its dependency on the evaluator package.
type stewardPolicyAdapter struct {
	store *steward.SQLStore
}

func (a *stewardPolicyAdapter) GetPolicyVersion(ctx context.Context, id uuid.UUID) (steward.PolicyVersion, error) {
	if a == nil || a.store == nil {
		return steward.PolicyVersion{}, errors.New("evaluator: stewardPolicyAdapter: store is nil")
	}
	return a.store.GetPolicyVersion(ctx, id)
}

// stewardSignatureAdapter adapts the steward onto the
// gate's [PolicySignatureVerifier] interface. Same
// rationale as [stewardPolicyAdapter].
type stewardSignatureAdapter struct {
	steward *steward.Steward
}

func (a *stewardSignatureAdapter) VerifyPolicyVersionSignature(ctx context.Context, pv steward.PolicyVersion) error {
	if a == nil || a.steward == nil {
		return errors.New("evaluator: stewardSignatureAdapter: steward is nil")
	}
	return a.steward.VerifyPolicyVersionSignature(ctx, pv)
}

// stewardActivationAdapter adapts the steward onto the
// gate's [PolicyActivationReader] interface. The
// production wiring resolves the active `policy_version_id`
// via [steward.Steward.ActivePolicyVersion] -- which reads
// the latest `clean_code.policy_activation` row and
// dereferences to its policy_version. The Stage 6.1 brief
// step (1) is for the `eval.gate(repo_id, sha, scope?)`
// verb to perform this lookup itself.
//
// Defence in depth: a `(PolicyVersion, true, nil)` reply
// from the steward whose `PolicyVersionID` is the zero
// uuid is treated as an invariant violation (loud error
// rather than silent ok=false). The same guard the
// `rule_engine.StewardActivationReader` enforces.
type stewardActivationAdapter struct {
	steward *steward.Steward
}

func (a *stewardActivationAdapter) ActivePolicyVersionID(ctx context.Context) (uuid.UUID, bool, error) {
	if a == nil || a.steward == nil {
		return uuid.Nil, false, errors.New("evaluator: stewardActivationAdapter: steward is nil")
	}
	pv, ok, err := a.steward.ActivePolicyVersion(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	if !ok {
		return uuid.Nil, false, nil
	}
	if pv.PolicyVersionID == uuid.Nil {
		return uuid.Nil, false, errors.New("evaluator: stewardActivationAdapter: steward returned ok=true with zero PolicyVersionID")
	}
	return pv.PolicyVersionID, true, nil
}

// NewProductionGate wires a [*Gate] whose [Gate.Evaluate]
// path is fully production-ready. The caller still owns
// the `*sql.DB` and `*steward.Steward` lifecycles -- this
// helper does NOT call `Close` on either.
//
// Returns an error if any required dependency is missing.
//
// Composition (Stage 5.7 evaluator feedback #2 + Stage 6.1):
//
//   - [SQLSampleReadiness] -- backs [SampleReadinessReader]
//     against `clean_code.commit.scan_status`.
//   - [SQLDegradedRunStore] -- backs [DegradedRunStore]
//     against `clean_code.evaluation_run` +
//     `clean_code.evaluation_verdict` with
//     `caller='eval_gate'`.
//   - [stewardPolicyAdapter] -- backs [PolicyVersionReader]
//     against [steward.SQLStore.GetPolicyVersion].
//   - [stewardSignatureAdapter] -- backs
//     [PolicySignatureVerifier] against
//     [steward.Steward.VerifyPolicyVersionSignature].
//   - [stewardActivationAdapter] -- backs
//     [PolicyActivationReader] against
//     [steward.Steward.ActivePolicyVersion]; this is the
//     Stage 6.1 step (1) "resolve active
//     `policy_version_id` via latest `policy_activation`
//     row" wiring that lets [Gate.Gate] satisfy the
//     canonical verb signature.
//   - [uuid.NewV4] / [time.Now] -- production defaults
//     for the [IDMinter] / now hooks.
func NewProductionGate(cfg ProductionGateConfig) (*Gate, error) {
	if cfg.DB == nil {
		return nil, errors.New("evaluator: NewProductionGate: DB is required")
	}
	if cfg.Steward == nil {
		return nil, errors.New("evaluator: NewProductionGate: Steward is required")
	}
	if cfg.StewardStore == nil {
		return nil, errors.New("evaluator: NewProductionGate: StewardStore is required")
	}
	if cfg.Engine == nil {
		return nil, errors.New("evaluator: NewProductionGate: Engine is required")
	}
	if cfg.WalWriter == nil {
		return nil, errors.New("evaluator: NewProductionGate: WalWriter is required (Stage 9.1: every degraded Audit INSERT MUST be paired with a WAL frame fsynced before SQL commit; supply a *wal.Writer rooted at CLEAN_CODE_AUDIT_WAL_DIR)")
	}
	readiness, err := NewSQLSampleReadiness(SQLSampleReadinessConfig{DB: cfg.DB, Schema: cfg.Schema})
	if err != nil {
		return nil, fmt.Errorf("evaluator: NewProductionGate: NewSQLSampleReadiness: %w", err)
	}
	degraded, err := NewSQLDegradedRunStore(SQLDegradedRunStoreConfig{DB: cfg.DB, Schema: cfg.Schema, WalWriter: cfg.WalWriter})
	if err != nil {
		return nil, fmt.Errorf("evaluator: NewProductionGate: NewSQLDegradedRunStore: %w", err)
	}
	g := NewGate(cfg.KeyManager)
	g = NewGateWithEngine(g, EvaluateConfig{
		Engine:          cfg.Engine,
		Readiness:       readiness,
		PolicyReader:    &stewardPolicyAdapter{store: cfg.StewardStore},
		SignatureVerify: &stewardSignatureAdapter{steward: cfg.Steward},
		DegradedStore:   degraded,
		Activation:      &stewardActivationAdapter{steward: cfg.Steward},
		NewID:           uuid.NewV4,
		Now:             func() int64 { return time.Now().UTC().UnixNano() },
	})
	return g, nil
}
