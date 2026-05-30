// -----------------------------------------------------------------------
// <copyright file="unsigned_dev.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build !prod

// This file is the !prod (dev) build's [Loader] implementation.
// It is mutually exclusive with `unsigned_prod.go`; only one
// of the two is consumed per build. Anything BOTH files need
// to agree on lives in `bypass.go` (no build tag).
//
// # Stage 1.1 scope
//
// This file ships the SHELL of the dev-build loader -- a
// concrete implementation of the [Loader] interface whose
// `Load` method validates the [LoaderSource] and then returns
// [ErrLoaderNotYetImplemented]. That is enough to:
//
//   - Make `Loader` reachable in the dev build (the
//     `cmd/cleanc` dispatcher can call `devpolicy.NewLoader()`
//     without a per-build-tag branch).
//   - Pin the source-resolution behaviour (an operator who
//     forgets `--policy <path>` still gets
//     [ErrMissingPolicyDir] at THIS layer, not later inside
//     an empty walk).
//
// `implementation-plan.md` Stage 1.4 items 97-102 swap the
// stub body for the real YAML decoder + unsigned
// `PolicyVersion` synthesiser. Nothing in this file's API
// surface needs to change in that follow-up; only the
// `Load` body.
//
// # Why the package-doc note in `embed.go` still applies
//
// `embed.go` already documents that `unsigned_dev.go` will
// READ from `embeddedRulePacks` to produce an unsigned
// `steward.PolicyVersion` (architecture Sec 3.8 STRUCTURAL
// bypass). This file honours that documentation today by
// SHIPPING THE FILE under the same `//go:build !prod` tag;
// the missing piece is only the YAML decoder body, which
// is Stage 1.4 work as noted.

package devpolicy

import (
	"context"
)

// devLoader is the !prod build's concrete [Loader]
// implementation. It carries no state: every Load call is
// self-contained (the [LoaderSource] supplies the input
// kind and any directory path).
//
// The type is unexported on purpose: callers route through
// the [Loader] interface that [NewLoader] returns, never
// reaching into the concrete value directly. This keeps the
// dev / prod swap a SHAPE swap rather than a TYPE swap.
type devLoader struct{}

// NewLoader returns the active build's [Loader].
//
// In the !prod (dev) build (this file), it returns a
// [devLoader] whose Load method validates the
// [LoaderSource] and then returns
// [ErrLoaderNotYetImplemented] (the Stage 1.1 shell;
// Stage 1.4 wires the real YAML decoder behind the same
// interface).
//
// In the prod build (`unsigned_prod.go`), it returns a
// loader whose Load method ALWAYS returns
// [ErrDevModeUnavailable] regardless of input.
//
// The function name and signature are IDENTICAL across
// both files so the `cmd/cleanc` dispatcher can call
// `devpolicy.NewLoader()` without a per-build-tag import.
func NewLoader() Loader { return devLoader{} }

// Load is the Stage 1.1 dev-build stub. It validates the
// [LoaderSource] first so an operator typo in `--policy
// <path>` surfaces as the canonical
// [ErrMissingPolicyDir] / "policy dir: ..." diagnostic
// here -- not later, after a confusing empty walk -- and
// then returns [ErrLoaderNotYetImplemented].
//
// The `ctx` parameter is currently unused; it is kept on
// the signature because (a) the [Loader] interface
// declares it and (b) Stage 1.4's real YAML decoder will
// thread it into per-file reads so a cancelled CLI run
// stops walking promptly.
func (devLoader) Load(_ context.Context, src LoaderSource) (Bundle, error) {
	// Validate the source SHAPE eagerly so an operator-
	// facing diagnostic (forgot `--policy <path>`, typo in
	// the path) surfaces before the stub error below.
	if _, err := src.FS(); err != nil {
		return Bundle{}, err
	}
	return Bundle{}, ErrLoaderNotYetImplemented
}
