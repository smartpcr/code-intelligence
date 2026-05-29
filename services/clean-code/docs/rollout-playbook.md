# Rollout Playbook: `services/clean-code`

Deployment and rollout procedures for the clean-code service.

## Pre-deployment checklist

1. Ensure Postgres 14+ is running with `clean_code_*` roles provisioned.
2. Verify all migrations have been applied.
3. Confirm the canonical verb set is unchanged:
   - `mgmt.register_repo`
   - `mgmt.retract_sample`
   - `mgmt.rescan`
   - `mgmt.override`
   - `policy.publish`
   - `policy.activate`
   - `eval.gate`

## Rollout sequence

### Step 1: Database migration

Apply pending migrations via the migration runner.

### Step 2: Deploy service binaries

Deploy the clean-code-eval-gate and clean-code-policy-steward binaries.

### Step 3: Smoke tests

Run the canonical verb smoke tests:

```bash
# Register a test repo
curl -X POST /v1/mgmt/register_repo -d '{"url":"https://example.com/repo"}'

# Publish and activate a policy
curl -X POST /v1/policy/publish -d '{"rules":["SOLID-001"]}'
curl -X POST /v1/policy/activate -d '{"policy_version_id":"<id>"}'

# Gate check
curl -X POST /v1/eval/gate -d '{"repo_id":"<id>","sha":"<hex>"}'
```

### Step 4: Traffic cutover

Gradually shift traffic from old to new deployment.

## Rollback procedure

1. Revert binary deployment to previous version.
2. Verify `eval.gate` responses match expected format.
3. No database rollback needed (schema is backward-compatible).
