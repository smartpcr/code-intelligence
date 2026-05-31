# Makefile for clean_code schema migrations
# Requires CLEAN_CODE_PG_URL environment variable pointing to a PostgreSQL instance.
# Example: export CLEAN_CODE_PG_URL="postgres://user:pass@localhost:5432/cleancode?sslmode=disable"

MIGRATIONS_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))migrations

# ---------------------------------------------------------------------------
# Go service targets
#
# These targets are consumed by `.github/workflows/clean-code-ci.yml`
# (`make lint`, `make build`, `make test`, `make test-race`) and by
# developer workflows that pre-date the schema-migrations refactor
# (`make vet`, `make tidy`, `make clean`, `make proto`, `make docker-build`,
# `make compose-up`, `make compose-down`).
# ---------------------------------------------------------------------------

BIN_DIR             := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))bin

GO                  ?= go
DOCKER              ?= docker
COMPOSE             ?= docker compose
GOLANGCI_LINT       ?= golangci-lint
PROTOC              ?= protoc

# Pinned protoc-gen-go version. The generated `.pb.go` carries
# this string as its `// versions:` header so a regen with a
# mismatching plugin produces a noisy diff that is easy to spot
# in review. Mirrors `services/agent-memory/Makefile`.
PROTOC_GEN_GO_VERSION ?= v1.36.11

GO_PKGS             ?= ./...
TEST_FLAGS          ?= -count=1
DOCKER_IMAGE        ?= clean-code/clean-coded:dev

# CMD_DIRS lists every `cmd/*` package that produces a binary.
# Iterating over this list is how `make build` emits a binary
# per service (the legacy single-binary layout that built a
# `clean-coded` binary from `./cmd/clean-coded` was removed in
# #89; the service is now a fleet of focused entry points).
CMD_DIRS            := $(notdir $(patsubst %/,%,$(dir $(wildcard cmd/*/main.go))))

# Inputs to `make proto`. Stage 2.1 ships exactly one .proto file
# (`proto/ast/v1/ast.proto`); add new IDLs here as the canonical
# shape grows. The plugin is invoked with
# `--go_opt=module=$(PROTO_GO_MODULE)` so generated files land at
# the import path the source-tree expects -- specifically
# `internal/ast/v1/ast.pb.go` for the AST v1 proto.
PROTO_GO_MODULE     ?= github.com/smartpcr/code-intelligence/services/clean-code
PROTO_DIR           ?= proto
PROTO_OUT_DIR       ?= .
PROTO_FILES         ?= ast/v1/ast.proto

# ---------------------------------------------------------------------------
# Phase-09 audit-WAL E2E targets
# ---------------------------------------------------------------------------

PG_URL ?= postgres://clean_code_admin:e2e_test_password@localhost:5432/clean_code?sslmode=disable
COMPOSE_FILE ?= ../../tests/e2e/phase-09-audit-wal/docker-compose.yml

.PHONY: build build-prod test test-prod test-cgo test-nocgo test-race lint lint-cli vet tidy clean proto proto-tools docker-build compose-up compose-down update-cli-golden
.PHONY: migrate-up migrate-down seed-phase-09 test-phase-09
.PHONY: test-e2e

build:
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMD_DIRS); do \
	    echo "$(GO) build -o $(BIN_DIR)/$$cmd ./cmd/$$cmd"; \
	    $(GO) build -o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit $$?; \
	done

## build-prod: compile every `cmd/` binary with `-tags prod`,
## then emit the spec-pinned `bin/cleanc-prod` alias for the
## `cmd/cleanc` entrypoint.
##
## The prod build tag flips `flags.DefaultDevMode` to false and
## activates the `prodSentinelLoader` in `internal/cli/devpolicy`
## so that unsigned rule-pack bypass is excluded at compile time
## (architecture.md Sec 1.6, "policy-signing-required"; tech-spec
## Sec 8.9, "Build-tag matrix"). Referenced by USAGE.md §2.1 and
## by the `build-prod` job in `.github/workflows/clean-code-ci.yml`
## (implementation-plan.md Stage 1.4 and Stage 5.1
## "Build Tag Matrix"). The Stage 5.1 workstream brief pins the
## artifact name `bin/cleanc-prod`; the explicit second invocation
## below produces exactly that path so downstream tooling
## (release packaging, docker contexts) can locate the prod
## CLI artifact at the spec-literal location while the loop
## above keeps every other `cmd/*` package honest under
## `//go:build prod`.
build-prod:
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMD_DIRS); do \
	    echo "$(GO) build -tags prod -o $(BIN_DIR)/$$cmd ./cmd/$$cmd"; \
	    $(GO) build -tags prod -o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit $$?; \
	done
	@echo "$(GO) build -tags prod -o $(BIN_DIR)/cleanc-prod ./cmd/cleanc"
	@$(GO) build -tags prod -o $(BIN_DIR)/cleanc-prod ./cmd/cleanc

## test-prod: run the prod-gated unit tests. Only the test
## functions that carry `//go:build prod` are exercised; the
## dev-mode tests in `cmd/cleanc` (e.g. TestAnalyzeHappyPath*)
## assume `flags.DefaultDevMode == true` and fail correctly
## under `-tags prod`, so they are excluded via `-run`.
##
## Packages covered:
##   - cmd/cleanc          (TestBuildTagIsProd*, TestFlagsDefault*)
##   - internal/cli/devpolicy (TestProdBuildExcludesDevBypass,
##                             TestLoadUnsignedBundle_Prod*,
##                             TestProdNewLoader*)
##   - internal/cli/flags  (TestFlagsDefaultDevMode*)
##
## Referenced by `implementation-plan.md` Stage 5.1 acceptance
## scenario "prod tests pass" and the CI matrix entry on
## `ubuntu-latest`.
test-prod:
	$(GO) test -tags prod $(TEST_FLAGS) \
		-run 'TestProdBuildExcludesDevBypass|TestLoadUnsignedBundle_Prod|TestBuildTagIsProd|TestFlagsDefaultDevMode|TestProdNewLoader' \
		./cmd/cleanc/... \
		./internal/cli/devpolicy/... \
		./internal/cli/flags/...

## test: run the full Go test suite. CI overrides via
## `test-nocgo` / `test-cgo` for explicit CGO mode pinning.
test:
	$(GO) test $(TEST_FLAGS) $(GO_PKGS)

update-cli-golden:
	UPDATE=1 $(GO) test $(TEST_FLAGS) -run TestAnalyzeGolden ./internal/cli/orchestrator/...

## test-nocgo: exercise the `//go:build !cgo` lexer-fallback
## adapters for Python / TypeScript / Java (and stdlib parser
## for Go). MUST be runnable on hosts without a C compiler --
## this is the fallback contract the !cgo build tag exists for.
test-nocgo:
	CGO_ENABLED=0 $(GO) test $(TEST_FLAGS) $(GO_PKGS)

## test-cgo: exercise the `//go:build cgo` tree-sitter-backed
## adapters. Requires a working C toolchain (`gcc` / `cc` /
## `clang`). CI runs this target as a NON-OPTIONAL step on
## Linux runners (ubuntu-latest ships gcc by default) -- the
## explicit step ensures the supported tree-sitter path is
## ALWAYS validated, never silently skipped.
test-cgo:
	CGO_ENABLED=1 $(GO) test $(TEST_FLAGS) $(GO_PKGS)

test-race:
	$(GO) test -race $(GO_PKGS)

## lint: run golangci-lint over every Go package. Depends on
## `lint-cli` so the two custom lint rules required by
## tech-spec REFACTOR-GUIDE Sec 8.10 (no-production-sql-import
## + no-production-build-tag-bypass) ALWAYS run as part of a
## standard `make lint` developer workflow.
lint: lint-cli
	$(GOLANGCI_LINT) run $(GO_PKGS)

## lint-cli: run the two CLI-specific custom lint rules
## end-to-end against the real `cmd/cleanc/...` and
## `internal/cli/...` source trees (tech-spec
## REFACTOR-GUIDE Sec 8.10).
##
## Step 1 -- build the custom `go vet` analyser binary.
## The binary multiplexes two `go/analysis` analyzers
## (`nobuildtagbypass`, `nocliproductionsqlimport`) under
## `multichecker.Main` so a single `go vet` invocation
## exercises BOTH rules. See `tools/buildtaglint/README.md`
## for the rule reference.
##
## Step 2 -- invoke `go vet -vettool=<binary>` over the
## CLI source trees. A violation in either analyzer prints
## `file:line:col: <rule-name>: <message>` to stderr (the
## rule name is part of the diagnostic message so `grep`
## for "no-production-sql-import" or
## "no-production-build-tag-bypass" surfaces the relevant
## hit) and produces a non-zero exit status that aborts
## the build.
##
## Step 3 -- run the forbidigo half of the SQL-import
## rule via golangci-lint. The vettool's import-list walker
## catches blank imports that forbidigo cannot see (per
## evaluator iter 1, item #2); golangci-lint's forbidigo
## rule catches symbol-use violations under the same name
## and supplies the canonical `golangci-lint`-style output
## that CI dashboards already parse. Both layers run on
## every PR so a regression on either path fails fast.
lint-cli:
	@echo "==> buildtaglint: building vettool"
	@mkdir -p $(BIN_DIR)
	@$(GO) build -o $(BIN_DIR)/buildtaglint ./tools/buildtaglint
	@echo "==> go vet -vettool=buildtaglint ./cmd/cleanc/... ./internal/cli/..."
	$(GO) vet -vettool=$(BIN_DIR)/buildtaglint ./cmd/cleanc/... ./internal/cli/...
	@echo "==> golangci-lint forbidigo (no-production-sql-import) on cmd/cleanc + internal/cli"
	$(GOLANGCI_LINT) run --enable-only forbidigo ./cmd/cleanc/... ./internal/cli/...

vet:
	$(GO) vet $(GO_PKGS)

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)

## proto: regenerate the Go bindings from every .proto under
## `proto/`. Output lands at the path the `go_package` option
## resolves to under the module root (e.g. `proto/ast/v1/ast.proto`
## with `go_package = ".../internal/ast/v1;astv1"` writes
## `internal/ast/v1/ast.pb.go`). The `--go_opt=module=` flag
## strips the module prefix from every `go_package` so the
## remaining suffix is the on-disk write target. Run after
## editing any .proto IDL; commit the regenerated `.pb.go`.
proto:
	$(PROTOC) \
		-I $(PROTO_DIR) \
		--go_out=$(PROTO_OUT_DIR) \
		--go_opt=module=$(PROTO_GO_MODULE) \
		$(PROTO_FILES)

## proto-tools: install pinned protoc-gen-go into `$(GOBIN)` /
## `$(GOPATH)/bin`. Run once on a fresh dev machine so `make
## proto` finds the plugin.
proto-tools:
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

docker-build:
	$(DOCKER) build -t $(DOCKER_IMAGE) .

compose-up:
	$(COMPOSE) up -d

compose-down:
	$(COMPOSE) down

# ---------------------------------------------------------------------------
# Schema migration targets
# ---------------------------------------------------------------------------

migrate-up:
	@echo "Running migrations against $${CLEAN_CODE_PG_URL:-$(PG_URL)}"
	psql "$${CLEAN_CODE_PG_URL:-$(PG_URL)}" -v ON_ERROR_STOP=1 -f migrations/001_create_audit_tables.sql
	psql "$${CLEAN_CODE_PG_URL:-$(PG_URL)}" -v ON_ERROR_STOP=1 -f migrations/002_create_wal_inbox.sql
	psql "$${CLEAN_CODE_PG_URL:-$(PG_URL)}" -v ON_ERROR_STOP=1 -f migrations/003_create_roles.sql
	@echo "Migrations complete."

migrate-down:
	@test -n "$(CLEAN_CODE_PG_URL)" || (echo "ERROR: CLEAN_CODE_PG_URL is not set" && exit 1)
	psql "$(CLEAN_CODE_PG_URL)" -f "$(MIGRATIONS_DIR)/001_down.sql"

seed-phase-09:
	@echo "Seeding phase-09 baseline data"
	psql "$${CLEAN_CODE_PG_URL:-$(PG_URL)}" -v ON_ERROR_STOP=1 -f seeds/phase-09-baseline.sql
	@echo "Seeding complete."

test-phase-09:
	go test -tags e2e -v -count=1 -timeout 120s ./test/e2e/code-intelligence-CLEAN-CODE/...

# ---------------------------------------------------------------------------
# CLI Binary Skeleton E2E targets (Stage 1.1)
# ---------------------------------------------------------------------------

# test-e2e runs the E2E acceptance tests (behind the `e2e` build tag).
# Pre-requisite: `make build` (wired as a dependency).
test-e2e: build
	go test -tags e2e ./test/e2e/... -count=1