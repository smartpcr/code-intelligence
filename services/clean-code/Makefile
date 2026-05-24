# Makefile for clean_code schema migrations
# Requires CLEAN_CODE_PG_URL environment variable pointing to a PostgreSQL instance.
# Example: export CLEAN_CODE_PG_URL="postgres://user:pass@localhost:5432/cleancode?sslmode=disable"

MIGRATIONS_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))migrations

.PHONY: migrate-up migrate-down

migrate-up:
	@test -n "$(CLEAN_CODE_PG_URL)" || (echo "ERROR: CLEAN_CODE_PG_URL is not set" && exit 1)
	@files="$(sort $(wildcard $(MIGRATIONS_DIR)/*_up.sql))"; \
	if [ -z "$$files" ]; then echo "No *_up.sql migrations found in $(MIGRATIONS_DIR)"; exit 1; fi; \
	for f in $$files; do \
		echo "Applying $$f"; \
		psql "$(CLEAN_CODE_PG_URL)" -f "$$f" || exit 1; \
	done

migrate-down:
	@test -n "$(CLEAN_CODE_PG_URL)" || (echo "ERROR: CLEAN_CODE_PG_URL is not set" && exit 1)
	@files="$(sort $(wildcard $(MIGRATIONS_DIR)/*_down.sql))"; \
	if [ -z "$$files" ]; then echo "No *_down.sql migrations found in $(MIGRATIONS_DIR)"; exit 1; fi; \
	for f in $$(printf '%s\n' $$files | sort -r); do \
		echo "Reverting $$f"; \
		psql "$(CLEAN_CODE_PG_URL)" -f "$$f" || exit 1; \
	done
