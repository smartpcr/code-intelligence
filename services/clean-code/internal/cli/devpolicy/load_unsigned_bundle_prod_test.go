// -----------------------------------------------------------------------
// <copyright file="load_unsigned_bundle_prod_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build prod

// This test is the prod-gated proof the acceptance scenario
// (implementation-plan.md Stage 3.4) requires: it calls
// LoadUnsignedBundle(...) and asserts the returned error
// equals devpolicy.ErrDevModeUnavailable with the exact
// message "dev-mode policy bypass not available in prod build".

package devpolicy

import (
	"context"
	"testing"
)

// TestLoadUnsignedBundle_ProdReturnsErrDevModeUnavailable calls
// LoadUnsignedBundle and asserts the returned error is IDENTICAL
// (not merely errors.Is-compatible) to ErrDevModeUnavailable,
// and that the error message exactly matches the impl-plan-pinned
// text.
func TestLoadUnsignedBundle_ProdReturnsErrDevModeUnavailable(t *testing.T) {
	t.Parallel()

	bundle, err := LoadUnsignedBundle(context.Background(), LoaderSource{UseEmbedded: true})
	if err == nil {
		t.Fatal("LoadUnsignedBundle returned nil error; want ErrDevModeUnavailable")
	}

	// Identity check: the prod build must return the sentinel
	// value VERBATIM, not a wrapped copy.
	if err != ErrDevModeUnavailable { //nolint:errorlint
		t.Fatalf("LoadUnsignedBundle error = %v (identity %p); want IDENTITY ErrDevModeUnavailable (%p)",
			err, err, ErrDevModeUnavailable)
	}

	// Exact message check (not substring): the error text
	// must equal the full sentinel string.
	const wantMsg = "devpolicy: dev-mode policy bypass not available in prod build"
	if got := err.Error(); got != wantMsg {
		t.Errorf("LoadUnsignedBundle error message = %q; want exact %q", got, wantMsg)
	}

	// Zero-value bundle check: no partial state leak.
	if len(bundle.Rules) != 0 || len(bundle.RulePacks) != 0 {
		t.Errorf("LoadUnsignedBundle returned non-zero Bundle %+v; want zero value", bundle)
	}
}
