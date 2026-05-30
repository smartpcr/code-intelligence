// -----------------------------------------------------------------------
// <copyright file="doc.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Command cleanc -- developer-laptop CLI for the clean-code service.
//
// This `doc.go` is the canonical anchor index for the `cleanc`
// command. It complements `main.go` (which carries the dispatcher
// itself) by giving godoc a stable, table-of-contents-style entry
// point that lists every spec anchor the binary observes and
// every sibling package it composes.
//
// # Spec anchors observed by this binary
//
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 1.3 row `cli-binary-location` -- the binary lives at
//     `services/clean-code/cmd/cleanc/` alongside the six existing
//     `clean-code-*` service binaries and ships via the same
//     Makefile `CMD_DIRS` glob.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 1.3 row `cli-l7-authority` -- the `apply` verb is reserved
//     (registered with help text + a non-zero exit code) until the
//     L7 patch-suggestion authority is pinned by the architecture
//     team.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 6.3 -- pre-pinned exit-code surface forwarded to the
//     operator-facing rejection message.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
//     Sec 8.1 -- the canonical 10-flag global surface
//     (`--out`, `--findings`, `--emit-prompts`, `--policy`,
//     `--with-churn`, `--top-n`, `--exit-on`, `--diagnostics`,
//     `--dev-mode`, `--telemetry-otlp`) plus the reserved
//     `--snippet-cap-lines` rejection token.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
//     Sec 8.6 -- the pinned exit-code closed set
//     `{0, 1, 2, 64, 70}` enforced by [internal/cli/flags].
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
//     Sec 8.10 -- dependency-budget linter; this `main` package
//     MUST NOT import any `*_sql_store` package or any constructor
//     that takes a `*sql.DB`.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/e2e-scenarios.md`
//     Phase 1 Background -- the version-string regex and the
//     `unknown sub-command` literal.
//
// # Sibling packages composed by this binary
//
//   - `internal/cli/flags` -- exit codes, verb names, global-flag
//     defaults, reserved-flag rejection messages. THE single place
//     a pinned constant changes.
//   - `internal/cli/devpolicy` -- dev-mode policy loader; produces
//     the unsigned `steward.PolicyVersion` the rule engine accepts
//     when the binary is built without the `prod` tag.
//   - `internal/cli/effort` -- deterministic effort-estimator
//     fallback used when the ONNX model is unavailable.
//   - `internal/cli/repocontext` -- deterministic `repo_id` /
//     `head_sha` minting (architecture G2).
//   - `internal/cli/scopebinding` -- deterministic `scope_id`
//     UUID-v5 minting (architecture G2).
//
// # Build-tag matrix
//
//   - Default (no tags): dev build -- `defaultDevMode = true`,
//     `buildTag = ""`. `--dev-mode` defaults to true and the
//     dev-mode policy loader is reachable.
//   - `-tags prod`: production build -- `defaultDevMode = false`,
//     `buildTag = "prod"`. The dev-mode policy loader returns
//     `devpolicy.ErrDevModeUnavailable` for any `Load` call.
//
// The two build-tag-paired files in this directory
// (`buildtag_default.go` and `buildtag_prod.go`) are the single
// pivot point; nothing else in the package switches on the build
// tag.
//
// # Subsequent stages
//
// This file ships the Stage 1.1 SKELETON only. Subsequent stages
// wire the real `analyze` / `report` / `apply` bodies in dedicated
// sibling packages (`internal/cli/walk`, `internal/cli/orchestrator`,
// `internal/cli/report`, `internal/cli/suggest`); they do NOT modify
// `cmd/cleanc/main.go` itself beyond replacing the stub `runAnalyze`
// / `runReport` bodies.
package main
