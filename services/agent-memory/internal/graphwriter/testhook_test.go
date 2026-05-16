package graphwriter

// This file contains the writer "test hook" used to exercise the
// WriteContractViolation path for the Stage 2.1 scenario
// "writer denied UPDATE". Because it lives in a `_test.go` file
// it is compiled ONLY when the test binary is built — never into
// the production library. Production callers cannot reach
// `forceUpdateNodeForTesting` even through package-private
// access; the Go build system simply does not emit the symbol
// outside of `go test`.
//
// This file lives in `package graphwriter` (not
// `graphwriter_test`) so the integration tests in
// writer_integration_test.go — which are also in
// `package graphwriter` — can call the test hook by name.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// forceUpdateNodeForTesting attempts an UPDATE on `node` from
// inside the writer's runInTx + classifyErr plumbing. The
// agent_memory_app role does not have UPDATE on `node` (per
// migration 0016), so PostgreSQL returns SQLSTATE 42501 and the
// writer surfaces a typed *WriteContractViolation. This proves
// the role-grant policy is the load-bearing G5 enforcer the
// architecture says it is — and that the writer's classifier
// covers production-shaped code, not just a hand-rolled fixture.
func (w *Writer) forceUpdateNodeForTesting(
	ctx context.Context, nodeID string,
) error {
	if nodeID == "" {
		return errors.New("graphwriter: forceUpdateNodeForTesting: empty node_id")
	}
	return w.runInTx(ctx, "force_update_for_testing", func(tx *sql.Tx) error {
		// Any UPDATE on `node` is forbidden for `agent_memory_app`
		// per migration 0016. Setting attrs_json to itself is the
		// minimum-side-effect statement we can issue: if the role
		// somehow does have UPDATE the row is unchanged, but the
		// SQLSTATE 42501 path is the only one we expect here.
		res, err := tx.ExecContext(ctx,
			`UPDATE node SET attrs_json = attrs_json WHERE node_id = $1`,
			nodeID,
		)
		if err != nil {
			return err
		}
		// Defence in depth: if the UPDATE somehow succeeds the
		// test hook still fails loudly so the caller doesn't
		// silently pass on a misconfigured role.
		n, _ := res.RowsAffected()
		return fmt.Errorf(
			"graphwriter: forceUpdateNodeForTesting: UPDATE unexpectedly succeeded (%d rows)",
			n,
		)
	})
}
