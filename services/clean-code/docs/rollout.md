# `services/clean-code` rollout playbook

How to roll a new build of the clean-code service into a
production-tier environment. The instructions assume Postgres
14+ is already running and the `clean_code_*` roles have been
created via the `0004_roles.up.sql` migration.

## Stage 5.1: Policy Steward signing-key store

### One-time bootstrap (per environment)

1. **Generate the AES-256 master key** for the LocalSealedKMS:

   ```bash
   openssl rand -hex 32
   ```

   This is 64 hex chars. Store it in your secret manager (Key
   Vault, AWS Secrets Manager, Vault, ...) as
   `clean-code/kms/master-key`. **DO NOT commit it to source.**
   **DO NOT log it.**

2. **Apply migration `0005_policy_signing_keys`** against the
   shared Postgres instance. The migration grants:
   * `INSERT, SELECT` to `clean_code_policy_steward` (sole
     writer);
   * `SELECT` to every other writer role;
   * REVOKEs `UPDATE, DELETE` from every grantee (append-only).

   The table is created on `clean_code` schema and is
   independent of every other 0001-0004 migration.

3. **Set the env vars** on the long-running process:

   | Env var                           | Value                                                |
   | --------------------------------- | ---------------------------------------------------- |
   | `CLEAN_CODE_KMS_PROVIDER`         | `local`                                              |
   | `CLEAN_CODE_KMS_MASTER_KEY_HEX`   | the 64-hex master key from step 1                    |
   | `CLEAN_CODE_PG_URL`               | postgres connection string (with steward role auth)  |

   Production deploys MUST set all three. Scaffold-mode
   (`KMS_PROVIDER=""`) is dev-loop only and leaves the signing
   cache disabled.

### Per-rollout verification

After deploying a new build, confirm:

1. `/healthz` returns 200 within 5s of pod-ready.
2. `/readyz` flips to 200 within 30s -- the `signing_key_cache`
   readiness check passes once the KMS responds and at least
   one key is loaded.
3. `GET /v1/policy/keys/list_active` returns a single-entry
   array on first boot:

   ```bash
   curl -fsS http://$POD:8080/v1/policy/keys/list_active | jq .
   ```

4. The audit log shows `policy_signing_keys` writes by
   `clean_code_policy_steward` only -- no other role.

### Key compromise drill

If a private key is suspected compromised:

1. Page the on-call holder of `clean-code/kms/master-key`.
2. Run the `keys.compromise` runbook step (TBD in Stage 5.2).
   This calls `Manager.ForceRotate`, bypassing the 24h overlap
   guard.
3. Revoke the compromised key by inserting a new row and
   verifying the old row's `derived valid_until` falls inside
   the past (verification will fail at
   `now >= compromised_key.valid_until`).
4. Audit-log the rotation event with severity `high` and
   `kind=key_compromise`.

### Migration rollback

`0005_policy_signing_keys.down.sql` drops the table and its
index. **Never run on a production environment that has
already minted production keys** -- doing so destroys the
public-key record and every signed bundle becomes
unverifiable. The DOWN is reserved for clean-room dev-loop
re-bootstraps.
