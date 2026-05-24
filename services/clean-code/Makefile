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

# ---------------------------------------------------------------------------
# Go service targets
#
# These targets are consumed by `.github/workflows/clean-code-ci.yml`
# (`make lint`, `make build`, `make test`, `make test-race`) and by
# developer workflows that pre-date the schema-migrations refactor
# (`make vet`, `make tidy`, `make clean`, `make proto`, `make docker-build`,
# `make compose-up`, `make compose-down`). They were inadvertently dropped
# when this Makefile was trimmed to schema-only targets; restoring them
# keeps the CI workflow green.
# ---------------------------------------------------------------------------

BIN_DIR             := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))bin
CLEAN_CODED         := $(BIN_DIR)/clean-coded

GO                  ?= go
DOCKER              ?= docker
COMPOSE             ?= docker compose
GOLANGCI_LINT       ?= golangci-lint

GO_PKGS             ?= ./...
DOCKER_IMAGE        ?= clean-code/clean-coded:dev
CLEAN_CODE_MAIN_PKG ?= ./cmd/clean-coded

.PHONY: build test test-race lint vet tidy clean proto docker-build compose-up compose-down

build:
	$(GO) build -o $(CLEAN_CODED) $(CLEAN_CODE_MAIN_PKG)

test:
	$(GO) test $(GO_PKGS)

test-race:
	$(GO) test -race $(GO_PKGS)

lint:
	$(GOLANGCI_LINT) run $(GO_PKGS)

vet:
	$(GO) vet $(GO_PKGS)

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)

# `proto` is a placeholder. The clean-code service does not ship any
# .proto definitions yet; the target is preserved so CI and developer
# scripts that invoke `make proto` continue to work without error.
proto:
	@echo "no .proto files in services/clean-code yet; nothing to generate"

docker-build:
	$(DOCKER) build -t $(DOCKER_IMAGE) .

compose-up:
	$(COMPOSE) up -d

compose-down:
	$(COMPOSE) down
