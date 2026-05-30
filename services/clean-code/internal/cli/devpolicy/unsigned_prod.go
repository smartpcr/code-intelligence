// -----------------------------------------------------------------------
// <copyright file="unsigned_prod.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build prod

// This file is the prod build's [Loader] implementation. It
// is mutually exclusive with `unsigned_dev.go`; only one of
// the two is consumed per build. Anything BOTH files need to
// agree on lives in `bypass.go` (no build tag).
//
// # Why this file exists
//
// `architecture.md` Sec 7.2 mandates that a prod binary MUST
// refuse the dev-mode policy bypass at the EARLIEST reachable
// layer. This file is that layer: `NewLoader()` returns a
// `prodSentinelLoader` whose Load method returns
// [ErrDevModeUnavailable] REGARDLESS of the [LoaderSource] --
// no `embeddedRulePacks` access, no `os.DirFS` access, no
// filesystem stat. The refusal is total and compile-time-
// enforced (the dev `Load` body is simply not in the prod
// binary because `unsigned_dev.go` carries `//go:build !prod`).
//
// # Determinism / no side effects
//
// The Load body MUST NOT perform any I/O before returning the
// sentinel. The test in `unsigned_prod_test.go` exercises
// this by passing a [LoaderSource] with a `DirPath` that
// would PANIC if statted (a NUL-byte-containing path on
// POSIX-shaped roots). A future edit that adds an early
// `src.FS()` call would fail that test loudly.

package devpolicy

import (
	"context"
)

// prodSentinelLoader is the prod build's concrete [Loader]
// implementation. Like its `devLoader` sibling, it carries
// no state -- every Load call is self-contained and returns
// the same sentinel.
type prodSentinelLoader struct{}

// NewLoader returns the active build's [Loader].
//
// In the prod build (this file), it returns a
// [prodSentinelLoader] whose Load method ALWAYS returns
// [ErrDevModeUnavailable] regardless of input.
//
// In the !prod (dev) build (`unsigned_dev.go`), it returns
// a loader whose Load method validates the [LoaderSource]
// and returns [ErrLoaderNotYetImplemented].
//
// The function name and signature are IDENTICAL across
// both files so the `cmd/cleanc` dispatcher can call
// `devpolicy.NewLoader()` without a per-build-tag import.
func NewLoader() Loader { return prodSentinelLoader{} }

// Load returns [ErrDevModeUnavailable] for every input.
//
// The method body MUST NOT perform I/O before returning
// the sentinel (no `src.FS()`, no `os.Stat`). The pinned
// `TestProdNewLoader_NoFSAccessBeforeError` in
// `unsigned_prod_test.go` exercises this contract.
func (prodSentinelLoader) Load(_ context.Context, _ LoaderSource) (Bundle, error) {
	return Bundle{}, ErrDevModeUnavailable
}
