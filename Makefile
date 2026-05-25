# Makefile — E2E helpers for clean-code services
# All targets expect CLEAN_CODE_PG_URL to be set (or defaulted below).

CLEAN_CODE_PG_URL ?= postgres://postgres:postgres@localhost:5432/clean_code?sslmode=disable
COMPOSE_FILE      := tests/e2e/phase-03-indexer-ingestor/docker-compose.yml

.PHONY: migrate-up seed-fixtures-phase-03 test-phase-03

## migrate-up: run SQL migrations against the database
migrate-up:
	@echo "==> Running migrations against $${CLEAN_CODE_PG_URL}"
	psql "$${CLEAN_CODE_PG_URL}" -f services/clean-code/migrations/001_init.sql

## seed-fixtures-phase-03: insert the 3-repo, 12-SHA fixture corpus
seed-fixtures-phase-03:
	@echo "==> Seeding phase-03 fixtures"
	psql "$${CLEAN_CODE_PG_URL}" -f tests/e2e/phase-03-indexer-ingestor/fixtures/seed.sql

## test-phase-03: discover compose ports, bootstrap, and run E2E tests
test-phase-03:
	@echo "==> Discovering compose-provided ports"
	$(eval PG_PORT := $(shell docker compose -f $(COMPOSE_FILE) port postgres 5432 | cut -d: -f2))
	$(eval INGESTOR_PORT := $(shell docker compose -f $(COMPOSE_FILE) port ingestor 8080 | cut -d: -f2))
	$(eval OTEL_PORT := $(shell docker compose -f $(COMPOSE_FILE) port otel-collector 4317 | cut -d: -f2))
	@echo "  postgres=$(PG_PORT) ingestor=$(INGESTOR_PORT) otel=$(OTEL_PORT)"
	CLEAN_CODE_PG_URL="postgres://postgres:postgres@localhost:$(PG_PORT)/clean_code?sslmode=disable" \
		$(MAKE) migrate-up
	CLEAN_CODE_PG_URL="postgres://postgres:postgres@localhost:$(PG_PORT)/clean_code?sslmode=disable" \
		$(MAKE) seed-fixtures-phase-03
	cd services/clean-code && \
		CLEAN_CODE_PG_URL="postgres://postgres:postgres@localhost:$(PG_PORT)/clean_code?sslmode=disable" \
		CLEAN_CODE_INGESTOR_URL="http://localhost:$(INGESTOR_PORT)" \
		CLEAN_CODE_OTEL_ENDPOINT="http://localhost:$(OTEL_PORT)" \
		go test -tags e2e -v -count=1 ./test/e2e/code-intelligence-CLEAN-CODE/... \
			-run TestE2E_repo_indexer_and_metric_ingestor_metric_ingestor_and_scanrun_state_machine