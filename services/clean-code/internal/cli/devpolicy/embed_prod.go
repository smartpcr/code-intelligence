// -----------------------------------------------------------------------
// <copyright file="embed_prod.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build prod

package devpolicy

import "io/fs"

var embeddedRulePacks fs.FS