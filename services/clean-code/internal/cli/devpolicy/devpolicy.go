// -----------------------------------------------------------------------
// <copyright file="devpolicy.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package devpolicy loads YAML rule packs into an in-memory unsigned
// PolicyVersion for dev-mode CLI usage.
package devpolicy

// DevPolicyLoader loads YAML rule packs into an unsigned in-memory
// PolicyVersion for local development use.
type DevPolicyLoader struct {
	RulePackPaths []string
}
