package rule_engine

import (
	"context"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/evaluator"
)

// EvaluatorAdapter wraps an [Engine] in the shape the
// [evaluator.Gate] expects. The wrapping is necessary
// because the engine's [RunResult] is typed inside this
// package (which knows about [Verdict] and friends) and the
// evaluator's [evaluator.EngineRunResult] is a string-typed
// projection -- direct interface satisfaction would create
// an import cycle (`evaluator` -> `rule_engine` -> `evaluator`).
//
// Construct via [NewEvaluatorAdapter] at the composition
// root and pass into `EvaluateConfig.Engine`:
//
//	gate := evaluator.NewGateWithEngine(evaluator.NewGate(km), evaluator.EvaluateConfig{
//	    Engine: rule_engine.NewEvaluatorAdapter(engine),
//	    ...,
//	})
type EvaluatorAdapter struct {
	engine *Engine
}

// NewEvaluatorAdapter wraps engine so it satisfies
// [evaluator.RuleEngine]. A nil engine is treated as "no
// engine"; the adapter's `RunSync` returns
// [ErrStoreUnwired] in that case (loud, not silent).
func NewEvaluatorAdapter(engine *Engine) *EvaluatorAdapter {
	return &EvaluatorAdapter{engine: engine}
}

// RunSync implements [evaluator.RuleEngine]. Delegates to
// the underlying engine and projects the result onto the
// string-typed verdict the evaluator surface expects.
func (a *EvaluatorAdapter) RunSync(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID, policyVersionID uuid.UUID) (evaluator.EngineRunResult, error) {
	if a == nil || a.engine == nil {
		return evaluator.EngineRunResult{}, ErrStoreUnwired
	}
	r, err := a.engine.RunSync(ctx, repoID, sha, scope, policyVersionID)
	if err != nil {
		return evaluator.EngineRunResult{}, err
	}
	return evaluator.EngineRunResult{
		EvaluationRunID:     r.EvaluationRunID,
		EvaluationVerdictID: r.EvaluationVerdictID,
		FindingIDs:          r.FindingIDs,
		Verdict:             string(r.Verdict),
	}, nil
}

// Compile-time assertion that EvaluatorAdapter satisfies
// the evaluator's expected port.
var _ evaluator.RuleEngine = (*EvaluatorAdapter)(nil)
