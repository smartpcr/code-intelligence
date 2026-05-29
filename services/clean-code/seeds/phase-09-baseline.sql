-- phase-09-baseline.sql
-- Seeds baseline data required for phase-09 E2E tests:
-- a metric snapshot and an active PolicyVersion.

BEGIN;

-- Baseline metric snapshot.
INSERT INTO metric_sample (id, metric_name, value, created_at)
VALUES ('seed-metric-001', 'cyclomatic_complexity_p95', 12.5, NOW())
ON CONFLICT (id) DO NOTHING;

INSERT INTO metric_sample (id, metric_name, value, created_at)
VALUES ('seed-metric-002', 'duplication_ratio', 0.03, NOW())
ON CONFLICT (id) DO NOTHING;

-- Baseline evaluation run (active PolicyVersion context).
INSERT INTO evaluation_run (id, caller, status, detail, created_at)
VALUES ('seed-run-001', 'seed', 'complete', 'baseline policy version', NOW())
ON CONFLICT (id) DO NOTHING;

INSERT INTO evaluation_verdict (id, run_id, verdict, detail, created_at)
VALUES ('seed-verdict-001', 'seed-run-001', 'pass', 'baseline verdict', NOW())
ON CONFLICT (id) DO NOTHING;

COMMIT;