//go:build e2e

package repoindexer

import "context"

// RunFullForE2E exposes the unexported (*Worker).runFull method
// for e2e testing. The caller constructs a real *Worker (via
// NewWorker with sqlmock-backed dependencies) and hands it here;
// the bridge simply forwards to runFull so the actual production
// code path executes.
func RunFullForE2E(ctx context.Context, w *Worker, job Job) (FullSummary, error) {
	return w.runFull(ctx, job)
}
