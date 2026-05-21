-- Stage 1.1 init script: enable pgcrypto in the `clean_code`
-- database so the Stage 1.2 migrations can mint UUIDv4 / UUIDv5
-- primary keys with `gen_random_uuid()` and `digest()`.
--
-- The schema itself (`clean_code`) is created by the first
-- migration; this file only enables the extension that the schema
-- depends on.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
