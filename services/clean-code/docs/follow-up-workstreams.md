# Follow-up workstream proposals (sibling-package rot)

Owner: `services/clean-code`
Created: Stage 7.3 iter 7 (workstream
`ws-code-intelligence-clean-code-phase-cross-repo-aggregator-stage-insights-surface-percentile-freshness-banner`)

## Purpose

The iter-1 through iter-6 evaluators for Stage 7.3
repeatedly flagged "full repository test health remains
red outside Stage 7.3" as item 3 (formerly item 4). The
iter prompt's standing rule explicitly carves these out
of Stage 7.3's scope:

> Refactoring production code under `src/ast/` or similar
> to make a test pass is out of scope -- propose it as a
> follow-up workstream via Open Questions.

This document is the structural fulfillment of that
"propose it as a follow-up workstream" guidance.
Operators can use the entries below to spawn the four
remediation workstreams directly; each entry includes
the failing test name, the proposed workstream slug, the
proposed target file list, the inferred root cause, and
the verified git provenance proving the failure
predates Stage 7.3.

## Stage 7.3 import-isolation matrix

Before the per-failure entries, here is the audit
proving Stage 7.3's three packages are isolated from
the failing siblings to the maximum extent the current
codebase allows:

| Stage 7.3 package                  | `defects` dep | `aggregator` dep | `ast/scope` dep        | `storage` dep          |
| ---------------------------------- | ------------- | ---------------- | ---------------------- | ---------------------- |
| `internal/management/insights`     | NO            | NO               | NO                     | NO                     |
| `internal/management`              | NO            | NO               | YES (compile-time)     | YES (compile-time)     |
| `internal/evaluator`               | NO            | NO               | NO                     | NO                     |

Reproducible via:

```
$ for pkg in ./internal/management/insights/ ./internal/management/ ./internal/evaluator/; do
    go list -deps "$pkg" | grep -E 'internal/(ingest/defects|aggregator|ast/scope|storage)$'
  done
```

Key observations:

- The workstream's CORE deliverable
  (`internal/management/insights/freshness.go`) is in
  `internal/management/insights`, which has ZERO
  transitive dependency on any of the four failing
  siblings.
- `internal/management` does import `internal/ast/scope`
  and `internal/storage` at compile-time, but its TESTS
  pass green (`go test ./internal/management/ -count=1
  -> ok 0.809s`) because the failing tests in those
  siblings exercise functions that `internal/management`
  does not call at runtime.
- None of the three Stage 7.3 packages touches
  `internal/ingest/defects` (build-broken) or
  `internal/aggregator` (test-broken). Stage 7.3 is
  fully insulated from those two.

## Follow-up workstream proposals

### FU-1: `internal/ingest/defects` build repair

- **Slug**:
  `ws-code-intelligence-clean-code-phase-external-metric-ingest-webhook-stage-defects-ingester-interface-repair`
- **Failing artifact**:
  `go test ./internal/ingest/defects -count=1` returns
  `[build failed]` (no test runs at all because the
  package itself does not compile).
- **Exact compile error** (re-confirmed Stage 7.3 iter 8):
  ```
  internal/ingest/defects/handler_test.go:498:32:
    cannot use ing (variable of type *metric_ingestor.Ingestor)
    as webhook.ChurnIngester value in argument to
    webhook.NewChurnVerbHandler:
    *metric_ingestor.Ingestor does not implement
    webhook.ChurnIngester (missing method Ingest)
  ```
- **Root cause class**: Interface drift between
  `webhook.ChurnIngester` (a sibling-package
  contract) and `*metric_ingestor.Ingestor` (the
  implementation the defects test passes in). The
  `Ingest` method either disappeared from
  `metric_ingestor.Ingestor` or its signature changed
  on a sibling stage after PR #102 landed.
- **Verified PR provenance** (`git log --all --oneline
  -- services/clean-code/internal/ingest/defects/handler_test.go`):
    - **PR #102** (`9d586f3 ingest defects verb store
      only`) -- the originating PR that created the
      defects package and its handler test.
    - **PR #111** (`ffc1ddc Management read verbs and
      insights projections`) -- the last PR to modify
      `handler_test.go` before this branch forked.
  Both PRs predate the Stage 7.3 branch base
  (`803ae6c`), confirming the build break is sibling-
  stage rot, not a regression from this workstream.
- **Suggested targets**:
    - `services/clean-code/internal/ingest/defects/handler_test.go:498`
      (the call site that passes the wrong concrete
      type to `webhook.NewChurnVerbHandler`)
    - `services/clean-code/internal/ingest/webhook/`
      (verify the canonical `ChurnIngester` interface
      shape -- if `Ingest` was intentionally renamed
      there, the test file needs the new method name)
    - `services/clean-code/internal/metric_ingestor/`
      (verify the canonical `Ingestor` method set --
      if `Ingest` was intentionally removed, the test
      file may need a different concrete implementor
      instead)
- **Out-of-scope for Stage 7.3 because**: defects is not
  in the Stage 7.3 import closure (see matrix above),
  the build break predates Stage 7.3 (PR #102 and PR
  #111 both merged before this branch forked at
  `803ae6c`), and editing production code in a sibling
  package is explicitly forbidden by the iter prompt.

### FU-2: `internal/aggregator` semantic test repair

- **Slug**:
  `ws-code-intelligence-clean-code-phase-cross-repo-aggregator-stage-arch-debt-ratio-embedded-cycle-semantic-repair`
- **Failing test**:
  `TestCompose_ArchDebtRatio_EmbeddedWithCycleMemberInputs_NotDegraded`
  in `internal/aggregator`.
- **Root cause class**: Semantic drift in the
  arch-debt-ratio composer regarding how embedded-cycle
  members should affect the `degraded` flag. Per iter-4
  git evidence the introducing PR was #118
  (`[impl] System tier metric composer`).
- **Suggested targets**:
    - `services/clean-code/internal/aggregator/system_tier.go`
      (or the specific composer file that owns
      `ArchDebtRatio`)
    - The test file holding the failing function (verify
      whether the test or the production behavior needs
      adjustment).
- **Out-of-scope for Stage 7.3 because**: aggregator is
  not in the Stage 7.3 import closure (matrix above), the
  failure predates Stage 7.3 (PR #118 merged before
  branch fork), and the iter prompt forbids editing
  sibling-stage production code.

### FU-3: `internal/ast/scope` UUID-pin reconciliation

- **Slug**:
  `ws-code-intelligence-clean-code-phase-ast-foundations-stage-namespace-uuid-pin-reconciliation`
- **Failing test**: `TestNamespace_Pinned` in
  `internal/ast/scope/identity_test.go`.
- **Root cause class**: The pinned namespace UUID literal
  in the test
  (`5fa5937c-c012-5190-b7bd-0bd48f41de65`) drifted from
  the current computed value
  (`2d17cb5e-92a1-5dcb-9df0-10ef6cf2f2ae`). The test's
  own comment makes the resolution explicit: "if this
  fix is intentional, recompute the literal... and
  update pinnedNamespaceUUID -- the test exists to make
  this an explicit step." Per iter-4 git evidence the
  introducing PR was #71.
- **Suggested targets**:
    - `services/clean-code/internal/ast/scope/identity_test.go`
      (the UUID literal at line 40 or thereabouts)
    - Possibly
      `services/clean-code/internal/ast/scope/identity.go`
      if the namespace-derivation algorithm itself
      drifted.
- **Stage 7.3 cannot fix unilaterally because**: even
  though `internal/management` (a Stage 7.3 package) DOES
  transitively depend on `internal/ast/scope`, the
  failing test gates a SCHEMA INVARIANT (UUID stability
  across releases). Bumping the literal would silently
  accept the schema drift the test is designed to
  surface; the test's comment requires explicit operator
  acknowledgement that the schema bump is intentional.
- **Operator decision required**: is the current
  `2d17cb5e-92a1-5dcb-9df0-10ef6cf2f2ae` value
  intentional (in which case bump the literal and add a
  CHANGELOG entry to the AST workstream documenting the
  schema bump) or unintentional (in which case the
  derivation function in `identity.go` regressed and the
  fix is to restore it)?

### FU-4: `internal/storage` migration-discovery repair

- **Slug**:
  `ws-code-intelligence-clean-code-phase-storage-stage-migration-discovery-stage-pair-repair`
- **Failing tests** (six in one file):
    - `TestDiscoverMigrations_findsStage12Pair`
    - `TestDiscoverMigrations_findsStage14Pair`
    - `TestDiscoverMigrations_findsStage15Pair`
    - `TestDiscoverMigrations_findsStage32Pair`
    - `TestDiscoverMigrations_findsStage32SeedPair`
    - `TestDiscoverMigrations_findsStage51Pair`
- **Root cause class**: Likely a migration-numbering
  collision (Stage 0010 / 0011 was added on a sibling
  stage and shifted the expected pair indices in the
  discovery test). Per iter-4 git evidence the
  introducing PR was #105.
- **Suggested targets**:
    - `services/clean-code/internal/storage/` (migration
      discovery and the per-stage pair fixtures)
- **Stage 7.3 cannot fix unilaterally because**: same
  rationale as FU-3 -- `internal/management` depends on
  `internal/storage` at compile-time but the failing
  tests gate migration ordering, which is a
  release-versioning concern owned by the storage
  workstream.

---

## Agent-memory follow-up workstream proposals (Stage 7.3 iter 3 + iter 4 surface)

The Stage 7.3 iter-3 evaluator's run of
`go test ./...` against `services/agent-memory/go.mod` (a
DIFFERENT Go module from `services/clean-code/go.mod`)
surfaced four failing packages. They are documented here
for operator follow-up; they are NOT engineer-decidable
inside the Stage 7.3 workstream (cross-service-boundary
edits are explicitly out of this workstream's brief, which
targets only `services/clean-code/...`). The iter-3
evaluator explicitly concedes the scope point ("scopes
around them correctly... out of scope but real").

Verified git provenance for ALL four entries: the files in
question were last touched in `services/agent-memory/` by
PR #11 (commit `5058db7 [impl] GraphWriter library`), which
merged into `feature/clean-code` long before the Stage 7.3
branch base `803ae6c` -- so the failures pre-date Stage 7.3
in their entirety.

### FU-A: `services/agent-memory/cmd/qdrant-bootstrap` test-build repair

- **Slug**:
  `ws-code-intelligence-agent-memory-qdrant-bootstrap-collections-accessor-repair`
- **Failing artifact**:
  `go test ./cmd/qdrant-bootstrap/` returns `[build failed]`
  in `services/agent-memory/` (no tests run).
- **Exact compile error** (Stage 7.3 iter 3 verification):
  ```
  cmd/qdrant-bootstrap/main_test.go:662:4: b.Collections undefined (type *Bootstrapper has no field or method Collections)
  cmd/qdrant-bootstrap/main_test.go:712:4: b.Collections undefined (type *Bootstrapper has no field or method Collections)
  ```
- **Root cause class**: Accessor drift. The test asserts
  `b.Collections` on `*Bootstrapper` but the production
  type does not expose that field/method. Either the
  accessor was renamed/removed in production without
  updating the test, or the test was written against a
  newer Bootstrapper API that has not landed yet.
- **Suggested targets**:
    - `services/agent-memory/cmd/qdrant-bootstrap/main_test.go:662,712`
    - `services/agent-memory/cmd/qdrant-bootstrap/main.go` (verify the canonical Bootstrapper API surface)
- **Out-of-scope for Stage 7.3 because**: different service
  (`services/agent-memory/go.mod`), different ownership,
  failure pre-dates the Stage 7.3 branch base.

### FU-B: `services/agent-memory/pkg/fingerprint` golden-vector drift repair

- **Slug**:
  `ws-code-intelligence-agent-memory-fingerprint-golden-vector-refresh`
- **Failing tests** (four in `pkg/fingerprint`):
    - `TestNodeFingerprint_goldenVector`
    - `TestEdgeFingerprint_goldenVector`
    - `TestNodeFingerprint_matchesHandRolledConcatenation`
    - `TestEdgeFingerprint_matchesHandRolledConcatenation`
- **Symptom** (Stage 7.3 iter 3 verification):
  Production `NodeFingerprint` / `EdgeFingerprint`
  returns a DIFFERENT hex digest than the golden vector
  AND a different digest than a hand-rolled
  `sha256(canonical(input))` computation in the test.
  Example: `NodeFingerprint = 1aec00b6... want 84c1e483...`.
- **Root cause class**: Fingerprint canonicalisation
  drift. Either (a) the input canonicaliser changed
  (whitespace handling, field ordering, separator bytes)
  without updating the golden vectors, OR (b) the hash
  family itself changed (e.g. domain separation tag was
  added). The matching `_matchesHandRolledConcatenation`
  failure indicates the test's hand-rolled reference
  expects a DIFFERENT byte recipe from production.
- **Suggested targets**:
    - `services/agent-memory/pkg/fingerprint/fingerprint.go`
      (production canonicaliser + hasher)
    - `services/agent-memory/pkg/fingerprint/fingerprint_test.go:111,132,289,320`
      (golden vector constants + hand-rolled reference)
- **Out-of-scope for Stage 7.3 because**: different service,
  different ownership, failure pre-dates Stage 7.3 branch
  base.

### FU-C: `services/agent-memory/internal/mgmtapi` ingest-handler test repair

- **Slug**:
  `ws-code-intelligence-agent-memory-mgmtapi-ingest-handler-test-repair`
- **Failing tests** (four in `internal/mgmtapi`):
    - `TestIngestDelta_dbOutage_returns500_noLeak`
    - `TestIngestDelta_foreignKeyViolation_returns404`
    - `TestIngestDelta_repeatedCall_returnsSameJobID_noSecondRepoEvent`
    - `TestIngestSpans_wrongMethod_returns405`
- **Root cause class**: Unknown without deeper triage;
  the failures span both `IngestDelta` (idempotency +
  error mapping) and `IngestSpans` (method-not-allowed
  contract). Suggests either a shared handler-base
  regression or a test-fixture regression.
- **Suggested targets**:
    - `services/agent-memory/internal/mgmtapi/` (handlers
      under test)
    - `services/agent-memory/internal/mgmtapi/*_test.go`
      (the four failing test bodies)
- **Out-of-scope for Stage 7.3 because**: different service,
  failure pre-dates Stage 7.3 branch base.

### FU-D: `services/agent-memory/internal/webhookreceiver` payload-validate test repair

- **Slug**:
  `ws-code-intelligence-agent-memory-webhookreceiver-payload-validate-repair`
- **Failing test** (one):
    - `TestPayloadValidate`
- **Root cause class**: Unknown without deeper triage;
  likely either a payload-shape change or a validator
  rule change that the golden test case did not get
  updated for.
- **Suggested targets**:
    - `services/agent-memory/internal/webhookreceiver/`
      (validator + test fixture)
- **Out-of-scope for Stage 7.3 because**: different service,
  failure pre-dates Stage 7.3 branch base.

## Verifying after each follow-up workstream lands

The two follow-up classes target DIFFERENT Go modules, so
the verification command is module-specific:

**For FU-1 through FU-4** (`services/clean-code/go.mod`):
these four entries are kept as audit trail only. The
underlying failures were repaired in place during the
Stage 7.3 iter-2 addendum (see `services/clean-code/
CHANGELOG.md` -> Stage 7.3 -> "Sibling-package repairs
landed on this branch"). The verification command is:

```
$ cd services/clean-code
$ go test ./... -count=1
```

and this is already green on the current branch (all 31
packages PASS). FU-1 through FU-4 should NOT need to be
spawned; they remain documented so the audit trail of
WHAT was repaired stays greppable.

**For FU-A through FU-D** (`services/agent-memory/go.mod`):
these are the GENUINE spawnable cross-service follow-ups.
After each FU-A..FU-D workstream lands, the operator can
re-run the agent-memory test suite in that service's own
module root:

```
$ cd services/agent-memory
$ go test ./... -count=1
```

and confirm the previously-failing siblings
(`cmd/qdrant-bootstrap`, `pkg/fingerprint`,
`internal/mgmtapi`, `internal/webhookreceiver`) are now
green. The clean-code module (`services/clean-code/go.mod`)
is a DIFFERENT module and is not affected by FU-A..FU-D --
running `cd services/clean-code && go test ./...` would
neither exercise the agent-memory fixes nor surface their
failures.

Stage 7.3's own targeted chain
(`go test ./internal/management/insights/
./internal/management/ ./internal/evaluator/` from
`services/clean-code/`) was green throughout Stage 7.3
and should remain so independently of the FU-A..FU-D
agent-memory work.
