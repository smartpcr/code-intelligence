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
# AST_PKGS is the narrow package set exercised by `test-cgo` /
# `test-nocgo`. Per REPO-SCANNER implementation-plan Stage 1.1,
# the CGO-mode validation gate is scoped to the parser
# dispatcher subtree -- the only code path that branches on
# build tags (`parsers_cgo.go` vs `parsers_nocgo.go`). The full
# suite is already covered by `make test` / `make test-race`.
AST_PKGS        ?= ./internal/repoindexer/ast/...

# Inputs to `make proto` -- single proto IDL today; add more here
# as the Agent Surface grows.
PROTO_DIR       ?= proto
PROTO_PKG_DIR   ?= proto/agent
PROTO_FILES     ?= agent.proto

.PHONY: all build test test-race test-cgo test-nocgo test-cgo-rust lint vet tidy clean help proto proto-tools loadtest-calibration loadtest-calibration-smoke loadtest-gen-artifact

all: lint build test

## build: compile every Go package in the service tree.
build:
	$(GO) build $(GOFLAGS) $(PKGS)

## test: run the full Go test suite.
test:
	$(GO) test $(GOFLAGS) $(TEST_FLAGS) $(PKGS)

## test-nocgo: exercise the AST parser dispatcher subtree with
## CGO explicitly OFF. Validates the `//go:build !cgo`
## registration in `parsers_nocgo.go` (PowerShell only) and the
## scanner / stdlib-only fallback parsers
## (`parser_typescript.go`, `parser_python.go`) plus the
## `ast.dispatch.skip{reason="no_parser"}` branch for
## CGO-only languages (`.c .cpp .cs .go .rs`). Prints
## `go env CGO_ENABLED CC CXX` first so the CI log records the
## active toolchain (REPO-SCANNER impl-plan Stage 1.1 scenario
## `cgo-flag-printed`). Scoped to `$(AST_PKGS)` because the
## parser-registration split is the only code path that
## branches on `//go:build cgo`; the broader suite is covered
## by `make test`. Documented as the canonical scanner-path
## validation gate in `.claude/context/tests.md`.
test-nocgo:
	@echo "==> test-nocgo: active Go toolchain (CGO_ENABLED=0)"
	@CGO_ENABLED=0 $(GO) env CGO_ENABLED CC CXX
	CGO_ENABLED=0 $(GO) test $(GOFLAGS) $(TEST_FLAGS) $(AST_PKGS)

## test-cgo: exercise the AST parser dispatcher subtree with
## CGO explicitly ON so the tree-sitter-backed parsers
## (`parser_treesitter_{c,cpp,csharp,go,rust}.go`,
## `parser_treesitter.go` for TS / Python) are compiled and
## exercised against the smacker `go-tree-sitter` grammars.
## Validates the `//go:build cgo` registration in
## `parsers_cgo.go` (six parsers). Requires a C compiler on
## PATH (`gcc` / `clang` on Linux/macOS; a MinGW-W64 /
## TDM-GCC install on Windows -- see the "Building with CGO"
## subsection of services/agent-memory/README.md). Prints
## `go env CGO_ENABLED CC CXX` first so the CI log records the
## active toolchain (REPO-SCANNER impl-plan Stage 1.1 scenario
## `cgo-flag-printed`). Scoped to `$(AST_PKGS)` for the same
## reason as `test-nocgo`. Documented as the canonical
## tree-sitter validation gate in `.claude/context/tests.md`.
test-cgo:
	@echo "==> test-cgo: active Go toolchain (CGO_ENABLED=1)"
	@CGO_ENABLED=1 $(GO) env CGO_ENABLED CC CXX
	CGO_ENABLED=1 $(GO) test $(GOFLAGS) $(TEST_FLAGS) $(AST_PKGS)

## test-race: run the full Go test suite with the race detector
## (requires CGO; CI invokes this on Linux runners).
test-race:
	CGO_ENABLED=1 $(GO) test $(GOFLAGS) -race $(TEST_FLAGS) $(PKGS)

## test-cgo-rust: exercise the Rust tree-sitter parser's CGO-gated
## tests directly (the smacker rust binding links against a
## C-compiled grammar). The default `make test` path runs with
## CGO=0 and silently skips these files; this target is the
## canonical local exerciser for the Rust parser when a C
## toolchain (gcc on Linux/macOS, mingw/clang on Windows) is on
## PATH. The non-CGO contract test
## (`parser_treesitter_rust_contract_test.go`) still runs under
## `make test` to guard the same invariants symbolically.
test-cgo-rust:
	CGO_ENABLED=1 $(GO) test $(GOFLAGS) $(TEST_FLAGS) -run Rust ./internal/repoindexer/ast/...

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

## loadtest-calibration: drive the §8.3 envelope against a live
## agent-memory stack and write the calibration artifact to
## `docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md`.
## Operator-facing wrapper for `cmd/loadtest-harness`. See
## implementation-plan.md Stage 8.4 for the operator workflow
## (seeded 200 k LOC fixture, 30-minute run, ≤ 1 % error budget).
##
## Override AGENT_TARGET / MGMT_TARGET / REPO_ID / SEEDED_LOC /
## LABELED_QUERIES / PROVENANCE for the deploy/local stack; the
## defaults assume a localhost insecure deploy/local bring-up.
##
## LABELED_QUERIES is the path to a JSON array of
## `{query, expected_node_id, expected_concept_ids[]}` entries
## (see ../../docs/stories/code-intelligence-AGENT-MEMORY/labeled-queries.sample.json
## for the on-disk shape). Supplying it populates the artifact's
## "learning-quality" table with numeric
## `rank_of_correct_node_at_k20` and `concept_hit_fraction_at_k20`
## values; omitting it leaves those rows as `n/a` (per Stage 8.4,
## learning-quality is a labelled-query proxy and the operator
## decides which fixture to point at for a given calibration run).
##
## PROVENANCE is the operator-supplied banner the harness stamps
## onto the artifact's top-of-file callout so a reviewer can
## immediately see whether the numbers came from the deploy/local
## stack or an in-process baseline. Default value below labels
## this make-driven run as a §8.3 production-seal candidate;
## override when calibrating against a non-canonical stack.
##
## REPO_ID MUST be UUID-formatted: the mgmt-api repo_id contract
## (`internal/mgmtapi.handler.reUUID`) rejects non-UUIDs, and the
## harness's mgmt scenario forwards REPO_ID into the
## `X-Mgmt-Repo-ID` header + `mgmt.repo_id` resource attribute.
##
## Path normalisation for `ARTIFACT` and `LABELED_QUERIES`:
## When the operator invokes `make -C services/agent-memory
## loadtest-calibration ...` from the repo root, GNU make
## rewrites the cwd to `services/agent-memory/` BEFORE running
## the recipe. A relative path supplied on the command line
## (e.g. `LABELED_QUERIES=docs/stories/.../labeled-queries.sample.json`)
## would resolve against the post-`-C` cwd
## (`services/agent-memory/`) and silently miss the file. The
## `resolve_path` function below tries the path as-supplied
## first (against `$(CURDIR)`), then re-tries with the
## `../../` prefix that maps `$(CURDIR)/../../` back onto the
## repo root, and returns the first match. Absolute paths are
## passed through unchanged. This lets the operator type the
## repo-rooted path that's natural from a repo-root prompt AND
## the `../../`-prefixed path that's natural from a
## `services/agent-memory/` prompt without either form failing.
AGENT_TARGET     ?= localhost:8443
MGMT_TARGET      ?= http://localhost:8444
REPO_ID          ?= ca11ca11-0000-4000-8000-000000000001
SEEDED_LOC       ?= 200000
LABELED_QUERIES  ?=
ARTIFACT         ?= ../../docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md
PROVENANCE       ?= DEPLOY/LOCAL STACK NOMINAL CALIBRATION — make loadtest-calibration against $(AGENT_TARGET) / $(MGMT_TARGET) (seeded $(SEEDED_LOC) LOC fixture). §8.3 production-seal candidate.

# resolve_path(p) -> p if absolute; else first of {p, ../../p}
# that exists. Falls back to p so the harness can emit its own
# `file not found` error rather than the Makefile swallowing it.
resolve_path = $(strip $(if $(filter /%,$(1)),$(1),\
    $(if $(wildcard $(1)),$(1),\
    $(if $(wildcard ../../$(1)),../../$(1),$(1)))))

loadtest-calibration:
	$(GO) run ./cmd/loadtest-harness \
	    --agent-target $(AGENT_TARGET) \
	    --mgmt-target  $(MGMT_TARGET) \
	    --repo-id      $(REPO_ID) \
	    --seeded-loc   $(SEEDED_LOC) \
	    --artifact     $(call resolve_path,$(ARTIFACT)) \
	    $(if $(strip $(LABELED_QUERIES)),--labeled-queries $(call resolve_path,$(LABELED_QUERIES))) \
	    --provenance   "$(PROVENANCE)" \
	    --no-tls

## loadtest-calibration-smoke: in-process smoke that proves the
## harness wiring (flag parsing → artifact write → exit code)
## without requiring a live agent-memory stack. Runs the
## end-to-end test which starts an httptest server for the
## mgmt-api surface and exercises the full code path under one
## second.
loadtest-calibration-smoke:
	$(GO) test $(TEST_FLAGS) -run TestRun_EndToEnd_SmokeArtifactWritten -v ./cmd/loadtest-harness

## loadtest-gen-artifact: regenerate the persisted Stage 8.4
## calibration artifact (`docs/stories/.../load-test-iter1.md`)
## using the IN-PROCESS gen_artifact.go helper — useful when no
## deploy/local stack is available (PR CI, fast iteration).
##
## This target is the canonical entry point for the in-process
## regeneration because `go run` requires `go.mod` in the
## current directory; the Makefile pins cwd to
## `services/agent-memory/` via `make -C`, so the operator can
## invoke this target from ANYWHERE in the worktree without
## needing to `cd` first:
##
##     make -C services/agent-memory loadtest-gen-artifact DURATION=30m
##
## Defaults to a 30-minute window to match the §8.3 envelope so
## the committed artifact's `planned_duration` field is the
## production-seal duration. Override DURATION for fast local
## iteration (e.g. `DURATION=2m`) but do NOT commit a sub-30m
## artifact — the §8.3 acceptance criterion is the 30-minute
## window.
##
## The artifact is stamped with an `IN-PROCESS STUB BASELINE`
## provenance banner by default. Override GEN_PROVENANCE to a
## different banner when running against a deploy/local stack
## via `make loadtest-calibration` instead.
.PHONY: loadtest-gen-artifact
DURATION         ?= 30m
GEN_PROVENANCE   ?=
loadtest-gen-artifact:
	$(GO) run ./cmd/loadtest-harness/gen_artifact.go \
	    --duration $(DURATION) \
	    $(if $(strip $(GEN_PROVENANCE)),--provenance "$(GEN_PROVENANCE)")

## seed-fixture-200k: DEV PREFLIGHT ONLY — drive the
## mgmt.ingest_spans surface at high throughput to seed a
## ~200 k SYNTHETIC-span fixture into the agent-memory graph
## for downstream `loadtest-calibration` runs. This target
## is the docs' quick-start **Option B** (developer preflight);
## it is NOT the §8.3 production-seal seed path.
##
## DO NOT use this target as a substitute for the production-
## seal seed (quick-start **Option A**) — the synthetic spans
## do not represent a real code corpus, and the resulting
## calibration artifact's provenance banner will correctly
## flag it as `SYNTHETIC SEED`. See
## `docs/stories/code-intelligence-AGENT-MEMORY/operator-action-items.md`
## for the tracked production-seal operator action.
##
## Architecture: the agent-memory service has no purpose-built
## corpus loader for 200 k LOC — the §8.3 production-seal
## protocol assumes the operator runs a source-of-truth ingest
## from an external code corpus (e.g. a Git repo, an IDE
## extension's exported trace, an OTel agent's emitted spans).
## In the absence of that external feed, this target uses the
## load-test harness itself in MGMT-ONLY burst mode to push
## ~200000 synthetic OTel spans through the same wire path the
## production seal will exercise. The resulting graph is a
## structural stand-in for the production fixture — it has the
## right edge/node count order of magnitude but synthetic
## payload — and the operator MUST overwrite it with a real
## corpus ingest before treating the calibration artifact as a
## production seal.
##
## Throughput math (default values):
##   SEED_BATCH_SIZE = 1000 spans/batch  (§8.3 cap)
##   nominal mgmt    = 50 batches/min    (config.go ProfileNominal)
##   spans/sec       = 1000 * 50 / 60 ≈ 833
##   spans needed    = 200000
##   duration        = 200000 / 833 ≈ 4 min, padded to 5 min for slack
##
## Override SEED_DURATION / SEED_BATCH_SIZE for a different
## span budget (e.g. SEED_DURATION=10m + SEED_BATCH_SIZE=1000
## ≈ 500 k spans for the §C14 / 500 k LOC stretch envelope).
##
## NOTE: this target uses `--skip-agent` so it only exercises
## the mgmt path; no `agent.recall` / `agent.observe` /
## `agent.expand` / `agent.summarize` traffic. The agent.*
## surface remains untouched until `make loadtest-calibration`
## runs against the seeded graph.
##
## NOTE: the harness writes a calibration ARTIFACT every run.
## For the seed-only run we redirect the artifact to a temp
## file under /tmp so the committed
## `docs/stories/.../load-test-iter1.md` is not overwritten by
## the seed step. The seed's per-verb counts can still be
## inspected at $(SEED_ARTIFACT) after the run completes.
.PHONY: seed-fixture-200k
SEED_DURATION    ?= 5m
SEED_BATCH_SIZE  ?= 1000
SEED_ARTIFACT    ?= /tmp/agent-memory-seed-fixture-200k.md
seed-fixture-200k:
	@echo "seed-fixture-200k: ~200k spans into $(MGMT_TARGET) (repo $(REPO_ID))"
	@echo "seed-fixture-200k: NOTE this is a SYNTHETIC seed (structural"
	@echo "                   stand-in for a real 200k LOC corpus); for"
	@echo "                   the production-seal artifact, replace with"
	@echo "                   an OTel-agent / repo-walker corpus ingest"
	@echo "                   before running loadtest-calibration."
	$(GO) run ./cmd/loadtest-harness \
	    --mgmt-target     $(MGMT_TARGET) \
	    --repo-id         $(REPO_ID) \
	    --duration        $(SEED_DURATION) \
	    --spans-per-batch $(SEED_BATCH_SIZE) \
	    --skip-agent \
	    --artifact        $(SEED_ARTIFACT) \
	    --provenance      "SYNTHETIC SEED (target=seed-fixture-200k) — NOT a calibration artifact; mgmt-only burst to populate the agent-memory graph for downstream loadtest-calibration runs." \
	    --no-tls
	@echo "seed-fixture-200k: complete; verify via mgmt-api"
	@echo "                   /v1/stats?repo_id=$(REPO_ID) that the"
	@echo "                   graph has the expected span count."
