// -----------------------------------------------------------------------
// <copyright file="bypass.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// This file ships the build-tag-AGNOSTIC half of the dev-mode
// policy bypass surface: the sentinels both build-tag-gated
// `NewLoader` implementations return, and the operator-facing
// banner string a dev build emits unconditionally at the start
// of every `cleanc analyze` run.
//
// # Why split from unsigned_dev.go / unsigned_prod.go
//
// `unsigned_dev.go` (`//go:build !prod`) and `unsigned_prod.go`
// (`//go:build prod`) are MUTUALLY EXCLUSIVE -- the Go compiler
// only consumes one of them per build. Anything those two files
// need to AGREE on (sentinel error values, the banner string,
// the [Loader] interface in `loader.go`) MUST live in a file
// without a build tag so both compilations resolve it to the
// same symbol.
//
// # Spec anchors
//
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 3.8 ("L8 -- Dev-mode Policy Loader") -- the
//     STRUCTURAL signature bypass that the unsigned `Bundle`
//     synthesisers ship under in dev builds.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 7.2 ("Production hardening") -- the prod build MUST
//     fail any `--dev-mode` / `--policy <path>` invocation at
//     the earliest reachable layer; [ErrDevModeUnavailable] is
//     that layer.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
//     Sec 8.4 ("Rule-pack distribution") -- the precedence and
//     refusal rules the [Loader] consumers honour.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
//     constraint C6 ("dev-mode bypass must be loud") -- the
//     [BannerText] is the LOUD half of that rule; the prod
//     refusal sentinel [ErrDevModeUnavailable] is the OTHER
//     half.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
//     constraint C10 ("banner text exact") -- pins the
//     byte-for-byte content of [BannerText]; a future edit
//     here MUST cascade into the e2e fixture.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/implementation-plan.md`
//     Stage 1.4 items 99-102 -- the workstream that wires the
//     real YAML-decoding body behind the Stage 1.1 shells.

package devpolicy

import (
	"errors"
	"io"
)

// ErrDevModeUnavailable is the sentinel returned by the prod
// build's [NewLoader].Load (declared in `unsigned_prod.go`)
// when ANY [Loader.Load] call is attempted in a binary built
// with the `prod` build tag.
//
// The error string is byte-identical to the form pinned in
// `implementation-plan.md` Stage 1.4 line 100, so a future
// operator-facing diagnostic can `errors.Is(err,
// devpolicy.ErrDevModeUnavailable)` AND fall back to string
// matching against the impl-plan-pinned phrase without drift.
//
// The dev build's [Loader] does NOT return this sentinel --
// it returns [ErrLoaderNotYetImplemented] for the Stage 1.1
// skeleton, which Stage 1.4 swaps out for the real YAML
// decoder + unsigned `PolicyVersion` synthesiser.
var ErrDevModeUnavailable = errors.New("devpolicy: dev-mode policy bypass not available in prod build")

// ErrLoaderNotYetImplemented is the sentinel returned by the
// dev build's [NewLoader].Load (declared in `unsigned_dev.go`)
// while the Stage 1.1 CLI skeleton is in place. The dev
// loader still validates the [LoaderSource] (so an operator
// who forgets `--policy <path>` sees [ErrMissingPolicyDir]
// at this layer rather than later inside an empty walk),
// but does not yet decode YAML or synthesise an unsigned
// `PolicyVersion` -- that body lands in
// `implementation-plan.md` Stage 1.4 items 97-102.
//
// The sentinel is DELIBERATELY DISTINCT from
// [ErrDevModeUnavailable] so a future test cannot confuse a
// "dev build with no YAML wiring yet" condition with a
// "prod build refusing the bypass" condition.
var ErrLoaderNotYetImplemented = errors.New("devpolicy: Load not yet implemented (Stage 1.1 skeleton; Stage 1.4 wiring follow-up)")

// BannerText is the byte-for-byte exact operator-facing
// warning the dev build emits at the start of every
// `cleanc analyze` run (per `tech-spec.md` constraint C10).
//
// The text is intentionally PINNED rather than templated so:
//
//   - The e2e Phase 1 scenario "banner text exact" can assert
//     against this constant verbatim.
//   - A future translation / colourisation edit cannot
//     silently drift the operator-facing language away from
//     the constraint.
//
// [EmitBanner] writes this string followed by a single `\n`
// to the supplied writer; downstream consumers MUST treat
// the constant as the canonical contract surface and use
// [EmitBanner] for the actual write so the trailing newline
// stays consistent.
const BannerText = "WARNING: dev-mode policy is unsigned. Do NOT use cleanc output as the source of truth for a production gate."

// EmitBanner writes [BannerText] followed by a single `\n`
// to w and returns the byte count and any underlying write
// error verbatim.
//
// Both build-tag-gated `NewLoader` implementations
// (`unsigned_dev.go` and `unsigned_prod.go`) are free to
// call this helper, but only the dev build's CLI plumbing
// is expected to do so: the prod build refuses every Load
// at the [ErrDevModeUnavailable] layer, so there is no
// dev-mode bypass to warn about.
//
// The returned byte count is exactly `len(BannerText) + 1`
// on a successful write; callers do NOT need to count
// themselves.
func EmitBanner(w io.Writer) (int, error) {
	return io.WriteString(w, BannerText+"\n")
}
