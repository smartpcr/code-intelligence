.PHONY: migrate-up seed-phase-09 test-phase-09

PG_URL ?= postgres://clean_code_admin:e2e_test_password@localhost:5432/clean_code?sslmode=disable
COMPOSE_FILE ?= ../../tests/e2e/phase-09-audit-wal/docker-compose.yml

migrate-up:
	@echo "Running migrations against $${CLEAN_CODE_PG_URL:-$(PG_URL)}"
	psql "$${CLEAN_CODE_PG_URL:-$(PG_URL)}" -v ON_ERROR_STOP=1 -f migrations/001_create_audit_tables.sql
	psql "$${CLEAN_CODE_PG_URL:-$(PG_URL)}" -v ON_ERROR_STOP=1 -f migrations/002_create_wal_inbox.sql
	psql "$${CLEAN_CODE_PG_URL:-$(PG_URL)}" -v ON_ERROR_STOP=1 -f migrations/003_create_roles.sql
	@echo "Migrations complete."

seed-phase-09:
	@echo "Seeding phase-09 baseline data"
	psql "$${CLEAN_CODE_PG_URL:-$(PG_URL)}" -v ON_ERROR_STOP=1 -f seeds/phase-09-baseline.sql
	@echo "Seeding complete."

test-phase-09:
	go test -tags e2e -v -count=1 -timeout 120s ./test/e2e/code-intelligence-CLEAN-CODE/...