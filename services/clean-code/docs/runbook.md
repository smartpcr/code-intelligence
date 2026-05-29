# Operator Runbook: `services/clean-code`

This runbook documents the canonical management, policy, and evaluation
verbs exposed by the clean-code service, along with operator triage
procedures for each verb's failure modes.

## Canonical verb reference

The service exposes exactly seven canonical verbs:

| Verb                   | Route                            | Description                        |
|------------------------|----------------------------------|------------------------------------|
| `mgmt.register_repo`  | `POST /v1/mgmt/register_repo`   | Register a repository for scanning |
| `mgmt.retract_sample` | `POST /v1/mgmt/retract_sample`  | Retract a code sample              |
| `mgmt.rescan`         | `POST /v1/mgmt/rescan`          | Trigger a repository rescan        |
| `mgmt.override`       | `POST /v1/mgmt/override`        | Create/update a rule override      |
| `policy.publish`      | `POST /v1/policy/publish`       | Publish a policy version           |
| `policy.activate`     | `POST /v1/policy/activate`      | Activate a published policy        |
| `eval.gate`           | `POST /v1/eval/gate`            | Evaluate a commit against policy   |

> **Important**: Only the verbs listed above are part of the canonical
> API surface. Non-canonical names must not be used in operator tooling
> or documentation.

## Operator triage

### `mgmt.register_repo` failures

- **HTTP 409 (conflict)**: Repository already registered. Verify via
  `SELECT repo_id FROM clean_code.repo WHERE url = '<url>'`.
- **HTTP 500**: Check Postgres connectivity and `clean_code_writer` role
  permissions.

### `mgmt.retract_sample` failures

- **HTTP 404**: Sample not found. Confirm sample_id exists in
  `clean_code.sample`.
- **HTTP 500**: Check for foreign-key constraint violations.

### `mgmt.rescan` failures

- **HTTP 404**: Repository not registered. Use `mgmt.register_repo` first.
- **HTTP 503**: Scanner pool exhausted. Check pod scaling.

### `mgmt.override` failures

- **HTTP 400**: Invalid rule_id or scope. Verify against
  `clean_code.rule_definition`.
- **HTTP 500**: Database write failure; check connection pool.

### `policy.publish` failures

- **HTTP 400**: Policy validation error. Check rule references.
- **HTTP 409**: Version already published.

### `policy.activate` failures

- **HTTP 404**: Policy version not found. Publish first via
  `policy.publish`.
- **HTTP 409**: Already active.

### `eval.gate` failures

- **HTTP 409 (no active policy)**: No policy activated for this repo.
  Activate via `policy.activate`.
- **HTTP 200 with `degraded_reason='samples_pending'`**: Inspect the
  underlying state with a direct table read --
  `SELECT scan_status FROM clean_code.commit WHERE repo_id = '<uuid>'
  AND sha = '<hex>'` -- and wait for the metric ingestor to catch up.
