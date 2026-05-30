// -----------------------------------------------------------------------
// <copyright file="embed_dev.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// This file is a STALE COPY of the `embeddedRulePacks` alias.
// The canonical declaration lives in `embed.go` (no build tag,
// so visible in every compilation). This file's `!prod`-gated
// duplicate dates from before the CLI Binary Skeleton workstream
// (commit 94e3f11) consolidated the alias into `embed.go`, and
// it also references `rulepacks.EmbeddedRulePacks` -- a symbol
// that no longer exists (the canonical export is
// `rulepacks.EmbeddedFS`). Preserving the file on disk (per the
// workstream brief's "edit don't delete" rule) requires gating
// it with a build tag that is NEVER set, so the duplicate
// declaration AND the undefined symbol do not break the default
// `go build ./...` invocation.
//
// See `banner.go` for the same rationale + the
// `legacy_superseded` build tag convention.

//go:build legacy_superseded

package devpolicy

import (
	"io/fs"

	"github.com/smartpcr/code-intelligence/services/clean-code/policy/rulepacks"
)

var embeddedRulePacks fs.FS = rulepacks.EmbeddedRulePacks