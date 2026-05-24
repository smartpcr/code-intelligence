-- 0005_policy_signing_keys.up.sql
--
-- Stage 5.1 (implementation-plan.md "Policy Steward signing key
-- store"): create the `clean_code.policy_signing_keys` table named
-- in tech-spec Sec 8.4 ("Active and prior public keys live in the
-- `clean_code.policy_signing_keys` table. Private keys live in the
-- deployment's secret manager.").
--
-- Append-only. One row per signing-key generation. The PRIVATE
-- key NEVER lives in this table -- only the Ed25519 public key
-- (32 bytes), an opaque KMS handle reference (`key_handle`) the
-- KMS resolves back to the sealed private key, and the public
-- fingerprint that callers can quote when reporting verification
-- failures. The `policy.keys.list_active` read verb (Stage 5.1)
-- projects rows through this table.
--
-- Lifecycle model (tech-spec Sec 8.2 row 6 + Sec 8.4 "Overlap"):
-- the upper bound `valid_until` is NOT stored. It is derived at
-- read time as
--
--     valid_until(k) := next(k).valid_from + policy_publish_overlap_min_seconds
--
-- where `next(k)` is the row with the smallest `valid_from`
-- strictly greater than `k.valid_from` (tie-break by `key_id`).
-- For the newest key (no successor) `valid_until` is the
-- application's far-future sentinel. This keeps the table
-- append-only without UPDATEs -- the moment a successor row is
-- inserted, the prior key's effective `valid_until` becomes a
-- bounded value derived from the successor's `valid_from`.
--
-- The implementation-plan Stage 5.1 "overlap-window-enforced"
-- scenario reads from this model directly: at T0 the new key is
-- inserted; the old key's derived valid_until = T0 +
-- policy_publish_overlap_min_seconds. At T0+23h59m the old key
-- is still inside [valid_from, valid_until) so verification
-- succeeds; at T0+24h+1s the old key is outside so verification
-- fails (half-open upper bound).

BEGIN;

CREATE TABLE clean_code.policy_signing_keys (
    -- The `signing_key_id` Stage 5.1 brief names. UUID so a
    -- compromise / forensic trace can correlate a finding's
    -- key_id back to exactly one row without ambiguity.
    key_id        uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    -- SHA-256 of the public key, hex-encoded lowercase. The
    -- `policy.keys.list_active` verb returns this verbatim so an
    -- operator can pin the fingerprint into runbook / change
    -- management tickets without exposing the raw public key.
    -- 64 hex chars + nothing else; the LENGTH+regex CHECK below
    -- enforces shape.
    fingerprint   text           NOT NULL UNIQUE,
    -- Ed25519 public key (RFC 8032). Exactly 32 bytes. The
    -- CHECK constraint enforces shape so a future migration
    -- cannot silently widen the algorithm without surfacing
    -- here.
    public_key    bytea          NOT NULL,
    -- Opaque KMS handle the operator's secret manager resolves
    -- back to the sealed private key. Tech-spec Sec 8.4 pins
    -- private keys live "in the deployment's secret manager
    -- (out-of-band; service reads via env var on boot)". The
    -- handle is treated as non-secret reference material
    -- (typically a Key Vault key URI or env-var name); the
    -- column is non-null so a row is always actionable for
    -- Sign().
    key_handle    text           NOT NULL,
    -- The wall-clock moment the key entered service. Derived
    -- `valid_until` is computed against this column on the next
    -- key's row. ORDER BY (valid_from ASC, key_id ASC) is the
    -- canonical lookup so a tied timestamp resolves
    -- deterministically.
    valid_from    timestamptz    NOT NULL DEFAULT now(),
    -- Closed-set algorithm label so a future bump (e.g.
    -- post-quantum Dilithium3) lands as a new label rather than
    -- overloading the public_key column shape. Tech-spec Sec
    -- 8.4 pins Ed25519 as the v1 algorithm; the CHECK enforces
    -- that.
    algorithm     text           NOT NULL DEFAULT 'ed25519',

    CONSTRAINT policy_signing_keys_fingerprint_shape CHECK (
        fingerprint ~ '^[0-9a-f]{64}$'
    ),
    CONSTRAINT policy_signing_keys_public_key_ed25519_len CHECK (
        octet_length(public_key) = 32
    ),
    CONSTRAINT policy_signing_keys_algorithm_canonical CHECK (
        algorithm IN ('ed25519')
    )
);

COMMENT ON TABLE clean_code.policy_signing_keys IS
    'Policy / rules sub-store (architecture Sec 1.5 G1; tech-spec '
    'Sec 8.4). Append-only. Writer: the Policy Steward signing-key '
    'manager (internal/policy/keys). Stores ONLY the Ed25519 public '
    'key + an opaque KMS handle -- the private key lives in the '
    'deployment''s secret manager and is never persisted here. '
    'Lifecycle: each row records a key''s `valid_from`; the '
    '`valid_until` upper bound is DERIVED at read time as '
    'next_key.valid_from + policy_publish_overlap_min_seconds '
    '(tech-spec Sec 8.2 row 6, default 86400s/24h). There is '
    'intentionally NO `valid_until`, NO `retired_at`, and NO '
    'private-key column. Half-open interval semantics: a key '
    'verifies for `[valid_from, valid_until)` -- at exactly '
    'valid_until verification fails.';

COMMENT ON COLUMN clean_code.policy_signing_keys.key_id IS
    'The Stage 5.1 `signing_key_id`. UUID so audit / forensic '
    'traces can correlate any finding''s policy_version.signature '
    'back to exactly one signing-key row.';

COMMENT ON COLUMN clean_code.policy_signing_keys.fingerprint IS
    'SHA-256 of the public key, hex-encoded lowercase (64 chars). '
    'The `policy.keys.list_active` read verb returns this '
    'verbatim so operators can pin the fingerprint in runbook / '
    'change-management tickets without exposing the raw bytes.';

COMMENT ON COLUMN clean_code.policy_signing_keys.public_key IS
    'Ed25519 public key bytes (RFC 8032) -- exactly 32 bytes. '
    'The `_ed25519_len` CHECK constraint enforces shape so a '
    'future algorithm bump must land as a new row with the '
    'matching `algorithm` label, never by widening this column.';

COMMENT ON COLUMN clean_code.policy_signing_keys.key_handle IS
    'Opaque KMS handle the operator''s secret manager resolves '
    'to the sealed private key (typically a Key Vault key URI '
    'or env-var name). NON-SECRET reference material; never the '
    'private key itself.';

COMMENT ON COLUMN clean_code.policy_signing_keys.valid_from IS
    'Wall-clock moment the key entered service. The implicit '
    '`valid_until` is computed against the NEXT row''s '
    'valid_from + policy_publish_overlap_min_seconds (tech-spec '
    'Sec 8.2). Two keys may co-exist during the overlap window; '
    'both successfully verify (C13 mitigation).';

COMMENT ON COLUMN clean_code.policy_signing_keys.algorithm IS
    'Closed-set algorithm label; v1 pins `ed25519` per tech-spec '
    'Sec 8.4. A future algorithm bump (e.g. post-quantum '
    'Dilithium3) lands as a new label here rather than '
    'overloading public_key column shape.';

-- The Evaluator and Refactor Planner read these rows on every
-- `eval.gate` signature verification; an index on the canonical
-- `(valid_from DESC, key_id DESC)` ordering keeps the active-set
-- lookup a single index range scan.
CREATE INDEX policy_signing_keys_valid_from_idx
    ON clean_code.policy_signing_keys (valid_from DESC, key_id DESC);

-- ---------------------------------------------------------------------------
-- Privileges (tech-spec Sec 7.2 -- writer isolation C5)
-- ---------------------------------------------------------------------------
--
-- The Policy Steward is the SOLE writer of this table. Every
-- other writer role gets SELECT only so:
--   * the Evaluator Surface can verify `policy_version.signature`
--     against the active set on every `eval.gate` call;
--   * the WAL Reconciler can re-verify replayed PolicyVersion
--     rows;
--   * the Insights / Management read paths can project the
--     `policy.keys.list_active` shape.
-- Append-only per G3 -- UPDATE and DELETE are revoked from
-- every grantee plus PUBLIC.

GRANT INSERT, SELECT ON clean_code.policy_signing_keys TO clean_code_policy_steward;

GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_repo_indexer;
GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_metric_ingestor;
GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_xrepo_aggregator;
GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_solid_batch;
GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_evaluator;
GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_wal_reconciler;
GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_refactor_planner;
GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_management;

REVOKE UPDATE, DELETE ON clean_code.policy_signing_keys FROM PUBLIC, clean_code_policy_steward, clean_code_repo_indexer, clean_code_metric_ingestor, clean_code_xrepo_aggregator, clean_code_solid_batch, clean_code_evaluator, clean_code_wal_reconciler, clean_code_refactor_planner, clean_code_management;

COMMIT;
