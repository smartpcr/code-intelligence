// -----------------------------------------------------------------------
// <copyright file="load_unsigned_bundle.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// LoadUnsignedBundle is the entry point the acceptance scenario
// (implementation-plan.md Stage 3.4) names for the prod-build
// exclusion proof. It wraps NewLoader().Load() so the symbol
// is directly testable in both -tags prod and dev builds.
//
// In a dev build (default), LoadUnsignedBundle returns
// ErrLoaderNotYetImplemented (the Stage 1.1 stub).
// In a -tags prod build, it returns ErrDevModeUnavailable
// immediately without performing any I/O.

package devpolicy

import "context"

// LoadUnsignedBundle loads an unsigned dev-mode policy bundle
// via the active build's Loader. This is the canonical entry
// point for the prod-build exclusion acceptance scenario.
func LoadUnsignedBundle(ctx context.Context, src LoaderSource) (Bundle, error) {
	return NewLoader().Load(ctx, src)
}
