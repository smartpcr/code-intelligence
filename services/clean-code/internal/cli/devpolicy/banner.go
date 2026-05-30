// -----------------------------------------------------------------------
// <copyright file="banner.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// This file is a STALE COPY of the dev-mode banner surface. The
// canonical declarations of `EmitBanner` (now returning
// `(int, error)`) and the C10 banner string `BannerText` live in
// `bypass.go`, which post-dates this file and was added by the
// CLI Binary Skeleton workstream (commit 94e3f11). This file's
// EmitBanner+DevModeBanner pair is a redundant pre-skeleton
// version that the cleanup pass forgot to delete; preserving the
// file on disk (per the workstream brief's "edit don't delete"
// rule) requires gating it with a build tag that is NEVER set,
// so the duplicate declarations do not break the default `go
// build ./...` invocation.
//
// `//go:build legacy_superseded` is a deliberately invented tag
// name -- no part of the build, lint, or CI matrix sets it, so
// this file is excluded from every compilation. A follow-up
// devpolicy-cleanup workstream is expected to delete this file
// outright; until then the gate keeps the build green.

//go:build legacy_superseded

package devpolicy

import "io"

// DevModeBanner is the C10 banner string emitted to stderr when
// the CLI runs with the unsigned dev-mode policy bypass active.
const DevModeBanner = "\u26a0  DEV MODE \u2014 unsigned policy bypass active. Not for production use.\n"

// EmitBanner writes the dev-mode warning banner to w.
func EmitBanner(w io.Writer) {
	_, _ = io.WriteString(w, DevModeBanner)
}