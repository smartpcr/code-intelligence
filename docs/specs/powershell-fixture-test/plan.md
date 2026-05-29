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

## 1.5. Branch state reality (HEAD post-iter-6 reconciliation)

The Stage 6.1 / 6.2 surfaces the test artifact targets were **partially
reverted** before this plan was first written (iter 4). The iter-1 test
file referenced symbols that no longer exist at HEAD; **iter 6 reconciled
the test file against HEAD's actual symbol surface** so it COMPILES today.
The table below records the symbol surface used by the **current**
`parser_powershell_test.go` (post-iter-6), with the iter-1 references kept
in a strikethrough column for historical traceability:

| Surface | Iter-1 (pre-iter-6, broken) | Iter-6 reconciled (current) | HEAD source of truth |
|---|---|---|---|
| Parser struct | ~~`powershellParser` (lowercase)~~ | `PowerShellParser` (uppercase) | `parser_powershell.go` line 7 |
| Construction shape | ~~`&powershellParser{pwshBin: ""}` and `{pwshBin: bin, timeout: 1ns}`~~ | `&PowerShellParser{pwshBin: ""}` only (no `timeout` reference; field does not exist on HEAD) | `parser_powershell.go` line 8 |
| Sentinel reason check | ~~`strings.Contains(err.Error(), "reason=pwsh_not_available")`~~ | `var ue *UnavailableError; errors.As(err, &ue) && ue.Reason == "pwsh_not_available"` (structural) | `parser.go` lines 17-22 (`UnavailableError{Reason}`) |
| `ClassDecl` field | ~~`.QualifiedName`, `.Kind`, `.Extends`, `.Implements`~~ | `.Name` only | `parser.go` lines 39-42 |
| `MethodDecl` field | ~~`.QualifiedName`, `.EnclosingClass`, `.ReceiverCalls`, `.MemberAccesses`~~ | `.Name`, `.ClassName`, `.LangMeta map[string]string` | `parser.go` lines 45-49 |
| `Import` field | ~~`.Module`~~ | `.Path`, `.LangMeta map[string]string` | `parser.go` lines 52-55 |
| Receiver-call slug | ~~`MethodDecl.ReceiverCalls []string`~~ | `MethodDecl.LangMeta["receiver_calls"]` (comma-joined member names) | PR #175 production contract |
| `Parse()` body on `pwsh` present | implied by fixture tests | returns `ParseResult{}, nil` (stub) — fixture test intentionally fails on stub | `parser_powershell.go` line 30 |

Net effect after iter 6: **the test file COMPILES against HEAD**. It still
cannot RUN (the ast package as a whole fails to build due to pre-existing
`parser.go`/`dispatcher.go` redeclarations — pre-existing baseline,
unrelated to this workstream), and the Greeter / receiver-call fixture
assertions will fail when `pwsh` is present until PR #175's 724-line real
subprocess implementation is restored (intentional production-gap signal
per operator pin §6.5).

History:

- PR #175 added the real ~724-line `powershellParser` implementation
  (lowercase struct, subprocess body, timeout field).
- PR #183 reverted `parser_powershell.go` back to the 32-line stub above
  while keeping the parser registration in both build-tag files.
- Iter-1 commit (`cff1320`) authored the test against the post-#175
  shape, before #183 reverted it.
- Iter-6 commit reconciled the test file against HEAD's actual symbol
  surface (uppercase `PowerShellParser`, structural `errors.As` sentinel,
  HEAD-shaped `ClassDecl`/`MethodDecl`/`Import` fields, `LangMeta`-based
  receiver-call slug), removed four tests that depended on symbols never
  restored to HEAD, and prefixed file-local helpers `ps*` to avoid
  clashes with cgo-tagged sibling tests.

## 2. Architectural approach

### 2.1 Parser-level shape, dispatcher-aware mapping

For each brief item, the test asserts on the `ParseResult` field the
dispatcher reads, and the test's inline doc-comment records the
parser→dispatcher mapping so a future reviewer can verify the contract
without re-deriving it:

| Brief item | `ParseResult` field asserted (HEAD-shaped, post-iter-6) | Dispatcher emission it drives |
|---|---|---|
| 1 class node `Greeter` | `len(res.Classes) == 1`, `Classes[0].Name == "Greeter"` | one `class` node + one `file → class` `contains` edge |
| 3 method nodes (`Greeter.Format`, `Greeter.Greet`, `Format-Hello`) | `len(res.Methods) == 3`; index by `(ClassName, Name)` tuple: `{"Greeter","Format"}`, `{"Greeter","Greet"}`, `{"","Format-Hello"}` | three `method` nodes + per-method `contains` edge |
| Containment edges | per-method `ClassName` (`"Greeter"` or `""`) | `ClassName != ""` → `class → method`; `== ""` → `file → method` |
| 1 `imports` edge to `Foo` | `len(res.Imports) == 1`, `Path == "Foo"`, `LangMeta["module_kind"] == "Import-Module"` | one external `package` node + one `file → package` `imports` edge |
| `Greeter.Greet`'s `static_calls` → `Greeter.Format` via `$this.Format(...)` | `Greeter.Greet.LangMeta["receiver_calls"]` contains `"Format"`; **must not** contain `"Format-Hello"` | Pass 2b receiver-qualified resolver reads the slug and emits the `static_calls` edge to the same-class `Format` |

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
   against HEAD even when the parser type matches. **Iter-6 reconciled
   the shipped test to use the structural `errors.As(*UnavailableError)`
   form documented in #2 above.**

**Construction shape** (post-iter-6, against HEAD's uppercase parser type):
`&PowerShellParser{pwshBin: ""}` directly (HEAD's exported type with the
unexported `pwshBin` field, accessible from inside `package ast`). The test
runs on every CI host (including PowerShell-less hosts) without needing to
mutate `PATH`. The brief's literal lowercase `&powershellParser{}` reference
is preserved as intent only — the upstream PR #175 lowercase struct would
have to land before that form compiles; iter-6 chose to ship against HEAD
rather than preserve a known compile error.

### 2.3 Dot-source variant (removed in iter 6)

The iter-1 file shipped a `TestPowerShellFixture_DotSourceDropped` test
that asserted on `Import.Module` and `isRelativeImport(...)`. **Iter-6
removed this test** because HEAD's `Import` exposes `Path` (not `Module`),
and forcing the test to compile against the iter-1 shape would re-introduce
a stale reference. The dispatcher-level dot-source-edge-drop coverage of
the same scenario lives in `parser_powershell_dispatcher_test.go`
(canonical_dispatcher-tagged) and in `dispatcher_relative_import_legacy.go`'s
`isRelativeImport` unit coverage — both untouched by this workstream. If
the brief's intent later requires re-shipping the parser-level assertion,
it should be authored against HEAD's `Import.Path` + `LangMeta["module_kind"]
== "dot_source"` shape.

### 2.4 Test helpers and patterns

Small, file-local helpers keep the assertion bodies readable. **Iter 6
renamed them with a `ps*` prefix** to avoid clashes with same-named
helpers in cgo-tagged sibling tests (`parser_treesitter_cpp_test.go` has
`classNames`; `parser_treesitter_go_test.go` has `methodNames` /
`importModules` / `containsString`). The current helper set:
`psClassNames`, `psMethodNames`, `psImportPaths`, `psLangMetaStr`. The
iter-1 helpers `containsMemberAccessName` and `langMetaString` were
removed because they referenced the nonexistent `MemberAccess` type and
mis-typed `LangMeta` as `map[string]any` (it is `map[string]string` on
HEAD). No external test-helper package is introduced.

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
  and the file-local helper set (post-iter-6 names: `psClassNames`,
  `psMethodNames`, `psImportPaths`, `psLangMetaStr`). Add
  `TestPowerShellParser_NoPwsh_ReturnsSentinel` per §2.2.
  - `expectedFileChanges`: **1** (the new test file).
  - **Status (post-iter-6): SHIPPED — reconciled.** The current file uses
    `&PowerShellParser{pwshBin: ""}` (HEAD's uppercase type) and the
    structural `errors.As(*UnavailableError).Reason == "pwsh_not_available"`
    assertion; the iter-1 fragile substring check is gone.

#### Stage B: Fixture-driven acceptance (pwsh-gated)

Tests that need `pwsh` on PATH to execute the embedded fixtures end-to-end.
Each test opens with `if _, err := exec.LookPath("pwsh"); err != nil {
t.Skip("pwsh not on PATH") }` so CI hosts without PowerShell stay green.

- **Step B1 — Add `TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet`.**
  Embeds the brief's fixture verbatim (Greeter class with `[string] $Prefix`,
  `Format([string]$name)`, `Greet([string]$name)` calling `$this.Format($name)`;
  `function Format-Hello`; `Import-Module Foo`). Asserts the full
  parser→dispatcher mapping table in §2.1, including the negative-resolution
  guard that `Greeter.Greet.LangMeta["receiver_calls"]` must NOT include
  `"Format-Hello"` (catches a regression where the extractor walks
  command-calls instead of `MemberExpressionAst` and mis-resolves the same
  simple prefix).
  - `expectedFileChanges`: **1**.
  - **Status (post-iter-6): SHIPPED — reconciled.** Reconciled against
    HEAD's field shape (`ClassDecl.Name`, `MethodDecl.Name`+`ClassName`,
    `Import.Path`, `LangMeta map[string]string`). Receiver-call invariant
    asserted via `LangMeta["receiver_calls"]` slug per the PR #175
    production contract. The test compiles today; assertions still fail
    when `pwsh` is present because HEAD's stub `Parse()` returns an empty
    `ParseResult` — that failure is the production-gap signal per operator
    pin §6.5.

- **Step B2 — `TestPowerShellFixture_DotSourceDropped` (REMOVED in iter 6).**
  The iter-1 file shipped this test, but it referenced `Import.Module`
  which HEAD does not expose (HEAD's `Import` has `.Path`). Iter-6 removed
  the test rather than ship another known-stale reference. See §2.3 for
  the disposition: parser-level dot-source coverage is now considered
  out of scope for this stage; dispatcher-level coverage continues in
  `parser_powershell_dispatcher_test.go` (canonical_dispatcher-tagged).
  - `expectedFileChanges`: **0** (removed).
  - **Status (post-iter-6): WITHDRAWN.**

#### Stage C: Reconciliation after prerequisites land (COMPLETED iter 6)

This stage existed because earlier iters of §1.5 documented an honest gap
between the iter-1 shipped test file and HEAD. **Iter 6 completed the
reconciliation directly** rather than waiting for prerequisites; per
operator pin §6.5, the test is accepted as an artifact even though one
prerequisite (PR #175 restoration) remains unmet, so the prior wording of
"blocked-on-upstream" was misleading. The single remaining production gap
(stub `Parse()` returns empty `ParseResult`) is captured as an *intentional
test failure* per operator pin, not as a step that needs to land.

- **Step C1 — Reconcile shipped test file with HEAD (COMPLETED iter 6).**
  Iter-6 commit reconciled `parser_powershell_test.go` against HEAD's
  actual symbol surface in a single 214+/641- diff:
  - **Sentinel substring → structural.** `strings.Contains(err.Error(),
    "reason=pwsh_not_available")` was replaced with `var u
    *UnavailableError; errors.As(err, &u); u.Reason ==
    "pwsh_not_available"` per §2.2.
  - **Parser construction shape.** `&powershellParser{...}` (lowercase,
    iter-1) was rewritten to `&PowerShellParser{pwshBin: ""}` (HEAD's
    uppercase type, unexported `pwshBin` accessible from inside
    `package ast`). The `timeout` field reference was dropped along with
    `TestPowerShellParser_Timeout_ReturnsNonSentinelError` because HEAD's
    `PowerShellParser` does not expose a timeout field.
  - **Stub-returns-empty disposition.** Iter-6 followed the operator pin
    "accept test as artifact for when prod catches up": kept the Greeter
    fixture assertions strict so the test fails on the stub, signalling
    the production gap. Did NOT replace with `t.Skip` — that would
    silently accept the stub forever (rubber-duck-confirmed).
  - **Stage C1 status: COMPLETED (iter 6).** The test file now compiles
    against HEAD. The only outstanding production-side work is restoring
    PR #175's 724-line subprocess body in `parser_powershell.go`, which
    is tracked by the separate `powershellparser-subprocess-implementation`
    workstream (already in the dependency context, status `complete` on
    its own branch but reverted on `feature/memory` by PR #183).
  - `expectedFileChanges`: **1** (already shipped in iter 6).

### 3.1 Step ordering and dependencies

- A1 must land before B1 and B2 (creates the file and helpers).
- B1 and B2 were originally independent of each other; iter-6 removed B2
  (see §2.3 and Stage B above).
- C1 was originally blocked on upstream PR #175 + dispatcher canonical
  merge; iter-6 completed it directly per operator pin §6.5.

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

The shipped `parser_powershell_test.go` on this branch (post-iter-6)
contains two contract tests beyond the three WIT steps above:
`TestPowerShellParser_Interface_Wired` (Language/Extensions surface) and
`TestPowerShellParser_RegisteredInActiveBuild` /
`TestPowerShellParser_RegisteredInBothBuildTagSources` (build-tag
registration smoke + the both-source-files literal check). These came in
alongside Stage 6.1 / 6.2 hardening and are useful but **not** part of
this stage's WIT — they are out of scope for the Stage 6.3 deliverable
and do not need to be re-planned or re-shipped.

### 3.4 Removed in iter 6

Iter-6 removed two additional tests that the iter-1 file had carried:
`TestPowerShellParser_Timeout_ReturnsNonSentinelError` (depended on the
nonexistent `timeout` field on `PowerShellParser`) and
`TestPowerShellEnvelope_ToParseResult_MapsAllFields` (depended on the
entire `powershellEnvelope` / `psTypeRecord` / `psMethodRecord` /
`psImportRecord` / `MemberAccess` type chain that does not exist on
HEAD). Both will need to be re-authored if and when PR #175's
subprocess-and-envelope implementation is restored; until then, their
absence keeps the test file compileable.

## 4. Prerequisites and current HEAD coverage (iter-6 reconciled)

The iter-6 reconciled Stage 6.3 test artifact targets HEAD's actual
symbol surface, **not** the post-PR-#175 shape that iter-1 forward-assumed.
Per operator pin `ps-fixture-test-blocked-on-prod` ("Not blocked — accept
test as artifact for when prod catches up"), the table below records
what each surface the artifact uses needs and how HEAD supplies it.

| Target surface (actually used by iter-6 test) | HEAD status | Notes |
|---|---|---|
| Uppercase `PowerShellParser{pwshBin string}` (no `timeout` field) | ✅ Present at `parser_powershell.go` line 7 | Iter-6 rewrote iter-1's `&powershellParser{pwshBin,timeout}` → `&PowerShellParser{pwshBin: ""}`; the `timeout` field is not on HEAD. |
| `Parse()` body returning populated `ParseResult` from a `pwsh` subprocess | ❌ Stub returns `ParseResult{}, nil` when `pwsh` resolves | **Production-gap signal**: the Greeter fixture assertions intentionally fail against the stub. They will start passing automatically once PR #175's 724-line subprocess body is restored — no further test edits required. |
| `ErrParserUnavailable` sentinel + `UnavailableError{Reason string}` with `.Is(target) bool` | ✅ Present at `parser.go` lines 14–22 | Iter-6 sentinel test uses `errors.Is(err, ErrParserUnavailable)` AND structural `errors.As(err, &ue) && ue.Reason == "pwsh_not_available"` — replaces iter-1's fragile substring check. |
| `NewPowerShellParser()` constructor | ✅ Present | Used by registration smoke tests under both `parsers_cgo.go` and `parsers_nocgo.go` build tags. |
| `ClassDecl{Name, LangMeta map[string]string}` | ✅ Present | Iter-6 dropped iter-1's references to `QualifiedName`, `Kind`, `Extends`, `Implements` (none on HEAD). |
| `MethodDecl{Name, ClassName, LangMeta map[string]string}` | ✅ Present | Iter-6 dropped iter-1's references to `QualifiedName`, `EnclosingClass`, `ReceiverCalls`, `MemberAccesses` (none on HEAD); receiver-call invariant now asserted via `LangMeta["receiver_calls"]` slug. |
| `Import{Path, LangMeta map[string]string}` | ✅ Present | Iter-6 dropped iter-1's `Import.Module` reference; field is `Import.Path` on HEAD. The dot-source variant test was withdrawn in iter 6 along with the `Import.Module` dependency. |
| Package `ast` compiles at HEAD | ❌ Pre-existing duplicate `Edge`/`Node`/`EmitResult`/`Logger`/`Dispatcher`/`NewDispatcher`/`Dispatcher.EmitFile` decls across `parser.go` + `dispatcher.go`, missing `lib/pq` / `pgx` / `otel` go.mod deps | **Out of scope per operator pin.** Will be resolved by the separate `dispatcher canonical merge` workstream. Per pin, this stage is **not** blocked on that resolution; the artifact ships as-is and will start running once both that workstream and PR #175 land. |

**Operator-pin consequence (iter 6 onward).** The iter-1 "blocked
prerequisite" rule has been superseded. Steps in §3 are no longer
parked waiting on prerequisites; instead, the artifact ships against
HEAD, and the single production-side gap (stub `Parse()` returning
empty `ParseResult`) is captured as an *intentional fixture-test
failure* until the upstream workstreams restore the subprocess body.
This is documented in §3 Step B1 and §6.5 (operator-pin section).

## 5. Verification strategy

Stage 6.3 verification has two layers:

1. **Pure-Go layer (Stage A steps).** Runs on every CI host regardless of
   `pwsh` availability. Exercises only the sentinel + interface contracts
   the dispatcher relies on — specifically `errors.Is(err, ErrParserUnavailable)`
   plus structural `errors.As(*UnavailableError).Reason == "pwsh_not_available"`,
   and the `Language()` / `Extensions()` interface surface. **Does NOT
   exercise timeout or envelope-mapping contracts** — iter 6 removed
   `TestPowerShellParser_Timeout_ReturnsNonSentinelError` and
   `TestPowerShellEnvelope_ToParseResult_MapsAllFields` because their
   supporting fields/types (`timeout` field, `powershellEnvelope` / `psTypeRecord`
   / `psMethodRecord` / `psImportRecord` / `MemberAccess` chain) do not exist on
   HEAD (see §3.4 "Removed in iter 6" and the §1.5 reconciliation table for
   re-author guidance if PR #175 ever restores those surfaces).
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
    are the *design* of what the artifact tests; **iter 6** reconciled
    the shipped test file against HEAD's actual symbols (Stage C1 above,
    now COMPLETED), so the artifact COMPILES today. The remaining
    `Parse()` stub gap surfaces as an intentional test failure (the
    fixture assertions fail on the stub) rather than a separate plan
    step.

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
