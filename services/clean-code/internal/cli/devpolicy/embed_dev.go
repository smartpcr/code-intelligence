// -----------------------------------------------------------------------
// <copyright file="embed_dev.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build !prod

package devpolicy

import (
	"io/fs"

	"github.com/smartpcr/code-intelligence/services/clean-code/policy/rulepacks"
)

var embeddedRulePacks fs.FS = rulepacks.EmbeddedRulePacks