-- 001_up.sql — Policy, Audit, and Refactor schema for clean_code
-- Applied by `make migrate-up`; torn down by `make migrate-down`.

BEGIN;

CREATE SCHEMA IF NOT EXISTS clean_code;

-- -----------------------------------------------------------------------
-- Shared parent: repo
-- -----------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS clean_code.repo (
    repo_id         UUID PRIMARY KEY,
    display_name    TEXT NOT NULL,
    default_branch  TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- -----------------------------------------------------------------------
-- evaluation_verdict  (Sec 5.3)
-- verdict enum: pass | warn | block  (NOT fail, NOT gated)
-- degraded_reason: closed set enforced by CHECK
-- -----------------------------------------------------------------------
CREATE TYPE clean_code.verdict_enum AS ENUM ('pass', 'warn', 'block');

CREATE TABLE clean_code.evaluation_verdict (
    evaluation_verdict_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id                UUID NOT NULL REFERENCES clean_code.repo(repo_id),
    verdict                clean_code.verdict_enum NOT NULL,
    degraded_reason        TEXT,
    evaluated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_degraded_reason CHECK (
        degraded_reason IS NULL
        OR degraded_reason IN (
            'metric_regression',
            'threshold_breach',
            'stale_data',
            'percentile_stale'
        )
    )
);

-- -----------------------------------------------------------------------
-- override  (Sec 5.4) — NO expires_at column
-- -----------------------------------------------------------------------
CREATE TABLE clean_code.override (
    override_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id         UUID NOT NULL REFERENCES clean_code.repo(repo_id),
    rule_name       TEXT NOT NULL,
    justification   TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- -----------------------------------------------------------------------
-- finding  (Sec 5.5.1)
-- delta enum: new | newly_failing | unchanged | resolved
-- -----------------------------------------------------------------------
CREATE TYPE clean_code.finding_delta_enum AS ENUM (
    'new', 'newly_failing', 'unchanged', 'resolved'
);

CREATE TABLE clean_code.finding (
    finding_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id     UUID NOT NULL REFERENCES clean_code.repo(repo_id),
    rule_name   TEXT,
    file_path   TEXT,
    delta       clean_code.finding_delta_enum NOT NULL,
    message     TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- -----------------------------------------------------------------------
-- refactor_task  (Sec 5.5.3) — canonical columns ONLY
-- NO status column, NO expected_metric_delta column
-- -----------------------------------------------------------------------
CREATE TABLE clean_code.refactor_task (
    refactor_task_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id           UUID NOT NULL REFERENCES clean_code.repo(repo_id),
    finding_id        UUID REFERENCES clean_code.finding(finding_id),
    title             TEXT NOT NULL,
    description       TEXT,
    metric_name       TEXT NOT NULL,
    target_path       TEXT,
    priority          INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- -----------------------------------------------------------------------
-- policy_activation  (Sec 5.6) — latest-row-wins
-- NO scope column, NO deactivated_at column, NO partial unique index
-- -----------------------------------------------------------------------
CREATE TABLE clean_code.policy_activation (
    policy_activation_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_chain_id       TEXT NOT NULL,
    repo_id               UUID NOT NULL REFERENCES clean_code.repo(repo_id),
    config_json           JSONB,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;