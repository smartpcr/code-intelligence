// -----------------------------------------------------------------------
// <copyright file="banner.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package devpolicy

import "io"

// DevModeBanner is the C10 banner string emitted to stderr when
// the CLI runs with the unsigned dev-mode policy bypass active.
const DevModeBanner = "\u26a0  DEV MODE \u2014 unsigned policy bypass active. Not for production use.\n"

// EmitBanner writes the dev-mode warning banner to w.
func EmitBanner(w io.Writer) {
	_, _ = io.WriteString(w, DevModeBanner)
}