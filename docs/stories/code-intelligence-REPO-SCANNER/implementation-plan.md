---
title: "repo scanner"
storyId: "code-intelligence:REPO-SCANNER"
---

> Livedoc -- check boxes as work lands. Sibling specs:
> `architecture.md` (authoritative), `tech-spec.md`, and
> `e2e-scenarios.md` (forthcoming). Anchors / package paths
> mirror the architecture; this file owns sequencing and the
> per-stage test plan only. Phase / stage anchors are slugs of
> the heading names (no numbers).

# Phase 1: Parser coverage verification

## Dependencies
- _none -- start phase_

## Stage 1.1: CGO build proof

### Implementation Steps
- [ ] Add a `make test-cgo` target to `services/agent-memory/Makefile` that runs `CGO_ENABLED=1 go test ./internal/repoindexer/ast/...` and prints the active toolchain (`go env CGO_ENABLED CC CXX`).
- [ ] Add a `make test-nocgo` target that runs `CGO_ENABLED=0 go test ./internal/repoindexer/ast/...` so the parser-registration split (`parsers_cgo.go` vs `parsers_nocgo.go`) is exercised in CI.
- [ ] Document the C toolchain prerequisites (mingw on Windows, gcc/clang elsewhere) in `services/agent-memory/README.md` under a new "Building with CGO" subsection.
- [ ] Wire both targets into the existing CI job in `.github/workflows/agent-memory-ci.yml` so each PR runs CGO=1 and CGO=0 in parallel (the repo's actual CI entry point; no service-local `.github` directory exists).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: cgo-build-passes -- Given a host with `gcc` / `clang` on PATH, When `make test-cgo` runs from `services/agent-memory/`, Then the suite under `internal/repoindexer/ast/` passes including `parser_treesitter_{c,cpp,csharp,go,rust}_test.go`.
- [ ] Scenario: nocgo-build-passes -- Given the same checkout, When `make test-nocgo` runs, Then only the PowerShell-related tests register parsers, the suite passes, and `parsers_cgo_rust_test.go` is excluded by build tags.
- [ ] Scenario: cgo-flag-printed -- Given a `make test-cgo` invocation, When the target executes, Then the printed `go env CGO_ENABLED` line equals `1` and is captured in CI logs.

## Stage 1.2: Per-language parse smoke fixtures

### Implementation Steps
- [ ] Create `services/agent-memory/internal/repoindexer/ast/testdata/polyglot/` with one fixture file per supported language (`.ts`, `.py`, `.c`, `.cpp`, `.cs`, `.go`, `.rs`, `.ps1`) each containing a class/type, a free function, a same-file call, and one import.
- [ ] Add a table-driven test `parsers_polyglot_smoke_test.go` (CGO-tagged) that loads each fixture through `ast.Dispatcher.EmitFile` against an in-memory `NodeEdgeWriter` stub and asserts `>=1` class/type Node, `>=1` method Node, and `>=1` `static_calls` Edge for each language.
- [ ] Record the language coverage matrix produced by the smoke run in `.claude/context/tests.md` under a new "Polyglot coverage matrix" subsection.

### Dependencies
- phase-parser-coverage-verification/stage-cgo-build-proof

### Test Scenarios
- [ ] Scenario: polyglot-smoke-cgo -- Given `CGO_ENABLED=1`, When `parsers_polyglot_smoke_test.go` runs, Then each of the eight languages produces a non-empty Node + Edge set matching the assertion rules.
- [ ] Scenario: polyglot-smoke-nocgo-degraded -- Given `CGO_ENABLED=0`, When the same test runs, Then only the PowerShell fixture passes the assertion and the five tree-sitter fixtures are skipped via `t.Skip` keyed on `ErrParserUnavailable`.
- [ ] Scenario: pwsh-missing-skip -- Given a host with `pwsh` absent from PATH, When the PowerShell fixture runs, Then the dispatcher emits an `ast.dispatch.skip{reason:"pwsh_not_available"}` event and the test calls `t.Skip`.

## Stage 1.3: Degradation matrix in docs

### Implementation Steps
- [ ] Author `services/agent-memory/internal/repoindexer/ast/COVERAGE.md` summarising which extensions parse with CGO=1, with CGO=0, and which require `pwsh` on PATH; cross-reference to `parsers_cgo.go` / `parsers_nocgo.go`.
- [ ] Update `.claude/context/tests.md` to point to `COVERAGE.md` and note the CGO + `pwsh` caveats per architecture S7 / tech-spec C1, C2.

### Dependencies
- phase-parser-coverage-verification/stage-per-language-parse-smoke-fixtures

### Test Scenarios
- [ ] Scenario: coverage-doc-present -- Given a fresh clone, When `cat services/agent-memory/internal/repoindexer/ast/COVERAGE.md` runs, Then all eight language rows are present and each names its source `parser_*.go` file.
- [ ] Scenario: docs-link-resolves -- Given a fresh clone, When a Markdown link checker runs against `.claude/context/tests.md`, Then the link to `COVERAGE.md` resolves to an existing file.

# Phase 2: Identity and ancestry refactor

## Dependencies
- phase-parser-coverage-verification

## Stage 2.1: RepoIDFromURL helper

### Implementation Steps
- [ ] Add `pkg/fingerprint/repo_id.go` exporting `var namespaceRepoURL = uuid.MustParse("<fixed-uuidv4>")` (pinned constant) and `func RepoIDFromURL(url string) (RepoID, error)` computed as `uuid.NewSHA1(namespaceRepoURL, []byte(url))`.
- [ ] Add `repo_id_test.go` with vectors that pin the deterministic UUID for at least three representative URLs (`https://github.com/foo/bar`, `git@github.com:foo/bar.git`, `file:///c/code/repo`) so any future drift breaks the test.
- [ ] Document the helper in the `pkg/fingerprint/doc.go` package comment and cross-reference architecture S3.4.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: deterministic-for-same-url -- Given the same input URL, When `RepoIDFromURL` is called twice, Then the returned `RepoID` is byte-identical across both calls and across processes.
- [ ] Scenario: different-urls-diverge -- Given two different URLs differing by one character, When both are hashed, Then the returned `RepoID` values differ.
- [ ] Scenario: empty-url-rejected -- Given an empty string, When `RepoIDFromURL("")` is called, Then a non-nil error is returned and the `RepoID` is the zero value.

## Stage 2.2: Promote canonical-signature helpers

### Implementation Steps
- [ ] Move the four unexported helpers (`canonicalRepoSig`, `canonicalPackageDir`, `canonicalPackageSig`, `canonicalFileSig`) from `internal/repoindexer/worker.go` lines 1222-1259 into a new `internal/repoindexer/canonical.go` file with `CanonicalRepoSig` / `CanonicalPackageDir` / `CanonicalPackageSig` / `CanonicalFileSig` exported names.
- [ ] Rewrite the `worker.runFull` references at lines 1092, 1114, 1126, 1167 to use the exported names.
- [ ] Add `canonical_test.go` with the existing worker-test vectors restated against the exported helpers so the byte-output is pinned.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: signature-byte-stable -- Given a fixed `repoURL` and `relPath`, When the exported helpers run, Then their output strings match the previously-internal output byte-for-byte (pinned in the new test).
- [ ] Scenario: package-dir-rootfile -- Given `relPath="main.go"`, When `CanonicalPackageDir` runs, Then it returns `""` (empty string == repo root).
- [ ] Scenario: package-dir-nested -- Given `relPath="internal/foo/bar.go"`, When `CanonicalPackageDir` runs, Then it returns `"internal/foo"` with forward-slash separators.

## Stage 2.3: AncestryWriter factored from worker

### Implementation Steps
- [ ] Declare a narrow writer interface `repoindexer.RepoCommitNodeEdgeWriter` in a new `internal/repoindexer/ancestry_writer_iface.go` file with the four methods `EnsureRepo`, `EnsureCommit`, `InsertNode`, `InsertEdge`. `*graphwriter.Writer` satisfies this interface natively (no new method needed) so Phase 2 can land without depending on Phase 3 yet.
- [ ] Add `internal/repoindexer/ancestry.go` with the `AncestryWriter` type, `NewAncestryWriter(w RepoCommitNodeEdgeWriter, repoURL, sha string)`, `EnsureRepoAndCommit(ctx, defaultBranch, hints)`, `EnsureFile(ctx, WalkFile)`, and the `RepoAncestry` / `FileAncestry` value types per architecture S3.4. The constructor accepts the new `RepoCommitNodeEdgeWriter`; Stage 3.1 widens the parameter to `graphsink.Sink` (a strict superset that adds `Flush` + `Close`).
- [ ] Implement `EnsureRepoAndCommit` so it calls `w.EnsureRepo` (using a deterministic `RepoID` derived via `fingerprint.RepoIDFromURL` from Stage 2.1, passed through `RepoInput.RepoID` once Stage 3.2 ships -- until then the legacy zero-value path is used and the deterministic ID is recorded on the cached `RepoAncestry`), then `w.EnsureCommit`, then `w.InsertNode(kind=repo)`; cache the result on the writer for subsequent `EnsureFile` calls.
- [ ] Add `ancestry_test.go` with a fake `RepoCommitNodeEdgeWriter` asserting (a) `EnsureRepo` + `EnsureCommit` + `InsertNode(kind=repo)` are each called exactly once before any `EnsureFile`, (b) `package` Node deduped by directory, (c) `file` Node inserted per WalkFile, (d) two `contains` Edges per file (`repo->pkg` once per new directory, `pkg->file` always), and (e) calling `EnsureFile` before `EnsureRepoAndCommit` returns a non-nil error.
- [ ] Add a doc comment block on `AncestryWriter` noting "not safe for concurrent use; one writer per scan; the package cache is single-writer" per architecture S3.4.

### Dependencies
- phase-identity-and-ancestry-refactor/stage-promote-canonical-signature-helpers

### Test Scenarios
- [ ] Scenario: ensurerepo-and-commit-called-once -- Given a scan that walks 100 files, When the ancestry writer runs, Then `w.EnsureRepo`, `w.EnsureCommit`, and `InsertNode(kind=repo)` are each called exactly once and complete before any `EnsureFile` call.
- [ ] Scenario: repo-node-once -- Given a scan that walks 100 files, When the ancestry writer runs, Then `InsertNode(kind=repo)` is called exactly once.
- [ ] Scenario: package-deduped -- Given 50 files all under `internal/foo/`, When `EnsureFile` runs per file, Then `InsertNode(kind=package)` is called exactly once and the `repo->package` `contains` Edge is inserted exactly once.
- [ ] Scenario: file-and-contains-per-walkfile -- Given a workspace of 7 files, When `EnsureFile` runs once per file, Then 7 `file` Nodes and 7 `package->file` `contains` Edges are inserted.
- [ ] Scenario: ensurefile-before-ensurerepo-errors -- Given a fresh `AncestryWriter`, When `EnsureFile` is called before `EnsureRepoAndCommit`, Then a non-nil error is returned and no Nodes are inserted.

## Stage 2.4: Worker adopts AncestryWriter

### Implementation Steps
- [ ] Refactor `worker.runFull` to construct an `AncestryWriter` and call `EnsureRepoAndCommit` + per-file `EnsureFile` instead of inlining the repo/package/file insert loop.
- [ ] Delete the now-dead inline blocks at `worker.go` lines 1084-1219 (the loop body), keeping the `EmitFile` call and the `EmitResult.TouchedNodes` retire path intact.
- [ ] Run `go test ./internal/repoindexer/...` and update `worker_unit_test.go` mocks where the call shape changes; keep `worker_integration_test.go` assertions identical (the on-disk graph must be byte-identical to the pre-refactor run).

### Dependencies
- phase-identity-and-ancestry-refactor/stage-ancestrywriter-factored-from-worker

### Test Scenarios
- [ ] Scenario: worker-graph-byte-identical -- Given the same fixture repo, When `worker.runFull` runs before and after the refactor, Then the resulting `node` / `edge` rows (canonical_signature, kind, parent_node_id, fingerprint) are byte-identical.
- [ ] Scenario: worker-integration-still-passes -- Given the existing `worker_integration_test.go`, When the suite runs after the refactor, Then all tests pass without assertion edits to graph contents.
- [ ] Scenario: helpers-no-internal-callers -- Given the refactor, When `grep -RIn "canonicalRepoSig\|canonicalPackageDir\|canonicalPackageSig\|canonicalFileSig" services/agent-memory/internal/` runs, Then no hits remain (only the exported names appear).

# Phase 3: graphsink storage abstraction

## Dependencies
- phase-identity-and-ancestry-refactor

## Stage 3.1: Sink interface skeleton

### Implementation Steps
- [ ] Create `internal/graphsink/sink.go` declaring the `Sink` interface (`EnsureRepo`, `EnsureCommit`, `InsertNode`, `InsertEdge`, `Flush`, `Close`) per architecture S3.2. `Sink` is a strict superset of `repoindexer.RepoCommitNodeEdgeWriter` (Stage 2.3) -- it adds `Flush` + `Close` only -- so every existing `*graphwriter.Writer` consumer compiles unchanged.
- [ ] Create `internal/graphsink/reader.go` declaring the `Reader` interface with `ListRepos(ctx, ReaderOptions) ([]graphreader.RepoSummary, error)`, `ListNodes`, `ListEdgesFrom`, `ListEdgesTo`, `GetNode`, and `LookupBySignature`. The Postgres adapter cannot issue direct SQL (tech-spec C5 / S4.5), so Stage 3.3 lifts `ListRepos` into `internal/graphreader.Reader` itself (the SELECT is currently inlined in `internal/mgmtapi/read.go:803` `handleListRepos`); the Postgres adapter then forwards to that primitive.
- [ ] Add a `RepoSummary` value type in `internal/graphreader/types.go` (single source of truth) with fields `RepoID`, `URL`, `SHA`, `GeneratedAt`, `RepoUUID` so the JSON envelope matches the wire shape Stage 7.2 emits; `graphsink.Reader` and all three backends return `[]graphreader.RepoSummary` directly.
- [ ] Add `internal/graphsink/doc.go` summarising the three backends, the snapshot-vs-append rule (S5), and the parity invariant (S2).
- [ ] Widen `AncestryWriter`'s writer parameter from `repoindexer.RepoCommitNodeEdgeWriter` to `graphsink.Sink` (Stage 2.3 already accepts the narrower shape; `Sink` is a strict superset, so existing call sites compile unchanged).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: sink-interface-compiles -- Given an empty implementation `type stubSink struct{}` in a test file with the required methods, When `go vet ./internal/graphsink/...` runs, Then the implementation satisfies `graphsink.Sink`.
- [ ] Scenario: reader-interface-compiles -- Given a stub `Reader` impl in tests (including `ListRepos`), When `go vet ./internal/graphsink/...` runs, Then the stub satisfies `graphsink.Reader`.
- [ ] Scenario: graphwriter-still-satisfies-narrow-writer -- Given the unchanged `*graphwriter.Writer`, When a test assigns it to a `repoindexer.RepoCommitNodeEdgeWriter` variable, Then the assignment compiles without modification (proves the Phase 2 interface stays satisfied by the real production writer).

## Stage 3.2: RepoID extension to RepoInput

### Implementation Steps
- [ ] Extend `graphwriter.RepoInput` (`internal/graphwriter/writer.go:250`) with a new optional field `RepoID fingerprint.RepoID` (zero-value means "use schema default"); keep the existing constructor/call sites compiling unchanged.
- [ ] Add `graphwriter.Writer.EnsureRepoWithID(ctx, RepoInput) (RepoRecord, error)` that overrides the `repo_id` PK with the supplied value via INSERT ... ON CONFLICT (url) DO UPDATE; document the legacy-collision caveat (returns existing `repo_id` per architecture S3.4).
- [ ] Add `writer_repoid_test.go` with sqlmock vectors covering (a) fresh insert returns the precomputed UUID, (b) URL collision returns the existing repo's UUID (parity gap per architecture S6.5).
- [ ] Update `cmd/repoindexer/main.go` and `internal/mgmtapi/*.go` call sites to leave `RepoID` zero (preserve legacy behaviour); a follow-up story migrates them.

### Dependencies
- phase-identity-and-ancestry-refactor/stage-repoidfromurl-helper

### Test Scenarios
- [ ] Scenario: ensurerepowithid-deterministic-insert -- Given an empty `repo` table and a non-zero `RepoInput.RepoID`, When `EnsureRepoWithID` runs, Then the row's `repo_id` equals the supplied UUID.
- [ ] Scenario: ensurerepo-zero-id-uses-default -- Given a zero-value `RepoInput.RepoID`, When `EnsureRepo` runs (legacy path), Then the row's `repo_id` is allocated by `gen_random_uuid()` and is non-zero.
- [ ] Scenario: url-collision-returns-existing -- Given an existing row with URL `https://x/y` and `repo_id = A`, When `EnsureRepoWithID` runs with the same URL and a different precomputed `repo_id = B`, Then the returned `RepoRecord.RepoID` equals `A` and a structured log records the parity gap.

## Stage 3.3: Postgres sink adapter

### Implementation Steps
- [ ] Create `internal/graphsink/postgres/sink.go` that wraps `*graphwriter.Writer`, forwarding all six `Sink` methods 1:1, calling `EnsureRepoWithID` when `RepoInput.RepoID` is non-zero and falling back to `EnsureRepo` otherwise.
- [ ] Lift `ListRepos` into `internal/graphreader/reader.go`: add `func (r *Reader) ListRepos(ctx context.Context, opts ReaderOptions) ([]RepoSummary, error)` owning the SELECT that mirrors the existing `internal/mgmtapi/read.go:handleListRepos` query (line 816). This keeps tech-spec C5 intact -- graphsink/postgres remains a thin forwarder with zero direct SQL.
- [ ] Create `internal/graphsink/postgres/reader.go` that wraps `*graphreader.Reader` for ALL six methods (`ListRepos`, `ListNodes`, `ListEdgesFrom`, `ListEdgesTo`, `GetNode`, `LookupBySignature`) -- pure forwarding, no SQL anywhere in the adapter package.
- [ ] Implement `LookupBySignature` in the postgres adapter via `ListNodes` + `ListNodesFilter.CanonicalSignature` -- still forwards to `*graphreader.Reader`, no direct SQL.
- [ ] Add a CI lint rule (or a `go vet`-friendly comment ban) that prohibits any direct `*sql.DB` / `database/sql` import inside `internal/graphsink/postgres/` (sink.go AND reader.go) per tech-spec C5 / S4.5. No exemptions -- all SQL lives in `graphwriter` (writes) or `graphreader` (reads).
- [ ] Add `internal/graphreader/listrepos_test.go` (sqlmock) verifying `ListRepos` returns the expected `RepoSummary` tuples ordered by `created_at DESC`, matching the mgmt-api semantics; cross-reference `mgmtapi/read.go:816` so a follow-up story can refactor `handleListRepos` to call the new primitive.
- [ ] Add `postgres_sink_test.go` (sqlmock) verifying every `Sink` method delegates to the underlying writer and propagates `WriteContractViolation` (SQLSTATE 42501) verbatim.
- [ ] Add `internal/graphsink/postgres/reader_test.go` as a pure forwarding test: a `fakeGraphReader` records each call and asserts the postgres adapter delegates 1:1 with no SQL of its own (verified by `go list -deps` showing the adapter package does not import `database/sql`).

### Dependencies
- phase-graphsink-storage-abstraction/stage-sink-interface-skeleton
- phase-graphsink-storage-abstraction/stage-repoid-extension-to-repoinput

### Test Scenarios
- [ ] Scenario: postgres-forwarding -- Given a sqlmock-backed `*graphwriter.Writer`, When each `Sink` method runs, Then the corresponding writer method is invoked exactly once with the same arguments.
- [ ] Scenario: write-contract-violation-propagates -- Given a SQL error with SQLSTATE 42501, When `InsertNode` runs, Then the returned error is a typed `WriteContractViolation` and the user-facing message includes the role hint.
- [ ] Scenario: lookupbysignature-uses-filter -- Given an existing Node with kind `method` and canonical signature `S`, When `LookupBySignature(repoID, "method", S)` runs, Then it returns that Node via `ListNodes` with `ListNodesFilter.CanonicalSignature=S`.
- [ ] Scenario: postgres-adapter-no-database-sql-import -- Given the `internal/graphsink/postgres/` package source, When `go list -deps -f '{{join .Deps "\n"}}' ./internal/graphsink/postgres/...` runs, Then `database/sql` does NOT appear in the dependency list (proves C5 / S4.5 thin-forwarder invariant -- all SQL lives in `graphwriter` or `graphreader`).
- [ ] Scenario: listrepos-forwards-to-graphreader -- Given a fake `*graphreader.Reader` that records calls, When the postgres adapter's `ListRepos(ctx, opts)` runs, Then exactly one delegated call is recorded with the same args and the returned `[]graphreader.RepoSummary` is returned unmodified.
- [ ] Scenario: graphreader-listrepos-matches-mgmtapi -- Given the same fixture rows in the `repo` + `repo_commit` tables, When `graphreader.Reader.ListRepos` runs AND the existing `mgmtapi.handleListRepos` runs, Then the two return identical ordered `[]RepoSummary` slices (validates the lifted SELECT preserves mgmt-api semantics).

## Stage 3.4: SQLite sink backend

### Implementation Steps
- [ ] Create `internal/graphsink/sqlite/schema.sql` mirroring `migrations/0002_repo_commit.sql` and `0003_node_edge.sql` with SQLite types (`TEXT` for uuid, `BLOB` for bytea, `TEXT` for jsonb+CHECK json_valid, `INTEGER` for timestamptz unix-millis) and `CHECK` constraints replacing the `node_kind` / `edge_kind` ENUMs.
- [ ] Add `internal/graphsink/sqlite/sink.go` implementing the `Sink` interface against `database/sql` with the `github.com/mattn/go-sqlite3` driver (operator-pinned 2026-05-30 -- requires `CGO_ENABLED=1`, which the codeintel build already mandates for tree-sitter parsers per tech-spec C7); bootstrap the schema on `Open`.
- [ ] Add `//go:build cgo` tags to `sink.go` and `reader.go` (Stage 3.6) so a CGO=0 build fails fast with a clear compile-time error rather than silently producing a binary that cannot open a SQLite file.
- [ ] Reuse `pkg/fingerprint.NodeFingerprint` / `EdgeFingerprint` verbatim in `InsertNode` / `InsertEdge` so identity is byte-identical to Postgres.
- [ ] Implement `EnsureRepo` to require non-zero `RepoInput.RepoID` (construction-time guard per architecture S3.4), inserting the precomputed UUID directly.
- [ ] Add `sqlite_sink_test.go` covering schema bootstrap, idempotent re-insert via `(repo_id, fingerprint)` UNIQUE, and `Close` committing the open transaction.

### Dependencies
- phase-graphsink-storage-abstraction/stage-sink-interface-skeleton

### Test Scenarios
- [ ] Scenario: sqlite-bootstraps-on-open -- Given a fresh `.db` file path, When `sqlite.Open(path)` runs, Then the file exists, the schema is applied, and `sqlite_master` lists `repo`, `repo_commit`, `node`, `edge` tables.
- [ ] Scenario: sqlite-idempotent-reinsert -- Given a Node already inserted, When the same `NodeInput` is inserted again, Then no new row is created and the existing row's `node_id` is returned.
- [ ] Scenario: sqlite-enum-check-rejects-bad-kind -- Given an `InsertNode` call with `Kind="bogus"`, When the SQLite backend runs, Then a CHECK-constraint error is returned and no row is inserted.
- [ ] Scenario: sqlite-requires-precomputed-repoid -- Given a zero-value `RepoInput.RepoID`, When `EnsureRepo` runs against the SQLite sink, Then a construction-time error is returned.
- [ ] Scenario: sqlite-requires-cgo -- Given the `internal/graphsink/sqlite/` package, When `go vet -tags '!cgo' ./internal/graphsink/sqlite/...` runs (or equivalent CGO=0 build), Then the package fails to compile with a build-tag error naming the missing `cgo` tag (proves operator-pinned `mattn/go-sqlite3` choice is enforced at compile time).

## Stage 3.5: Memory sink and JSON export

### Implementation Steps
- [ ] Create `internal/graphsink/memory/sink.go` with two append-only slices (`nodes`, `edges`) and a `map[fingerprint.Sum]string` for idempotent re-emit; require non-zero `RepoInput.RepoID`.
- [ ] Implement `Close` to optionally write a JSON document with the exact key order from architecture S3.2.4 (`repo`, `nodes`, `edges`) when constructed with a non-empty export path.
- [ ] Add a sibling rehydrator helper `memory.LoadExport(path) (Sink, Reader, error)` so `codeintel diagram --from-export <file>` can read the projection without re-scanning.
- [ ] Add `memory_sink_test.go` covering idempotent re-emit, JSON export key order, and round-trip via `LoadExport`.

### Dependencies
- phase-graphsink-storage-abstraction/stage-sink-interface-skeleton

### Test Scenarios
- [ ] Scenario: memory-idempotent -- Given the same `NodeInput` inserted twice, When `InsertNode` runs both times, Then the second call returns the cached id and the slice length is 1.
- [ ] Scenario: json-export-key-order -- Given a scan that produces `repo` / nodes / edges, When `Close` writes the export, Then the top-level keys are `repo`, `nodes`, `edges` in that order (verified by streaming-decode).
- [ ] Scenario: roundtrip-via-loadexport -- Given a memory-sink scan written to disk, When `LoadExport` re-reads it, Then the rehydrated `Reader` returns the same Node and Edge counts as the original scan.

## Stage 3.6: SQLite reader adapter

### Implementation Steps
- [ ] Add `internal/graphsink/sqlite/reader.go` implementing `graphsink.Reader` with `ListNodes` honoring `ListNodesFilter.ParentNodeID` / `Kinds` / `CanonicalSignature` and `ReaderOptions.Limit` clamped at `graphreader.MaxListLimit`.
- [ ] Implement `ListRepos` against the SQLite `repo` + `repo_commit` tables, joining each `repo` row to its most recent `repo_commit.committed_at` so `RepoSummary.GeneratedAt` is populated for the `GET /api/repos` consumer.
- [ ] Add `ListEdgesFrom` / `ListEdgesTo` returning slices ordered by `(kind, dst_node_id)` to match the Postgres reader's deterministic order.
- [ ] Implement `LookupBySignature` directly via SQL `WHERE repo_id=? AND kind=? AND canonical_signature=?` rather than wrapping `ListNodes`.
- [ ] Add `sqlite_reader_test.go` with a fixture graph and assertions on (a) hierarchy walk via `ParentNodeID`, (b) inbound / outbound edge enumeration, (c) `MaxListLimit` clamp surfaces in the returned slice, (d) `ListRepos` returns the scanned repo with its latest `committed_at`.

### Dependencies
- phase-graphsink-storage-abstraction/stage-sqlite-sink-backend

### Test Scenarios
- [ ] Scenario: sqlite-list-nodes-by-parent -- Given a `repo` with two `package` children, When `ListNodes(kinds=["package"], ParentNodeID=repoID)` runs, Then the two packages are returned and nothing else.
- [ ] Scenario: sqlite-list-edges-from -- Given a method with three outbound `static_calls` edges, When `ListEdgesFrom(srcNodeID, kinds=["static_calls"])` runs, Then exactly three Edges are returned.
- [ ] Scenario: sqlite-maxlistlimit-clamp -- Given 15 000 Nodes of kind `method`, When `ListNodes(kinds=["method"], Limit=20000)` runs, Then exactly 10 000 are returned and a structured log records the clamp.

## Stage 3.7: Memory reader adapter

### Implementation Steps
- [ ] Add `internal/graphsink/memory/reader.go` implementing `graphsink.Reader` by linear scan over the in-process slices, applying the same filters as the SQLite reader.
- [ ] Add a `LookupBySignature` fast-path that maintains a `map[sigKey]nodeID` populated on insert.
- [ ] Implement `ListRepos` by returning the single in-process `RepoSummary` captured by `EnsureRepo` + `EnsureCommit` (the memory backend stores exactly one repo per process per architecture S3.2.4).
- [ ] Add `memory_reader_test.go` mirroring the SQLite reader test cases for parity, including a `ListRepos` parity assertion.

### Dependencies
- phase-graphsink-storage-abstraction/stage-memory-sink-and-json-export

### Test Scenarios
- [ ] Scenario: memory-reader-parity -- Given the same fixture graph inserted into the memory sink and the SQLite sink, When both readers run the same `ListNodes` / `ListEdgesFrom` / `ListEdgesTo` queries, Then the returned slices have identical lengths and identical `(kind, canonical_signature)` projections.
- [ ] Scenario: memory-lookup-fast-path -- Given a Node inserted with signature `S`, When `LookupBySignature("method", S)` runs, Then it returns the Node in O(1) (test asserts via `t.Helper` and a benchmark that does not scale with N).

## Stage 3.8: Backend parity golden test

### Implementation Steps
- [ ] Add `internal/graphsink/parity_test.go` that runs the dispatcher against a single fixture repo three times -- once per backend (`postgres` via `pgtest`, `sqlite` via temp file, `memory`) -- and asserts equal `(repo_id, fingerprint, kind, canonical_signature)` tuples for both Nodes and Edges.
- [ ] Tag the Postgres arm with the existing `//go:build integration` build tag so the unit run skips it and the integration job exercises it.
- [ ] Capture the parity-test setup in `.claude/context/tests.md` so a contributor can rerun it locally.

### Dependencies
- phase-graphsink-storage-abstraction/stage-postgres-sink-adapter
- phase-graphsink-storage-abstraction/stage-sqlite-reader-adapter
- phase-graphsink-storage-abstraction/stage-memory-reader-adapter

### Test Scenarios
- [ ] Scenario: parity-three-backends -- Given the fixture repo `testdata/polyglot/`, When the dispatcher runs against each backend in turn, Then the sorted `(kind, canonical_signature, fingerprint_hex)` lines for Nodes match across all three backends.
- [ ] Scenario: edge-parity -- Given the same fixture, When the test extracts `(kind, src_fingerprint_hex, dst_fingerprint_hex, fingerprint_hex)` for Edges, Then the sorted lines match across all three backends.
- [ ] Scenario: legacy-postgres-documented-exception -- Given a Postgres row pre-existing with a random `repo_id`, When the parity test runs against that row, Then the documented exception path executes (test asserts the parity diff is non-empty AND the test classifies it as "legacy data" rather than a regression).

# Phase 4: Local materializer and SHA synthesis

## Dependencies
- phase-identity-and-ancestry-refactor

## Stage 4.1: MTimeTreeSHA helper

### Implementation Steps
- [ ] Add `pkg/fingerprint/mtime_tree.go` exporting `func MTimeTreeSHA(rootDir string, excludes []string) (string, error)` that walks `rootDir` via `os.ReadDir` / `os.Stat`, applies the exclude set, and computes the 32-char lowercase hex of `sha256(for each (relPath, mtime-unix, size) in stable lex order: relPath || 0x00 || mtime || 0x00 || size || 0x00)[:16]` per architecture S4.3.
- [ ] Add `mtime_tree_test.go` covering (a) stable output on no-op walks, (b) output changes when any file's mtime moves, (c) excluded directories are skipped, (d) error on a non-existent root.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: stable-on-noop -- Given a directory tree and two back-to-back `MTimeTreeSHA` calls with no file changes between them, When both calls run, Then they return the identical 32-char hex string.
- [ ] Scenario: mtime-bump-changes-hash -- Given a tree where one file's mtime is updated via `os.Chtimes`, When `MTimeTreeSHA` is recomputed, Then the returned string differs from the pre-update value.
- [ ] Scenario: exclude-applied -- Given a `.git/` directory inside the root, When `MTimeTreeSHA(root, []string{".git"})` runs, Then files under `.git/` contribute zero bytes to the hash (verified by removing `.git/` and re-hashing -- identical output).
- [ ] Scenario: missing-root-errors -- Given a path that does not exist, When `MTimeTreeSHA` runs, Then a non-nil error is returned and the empty string is returned for the hash.

## Stage 4.2: LocalDirMaterializer

### Implementation Steps
- [ ] Add `LocalDirMaterializer` type to `internal/repoindexer/materialize.go` next to `GitMaterializer`, satisfying the existing `Materializer` interface and walking with `defaultExcludeDirs`.
- [ ] Synthesize the `Workspace.URL` as `file://<abs-path>` (lower-cased drive letters on Windows, forward slashes); resolve `Workspace.SHA` from `git rev-parse HEAD` when `.git/` is present, otherwise fall back to `fingerprint.MTimeTreeSHA(rootDir, defaultExcludeDirs)`.
- [ ] Add `local_dir_materializer_test.go` covering (a) a non-git directory yields a stable mtime-based sha, (b) a git directory yields the HEAD sha, (c) the operator-supplied `sha` flag overrides both, (d) `Walk` skips `defaultExcludeDirs`.

### Dependencies
- phase-local-materializer-and-sha-synthesis/stage-mtimetreesha-helper

### Test Scenarios
- [ ] Scenario: local-non-git-sha -- Given a directory without `.git/`, When `Materialize("file:///path", "")` runs, Then `Workspace.SHA` equals `MTimeTreeSHA(path, defaultExcludeDirs)`.
- [ ] Scenario: local-git-sha -- Given a directory that is a git checkout, When `Materialize` runs with empty `sha`, Then `Workspace.SHA` equals the output of `git rev-parse HEAD`.
- [ ] Scenario: operator-sha-override -- Given an operator-supplied non-empty `sha`, When `Materialize(url, sha)` runs, Then `Workspace.SHA` equals the supplied value and no `git rev-parse` is invoked.
- [ ] Scenario: walk-excludes-applied -- Given a directory containing `node_modules/` and `.git/`, When `Workspace.Walk` runs, Then no `WalkFile` originates inside either directory.

# Phase 5: codeintel CLI binary

## Dependencies
- phase-graphsink-storage-abstraction
- phase-local-materializer-and-sha-synthesis

## Stage 5.1: cobra scaffolding

### Implementation Steps
- [ ] Add `github.com/spf13/cobra` to `services/agent-memory/go.mod`, pinning the latest stable version.
- [ ] Create `services/agent-memory/cmd/codeintel/main.go` with the root cobra command and persistent flags (`--store`, `--db`, `--log`, `--with-embeddings`); wire `slog.Default` to a text handler by default and a JSON handler when `--log=json`.
- [ ] Scaffold empty subcommands `scan`, `scan-many`, `diagram`, `serve` returning `errors.New("not implemented")` so the CLI compiles end-to-end.
- [ ] Add a `version` subcommand via cobra's built-in `cmd.SetVersionTemplate` (or a dedicated `cmd/codeintel/version.go`) that prints the build's semver, git commit, and build date populated via `-ldflags "-X main.version=..."`.
- [ ] Add `cmd/codeintel/README.md` with a one-paragraph "what it does" description pointing back to architecture S3.1.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: cli-help-lists-subcommands -- Given a built `codeintel` binary, When `codeintel --help` runs, Then the output names all five subcommands (`scan`, `scan-many`, `diagram`, `serve`, `version`).
- [ ] Scenario: unknown-subcommand-errors -- Given the same binary, When `codeintel bogus` runs, Then the exit code is non-zero and stderr names the offending subcommand.
- [ ] Scenario: log-flag-respected -- Given `--log=json`, When any subcommand emits a log line, Then the line is valid JSON parseable by `encoding/json`.

## Stage 5.2: scan subcommand

### Implementation Steps
- [ ] Implement `cmd/codeintel/scan.go` so `codeintel scan <path-or-url>` selects `LocalDirMaterializer` vs `GitMaterializer` based on whether the input starts with `file://`, is an absolute path, or matches a git URL scheme.
- [ ] Wire the chosen materializer through `NewAncestryWriter` + `ast.NewDispatcher` + the chosen `graphsink.Sink` (default `sqlite` per architecture S9.4); honor `--sha`, `--out`, and `--lang-hints`.
- [ ] Print a structured summary on completion (files walked, files parsed, nodes by kind, edges by kind, skipped by reason) per architecture S7.5.
- [ ] Return non-zero exit only on fatal errors (config, IO, write contract); coverage-degraded scans return 0 per tech-spec C7.

### Dependencies
- phase-codeintel-cli-binary/stage-cobra-scaffolding

### Test Scenarios
- [ ] Scenario: scan-local-sqlite -- Given a small local fixture repo and `codeintel scan ./fixture --store sqlite --out fixture.db`, When the command runs, Then `fixture.db` exists, contains `>=1 repo / package / file / class / method` Node, and the summary lists non-zero counts for each kind.
- [ ] Scenario: scan-url-with-sha -- Given a remote `git URL` and a SHA, When `codeintel scan <url> --sha <sha> --store sqlite --out remote.db` runs, Then the GitMaterializer fetches the commit and the resulting graph is non-empty.
- [ ] Scenario: scan-coverage-degraded-exit-zero -- Given a fixture with `.c` files on a CGO=0 build, When `codeintel scan ./fixture` runs, Then the exit code is 0, the summary reports `skipped.no_parser >= 1`, and stderr contains the per-extension count.
- [ ] Scenario: scan-fatal-io-exit-nonzero -- Given `--out /nonexistent/dir/foo.db`, When the command runs, Then the exit code is non-zero and stderr names the IO error.

## Stage 5.3: scan-many subcommand

### Implementation Steps
- [ ] Implement `cmd/codeintel/scan_many.go` so `codeintel scan-many <manifest>` reads one line per repo (each line either `<path>` or `<git-url>@<sha>`), iterates them sequentially, and writes one `.db` file per repo under `--out-dir`.
- [ ] On per-repo failure, append a `failed: <reason>` summary line and continue to the next; emit an aggregate summary at the end (total succeeded, failed, total nodes/edges).
- [ ] Add `manifest_test.go` covering parsing (path-only, url@sha, blank-line skipping, comment lines beginning with `#`).

### Dependencies
- phase-codeintel-cli-binary/stage-scan-subcommand

### Test Scenarios
- [ ] Scenario: scan-many-three-repos -- Given a manifest with three valid entries, When the command runs with `--out-dir ./scans`, Then three `.db` files appear under `./scans/` and the summary reports `succeeded=3, failed=0`.
- [ ] Scenario: scan-many-partial-failure -- Given a manifest where the middle entry is an invalid git URL, When the command runs, Then two `.db` files are produced, the failed entry has a `failed:` line, and the exit code is non-zero per tech-spec S4.3.
- [ ] Scenario: manifest-comments-skipped -- Given a manifest with `# comment` and blank lines, When the parser runs, Then those lines yield zero scan iterations.

## Stage 5.4: scan summary printer

### Implementation Steps
- [ ] Add `internal/codeintel/summary/summary.go` with an `Aggregator` that subscribes to dispatcher log events (`ast.dispatch.skip`, `ast.parse.error`) and tracks Node / Edge counts by kind via the Sink wrapper.
- [ ] Render the summary in two formats: plain text (default) and JSON (when `--log=json`). Both list `walked, parsed, skipped{no_parser:N, pwsh_not_available:M, unsupported_ext:K}`, `nodes{repo, package, file, class, method, block}`, `edges{contains, imports, static_calls, ...}`.
- [ ] Wire the aggregator into `scan` and `scan-many` so each repo prints its own summary AND the manifest run prints an aggregate at the end.

### Dependencies
- phase-codeintel-cli-binary/stage-scan-many-subcommand

### Test Scenarios
- [ ] Scenario: summary-text-format -- Given a successful scan, When the text-format summary prints, Then it contains lines beginning with `walked:`, `parsed:`, `nodes:`, `edges:`, `skipped:`.
- [ ] Scenario: summary-json-format -- Given `--log=json`, When the scan completes, Then the last stdout line is a JSON object with the same six top-level keys and parses cleanly.
- [ ] Scenario: per-extension-skip-aggregation -- Given a scan with 3 `.c` files and 1 `.cs` file on a CGO=0 build, When the summary renders, Then `skipped.no_parser_by_ext` shows `{".c": 3, ".cs": 1}` (or equivalent map shape).

## Stage 5.5: opt-in embeddings flag

### Implementation Steps
- [ ] Wire `--with-embeddings` in `cmd/codeintel/scan.go` to construct the `embedding.AsASTPublisher` (only when set) and pass it through `ast.WithEmbeddingPublisher` on the dispatcher.
- [ ] When the flag is absent, do NOT read `AGENT_MEMORY_QDRANT_URL` or any embedder env vars; the CLI must run without Qdrant per tech-spec C8.
- [ ] Add `scan_embeddings_test.go` asserting (a) flag absent => no embedder is constructed, (b) flag present + `AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true` => the stub embedder is constructed and dispatched.

### Dependencies
- phase-codeintel-cli-binary/stage-scan-subcommand

### Test Scenarios
- [ ] Scenario: default-no-embedder -- Given a `codeintel scan` invocation without `--with-embeddings`, When the dispatcher is constructed, Then `ast.WithEmbeddingPublisher` is NOT in the option chain (verified via a test-only reflection helper).
- [ ] Scenario: opt-in-stub-embedder -- Given `--with-embeddings` and `AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true`, When the scan runs, Then the dispatcher's embedding publisher receives at least one `EmbeddingItem` for a method Node.
- [ ] Scenario: opt-in-no-stub-fails -- Given `--with-embeddings` without the allow-stub env var and without a real embedder URL, When the scan runs, Then it returns a non-zero exit and the error names the missing embedder configuration.

# Phase 6: Diagram projector

## Dependencies
- phase-graphsink-storage-abstraction

## Stage 6.1: Diagram envelope types

### Implementation Steps
- [ ] Create `internal/diagram/envelope.go` with the `Diagram`, `Node`, `Edge`, `Repo`, `Stats` Go structs whose `json` tags match the envelope shape in architecture S4.4 (incl. `layoutHint`, `truncated`, `stats.cappedAt`, `stats.skipped`).
- [ ] Add `envelope_marshal_test.go` with a golden JSON file asserting key order: `diagram, repo, generatedAt, layoutHint, nodes, edges, truncated, stats`.
- [ ] Document the envelope under `internal/diagram/doc.go` and cross-reference architecture S4.4 + tech-spec S6.2.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: envelope-marshal-key-order -- Given an envelope value, When `encoding/json.Marshal` runs, Then the resulting bytes match a stored golden file byte-for-byte.
- [ ] Scenario: envelope-unmarshal-roundtrip -- Given marshalled bytes, When unmarshalled back into the struct and re-marshalled, Then the second pass equals the first byte-for-byte.

## Stage 6.2: BuildModuleDiagram

### Implementation Steps
- [ ] Add `internal/diagram/module.go` with `BuildModuleDiagram(ctx, reader graphsink.Reader, repoID fingerprint.RepoID, granularity string) (Diagram, error)` that walks `repo -> package -> (file|class)` via `ListNodes(ParentNodeID=...)` and emits containment edges.
- [ ] Roll up `imports` edges: enumerate `imports` Edges via `ListEdgesFrom` per file node, resolve the src/dst to their owning package, dedupe (src_pkg, dst_pkg) pairs, and emit one synthetic `imports` edge per pair with `weight = count`.
- [ ] Set `layoutHint = "hierarchical-top-down"` for package granularity; use synthetic ids `pkg:<canonical_signature>` for roll-up nodes per architecture S4.4.
- [ ] Add `module_test.go` with a fixture graph asserting (a) the `repo` node anchors the tree, (b) one `contains` edge per parent->child, (c) the `imports` roll-up dedupes and weights correctly.

### Dependencies
- phase-diagram-projector/stage-diagram-envelope-types

### Test Scenarios
- [ ] Scenario: module-tree-shape -- Given a fixture with 1 repo, 2 packages, 5 files, When `BuildModuleDiagram(granularity="package")` runs, Then the diagram has 3 nodes (repo + 2 pkgs) and 2 contains edges.
- [ ] Scenario: imports-rollup-weight -- Given 3 files in `pkg/a/` that each import 2 files in `pkg/b/`, When the projector runs at `granularity="package"`, Then exactly one `imports` edge `pkg:a -> pkg:b` with `weight=6` is emitted.
- [ ] Scenario: granularity-file -- Given the same fixture and `granularity="file"`, When the projector runs, Then file Nodes appear and per-file `imports` edges are NOT rolled up.

## Stage 6.3: BuildCallChain BFS

### Implementation Steps
- [ ] Add `internal/diagram/callchain.go` with `BuildCallChain(ctx, reader graphsink.Reader, seed string, depth int, direction string) (Diagram, error)` that resolves `seed` first via `LookupBySignature` then via `GetNode` if it parses as a UUID.
- [ ] Perform a bounded BFS up to `depth` steps; per frontier node call `ListEdgesFrom(kinds=["static_calls","observed_calls"])` for callees (when `direction in {both, callees}`) and `ListEdgesTo(...)` for callers (when `direction in {both, callers}`).
- [ ] Tag each emitted edge with its underlying `kind` so the UI can style `observed_calls` differently per architecture S4.4.1.
- [ ] Set `layoutHint = "hierarchical-left-right"`; return an error envelope when `seed` does not resolve.

### Dependencies
- phase-diagram-projector/stage-diagram-envelope-types

### Test Scenarios
- [ ] Scenario: bfs-callees-only -- Given a method M with 2 callees and 3 callers, When `BuildCallChain(seed=M, depth=1, direction="callees")` runs, Then 3 nodes (M + 2 callees) and 2 edges are emitted.
- [ ] Scenario: bfs-both-directions -- Given the same fixture, When `direction="both"`, Then 6 nodes (M + 2 callees + 3 callers) and 5 edges are emitted.
- [ ] Scenario: bfs-depth-two -- Given a chain A -> B -> C -> D, When `BuildCallChain(seed=A, depth=2, direction="callees")` runs, Then nodes A, B, C appear and D does not.
- [ ] Scenario: seed-unresolved-error -- Given a `seed` that matches no Node, When `BuildCallChain` runs, Then it returns an error with a `seed_not_found` code and the diagram envelope is the zero value.

## Stage 6.4: Truncation accounting

### Implementation Steps
- [ ] Add a shared `internal/diagram/truncate.go` helper that the two builders call before appending a Node or Edge; when `len(nodes) + len(edges) >= cap`, set `truncated = true`, populate `stats.cappedAt`, and stop accumulating.
- [ ] Default the cap to `graphreader.MaxListLimit` (10 000); allow a builder option to override for tests.
- [ ] Add `truncate_test.go` covering (a) under-cap leaves `truncated=false`, (b) at-cap stops accumulation, (c) `stats` counts reflect the truncated state.

### Dependencies
- phase-diagram-projector/stage-buildmodulediagram
- phase-diagram-projector/stage-buildcallchain-bfs

### Test Scenarios
- [ ] Scenario: under-cap-not-truncated -- Given a graph with 50 nodes + 50 edges and `cap=10000`, When either builder runs, Then `truncated=false` and `stats.nodeCount + stats.edgeCount == 100`.
- [ ] Scenario: at-cap-truncated -- Given a graph that would produce 15 000 elements at the chosen builder and `cap=10000`, When the builder runs, Then `truncated=true`, `stats.cappedAt=10000`, and the returned slice lengths sum to exactly 10 000.
- [ ] Scenario: stats-skipped-populated -- Given a scan that emitted `no_parser` skips of 3 `.c` files, When the projector receives the summary alongside, Then `stats.skipped.no_parser == 3`.

## Stage 6.5: diagram CLI subcommands

### Implementation Steps
- [ ] Implement `cmd/codeintel/diagram.go` with two sub-subcommands: `codeintel diagram module --store sqlite --db repo.db --granularity package --out module.json` and `codeintel diagram calls --store sqlite --db repo.db --seed <sig> --depth 2 --direction both --out calls.json`.
- [ ] Open the chosen backend's `Reader`, dispatch to the matching builder, and write the envelope to `--out` (or stdout when `--out=-`).
- [ ] Support `--from-export <file>` to skip the live backend and rehydrate via `memory.LoadExport` per architecture S3.2.4.

### Dependencies
- phase-diagram-projector/stage-truncation-accounting
- phase-codeintel-cli-binary/stage-cobra-scaffolding

### Test Scenarios
- [ ] Scenario: diagram-module-out-file -- Given a populated `repo.db`, When `codeintel diagram module --db repo.db --out module.json` runs, Then `module.json` exists, parses as the envelope, and has `diagram="module"`.
- [ ] Scenario: diagram-calls-stdout -- Given a populated DB and `--out=-`, When `codeintel diagram calls --seed <sig> --depth 1` runs, Then stdout contains a valid envelope with `diagram="callchain"`.
- [ ] Scenario: diagram-from-export -- Given a memory-backend JSON export file, When `codeintel diagram module --from-export export.json --out module.json` runs, Then the produced envelope matches the live-backend equivalent.

# Phase 7: Serve endpoint

## Dependencies
- phase-diagram-projector

## Stage 7.1: codeintel serve binary

### Implementation Steps
- [ ] Implement `cmd/codeintel/serve.go` to start an `http.Server` on `--addr` (default `:8088`), using a `chi`-free stdlib `http.ServeMux` with three handlers wired to the chosen backend.
- [ ] Apply a CORS middleware that allows the dev origin pinned in tech-spec S6.3 (`http://localhost:5173`) for `GET` requests; reject other methods with 405.
- [ ] Add a `--cors-origin` flag to override the default origin; only allow exact-match origins (no wildcards) to keep the trust boundary tight per architecture S7.4.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: serve-listens-on-addr -- Given `codeintel serve --addr :18088 --store sqlite --db repo.db`, When the server starts, Then a `GET /` returns a 404 (no root handler) and the bind succeeds.
- [ ] Scenario: cors-allows-vite-origin -- Given a preflight `OPTIONS /api/repos` with `Origin: http://localhost:5173`, When the middleware processes it, Then the response sets `Access-Control-Allow-Origin: http://localhost:5173`.
- [ ] Scenario: cors-rejects-foreign-origin -- Given `Origin: https://evil.example`, When the same preflight runs, Then no `Access-Control-Allow-Origin` header is set and the response is 403.

## Stage 7.2: repos handler

### Implementation Steps
- [ ] Add `cmd/codeintel/serve_repos.go` exposing `GET /api/repos` by calling `graphsink.Reader.ListRepos` (defined in Stage 3.1 and implemented per-backend in Stages 3.3 / 3.6 / 3.7); the handler is backend-agnostic and works identically against SQLite, memory, and Postgres.
- [ ] Return `[{id, url, sha, generatedAt}]` JSON marshalled directly from the `RepoSummary` slice; `generatedAt` is the most recent `repo_commit.committed_at` already populated by each backend's `ListRepos` implementation.
- [ ] Add `serve_repos_test.go` (httptest) asserting (a) empty backend returns `[]` (not `null`), (b) one-repo backend returns the expected single-element JSON array, (c) the response Content-Type is `application/json`.

### Dependencies
- phase-serve-endpoint/stage-codeintel-serve-binary

### Test Scenarios
- [ ] Scenario: repos-sqlite-single -- Given a `repo.db` with one scanned repo, When `GET /api/repos` runs, Then the response is a one-element JSON array with the correct `url` and `sha`.
- [ ] Scenario: repos-postgres-multi -- Given a Postgres backend with three repos, When `GET /api/repos` runs, Then the response array length is 3 and contains the expected URLs.
- [ ] Scenario: repos-empty-returns-empty-array -- Given a backend with zero repos, When `GET /api/repos` runs, Then the response body is `[]` (not `null`) and the Content-Type is `application/json`.

## Stage 7.3: module diagram handler

### Implementation Steps
- [ ] Add `cmd/codeintel/serve_module.go` exposing `GET /api/diagram/module?repo=<id>&granularity=<package|file|class>` that calls `diagram.BuildModuleDiagram` and writes the envelope as JSON.
- [ ] Validate `granularity` against the closed set per architecture S9.5 (default `package`); 400 on invalid values.

### Dependencies
- phase-serve-endpoint/stage-codeintel-serve-binary

### Test Scenarios
- [ ] Scenario: module-handler-200 -- Given a populated backend, When `GET /api/diagram/module?repo=<id>&granularity=package` runs, Then the response is 200, content-type `application/json`, and body parses as the envelope.
- [ ] Scenario: module-handler-bad-granularity-400 -- Given `granularity=bogus`, When the handler runs, Then the response is 400 and the body names the closed-set values.

## Stage 7.4: callchain diagram handler

### Implementation Steps
- [ ] Add `cmd/codeintel/serve_calls.go` exposing `GET /api/diagram/calls?repo=<id>&seed=<sig-or-id>&depth=<n>&direction=<both|callers|callees>` calling `diagram.BuildCallChain`.
- [ ] Validate `depth` is a non-negative integer (default 2, cap at 10); validate `direction` against the closed set; 400 on invalid values; 404 on `seed_not_found`.

### Dependencies
- phase-serve-endpoint/stage-codeintel-serve-binary

### Test Scenarios
- [ ] Scenario: calls-handler-200 -- Given a known method seed, When the handler runs, Then the response is 200 and the envelope's `diagram="callchain"`.
- [ ] Scenario: calls-handler-seed-not-found-404 -- Given an unknown seed, When the handler runs, Then the response is 404 with `{"error":"seed_not_found"}`.
- [ ] Scenario: calls-handler-depth-clamped -- Given `depth=999`, When the handler runs, Then it returns 400 with a message naming the cap (10).

## Stage 7.5: mgmt-api integration mount

### Implementation Steps
- [ ] Add a new `internal/mgmtapi/diagram.go` registering the three routes on the existing `Handler.mux` so the production deployment exposes them alongside the `mgmt.read.*` surface.
- [ ] Gate the mount behind an `Options.EnableDiagramRoutes bool` (default false) so the existing mgmt-api binary opts in deliberately.
- [ ] Add `diagram_handler_test.go` with sqlmock + the existing handler test fixtures asserting the three routes are reachable when the option is set and 404 when it is not.

### Dependencies
- phase-serve-endpoint/stage-module-diagram-handler
- phase-serve-endpoint/stage-callchain-diagram-handler

### Test Scenarios
- [ ] Scenario: mgmtapi-routes-disabled-by-default -- Given `Options.EnableDiagramRoutes=false`, When `GET /api/diagram/module` hits the handler, Then the response is 404.
- [ ] Scenario: mgmtapi-routes-enabled -- Given `Options.EnableDiagramRoutes=true`, When the same request runs, Then the response is 200 and the body parses as the envelope.

# Phase 8: React and neo4j-nvl web UI

## Dependencies
- phase-serve-endpoint

## Stage 8.1: Vite and TypeScript scaffold

### Implementation Steps
- [ ] Run `npm create vite@latest services/agent-memory/web -- --template react-ts` (or equivalent) and commit the resulting `package.json`, `vite.config.ts`, `tsconfig.json`, `index.html`, `src/main.tsx`, `src/App.tsx`.
- [ ] Add `@neo4j-nvl/react` and `@neo4j-nvl/base` as dependencies; set `vite.config.ts` `server.proxy` to forward `/api/*` to `http://localhost:8088`.
- [ ] Add `services/agent-memory/web/README.md` with `npm install`, `npm run dev`, `npm run build` instructions and the URL the dev server listens on.
- [ ] Add a minimal `npm test` script (Vitest) so CI has a hook even if the test set is small.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: web-builds -- Given a fresh `npm install` under `services/agent-memory/web/`, When `npm run build` runs, Then the build succeeds and produces `dist/index.html`.
- [ ] Scenario: web-dev-server-proxies -- Given `npm run dev` with `codeintel serve` running on `:8088`, When the browser loads `http://localhost:5173/api/repos`, Then the response is proxied to the serve endpoint and returns 200.

## Stage 8.2: API client and envelope types

### Implementation Steps
- [ ] Add `services/agent-memory/web/src/api/types.ts` with TypeScript interfaces mirroring the diagram envelope from architecture S4.4 (one-to-one with the Go structs).
- [ ] Add `services/agent-memory/web/src/api/client.ts` with `fetchRepos()`, `fetchModuleDiagram(repoId, granularity)`, `fetchCallChain(repoId, seed, depth, direction)`; all return typed `Promise<Diagram>`.
- [ ] Add `src/api/client.test.ts` (Vitest) with `msw` (or `fetch` mocks) covering happy-path 200 and 404 `seed_not_found`.

### Dependencies
- phase-react-and-neo4j-nvl-web-ui/stage-vite-and-typescript-scaffold

### Test Scenarios
- [ ] Scenario: types-compile-against-envelope -- Given the envelope JSON returned by the Go side and the TS types, When `tsc --noEmit` runs, Then no type errors are reported.
- [ ] Scenario: client-404-rejected -- Given a `GET /api/diagram/calls` that returns 404 with `{"error":"seed_not_found"}`, When `fetchCallChain` runs, Then the promise rejects with an `Error` whose message contains `seed_not_found`.

## Stage 8.3: RepoPicker and DiagramSwitch

### Implementation Steps
- [ ] Add `src/components/RepoPicker.tsx` that calls `fetchRepos()` on mount and renders a `<select>` of repos; selection updates URL hash (`#repo=<id>`).
- [ ] Add `src/components/DiagramSwitch.tsx` with two buttons (Module / Call Chain); selection updates URL hash (`#mode=module|callchain`).
- [ ] Wire both into `App.tsx` so changing the picker or the switch triggers a re-fetch of the relevant diagram.

### Dependencies
- phase-react-and-neo4j-nvl-web-ui/stage-api-client-and-envelope-types

### Test Scenarios
- [ ] Scenario: repopicker-renders-options -- Given `fetchRepos` mock returning three repos, When `<RepoPicker>` renders, Then the `<select>` has three `<option>` elements with the correct labels.
- [ ] Scenario: diagramswitch-persists-mode -- Given the user clicks "Call Chain", When the test inspects `window.location.hash`, Then it contains `mode=callchain`.

## Stage 8.4: NvlCanvas mapping

### Implementation Steps
- [ ] Add `src/components/NvlCanvas.tsx` wrapping `@neo4j-nvl/react`'s `InteractiveNvlWrapper`; map the envelope to NVL `nodes` / `relationships` per the table in architecture S4.4.1.
- [ ] Apply `layoutHint` -> NVL layout: `hierarchical-top-down` => hierarchical down; `hierarchical-left-right` => hierarchical right; `force` => force-directed.
- [ ] Color nodes by `language` (per palette in tech-spec S7, default fallback to grey); size by `kind` (package=40, file=30, class=22, method=18, block=14).
- [ ] Style `observed_calls` edges as dashed grey to distinguish from `static_calls`.

### Dependencies
- phase-react-and-neo4j-nvl-web-ui/stage-repopicker-and-diagramswitch

### Test Scenarios
- [ ] Scenario: nvl-renders-module -- Given a fixture module envelope with 5 nodes and 4 edges, When `<NvlCanvas>` mounts, Then the rendered NVL canvas contains 5 nodes and 4 relationships (asserted via the wrapper's `getDataAdapter` API or DOM count).
- [ ] Scenario: layouthint-honored -- Given `layoutHint="hierarchical-left-right"`, When the component re-renders, Then the `nvlOptions.layout` prop equals the hierarchical direction-right value.
- [ ] Scenario: observed-calls-dashed -- Given an envelope with one `observed_calls` edge, When the canvas renders, Then that relationship's `color` prop is grey and `style` includes the dashed marker.

## Stage 8.5: Inspector panel

### Implementation Steps
- [ ] Add `src/components/Inspector.tsx` that subscribes to the canvas selection and renders a side panel with the selected node's `attrs` (language, modifiers, startLine, endLine, params_raw, calls_raw) per architecture S3.8.
- [ ] Surface raw call data (`calls_raw`) prominently so the user sees what the parser saw before the same-file resolver fallback, per tech-spec R3 mitigation.
- [ ] Add `Inspector.test.tsx` asserting the panel shows / hides based on selection state.

### Dependencies
- phase-react-and-neo4j-nvl-web-ui/stage-nvlcanvas-mapping

### Test Scenarios
- [ ] Scenario: inspector-shows-on-select -- Given a node click event, When the canvas dispatches the selection, Then the Inspector renders the node's `attrs` and the panel becomes visible.
- [ ] Scenario: inspector-hides-on-deselect -- Given a click on empty canvas space, When the selection clears, Then the Inspector is hidden (CSS `display:none` or component returns null).
- [ ] Scenario: inspector-shows-calls-raw -- Given a node with `attrs.calls_raw=["foo","bar"]`, When the Inspector renders, Then both strings appear in the DOM.

## Stage 8.6: Interactive call-chain re-seeding

### Implementation Steps
- [ ] Add `src/components/CallChainNav.tsx` that listens for node-click events when `mode=callchain` and re-issues `fetchCallChain(repoId, clickedNodeId, depth, direction)`.
- [ ] Update the URL hash with `seed=<id>` so the view is shareable; back-button navigation re-issues the previous fetch.
- [ ] Add `CallChainNav.test.tsx` covering (a) click triggers re-fetch, (b) URL hash updates, (c) back-button restores previous seed.

### Dependencies
- phase-react-and-neo4j-nvl-web-ui/stage-nvlcanvas-mapping

### Test Scenarios
- [ ] Scenario: click-reseeds -- Given a rendered call-chain diagram, When the user clicks node X, Then `fetchCallChain` is invoked with `seed=X.id` and the canvas re-renders with the new envelope.
- [ ] Scenario: hash-updates-on-reseed -- Given the click above, When the test reads `window.location.hash`, Then it contains `seed=<X.id>`.
- [ ] Scenario: back-button-restores-seed -- Given two consecutive re-seeds (A -> B -> C), When the user navigates back once, Then `fetchCallChain` is invoked with `seed=B.id` and the canvas reflects that envelope.

## Stage 8.7: Truncation badge and skip warnings

### Implementation Steps
- [ ] Add `src/components/TruncationBadge.tsx` that renders "Results clipped at N" when `envelope.truncated === true`, reading `stats.cappedAt`.
- [ ] Add `src/components/SkipBanner.tsx` that renders a yellow banner when `envelope.stats.skipped.no_parser > 0` or `pwsh_not_available > 0`, naming the missing prereq (CGO toolchain / `pwsh` on PATH).
- [ ] Wire both into `App.tsx` so they appear above the canvas whenever the envelope flags either condition.

### Dependencies
- phase-react-and-neo4j-nvl-web-ui/stage-nvlcanvas-mapping

### Test Scenarios
- [ ] Scenario: badge-shows-when-truncated -- Given an envelope with `truncated=true, stats.cappedAt=10000`, When `<App>` renders, Then "Results clipped at 10000" appears above the canvas.
- [ ] Scenario: badge-hidden-when-not-truncated -- Given `truncated=false`, When `<App>` renders, Then the badge is absent from the DOM.
- [ ] Scenario: skip-banner-shows -- Given `stats.skipped.no_parser=3`, When `<App>` renders, Then a yellow banner appears with the text "3 files skipped (no parser; needs CGO build)".

# Phase 9: Docs and shipping

## Dependencies
- phase-codeintel-cli-binary
- phase-react-and-neo4j-nvl-web-ui

## Stage 9.1: Quickstart documentation

### Implementation Steps
- [ ] Add `services/agent-memory/cmd/codeintel/QUICKSTART.md` with a "Scan a repo in 60 seconds" walkthrough covering `go build ./cmd/codeintel`, `codeintel scan ./fixture`, `codeintel serve`, and `npm run dev` from `web/`; include an inline ASCII diagram of the module and call-chain views so the doc is self-describing without external image assets.
- [ ] Update `services/agent-memory/README.md` to link to QUICKSTART and to the web README.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: quickstart-commands-valid -- Given a fresh clone with CGO toolchain on PATH, When the operator runs every command listed in QUICKSTART in order, Then each command exits 0 and the final `curl http://localhost:8088/api/repos` returns the scanned repo.
- [ ] Scenario: readme-links-resolve -- Given a Markdown link checker on `services/agent-memory/README.md`, When it runs, Then all new links to QUICKSTART and web/README resolve.

## Stage 9.2: Context refresh

### Implementation Steps
- [ ] Update `.claude/context/usage.md` with the `codeintel` CLI subcommands and one-line examples for each.
- [ ] Update `.claude/context/tests.md` to record the new test paths (parity test, polyglot smoke, web Vitest run) and the CGO + `pwsh` caveats.
- [ ] Update `.claude/context/structure.md` to add the new packages (`internal/graphsink/...`, `internal/diagram`, `cmd/codeintel`, `web/`).

### Dependencies
- phase-docs-and-shipping/stage-quickstart-documentation

### Test Scenarios
- [ ] Scenario: context-docs-mention-codeintel -- Given the updated `.claude/context/usage.md`, When a reader greps for `codeintel`, Then at least three command examples appear.
- [ ] Scenario: structure-doc-lists-new-packages -- Given the updated `.claude/context/structure.md`, When a reader greps for `graphsink`, `diagram`, `codeintel`, `web`, Then each grep returns at least one hit.

## Stage 9.3: End-to-end acceptance walk

### Implementation Steps
- [ ] Add `services/agent-memory/cmd/codeintel/e2e_test.go` (tagged `//go:build e2e`) that scans the repo's own `services/agent-memory/` directory to a temp SQLite file, builds the module diagram, builds a call-chain diagram, and serves them via `codeintel serve` on an ephemeral port.
- [ ] Add a sibling `services/agent-memory/web/e2e/` Playwright smoke test (operator-pinned 2026-05-30 -- Playwright matches the repo's existing e2e workflow conventions) that loads the served URL, picks the repo, switches modes, and asserts the canvas contains nodes for both diagrams.
- [ ] Add Playwright to `services/agent-memory/web/package.json` as a `devDependency`, scaffold `playwright.config.ts` pinned to the Chromium-only project for CI speed, and add an `npm run test:e2e` script invoked by the CI job.
- [ ] Wire the e2e job into CI behind the `e2e` tag and document how to run it locally in QUICKSTART.
- [ ] Inside the Playwright walk, capture two reference PNGs (`module.png`, `call-chain.png`) via `await page.screenshot({ path: "../docs/screenshots/module.png" })` once each diagram has rendered; commit both under `services/agent-memory/web/docs/screenshots/` and update `services/agent-memory/web/README.md` to embed them with `![module diagram](docs/screenshots/module.png)` so the web README has real visual references produced by the e2e walk itself.

### Dependencies
- phase-docs-and-shipping/stage-context-refresh

### Test Scenarios
- [ ] Scenario: e2e-scan-serve-fetch -- Given a clean checkout, When the Go e2e test runs, Then it scans `services/agent-memory/`, serves the result, fetches `GET /api/repos` and both diagram endpoints, and asserts each response is 200 with non-empty `nodes`.
- [ ] Scenario: e2e-ui-renders-both -- Given the served URL, When the Playwright test runs, Then it switches between Module and Call Chain modes and asserts at least one node is rendered in each.
- [ ] Scenario: e2e-acceptance-criteria -- Given the architecture S8 acceptance criteria, When the e2e walk finishes, Then each criterion in S8.2, S8.3, S8.4 is exercised by at least one of the steps above.
- [ ] Scenario: e2e-screenshots-committed -- Given the Playwright walk completes, When the test harness exits, Then `services/agent-memory/web/docs/screenshots/module.png` and `call-chain.png` both exist with byte size >0 and `web/README.md` references both filenames via `![...](docs/screenshots/...png)` markdown.

### Cross-Stage Dependencies
- phase-codeintel-cli-binary/stage-scan-subcommand
- phase-diagram-projector/stage-diagram-cli-subcommands
- phase-serve-endpoint/stage-callchain-diagram-handler
- phase-react-and-neo4j-nvl-web-ui/stage-interactive-call-chain-re-seeding
- phase-react-and-neo4j-nvl-web-ui/stage-truncation-badge-and-skip-warnings
