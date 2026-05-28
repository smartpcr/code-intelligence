package composition

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/evaluator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// EvalGateConfig is the input bundle for [BuildEvalGate].
// Pinned as a struct so additional knobs (e.g. evaluator-
// side caching, batch parallelism) can be added without
// changing the helper's positional signature.
type EvalGateConfig struct {
	// EvaluatorDB carries `clean_code_evaluator` grants.
	// Used for the two short-circuit degraded write
	// paths in [evaluator.NewProductionGate]
	// (signature-invalid, samples-pending) and for the
	// Steward's SQL store.
	EvaluatorDB *sql.DB

	// SolidBatchDB carries `clean_code_solid_batch`
	// grants. Used by the rule engine's SQLStore for
	// the canonical Audit triple INSERT (evaluation_run
	// + evaluation_verdict + finding) on the rule-pass
	// path. May be the SAME handle as EvaluatorDB in
	// dev / E2E when role boundaries are collapsed --
	// production deployments MUST pass distinct handles.
	SolidBatchDB *sql.DB

	// Signer is the policy-signing-key Manager. When
	// nil the Steward installs `noActiveSigner` and
	// `Gate.Evaluate` will degrade every request as
	// `policy_signature_invalid`; a production
	// composition root SHOULD pass a real Signer.
	Signer steward.Signer

	// WalWriter is the Audit WAL writer (Stage 9.1 /
	// architecture Sec 7.10). REQUIRED -- every audit
	// write performed by either the Rule Engine's
	// `SQLStore.AppendEvaluation` (rule-pass path) OR
	// the Evaluator's `SQLDegradedRunStore.AppendDegradedRun`
	// (signature-invalid / samples-pending paths) is
	// mirrored to this writer inside the same SQL
	// transaction; WAL fsync precedes SQL commit. The
	// binary's main reads `CLEAN_CODE_AUDIT_WAL_DIR`,
	// constructs the writer, and passes it here.
	WalWriter *wal.Writer
}

// BuildEvalGate assembles the production
// [evaluator.Gate] for the canonical `eval.gate` verb.
//
// The composition wires:
//
//   - [steward.NewSQLStore] over EvaluatorDB so the
//     evaluator's signature-verify path resolves the
//     same policy_version_id the publisher wrote.
//   - [steward.New] over (Store, Signer) so the steward
//     can verify the persisted signature against the
//     current signing key.
//   - [rule_engine.NewSQLStore] over SolidBatchDB +
//     stewardStore so the engine writes the canonical
//     Audit triple under the `clean_code_solid_batch`
//     grant (the evaluator's narrower grant is reserved
//     for the two degraded paths).
//   - [rule_engine.New] for the in-process engine.
//   - [evaluator.NewProductionGate] which binds it all
//     together and exposes the Stage 6.1 Gate.Gate verb
//     (active-policy-resolving canonical path) plus the
//     Gate.Evaluate admin/replay path.
//
// Returns a non-nil error if either DB / WalWriter is
// nil or any step in the composition fails. A nil Signer
// is PERMITTED but logged via the supplied logger --
// callers that want fail-loud-on-missing-Signer guard at
// the env-loading step.
func BuildEvalGate(_ context.Context, cfg EvalGateConfig, logger *slog.Logger) (*evaluator.Gate, error) {
	if cfg.EvaluatorDB == nil {
		return nil, fmt.Errorf("BuildEvalGate: EvaluatorDB is nil")
	}
	if cfg.SolidBatchDB == nil {
		return nil, fmt.Errorf("BuildEvalGate: SolidBatchDB is nil")
	}
	if cfg.WalWriter == nil {
		return nil, fmt.Errorf("BuildEvalGate: WalWriter is nil (Stage 9.1: every Audit INSERT MUST be paired with a WAL frame fsynced before SQL commit; the binary's main reads CLEAN_CODE_AUDIT_WAL_DIR and constructs the *wal.Writer)")
	}
	if logger == nil {
		logger = slog.Default()
	}

	stewardStore, err := steward.NewSQLStore(cfg.EvaluatorDB)
	if err != nil {
		return nil, fmt.Errorf("steward.NewSQLStore: %w", err)
	}
	if cfg.Signer == nil {
		logger.Warn("BuildEvalGate: Signer is nil; Gate.Evaluate will degrade EVERY request as policy_signature_invalid until a steward.Signer is plumbed")
	}
	stew, err := steward.New(steward.Config{Store: stewardStore, Signer: cfg.Signer})
	if err != nil {
		return nil, fmt.Errorf("steward.New: %w", err)
	}

	ruleStore, err := rule_engine.NewSQLStore(rule_engine.SQLStoreConfig{
		DB:        cfg.SolidBatchDB,
		Steward:   stewardStore,
		WalWriter: cfg.WalWriter,
	})
	if err != nil {
		return nil, fmt.Errorf("rule_engine.NewSQLStore: %w", err)
	}
	engine, err := rule_engine.New(rule_engine.Config{Store: ruleStore})
	if err != nil {
		return nil, fmt.Errorf("rule_engine.New: %w", err)
	}

	gate, err := evaluator.NewProductionGate(evaluator.ProductionGateConfig{
		DB:           cfg.EvaluatorDB,
		Steward:      stew,
		StewardStore: stewardStore,
		Engine:       rule_engine.NewEvaluatorAdapter(engine),
		WalWriter:    cfg.WalWriter,
		// KeyManager intentionally nil -- the legacy
		// signature-bundle Gate.VerifyPolicy surface is
		// not the focus here. Gate.Evaluate uses its
		// own steward-backed PolicySignatureVerifier.
	})
	if err != nil {
		return nil, fmt.Errorf("evaluator.NewProductionGate: %w", err)
	}
	return gate, nil
}
