// -----------------------------------------------------------------------
// <copyright file="embed_prod.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// This file is a STALE COPY of the `embeddedRulePacks` alias for
// prod builds. The canonical declaration lives in `embed.go` (no
// build tag, visible in every compilation). This file's
// prod-gated nil re-declaration conflicts with the canonical
// `embed.go` assignment and dates from before the CLI Binary
// Skeleton workstream consolidated the alias. The prod build's
// `unsigned_prod.go` returns `ErrDevModeUnavailable` immediately
// without accessing `embeddedRulePacks`, so having the canonical
// (non-nil) alias visible in prod builds does not create a bypass.
//
// Gating with the `legacy_superseded` build tag (NEVER set)
// keeps the file on disk while excluding it from every
// compilation, following the convention set by `embed_dev.go`
// and `dev_loader.go`.

//go:build legacy_superseded

package devpolicy

import "io/fs"

var embeddedRulePacks fs.FS