package repoindexer

import (
	"context"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/retirement"
)

// RetirementAdapter wraps a *retirement.Service so it satisfies
// the repoindexer-local `Retirer` interface. The split exists
// for two reasons:
//
//  1. The local interface narrows the surface to exactly what
//     the delta handler calls; tests can substitute a fake
//     without standing up a PostgreSQL fixture or depending on
//     the retirement package's typed-error machinery.
//
//  2. The shape impedance: retirement.Service.RetireMany returns
//     a retirement.BatchResult; the local contract returns
//     RetireBatchResult. The adapter is the single place the
//     translation lives so call sites stay shape-agnostic.
//
// Production wiring (cmd/repoindexer/main.go) constructs the
// adapter via `NewRetirementAdapter` and passes it to
// `WorkerOptions.Retirer`.
type RetirementAdapter struct {
	svc *retirement.Service
}

// NewRetirementAdapter wraps the supplied service. A nil service
// panics — the worker requires a configured retirer for delta
// mode and silent nil-acceptance would surface only on the first
// delta job (operationally too late).
func NewRetirementAdapter(svc *retirement.Service) *RetirementAdapter {
	if svc == nil {
		panic("repoindexer: NewRetirementAdapter: nil *retirement.Service")
	}
	return &RetirementAdapter{svc: svc}
}

// RetireMany delegates to retirement.Service.RetireMany and
// re-shapes the BatchResult into the local RetireBatchResult.
// Errors are forwarded unchanged so callers can errors.As against
// the retirement package's typed error family.
func (a *RetirementAdapter) RetireMany(ctx context.Context, nodeIDs []string, retiredAtSHA string) (RetireBatchResult, error) {
	res, err := a.svc.RetireMany(ctx, nodeIDs, retiredAtSHA)
	if err != nil {
		return RetireBatchResult{}, err
	}
	return RetireBatchResult{InsertedCount: res.InsertedCount}, nil
}

// RetireManyEdges delegates to retirement.Service.RetireManyEdges
// and re-shapes the EdgeBatchResult into the local
// RetireBatchResult. Symmetric with RetireMany.
func (a *RetirementAdapter) RetireManyEdges(ctx context.Context, edgeIDs []string, retiredAtSHA string) (RetireBatchResult, error) {
	res, err := a.svc.RetireManyEdges(ctx, edgeIDs, retiredAtSHA)
	if err != nil {
		return RetireBatchResult{}, err
	}
	return RetireBatchResult{InsertedCount: res.InsertedCount}, nil
}

// RetireNodeWithSupersede delegates to RetireNode with the
// supersede column populated. Distinct method (rather than
// folding into RetireMany with a separate supersede argument)
// because the retirement service deliberately does not expose
// supersede on the batch path; per-row supersede only makes
// sense in the rename scenario which is inherently per-row.
func (a *RetirementAdapter) RetireNodeWithSupersede(ctx context.Context, nodeID, retiredAtSHA, supersededByNodeID string) error {
	_, err := a.svc.RetireNode(ctx, retirement.NodeRetirementInput{
		NodeID:             nodeID,
		RetiredAtSHA:       retiredAtSHA,
		SupersededByNodeID: supersededByNodeID,
	})
	return err
}
