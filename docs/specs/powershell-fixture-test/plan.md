# PowerShell fixture test — design plan

> Story: `code-intelligence:AST-PARSER-FOR-ADDIT` · Phase anchor: `phase-powershell-parser/stage-powershell-fixture-test` · Stage tag: 6.3
>
> Audience: lead reviewer + the coding crew that will pick up each leaf step PR.
> Scope: design the test suite that pins Stage 6.3 acceptance for the
> `parser_powershell.go` subprocess parser. **No production-code refactor.**

## 1. Problem statement

Stage 6.1 was *intended* to ship the lowercase `powershellParser` subprocess
implementation with its sentinel/timeout error contract; Stage 6.2 was
intended to register the parser in both build-tag files (`parsers_cgo.go`,
`parsers_nocgo.go`). Stage 6.3 is the test counterpart: a single untagged Go
test file
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

## 1.5. Branch state reality (HEAD as of iter 5)

The Stage 6.1 / 6.2 surfaces the test artifact targets were **partially
reverted** before this plan was written. The plan must be read against the
following ground-truth, not the aspirational target:

| Symbol | What test file references | What HEAD actually has |
|---|---|---|
| Parser struct | `powershellParser` (lowercase, lines 35, 578 of `parser_powershell_test.go`) | `PowerShellParser` (uppercase, `parser_powershell.go` line 7) |
| Timeout field | `&powershellParser{pwshBin, timeout: …}` (line 578) | No `timeout` field; only `pwshBin string` |
| `Parse()` body on `pwsh` present | implied by fixture tests | returns `ParseResult{}, nil` (stub at line 30) |
| `UnavailableError.Error()` format | substring `"reason=pwsh_not_available"` (line 44) | `"pwsh_not_available: parser unavailable"` (`parser.go` line 21 — no `reason=` prefix) |
| `UnavailableError.Reason` field | not referenced via `errors.As` | exists (`parser.go` line 18) — usable via `errors.As` |

Net effect: **the iter-1 test file does not compile against HEAD, and even
if it did, the sentinel substring assertion would fail.** Per operator pin
(§6.5 below), this stage is **not blocked**: the test is accepted as a
forward-looking artifact for when prerequisites land. The plan documents
the gap honestly so a future picker is not misled.

History (per operator OQ text):

- PR #175 added the real ~724-line `powershellParser` implementation
  (lowercase struct, subprocess body, timeout field).
- PR #183 reverted `parser_powershell.go` back to the 32-line stub above
  while keeping the parser registration in both build-tag files.
- The iter-1 commit (`cff1320`) authored the test against the post-#175
  shape, before #183 reverted it.

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

`TestPowerShellParser_NoPwsh_ReturnsSentinel` is the brief-mandated
sentinel test. Required assertions on the returned `(ParseResult, error)`:

1. `errors.Is(err, ErrParserUnavailable) == true` — the brief's only literal
   acceptance criterion.
2. `var u *UnavailableError; errors.As(err, &u)` succeeds and `u.Reason ==
   "pwsh_not_available"` — pins the `Reason` field structurally. **Required
   structural form.** The dispatcher's existing
   `errors.As(err, &ue); reason = ue.Reason` path (`parser.go` lines
   151-154) drives the `ast.dispatch.skip{reason=...}` log slug.
3. **Do NOT use substring search on `err.Error()`.** The current
   `UnavailableError.Error()` returns `Reason + ": parser unavailable"` (no
   `reason=` prefix), so substring assertions like
   `strings.Contains(err.Error(), "reason=pwsh_not_available")` fail
   against HEAD even when the parser type matches. The iter-1 shipped test
   currently uses this fragile form — see Step C1 in §3 for the
   reconciliation step.

**Target construction shape** (against the post-PR-#175 parser):
`&powershellParser{pwshBin: ""}` directly, so the test runs on every CI
host (including PowerShell-less hosts) without needing to mutate `PATH`.
Until PR #175 is restored, this constructor reference is the compile gap
identified in §1.5.

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

#### Stage C: Reconciliation after prerequisites land (currently blocked)

This stage exists because §1.5 documents an honest gap between the iter-1
shipped test file and HEAD. The operator pinned "not blocked — accept test
as artifact" (§6.5), so Stage 6.3 closes without this stage *executing*;
the stage is recorded here so a future picker has a clear pickup point
once PR #175 is restored (or replaced) and the dispatcher canonical merge
lands.

- **Step C1 — Reconcile shipped test file with HEAD after prerequisites.**
  Triggered when both (a) PR #175's lowercase `powershellParser` (with
  `pwshBin` and `timeout` fields) is restored or re-shipped under an
  equivalent constructor, and (b) the dispatcher canonical merge lands so
  the `ast` package compiles cleanly. Edits needed in
  `parser_powershell_test.go`:
  - **Sentinel substring → structural.** Replace
    `strings.Contains(err.Error(), "reason=pwsh_not_available")` (current
    line 44) with `var u *UnavailableError; errors.As(err, &u); u.Reason
    == "pwsh_not_available"`. Justification in §2.2.
  - **Parser construction shape.** If PR #175 restores lowercase
    `powershellParser`, no change needed. If the future shape keeps
    uppercase `PowerShellParser`, rewrite the two `&powershellParser{…}`
    construction sites (lines 35 and 578) to `&PowerShellParser{…}` and
    drop any `timeout` field reference that the future shape doesn't
    expose. Decision deferred to the workstream that restores the parser.
  - **Stub-returns-empty workaround.** While HEAD's `Parse()` returns
    `ParseResult{}, nil` on `pwsh` present, the Greeter and dot-source
    fixture tests will fail (zero classes/methods/imports). Once the real
    subprocess body returns, no test change needed. If the real body is
    not coming back, replace those two fixture tests with a `t.Skip` that
    notes the parser is a stub.
  - **Blocked-on contract.** This step is BLOCKED until both prerequisites
    above land. Marker workstream-context entry:
    `phase-powershell-parser/stage-powershell-fixture-test/step-c1-reconcile`.
  - `expectedFileChanges`: **1** (the same test file).

### 3.1 Step ordering and dependencies

- A1 must land before B1 and B2 (creates the file and helpers).
- B1 and B2 are independent of each other and may land in any order after A1.
- C1 is blocked on **upstream**: PR #175 (or successor) restoring the
  lowercase `powershellParser` with `timeout` field, plus dispatcher
  canonical merge into `feature/memory`. Does NOT block Stage 6.3 closure
  per operator pin §6.5.

### 3.2 Capacity check against the locked budgets

| Container | Children | Cap | Used |
|---|---|---|---|
| phase `powershell-fixture-test` | stages | 20 | 3 |
| stage A | steps | 20 | 1 |
| stage B | steps | 20 | 2 |
| stage C | steps | 20 | 1 |
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

## 4. Prerequisites and current HEAD coverage

The Stage 6.3 test artifact targets the post-PR-#175 shape of
`parser_powershell.go`. The following table records what each target
surface needs vs. what HEAD has today, so a future picker can tell which
prerequisite workstream must land first.

| Target surface (used by test) | HEAD status | Workstream that lands it |
|---|---|---|
| Lowercase `powershellParser{pwshBin, timeout}` | ❌ Reverted by PR #183; only uppercase `PowerShellParser{pwshBin}` exists | PR #175 restoration (or successor `powershellParser subprocess implementation — E2E`) |
| `Parse()` body invoking pwsh subprocess and parsing the envelope | ❌ Stub returns `ParseResult{}, nil` on `pwsh` present | Same as above |
| `ErrParserUnavailable` sentinel | ✅ Present (`parser.go` line 14) | n/a (already on `feature/memory`) |
| `UnavailableError{Reason string}` with `.Is(target) bool` | ✅ Present (`parser.go` lines 17-22) | n/a |
| `NewPowerShellParser()` constructor returning a value with `Parse` | ✅ Present but on `PowerShellParser` (uppercase) | n/a for constructor shape; lowercase variant blocked on PR #175 |
| `ParseResult{Classes, Methods, Imports}` + `MethodDecl{QualifiedName, EnclosingClass, ReceiverCalls}` + `Import{Module, LangMeta}` | Mixed: `ParseResult` itself exists; subfield surfaces depend on `parser.go` additive workstream | `Additive parser.go struct surfaces — E2E` (status: complete per workstream-context dependency list) |
| `isRelativeImport(string) bool` | Depends on dispatcher canonical merge | `dispatcher canonical merge` workstream |
| Package `ast` compiles at HEAD | ❌ Pre-existing duplicate decls in `parser.go` / `dispatcher.go` + missing go.mod deps | `dispatcher canonical merge` workstream (out of scope per operator pin) |

If any unmet prerequisite is needed for a given step in §3, that step is
**blocked** and must be parked until the prerequisite workstream lands —
do NOT inline the missing production code into this stage.

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
  brief did not name. Both are rejected. Confirmed by operator pin §6.5
  (choice **A**).
- **Static-class invocation `[Greeter]::Format(...)`.** Explicitly out of v1
  scope per the brief; the fixture uses an instance receiver call
  (`$this.Format(...)`) instead. The `static_calls` edge is verified via the
  Stage 6.1 `$this.X(...)` extractor path only.
- **Pre-existing package-level compile state.** Targeted `go test` of the
  Stage 6.3 file is currently blocked by pre-existing baseline issues in
  sibling files (`parser.go` / `dispatcher.go` duplicate decls; missing
  go.mod deps `lib/pq`, `pgx`, `otel`). Those are tracked as separate
  workstreams (`dispatcher canonical merge`, `parser_powershell.go full
  impl`) and do NOT block Stage 6.3 closure per operator pin §6.5.

### 6.5 Operator pin record (received iter 5)

The operator's answers on the two iter-1 open questions are recorded here
verbatim so the plan is the durable artefact of the resolution:

- **Q `ps-fixture-test-blocked-on-prod`** — the test file references
  symbols and behaviours that don't exist at HEAD: lowercase
  `powershellParser`, `powershellEnvelope`, `MethodDecl.QualifiedName`,
  plus a 32-line stub `parser_powershell.go` (the 724-line PR #175 impl
  was wiped by PR #183). Should the workstream be marked blocked pending
  upstream merges?
  - **Operator answer: Not blocked — accept test as artifact for when
    prod catches up.**
  - Plan consequence: §1.5 documents the gap honestly; Stage A/B steps
    are the *design* of what the artifact tests; Stage C1 is the future
    reconciliation step that becomes runnable once prerequisites land.

- **Q `ps-fixture-test-build-tag`** — should the file be (A) untagged
  parser-level, (B) `canonical_dispatcher`-tagged dispatcher-level
  rewrite, or (C) split across an untagged parser-level file plus the
  existing dispatcher-tagged sibling?
  - **Operator answer: A — keep parser-level in untagged file (as
    currently shipped).**
  - Plan consequence: §2.1 + §1 constraint #3 codify choice A; choices
    B and C are explicitly rejected; the dispatcher-level capture path
    stays in `parser_powershell_dispatcher_test.go` (already in repo).

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
