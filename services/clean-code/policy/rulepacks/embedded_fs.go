// -----------------------------------------------------------------------
// <copyright file="embedded_fs.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package rulepacks

import "embed"

//go:embed solid/*.yaml decoupling/*.yaml
var EmbeddedFS embed.FS