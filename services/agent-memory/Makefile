# Makefile for the agent-memory Go service.
#
# Goals (per implementation-plan.md Stage 1.1):
#   * `make lint`  -- golangci-lint with zero findings on the empty tree.
#   * `make test`  -- `go test ./...` succeeds on the empty tree.
#   * `make build` -- `go build ./...` succeeds on the empty tree.
#
# All three targets are invoked by CI on every PR opened against this
# story's workstream branches; see .github/workflows/agent-memory-ci.yml.

# Pin tooling versions here so local dev and CI agree byte-for-byte.
GO              ?= go
GOLANGCI_LINT   ?= golangci-lint
GOFLAGS         ?=
# Default test flags stay portable: `-race` requires CGO which is
# off on stock Windows toolchains. CI overrides this to add `-race`
# (see .github/workflows/agent-memory-ci.yml) where gcc is present.
TEST_FLAGS      ?= -count=1
PKGS            ?= ./...

.PHONY: all build test test-race lint vet tidy clean help

all: lint build test

## build: compile every Go package in the service tree.
build:
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

## help: list available targets.
help:
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
