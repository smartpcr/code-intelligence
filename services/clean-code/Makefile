# Makefile for the clean-code Go service.
#
# Goals (per implementation-plan.md Stage 1.1):
#   * `make lint`         -- golangci-lint with zero findings on the empty tree.
#   * `make test`         -- `go test ./...` succeeds on the empty tree.
#   * `make build`        -- `go build ./...` succeeds; produces `clean-coded` binary.
#   * `make proto`        -- regenerate Go gRPC bindings from any `.proto` files
#                            under `proto/`. No-op when none exist yet (Stage 1.1).
#   * `make migrate-up`   -- apply every `migrations/*.up.sql` against $CLEAN_CODE_PG_URL,
#                            in lexical order. No-op when none exist yet (Stage 1.1).
#   * `make migrate-down` -- apply every `migrations/*.down.sql` in REVERSE lexical
#                            order against $CLEAN_CODE_PG_URL. No-op when none exist.
#   * `make docker-build` -- build the `clean-coded` container image.
#   * `make compose-up`   -- bring up the local dev stack (postgres + prometheus + otel + clean-coded).
#   * `make compose-down` -- tear down the local dev stack with volumes.
#
# All targets are invoked by CI on every PR opened against this
# story's workstream branches; see .github/workflows/clean-code-ci.yml.

# Pin tooling versions here so local dev and CI agree byte-for-byte.
GO              ?= go
GOLANGCI_LINT   ?= golangci-lint
PROTOC          ?= protoc
PSQL            ?= psql
DOCKER          ?= docker
# Pinned versions of the protoc Go plugins; matches agent-memory
# Stage 1.1 precedent so CI can reuse the same recipe shape.
PROTOC_GEN_GO_VERSION      ?= v1.36.11
PROTOC_GEN_GO_GRPC_VERSION ?= v1.5.1
GOFLAGS         ?=
# Default test flags stay portable: `-race` requires CGO which is
# off on stock Windows toolchains. CI overrides this to add `-race`
# on Linux runners where gcc is present.
TEST_FLAGS      ?= -count=1
PKGS            ?= ./...

# `make build` deposits the service binary under bin/ for predictable
# packaging by `make docker-build`. The `clean-coded` name matches the
# `cmd/clean-coded/` package and is the binary referenced in the
# container Dockerfile + docker-compose service definition.
BIN_DIR         ?= bin
CLEAN_CODED_BIN ?= $(BIN_DIR)/clean-coded
CLEAN_CODED_PKG ?= ./cmd/clean-coded

# Container image coordinates -- override via env in CI when pushing
# to a registry. Local dev uses the `:dev` tag.
DOCKER_IMAGE    ?= clean-code/clean-coded
DOCKER_TAG      ?= dev

# Migration inputs -- the SQL files arrive in Stage 1.2 and later.
# Stage 1.1 starts with an empty migrations/ directory so these
# globs expand to nothing and the for-loop is a no-op.
MIGRATIONS_DIR  ?= migrations
# CLEAN_CODE_PG_URL is the canonical migration / bootstrap DSN per
# docs/stories/code-intelligence-CLEAN-CODE/e2e-scenarios.md
# (lines 36, 45, 66, 77, 484). The e2e harness, the migrator
# container, and the Stage 1+ CI bootstrap all export this env
# var directly so `make migrate-up` works with zero translation.
# The local-dev default falls through to the docker-compose
# postgres credentials in deploy/local/docker-compose.yml.
CLEAN_CODE_PG_URL ?= postgres://clean_code:clean_code@localhost:5432/clean_code?sslmode=disable

# Inputs to `make proto` -- no proto IDL today; future stages add
# the Agent / Evaluator surfaces.
PROTO_DIR       ?= proto
PROTO_FILES     := $(wildcard $(PROTO_DIR)/*.proto)

# Local-stack compose file location -- mirrors the agent-memory layout.
COMPOSE_FILE    ?= deploy/local/docker-compose.yml

.PHONY: all build test test-race lint vet tidy clean help proto proto-tools \
        migrate-up migrate-down docker-build compose-up compose-down

all: lint build test

## build: compile every Go package in the service tree and emit the clean-coded binary.
build:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -o $(CLEAN_CODED_BIN) $(CLEAN_CODED_PKG)
	$(GO) build $(GOFLAGS) $(PKGS)

## test: run the full Go test suite.
test:
	$(GO) test $(GOFLAGS) $(TEST_FLAGS) $(PKGS)

## test-race: run the full Go test suite with the race detector
## (requires CGO; CI invokes this on Linux runners).
test-race:
	CGO_ENABLED=1 $(GO) test $(GOFLAGS) -race $(TEST_FLAGS) $(PKGS)

## lint: run golangci-lint over every Go package; fails on any finding.
lint:
	$(GOLANGCI_LINT) run $(PKGS)

## vet: shorthand for `go vet` -- useful in local dev, not required by CI.
vet:
	$(GO) vet $(PKGS)

## tidy: refresh go.mod / go.sum without network access.
tidy:
	$(GO) mod tidy

## clean: remove Go build artifacts.
clean:
	$(GO) clean -cache -testcache
	rm -rf $(BIN_DIR)

## help: list available targets.
help:
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## proto: regenerate the Go gRPC bindings from any `.proto` file under `proto/`.
## Stage 1.1 ships with no `.proto` IDL; the target is a no-op on the empty tree
## so CI's `make proto` step exits 0. Future stages add the Agent + Evaluator
## surfaces and this target wires the protoc invocation.
proto:
	@if [ -z "$(PROTO_FILES)" ]; then \
		echo "make proto: no .proto files under $(PROTO_DIR)/; skipping"; \
	else \
		$(PROTOC) \
			-I $(PROTO_DIR) \
			--go_out=$(PROTO_DIR) \
			--go_opt=paths=source_relative \
			--go-grpc_out=$(PROTO_DIR) \
			--go-grpc_opt=paths=source_relative \
			$(PROTO_FILES); \
	fi

## proto-tools: install pinned protoc-gen-go and protoc-gen-go-grpc
## binaries into `$(GOBIN)` (or `$(GOPATH)/bin`). Run once on a
## fresh dev machine, then `make proto` works against the pinned
## plugin versions.
proto-tools:
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

## migrate-up: apply every migrations/*.up.sql against $(CLEAN_CODE_PG_URL), in
## lexical order. Stage 1.1 ships an empty migrations/ directory so the
## for-loop iterates zero files and the target is a no-op on the empty
## tree. Stage 1.2 adds the Catalog/Lifecycle DDL; later stages add the
## Measurement, Policy, Audit, Refactor, and Roles migrations.
migrate-up:
	@set -e; \
	files=$$(ls $(MIGRATIONS_DIR)/*.up.sql 2>/dev/null | sort); \
	if [ -z "$$files" ]; then \
		echo "make migrate-up: no *.up.sql files under $(MIGRATIONS_DIR)/; skipping"; \
	else \
		for f in $$files; do \
			echo "applying $$f"; \
			$(PSQL) "$(CLEAN_CODE_PG_URL)" -v ON_ERROR_STOP=1 -f "$$f"; \
		done; \
	fi

## migrate-down: apply every migrations/*.down.sql against $(CLEAN_CODE_PG_URL),
## in REVERSE lexical order. No-op on the empty tree.
migrate-down:
	@set -e; \
	files=$$(ls $(MIGRATIONS_DIR)/*.down.sql 2>/dev/null | sort -r); \
	if [ -z "$$files" ]; then \
		echo "make migrate-down: no *.down.sql files under $(MIGRATIONS_DIR)/; skipping"; \
	else \
		for f in $$files; do \
			echo "reverting $$f"; \
			$(PSQL) "$(CLEAN_CODE_PG_URL)" -v ON_ERROR_STOP=1 -f "$$f"; \
		done; \
	fi

## docker-build: build the clean-coded container image.
## The image bundles the `clean-coded` binary plus its `/healthz` and
## `/readyz` listener. Tag with `DOCKER_TAG=...` to push to a registry.
docker-build:
	$(DOCKER) build \
		-f Dockerfile \
		-t $(DOCKER_IMAGE):$(DOCKER_TAG) \
		.

## compose-up: bring up the local dev stack (postgres + prometheus + otel + clean-coded).
compose-up:
	$(DOCKER) compose -f $(COMPOSE_FILE) up -d --build

## compose-down: tear down the local dev stack and drop named volumes.
compose-down:
	$(DOCKER) compose -f $(COMPOSE_FILE) down -v
