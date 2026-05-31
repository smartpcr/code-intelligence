// -----------------------------------------------------------------------
// <copyright file="doc.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package report renders a [RunArtifact] -- the CLI's full
// per-run output shape pinned by REFACTOR-GUIDE `architecture.md`
// Sec 4.7 -- into the operator-facing surfaces the `cleanc analyze`
// composition root writes:
//
//   - The markdown report ([Markdown.Render], architecture Sec 3.7.1
//     and Sec 5.7) emitted by default to stdout or, when
//     `--out <path>` is supplied, to that path.
//   - The JSON sidecar (future [JSON.Render], architecture Sec 3.7.2
//     and Sec 5.7) emitted only when `--findings <path>` is supplied.
//
// Both writers satisfy the single [Renderer] interface, so the
// composition root threads a `[]Renderer` and a matching
// `[]io.Writer` and dispatches in one loop.
//
// # Stage ownership
//
// This package is built up across the phase-p0 reports-and-delivery
// stages of `implementation-plan.md`:
//
//   - "Markdown Report Renderer" (THIS stage) -- ships the
//     [Renderer] interface, the [RunArtifact] container, and the
//     [Markdown] writer's header + verdict blocks (architecture
//     Sec 3.7.1 steps 1 and 2).
//   - "Markdown Findings Section" -- adds the findings-by-severity
//     block (Sec 3.7.1 step 3).
//   - "Markdown Hot-Spot Section" -- adds the top-N hot-spot table
//     (Sec 3.7.1 step 4).
//   - "Markdown Refactor Plan Section" -- adds the refactor plan
//     and task table (Sec 3.7.1 step 5).
//   - "Markdown Diagnostics Section" -- adds the diagnostics block
//     (Sec 3.7.1 step 6).
//   - "JSON Report Renderer" -- ships the JSON writer (Sec 3.7.2).
//
// Subsequent stages extend the [Markdown] writer in place rather
// than redefining the [RunArtifact] container, so every renderer
// observes the same architecture-pinned shape.
package report
