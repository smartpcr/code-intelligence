-- 01-extensions.sql
--
-- Idempotent extension bootstrap run by the Postgres docker entry
-- point on first start (when the data directory is empty). It
-- creates the two extensions Stage 1.1 promises are present in the
-- local stack: `pgcrypto` (ships with the postgres:16 base image)
-- and `pg_partman` (installed by the sibling Dockerfile).
--
-- This file is mounted read-only into /docker-entrypoint-initdb.d
-- and is not re-executed on subsequent container restarts; that is
-- fine because `CREATE EXTENSION IF NOT EXISTS` is idempotent and
-- migrations 1.2+ may re-run it inside their own transaction
-- without harm.

\connect agent_memory

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- pg_partman wants its own schema by convention.
CREATE SCHEMA IF NOT EXISTS partman;
CREATE EXTENSION IF NOT EXISTS pg_partman WITH SCHEMA partman;
