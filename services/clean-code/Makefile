# Makefile for clean_code schema migrations
# Requires CLEAN_CODE_PG_URL environment variable pointing to a PostgreSQL instance.
# Example: export CLEAN_CODE_PG_URL="postgres://user:pass@localhost:5432/cleancode?sslmode=disable"

MIGRATIONS_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))migrations

.PHONY: migrate-up migrate-down

migrate-up:
	@test -n "$(CLEAN_CODE_PG_URL)" || (echo "ERROR: CLEAN_CODE_PG_URL is not set" && exit 1)
	psql "$(CLEAN_CODE_PG_URL)" -f "$(MIGRATIONS_DIR)/001_up.sql"

migrate-down:
	@test -n "$(CLEAN_CODE_PG_URL)" || (echo "ERROR: CLEAN_CODE_PG_URL is not set" && exit 1)
	psql "$(CLEAN_CODE_PG_URL)" -f "$(MIGRATIONS_DIR)/001_down.sql"