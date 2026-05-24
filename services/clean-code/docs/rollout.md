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

## Stage 5.2: Policy publish/activate/rulepack verbs

### Pre-requisites

This stage builds on the Stage 5.1 signing-key cache. Before
rolling out 5.2, confirm Stage 5.1's bootstrap (master key,
migration 0005, env vars, `signing_key_cache` readiness)
already lands green per the section above. The publish verbs
refuse to write when no active signing key is loaded
(`ErrNoActiveSigningKey` -> 503).

### One-time bootstrap (per environment)

1. **Apply migration `0003_policy_audit_refactor`** against
   the shared Postgres instance. It creates the canonical
   `clean_code.{policy_version, policy_activation, rule_pack,
   rule}` tables plus the `clean_code.rule_severity` ENUM and
   the supporting append-only column constraints.

2. **Confirm the role grants** from `0004_roles.up.sql` are in
   place: the steward role must have `INSERT, SELECT` on all
   four tables and MUST NOT have `UPDATE` or `DELETE`. The
   policy/rules sub-store is append-only per architecture G3.

3. **No additional env vars** are required beyond Stage 5.1.
   The composition root constructs the steward against the
   same `*sql.DB` handle the keys subsystem uses; when
   `CLEAN_CODE_PG_URL` is set the steward writes to PostgreSQL,
   otherwise it falls back to the in-memory store
   (development only).

### Per-rollout verification

After deploying a new build, confirm:

1. `/readyz` returns 200 within 30s (Stage 5.1 cache loaded).

2. **Bootstrap a rulepack BEFORE the first `policy.publish`.**
   `Steward.Publish` enforces the JSON-FK contract on
   `rule_refs` -- a publish naming a `(rule_id, version)` that
   has not yet been registered returns **400** with
   `error:"unknown_rule_ref"`. On a fresh environment you MUST
   publish the rulepack(s) you intend to reference first:

   ```bash
   curl -fsS -X POST http://$POD:8080/v1/policy/publish_rulepack \
     -H 'Content-Type: application/json' \
     -d '{
       "pack_id": "solid.srp",
       "version": 1,
       "display_name": "Single Responsibility",
       "description_md": "SOLID SRP rulepack.",
       "rules": [
         {
           "rule_id": "solid.srp.lcom4_high",
           "version": 1,
           "predicate_dsl": "lcom4 > 0.7",
           "severity_default": "block",
           "description_md": "High LCOM4 -- methods share little state."
         }
       ]
     }' | jq .
   ```

   Expect 200 with the inserted `rule_pack` + `rules` echoed.
   A 409 means the pack was already published in a prior
   rollout -- safe to skip and move on. A 503 means the
   signing-key cache isn't loaded; triage via the Stage 5.1
   runbook before re-running.

3. A canary `policy.publish` succeeds (this references the
   rule registered in step 2):

   ```bash
   curl -fsS -X POST http://$POD:8080/v1/policy/publish \
     -H 'Content-Type: application/json' \
     -d '{
       "name": "canary",
       "rule_refs": [{"rule_id": "solid.srp.lcom4_high", "version": 1}],
       "threshold_refs": [],
       "refactor_weights": {
         "alpha": 0.4, "beta": 0.3, "gamma": 0.2, "delta": 0.1,
         "effort_model_version": "v1.0",
         "window_days": 90
       }
     }' | jq .
   ```

   Expect 200 + a `policy_version_id`, `signature`, and
   `created_at`. A 400 with `error:"unknown_rule_ref"` here
   means step 2 was skipped (or the `rule_id`/`version` in
   this body does not match what step 2 registered). A 503
   indicates the signing-key cache isn't loaded -- triage via
   the Stage 5.1 runbook.

4. A `policy.activate` against the returned id succeeds:

   ```bash
   curl -fsS -X POST http://$POD:8080/v1/policy/activate \
     -H 'Content-Type: application/json' \
     -d '{"policy_version_id":"<uuid from step 3>","activated_by":"rollout"}' | jq .
   ```

5. The banned-verb paths return 501 (NOT 404 -- the route is
   intentionally mounted as a "verb is rejected" signal):

   ```bash
   curl -i -X POST http://$POD:8080/v1/policy/rulepack/add -d '{}'
   # HTTP/1.1 501 Not Implemented
   ```

6. The audit log shows `policy_version`, `policy_activation`,
   `rule_pack`, and `rule` writes by the
   `clean_code_policy_steward` role only -- no other role
   should appear in INSERT statements against these tables.

### Backout

Stage 5.2 is purely additive (new routes + new tables in
migration 0003). To back out:

1. Stop calling the new verbs from the operator dashboard
   client.
2. Restart the pod with the previous build; the new tables
   stay populated (append-only) but are no longer written.

There is no DOWN-migration step required for backout because
the existing append-only rows do not block the prior build.


## Stage 5.3: `mgmt.override` write verb

### Migration

**None required.** The `clean_code.override` table (with the
`override_reason_required_when_muted` CHECK constraint and the
`override_rule_created_idx (rule_id, created_at DESC)` index)
shipped in migration 0003 during Stage 1.4. Stage 5.3 is the
first stage to actually write rows; no schema change is needed.

### Env vars

**None.** The verb reuses the existing `CLEAN_CODE_PG_URL`
(append target) and the auth gateway's existing
`X-OIDC-Subject` header contract that Stage 5.2 introduced. No
KMS key is required -- the override row carries no signature
column (kill-switch contract: the verb must work during a
signing-key outage).

### Per-rollout verification

After deploying a new build:

1. `/healthz` returns 200.
2. `/readyz` returns 200 (steward wired).
3. **Smoke-test the verb** with a known `rule_id` (the one the
   smoke-test rulepack registers in Stage 5.2):

   ```bash
   curl -X POST \
        -H 'Content-Type: application/json' \
        -H 'X-OIDC-Subject: rollout-smoke@operator.local' \
        --data '{"rule_id":"solid.srp.lcom4_high",
                 "scope_filter":{"repo_id":"smoke","scope_kind":"repo","scope_signature_glob":"*"},
                 "mute":true,"reason":"rollout smoke"}' \
        https://clean-coded.example.com/v1/mgmt/override
   ```

   Expect 200 + `{"override_id":"..."}`. A 503 indicates the
   steward is not wired; a 400 with "expires_at" or "actor_id"
   in the error body indicates a client typing the body the
   wrong way; a 401 indicates the gateway is not setting
   `X-OIDC-Subject`.

4. **Unmute the smoke row** with `mute=false` so the smoke
   rule is not silenced for real evaluator traffic. The
   evaluator (Stage 5.7) will read the LATEST row and see the
   row is back to unmuted.

5. The audit log shows `override` writes by the
   `clean_code_policy_steward` role only.

### Backout

Stage 5.3 is purely additive (new route + first writes against
the migration-0003 `override` table). To back out:

1. Stop calling `mgmt.override` from the operator dashboard.
2. Restart the pod with the previous build; the new route is
   gone but the `override` rows remain. The evaluator (Stage
   5.7) is not yet shipped, so the rows are inert.

There is no DOWN-migration required for backout. The
`override` table stays populated.
