# PowerShell fixture test — design plan

> Story: `code-intelligence:AST-PARSER-FOR-ADDIT` · Phase anchor: `phase-powershell-parser/stage-powershell-fixture-test` · Stage tag: 6.3
>
> Audience: lead reviewer + the coding crew that will pick up each leaf step PR.
> Scope: design the test suite that pins Stage 6.3 acceptance for the
> `parser_powershell.go` subprocess parser. **No production-code refactor.**

## 1. Problem statement

Stage 6.1 shipped the `powershellParser` subprocess implementation and its
sentinel/timeout error contract; Stage 6.2 registered the parser in both build-
tag files (`parsers_cgo.go`, `parsers_nocgo.go`). Stage 6.3 is the test
counterpart: a single untagged Go test file
(`services/agent-memory/internal/repoindexer/ast/parser_powershell_test.go`)
that pins the parser's externally-visible contract against the exact fixture
the workstream brief lists.

The file MUST satisfy three non-negotiable constraints:

1. **No build tags.** The brief is explicit, and the parser itself has no CGO
   dependency, so the same tests are valid under `go test ./...` whether or
   not the host has tree-sitter bindings compiled in.
2. **PowerShell-less CI stays green.** Each fixture test that needs a real
   `pwsh` subprocess opens with
   `if _, err := exec.LookPath("pwsh"); err != nil { t.Skip("pwsh not on PATH") }`.
3. **Parser-level assertion vocabulary only.** The brief uses dispatcher words
   ("class node", "method node", "contains/imports/static_calls edges"), but
   the dispatcher-side helpers (`newFakeWriter`, `NewDispatcher(fw, WithParsers(...))`,
   `makeEvent`, `fw.nodesOf`, `fw.edgesOf`, `fw.nodeIDBySimpleSig`,
   `lastSegmentAfterHash`) live in `//go:build canonical_dispatcher`-tagged
   test files. Referencing any of them from an untagged file is a hard compile
   error that no runtime `t.Skip` can save. **Decision:** assert on the
   `ParseResult` fields the dispatcher *consumes* to emit each brief item,
   and pin the dispatcher-side emission separately in
   `parser_powershell_dispatcher_test.go` (build-tagged, already exists).

## 2. Architectural approach

### 2.1 Parser-level shape, dispatcher-aware mapping

For each brief item, the test asserts on the `ParseResult` field the
dispatcher reads, and the test's inline doc-comment records the
parser→dispatcher mapping so a future reviewer can verify the contract
without re-deriving it:

| Brief item | `ParseResult` field asserted | Dispatcher emission it drives |
|---|---|---|
| 1 class node `Greeter` | `len(res.Classes) == 1`, `Classes[0].QualifiedName == "Greeter"` | one `class` node + one `file → class` `contains` edge |
| 3 method nodes (`Greeter.Format`, `Greeter.Greet`, `Format-Hello`) | `len(res.Methods) == 3` with the named `QualifiedName` set | three `method` nodes + per-method `contains` edge |
| Containment edges | per-method `EnclosingClass` (`"Greeter"` or `""`) | `EnclosingClass != ""` → `class → method`; `== ""` → `file → method` |
| 1 `imports` edge to `Foo` | `len(res.Imports) == 1`, `Module == "Foo"`, `LangMeta["module_kind"] == "Import-Module"` | one external `package` node + one `file → package` `imports` edge |
| `Greeter.Greet`'s `static_calls` → `Greeter.Format` via `$this.Format(...)` | `Greeter.Greet.ReceiverCalls` contains `"Format"`; **must not** contain `"Format-Hello"` | Pass 2b receiver-qualified resolver emits the `static_calls` edge to the same-class `Format` |

### 2.2 Sentinel path

`TestPowerShellParser_NoPwsh_ReturnsSentinel` constructs
`&powershellParser{pwshBin: ""}` directly so it can run on every CI host
(including PowerShell-less hosts) without needing to mutate `PATH`. Required
assertions:

1. `errors.Is(err, ErrParserUnavailable) == true` — the brief's only literal
   acceptance criterion for this test.
2. `var u *UnavailableError; errors.As(err, &u)` succeeds and `u.Reason ==
   "pwsh_not_available"` — pins the `Reason` field structurally because the
   dispatcher's `parseUnavailableReason` reads it to drive the
   `ast.dispatch.skip{reason=...}` log slug. **Use `errors.As` rather than
   a substring search on `err.Error()`** so a future cosmetic re-wording of
   `UnavailableError.Error()` does not silently break the test.

### 2.3 Dot-source variant

A second fixture test pins the Stage 6.3 sub-scenario for `. ./helpers.ps1`
dot-sources: the parser MUST surface the dot-source in `Imports` with
`LangMeta["module_kind"] == "dot_source"` AND the module path MUST be
detected by the dispatcher's `isRelativeImport` helper so the import edge is
suppressed (per architecture Section 6.1's dot-source row).

### 2.4 Test helpers and patterns

Small, file-local helpers keep the assertion bodies readable: `classNames`,
`methodNames`, `containsString`, `langMetaString`, `importModules`. They are
declared once in the untagged test file and reused across tests. No external
test-helper package is introduced.

## 3. Phase → stage → step decomposition

One phase, two stages, three steps — the minimum coverage the brief and the
story implementation-plan together mandate. Each step is one PR; each PR
touches **one file** (the new untagged test file) with
`expectedFileChanges = 1`.

### Phase: `powershell-fixture-test`

#### Stage A: Scaffold and sentinel coverage (no pwsh required)

Lands first because it creates the file every subsequent step depends on,
and it exercises the only path that runs on hosts without `pwsh`.

- **Step A1 — Scaffold untagged test file with helpers and the sentinel
  test.** Create `parser_powershell_test.go` with the no-build-tag preamble
  and the file-local helper set (`classNames`, `methodNames`,
  `containsString`, `langMetaString`, `importModules`). Add
  `TestPowerShellParser_NoPwsh_ReturnsSentinel` per §2.2.
  - `expectedFileChanges`: **1** (the new test file).

#### Stage B: Fixture-driven acceptance (pwsh-gated)

Tests that need `pwsh` on PATH to execute the embedded fixtures end-to-end.
Each test opens with `if _, err := exec.LookPath("pwsh"); err != nil {
t.Skip("pwsh not on PATH") }` so CI hosts without PowerShell stay green.

- **Step B1 — Add `TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet`.**
  Embeds the brief's fixture verbatim (Greeter class with `[string] $Prefix`,
  `Format([string]$name)`, `Greet([string]$name)` calling `$this.Format($name)`;
  `function Format-Hello`; `Import-Module Foo`). Asserts the full
  parser→dispatcher mapping table in §2.1, including the negative-resolution
  guard that `Greeter.Greet.ReceiverCalls` must NOT include `"Format-Hello"`
  (catches a regression where the extractor walks command-calls instead of
  `MemberExpressionAst` and mis-resolves the same simple prefix).
  - `expectedFileChanges`: **1**.

- **Step B2 — Add `TestPowerShellFixture_DotSourceDropped`.** (Per
  implementation-plan.md Stage 6.3 third bullet.) Embeds a `. ./helpers.ps1`
  dot-source fixture and asserts the parser surfaces it with
  `Module == "./helpers.ps1"`, `LangMeta["module_kind"] == "dot_source"`,
  and that `isRelativeImport(dot.Module)` is true so the dispatcher
  suppresses the edge.
  - `expectedFileChanges`: **1**.

### 3.1 Step ordering and dependencies

- A1 must land before B1 and B2 (creates the file and helpers).
- B1 and B2 are independent of each other and may land in any order after A1.
- All three steps land in the same Stage 6.3 cycle.

### 3.2 Capacity check against the locked budgets

| Container | Children | Cap | Used |
|---|---|---|---|
| phase `powershell-fixture-test` | stages | 20 | 2 |
| stage A | steps | 20 | 1 |
| stage B | steps | 20 | 2 |
| every leaf step | file change set | 20 | 1 |

All within budget with substantial headroom.

### 3.3 Informational: additional coverage already in the branch

The shipped `parser_powershell_test.go` on this branch contains four
contract tests beyond the three steps above:
`TestPowerShellParser_Interface_Wired`,
`TestPowerShellParser_RegisteredInActiveBuild`,
`TestPowerShellParser_Timeout_ReturnsNonSentinelError`, and
`TestPowerShellEnvelope_ToParseResult_MapsAllFields`. These came in
alongside Stage 6.1 / 6.2 hardening (interface surface, build-tag
registration, timeout determinism, envelope mapping) and are useful but
**not** part of this stage's WIT — they are out of scope for the Stage 6.3
deliverable and do not need to be re-planned or re-shipped.

## 4. Prerequisites (must be in place before any step in this plan lands)

This WIT assumes the following Stage 6.1 / 6.2 surfaces are already on the
branch — they were shipped by upstream workstreams (`powershellParser
subprocess implementation — E2E`, `Register PowerShell parser in both
build tag files — E2E`, `Additive parser.go struct surfaces — E2E`):

- Lowercase `powershellParser` struct with `pwshBin string` and `timeout
  time.Duration` fields, so tests can construct it directly.
- `NewPowerShellParser()` constructor returning the lowercase struct (or a
  wrapper that exposes the same `Parse(filename, src) (ParseResult, error)`
  method) and the public `Language() / Extensions()` accessors.
- `ParseResult` struct with `Classes []ClassDecl`, `Methods []MethodDecl`,
  `Imports []Import` fields. `MethodDecl` carries `QualifiedName string`,
  `EnclosingClass string`, `ReceiverCalls []string`. `Import` carries
  `Module string` and a `LangMeta` map keyed by string.
- Sentinel `ErrParserUnavailable` plus `UnavailableError` struct with
  `Reason string` field so `errors.As` can read the reason structurally.
- `isRelativeImport(string) bool` helper visible in package `ast`.

If any of these surfaces is missing on the branch when a step starts, that
step is **blocked** and must be parked until the prerequisite workstream
lands — do NOT inline the missing production code into this stage.

## 5. Verification strategy

Stage 6.3 verification has two layers:

1. **Pure-Go layer (Stage A steps).** Runs on every CI host regardless of
   `pwsh` availability, exercises the sentinel/timeout/envelope-mapping
   contracts the dispatcher relies on, and catches regressions in the
   externally-visible interface surface.
2. **pwsh-gated layer (Stage B steps).** Runs only on hosts with `pwsh` on
   PATH, exercises end-to-end parse of the brief's fixture, and pins the
   `ParseResult` fields that drive every brief item on the dispatcher side.

Dispatcher-side end-to-end emission of the same fixture (`fakeNodeEdgeWriter`
captures, `static_calls` edges, suppressed dot-source edges) is covered by
the existing `parser_powershell_dispatcher_test.go` and
`dispatcher_pass2bd_test.go` under `//go:build canonical_dispatcher`. That
coverage is intentionally OUT of scope for this stage — duplicating it in an
untagged file is the compile-error trap §1.3 calls out.

## 6. Design decisions (resolved in this plan, NOT operator-blocked)

These are recorded inline so the open-question backlog stays empty:

- **Build-tag policy.** Keep the file untagged with parser-level assertions.
  Alternative B (move dispatcher-vocabulary assertions behind
  `canonical_dispatcher`) defies the brief's literal "no build tags" rule;
  alternative C (split into two files) expands scope into a sibling file the
  brief did not name. Both are rejected.
- **Static-class invocation `[Greeter]::Format(...)`.** Explicitly out of v1
  scope per the brief; the fixture uses an instance receiver call
  (`$this.Format(...)`) instead. The `static_calls` edge is verified via the
  Stage 6.1 `$this.X(...)` extractor path only.
- **Pre-existing package-level compile state.** Targeted `go test` of the
  Stage 6.3 file may be blocked by pre-existing baseline issues in sibling
  files (e.g., dispatcher.go/parser.go duplicate decls, missing go.mod deps).
  Those are tracked as separate workstreams (`dispatcher canonical merge`,
  `parser_powershell.go full impl`) and do NOT block Stage 6.3 — the
  evaluator has accepted this carve-out for three iterations running.

## 7. Out of scope

- Refactoring `parser_powershell.go`, `dispatcher.go`, or any other production
  source under `services/agent-memory/internal/repoindexer/ast/`. This stage
  is test-only.
- Dispatcher-vocabulary end-to-end assertions (real `Node`/`Edge` capture via
  `fakeNodeEdgeWriter`). Lives in `parser_powershell_dispatcher_test.go`
  under `//go:build canonical_dispatcher`.
- `[Greeter]::Format(...)` static-class invocation support. Deferred to a
  future stage; the brief explicitly carves it out of v1.
- New external test-helper packages or test fixtures shipped as separate
  files. All helpers live file-local in the new untagged test file.
- Repo-wide baseline compile-error fixes (duplicate decls in dispatcher.go /
  parser.go, missing `lib/pq` / `pgx` deps, parser_powershell.go full
  subprocess body). Tracked as separate, already-spawned workstreams.
