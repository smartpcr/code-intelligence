# Postgres 16 image preloaded with pg_partman for the agent-memory
# service. pgcrypto is part of the contrib package that ships with
# the upstream postgres:16 image, so it does not need a separate
# install -- only a `CREATE EXTENSION` (see ../init/01-extensions.sql).
#
# pg_partman is *not* in the base image; we install it from the
# PostgreSQL Global Development Group apt repo that the official
# postgres:16 Debian image already has configured.
FROM postgres:16

ARG PARTMAN_PACKAGE=postgresql-16-partman

# `set -eux` + a single RUN keeps the image layer small and the
# failure mode obvious if PGDG rolls a breaking package.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends "${PARTMAN_PACKAGE}"; \
    rm -rf /var/lib/apt/lists/*

# pg_partman's background worker needs to be in
# shared_preload_libraries. We pass that via `command:` in
# docker-compose.yml rather than mutating postgresql.conf here so
# the image stays generic (the same image can serve multiple
# databases via different compose configs).
