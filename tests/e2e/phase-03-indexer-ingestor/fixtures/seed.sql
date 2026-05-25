-- seed.sql — Phase-03 E2E fixture corpus: 3 repos, 12 SHAs.
-- Idempotent via ON CONFLICT DO NOTHING.

-- Repos
INSERT INTO clean_code.repo (repo_id, display_name, default_branch) VALUES
    ('00000000-0000-0000-0000-000000000001', 'e2e-repo-alpha', 'main'),
    ('00000000-0000-0000-0000-000000000002', 'e2e-repo-beta',  'main'),
    ('00000000-0000-0000-0000-000000000003', 'e2e-repo-gamma', 'develop')
ON CONFLICT (repo_id) DO NOTHING;

-- Commits — 4 per repo, all starting in 'pending'.
INSERT INTO clean_code.commit (sha, repo_id, scan_status) VALUES
    ('aaa0000000000001', '00000000-0000-0000-0000-000000000001', 'pending'),
    ('aaa0000000000002', '00000000-0000-0000-0000-000000000001', 'pending'),
    ('aaa0000000000003', '00000000-0000-0000-0000-000000000001', 'pending'),
    ('aaa0000000000004', '00000000-0000-0000-0000-000000000001', 'pending'),
    ('bbb0000000000001', '00000000-0000-0000-0000-000000000002', 'pending'),
    ('bbb0000000000002', '00000000-0000-0000-0000-000000000002', 'pending'),
    ('bbb0000000000003', '00000000-0000-0000-0000-000000000002', 'pending'),
    ('bbb0000000000004', '00000000-0000-0000-0000-000000000002', 'pending'),
    ('ccc0000000000001', '00000000-0000-0000-0000-000000000003', 'pending'),
    ('ccc0000000000002', '00000000-0000-0000-0000-000000000003', 'pending'),
    ('ccc0000000000003', '00000000-0000-0000-0000-000000000003', 'pending'),
    ('ccc0000000000004', '00000000-0000-0000-0000-000000000003', 'pending')
ON CONFLICT (sha) DO NOTHING;
