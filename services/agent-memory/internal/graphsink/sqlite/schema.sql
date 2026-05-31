-- schema.sql -- SQLite mirror of the Postgres schema this story's
-- Sink writes through.
--
-- This file is the bootstrap DDL the `sqlite.Sink` Open path
-- applies once per database file. It mirrors the column shape of
-- `migrations/0001_enums.sql`, `migrations/0002_repo_commit.sql`,
-- and `migrations/0003_node_edge.sql` (plus the closed-set
-- expansion in `migrations/0022_edge_kind_overrides.sql`) so a
-- repo scanned to SQLite and later re-scanned to Postgres yields
-- IDENTICAL node identities (the S3.4 backend-parity invariant).
--
-- Type mapping (operator-pinned, tech-spec §8.7.1 read together
-- with workstream brief 2026-05-30):
--
--   Postgres `uuid`        -> SQLite `TEXT` carrying the
--                              canonical 36-character UUID string
--                              (`fingerprint.RepoID.String()` and
--                              `uuid.NewString()` both emit this
--                              form). SQLite has no native UUID;
--                              keeping the string form means
--                              `repo_id::text` joins in the
--                              Postgres reader and the SQLite
--                              reader have IDENTICAL wire shape.
--
--   Postgres `bytea`       -> SQLite `BLOB`. The 32-byte
--                              fingerprint G2 invariant is
--                              enforced by the same CHECK shape
--                              as the Postgres
--                              `node_fingerprint_octet_length_chk`
--                              / `edge_fingerprint_octet_length_chk`.
--
--   Postgres `jsonb`       -> SQLite `TEXT` with a
--                              `CHECK (json_valid(attrs_json))`
--                              constraint, matching the
--                              jsonb-NOT-NULL-DEFAULT-'{}' shape
--                              Postgres enforces structurally.
--                              SQLite's JSON1 extension is bundled
--                              in `mattn/go-sqlite3`'s default
--                              build, so `json_valid()` is
--                              available without a pragma.
--
--   Postgres `timestamptz` -> SQLite `INTEGER` carrying Unix
--                              milliseconds (UTC). The writer
--                              passes `time.Time.UnixMilli()` so
--                              the SQLite store has a stable,
--                              orderable numeric representation
--                              that round-trips through
--                              `time.UnixMilli()` without losing
--                              the timezone semantics Postgres
--                              `timestamptz` preserves.
--
--   Postgres ENUMs         -> SQLite `TEXT` columns with `CHECK`
--                              constraints listing the closed
--                              member set. The set is the union of
--                              `migrations/0001_enums.sql` (initial
--                              members) and any later widening
--                              migration -- today only
--                              `migrations/0022_edge_kind_overrides.sql`,
--                              which appends `'overrides'` to
--                              `edge_kind`.
--
-- FOREIGN KEY enforcement: SQLite's `PRAGMA foreign_keys = ON`
-- is OFF by default. The `sqlite.Sink.Open` path issues that
-- pragma on every connection (see `sink.go`), and the
-- `ON DELETE RESTRICT` clauses below mirror the Postgres FK
-- shape so a misordered insert fails the same way on both
-- backends.
--
-- IDEMPOTENCY: every CREATE statement uses `IF NOT EXISTS` so
-- `Open` is safe to call against an existing database file
-- without an explicit migration version table. Schema evolution
-- (new columns, new closed-set members) will land as additive
-- `ALTER TABLE` blocks alongside the Postgres migration that
-- introduced the change.

-- ----- repo / repo_commit ------------------------------------------

CREATE TABLE IF NOT EXISTS repo (
    repo_id          TEXT    PRIMARY KEY,
    url              TEXT    NOT NULL,
    default_branch   TEXT    NOT NULL,
    current_head_sha TEXT    NOT NULL,
    language_hints   TEXT    NOT NULL DEFAULT '[]'
                     CHECK (json_valid(language_hints)),
    created_at       INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS repo_url_uidx ON repo (url);

CREATE TABLE IF NOT EXISTS repo_commit (
    repo_id      TEXT    NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    sha          TEXT    NOT NULL,
    parent_sha   TEXT,
    committed_at INTEGER NOT NULL,
    index_status TEXT    NOT NULL DEFAULT 'pending',
    PRIMARY KEY (repo_id, sha)
);

CREATE INDEX IF NOT EXISTS repo_commit_committed_at_idx
    ON repo_commit (repo_id, committed_at DESC);

-- ----- node / edge -------------------------------------------------

CREATE TABLE IF NOT EXISTS node (
    node_id             TEXT PRIMARY KEY,
    fingerprint         BLOB NOT NULL,
    repo_id             TEXT NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    kind                TEXT NOT NULL
                        CHECK (kind IN ('repo','package','file','class','method','block')),
    canonical_signature TEXT NOT NULL,
    parent_node_id      TEXT          REFERENCES node (node_id) ON DELETE RESTRICT,
    from_sha            TEXT NOT NULL,
    attrs_json          TEXT NOT NULL DEFAULT '{}'
                        CHECK (json_valid(attrs_json)),
    CONSTRAINT node_fingerprint_octet_length_chk
        CHECK (length(fingerprint) = 32)
);

CREATE UNIQUE INDEX IF NOT EXISTS node_repo_fingerprint_uidx
    ON node (repo_id, fingerprint);

CREATE INDEX IF NOT EXISTS node_parent_idx
    ON node (parent_node_id)
    WHERE parent_node_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS node_repo_kind_idx
    ON node (repo_id, kind);

CREATE TABLE IF NOT EXISTS edge (
    edge_id     TEXT PRIMARY KEY,
    fingerprint BLOB NOT NULL,
    repo_id     TEXT NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    kind        TEXT NOT NULL
                CHECK (kind IN (
                    'contains','imports','static_calls','observed_calls',
                    'extends','implements','reads','writes','renamed_to',
                    'overrides'
                )),
    src_node_id TEXT NOT NULL REFERENCES node (node_id) ON DELETE RESTRICT,
    dst_node_id TEXT NOT NULL REFERENCES node (node_id) ON DELETE RESTRICT,
    from_sha    TEXT NOT NULL,
    attrs_json  TEXT NOT NULL DEFAULT '{}'
                CHECK (json_valid(attrs_json)),
    CONSTRAINT edge_fingerprint_octet_length_chk
        CHECK (length(fingerprint) = 32)
);

CREATE UNIQUE INDEX IF NOT EXISTS edge_repo_fingerprint_uidx
    ON edge (repo_id, fingerprint);

CREATE INDEX IF NOT EXISTS edge_src_kind_idx ON edge (src_node_id, kind);
CREATE INDEX IF NOT EXISTS edge_dst_kind_idx ON edge (dst_node_id, kind);
