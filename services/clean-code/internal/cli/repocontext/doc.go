// -----------------------------------------------------------------------
// <copyright file="doc.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package repocontext -- canonical anchor index.
//
// This `doc.go` complements `repocontext.go` (which carries the
// concrete `RepoContext` value, [MintRepoID], [DetectHeadSHA], and
// [DetectModulePath]) by giving godoc a stable, table-of-contents-style
// entry point that lists every spec anchor the package observes and
// every invariant it guards.
//
// # Spec anchors
//
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 1.4 row G2 ("stable identifiers") -- `repo_id` is a
//     deterministic UUID-v5 over the canonical root-path so re-runs
//     on the same checkout produce byte-identical findings.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 4.1 ("RepoContext") -- the four-field row shape mirrored
//     by [RepoContext]; the `working-copy` sentinel pinned for
//     un-versioned trees.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
//     Sec 4.11 ("stable identity") and constraint C3 ("no native
//     git bindings; shell out to `git`") -- pin the algorithm
//     [MintRepoID] implements and the no-dep rule [DetectHeadSHA]
//     follows.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/implementation-plan.md`
//     Stage 1.2 -- the workstream that introduces this package.
//
// # Invariants guarded
//
//   - [MintRepoID] is host-OS-independent: `C:\path\to\repo` on
//     Windows and `/path/to/repo` on POSIX produce DIFFERENT
//     ids (each is the canonical form of its own input), but
//     `C:\Users\dev\repo` and `C:/Users/dev/repo` on Windows
//     produce the SAME id. The `TestMintRepoID_*` goldens in
//     `repocontext_test.go` lock both halves.
//   - [DetectHeadSHA] never returns the empty string. The
//     two-tuple `(sha, isGitRepo)` always has a non-empty `sha`
//     (either a real commit hash or the
//     [HeadSHAWorkingCopySentinel]).
//   - [DetectModulePath] is silent-on-failure by design: an
//     empty return is a contract, not a bug -- the orchestrator
//     simply skips the `cycle_member` recipe for that language.
//
// # Sibling packages
//
//   - `internal/cli/scopebinding` -- the downstream consumer of
//     [RepoContext.RepoID]; mints `scope_id` from
//     `(RepoID, scopeKind, canonicalSignature, HeadSHA)`.
//   - `cmd/cleanc` -- the dispatcher; threads a frozen
//     [RepoContext] through every sub-command's pipeline.
package repocontext
