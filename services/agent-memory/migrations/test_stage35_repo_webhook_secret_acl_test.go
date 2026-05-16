package migrations_test

// Stage 3.5 ACL contract — `repo_webhook_secret` MUST be
// invisible to the reader role but readable + writable by the
// app role. Migration 0018 sets this up via:
//
//   * `agent_memory_app` -- GRANT INSERT, SELECT, UPDATE on the
//     new table (Webhook Receiver lookup + mgmt.register write).
//   * `agent_memory_ro`  -- REVOKE ALL PRIVILEGES (defeats the
//     ALTER DEFAULT PRIVILEGES auto-grant that 0017 installed
//     for every future table in the schema).
//
// If this contract regresses, the per-repo HMAC secret leaks to
// every recall / mgmt.read.* path that connects as
// `agent_memory_ro`. That is a Stage 3.5 trust-boundary breach
// (architecture.md §4.6) and warrants a hard test failure.
//
// We do NOT swap to a separate LOGIN-credentialed connection
// here -- the existing test_stage14_role_grants_test.go covers
// the LOGIN path for `agent_memory_app`. Verifying the privilege
// table via `SET ROLE` from the owner session is sufficient
// because PostgreSQL evaluates table privileges against
// current_user, which SET ROLE changes (see
// https://www.postgresql.org/docs/16/sql-set-role.html and
// https://www.postgresql.org/docs/16/ddl-priv.html). The
// session's superuser context does NOT bypass the check after
// SET ROLE.

import (
	"context"
	"errors"
	"testing"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

func TestRepoWebhookSecret_ACL_readerRoleCannotSelect_appRoleCan(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	if err := migrations.New(db).Up(ctx); err != nil {
		t.Fatalf("migrations.Up: %v", err)
	}

	// --- Negative: agent_memory_ro must NOT see the row -------
	if _, err := db.ExecContext(ctx, `SET ROLE agent_memory_ro`); err != nil {
		t.Fatalf("SET ROLE agent_memory_ro: %v", err)
	}
	_, err := db.ExecContext(ctx, `SELECT webhook_secret FROM repo_webhook_secret LIMIT 1`)
	if err == nil {
		// Reset role before failing so the cleanup hook still
		// works as the schema owner.
		_, _ = db.ExecContext(ctx, `RESET ROLE`)
		t.Fatal("agent_memory_ro SELECT on repo_webhook_secret succeeded; " +
			"expected permission-denied (SQLSTATE 42501). This is the " +
			"secret-leak regression migration 0018 was written to prevent.")
	}
	if !isInsufficientPrivilege(err) {
		_, _ = db.ExecContext(ctx, `RESET ROLE`)
		t.Fatalf("expected SQLSTATE 42501 (insufficient_privilege), got: %v", err)
	}
	if _, err := db.ExecContext(ctx, `RESET ROLE`); err != nil {
		t.Fatalf("RESET ROLE after ro probe: %v", err)
	}

	// --- Positive: agent_memory_app must be allowed to SELECT -
	if _, err := db.ExecContext(ctx, `SET ROLE agent_memory_app`); err != nil {
		t.Fatalf("SET ROLE agent_memory_app: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`SELECT webhook_secret FROM repo_webhook_secret WHERE repo_id = $1::uuid`,
		"00000000-0000-0000-0000-000000000000"); err != nil {
		_, _ = db.ExecContext(ctx, `RESET ROLE`)
		t.Fatalf("agent_memory_app SELECT on repo_webhook_secret failed: %v", err)
	}

	// --- Positive: agent_memory_app must be allowed to INSERT.
	// The row will be a no-op because no `repo` parent exists
	// for the synthetic UUID, so we expect either success (only
	// possible if the FK is missing -- it isn't) or an FK
	// violation (SQLSTATE 23503). We MUST NOT see SQLSTATE
	// 42501 (insufficient_privilege); that is the bug. Postgres
	// auto-rolls back the implicit transaction for the failed
	// statement, so connection state remains usable.
	_, insertErr := db.ExecContext(ctx,
		`INSERT INTO repo_webhook_secret (repo_id, webhook_secret)
		 VALUES ($1::uuid, $2)`,
		"00000000-0000-0000-0000-000000000000", "probe-secret-value")
	if insertErr != nil && isInsufficientPrivilege(insertErr) {
		_, _ = db.ExecContext(ctx, `RESET ROLE`)
		t.Fatalf("agent_memory_app INSERT on repo_webhook_secret was denied: %v", insertErr)
	}
	// Anything else (nil, FK violation, NOT-NULL etc.) is fine
	// for this assertion -- we are checking GRANT, not data
	// integrity.

	if _, err := db.ExecContext(ctx, `RESET ROLE`); err != nil {
		t.Fatalf("RESET ROLE after app probe: %v", err)
	}
}

// isInsufficientPrivilege returns true when err is a *pq.Error
// carrying SQLSTATE 42501 (insufficient_privilege). Other error
// shapes (network / driver / wrapping) are explicitly rejected
// so a Postgres outage cannot masquerade as a privilege success.
func isInsufficientPrivilege(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	return string(pqErr.Code) == "42501"
}
