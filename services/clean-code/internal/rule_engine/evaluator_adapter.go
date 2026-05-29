package rule_engine

import (
	"context"
	"fmt"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/evaluator"
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
// typed [evaluator.Verdict] the evaluator surface expects.
//
// Validation: the projected verdict is checked via
// [evaluator.Verdict.IsValid] so a non-canonical engine
// rollup (`fail`/`gated`/...) is rejected here at the
// adapter boundary BEFORE it reaches the gate. The Stage
// 6.1 brief calls this out: "Verdict is the canonical
// enum `pass | warn | block` with no other values" --
// the closure is enforced at every trust boundary, not
// just at construction.
func (a *EvaluatorAdapter) RunSync(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID, policyVersionID uuid.UUID) (evaluator.EngineRunResult, error) {
	if a == nil || a.engine == nil {
		return evaluator.EngineRunResult{}, ErrStoreUnwired
	}
	r, err := a.engine.RunSync(ctx, repoID, sha, scope, policyVersionID)
	if err != nil {
		return evaluator.EngineRunResult{}, err
	}
	verdict := evaluator.Verdict(string(r.Verdict))
	if !verdict.IsValid() {
		return evaluator.EngineRunResult{}, fmt.Errorf("rule_engine: EvaluatorAdapter: non-canonical verdict %q (allowed: pass|warn|block)", r.Verdict)
	}
	return evaluator.EngineRunResult{
		EvaluationRunID:     r.EvaluationRunID,
		EvaluationVerdictID: r.EvaluationVerdictID,
		FindingIDs:          r.FindingIDs,
		Verdict:             verdict,
	}, nil
}

// Compile-time assertion that EvaluatorAdapter satisfies
// the evaluator's expected port.
var _ evaluator.RuleEngine = (*EvaluatorAdapter)(nil)
