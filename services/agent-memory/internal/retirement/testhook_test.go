package retirement

// Writer "test hook" used to exercise the WriteContractViolation
// path. Because it lives in a `_test.go` file it is compiled ONLY
// when the test binary is built -- never into the production
// library. Production callers cannot reach
// `forceDeleteRetirementForTesting` even through package-private
// access; the Go build system simply does not emit the symbol
// outside of `go test`.
//
// The file lives in `package retirement` (not `retirement_test`)
// so the integration tests in service_integration_test.go -- which
// are also in `package retirement` -- can call the test hook by
// name. Pattern mirrored from graphwriter's testhook_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// forceDeleteRetirementForTesting attempts a DELETE on
// `node_retirement` from inside the service's runInTx +
// classifyErr plumbing. The agent_memory_app role does NOT have
// DELETE on node_retirement (per migration 0016: the table is in
// the INSERT/SELECT append-only set), so PostgreSQL returns
// SQLSTATE 42501 and the service surfaces a typed
// *WriteContractViolation. This proves the role-grant policy is
// the load-bearing G5 enforcer the architecture says it is.
func (s *Service) forceDeleteRetirementForTesting(
	ctx context.Context, nodeID string,
) error {
	if nodeID == "" {
		return errors.New(
			"retirement: forceDeleteRetirementForTesting: empty node_id")
	}
	return s.runInTx(ctx, "force_delete_for_testing", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM node_retirement WHERE node_id = $1`,
			nodeID,
		)
		if err != nil {
			return err
		}
		// Defence in depth: if the DELETE somehow succeeds the
		// test hook still fails loudly so the caller does not
		// silently pass on a misconfigured role.
		n, _ := res.RowsAffected()
		return fmt.Errorf(
			"retirement: forceDeleteRetirementForTesting: DELETE unexpectedly succeeded (%d rows)",
			n,
		)
	})
}
