// -----------------------------------------------------------------------
// <copyright file="prod_loader.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// This file is a STALE COPY of the prod-build [Loader]
// implementation. The canonical `//go:build prod` loader is in
// `unsigned_prod.go`, added by the CLI Binary Skeleton workstream
// (commit 94e3f11) as the Stage 1.1 shell. This file's pre-skeleton
// implementation redeclares `prodLoader`, `NewLoader`, and `Load`
// (all declared in `unsigned_prod.go` under the same `prod` tag).
//
// Gating with the `legacy_superseded` build tag (NEVER set)
// keeps the file on disk while excluding it from every
// compilation, following the convention set by `embed_dev.go`
// and `dev_loader.go`.

//go:build legacy_superseded

package devpolicy

import "context"

type prodLoader struct{}

// NewLoader returns the prod-mode Loader that refuses the
// dev-mode policy bypass at compile time.
func NewLoader() Loader {
	return &prodLoader{}
}

func (l *prodLoader) Load(_ context.Context, _ LoaderSource) (Bundle, error) {
	return Bundle{}, ErrDevModeUnavailable
}