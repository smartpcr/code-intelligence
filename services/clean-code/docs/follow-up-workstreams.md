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
- **Root cause class**: Interface drift on
  `webhook.ChurnIngester` (the type the defects ingester
  embeds was renamed or re-shaped on a sibling stage and
  defects was not updated). Per iter-4 git evidence the
  introducing PR was #103.
- **Suggested targets**:
    - `services/clean-code/internal/ingest/defects/` (all
      files in that package)
    - `services/clean-code/internal/ingest/webhook/`
      (verify the canonical interface shape)
- **Out-of-scope for Stage 7.3 because**: defects is not
  in the Stage 7.3 import closure (see matrix above), the
  build break predates Stage 7.3 (PR #103 merged before
  this branch forked at `803ae6c`), and editing
  production code in a sibling package is explicitly
  forbidden by the iter prompt.

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

## Verifying after each follow-up workstream lands

After each FU-* workstream completes, the operator can
re-run:

```
$ cd services/clean-code
$ go test ./... -count=1
```

and confirm that the previously-failing siblings are now
green. Stage 7.3's own targeted chain
(`go test ./internal/management/insights/
./internal/management/ ./internal/evaluator/`) was green
throughout Stage 7.3 and should remain so.
