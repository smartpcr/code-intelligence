# PostgreSQL 16 image for the clean-code local stack.
#
# Extends the upstream `postgres:16` image with no extra packages
# in Stage 1.1 -- `pgcrypto` is bundled with the base image and is
# enabled by the init script in ./init/. Stage 1.3 adds the
# `pg_partman` package alongside the `MetricSample` partitioning
# migration.

FROM postgres:16

# Stage 1.1: no extra apt packages. `pgcrypto` is already shipped
# with the base image, and Stage 1.2's migrations supply the
# `clean_code` schema's DDL.
