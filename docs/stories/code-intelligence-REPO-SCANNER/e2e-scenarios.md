---
title: "repo scanner -- e2e scenarios"
storyId: "code-intelligence:REPO-SCANNER"
---

> Gherkin-style feature scenarios for the `codeintel` CLI, the
> `graphsink` storage backends, the `diagram` projector, the serve
> endpoint, and the `services/agent-memory/web/` React + neo4j-nvl
> SPA. Phases mirror the nine-phase ladder in
> `implementation-plan.md`; each phase block names its `### Setup`
> envelope (type, local runner, CI runner, secrets, bootstrap)
> directly before its `### Scenarios` list.
>
> Sibling specs (read for shared concepts, do not duplicate):
>
> - `architecture.md` -- component map, data flows, pinned
>   defaults (S9.1-S9.5), diagram envelope (S4.4), acceptance
>   criteria (S8.1-S8.5).
> - `tech-spec.md` -- hard constraints (C1-C10), risk register
>   (R1-R12), in/out-of-scope split.
> - `implementation-plan.md` -- stage order and the per-stage
>   `Test Scenarios` checkboxes this file expands into Gherkin
>   form.
>
> Conventions:
>
> - Each `Scenario:` covers ONE behavioural assertion. Where the
>   `implementation-plan.md` stage names a `Scenario: foo`
>   checkbox, that scenario is restated here in Given/When/Then
>   form and tagged with its source anchor in parentheses
>   (e.g. `(impl-plan Stage 5.2)`).
> - Edge cases prefixed `Edge:` are NEW QA-only checks this file
>   adds on top of the implementation-plan list.
> - Connection strings are always env-var names
>   (`AGENT_MEMORY_PG_URL`, `CODEINTEL_DB_PATH`,
>   `CODEINTEL_SERVE_ADDR`, ...), never literal credentials.
> - The fixture repo `services/agent-memory/internal/repoindexer/ast/testdata/polyglot/`
>   (one source file per supported language; introduced in
>   implementation-plan Stage 1.2) is the shared reference
>   workload referenced across phases.

---

# Phase 1: Parser coverage verification

### Setup
- **Type**: inline
- **Local**: From `services/agent-memory/`, run `make test-cgo`
  (requires `CGO_ENABLED=1` and a C toolchain on PATH --
  `gcc`/`clang` on Linux/macOS, `mingw-w64` on Windows) and
  `make test-nocgo` (no toolchain needed) per implementation-plan
  Stage 1.1; both targets exist verbatim in
  `services/agent-memory/Makefile` (`test-cgo` runs
  `CGO_ENABLED=1 go test ./...`, `test-nocgo` runs
  `CGO_ENABLED=0 go test ./...`). The fuller live-pwsh PowerShell
  parse path additionally requires `pwsh` on PATH (per tech-spec
  C2); the `pwsh_not_available` skip-path coverage is provided
  in-tree by `TestPowerShellParser_NoPwsh_ReturnsSentinel` in
  `services/agent-memory/internal/repoindexer/ast/parser_powershell_test.go`
  (line 34) which constructs a `powershellParser{pwshBin: ""}`
  directly to force the empty-bin branch without needing PATH
  surgery, and by `TestDispatcher_ErrParserUnavailable_LogsSkip`
  in `dispatcher_pass2bd_test.go` (line 49) for the dispatcher
  side -- both run inside the existing `make test-nocgo` /
  `make test-cgo` invocations (no separate Makefile target).
- **CI runner**: GitHub-hosted `ubuntu-latest` for the Linux CGO
  matrix (`apt-get install -y build-essential powershell` from a
  cached step) and `windows-latest` for the Windows CGO matrix
  (`choco install mingw` + `choco install powershell-core` from a
  cached step). Both matrix legs run inside the existing
  `.github/workflows/agent-memory-ci.yml` workflow under labels
  `ubuntu-latest`, `windows-latest`. No self-hosted runner.
- **Secrets**: none -- this phase reads no remote services and
  produces no artifacts that require signing.
- **Pre-test bootstrap**:
  - `cd services/agent-memory`
  - `make test-cgo` (asserts `go env CGO_ENABLED` prints `1`)
  - `make test-nocgo`
  - On Windows: `pwsh -Command "Get-Command pwsh"` to confirm the
    PowerShell parser will not return `ErrParserUnavailable`.

### Scenarios
- **Scenario:** cgo-build-passes-all-languages
  - **Given** a checkout of `services/agent-memory/` on a host
    with `gcc` on PATH and `CGO_ENABLED=1`.
  - **When** `make test-cgo` runs.
  - **Then** the test binaries under
    `internal/repoindexer/ast/parser_treesitter_{c,cpp,csharp,go,rust}_test.go`,
    `parser_powershell_test.go`, `parser_python_test.go`, and
    `parser_typescript_test.go` all exit 0.
  - **And** the Makefile prints `CGO_ENABLED=1` to stdout so CI
    log scraping can pin the build mode (impl-plan Stage 1.1).

- **Scenario:** nocgo-build-registers-only-powershell
  - **Given** the same checkout with `CGO_ENABLED=0`.
  - **When** `make test-nocgo` runs.
  - **Then** the suite passes with only the PowerShell parser
    registered, and `parser_treesitter_*_test.go` files are
    excluded via the `//go:build cgo` tag.
  - **And** running `go test ./internal/repoindexer/ast/...` with
    a fixture containing `.c` / `.cpp` / `.cs` / `.go` / `.rs`
    files emits `ast.dispatch.skip{reason:"no_parser"}` for each
    extension (impl-plan Stage 1.1).

- **Scenario:** polyglot-smoke-every-language-emits-class-and-method
  - **Given** the fixture directory
    `internal/repoindexer/ast/testdata/polyglot/` containing one
    file per supported language with a class/type, a free
    function, a same-file call, and one import (impl-plan Stage
    1.2).
  - **When** `parsers_polyglot_smoke_test.go` runs under
    `CGO_ENABLED=1` with `pwsh` on PATH.
  - **Then** each of the eight languages produces at least one
    `class` (or type) Node and at least one `method` Node in the
    in-memory `NodeEdgeWriter` stub.
  - **And** every language whose fixture contains a same-file
    call emits at least one `static_calls` Edge.

- **Scenario:** polyglot-smoke-skips-degrade-cleanly-on-nocgo
  - **Given** the same fixture and `CGO_ENABLED=0`.
  - **When** `parsers_polyglot_smoke_test.go` runs.
  - **Then** only the PowerShell row asserts non-zero
    Node/Edge output.
  - **And** the five tree-sitter rows call `t.Skip` keyed on the
    sentinel error `ErrParserUnavailable` rather than failing the
    test (impl-plan Stage 1.2 scenario `polyglot-smoke-nocgo-degraded`).

- **Scenario:** pwsh-missing-marks-powershell-skip
  - **Given** a host with `pwsh` absent from PATH and the
    PowerShell fixture `polyglot/sample.ps1`.
  - **When** the dispatcher invokes `parser_powershell.go` on the
    fixture.
  - **Then** the parser returns `ErrParserUnavailable` with the
    embedded reason slug `pwsh_not_available`.
  - **And** the dispatcher emits `ast.dispatch.skip{reason:"pwsh_not_available"}`
    rather than a `parse.error` event (tech-spec C2; impl-plan
    Stage 1.2 scenario `pwsh-missing-skip`).

- **Scenario:** coverage-doc-rows-cover-every-language
  - **Given** the file
    `services/agent-memory/internal/repoindexer/ast/COVERAGE.md`
    written in implementation-plan Stage 1.3.
  - **When** a Markdown structure test reads the table.
  - **Then** all eight language rows are present
    (TypeScript/JavaScript, Python, C, C++, C#, Go, Rust,
    PowerShell) and each row names its source `parser_*.go`
    file.

- **Edge:** ci-matrix-publishes-coverage-degradation-summary
  - **Given** a PR that touches `parsers_cgo.go`.
  - **When** the `agent-memory-ci.yml` workflow runs both matrix
    legs (CGO=1 and CGO=0) and uploads `coverage-summary.txt`.
  - **Then** the artifact lists per-extension parse counts so a
    reviewer can spot a language regression without re-running
    the suite locally.

---

# Phase 2: Identity and ancestry refactor

### Setup
- **Type**: inline
- **Local**: From `services/agent-memory/`, run `go test
  ./pkg/fingerprint/... ./internal/repoindexer/...`. No external
  services; sqlmock covers the `graphwriter.EnsureRepoWithID`
  unit test added in implementation-plan Stage 3.2 (the test
  file lives in this phase's RepoID extension and is referenced
  here for completeness).
- **CI runner**: GitHub-hosted `ubuntu-latest`, label
  `ubuntu-latest`. The job inherits the existing
  `.github/workflows/agent-memory-ci.yml` go-test step; no
  toolchain additions.
- **Secrets**: none -- pure-Go tests with no I/O outside the
  process and no network calls.
- **Pre-test bootstrap**: `go mod download` (cached by the
  workflow); no docker.

### Scenarios
- **Scenario:** repoid-deterministic-for-same-url
  - **Given** the new `fingerprint.RepoIDFromURL` helper
    (implementation-plan Stage 2.1).
  - **When** `RepoIDFromURL("https://github.com/foo/bar")` is
    invoked twice from the same process AND once from a fresh
    process.
  - **Then** all three returned `RepoID` values are byte-identical
    (the helper is a pure `uuid.NewSHA1(namespaceRepoURL, url)`).

- **Scenario:** repoid-differs-by-one-character
  - **Given** the URLs `https://github.com/foo/bar` and
    `https://github.com/foo/baz`.
  - **When** both are passed through `RepoIDFromURL`.
  - **Then** the returned `RepoID` values differ (impl-plan
    Stage 2.1 scenario `different-urls-diverge`).

- **Scenario:** repoid-rejects-empty-url
  - **Given** an empty string.
  - **When** `RepoIDFromURL("")` is called.
  - **Then** the call returns a non-nil error and the `RepoID`
    is the zero value (impl-plan Stage 2.1).

- **Scenario:** canonical-helpers-byte-stable-after-export
  - **Given** the four canonical-signature helpers promoted from
    `worker.go` to `internal/repoindexer/canonical.go`
    (implementation-plan Stage 2.2).
  - **When** the new `CanonicalRepoSig` / `CanonicalPackageDir` /
    `CanonicalPackageSig` / `CanonicalFileSig` are called with
    the test vectors restated in `canonical_test.go`.
  - **Then** the returned strings match the byte output of the
    previously-internal helpers verbatim.
  - **And** `grep -RIn "canonicalRepoSig\|canonicalPackageDir\|canonicalPackageSig\|canonicalFileSig" services/agent-memory/internal/`
    returns zero hits (impl-plan Stage 2.4 scenario
    `helpers-no-internal-callers`).

- **Scenario:** ancestrywriter-ensures-repo-and-commit-exactly-once
  - **Given** a fresh `AncestryWriter` constructed via
    `NewAncestryWriter(fakeSink, "file:///tmp/x", "abc")` and a
    walk of 100 files (implementation-plan Stage 2.3).
  - **When** `EnsureRepoAndCommit(ctx, "", nil)` is called
    followed by 100 `EnsureFile(ctx, walkFile)` calls.
  - **Then** the fake sink records exactly one `EnsureRepo`, one
    `EnsureCommit`, and one `InsertNode(kind=repo)` call -- all
    of which complete before the first `EnsureFile`.

- **Scenario:** ancestrywriter-package-cache-deduplicates
  - **Given** an `AncestryWriter` and 50 walk files all under
    `internal/foo/`.
  - **When** `EnsureFile` runs once per file.
  - **Then** `InsertNode(kind=package)` is invoked exactly once
    AND `InsertEdge(kind=contains, src=repoNodeID,
    dst=packageNodeID)` is invoked exactly once (impl-plan Stage
    2.3 scenario `package-deduped`).

- **Scenario:** ancestrywriter-file-and-contains-emitted-per-walkfile
  - **Given** a workspace of 7 distinct files across two
    packages.
  - **When** `EnsureFile` runs once per file.
  - **Then** 7 `file` Nodes and 7 `package -> file` `contains`
    Edges are inserted.

- **Scenario:** ancestrywriter-ensurefile-before-ensurerepo-errors
  - **Given** a fresh `AncestryWriter` with no prior
    `EnsureRepoAndCommit` call.
  - **When** `EnsureFile(ctx, walkFile)` is invoked.
  - **Then** a non-nil error is returned and no `InsertNode`
    occurs (impl-plan Stage 2.3 scenario
    `ensurefile-before-ensurerepo-errors`).

- **Scenario:** worker-refactor-preserves-on-disk-graph
  - **Given** an in-memory fixture repo and a recording
    `RepoCommitNodeEdgeWriter` (the Stage 2.3 interface) that
    captures every node/edge the writer is asked to insert, plus
    a committed golden fixture of the tuples captured from the
    PRE-refactor implementation.
  - **When** the post-refactor `worker.runFull` runs through
    `AncestryWriter` against that fixture (implementation-plan
    Stage 2.4).
  - **Then** the captured `(canonical_signature, kind,
    parent_node_id, fingerprint_hex)` tuples for every `node`
    row are byte-identical to the golden fixture
    (`worker-graph-byte-identical`). Proven IN-PROCESS with NO
    Postgres; the live Postgres `worker_integration_test.go`
    stays a CI regression guard, not a gate requirement.

- **Edge:** ancestrywriter-rejects-cross-repo-walkfile
  - **Given** an `AncestryWriter` bound to repo URL
    `file:///tmp/a` and a `WalkFile` whose synthesized parent
    package falls outside the cached repo root.
  - **When** `EnsureFile` is invoked.
  - **Then** the writer surfaces a typed error before any sink
    call (guards against test-only misuse where two
    materialisers share one writer).

---

# Phase 3: graphsink storage abstraction

### Setup
- **Type**: compose
- **Local**: `docker compose -f
  tests/e2e/phase-3-graphsink-storage-abstraction/docker-compose.yml
  up -d` brings up Postgres 16 (matching the production schema
  range from `migrations/0001_enums.sql` through
  `migrations/0022_edge_kind_overrides.sql` -- the current tree
  HEAD); then `cd services/agent-memory && go test
  ./internal/graphsink/... -tags integration` exercises all
  three sinks. The integration test fixtures apply the schema
  programmatically via `migrations.New(db).Up(ctx)` (the same
  helper used by `internal/graphwriter/writer_integration_test.go`
  line 107 and ~20 other `*_integration_test.go` files in this
  service); no shell-level `psql -f` step is required because
  the repo has no consolidated `all.sql` artifact. Unit-only
  runs (no docker) work with `go test ./internal/graphsink/...
  -short`.
- **CI runner**: GitHub-hosted `ubuntu-latest` (the existing
  `agent-memory-ci.yml` workflow already runs docker compose for
  the queue worker's integration tests). Job uses the standard
  `docker-compose` action; no self-hosted runner.
- **Secrets**: none. All Postgres connection details (host,
  port, role, password, database) are read by the test process
  from the single env var `AGENT_MEMORY_PG_URL`; no literal
  credentials appear in test source or this doc. The compose
  file at `tests/e2e/phase-3-graphsink-storage-abstraction/docker-compose.yml`
  defines an isolated local-only Postgres service whose
  credentials are set by the compose env block (in-file, not
  exported), and the CI workflow injects the matching
  `AGENT_MEMORY_PG_URL` via the standard
  `agent-memory-ci.yml` job-env step.
- **Pre-test bootstrap**:
  - `docker compose -f
    tests/e2e/phase-3-graphsink-storage-abstraction/docker-compose.yml
    up -d` (services started: `postgres`).
  - Wait for the compose `postgres` healthcheck to report
    `healthy` (the existing
    `tests/e2e/phase-09-audit-wal/docker-compose.yml`
    healthcheck pattern -- `pg_isready` with 5s interval -- is
    reused verbatim).
  - `go test -tags integration ./internal/graphsink/...`
    (the test `TestMain` calls `migrations.New(db).Up(ctx)` to
    apply the full migration set against the empty schema; no
    separate migration shell step exists or is needed).

### Scenarios
- **Scenario:** sink-interface-satisfied-by-stub
  - **Given** a `type stubSink struct{}` with all six interface
    methods (`EnsureRepo`, `EnsureCommit`, `InsertNode`,
    `InsertEdge`, `Flush`, `Close`) (impl-plan Stage 3.1).
  - **When** `go vet ./internal/graphsink/...` runs.
  - **Then** the assignment `var _ graphsink.Sink = stubSink{}`
    compiles, confirming the interface stays satisfied.

- **Scenario:** graphwriter-still-satisfies-narrow-writer
  - **Given** the unchanged `*graphwriter.Writer` and the Phase 2
    `repoindexer.RepoCommitNodeEdgeWriter` interface.
  - **When** a compile-time assertion `var _
    repoindexer.RepoCommitNodeEdgeWriter = (*graphwriter.Writer)(nil)`
    is added to a test file.
  - **Then** the assignment compiles without modifications,
    proving the Phase 2 interface stays satisfied by the real
    production writer (impl-plan Stage 3.1 scenario
    `graphwriter-still-satisfies-narrow-writer`).

- **Scenario:** ensurerepowithid-inserts-precomputed-uuid
  - **Given** an empty `repo` table on the live Postgres
    instance reachable via `AGENT_MEMORY_PG_URL` and a non-zero
    `RepoInput.RepoID` derived from
    `fingerprint.RepoIDFromURL("file:///tmp/r")` (impl-plan
    Stage 3.2).
  - **When** `EnsureRepoWithID(ctx, input)` runs.
  - **Then** the inserted row's `repo_id` column equals the
    supplied UUID.
  - **And** a follow-up `SELECT` confirms the row was created via
    `INSERT ... ON CONFLICT (url) DO UPDATE` (not via a
    schema-default UUID).

- **Scenario:** ensurerepo-with-zero-id-falls-back-to-gen-random-uuid
  - **Given** a zero-value `RepoInput.RepoID` and the existing
    `EnsureRepo` (legacy path) -- impl-plan Stage 3.2 scenario
    `ensurerepo-zero-id-uses-default`.
  - **When** the legacy path runs.
  - **Then** the row's `repo_id` is non-zero and was assigned by
    the schema's `gen_random_uuid()` default
    (`migrations/0002_repo_commit.sql:32`).

- **Scenario:** url-collision-returns-existing-repo-id
  - **Given** an existing row with URL `https://x/y` and
    `repo_id = A` written by the legacy path.
  - **When** `EnsureRepoWithID(ctx, input)` runs with the same
    URL and precomputed `repo_id = B != A`.
  - **Then** the returned `RepoRecord.RepoID` equals `A`
    (architecture S3.4 caveat; impl-plan Stage 3.2 scenario
    `url-collision-returns-existing`).
  - **And** a structured log line under op
    `graphwriter.ensurerepowithid.parity_gap` records the
    mismatch so the legacy-data exception is observable.

- **Scenario:** postgres-adapter-has-no-database-sql-import
  - **Given** the package `internal/graphsink/postgres/` after
    implementation-plan Stage 3.3 ships.
  - **When** `go list -deps -f '{{join .Deps "\n"}}'
    ./internal/graphsink/postgres/...` runs.
  - **Then** `database/sql` does NOT appear in the dependency
    list (proves the C5 / S4.5 thin-forwarder invariant -- all
    SQL lives in `graphwriter` or `graphreader`; impl-plan Stage
    3.3 scenario `postgres-adapter-no-database-sql-import`).

- **Scenario:** write-contract-violation-propagates-verbatim
  - **Given** a sqlmock-backed `*graphwriter.Writer` configured
    to return SQLSTATE 42501 (insufficient privilege) on
    `INSERT INTO node`.
  - **When** `InsertNode` runs via the Postgres sink adapter.
  - **Then** the returned error is the typed
    `graphwriter.WriteContractViolation` and the user-facing
    message names the missing grant on `agent_memory_app`
    (tech-spec C5; impl-plan Stage 3.3).

- **Scenario:** postgres-listrepos-matches-mgmtapi-semantics
  - **Given** the live Postgres instance with three `repo` rows
    inserted in known order and a `repo_commit` per repo.
  - **When** both `graphreader.Reader.ListRepos(ctx, opts)` and
    the existing `mgmtapi.handleListRepos` handler run against
    the same data (impl-plan Stage 3.3 scenario
    `graphreader-listrepos-matches-mgmtapi`).
  - **Then** the two return identical ordered
    `[]graphreader.RepoSummary` slices.
  - **And** the ordering is `created_at DESC` matching the
    existing SELECT at `internal/mgmtapi/read.go:816`.

- **Scenario:** sqlite-schema-bootstraps-on-open
  - **Given** a fresh `.db` file path under a temp dir (impl-plan
    Stage 3.4).
  - **When** `sqlite.Open(path)` runs.
  - **Then** the file exists on disk AND `sqlite_master` lists
    `repo`, `repo_commit`, `node`, `edge` tables.
  - **And** the file is opened with the `mattn/go-sqlite3` driver
    (operator-pinned 2026-05-30) under a `//go:build cgo` tag,
    so a CGO=0 build refuses to compile the package.

- **Scenario:** sqlite-rejects-zero-repoid-on-ensurerepo
  - **Given** a `RepoInput` whose `RepoID` field is the zero
    value (impl-plan Stage 3.4 scenario
    `sqlite-requires-precomputed-repoid`).
  - **When** `sqlite.Sink.EnsureRepo` is invoked.
  - **Then** a typed construction error is returned without
    touching the database.

- **Scenario:** sqlite-enum-check-rejects-bogus-kind
  - **Given** an `InsertNode` call with `Kind="bogus"`.
  - **When** the SQLite backend's CHECK-constraint fires.
  - **Then** the SQL error is propagated, no row is inserted,
    and the error message names the closed node-kind set per
    `migrations/0001_enums.sql`.

- **Scenario:** memory-sink-roundtrip-via-loadexport
  - **Given** a memory-backend scan that wrote its export to
    `export.json` (impl-plan Stage 3.5).
  - **When** `memory.LoadExport(path)` rehydrates the data.
  - **Then** the resulting `graphsink.Reader` returns identical
    Node and Edge counts as the original scan, and the JSON
    file's top-level key order is `repo`, `nodes`, `edges`
    (architecture S3.2.4).

- **Scenario:** sqlite-maxlistlimit-clamp-surfaces-in-log
  - **Given** 15 000 `method` Nodes inserted into the SQLite
    backend (impl-plan Stage 3.6).
  - **When** `ListNodes(kinds=["method"], Limit=20000)` runs.
  - **Then** exactly 10 000 rows are returned AND a structured
    log line records the clamp under op
    `graphsink.sqlite.list_nodes.clamped` so downstream
    `truncated`/`stats.cappedAt` accounting (architecture S6) is
    auditable.

- **Scenario:** memory-and-sqlite-readers-return-parity
  - **Given** the same fixture graph inserted into both the
    memory sink and the SQLite sink (impl-plan Stage 3.7).
  - **When** identical `ListNodes` / `ListEdgesFrom` /
    `ListEdgesTo` queries run against both readers.
  - **Then** the returned slices have identical lengths and
    identical sorted `(kind, canonical_signature)` projections.

- **Scenario:** backend-parity-three-backends-fingerprints-match
  - **Given** the polyglot fixture
    `internal/repoindexer/ast/testdata/polyglot/`.
  - **When** the dispatcher runs against each backend in turn --
    Postgres (via the live compose instance), SQLite (temp
    file), and memory -- per impl-plan Stage 3.8.
  - **Then** the sorted `(kind, canonical_signature,
    fingerprint_hex)` lines for the `node` and `edge` tables
    match across all three backends (architecture S2, S6.5
    parity).

- **Scenario:** backend-parity-flags-legacy-postgres-row-as-known-exception
  - **Given** a `repo` row pre-existing in Postgres with a
    schema-default random `repo_id` (legacy mgmt-api ingestion;
    architecture S3.4 caveat).
  - **When** the parity test compares the Postgres row to a
    fresh SQLite scan of the same URL.
  - **Then** the test classifies the diff as `legacy_data`
    (not a regression) and reports the gap in a JSON sidecar
    rather than failing the suite (impl-plan Stage 3.8 scenario
    `legacy-postgres-documented-exception`).

- **Edge:** sqlite-rotation-prior-file-moved-aside
  - **Given** an existing `repo.db` at the target path and a
    second `codeintel scan ... --store sqlite --out repo.db`
    invocation (architecture S3.2.3 rotation policy).
  - **When** the SQLite backend opens the path.
  - **Then** the prior file is moved to
    `repo.db.<RFC3339>.bak` (tech-spec S8.2 future) and the new
    scan starts against a fresh database; the rotation target
    path appears in the CLI summary.

---

# Phase 4: Local materializer and SHA synthesis

### Setup
- **Type**: inline
- **Local**: From `services/agent-memory/`, run `go test
  ./pkg/fingerprint/... ./internal/repoindexer/...`. The
  `local_dir_materializer_test.go` exercises `git rev-parse
  HEAD` so `git` must be on PATH (already required by
  `GitMaterializer`); on Windows the test uses `git.exe`
  detected via `exec.LookPath`.
- **CI runner**: GitHub-hosted `ubuntu-latest` and
  `windows-latest` (matrix) under `.github/workflows/agent-memory-ci.yml`.
  Both runners ship with `git` pre-installed.
- **Secrets**: none.
- **Pre-test bootstrap**: `git --version` (sanity check); no
  docker.

### Scenarios
- **Scenario:** mtimetreesha-stable-on-noop-walk
  - **Given** a populated directory tree under a temp dir
    (impl-plan Stage 4.1).
  - **When** `MTimeTreeSHA(dir, defaultExcludeDirs)` is invoked
    twice without any file changes between calls.
  - **Then** both calls return the identical 32-char lowercase
    hex string (architecture S4.3).

- **Scenario:** mtimetreesha-changes-when-any-mtime-moves
  - **Given** a tree where one file's mtime is updated via
    `os.Chtimes` between calls.
  - **When** `MTimeTreeSHA` is recomputed after the bump.
  - **Then** the returned string differs from the pre-bump value
    (impl-plan Stage 4.1 scenario `mtime-bump-changes-hash`).

- **Scenario:** mtimetreesha-honors-default-excludes
  - **Given** a directory containing `.git/` and
    `node_modules/`.
  - **When** `MTimeTreeSHA(root, defaultExcludeDirs)` runs and
    is compared to a re-hash after both excluded directories
    are physically deleted.
  - **Then** the two hash strings are byte-identical, proving
    no excluded file contributed to the digest.

- **Scenario:** mtimetreesha-returns-error-on-missing-root
  - **Given** a path that does not exist on disk.
  - **When** `MTimeTreeSHA(missing, nil)` runs.
  - **Then** a non-nil error is returned AND the returned hash
    string is empty (impl-plan Stage 4.1 scenario
    `missing-root-errors`).

- **Scenario:** local-non-git-directory-synthesizes-mtime-sha
  - **Given** a directory without a `.git/` subdirectory.
  - **When** `LocalDirMaterializer.Materialize("file:///path",
    "")` runs (impl-plan Stage 4.2).
  - **Then** the returned `Workspace.SHA` equals
    `MTimeTreeSHA(path, defaultExcludeDirs)`.
  - **And** the returned `Workspace.URL` equals `file://`
    followed by the absolute path with forward slashes and a
    lower-cased drive letter on Windows.

- **Scenario:** local-git-directory-synthesizes-rev-parse-head
  - **Given** a directory that is a git checkout with a known
    commit on `HEAD`.
  - **When** `Materialize(url, "")` runs.
  - **Then** `Workspace.SHA` equals the output of `git
    rev-parse HEAD` against that directory.

- **Scenario:** operator-supplied-sha-overrides-both-paths
  - **Given** a git checkout and an operator-supplied non-empty
    `sha` argument.
  - **When** `Materialize(url, "abc123")` runs.
  - **Then** `Workspace.SHA` equals `"abc123"` AND `git
    rev-parse` is NOT invoked (asserted via a test wrapper that
    counts subprocess calls; impl-plan Stage 4.2 scenario
    `operator-sha-override`).

- **Scenario:** walk-skips-default-exclude-directories
  - **Given** a directory tree containing files inside `.git/`,
    `node_modules/`, `vendor/`, `target/`, `bin/`, `obj/`, and
    `__pycache__/`.
  - **When** `Workspace.Walk(fn)` runs.
  - **Then** zero `WalkFile` values originate inside any of
    those directories.

- **Edge:** local-windows-drive-letter-lowercased
  - **Given** an absolute Windows path
    `C:\Users\me\code\repo`.
  - **When** `LocalDirMaterializer.Materialize` synthesizes the
    repo URL.
  - **Then** the URL begins with `file:///c:/Users/me/code/repo`
    (drive letter lowercased per architecture S4.3) so the same
    checkout on a peer's lowercase-mounted filesystem produces
    the same `RepoID`.

---

# Phase 5: codeintel CLI binary

### Setup
- **Type**: inline
- **Local**: `cd services/agent-memory && go build -o
  ./bin/codeintel ./cmd/codeintel && ./bin/codeintel scan
  ./internal/repoindexer/ast/testdata/polyglot --store sqlite
  --out /tmp/polyglot.db`. End-to-end tests live alongside the
  source files (`cmd/codeintel/scan_test.go`,
  `cmd/codeintel/scan_many_test.go`, `cmd/codeintel/summary_test.go`).
- **CI runner**: GitHub-hosted `ubuntu-latest` for the Linux
  CGO build matrix and `windows-latest` for the Windows CGO
  build matrix (both required because the CLI defaults to the
  SQLite backend, which inherits the CGO requirement from
  `mattn/go-sqlite3` per impl-plan Stage 3.4). Workflow:
  `.github/workflows/agent-memory-ci.yml`.
- **Secrets**: none -- the CLI does not read remote services
  unless `--with-embeddings` is set (tested separately in
  Stage 5.5 with the stub embedder, which requires only an env
  var, not a secret).
- **Pre-test bootstrap**:
  - `go build -o ./bin/codeintel ./cmd/codeintel`
  - `mkdir -p ./scans` (for `scan-many` test runs)
  - On Windows: `where pwsh.exe` to confirm PowerShell coverage
    is not silently dropped.

### Scenarios
- **Scenario:** cli-help-lists-five-subcommands
  - **Given** a built `codeintel` binary (impl-plan Stage 5.1).
  - **When** `codeintel --help` runs.
  - **Then** the printed help text names `scan`, `scan-many`,
    `diagram`, `serve`, and `version` (architecture S3.1).

- **Scenario:** unknown-subcommand-exits-nonzero
  - **Given** the same binary.
  - **When** `codeintel bogus` runs.
  - **Then** the exit code is non-zero AND stderr names the
    offending subcommand string `bogus` (impl-plan Stage 5.1
    scenario `unknown-subcommand-errors`).

- **Scenario:** log-json-flag-emits-valid-json-records
  - **Given** any subcommand invoked with `--log=json`.
  - **When** the dispatcher emits a `ast.dispatch.skip` log
    event.
  - **Then** stdout contains one JSON object per line, each
    parseable by `encoding/json` and each carrying the
    `level`, `time`, `msg`, and skip-reason fields.

- **Scenario:** scan-local-sqlite-writes-graph-and-summary
  - **Given** the polyglot fixture and the command
    `codeintel scan
    ./internal/repoindexer/ast/testdata/polyglot --store sqlite
    --out polyglot.db` (impl-plan Stage 5.2).
  - **When** the command runs to completion.
  - **Then** `polyglot.db` exists, contains at least one
    `repo`, `package`, `file`, `class`, and `method` Node, and
    the stdout summary lists non-zero counts for each kind.

- **Scenario:** scan-url-with-sha-fetches-and-indexes
  - **Given** a remote git URL pinned to a known SHA reachable
    from the CI runner (the fixture URL is set via the env var
    `CODEINTEL_FIXTURE_REPO_URL` to keep the literal out of the
    test source).
  - **When** `codeintel scan $CODEINTEL_FIXTURE_REPO_URL
    --sha $CODEINTEL_FIXTURE_REPO_SHA --store sqlite --out
    remote.db` runs.
  - **Then** the GitMaterializer fetches the commit, the
    resulting graph is non-empty, and the SQLite file is
    written to the supplied path.

- **Scenario:** scan-degraded-coverage-exits-zero
  - **Given** a fixture containing `.c` files and a CGO=0 build
    of `codeintel` (impl-plan Stage 5.2 scenario
    `scan-coverage-degraded-exit-zero`).
  - **When** the scan runs.
  - **Then** the exit code is 0, the stdout summary reports
    `skipped.no_parser >= 1`, AND stderr contains the
    per-extension counts so the operator notices the gap
    (tech-spec C7).

- **Scenario:** scan-fatal-io-error-exits-nonzero
  - **Given** an unwritable `--out` path
    (`/nonexistent/dir/foo.db`).
  - **When** `codeintel scan ./fixture --out
    /nonexistent/dir/foo.db` runs.
  - **Then** the exit code is non-zero AND stderr names the
    underlying `open: no such file or directory` error
    (impl-plan Stage 5.2).

- **Scenario:** scan-many-three-repos-writes-one-db-each
  - **Given** a manifest file with three valid entries (one
    `file://` path, two `<url>@<sha>` lines) and the command
    `codeintel scan-many manifest.txt --out-dir ./scans`
    (impl-plan Stage 5.3).
  - **When** the command runs.
  - **Then** three `.db` files appear under `./scans/`
    (per-repo-file policy from architecture S9.4) AND the
    aggregate summary reports `succeeded=3, failed=0`.

- **Scenario:** scan-many-partial-failure-continues-loop
  - **Given** a three-entry manifest where the middle line is an
    invalid git URL.
  - **When** `codeintel scan-many manifest.txt --out-dir
    ./scans` runs.
  - **Then** two `.db` files are produced (first and third
    entries), the failed entry has a `failed: <reason>` line in
    the summary, and the exit code is non-zero per tech-spec
    S4.3 (impl-plan Stage 5.3 scenario `scan-many-partial-failure`).

- **Scenario:** scan-many-skips-comments-and-blank-lines
  - **Given** a manifest containing `# comment` lines and
    blank lines interleaved with valid entries.
  - **When** the parser runs.
  - **Then** the comment and blank lines yield zero scan
    iterations and only the valid entries are processed
    (impl-plan Stage 5.3 scenario `manifest-comments-skipped`).

- **Scenario:** summary-text-format-lists-six-sections
  - **Given** a successful local scan.
  - **When** the text-format summary prints to stdout
    (impl-plan Stage 5.4).
  - **Then** the output contains six lines beginning with
    `walked:`, `parsed:`, `nodes:`, `edges:`, `skipped:`,
    and `duration:` (architecture S7.5).

- **Scenario:** summary-json-format-machine-parseable
  - **Given** the same scan invoked with `--log=json`.
  - **When** the summary writes the final line.
  - **Then** that line is a single JSON object with the keys
    `walked, parsed, nodes, edges, skipped, duration` and
    parses cleanly via `json.Unmarshal` (impl-plan Stage 5.4).

- **Scenario:** summary-skipped-aggregated-by-extension
  - **Given** a CGO=0 scan of a fixture with 3 `.c` files and 1
    `.cs` file.
  - **When** the summary renders.
  - **Then** the `skipped.no_parser_by_ext` map equals
    `{".c": 3, ".cs": 1}` so the operator sees per-language
    impact at a glance.

- **Scenario:** default-scan-does-not-construct-embedder
  - **Given** a `codeintel scan ./fixture --store sqlite`
    invocation without `--with-embeddings` (impl-plan Stage
    5.5).
  - **When** the dispatcher is constructed.
  - **Then** `ast.WithEmbeddingPublisher` is NOT in the option
    chain (asserted via a test-only reflection helper) AND
    `AGENT_MEMORY_QDRANT_URL` is never read from the
    environment (tech-spec C8).

- **Scenario:** with-embeddings-flag-uses-stub-embedder
  - **Given** the env var
    `AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true` and `codeintel
    scan ./fixture --with-embeddings --store sqlite`.
  - **When** the scan runs.
  - **Then** the dispatcher's embedding publisher receives at
    least one `EmbeddingItem` for a `method` Node (impl-plan
    Stage 5.5 scenario `opt-in-stub-embedder`).

- **Scenario:** with-embeddings-without-config-exits-nonzero
  - **Given** `--with-embeddings` set without the stub-allow
    env var and without a real embedder URL.
  - **When** the scan runs.
  - **Then** the exit code is non-zero AND stderr names the
    missing embedder configuration (impl-plan Stage 5.5
    scenario `opt-in-no-stub-fails`).

- **Edge:** scan-respects-language-hints-flag
  - **Given** a TypeScript fixture and `codeintel scan ./ts
    --lang-hints=typescript --store sqlite`.
  - **When** the scan runs.
  - **Then** the dispatcher receives `LanguageHints:
    ["typescript"]` in every `EmitFileEvent` (architecture
    S5.2) AND the summary lists only TypeScript Nodes for the
    relevant files.

---

# Phase 6: Diagram projector

### Setup
- **Type**: inline
- **Local**: `cd services/agent-memory && go test
  ./internal/diagram/...`. The CLI sub-subcommands
  (`codeintel diagram module|calls`) are exercised against a
  pre-built `polyglot.db` fixture written by `codeintel scan`
  during the test setup. No external services -- the SQLite
  backend is local-only.
- **CI runner**: GitHub-hosted `ubuntu-latest` and
  `windows-latest` matrix under `agent-memory-ci.yml`. CGO=1 is
  required (inherited from the SQLite reader's
  `mattn/go-sqlite3` dependency).
- **Secrets**: none.
- **Pre-test bootstrap**:
  - `go build -o ./bin/codeintel ./cmd/codeintel`
  - `./bin/codeintel scan
    ./internal/repoindexer/ast/testdata/polyglot --store sqlite
    --out ./testdata/polyglot.db`
  - `go test ./internal/diagram/...`

### Scenarios
- **Scenario:** envelope-marshal-matches-golden-key-order
  - **Given** a populated `diagram.Diagram` value (impl-plan
    Stage 6.1).
  - **When** `json.Marshal` runs on it.
  - **Then** the resulting bytes match the stored golden file
    `internal/diagram/testdata/envelope_golden.json` byte-for-byte
    -- key order is `diagram, repo, generatedAt, layoutHint,
    nodes, edges, truncated, stats` (architecture S4.4).

- **Scenario:** envelope-roundtrip-stable
  - **Given** marshalled envelope bytes (impl-plan Stage 6.1).
  - **When** the bytes are unmarshalled into a `Diagram` struct
    and re-marshalled.
  - **Then** the second pass equals the first byte-for-byte
    (proves no lossy field mapping).

- **Scenario:** module-tree-anchored-at-repo-node
  - **Given** a fixture with 1 repo, 2 packages, and 5 files
    (impl-plan Stage 6.2).
  - **When** `BuildModuleDiagram(reader, repoID,
    granularity="package")` runs.
  - **Then** the diagram has 3 nodes (1 repo + 2 packages) and
    2 `contains` edges (repo -> each package).

- **Scenario:** module-imports-rollup-aggregates-and-weights
  - **Given** 3 files under `pkg/a/` each importing 2 files
    under `pkg/b/` (impl-plan Stage 6.2 scenario
    `imports-rollup-weight`).
  - **When** the projector runs at `granularity="package"`.
  - **Then** exactly one `imports` edge `pkg:a -> pkg:b` is
    emitted with `weight = 6` and synthetic node ids of the form
    `pkg:<canonical_signature>` (architecture S4.4).

- **Scenario:** module-granularity-file-disables-rollup
  - **Given** the same fixture and `granularity="file"`.
  - **When** the projector runs.
  - **Then** per-file Nodes appear in the diagram AND the
    `imports` edges are NOT rolled up to packages (impl-plan
    Stage 6.2 scenario `granularity-file`).

- **Scenario:** callchain-callees-only-correct-cardinality
  - **Given** a method `M` with 2 callees and 3 callers
    (impl-plan Stage 6.3).
  - **When** `BuildCallChain(reader, seed=M, depth=1,
    direction="callees")` runs.
  - **Then** the envelope contains 3 nodes (M + 2 callees) and
    2 edges with `kind="static_calls"`.

- **Scenario:** callchain-both-directions-correct-cardinality
  - **Given** the same fixture and `direction="both"`.
  - **When** `BuildCallChain` runs.
  - **Then** 6 nodes (M + 2 callees + 3 callers) and 5 edges
    are emitted.

- **Scenario:** callchain-depth-bounded
  - **Given** a chain `A -> B -> C -> D`.
  - **When** `BuildCallChain(seed=A, depth=2,
    direction="callees")` runs.
  - **Then** nodes `A`, `B`, `C` appear in the envelope AND `D`
    does not (impl-plan Stage 6.3 scenario `bfs-depth-two`).

- **Scenario:** callchain-unresolved-seed-returns-error-envelope
  - **Given** a `seed` value that matches no Node via
    `LookupBySignature` and is not a valid UUID.
  - **When** `BuildCallChain` runs.
  - **Then** the function returns an error with code
    `seed_not_found` AND the diagram envelope is the zero value
    (impl-plan Stage 6.3 scenario `seed-unresolved-error`).

- **Scenario:** truncation-under-cap-not-flagged
  - **Given** a graph that produces 50 nodes + 50 edges with
    builder cap=10000 (impl-plan Stage 6.4 scenario
    `under-cap-not-truncated`).
  - **When** either builder runs.
  - **Then** the envelope has `truncated=false` AND
    `stats.nodeCount + stats.edgeCount == 100`.

- **Scenario:** truncation-at-cap-clips-and-flags
  - **Given** a graph that would produce 15 000 elements with
    cap=10000.
  - **When** the builder runs.
  - **Then** `truncated=true`, `stats.cappedAt=10000`, AND the
    summed slice lengths equal exactly 10 000 (architecture
    S6/S7.3).

- **Scenario:** stats-skipped-mirrors-scan-summary
  - **Given** a backend whose `Reader` reports 3 `no_parser`
    skips for `.c` files (impl-plan Stage 6.4 scenario
    `stats-skipped-populated`).
  - **When** the projector emits the diagram envelope.
  - **Then** `stats.skipped.no_parser == 3`.

- **Scenario:** diagram-module-cli-writes-file
  - **Given** a populated `repo.db` and the command
    `codeintel diagram module --db repo.db --out module.json`
    (impl-plan Stage 6.5).
  - **When** the command runs.
  - **Then** `module.json` exists, parses as the envelope, AND
    has `diagram="module"`.

- **Scenario:** diagram-calls-cli-streams-to-stdout
  - **Given** a populated DB and the command `codeintel
    diagram calls --db repo.db --seed <sig> --depth 1 --out -`.
  - **When** the command runs.
  - **Then** stdout contains a single valid envelope with
    `diagram="callchain"` and the exit code is 0.

- **Scenario:** diagram-from-export-matches-live-backend
  - **Given** a memory-backend JSON export file written in
    Phase 3 AND a live SQLite backend populated from the same
    fixture (impl-plan Stage 6.5).
  - **When** `codeintel diagram module --from-export
    export.json` and `codeintel diagram module --db repo.db`
    both run.
  - **Then** the two envelopes have identical sorted
    `(node.id, node.kind)` and `(edge.from, edge.to,
    edge.kind)` projections.

- **Edge:** module-diagram-orphan-files-still-attached
  - **Given** a repo where one file has no detectable
    `imports` edges in either direction.
  - **When** `BuildModuleDiagram(granularity="file")` runs.
  - **Then** the orphan file still appears under its owning
    package via `contains` so the UI tree is complete.

---

# Phase 7: Serve endpoint

### Setup
- **Type**: compose
- **Local**: `docker compose -f
  tests/e2e/phase-7-serve-endpoint/docker-compose.yml up -d`
  brings up Postgres 16 (for `--store=postgres` parity, schema
  range `migrations/0001_enums.sql` through
  `migrations/0022_edge_kind_overrides.sql` -- the current tree
  HEAD) and a `codeintel-serve` container running `codeintel
  serve --store sqlite --db /data/polyglot.db --addr :8088`.
  The Postgres schema is applied programmatically by the test
  process via `migrations.New(db).Up(ctx)` (the helper used
  across the existing `*_integration_test.go` suite -- e.g.
  `internal/mgmtapi/read_integration_test.go` line 118); the
  repo has no consolidated `all.sql` artifact. Then `cd
  services/agent-memory && go test ./cmd/codeintel/... -tags
  integration -run TestServe`. Unit-only runs (httptest, no
  docker) use `go test ./cmd/codeintel/... -short -run
  TestServe`.
- **CI runner**: GitHub-hosted `ubuntu-latest` (the existing
  `agent-memory-ci.yml` workflow already exercises docker
  compose for the queue worker). Labels: `ubuntu-latest`. No
  self-hosted runner.
- **Secrets**: none. All connection details (Postgres host,
  port, role, password, database; serve bind address; CORS dev
  origin) are read from env vars in test source and never
  appear as literals: `AGENT_MEMORY_PG_URL` (Postgres DSN),
  `CODEINTEL_DB_PATH` (SQLite path mounted into the
  `codeintel-serve` container), `CODEINTEL_SERVE_ADDR` (HTTP
  bind), `CODEINTEL_CORS_ORIGIN` (pinned to
  `http://localhost:5173` per architecture S9.2 and tech-spec
  S6.3). Postgres credentials live exclusively inside the
  compose service's env block; the CI workflow injects the
  matching `AGENT_MEMORY_PG_URL` via the existing
  `agent-memory-ci.yml` job-env step.
- **Pre-test bootstrap**:
  - `docker compose -f
    tests/e2e/phase-7-serve-endpoint/docker-compose.yml up -d`
    (services started: `postgres`, `codeintel-serve`).
  - Wait for the compose `postgres` healthcheck (`pg_isready`,
    5s interval -- reusing the pattern from
    `tests/e2e/phase-09-audit-wal/docker-compose.yml`).
  - `./bin/codeintel scan
    ./internal/repoindexer/ast/testdata/polyglot --store sqlite
    --out "$CODEINTEL_DB_PATH"` (populates the SQLite file
    mounted at the path the `codeintel-serve` container
    reads).
  - For the Postgres-backed scenarios: `go test -tags
    integration -run TestServePostgres ./cmd/codeintel/...`
    (the test `TestMain` calls `migrations.New(db).Up(ctx)`
    before the first scenario runs; no separate migration
    shell step exists or is needed).
  - Wait for `curl -sf
    "http://localhost:${CODEINTEL_SERVE_ADDR##*:}/api/repos"`
    to succeed before invoking the SQLite-backed test suite.

### Scenarios
- **Scenario:** serve-binds-on-supplied-addr
  - **Given** the command `codeintel serve --addr :18088
    --store sqlite --db repo.db` (impl-plan Stage 7.1).
  - **When** the server starts and a `GET /` is issued.
  - **Then** the bind succeeds (port 18088 listening) AND the
    response is 404 because no root handler is registered.

- **Scenario:** cors-preflight-allows-vite-dev-origin
  - **Given** a preflight `OPTIONS /api/repos` with `Origin:
    http://localhost:5173` (impl-plan Stage 7.1).
  - **When** the CORS middleware processes the request.
  - **Then** the response sets `Access-Control-Allow-Origin:
    http://localhost:5173` (architecture S9.2; tech-spec S6.3).

- **Scenario:** cors-preflight-rejects-foreign-origin
  - **Given** `Origin: https://evil.example` on the same
    preflight.
  - **When** the middleware runs.
  - **Then** no `Access-Control-Allow-Origin` header is set
    AND the response status is 403 (impl-plan Stage 7.1
    scenario `cors-rejects-foreign-origin`).

- **Scenario:** cors-flag-rejects-wildcard
  - **Given** the command `codeintel serve --cors-origin '*'`.
  - **When** the server starts.
  - **Then** the startup exits non-zero and stderr names the
    "exact-match origins only" constraint (architecture S7.4).

- **Scenario:** repos-handler-returns-single-repo-from-sqlite
  - **Given** a `repo.db` with one scanned repo (impl-plan
    Stage 7.2).
  - **When** `GET /api/repos` runs against the serve endpoint.
  - **Then** the response is a single-element JSON array
    containing the expected `id`, `url`, `sha`, and
    `generatedAt`.
  - **And** the response `Content-Type` is `application/json`.

- **Scenario:** repos-handler-returns-empty-array-not-null
  - **Given** a backend with zero scanned repos.
  - **When** `GET /api/repos` runs.
  - **Then** the response body is the JSON literal `[]` (not
    `null`) AND the `Content-Type` is `application/json`
    (impl-plan Stage 7.2 scenario `repos-empty-returns-empty-array`).

- **Scenario:** repos-handler-multi-repo-via-postgres
  - **Given** the compose Postgres instance reached via
    `AGENT_MEMORY_PG_URL` with three repos scanned via
    `--store=postgres`.
  - **When** `GET /api/repos` runs against `codeintel serve
    --store postgres`.
  - **Then** the response array length is 3, the order matches
    `created_at DESC`, and the URLs match those scanned
    (impl-plan Stage 7.2 scenario `repos-postgres-multi`).

- **Scenario:** module-handler-returns-envelope
  - **Given** a populated backend (impl-plan Stage 7.3).
  - **When** `GET /api/diagram/module?repo=<id>&granularity=package`
    runs.
  - **Then** the response status is 200, `Content-Type` is
    `application/json`, AND the body parses as the diagram
    envelope with `diagram="module"`.

- **Scenario:** module-handler-rejects-bogus-granularity
  - **Given** the query parameter `granularity=bogus`
    (architecture S9.5 closed set).
  - **When** `GET /api/diagram/module?repo=<id>&granularity=bogus`
    runs.
  - **Then** the response status is 400 AND the body lists the
    valid values `package, file, class` (impl-plan Stage 7.3
    scenario `module-handler-bad-granularity-400`).

- **Scenario:** calls-handler-returns-envelope-for-known-seed
  - **Given** a populated backend and a known method
    signature (impl-plan Stage 7.4).
  - **When** `GET /api/diagram/calls?repo=<id>&seed=<sig>&depth=2&direction=both`
    runs.
  - **Then** the response status is 200 AND the envelope's
    `diagram` field equals `"callchain"`.

- **Scenario:** calls-handler-seed-not-found-returns-404
  - **Given** a `seed` value that resolves to no Node.
  - **When** the handler runs.
  - **Then** the response status is 404 AND the body equals
    `{"error":"seed_not_found"}` (impl-plan Stage 7.4 scenario
    `calls-handler-seed-not-found-404`).

- **Scenario:** calls-handler-depth-cap-rejects-large-values
  - **Given** `depth=999` (impl-plan Stage 7.4 scenario
    `calls-handler-depth-clamped`).
  - **When** the handler runs.
  - **Then** the response status is 400 AND the error body
    names the cap value `10` so the operator knows the limit.

- **Scenario:** mgmtapi-routes-404-when-disabled
  - **Given** `mgmt-api` started with
    `Options.EnableDiagramRoutes=false` (impl-plan Stage 7.5).
  - **When** `GET /api/diagram/module` hits the handler.
  - **Then** the response status is 404.

- **Scenario:** mgmtapi-routes-200-when-enabled
  - **Given** `mgmt-api` started with
    `Options.EnableDiagramRoutes=true` and the same Postgres
    backend.
  - **When** the same request runs.
  - **Then** the response status is 200 AND the body parses as
    the diagram envelope (impl-plan Stage 7.5 scenario
    `mgmtapi-routes-enabled`).

- **Scenario:** disallowed-http-method-returns-405
  - **Given** any of the three routes.
  - **When** the client sends a `POST` or `PUT`.
  - **Then** the response status is 405 AND the
    `Allow: GET, OPTIONS` header is set (impl-plan Stage 7.1
    middleware behaviour).

- **Edge:** serve-truncation-flag-surfaces-in-response
  - **Given** a backend whose diagram exceeds the 10 000 cap
    (architecture S6 / tech-spec C6).
  - **When** `GET /api/diagram/module` runs.
  - **Then** the response body sets `truncated=true` AND
    `stats.cappedAt=10000` so the UI's badge can fire without
    a separate signal.

---

# Phase 8: React and neo4j-nvl web UI

### Setup
- **Type**: inline
- **Local**: `cd services/agent-memory/web && npm install`,
  then `npm run dev` (Vite serves on `:5173`) and in a separate
  shell `cd ../ && ./bin/codeintel serve --store sqlite --db
  ./testdata/polyglot.db --addr :8088`. `npm run test` invokes
  Vitest for unit tests; `npm run test:e2e` invokes Playwright
  (Chromium-only project pinned in `playwright.config.ts`) for
  the full UI walk.
- **CI runner**: GitHub-hosted `ubuntu-latest` with Node 20
  (`actions/setup-node@v4`); the existing
  `agent-memory-ci.yml` workflow gains a new `web` job. The
  Playwright dependency installs its Chromium browser via
  `npx playwright install --with-deps chromium` in the
  bootstrap step. No self-hosted runner.
- **Secrets**: none -- the dev server proxies to `:8088` on
  loopback; no external network calls. The proxy target is set
  via the env var `VITE_API_BASE_URL=http://localhost:8088`
  (no literal hostnames in source).
- **Pre-test bootstrap**:
  - `cd services/agent-memory && go build -o ./bin/codeintel
    ./cmd/codeintel`
  - `./bin/codeintel scan
    ./internal/repoindexer/ast/testdata/polyglot --store sqlite
    --out ./web/testdata/polyglot.db`
  - `./bin/codeintel serve --store sqlite --db
    ./web/testdata/polyglot.db --addr :8088 &`
  - `cd web && npm install && npx playwright install --with-deps
    chromium`
  - `npm run build` (Vite production build) before `npm run
    test:e2e`.

### Scenarios
- **Scenario:** web-vite-build-succeeds
  - **Given** a fresh `npm install` under
    `services/agent-memory/web/` (impl-plan Stage 8.1).
  - **When** `npm run build` runs.
  - **Then** the build exits 0 AND produces `dist/index.html`.

- **Scenario:** web-dev-server-proxies-api
  - **Given** `npm run dev` running with `codeintel serve` on
    `:8088`.
  - **When** the browser loads
    `http://localhost:5173/api/repos`.
  - **Then** the response is proxied through Vite and equals
    the serve endpoint's `GET /api/repos` body
    (impl-plan Stage 8.1 scenario `web-dev-server-proxies`).

- **Scenario:** ts-types-match-envelope-via-tsc
  - **Given** the TS interfaces in `web/src/api/types.ts` and
    the Go envelope structs in `internal/diagram/envelope.go`
    (impl-plan Stage 8.2).
  - **When** `npx tsc --noEmit` runs.
  - **Then** the type-check passes AND a CI side-test parses a
    real envelope JSON from the serve endpoint into the TS
    interfaces with no missing-field errors.

- **Scenario:** api-client-rejects-404-with-typed-error
  - **Given** a `GET /api/diagram/calls` that returns 404 with
    `{"error":"seed_not_found"}`.
  - **When** `fetchCallChain(repoId, seed, depth, direction)`
    runs (impl-plan Stage 8.2).
  - **Then** the promise rejects with an `Error` whose message
    contains `seed_not_found`.

- **Scenario:** repopicker-renders-three-options
  - **Given** a mocked `fetchRepos()` returning three repos
    (impl-plan Stage 8.3).
  - **When** `<RepoPicker>` mounts.
  - **Then** the `<select>` element contains exactly three
    `<option>` children with the correct labels.

- **Scenario:** diagramswitch-updates-url-hash
  - **Given** the user clicks the "Call Chain" button on
    `<DiagramSwitch>`.
  - **When** the test inspects `window.location.hash`.
  - **Then** the hash contains `mode=callchain` (impl-plan
    Stage 8.3 scenario `diagramswitch-persists-mode`).

- **Scenario:** nvl-canvas-renders-module-envelope
  - **Given** a fixture envelope with 5 nodes and 4 edges
    (impl-plan Stage 8.4).
  - **When** `<NvlCanvas>` mounts with the envelope.
  - **Then** the rendered NVL canvas contains 5 nodes and 4
    relationships (asserted via the wrapper's
    `getDataAdapter` API or DOM count).

- **Scenario:** nvl-canvas-honors-layout-hint
  - **Given** an envelope with
    `layoutHint="hierarchical-left-right"` (impl-plan Stage
    8.4 scenario `layouthint-honored`).
  - **When** the component re-renders.
  - **Then** the `nvlOptions.layout` prop equals the
    hierarchical direction-right value (architecture S4.4.1
    mapping).

- **Scenario:** observed-calls-edges-styled-dashed-grey
  - **Given** an envelope containing one `observed_calls`
    edge.
  - **When** the canvas renders.
  - **Then** that relationship's `color` prop is grey AND its
    `style` includes the dashed-line marker (architecture
    S4.4.1 styling rule).

- **Scenario:** inspector-shows-attrs-on-selection
  - **Given** a node-click selection on the canvas (impl-plan
    Stage 8.5).
  - **When** the canvas dispatches the selection event.
  - **Then** the `<Inspector>` panel becomes visible AND
    renders the node's `attrs.language`,
    `attrs.modifiers`, `attrs.startLine`, `attrs.endLine`.

- **Scenario:** inspector-hides-on-deselect
  - **Given** an Inspector already showing a selected node.
  - **When** the user clicks empty canvas space.
  - **Then** the Inspector hides via CSS `display:none` or
    component-returns-null (impl-plan Stage 8.5).

- **Scenario:** inspector-shows-calls-raw-for-unresolved-calls
  - **Given** a node with `attrs.calls_raw=["foo","bar"]`
    (tech-spec R3 mitigation; same-file resolution only).
  - **When** `<Inspector>` renders.
  - **Then** both raw strings appear in the DOM so the user
    sees what the parser saw pre-resolver (impl-plan Stage 8.5
    scenario `inspector-shows-calls-raw`).

- **Scenario:** call-chain-click-reseeds-bfs
  - **Given** a rendered call-chain diagram in
    `mode=callchain` (impl-plan Stage 8.6).
  - **When** the user clicks node `X`.
  - **Then** `fetchCallChain(repoId, X.id, depth, direction)`
    is invoked AND the canvas re-renders with the new
    envelope.

- **Scenario:** call-chain-hash-updates-on-reseed
  - **Given** the click above.
  - **When** the test reads `window.location.hash`.
  - **Then** the hash contains `seed=<X.id>` so the view is
    shareable (impl-plan Stage 8.6 scenario
    `hash-updates-on-reseed`).

- **Scenario:** call-chain-back-button-restores-previous-seed
  - **Given** two consecutive re-seeds (A -> B -> C).
  - **When** the user navigates back once.
  - **Then** `fetchCallChain` is invoked with `seed=B.id` AND
    the canvas reflects the matching envelope.

- **Scenario:** truncation-badge-renders-when-flag-set
  - **Given** an envelope with `truncated=true` and
    `stats.cappedAt=10000` (impl-plan Stage 8.7).
  - **When** `<App>` renders.
  - **Then** the text "Results clipped at 10000" appears above
    the canvas in the `<TruncationBadge>` component.

- **Scenario:** truncation-badge-hidden-when-not-truncated
  - **Given** `truncated=false`.
  - **When** `<App>` renders.
  - **Then** the badge is absent from the DOM (impl-plan Stage
    8.7).

- **Scenario:** skip-banner-warns-when-no-parser-skips-occur
  - **Given** an envelope with
    `stats.skipped.no_parser=3` (impl-plan Stage 8.7
    scenario `skip-banner-shows`).
  - **When** `<App>` renders.
  - **Then** a yellow banner appears with the text "3 files
    skipped (no parser; needs CGO build)" (architecture S7;
    tech-spec C7).

- **Scenario:** skip-banner-warns-when-pwsh-missing
  - **Given** an envelope with
    `stats.skipped.pwsh_not_available=2`.
  - **When** `<App>` renders.
  - **Then** a banner appears naming the missing `pwsh` on
    PATH prerequisite (tech-spec C2).

- **Edge:** ui-survives-empty-envelope
  - **Given** a backend with one repo but no Class/Method
    Nodes (e.g. a Markdown-only repo).
  - **When** the user switches to Module mode.
  - **Then** the canvas renders the repo + package
    containment tree without throwing, AND the Inspector and
    truncation banner stay hidden.

---

# Phase 9: Docs and shipping

### Setup
- **Type**: inline
- **Local**: `cd services/agent-memory && go test -tags e2e
  ./cmd/codeintel/...` for the Go e2e walk; `cd web && npm run
  test:e2e` for the Playwright walk that loads the served URL
  end-to-end and writes the reference screenshots. Both runs
  start `codeintel serve` on an ephemeral port chosen by the
  test harness (no fixed port collisions).
- **CI runner**: GitHub-hosted `ubuntu-latest` for the
  Linux/CGO matrix and `windows-latest` for the Windows/CGO
  matrix, both under `.github/workflows/agent-memory-ci.yml`.
  Playwright runs only on `ubuntu-latest` (the Chromium-only
  project pin keeps cross-OS browser cost low per impl-plan
  Stage 9.3). No self-hosted runner.
- **Secrets**: none -- the e2e walk runs entirely on the
  runner against a temp SQLite file written from the repo's
  own `services/agent-memory/` source tree.
- **Pre-test bootstrap**:
  - `go build -o ./bin/codeintel ./cmd/codeintel`
  - `npm install` and `npx playwright install --with-deps
    chromium` under `services/agent-memory/web/`
  - For the docs-link-check stage: `npx markdown-link-check
    services/agent-memory/README.md` and
    `services/agent-memory/cmd/codeintel/QUICKSTART.md`.

### Scenarios
- **Scenario:** quickstart-walkthrough-runs-clean
  - **Given** a fresh clone of the repo with a working CGO
    toolchain and `pwsh` on PATH (impl-plan Stage 9.1).
  - **When** the operator runs every command listed in
    `cmd/codeintel/QUICKSTART.md` in order (build, scan, serve,
    `npm run dev`, `curl /api/repos`).
  - **Then** every command exits 0 AND the final `curl
    http://localhost:8088/api/repos` returns a single-element
    JSON array naming the scanned repo (impl-plan Stage 9.1
    scenario `quickstart-commands-valid`).

- **Scenario:** readme-links-resolve
  - **Given** the updated `services/agent-memory/README.md`
    referencing `QUICKSTART.md` and `web/README.md`.
  - **When** `npx markdown-link-check
    services/agent-memory/README.md` runs.
  - **Then** all new links resolve (impl-plan Stage 9.1
    scenario `readme-links-resolve`).

- **Scenario:** context-usage-doc-lists-codeintel-examples
  - **Given** the updated `.claude/context/usage.md` (impl-plan
    Stage 9.2).
  - **When** a grep test searches for `codeintel`.
  - **Then** at least three command examples appear (`scan`,
    `diagram`, `serve` minimum).

- **Scenario:** context-structure-doc-lists-new-packages
  - **Given** the updated `.claude/context/structure.md`
    (impl-plan Stage 9.2 scenario
    `structure-doc-lists-new-packages`).
  - **When** a grep test searches for `graphsink`, `diagram`,
    `codeintel`, and `web`.
  - **Then** each substring returns at least one hit.

- **Scenario:** context-tests-doc-records-language-matrix
  - **Given** the updated `.claude/context/tests.md`.
  - **When** a grep test searches for the polyglot language
    list (`typescript`, `python`, `c`, `cpp`, `csharp`, `go`,
    `rust`, `powershell`).
  - **Then** each language appears at least once with its CGO
    and `pwsh` requirements documented (tech-spec C1, C2).

- **Scenario:** e2e-walk-scans-serves-and-fetches
  - **Given** a clean checkout (impl-plan Stage 9.3 scenario
    `e2e-scan-serve-fetch`).
  - **When** the Go e2e test runs:
    1. Scan `services/agent-memory/` to a temp SQLite file.
    2. Start `codeintel serve` on an ephemeral port (logged via
       env var `CODEINTEL_SERVE_ADDR`).
    3. Fetch `GET /api/repos`, `GET /api/diagram/module`,
       `GET /api/diagram/calls?seed=<sig>&depth=2`.
  - **Then** every response is 200 AND the body has a non-empty
    `nodes` array.

- **Scenario:** playwright-walk-renders-both-diagrams
  - **Given** the served URL above and a Playwright test
    runner (impl-plan Stage 9.3 scenario `e2e-ui-renders-both`).
  - **When** the test loads the URL, picks the scanned repo,
    and switches between Module and Call Chain modes.
  - **Then** each mode renders at least one node on the NVL
    canvas (asserted via the wrapper's data adapter).

- **Scenario:** playwright-walk-captures-reference-screenshots
  - **Given** the same Playwright walk reaches the rendered
    canvas (impl-plan Stage 9.3 scenario
    `e2e-screenshots-committed`).
  - **When** `await page.screenshot({ path: "..." })` runs for
    each diagram mode.
  - **Then** `services/agent-memory/web/docs/screenshots/module.png`
    and `call-chain.png` both exist with byte size > 0 AND
    `web/README.md` references both filenames via standard
    `![alt](docs/screenshots/...png)` markdown.

- **Scenario:** acceptance-criteria-fully-exercised
  - **Given** the architecture S8.1-S8.5 acceptance criteria
    (impl-plan Stage 9.3 scenario `e2e-acceptance-criteria`).
  - **When** the combined Go + Playwright walk finishes.
  - **Then** every bullet in S8.1 (parser coverage), S8.2 (CLI
    scanning), S8.3 (diagram generation), and S8.4 (React UI)
    is exercised by at least one step in the walk (architecture
    S8 is the contract; this scenario asserts coverage).

- **Edge:** quickstart-on-windows-without-cgo-still-runs-degraded
  - **Given** a Windows runner without a CGO toolchain
    installed AND the QUICKSTART invoked with
    `codeintel scan ... --store memory --out graph.json`
    (architecture S9.1 default for the no-CGO path; memory
    sink has no CGO dependency per impl-plan Stage 3.5).
  - **When** the QUICKSTART steps run end-to-end against the
    polyglot fixture (`scan --store=memory` -> static JSON ->
    open `web/` against the exported JSON -> see both
    diagrams).
  - **Then** the scan summary lists
    `stats.skipped.no_parser >= 1` for `.c/.cpp/.cs/.go/.rs`
    (tree-sitter parsers are CGO-only per tech-spec C7) but
    the QUICKSTART completes successfully because (a) the
    `--store=memory` sink has no native dependency, and (b)
    the web UI renders the exported JSON's `SkipBanner` to
    name the missing prereq (impl-plan Stage 8.x
    `SkipBanner.tsx`).
  - **And** the same QUICKSTART invoked with
    `--store=postgres --pg-url $AGENT_MEMORY_PG_URL` against
    a docker-compose Postgres also completes in degraded
    mode -- the postgres sink does not depend on CGO either
    (`graphwriter` uses `database/sql` + `lib/pq`, not
    `mattn/go-sqlite3`).

- **Edge:** quickstart-with-sqlite-store-without-cgo-fails-at-build
  - **Given** the same Windows runner without a CGO toolchain
    AND an operator who explicitly asks for the SQLite store
    via `--store=sqlite` (or who tries to `go build
    -tags '!cgo' ./cmd/codeintel`).
  - **When** the build step runs.
  - **Then** the build fails fast with a clear compile error
    that names the missing `cgo` tag, NOT a runtime
    error at scan time (impl-plan Stage 3.4 scenario
    `sqlite-requires-cgo`: "`go vet -tags '!cgo'
    ./internal/graphsink/sqlite/...` runs ... the package
    fails to compile with a build-tag error naming the
    missing `cgo` tag"). This proves the operator-pinned
    `mattn/go-sqlite3` choice is enforced at compile time
    rather than producing a confusing runtime failure deep
    inside a scan.
  - **And** the QUICKSTART docs (architecture S9.2 Windows
    quickstart section, plus
    `services/agent-memory/web/README.md` per implementation-
    plan Phase 9) carry an explicit callout naming this
    SQLite-needs-CGO constraint and pointing Windows
    operators without a C toolchain to `--store=memory` or
    `--store=postgres` as the supported no-CGO sinks.

---

## Iteration history

- 2026-05-30 (iter 1): initial draft -- nine phases mirroring
  the `implementation-plan.md` ladder; each phase carries a
  `### Setup` block (one of `inline` / `compose`) and a
  Gherkin-style scenario list. Phase 3 and Phase 7 use docker
  compose (file paths `tests/e2e/phase-3-graphsink-storage-abstraction/docker-compose.yml`
  and `tests/e2e/phase-7-serve-endpoint/docker-compose.yml`,
  services `postgres` and `postgres + codeintel-serve`
  respectively). All other phases run on hosted runners
  without external services. Connection strings are
  referenced as env-var names (`AGENT_MEMORY_PG_URL`,
  `CODEINTEL_DB_PATH`, `CODEINTEL_SERVE_ADDR`,
  `CODEINTEL_CORS_ORIGIN`, `VITE_API_BASE_URL`,
  `CODEINTEL_FIXTURE_REPO_URL`, `CODEINTEL_FIXTURE_REPO_SHA`).
  No `lab-*` setup type is used because the story is a
  developer-tool CLI with hosted-runner-friendly toolchain
  needs (CGO + `pwsh`).
- 2026-05-30 (iter 2): addressed both evaluator findings from
  iter 1 (score 86). Item 1 (Phase 3 + Phase 7 compose-bootstrap
  referenced a non-existent `services/agent-memory/migrations/all.sql`
  and the wrong upper bound `0001-0023`) is now FIXED: the
  Phase 3 and Phase 7 setup envelopes name the real migration
  range as `migrations/0001_enums.sql` through
  `migrations/0022_edge_kind_overrides.sql` (the actual current
  tree HEAD), and the shell-level `psql -f all.sql` step is
  replaced by an in-process `migrations.New(db).Up(ctx)`
  invocation from the integration-test `TestMain` -- the same
  helper used by `internal/graphwriter/writer_integration_test.go`
  line 107 and ~20 other `*_integration_test.go` files across
  the agent-memory service. Item 2 (Phase 3 Secrets bullet
  reproduced a `role / password` literal) is FIXED: the bullet
  now references only env-var names
  (`AGENT_MEMORY_PG_URL`) and notes that credential material
  lives exclusively inside the compose service's env block --
  no role / password literals appear in the plan doc. The
  Phase 7 Secrets bullet was re-stated for symmetry to make
  the env-var-only contract explicit there too. Stale snapshot
  file `e2e-scenarios.md.iter-snapshot.bak` (which still held
  the pre-fix phrasing) was removed from the story directory.
- 2026-05-30 (iter 3): addressed both iter-2 evaluator findings
  (score 88). Item 1 (Phase 1 setup cited the non-existent
  `make test-cgo-no-pwsh` Makefile target) is FIXED: the Phase
  1 `### Setup` `Local` bullet (lines 48-65) now cites only
  the real Makefile targets `test-cgo` and `test-nocgo` (both
  verified present in `services/agent-memory/Makefile` lines
  58-70) and routes the `pwsh_not_available` skip-path
  coverage through the existing in-tree test
  `TestPowerShellParser_NoPwsh_ReturnsSentinel`
  (`parser_powershell_test.go` line 34) which forces the
  empty-bin branch in-process and runs under either existing
  make target -- no new Makefile target is invented. Item 2
  (Phase 9 Edge `quickstart-on-windows-without-cgo-still-runs-degraded`
  was internally contradictory: "runs end-to-end in degraded
  mode" vs. "SQLite build fails fast") is FIXED by SPLITTING
  the edge into two coherent scenarios: (a)
  `quickstart-on-windows-without-cgo-still-runs-degraded` now
  pins the QUICKSTART to a non-SQLite backend (`--store=memory`
  per architecture S9.1, with `--store=postgres` as a parallel
  no-CGO sink) and asserts only the parser-degradation
  contract (`stats.skipped.no_parser >= 1` for the CGO-only
  extensions plus the web `SkipBanner`); (b) a new edge
  `quickstart-with-sqlite-store-without-cgo-fails-at-build`
  pins the SQLite-needs-CGO build-tag failure to compile time
  (matching impl-plan Stage 3.4 scenario `sqlite-requires-cgo`
  verbatim: `go vet -tags '!cgo' ./internal/graphsink/sqlite/...`
  fails with a clear cgo-tag error) and references the
  QUICKSTART doc callout pointing Windows operators without a
  C toolchain to `--store=memory` or `--store=postgres`.
