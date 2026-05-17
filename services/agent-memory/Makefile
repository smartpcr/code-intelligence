# Makefile for the agent-memory Go service.
#
# Goals (per implementation-plan.md Stage 1.1 / Stage 5.x):
#   * `make lint`  -- golangci-lint with zero findings on the empty tree.
#   * `make test`  -- `go test ./...` succeeds on the empty tree.
#   * `make build` -- `go build ./...` succeeds on the empty tree.
#   * `make proto` -- regenerate `proto/agent/*.pb.go` from
#                     `proto/agent.proto`; required after any IDL edit
#                     so the checked-in gRPC bindings stay in sync.
#
# All four targets are invoked by CI on every PR opened against this
# story's workstream branches; see .github/workflows/agent-memory-ci.yml.

# Pin tooling versions here so local dev and CI agree byte-for-byte.
GO              ?= go
GOLANGCI_LINT   ?= golangci-lint
PROTOC          ?= protoc
# Pinned versions of the protoc Go plugins; the generated `.pb.go`
# files carry these strings as their `// versions:` header so a
# regen with mismatching plugins produces a noisy diff that's easy
# to spot in review. See `make proto`.
PROTOC_GEN_GO_VERSION      ?= v1.36.11
PROTOC_GEN_GO_GRPC_VERSION ?= v1.5.1
GOFLAGS         ?=
# Default test flags stay portable: `-race` requires CGO which is
# off on stock Windows toolchains. CI overrides this to add `-race`
# (see .github/workflows/agent-memory-ci.yml) where gcc is present.
TEST_FLAGS      ?= -count=1
PKGS            ?= ./...

# Inputs to `make proto` -- single proto IDL today; add more here
# as the Agent Surface grows.
PROTO_DIR       ?= proto
PROTO_PKG_DIR   ?= proto/agent
PROTO_FILES     ?= agent.proto

.PHONY: all build test test-race lint vet tidy clean help proto proto-tools

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
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## proto: regenerate the Go gRPC bindings from `proto/agent.proto`.
## Output lands at `proto/agent/agent.pb.go` and
## `proto/agent/agent_grpc.pb.go`. The package directory under
## `proto/agent/` is intentional -- the `.proto` source lives at
## `proto/agent.proto` (one IDL file at the top of the proto tree)
## while the generated Go package `agentpb` lives one level deeper
## so the `go_package` import path stays stable. The `protoc -I`
## flag is set to `proto/`, NOT `.`, so the descriptor's source
## name is `agent.proto` rather than `proto/agent.proto`; this
## keeps the generated `file_agent_proto_*` symbols aligned with
## the package directory name. Run after editing the .proto IDL.
proto:
	$(PROTOC) \
		-I $(PROTO_DIR) \
		--go_out=$(PROTO_PKG_DIR) \
		--go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_PKG_DIR) \
		--go-grpc_opt=paths=source_relative \
		$(PROTO_FILES)

## proto-tools: install pinned protoc-gen-go and protoc-gen-go-grpc
## binaries into `$(GOBIN)` (or `$(GOPATH)/bin`). Run once on a
## fresh dev machine, then `make proto` works against the pinned
## plugin versions.
proto-tools:
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
