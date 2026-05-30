// -----------------------------------------------------------------------
// <copyright file="prod_loader.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build prod

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